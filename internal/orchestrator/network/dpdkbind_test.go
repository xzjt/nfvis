package network

// FR-NET-001（决策 #72）：网卡驱动接管（sysfs driver_override/bind/unbind）单测。
// 用临时目录伪造 sysfs 结构（class/net/<if>/device 符号链接 + bus/pci/...）。
//
// 注：夹具里的 PCI 地址用连字符（0000-13-00.0）而非 Linux 的 0000:13:00.0——
// Windows 文件名不允许冒号，而本包单测在开发机（Windows）上也必须能跑；
// binder 只把 PCI 当不透明字符串拼接，真实路径由 nfvis-vm 上的实机验证覆盖。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSysfs 建一个最小 sysfs 树：网卡已绑到 curDriver。
func fakeSysfs(t *testing.T, ifname, pci, curDriver string) (root string, writes map[string][]string) {
	t.Helper()
	root = t.TempDir()
	// /sys/class/net/<if>/device -> ../../devices/<pci>
	netDir := filepath.Join(root, "class", "net", ifname)
	if err := os.MkdirAll(netDir, 0o755); err != nil {
		t.Fatal(err)
	}
	devDir := filepath.Join(root, "devices", pci)
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(devDir, filepath.Join(netDir, "device")); err != nil {
		t.Fatal(err)
	}
	// /sys/bus/pci/devices/<pci>/{driver_override,driver}
	busDev := filepath.Join(root, "bus", "pci", "devices", pci)
	if err := os.MkdirAll(busDev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(busDev, "driver_override"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if curDriver != "" {
		drvDir := filepath.Join(root, "bus", "pci", "drivers", curDriver)
		if err := os.MkdirAll(drvDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(drvDir, filepath.Join(busDev, "driver")); err != nil {
			t.Fatal(err)
		}
	}
	// 目标 DPDK 驱动目录
	if err := os.MkdirAll(filepath.Join(root, "bus", "pci", "drivers", DefaultUioDriver), 0o755); err != nil {
		t.Fatal(err)
	}
	return root, map[string][]string{}
}

func newTestBinder(t *testing.T, root string) (*DPDKBinder, *map[string][]string) {
	t.Helper()
	writes := &map[string][]string{}
	b := &DPDKBinder{
		SysfsRoot: root,
		ReadFile:  os.ReadFile,
		WriteFile: func(path string, data []byte) error {
			(*writes)[path] = append((*writes)[path], string(data))
			return nil
		},
	}
	b.Rescan = func() error { return b.WriteFile(b.path("bus/pci/rescan"), []byte("1")) }
	return b, writes
}

func TestDPDKBinderReadsPCIDriver(t *testing.T) {
	root, _ := fakeSysfs(t, "ens224", "0000-13-00.0", "vmxnet3")
	b, _ := newTestBinder(t, root)

	pci, err := b.PCIAddrOf("ens224")
	if err != nil || pci != "0000-13-00.0" {
		t.Fatalf("PCIAddrOf = %q, %v", pci, err)
	}
	drv, err := b.DriverOf("ens224")
	if err != nil || drv != "vmxnet3" {
		t.Fatalf("DriverOf = %q, %v", drv, err)
	}
	// 无 PCI 设备的接口（如 bond/veth）应明确报错
	if _, err := b.PCIAddrOf("不存在"); err == nil {
		t.Fatal("无 PCI 设备应报错")
	}
}

func TestDPDKBinderBind(t *testing.T) {
	root, _ := fakeSysfs(t, "ens224", "0000-13-00.0", "vmxnet3")
	b, writes := newTestBinder(t, root)

	pci, err := b.Bind(context.Background(), "ens224", "")
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if pci != "0000-13-00.0" {
		t.Fatalf("pci = %q", pci)
	}
	// 必须：先解绑原驱动、再写 driver_override、最后 bind 到 vfio-pci
	w := *writes
	unbindKey := filepath.Join(root, "bus", "pci", "drivers", "vmxnet3", "unbind")
	overrideKey := filepath.Join(root, "bus", "pci", "devices", pci, "driver_override")
	bindKey := filepath.Join(root, "bus", "pci", "drivers", DefaultUioDriver, "bind")
	if got := w[unbindKey]; len(got) != 1 || got[0] != pci {
		t.Fatalf("应从 vmxnet3 解绑 %s: %v", pci, got)
	}
	if got := w[overrideKey]; len(got) != 1 || got[0] != DefaultUioDriver {
		t.Fatalf("应写 driver_override=%s: %v", DefaultUioDriver, got)
	}
	if got := w[bindKey]; len(got) != 1 || got[0] != pci {
		t.Fatalf("应绑定到 vfio-pci: %v", got)
	}
}

// 已绑到目标驱动 → 幂等（不产生任何写入）。
func TestDPDKBinderBindIdempotent(t *testing.T) {
	root, _ := fakeSysfs(t, "ens224", "0000-13-00.0", DefaultUioDriver)
	b, writes := newTestBinder(t, root)
	if _, err := b.Bind(context.Background(), "ens224", ""); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if len(*writes) != 0 {
		t.Fatalf("已就位不应有写入: %v", *writes)
	}
}

func TestDPDKBinderUnbind(t *testing.T) {
	root, _ := fakeSysfs(t, "ens224", "0000-13-00.0", DefaultUioDriver)
	b, writes := newTestBinder(t, root)

	pci, err := b.Unbind(context.Background(), "ens224", "vmxnet3")
	if err != nil {
		t.Fatalf("Unbind: %v", err)
	}
	w := *writes
	unbindKey := filepath.Join(root, "bus", "pci", "drivers", DefaultUioDriver, "unbind")
	if got := w[unbindKey]; len(got) != 1 || got[0] != pci {
		t.Fatalf("应从 vfio-pci 解绑: %v", got)
	}
	// 必须清空 driver_override 并触发 rescan（否则内核不会重新探测原生驱动）
	if got := w[filepath.Join(root, "bus", "pci", "devices", pci, "driver_override")]; len(got) != 1 || strings.TrimSpace(got[0]) != "" {
		t.Fatalf("应清空 driver_override: %v", got)
	}
	// 给出 to-driver → 显式 bind（实测 rescan 不足以让内核重新探测原生驱动）
	if got := w[filepath.Join(root, "bus", "pci", "drivers", "vmxnet3", "bind")]; len(got) != 1 || got[0] != pci {
		t.Fatalf("应显式绑定到 vmxnet3: %v", got)
	}
}

// 目标驱动不可用（模块未加载）→ 明确报错，且不得留下半绑状态。
func TestDPDKBinderBindMissingDriver(t *testing.T) {
	root, _ := fakeSysfs(t, "ens224", "0000-13-00.0", "vmxnet3")
	// 移除 vfio-pci 驱动目录
	if err := os.RemoveAll(filepath.Join(root, "bus", "pci", "drivers", DefaultUioDriver)); err != nil {
		t.Fatal(err)
	}
	b, _ := newTestBinder(t, root)
	if _, err := b.Bind(context.Background(), "ens224", ""); err == nil {
		t.Fatal("驱动不可用应报错")
	} else if !strings.Contains(err.Error(), DefaultUioDriver) {
		t.Fatalf("错误应指明驱动: %v", err)
	}
}

// 已被 DPDK 接管的网卡在内核中无 netdev → 必须支持直接用 PCI 地址定位（unbind 的常态）。
func TestDPDKBinderPCIPassthrough(t *testing.T) {
	root, _ := fakeSysfs(t, "ens224", "0000-13-00.0", DefaultUioDriver)
	b, _ := newTestBinder(t, root)

	// 接口名解析不到（模拟 DPDK 已接管：无 netdev）→ 明确提示改用 PCI
	if _, err := b.PCIAddrOf("ens224-gone"); err == nil {
		t.Fatal("无 netdev 应报错")
	} else if !strings.Contains(err.Error(), "PCI") {
		t.Fatalf("错误应提示改用 PCI 地址: %v", err)
	}
	// 直接给 PCI 地址（域可省）应可用
	if got, err := b.PCIAddrOf("0000:13:00.0"); err != nil || got != "0000:13:00.0" {
		t.Fatalf("完整 PCI 应直接通过: %q %v", got, err)
	}
	if got, err := b.PCIAddrOf("13:00.0"); err != nil || got != "0000:13:00.0" {
		t.Fatalf("省略域应补全: %q %v", got, err)
	}
	if IsPCIAddr("ens224") {
		t.Fatal("接口名不应判为 PCI 地址")
	}
}

// 解绑但内核未自动重新探测（实测常见）→ 必须报可操作错误，不得静默留下无驱动网卡。
func TestDPDKBinderUnbindNoAutoProbe(t *testing.T) {
	// 建一棵"解绑后无驱动"的树（无 driver 符号链接）→ 模拟内核未自动重新探测
	root, _ := fakeSysfs(t, "ens224", "0000-13-00.0", "")
	b, _ := newTestBinder(t, root)
	if _, err := b.Unbind(context.Background(), "ens224", ""); err == nil {
		t.Fatal("未自动探测时应报错（不静默留下无驱动网卡）")
	} else if !strings.Contains(err.Error(), "to-driver") {
		t.Fatalf("错误应给出可操作提示: %v", err)
	}
}
