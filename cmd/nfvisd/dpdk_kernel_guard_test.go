package main

// 决策 #426②：DPDK 接管的能力守卫矩阵（两数据面 × bind/unbind 两向 × 管理口/非管理口）。
//
// 口径（附录 A #426）：内核数据面下 `request interfaces <n> unbind-dpdk` **恢复可用**
// （它正是「把网卡交还内核」的动作），`bind-dpdk` **保持拒绝**（绑定会把网卡从内核拿走）；
// 管理口两向一律拒；解绑前「仍在用」守卫按数据面各自判据（VPP 问 VPP、内核问内核）。
// 守卫在 CLI 与 REST 的共同落点（DPDKSetter.SetDPDKBound）上判定，故本用例直接打它。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// guardHarness 一套假底座：sysfs 树（含一个 DPDK 口 ens224 / 一个内核口 ens160）+ 写入记录。
type guardHarness struct {
	binder  *network.DPDKBinder
	rec     *network.Bindings
	writes  map[string][]string
	ifnames map[string]string // 口名 → PCI
}

// 夹具用连字符 PCI（Windows 文件名不允许冒号；binder 只把 PCI 当不透明字符串）。
// 注：binder 的 normPCI 会给「不含两个冒号」的串补域前缀（0000:…），Windows 上造不出带冒号的
// 真实路径，故 harness 把补前缀后的名字也录进绑定记录——真机上两者本就是同一个设备地址，
// 这条登记只服务于「按 PCI 回读驱动」的路径解析（否则该路径在合成夹具里永不命中）。
const (
	guardPCIDPDK   = "0000-15-00.0" // ens224：已交 vfio-pci（内核里没有它）
	guardPCIKernel = "0000-13-00.0" // ens192：内核口（非管理口）
	guardPCIMgmt   = "0000-0b-00.0" // ens160：内核口（管理口）
)

