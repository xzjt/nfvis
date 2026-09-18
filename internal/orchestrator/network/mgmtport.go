package network

// 管理口守卫（发现 #7，决策 #101）：把承载管理路径的网卡交给 DPDK，会**当场**失去 SSH 与管理 API。
//
// 为什么必须有：`request interfaces <n> bind-dpdk` 的候选清单里就含管理口（候选取「数据面 ∪ 内核网卡」），
// 而 dpdkbind.go 的 Bind/Unbind **没有任何管理口判断**——文档（AGENTS §3.4、用户手册）只是"声明约束"。
// 2026-09-18 真机实测：**未声明管理口**时 `request interfaces ens160 bind-dpdk --yes` 照做，
// 承载 SSH/默认路由的网卡交给 vfio-pci，会话当场断开，只能带外重启恢复（发现 #7）。
// 注：**已在配置里声明**管理口的那条路已有校验拦（model 的 checkManagementIsolation 禁止管理口出现在
// 数据面配置里），本守卫补的是**运行态动作**这条路——它不看配置也能拦，两条路径的事实来源保持同源
// （都用 `system.management.interface`）。
//
// 判据：**只取高置信度事实，宁漏不误**
//   - 配置声明的管理口；
//   - 承载默认路由的口；
//   - 守护进程监听地址所属的口。
//
// **低置信度线索不作为拒绝理由**：真机上 ens192/ens224 与 ens160 同在 VMnet8 广播域
// （交接文档的 cross-connect 环路一节即因此），按「有 IP」「与管理地址同网段」判会把业务口全数误判，
// 挡住合法的 DPDK 接管。
//
// 口径（决策 #101）：**默认拒绝，且不提供显式越过**——管理路径不拿来做试验。
// 确需变更该网卡驱动时，按用户手册的带外步骤操作（不依赖本进程）。

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// ErrManagementPort 拒绝对管理口执行会中断管理路径的操作。
var ErrManagementPort = errors.New("拒绝操作管理口")

// ManagementFacts 判定管理路径所需的事实（装配层提供；空值 = 未知，不参与判定）。
type ManagementFacts struct {
	// DeclaredMgmtIface 配置 `system management interface` 声明的管理口名。
	DeclaredMgmtIface string
	// DefaultRouteIface 承载默认路由的网卡名。
	DefaultRouteIface string
	// ListenIface 守护进程监听地址所属的网卡名。
	ListenIface string
}

// CheckManagementPort 判定 ifname 是否属于管理路径；是则返回带原因的 ErrManagementPort。
//
// 任一条事实命中即拒绝：三者都是「该口一旦失去内核驱动，本机将无法远程管理」的充分理由。
func CheckManagementPort(ifname string, f ManagementFacts) error {
	iface := strings.TrimSpace(ifname)
	if iface == "" {
		return nil
	}
	for _, r := range []struct{ name, why string }{
		{f.DeclaredMgmtIface, "配置中声明的管理口"},
		{f.DefaultRouteIface, "承载默认路由"},
		{f.ListenIface, "守护进程当前监听的网卡"},
	} {
		if r.name == "" || r.name != iface {
			continue
		}
		return fmt.Errorf("%w：%s（%s）。绑定或解绑都会中断 SSH 与管理 API，"+
			"本机随即无法远程管理；如确需变更该网卡的驱动，请按用户手册的带外步骤操作",
			ErrManagementPort, iface, r.why)
	}
	return nil
}

// DefaultRoutePath 内核路由表路径（Linux）。
const DefaultRoutePath = "/proc/net/route"

// DefaultRouteIfaceOf 读内核路由表求默认路由所在网卡；取不到返回空串（未知）。
//
// /proc/net/route 一行一个路由，字段为
// `Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT`；
// 默认路由即 Destination 与 Mask 全 0 的那行。用 /proc 而非 `ip route`：无需外部命令与权限。
func DefaultRouteIfaceOf(path string) string {
	if strings.TrimSpace(path) == "" {
		path = DefaultRoutePath
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for i, line := range strings.Split(string(raw), "\n") {
		if i == 0 { // 表头
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		if fields[1] == "00000000" && fields[7] == "00000000" {
			return fields[0]
		}
	}
	return ""
}

// IfaceOfIP 返回拥有该 IP 的网卡名；取不到返回空串（未知）。
func IfaceOfIP(ip string) string {
	want := net.ParseIP(strings.TrimSpace(ip))
	if want == nil {
		return ""
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			switch v := a.(type) {
			case *net.IPNet:
				if v.IP.Equal(want) {
					return iface.Name
				}
			case *net.IPAddr:
				if v.IP.Equal(want) {
					return iface.Name
				}
			}
		}
	}
	return ""
}

// IfaceOfPCI 由绑定记录反查口名（解绑路径给的可能就是 PCI 地址）。未知返回 ok=false。
func (b *Bindings) IfaceOfPCI(pci string) (string, bool) {
	pci = strings.TrimSpace(pci)
	if pci == "" || b == nil {
		return "", false
	}
	for name, p := range b.All() {
		if strings.EqualFold(p, pci) {
			return name, true
		}
	}
	return "", false
}

// SysfsRoot 缺省 sysfs 根（单测注入临时目录）。
const SysfsRoot = "/sys"

// KernelIfaceOfPCI 返回该 PCI 设备在内核中的网卡名（读 /sys/bus/pci/devices/<pci>/net/）。
//
// 已交 DPDK 的设备在内核里没有 netdev（该目录为空/不存在）→ 返回空，此时由绑定记录兜底。
// 有它才拦得住「按 PCI 地址解绑管理口」这条：那种情况下按口名的记录里根本没有该口。
func KernelIfaceOfPCI(pci string) string { return KernelIfaceOfPCIIn(SysfsRoot, pci) }

// KernelIfaceOfPCIIn 指定 sysfs 根（单测用）。
func KernelIfaceOfPCIIn(root, pci string) string {
	pci = strings.TrimSpace(pci)
	if pci == "" {
		return ""
	}
	if strings.TrimSpace(root) == "" {
		root = SysfsRoot
	}
	entries, err := os.ReadDir(filepath.Join(root, "bus/pci/devices", pci, "net"))
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if name := strings.TrimSpace(e.Name()); name != "" {
			return name
		}
	}
	return ""
}

// ResolveIfaceName 把「口名或 PCI 地址」解析成口名，供管理口判定使用。
//
// 顺序（按可信度）：本身就是口名 → 直接用；PCI 且内核仍绑着网卡 → 用内核名字；
// PCI 且已交 DPDK（内核无 netdev）→ 用绑定记录反查。都认不出返回 ok=false——
// 此时**不拦**：认不出名字就拒绝，堵住的是「把管理口交还内核驱动」这类唯一能救回网卡的操作，
// 而合法路径（绑定必须给口名）始终可识别。
func ResolveIfaceName(target string, rec *Bindings) (string, bool) {
	name := strings.TrimSpace(target)
	if name == "" {
		return "", false
	}
	if !IsPCIAddr(name) {
		return name, true
	}
	if viaKernel := KernelIfaceOfPCI(name); viaKernel != "" {
		return viaKernel, true
	}
	if rec == nil {
		return "", false
	}
	return rec.IfaceOfPCI(name)
}
