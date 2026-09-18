package network

// 决策 #100（发现 #8）：物理口一旦交 DPDK，内核里就没有 netdev 了；而 startup.conf 的
// dpdk 段以 PCI 为键。故必须把「口名 → PCI」在**绑定那一刻**落盘，生成时回退使用，
// 否则「声明端口 → 重启数据面」这条路走不通（生成时解析不到 PCI）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

func TestBindingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dpdk-bindings.json")
	rec := NewBindings(path)

	if _, ok := rec.Get("ens224"); ok {
		t.Fatal("未记录时不应命中")
	}
	if err := rec.Set("ens224", "0000:13:00.0"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := rec.Set("ens192", "0000:0b:00.0"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// 落盘可被新实例读回（重启后仍有效）
	again := NewBindings(path)
	if pci, ok := again.Get("ens224"); !ok || pci != "0000:13:00.0" {
		t.Fatalf("应从磁盘读回: %q %v", pci, ok)
	}
	// 解绑按 **PCI** 删除（解绑时操作者给的常是 PCI 地址，而不是口名）
	if err := again.DeleteByPCI("0000:13:00.0"); err != nil {
		t.Fatalf("DeleteByPCI: %v", err)
	}
	if _, ok := again.Get("ens224"); ok {
		t.Fatal("应已删除 ens224")
	}
	if pci, ok := NewBindings(path).Get("ens192"); !ok || pci != "0000:0b:00.0" {
		t.Fatal("不应误删其它口")
	}
}

// 解析顺序：netdev 仍在时以 sysfs 为准（活的事实优先于可能过期的记录）。
func TestPCIResolverPrefersLiveSysfs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.json")
	rec := NewBindings(path)
	if err := rec.Set("ens224", "0000:99:00.0"); err != nil { // 记录已过期（换过槽位）
		t.Fatal(err)
	}
	live := func(string) (string, error) { return "0000:13:00.0", nil }
	got, err := PCIResolverWithBindings(live, rec)("ens224")
	if err != nil || got != "0000:13:00.0" {
		t.Fatalf("sysfs 可用时应以它为准: %q %v", got, err)
	}
	// sysfs 解析不到（netdev 已消失 = 确已交 DPDK）→ 回退到记录
	gone := func(name string) (string, error) { return "", errNotFound(name) }
	got, err = PCIResolverWithBindings(gone, rec)("ens224")
	if err != nil || got != "0000:99:00.0" {
		t.Fatalf("netdev 消失时应回退到绑定记录: %q %v", got, err)
	}
	// 两边都没有 → 保留原错误（可读）
	if _, err := PCIResolverWithBindings(gone, rec)("ens224-x"); err == nil {
		t.Fatal("都解析不到应报错")
	}
}

