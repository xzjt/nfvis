package network

// FR-NET-001（决策 #72）：业务网卡的 DPDK 驱动接管。
//
// 背景：此前产品只生成 startup.conf 里的 `dev <pci> { name <ifname> }`，
// 却没有任何代码把网卡从内核驱动切到 vfio-pci——实机必须人工/安装期带外完成
// （M3 交接文档即为手敲 `echo vfio-pci > driver_override`）。本文件把该能力做进产品。
//
// 实现即 sysfs 标准流程（Linux 文档 binding/unbinding）：
//   bind:   echo <driver> > <pci>/driver_override
//           echo <pci>    > /sys/bus/pci/drivers/<driver>/bind
//   unbind: echo <pci>    > /sys/bus/pci/drivers/<cur>/unbind
//           echo          > <pci>/driver_override      （清空覆盖，交还内核自动探测）
//
// 底座交互藏在可注入的 sysfs 根与读写函数后，单测用假实现。

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// DefaultUioDriver DPDK 默认绑定驱动（决策 #18 的 vpp.dpdk.uio_driver 缺省值）。
const DefaultUioDriver = "vfio-pci"

// DefaultWriteTimeout 单次 sysfs 写的缺省时限（发现 #13：不能让一条命令永久占住执行器）。
const DefaultWriteTimeout = 20 * time.Second

// DPDKBinder 网卡驱动接管编排。
type DPDKBinder struct {
	// SysfsRoot sysfs 根（缺省 /sys，单测注入临时目录）。
	SysfsRoot string
	// ReadFile / WriteFile 注入点（缺省 os 实现）。
	ReadFile  func(string) ([]byte, error)
	WriteFile func(string, []byte) error
	// Rescan 触发 PCI 重新探测（解绑后交还内核驱动；缺省写 /sys/bus/pci/rescan）。
	Rescan func() error
	// WriteTimeout 单次 sysfs 写的时限（<=0 取 DefaultWriteTimeout）。
	// 由来：对正在被数据面占用的口写 sysfs 会阻塞在内核里（发现 #13）。
	WriteTimeout time.Duration
	// Bindings 绑定记录（决策 #100）：netdev 已消失时的口名→PCI 回退来源。
	// 有了它，「按口名解绑」（`request interfaces ens224 unbind-dpdk`）才成立——
	// 否则交 DPDK 后内核无 netdev，只能凭 PCI 地址操作。
	Bindings *Bindings
	// ModuleLoader 加载内核模块（缺省 exec modprobe；单测注入假实现）。
	ModuleLoader func(module string) error
	// ModulesLoadDir 开机自动加载模块的 drop-in 目录（缺省 /etc/modules-load.d）。
	ModulesLoadDir string
	// Logf 运行期提示输出（缺省 log.Printf → journald；单测注入以断言）。
	Logf func(format string, args ...any)
}

// NewDPDKBinder 构造缺省（真实 sysfs）实现。
func NewDPDKBinder() *DPDKBinder {
	b := &DPDKBinder{
		SysfsRoot:      "/sys",
		ReadFile:       os.ReadFile,
		WriteFile:      func(path string, data []byte) error { return os.WriteFile(path, data, 0o644) },
		ModulesLoadDir: "/etc/modules-load.d",
		Logf:           log.Printf,
	}
	b.Rescan = func() error { return b.WriteFile(b.path("bus/pci/rescan"), []byte("1")) }
	b.ModuleLoader = func(module string) error {
		return exec.Command("modprobe", module).Run()
	}
	return b
}

func (b *DPDKBinder) logf(format string, args ...any) {
	if b.Logf != nil {
		b.Logf(format, args...)
	}
}

func (b *DPDKBinder) root() string {
	if b.SysfsRoot == "" {
		return "/sys"
	}
	return b.SysfsRoot
}

func (b *DPDKBinder) path(rel string) string { return filepath.Join(b.root(), rel) }

func (b *DPDKBinder) write(path, data string) error {
	if b.WriteFile == nil {
		return fmt.Errorf("sysfs 写入未装配")
	}
	// 超时保护（发现 #13）：**对正在被数据面占用的口写 sysfs 会阻塞在内核里**（VPP 持着
	// 该设备的 vfio group），实测请求永不返回、并把 CLI 执行器的锁一直占着。
	// 此处给每次写加时限：超时即返回可读错误，至少不把整条管理通道拖死。
	timeout := b.WriteTimeout
	if timeout <= 0 {
		timeout = DefaultWriteTimeout
	}
	done := make(chan error, 1)
	go func() { done <- b.WriteFile(path, []byte(data)) }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		// 写操作可能仍在内核里挂着（无法取消），故措辞如实说明并给出下一步
		return fmt.Errorf("写 %s 超时（%s）：内核侧可能仍被数据面占用或设备状态异常；"+
			"请先确认该口已离开数据面（request vpp restart），必要时重启 nfvisd", path, timeout)
	}
}

