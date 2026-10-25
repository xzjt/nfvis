package compute

// BUG-24：PCI 设备清单（`show system pci` 的事实源）的读法守护。
//
// 单测用临时目录造一份最小 sysfs 树（照 port_inventory_test/dpdkbind_test 的既有范式），
// 断言的三件事都是**真机现场的痛点**：
//  1. IOMMU 组必须**按组目录反查**（组里含桥/root port 时直通需整组隔离，不能只说「有组」）；
//  2. 无 IOMMU 组的设备要如实区分「无组」（不可直通）与「组信息取不到」；
//  3. 字段读不到就留空/-1（调用方渲染 `-`），不编造。
//
// 平台注记：Windows 的文件名不允许 `:`，而 sysfs 的目录名就是 BDF（`0000:0b:00.0`）。
// 本文件因此经 `fix.name()` 把 `:` 换成 `-` 造夹具、断言时同样经 `fix.name()` 比对——
// **被测逻辑与 BDF 字面量无关**（compute 层只把目录名当标识符，不解析它；BDF 语法校验
// 在 model.NormalizeBDF，另一处）。Linux 上 `fix.name()` 是恒等映射，夹具与真机形态一致。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// pciFixture 造一份最小 sysfs：bus/pci/devices + kernel/iommu_groups。
type pciFixture struct {
	root   string
	devs   string
	groups string
}