// 迁移：把当前部署的 startup.conf 里 dev <pci> { name <口> } 的映射并入记录。
// 覆盖两种写法——产品生成的（多行）与手册里带外手写的（压一行）。
func TestBindingsImportStartupConf(t *testing.T) {
	dir := t.TempDir()
	generated := `unix {
  nodaemon
}
dpdk {
  dev default {
    num-rx-queues 2
  }
  dev 0000:0b:00.0 {
    name ens192
  }
  dev 0000:13:00.0 {
    name ens224
  }
  uio-driver vfio-pci
}
`
	genPath := filepath.Join(dir, "generated.conf")
	if err := os.WriteFile(genPath, []byte(generated), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := NewBindings(filepath.Join(dir, "b.json"))
	n, err := rec.ImportStartupConf(genPath)
	if err != nil || n != 2 {
		t.Fatalf("应导入 2 条: n=%d err=%v", n, err)
	}
	if pci, ok := rec.Get("ens224"); !ok || pci != "0000:13:00.0" {
		t.Fatalf("ens224 未导入: %q %v", pci, ok)
	}

	handwritten := `dpdk { dev 0000:0b:00.0 { name ens192 } dev 0000:13:00.0 { name ens224 } }` + "\n"
	hwPath := filepath.Join(dir, "hand.conf")
	if err := os.WriteFile(hwPath, []byte(handwritten), 0o644); err != nil {
		t.Fatal(err)
	}
	rec2 := NewBindings(filepath.Join(dir, "b2.json"))
	if n, err := rec2.ImportStartupConf(hwPath); err != nil || n != 2 {
		t.Fatalf("带外手写写法应同样可导入: n=%d err=%v", n, err)
	}
	// 文件不存在（首次安装）→ 不算错，导入 0 条
	rec3 := NewBindings(filepath.Join(dir, "b3.json"))
	if n, err := rec3.ImportStartupConf(filepath.Join(dir, "nope.conf")); err != nil || n != 0 {
		t.Fatalf("文件缺失不应报错: n=%d err=%v", n, err)
	}
}

// 生成 startup.conf 时，若已知「已交 DPDK 但本次未声明」的口会因此掉出数据面，必须告警。
// （真机上「掉口」正是这么发生的：手写播种被产品重生成的 startup.conf 覆盖。）
func TestApplierWarnsAboutDroppedBoundPort(t *testing.T) {
	dir := t.TempDir()
	rec := NewBindings(filepath.Join(dir, "b.json"))
	if err := rec.Set("ens192", "0000:0b:00.0"); err != nil {
		t.Fatal(err)
	}
	if err := rec.Set("ens224", "0000:13:00.0"); err != nil {
		t.Fatal(err)
	}
	var warns []string
	applier := &Applier{
		Path:     filepath.Join(dir, "startup.conf"),
		Write:    func(string, []byte) error { return nil },
		PCI:      PCIResolverWithBindings(func(name string) (string, error) { return "", errNotFound(name) }, rec),
		Bindings: rec,
		Warn:     func(m string) { warns = append(warns, m) },
	}
	// 只声明 ens224：ens192 会掉出数据面，应告警；ens224 不应被报
	cfg := &model.Config{
		Interfaces: []model.InterfaceConfig{{Name: "ens192"}, {Name: "ens224"}},
		Vpp: &model.VppConfig{DPDK: &model.VppDPDK{
			PerDev: []model.VppDevOverride{{Interface: "ens224"}},
		}},
	}
	if _, err := applier.Apply(context.Background(), cfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	joined := strings.Join(warns, " | ")
	if !strings.Contains(joined, "ens192") {
		t.Fatalf("应告警 ens192 会掉出数据面: %q", joined)
	}
	if strings.Contains(joined, "ens224") {
		t.Fatalf("已声明的口不应被告警: %q", joined)
	}

	// 两个都声明 → 不应有告警
	warns = nil
	cfg.Vpp.DPDK.PerDev = append(cfg.Vpp.DPDK.PerDev, model.VppDevOverride{Interface: "ens192"})
	if _, err := applier.Apply(context.Background(), cfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("全部声明后不应告警: %v", warns)
	}
}

// 绑定记录使「按口名解绑」成为可能：netdev 已消失时按记录定位 PCI。
func TestDPDKBinderPCIAddrOfFallsBackToBindings(t *testing.T) {
	root, _ := fakeSysfs(t, "ens224", "0000-13-00.0", DefaultUioDriver)
	b, _ := newTestBinder(t, root)
	rec := NewBindings(filepath.Join(t.TempDir(), "b.json"))
	if err := rec.Set("ens224", "0000-13-00.0"); err != nil {
		t.Fatal(err)
	}
	b.Bindings = rec

	if pci, err := b.PCIAddrOf("ens224"); err != nil || pci != "0000-13-00.0" {
		t.Fatalf("netdev 消失时应按记录解析: %q %v", pci, err)
	}
	if drv, err := b.DriverOf("ens224"); err != nil || drv != DefaultUioDriver {
		t.Fatalf("DriverOf 亦应可用: %q %v", drv, err)
	}
	// 记录里没有的口：照旧报错（并提示可用 PCI 地址）
	if _, err := b.PCIAddrOf("ens999"); err == nil {
		t.Fatal("无记录且无 netdev 应报错")
	}
}