// pciAddrRe PCI 地址形如 0000:0b:00.0（域可省）。
var pciAddrRe = regexp.MustCompile(`^(?:[0-9a-fA-F]{4}:)?[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)

// IsPCIAddr 判断入参是否已是 PCI 地址。
func IsPCIAddr(s string) bool { return pciAddrRe.MatchString(strings.TrimSpace(s)) }

// PCIAddrOf 把「接口名或 PCI 地址」解析为 PCI 地址。
//
// 之所以必须接受 PCI 地址：**已被 DPDK 接管的网卡在内核里没有 netdev**
// （/sys/class/net/<ifname> 不存在），此时只能按 PCI 定位。
// 若无 netdev，再回退到**绑定记录**（决策 #100）：记录里有这个口是产品自己绑的结论，
// 于是「按口名」操作（如 unbind-dpdk ens224）在接管后依然可用。
func (b *DPDKBinder) PCIAddrOf(ifnameOrPCI string) (string, error) {
	arg := strings.TrimSpace(ifnameOrPCI)
	if arg == "" {
		return "", fmt.Errorf("接口名或 PCI 地址不能为空")
	}
	if IsPCIAddr(arg) {
		return normPCI(arg), nil
	}
	lnk := b.path(filepath.Join("class/net", arg, "device"))
	target, err := os.Readlink(lnk)
	if err != nil {
		if b.Bindings != nil {
			if pci, ok := b.Bindings.Get(arg); ok {
				return pci, nil
			}
		}
		return "", fmt.Errorf("接口 %s 无 PCI 设备（DPDK 已接管的网卡在内核中无 netdev，请改用 PCI 地址）: %w", arg, err)
	}
	return filepath.Base(target), nil
}

// normPCI 补全 4 位域前缀（内核 sysfs 路径用完整域）。
func normPCI(pci string) string {
	if strings.Count(pci, ":") == 2 {
		return pci
	}
	return "0000:" + pci
}

// DriverOf 返回网卡当前绑定的驱动名（无驱动返回空串）。
func (b *DPDKBinder) DriverOf(ifname string) (string, error) {
	pci, err := b.PCIAddrOf(ifname)
	if err != nil {
		return "", err
	}
	lnk := b.path(filepath.Join("bus/pci/devices", pci, "driver"))
	target, err := os.Readlink(lnk)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // 未绑定任何驱动
		}
		return "", fmt.Errorf("读取 %s 的驱动: %w", pci, err)
	}
	return filepath.Base(target), nil
}

// Bind 把网卡绑定到 DPDK 驱动（driver 为空用 vfio-pci）。
//
// 幂等：已绑定到目标驱动时直接返回。切换驱动会**中断该网卡现有流量**，
// 且该网卡不得正被 VPP 使用（调用方负责编排，见 CLI/API 的确认语义）。
func (b *DPDKBinder) Bind(ctx context.Context, ifname, driver string) (string, error) {
	if driver = strings.TrimSpace(driver); driver == "" {
		driver = DefaultUioDriver
	}
	pci, err := b.PCIAddrOf(ifname)
	if err != nil {
		return "", err
	}
	cur, err := b.DriverOf(ifname)
	if err != nil {
		return "", err
	}
	if cur == driver {
		return pci, nil // 已就位，幂等
	}
	// 目标驱动就位检查（决策 #112）：模块未加载时**先自动补一次**。
	// 业务口绑定是产品自己的底座动作，modprobe 这种机械步骤不该留给操作者——
	// 首次绑定与每次重启后都会撞上（实测首启路径最常报的错就是这条）。
	drvDir := b.path(filepath.Join("bus/pci/drivers", driver))
	if _, err := os.Stat(drvDir); err != nil {
		if b.ModuleLoader == nil {
			return "", fmt.Errorf("目标驱动 %s 不可用（模块未加载？）: %w", driver, err)
		}
		if lerr := b.ModuleLoader(driver); lerr != nil {
			return "", fmt.Errorf("目标驱动 %s 不可用，自动加载模块失败（%v）: %w", driver, lerr, err)
		}
		if _, err2 := os.Stat(drvDir); err2 != nil {
			return "", fmt.Errorf("目标驱动 %s 不可用（已尝试加载模块仍未见）: %w", driver, err2)
		}
		b.logf("已自动加载内核模块 %s", driver)
	}
	// 先从当前驱动解绑
	if cur != "" {
		if err := b.write(b.path(filepath.Join("bus/pci/drivers", cur, "unbind")), pci); err != nil {
			return "", fmt.Errorf("从 %s 解绑 %s: %w", cur, pci, err)
		}
	}
	if err := b.write(b.path(filepath.Join("bus/pci/devices", pci, "driver_override")), driver); err != nil {
		return "", fmt.Errorf("设置 driver_override=%s: %w", driver, err)
	}
	if err := b.write(b.path(filepath.Join("bus/pci/drivers", driver, "bind")), pci); err != nil {
		return "", fmt.Errorf("绑定 %s 到 %s: %w", pci, driver, err)
	}
	// 绑定成功即持久化开机加载（决策 #112）：绑定不跨重启（重启后网卡回内核驱动、
	// 操作者要重新 bind），但驱动模块必须已在位——不持久化则每次重启后都得记得手工
	// modprobe，实测这正是首启路径反复卡住的一步。
	b.persistModuleLoad(driver)
	return pci, nil
}

// persistModuleLoad 把驱动模块写进开机自动加载目录（幂等；失败不阻断绑定）。
//
// 只写 drop-in、不动 /etc/modules——后者是操作者/发行版的地盘，产品文件单独成件便于卸载时撤销。
func (b *DPDKBinder) persistModuleLoad(driver string) {
	dir := b.ModulesLoadDir
	if dir == "" {
		dir = "/etc/modules-load.d"
	}
	name := "nfvis-" + strings.ReplaceAll(driver, "_", "-") + ".conf"
	file := filepath.Join(dir, name)
	want := driver + "\n"
	if b.ReadFile != nil {
		if cur, err := b.ReadFile(file); err == nil && string(cur) == want {
			return // 幂等：内容已是目标
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.logf("警告：创建 %s 失败：%v（重启后需手工 modprobe %s）", dir, err, driver)
		return
	}
	write := b.WriteFile
	if write == nil {
		write = func(path string, data []byte) error { return os.WriteFile(path, data, 0o644) }
	}
	if err := write(file, []byte(want)); err != nil {
		b.logf("警告：写入 %s 失败：%v（重启后需手工 modprobe %s）", file, err, driver)
		return
	}
	b.logf("已持久化开机自动加载：%s（%s）", driver, file)
}

// Unbind 把网卡解绑出 vfio-pci 并交还内核驱动。
//
// toDriver 非空时**显式绑定**到该驱动；为空则仅清空 driver_override 并触发 rescan
// 交内核自动探测。
//
// 实测（nfvis-vm，内核 6.x + vfio-pci）：清空 override + rescan **不足以**让内核重新探测
// 原生驱动（设备停留在无驱动状态），必须显式 `bind`。故提供 toDriver，并在未给定时
// 返回可操作的错误，而不是静默留下一张无驱动的网卡。
func (b *DPDKBinder) Unbind(ctx context.Context, ifname, toDriver string) (string, error) {
	pci, err := b.PCIAddrOf(ifname)
	if err != nil {
		return "", err
	}
	cur, err := b.DriverOf(ifname)
	if err != nil {
		return "", err
	}
	if cur != "" {
		if err := b.write(b.path(filepath.Join("bus/pci/drivers", cur, "unbind")), pci); err != nil {
			return "", fmt.Errorf("从 %s 解绑 %s: %w", cur, pci, err)
		}
	}
	// 清空覆盖，让内核按常规匹配重新绑定原生驱动
	if err := b.write(b.path(filepath.Join("bus/pci/devices", pci, "driver_override")), "\n"); err != nil {
		return "", fmt.Errorf("清除 driver_override: %w", err)
	}
	if toDriver = strings.TrimSpace(toDriver); toDriver != "" {
		if err := b.write(b.path(filepath.Join("bus/pci/drivers", toDriver, "bind")), pci); err != nil {
			return pci, fmt.Errorf("绑定 %s 到 %s: %w", pci, toDriver, err)
		}
		return pci, nil
	}
	if b.Rescan != nil {
		if err := b.Rescan(); err != nil {
			return pci, fmt.Errorf("已解绑 %s，但触发 PCI 重新探测失败（可手工 echo 1 > /sys/bus/pci/rescan）: %w", pci, err)
		}
	}
	// 核实结果：未自动绑定则给出可操作提示（实测该情形常见，不静默留下无驱动网卡）
	if after, _ := b.DriverOf(ifname); after == "" {
		return pci, fmt.Errorf("已解绑 %s，但内核未自动重新探测原生驱动；请显式指定："+
			"request interfaces %s unbind-dpdk to-driver <驱动名>", pci, pci)
	}
	return pci, nil
}