func newPCIFixture(t *testing.T) *pciFixture {
	t.Helper()
	root := t.TempDir()
	f := &pciFixture{
		root:   root,
		devs:   filepath.Join(root, "bus", "pci", "devices"),
		groups: filepath.Join(root, "kernel", "iommu_groups"),
	}
	if err := os.MkdirAll(f.devs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.groups, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// name 把 BDF 规整成当前平台可用的目录名（Windows 不允许 `:`；其余平台恒等）。
func (f *pciFixture) name(bdf string) string {
	if runtime.GOOS == "windows" {
		return strings.ReplaceAll(bdf, ":", "-")
	}
	return bdf
}

// dev 造一台设备：files 是 sysfs 字段文件（vendor/device/class/numa_node/enable/label）。
// driver 非空时建 `driver` 符号链接（指向 drivers/<driver>，与真实 sysfs 形态一致）。
func (f *pciFixture) dev(t *testing.T, bdf string, files map[string]string, driver string) {
	t.Helper()
	dir := filepath.Join(f.devs, f.name(bdf))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range files {
		if err := os.WriteFile(filepath.Join(dir, k), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if driver == "" {
		return
	}
	target := filepath.Join(f.root, "bus", "pci", "drivers", driver)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "driver")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("跳过：本环境无法创建符号链接（%v）", err)
		}
		t.Fatal(err)
	}
}

// group 把 devices 加入 IOMMU 组 n（组目录下的 devices/ 是内核的权威清单）。
func (f *pciFixture) group(t *testing.T, n int, devices ...string) {
	t.Helper()
	gdir := filepath.Join(f.groups, itoa(n), "devices")
	if err := os.MkdirAll(gdir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range devices {
		if err := os.Symlink(filepath.Join(f.devs, f.name(d)), filepath.Join(gdir, f.name(d))); err != nil {
			if runtime.GOOS == "windows" {
				t.Skipf("跳过：本环境无法创建符号链接（%v）", err)
			}
			t.Fatal(err)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// read 走与生产同一份实现（仅落点不同）。
func (f *pciFixture) read(t *testing.T) (PCIInventory, error) {
	t.Helper()
	return readPCIInventory(f.devs, f.groups)
}

// findDev 按 BDF 取设备（名字经平台规整）。
func (f *pciFixture) findDev(t *testing.T, inv PCIInventory, bdf string) PCIDeviceFacts {
	t.Helper()
	want := f.name(bdf)
	for _, d := range inv.Devices {
		if d.BDF == want {
			return d
		}
	}
	t.Fatalf("清单里没有 %s（实际：%v）", want, pciBDFs(inv))
	return PCIDeviceFacts{}
}

// groupNames 把组内 BDF 列表规整成平台形态后按序返回（断言用）。
func (f *pciFixture) groupNames(devs []string) string {
	out := make([]string, 0, len(devs))
	for _, d := range devs {
		out = append(out, f.name(d))
	}
	return strings.Join(out, ",")
}

func pciBDFs(inv PCIInventory) []string {
	out := make([]string, 0, len(inv.Devices))
	for _, d := range inv.Devices {
		out = append(out, d.BDF)
	}
	return out
}

// TestPCIInventoryReadsSysfsFields 逐字段读：vendor/device/class 去掉 0x 前缀、
// driver 取链接 basename、numa_node/enable 按整数、label 原文。
func TestPCIInventoryReadsSysfsFields(t *testing.T) {
	f := newPCIFixture(t)
	f.dev(t, "0000:0b:00.0", map[string]string{
		"vendor": "0x15ad", "device": "0x07b0", "class": "0x020000",
		"numa_node": "0\n", "enable": "1\n", "label": "  SLOT 3\n",
	}, "vmxnet3")

	inv, err := f.read(t)
	if err != nil {
		t.Fatalf("readPCIInventory: %v", err)
	}
	d := f.findDev(t, inv, "0000:0b:00.0")
	if d.Vendor != "15ad" || d.Device != "07b0" || d.Class != "020000" {
		t.Fatalf("ID 字段应去掉 0x 前缀：%+v", d)
	}
	if d.Driver != "vmxnet3" {
		t.Fatalf("driver 应取符号链接 basename：%q", d.Driver)
	}
	if d.NUMA != 0 || d.Enabled != 1 {
		t.Fatalf("numa_node/enable 应读整数：numa=%d enable=%d", d.NUMA, d.Enabled)
	}
	if d.Label != "SLOT 3" {
		t.Fatalf("label 应去空白：%q", d.Label)
	}
}

// TestPCIInventoryIOMMUGroupWithBridge 组内含 root port（class 0604）：
// 组号/组内数/同组 BDF 都必须读出来，且标记 GroupHasBridge ⇒ 直通建议「同组含桥」。
func TestPCIInventoryIOMMUGroupWithBridge(t *testing.T) {
	f := newPCIFixture(t)
	f.dev(t, "0000:0b:00.0", map[string]string{
		"vendor": "0x15ad", "device": "0x07b0", "class": "0x020000", "numa_node": "0", "enable": "1",
	}, "vmxnet3")
	f.dev(t, "0000:00:1c.0", map[string]string{
		"vendor": "0x8086", "device": "0xa135", "class": "0x060400",
	}, "pcieport")
	f.group(t, 7, "0000:0b:00.0", "0000:00:1c.0")

	inv, err := f.read(t)
	if err != nil {
		t.Fatalf("readPCIInventory: %v", err)
	}
	if !inv.IOMMUAvailable {
		t.Fatalf("组目录可读时 IOMMUAvailable 应为 true：%+v", inv)
	}
	d := f.findDev(t, inv, "0000:0b:00.0")
	if d.IOMMUGroup != 7 {
		t.Fatalf("组号应按组目录反查得 7，实得 %d", d.IOMMUGroup)
	}
	if d.GroupSize != 2 {
		t.Fatalf("组内设备数应为 2，实得 %d（%v）", d.GroupSize, d.GroupDevices)
	}
	if got := f.groupNames(d.GroupDevices); got != f.groupNames([]string{"0000:00:1c.0", "0000:0b:00.0"}) {
		t.Fatalf("组内 BDF 列表应按序含两台设备：%s", got)
	}
	if !d.GroupHasBridge {
		t.Fatalf("组内含 class 0604（root port）时应标 GroupHasBridge")
	}
	if got := d.PassthroughKind(); got != PassthroughGroupHasBridge {
		t.Fatalf("直通分类应为 %q，实得 %q", PassthroughGroupHasBridge, got)
	}
	// 桥自身也在组里：它不是 vfio 绑定，按「同组含桥」呈现（不因自己是桥就特殊对待）。
	if br := f.findDev(t, inv, "0000:00:1c.0"); br.IOMMUGroup != 7 || br.GroupSize != 2 {
		t.Fatalf("桥设备同样属该组：%+v", br)
	}
}

// TestPCIInventoryGroupWithoutBridgeIsUsable 组内只有端点设备 ⇒ 直通建议「可用」。
func TestPCIInventoryGroupWithoutBridgeIsUsable(t *testing.T) {
	f := newPCIFixture(t)
	f.dev(t, "0000:13:00.0", map[string]string{
		"vendor": "0x15ad", "device": "0x07b0", "class": "0x020000",
	}, "")
	f.group(t, 8, "0000:13:00.0")

	inv, err := f.read(t)
	if err != nil {
		t.Fatalf("readPCIInventory: %v", err)
	}
	d := f.findDev(t, inv, "0000:13:00.0")
	if d.IOMMUGroup != 8 || d.GroupSize != 1 || d.GroupHasBridge {
		t.Fatalf("单端点组应得 group=8/size=1/hasBridge=false：%+v", d)
	}
	if got := d.PassthroughKind(); got != PassthroughOK {
		t.Fatalf("直通分类应为 %q，实得 %q", PassthroughOK, got)
	}
}

// TestPCIInventoryVfioBound 已绑 vfio-pci ⇒ 分类 bound（与组是否含桥无关：设备已交给直通驱动）。
func TestPCIInventoryVfioBound(t *testing.T) {
	f := newPCIFixture(t)
	f.dev(t, "0000:13:00.1", map[string]string{
		"vendor": "0x15ad", "device": "0x07b0", "class": "0x020000",
	}, VFIOPCIDriver)
	f.group(t, 9, "0000:13:00.1")

	inv, err := f.read(t)
	if err != nil {
		t.Fatalf("readPCIInventory: %v", err)
	}
	d := f.findDev(t, inv, "0000:13:00.1")
	if d.Driver != VFIOPCIDriver {
		t.Fatalf("driver 应为 %s：%q", VFIOPCIDriver, d.Driver)
	}
	if got := d.PassthroughKind(); got != PassthroughBound {
		t.Fatalf("直通分类应为 %q，实得 %q", PassthroughBound, got)
	}
}

// TestPCIInventoryNoIOMMUGroup 无组设备：组号 -1、组内数 0、「不可用（无 IOMMU 组）」。
// 也覆盖「驱动未绑」：没有 driver 链接 ⇒ Driver 为空（调用方渲染 `-`）。
func TestPCIInventoryNoIOMMUGroup(t *testing.T) {
	f := newPCIFixture(t)
	f.dev(t, "0000:00:16.0", map[string]string{
		"vendor": "0x8086", "device": "0xa135", "class": "0x078000",
	}, "")
	f.group(t, 3, "0000:00:1f.0") // 别的组，与本设备无关

	inv, err := f.read(t)
	if err != nil {
		t.Fatalf("readPCIInventory: %v", err)
	}
	d := f.findDev(t, inv, "0000:00:16.0")
	if d.IOMMUGroup != -1 || d.GroupSize != 0 || len(d.GroupDevices) != 0 {
		t.Fatalf("无组设备应得 group=-1/size=0/无成员：%+v", d)
	}
	if d.Driver != "" {
		t.Fatalf("未绑驱动时 Driver 应为空：%q", d.Driver)
	}
	if got := d.PassthroughKind(); got != PassthroughNoGroup {
		t.Fatalf("直通分类应为 %q，实得 %q", PassthroughNoGroup, got)
	}
}

// TestPCIInventoryGroupClassUnreadable 组内某设备 class 读不到 ⇒ 不断言「安全」（GroupClassesKnown=false）。
func TestPCIInventoryGroupClassUnreadable(t *testing.T) {
	f := newPCIFixture(t)
	f.dev(t, "0000:0a:00.0", map[string]string{"vendor": "0x8086", "device": "0x1234", "class": "0x020000"}, "")
	// 伙伴没有 class 文件（只有目录）：桥判定不完整
	if err := os.MkdirAll(filepath.Join(f.devs, f.name("0000:0a:00.1")), 0o755); err != nil {
		t.Fatal(err)
	}
	f.group(t, 11, "0000:0a:00.0", "0000:0a:00.1")

	inv, err := f.read(t)
	if err != nil {
		t.Fatalf("readPCIInventory: %v", err)
	}
	d := f.findDev(t, inv, "0000:0a:00.0")
	if d.GroupClassesKnown {
		t.Fatalf("组内伙伴 class 读不到时不得标为「class 齐全」：%+v", d)
	}
	if got := d.PassthroughKind(); got != PassthroughUnknown {
		t.Fatalf("直通分类应为 %q（不猜安全），实得 %q", PassthroughUnknown, got)
	}
}

// TestPCIInventoryMissingIOMMUDir 内核未启用 IOMMU（组目录不存在）：
// 不是读取失败，而是「无组」这一事实——所有设备按不可直通呈现，不误报 error。
func TestPCIInventoryMissingIOMMUDir(t *testing.T) {
	f := newPCIFixture(t)
	f.dev(t, "0000:0b:00.0", map[string]string{"vendor": "0x15ad", "device": "0x07b0", "class": "0x020000"}, "")
	if err := os.RemoveAll(f.groups); err != nil {
		t.Fatal(err)
	}

	inv, err := f.read(t)
	if err != nil {
		t.Fatalf("组目录不存在不该算读取失败：%v", err)
	}
	if inv.IOMMUAvailable {
		t.Fatalf("组目录不存在时 IOMMUAvailable 应为 false")
	}
	if inv.IOMMUError != "" {
		t.Fatalf("「目录不存在」不是读取错误，不该记原因：%q", inv.IOMMUError)
	}
	if got := f.findDev(t, inv, "0000:0b:00.0").PassthroughKind(); got != PassthroughNoGroup {
		t.Fatalf("无组目录时按无组呈现：%q", got)
	}
}

// TestPCIInventoryUnreadablePCIRoot PCI 目录不可读 ⇒ 返回 error（不谎报空清单）。
func TestPCIInventoryUnreadablePCIRoot(t *testing.T) {
	f := newPCIFixture(t)
	if err := os.RemoveAll(f.devs); err != nil {
		t.Fatal(err)
	}
	inv, err := f.read(t)
	if err == nil {
		t.Fatalf("PCI 目录不可读时必须返回 error（否则空清单会被读成「本机没有 PCI 设备」）：%+v", inv)
	}
	if !strings.Contains(err.Error(), f.devs) {
		t.Fatalf("错误文案应点名目录：%v", err)
	}
}

// TestPCIInventorySortedAndDetailFields 清单按 BDF 升序；numa_node/enable 读不到时为 -1。
func TestPCIInventorySortedAndDetailFields(t *testing.T) {
	f := newPCIFixture(t)
	f.dev(t, "0000:13:00.0", map[string]string{"vendor": "0x15ad", "device": "0x07b0", "class": "0x020000", "label": "SLOT 3\n"}, "")
	f.dev(t, "0000:00:16.0", map[string]string{"vendor": "0x8086", "device": "0xa135", "class": "0x078000"}, "")

	inv, err := f.read(t)
	if err != nil {
		t.Fatalf("readPCIInventory: %v", err)
	}
	want := f.name("0000:00:16.0") + "," + f.name("0000:13:00.0")
	if got := strings.Join(pciBDFs(inv), ","); got != want {
		t.Fatalf("清单应按 BDF 升序：%s（期望 %s）", got, want)
	}
	if got := f.findDev(t, inv, "0000:13:00.0").Label; got != "SLOT 3" {
		t.Fatalf("label 应去空白：%q", got)
	}
	d := f.findDev(t, inv, "0000:00:16.0")
	if d.NUMA != -1 || d.Enabled != -1 {
		t.Fatalf("numa_node/enable 读不到应为 -1（「取不到」≠ 0）：%+v", d)
	}
}

// TestPCIInventoryProductionSourceReadOnly 生产入口不写 sysfs（本文件是只读事实源）：
// 非 Linux 上直接调 ReadPCIInventory 应返回 error 而不是 panic；Linux 上返回 error 也要可读。
func TestPCIInventoryProductionSourceReadOnly(t *testing.T) {
	inv, err := ReadPCIInventory()
	if err != nil {
		t.Logf("本机 sysfs 不可读（容器/非 Linux/权限）：%v", err)
		return
	}
	if len(inv.Devices) == 0 {
		t.Logf("本机 sysfs 未枚举到 PCI 设备（虚拟化环境可能如此）")
		return
	}
	t.Logf("本机枚举到 %d 台 PCI 设备（首台 %s，组 %d）", len(inv.Devices), inv.Devices[0].BDF, inv.Devices[0].IOMMUGroup)
}

// TestPCIInventoryHandlesSymlinkedDeviceEntries 设备条目本身是**符号链接**时仍要枚举到。
//
// 这是真机才暴露的一条（v3.0.30 验证现场抓出）：Linux 的 `/sys/bus/pci/devices/<bdf>` 是
// **指向 devices/pciXXXX:XX/… 的符号链接**，而 `DirEntry.IsDir()` 用 lstat 语义、对链接恒
// 返回 false。第一版按 `IsDir()` 过滤 ⇒ 本机 45 台设备全被跳过，命令输出「（未枚举到 PCI
// 设备）」——**一条看起来成功、事实为空的假读数**（上面的夹具都用真实目录，照不出这个差异）。
//
// 守护方式：把设备目录放到 devices 之外，再在 devices 下建**指向它的符号链接**（与 sysfs 同形）。
func TestPCIInventoryHandlesSymlinkedDeviceEntries(t *testing.T) {
	f := newPCIFixture(t)
	// 真实设备目录放在真实 sysfs 会有的位置（<root>/devices/pci0000:00/…），devices/ 下只放链接。
	realDir := filepath.Join(f.root, "devices", f.name("pci0000:00"), f.name("0000:00:1f.2"))
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"vendor": "0x8086\n", "device": "0x2829\n", "class": "0x010601\n", "enable": "1\n"} {
		if err := os.WriteFile(filepath.Join(realDir, k), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(f.devs, f.name("0000:00:1f.2"))
	if err := os.Symlink(realDir, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("跳过：本环境无法创建符号链接（%v）", err)
		}
		t.Fatal(err)
	}
	// 组目录下的条目同样是符号链接（sysfs 形态）。
	if err := os.MkdirAll(filepath.Join(f.groups, "5", "devices"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(link, filepath.Join(f.groups, "5", "devices", f.name("0000:00:1f.2"))); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("跳过：本环境无法创建符号链接（%v）", err)
		}
		t.Fatal(err)
	}

	inv, err := readPCIInventory(f.devs, f.groups)
	if err != nil {
		t.Fatalf("读清单: %v", err)
	}
	if len(inv.Devices) != 1 {
		t.Fatalf("符号链接形态的设备条目必须被枚举到（IsDir() 对链接恒 false——这正是真机输出「未枚举到 PCI 设备」的原因）；实得 %d 台：%+v",
			len(inv.Devices), inv.Devices)
	}
	d := inv.Devices[0]
	if d.Vendor != "8086" || d.Device != "2829" {
		t.Fatalf("经符号链接也要读到真实字段（vendor/device）：%+v", d)
	}
	if d.IOMMUGroup != 5 {
		t.Fatalf("经符号链接也要反查到 IOMMU 组 5，实得 %d", d.IOMMUGroup)
	}
}