func newGuardHarness(t *testing.T) *guardHarness {
	t.Helper()
	root := t.TempDir()
	mkdir := func(rel string) string {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mkdir("bus/pci/drivers/vfio-pci")
	mkdir("bus/pci/drivers/vmxnet3")
	// ens224：**没有** class/net 条目（DPDK 已接管 ⇒ 内核无 netdev），PCI 由绑定记录定位；
	// ens192/ens160：内核口，device 链接指到自己的 PCI。
	// 另给三个 PCI 串各建一条 class/net 条目（连字符名在 Windows 上合法）：binder 按 **PCI**
	// 回读驱动时会先按名解析一次——这样「回读」这条路径在合成夹具里可解析（真机 PCI 含冒号、
	// 直接命中 PCIAddrOf 的地址分支，不存在这条弯路），也不必把 PCI 塞进绑定记录（那条记录
	// 会被解绑按 PCI 清理，夹具不能依赖它）。
	for _, pci := range []string{guardPCIDPDK, guardPCIKernel, guardPCIMgmt} {
		mkdir("devices/" + pci)
		mkdir("class/net/" + pci)
		if err := os.Symlink(filepath.Join(root, "devices", pci),
			filepath.Join(root, "class", "net", pci, "device")); err != nil {
			t.Fatal(err)
		}
	}
	for _, ifname := range []string{"ens192", "ens160"} {
		pci := guardPCIKernel
		if ifname == "ens160" {
			pci = guardPCIMgmt
		}
		mkdir("class/net/" + ifname)
		if err := os.Symlink(filepath.Join(root, "devices", pci),
			filepath.Join(root, "class", "net", ifname, "device")); err != nil {
			t.Fatal(err)
		}
	}
	for _, pci := range []string{guardPCIDPDK, guardPCIKernel, guardPCIMgmt} {
		dev := mkdir("bus/pci/devices/" + pci)
		if err := os.WriteFile(filepath.Join(dev, "driver_override"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// ens224 当前驱动 vfio-pci（DPDK 残留）；ens192/ens160 当前驱动 vmxnet3
	for pci, drv := range map[string]string{
		guardPCIDPDK: "vfio-pci", guardPCIKernel: "vmxnet3", guardPCIMgmt: "vmxnet3",
	} {
		link := filepath.Join(root, "bus", "pci", "devices", pci, "driver")
		if err := os.Symlink(filepath.Join(root, "bus", "pci", "drivers", drv), link); err != nil {
			t.Fatal(err)
		}
	}
	h := &guardHarness{writes: map[string][]string{}}
	h.binder = &network.DPDKBinder{
		SysfsRoot:      root,
		ReadFile:       os.ReadFile,
		ModulesLoadDir: filepath.Join(root, "etc", "modules-load.d"),
		WriteFile: func(path string, data []byte) error {
			h.writes[path] = append(h.writes[path], string(data))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			return os.WriteFile(path, data, 0o644)
		},
	}
	h.binder.Rescan = func() error { return h.binder.WriteFile(h.binder.SysfsRoot+"/bus/pci/rescan", []byte("1")) }
	// 绑定记录：ens224 → PCI（口交 DPDK 后内核无 netdev，这是唯一回退来源）
	rec := network.NewBindings(filepath.Join(root, "bindings.json"))
	if err := rec.Set("ens224", guardPCIDPDK); err != nil {
		t.Fatal(err)
	}
	h.binder.Bindings = rec
	h.rec = rec
	h.ifnames = map[string]string{
		"ens224": guardPCIDPDK, "ens192": guardPCIKernel, "ens160": guardPCIMgmt,
	}
	return h
}

// wrote 某路径是否被写过。
func (h *guardHarness) wrote(path string) bool { return len(h.writes[path]) > 0 }

// writeCount 系统调用层面的写入总数（被拒的行必须为 0 —— 守卫「先于任何 sysfs 动作」）。
func (h *guardHarness) writeCount() int {
	n := 0
	for _, v := range h.writes {
		n += len(v)
	}
	return n
}

func guardFacts() network.ManagementFacts {
	return network.ManagementFacts{DeclaredMgmtIface: "ens160"}
}

// vppSetter 构造 VPP 数据面接管面（dataplane 探针注入假件：true = 仍在 VPP 手里）。
func (h *guardHarness) vppSetter(inDataplane bool) *dpdkController {
	return &dpdkController{
		b: h.binder, rec: h.rec, logger: discardLogger(), facts: guardFacts,
		dataplane: func(string) (bool, error) { return inDataplane, nil },
	}
}

// kernelSetter 构造内核数据面接管面（inUse 探针注入假件）。
func (h *guardHarness) kernelSetter(inUse bool, why string) kernelDPDKSetter {
	return kernelDPDKSetter{
		b: h.binder, rec: h.rec, log: discardLogger(), facts: guardFacts,
		inUse: func(context.Context, string) (bool, string, error) { return inUse, why, nil },
	}
}

// TestDPDKSetterGuardMatrixByDataPlane 守卫矩阵：逐格断言「放行 / 拒绝 + 是否触到 sysfs」。
func TestDPDKSetterGuardMatrixByDataPlane(t *testing.T) {
	cases := []struct {
		name      string
		kernel    bool
		bound     bool   // true = bind-dpdk
		target    string // 口名（管理口守卫按它判）
		driver    string // 传给 SetDPDKBound 的驱动参数（bind：uio 驱动，unbind：交还的内核驱动）
		inUse     bool   // 解绑守卫看到的占用事实（bind 行不用）
		wantErr   string
		wantUnbin bool // 期望真的写了 unbind
		wantBind  bool
	}{
		{name: "vpp/非管理口/bind", bound: true, target: "ens192", driver: "", wantBind: true},
		{name: "vpp/非管理口/unbind（不在数据面）", target: "ens224", driver: "vmxnet3", wantUnbin: true},
		{name: "vpp/非管理口/unbind（仍在数据面）", target: "ens224", driver: "vmxnet3", inUse: true, wantErr: "仍在数据面中"},
		{name: "vpp/管理口/bind", bound: true, target: "ens160", driver: "", wantErr: "管理口"},
		{name: "vpp/管理口/unbind", target: "ens160", driver: "vmxnet3", wantErr: "管理口"},
		{name: "kernel/非管理口/bind", kernel: true, bound: true, target: "ens224", driver: "", wantErr: "不支持 bind-dpdk"},
		{name: "kernel/非管理口/unbind（DPDK 残留）", kernel: true, target: "ens224", driver: "vmxnet3", wantUnbin: true},
		{name: "kernel/非管理口/unbind（仍被内核数据面使用）", kernel: true, target: "ens224", driver: "vmxnet3", inUse: true, wantErr: "仍被内核数据面使用"},
		{name: "kernel/管理口/bind", kernel: true, bound: true, target: "ens160", driver: "", wantErr: "管理口"},
		{name: "kernel/管理口/unbind", kernel: true, target: "ens160", driver: "vmxnet3", wantErr: "管理口"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newGuardHarness(t)
			var (
				pci, drv string
				err      error
			)
			if tc.kernel {
				pci, drv, err = h.kernelSetter(tc.inUse, "是 vs-lan 的成员口").
					SetDPDKBound(context.Background(), tc.target, tc.bound, tc.driver)
			} else {
				pci, drv, err = h.vppSetter(tc.inUse).
					SetDPDKBound(context.Background(), tc.target, tc.bound, tc.driver)
			}

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("应被拒（含 %q），却放行：pci=%s drv=%s", tc.wantErr, pci, drv)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("拒绝文案应含 %q，得到：%v", tc.wantErr, err)
				}
				if n := h.writeCount(); n != 0 {
					t.Fatalf("被拒的行不得有任何 sysfs 写入，实际 %d 次：%v", n, h.writes)
				}
				return
			}

			if err != nil {
				t.Fatalf("应放行，得到错误：%v", err)
			}
			if tc.wantUnbin {
				key := filepath.Join(h.binder.SysfsRoot, "bus", "pci", "drivers", "vfio-pci", "unbind")
				if !h.wrote(key) {
					t.Fatalf("应从 vfio-pci 解绑（%s）：%v", key, h.writes)
				}
				// 绑定记录按 PCI 清理（口名→PCI 的陈旧记录会让后续操作指错设备）
				if _, ok := h.rec.Get("ens224"); ok {
					t.Fatalf("解绑后记录应删掉 ens224：%v", h.rec.All())
				}
			}
			if tc.wantBind {
				key := filepath.Join(h.binder.SysfsRoot, "bus", "pci", "drivers", "vfio-pci", "bind")
				if !h.wrote(key) {
					t.Fatalf("应绑定到 vfio-pci（%s）：%v", key, h.writes)
				}
			}
		})
	}
}

// 内核数据面下 bind 的拒绝文案必须**可照做**（含改回 VPP 数据面的路径），
// 且明确不是「该能力不可用」那种含糊措辞——方向是分开处置的，unbind 侧可用。
func TestKernelBindRejectedActionable(t *testing.T) {
	h := newGuardHarness(t)
	_, _, err := h.kernelSetter(false, "").SetDPDKBound(context.Background(), "ens224", true, "")
	if err == nil {
		t.Fatal("内核数据面下 bind 应被拒")
	}
	for _, want := range []string{"不支持 bind-dpdk", "set system dataplane vpp", "内核"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("拒绝文案应含 %q：%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "该能力不可用") {
		t.Fatalf("bind 与 unbind 是分开处置的，不应共用「该能力不可用」：%v", err)
	}
}

// 解绑守卫探测不到（口在内核里不存在——DPDK 残留的正常形态；或 ip 工具不可用）**不拦**：
// 与 VPP 侧同取向（探测通道不通不代表口在被使用），只有明确的「在用」事实才拒绝。
func TestKernelUnbindGuardProbeErrorDoesNotBlock(t *testing.T) {
	h := newGuardHarness(t)
	setter := kernelDPDKSetter{
		b: h.binder, rec: h.rec, log: discardLogger(), facts: guardFacts,
		inUse: func(context.Context, string) (bool, string, error) {
			return false, "", errors.New("Device \"ens224\" does not exist")
		},
	}
	if _, _, err := setter.SetDPDKBound(context.Background(), "ens224", false, "vmxnet3"); err != nil {
		t.Fatalf("探测不到不应拦（这是 DPDK 残留的正常形态）：%v", err)
	}
}

// 装配分派：内核数据面拿到的是能解绑的实现（不是「一切都不可用」的占位），VPP 侧不变。
func TestDPDKSetterForDispatchesByDataPlane(t *testing.T) {
	if got := dpdkSetterFor(model.DataPlaneKernel, nil, nil, discardLogger(), nil, nil, nil); got == nil {
		t.Fatal("内核数据面应装配非 nil 的接管面（解绑可用）")
	} else if _, ok := got.(kernelDPDKSetter); !ok {
		t.Fatalf("内核数据面应装配 kernelDPDKSetter，得到 %T", got)
	}
	if got := dpdkSetterFor(model.DataPlaneVPP, nil, nil, discardLogger(), nil, nil, nil); got == nil {
		t.Fatal("VPP 数据面应装配非 nil 的接管面")
	} else if _, ok := got.(*dpdkController); !ok {
		t.Fatalf("VPP 数据面应装配 *dpdkController，得到 %T", got)
	}
}

// 决策 #426③的点名探测：只对「内核里没有 + 能定位到 PCI + 当前绑着驱动」的口给出结论。
func TestDPDKHeldPortProbe(t *testing.T) {
	h := newGuardHarness(t)
	kernel := []string{"ens160", "ens192"}
	probe := dpdkHeldPortProbe(h.binder, func() ([]string, error) { return kernel, nil })

	// DPDK 残留（内核里没有、记录能定位、绑着 vfio-pci）→ 点名
	drv, pci, ok := probe("ens224")
	if !ok || drv != "vfio-pci" || pci != guardPCIDPDK {
		t.Fatalf("DPDK 残留应点名：drv=%q pci=%q ok=%v", drv, pci, ok)
	}
	// 内核里存在的口：没有「交还」这回事
	if _, _, ok := probe("ens192"); ok {
		t.Fatal("内核里的口不应被当作 DPDK 残留")
	}
	// 解析不到（既非内核口、也无绑定记录）→ 不给结论
	if _, _, ok := probe("nosuch0"); ok {
		t.Fatal("解析不到的口不应给结论")
	}
	// 内核清单读不到 → 一律不给结论（不猜）
	blind := dpdkHeldPortProbe(h.binder, func() ([]string, error) { return nil, errors.New("sysfs 不可读") })
	if _, _, ok := blind("ens224"); ok {
		t.Fatal("内核清单读不到时不应给结论")
	}
}
