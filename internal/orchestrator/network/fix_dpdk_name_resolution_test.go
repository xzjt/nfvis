package network

// v3.0.30 四套件复跑抓到的死锁：按**口名**声明的 `vpp dpdk dev <口名>` 在口已被 DPDK
// 接管后无法解析 PCI，且三条自救路全部走不通。
//
// 现场（真机原文，`request vpp restart` 的生成期告警）：
//
//	配置中的 vpp dpdk dev ens192 未能解析 PCI 地址（读取 /sys/class/net/ens192/device/uevent:
//	open /sys/class/net/ens192/device/uevent: no such file or directory），已从 startup.conf 中跳过
//
// 而告警给的出路 `request interfaces ens192 bind-dpdk --yes` 在无 netdev 时必然失败：
//
//	%% 接口 ens192 无 PCI 设备（DPDK 已接管的网卡在内核中无 netdev，请改用 PCI 地址）:
//	   readlink /sys/class/net/ens192/device: no such file or directory
//
// 根因不是「解析失败」本身（那个有具名告警、是既有设计），而是**绑定时知道、绑完就丢了**：
// 绑定记录 `dpdk-bindings.json` 当时是单表，按 **BDF** 接管（`request interfaces 0000:0b:00.0
// bind-dpdk`）只留下 `PCI → PCI`，「口名 → BDF」这一维从未落盘；设备一进 vfio-pci，
// `/sys/class/net/<口>` 与 `/sys/bus/pci/devices/<bdf>/net/` 双双消失，任何来源都问不到这个名字。
//
// 本文件钉住四条不回退：
//  1. 记录有「口名 → BDF」时，netdev 不存在也能解析（正向）；
//  2. 记录里没有该口名时**不编造** BDF，仍走「跳过 + 具名告警」；
//  3. 旧格式（只有 BDF→BDF）能读、不报错、不 panic，且**不把 BDF 当口名**报给操作者；
//  4. 跳过告警给出的出路必须是**真能走通**的（按口名 bind-dpdk 在无 netdev 时是死路）；
//  5. 删除声明不依赖 PCI 解析（配置键消失即成功）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// legacyBindingsJSON 修复前的真实落盘形态（真机 /var/lib/nfvis/dpdk-bindings.json 原文）：
// 按 BDF 接管的两个口，只有 PCI→PCI，没有口名。
const legacyBindingsJSON = `{
  "0000:0b:00.0": "0000:0b:00.0",
  "0000:13:00.0": "0000:13:00.0"
}
`

// writeBindings 把给定内容写成记录文件并返回记录对象。
func writeBindings(t *testing.T, content string) *Bindings {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dpdk-bindings.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return NewBindings(path)
}

// goneResolver 模拟「口已被 DPDK 接管」：内核里没有 netdev，sysfs 解析必失败。
func goneResolver(name string) (string, error) { return "", errNotFound(name) }

// ① 记录含「口名 → BDF」时，netdev 不存在也能解析出 BDF（正向）。
//
// 这条正是真机缺的那一环：记录由**按口名绑定**（或修复后按 BDF 绑定的学习步骤）写入，
// 此后设备在内核里消失，startup.conf 生成仍按名字解析得到 BDF。
func TestFixNameResolutionFromRecordWithoutNetdev(t *testing.T) {
	rec := writeBindings(t, `{
  "format": 2,
  "ifname_to_pci": {"ens192": "0000:0b:00.0", "ens224": "0000:13:00.0"},
  "pci_to_ifname": {"0000:0b:00.0": "ens192", "0000:13:00.0": "ens224"}
}
`)
	// 记录本身可读（不经过 netdev）
	if pci, ok := rec.Get("ens192"); !ok || pci != "0000:0b:00.0" {
		t.Fatalf("记录应直接给出 ens192 的 BDF: %q %v", pci, ok)
	}
	resolve := PCIResolverWithBindings(goneResolver, rec)

	cfg := &model.Config{Vpp: &model.VppConfig{DPDK: &model.VppDPDK{PerDev: []model.VppDevOverride{
		{Interface: "ens192", RxQueues: 1},
		{Interface: "ens224", RxQueues: 1},
	}}}}
	out, warnings, err := GenerateStartupWithWarnings(cfg, resolve)
	if err != nil {
		t.Fatalf("记录里有口名时不应失败: %v", err)
	}
	// 两个口都必须进 dpdk 段，且带正确的 name（VPP 侧按名解析 sw_if_index）
	for _, want := range []string{
		"dev 0000:0b:00.0 {\n    name ens192\n",
		"dev 0000:13:00.0 {\n    name ens224\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("startup.conf 缺少 %q:\n%s", want, out)
		}
	}
	if len(warnings) != 0 {
		t.Fatalf("两处都能解析时不应有告警: %v", warnings)
	}
	// 反向索引同源（按 PCI 操作时反查口名）
	if name, ok := rec.NameOfPCI("0000:13:00.0"); !ok || name != "ens224" {
		t.Fatalf("按 PCI 应反查到口名: %q %v", name, ok)
	}
}

// ② 记录里没有该口名且 netdev 不存在 → 仍走「跳过 + 具名告警」，**不编造** BDF。
func TestFixUnresolvedNameStillSkipsWithoutGuessing(t *testing.T) {
	rec := writeBindings(t, legacyBindingsJSON) // 只有 PCI→PCI，没有 ens192
	resolve := PCIResolverWithBindings(goneResolver, rec)

	// 反证：记录里的 BDF 不被当成「ens192 的 BDF」（否则会把两个口错配成同一设备）
	if pci, ok := rec.Get("ens192"); ok {
		t.Fatalf("记录里没有口名时不得凭空给出 BDF: %q", pci)
	}
	cfg := &model.Config{Vpp: &model.VppConfig{DPDK: &model.VppDPDK{PerDev: []model.VppDevOverride{
		{Interface: "ens192", RxQueues: 1},
	}}}}
	out, warnings, err := GenerateStartupWithWarnings(cfg, resolve)
	if err != nil {
		t.Fatalf("解析失败不应让整份生成失败（其余口会一起起不来）: %v", err)
	}
	if strings.Contains(out, "dev ") {
		t.Fatalf("解析不到时不应猜一个 dev 块出来:\n%s", out)
	}
	joined := strings.Join(warnings, "\n")
	if len(warnings) == 0 || !strings.Contains(joined, "vpp dpdk dev ens192") || !strings.Contains(joined, "跳过") {
		t.Fatalf("必须具名报出被跳过的口: %v", warnings)
	}
}

// ③ 旧格式能读、不 panic；BDF 不被当成口名（真机症状②：告警曾劝操作者
// `set vpp dpdk dev 0000:0b:00.0`，而该值过不了接口名校验）。
func TestFixLegacyRecordReadableAndNotReportedAsPort(t *testing.T) {
	rec := writeBindings(t, legacyBindingsJSON)

	// 不 panic、不报错：口名表为空，但设备被如实记为「已接管、口名未知」
	all := rec.All()
	if len(all) != 0 {
		t.Fatalf("旧格式只有 BDF→BDF 时口名表应为空，实际 %v", all)
	}
	unnamed := rec.UnnamedPCIs()
	if len(unnamed) != 2 || unnamed[0] != "0000:0b:00.0" || unnamed[1] != "0000:13:00.0" {
		t.Fatalf("旧格式里的设备应被如实保留为「口名未知」: %v", unnamed)
	}
	// 旧语义保留：入参本身是已知的已接管 BDF 时返回它自己（按 PCI 操作的定位不回归）
	if pci, ok := rec.Get("0000:0b:00.0"); !ok || pci != "0000:0b:00.0" {
		t.Fatalf("已知的已接管 BDF 应自映射: %q %v", pci, ok)
	}
	// 真机症状②：旧实现把 BDF 当「口」报进掉口告警，并劝 `set vpp dpdk dev 0000:0b:00.0`
	// ——那条命令过不了接口名校验（真机实测：「接口名称 "0000:0b:00.0" 不合法」），
	// 是条无法照做的建议。修复后该设备单独一行如实报出（「记录里没有口名」），不给这条建议。
	var warns []string
	applier := &Applier{
		Path:     filepath.Join(t.TempDir(), "startup.conf"),
		Write:    func(string, []byte) error { return nil },
		PCI:      PCIResolverWithBindings(goneResolver, rec),
		Bindings: rec,
		Warn:     func(m string) { warns = append(warns, m) },
	}
	if _, err := applier.Apply(context.Background(), &model.Config{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	joined := strings.Join(warns, "\n")
	if strings.Contains(joined, "set vpp dpdk dev 0000:0b:00.0") {
		t.Fatalf("不得建议 `set vpp dpdk dev <BDF>`（接口名校验不允许冒号）: %q", joined)
	}
	if strings.Contains(joined, "未在配置中声明") && strings.Contains(joined, "0000:0b:00.0") {
		t.Fatalf("BDF 不得被当作「未声明的物理口」报出: %q", joined)
	}
	// 但它必须如实可见：口名未知 ≠ 可以静默略过
	if !strings.Contains(joined, "没有口名") || !strings.Contains(joined, "0000:0b:00.0") {
		t.Fatalf("口名未知的设备必须如实报出（否则是静默丢失）: %q", joined)
	}
}

// ④ 跳过告警的出路必须真能走通：不得再劝操作者「按口名 bind-dpdk」（无 netdev 时必失败）。
func TestFixSkipWarningGivesWalkablePaths(t *testing.T) {
	cfg := &model.Config{Vpp: &model.VppConfig{DPDK: &model.VppDPDK{PerDev: []model.VppDevOverride{
		{Interface: "ens192", RxQueues: 1},
	}}}}
	_, warnings, err := GenerateStartupWithWarnings(cfg, goneResolver)
	if err != nil {
		t.Fatalf("GenerateStartupWithWarnings: %v", err)
	}
	msg := strings.Join(warnings, "\n")
	// 死路：按口名 bind-dpdk 在无 netdev 的现场不可能成功（真机原文见文件头）
	if strings.Contains(msg, "request interfaces ens192 bind-dpdk") {
		t.Fatalf("告警不得再给「按口名 bind-dpdk」这条死路（无 netdev 时必失败）:\n%s", msg)
	}
	for _, want := range []string{
		"delete vpp dpdk dev ens192", // 出路②：删声明（纯配置键，永远可执行）
		"unbind-dpdk",                // 交还内核（按 BDF）
		"<BDF>",                      // 按 PCI 定位（真正可用的那一维）
		"show system pci",            // 怎么拿到 BDF
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("告警缺少可照做的一步 %q:\n%s", want, msg)
		}
	}
}

// ⑤ 删除声明不依赖 PCI 解析：解析不出 PCI 的现场，删掉该键即成功、生成干净。
//
// 这条是「操作者必须能自救」的红线：删除只是「从配置里去掉声明」，与「能不能解析到 PCI」
// 无关。真机上 `delete vpp dpdk dev <口名>` 走的是配置树按**键**匹配（
// internal/api 的 dpdkPerDev），全程不碰 PCI；本包不得在这条路上插入任何解析/校验依赖。
func TestFixDeleteDeclarationIndependentOfPCIResolution(t *testing.T) {
	rec := writeBindings(t, legacyBindingsJSON)
	resolve := PCIResolverWithBindings(goneResolver, rec)

	cfg := &model.Config{
		Interfaces: []model.InterfaceConfig{{Name: "ens192"}},
		Vpp:        &model.VppConfig{DPDK: &model.VppDPDK{PerDev: []model.VppDevOverride{{Interface: "ens192", RxQueues: 1}}}},
	}
	// 前提：这个现场确实解析不出 PCI（否则本用例证明不了「解析失败也能删」）
	if _, err := resolve("ens192"); err == nil {
		t.Fatal("前置不成立：本用例要求 ens192 解析失败")
	}
	if _, _, err := GenerateStartupWithWarnings(cfg, resolve); err != nil {
		t.Fatalf("前置：解析失败不应让生成失败: %v", err)
	}

	// 删除的语义＝从声明里去掉该键（与 CLI `delete vpp dpdk dev ens192` 同效）
	cfg.Vpp.DPDK.PerDev = nil

	if errs := model.Validate(*cfg); len(errs) != 0 {
		t.Fatalf("删掉该声明后配置应可提交: %v", errs)
	}
	out, warnings, err := GenerateStartupWithWarnings(cfg, resolve)
	if err != nil {
		t.Fatalf("删掉该声明后生成不应失败: %v", err)
	}
	if strings.Contains(out, "ens192") || strings.Contains(out, "dev ") {
		t.Fatalf("删掉的声明不得再出现在 dpdk 段:\n%s", out)
	}
	if len(warnings) != 0 {
		t.Fatalf("删掉之后不应再有该口的跳过告警: %v", warnings)
	}
}

// ⑥ 绑定**成功时**记下「口名 → BDF」（这一维此前只在装配处记，绑定器自己不留）。
//
// 关键时序：必须在任何 sysfs 写之前读名字——设备一旦进 vfio-pci，内核里就没有 netdev 了。
func TestFixBindRecordsNameToPCI(t *testing.T) {
	root, _ := fakeSysfs(t, "ens224", "dev-ens224", "vmxnet3")
	b, _ := newTestBinder(t, root)
	rec := writeBindings(t, legacyBindingsJSON)
	b.Bindings = rec

	if _, err := b.Bind(context.Background(), "ens224", ""); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if pci, ok := rec.Get("ens224"); !ok || pci != "dev-ens224" {
		t.Fatalf("绑定成功应记下口名 → PCI: %q %v", pci, ok)
	}
	if name, ok := rec.NameOfPCI("dev-ens224"); !ok || name != "ens224" {
		t.Fatalf("反向索引应同步: %q %v", name, ok)
	}
	// 落盘可被新实例读回（重启 nfvisd 后仍有效）
	if pci, ok := NewBindings(rec.Path).Get("ens224"); !ok || pci != "dev-ens224" {
		t.Fatalf("记录应已落盘: %q %v", pci, ok)
	}
	// 旧格式里的两条 PCI→PCI 不能被这次写入抹掉（向后兼容：只增不减）
	if len(rec.UnnamedPCIs()) != 2 {
		t.Fatalf("既有 PCI 记录不应丢失: %v", rec.UnnamedPCIs())
	}
}

// ⑦ 按 **BDF** 接管时，名字在绑定前从 sysfs 反查（`/sys/bus/pci/devices/<bdf>/net/`）。
//
// 这是真机记录变成「只有 PCI→PCI」的那条路径：操作者给的是 BDF，旧实现把 BDF 当键存进
// 单表，口名就此丢失。夹具里的设备名用连字符（Windows 文件名不允许冒号），与该路径的
// 语义无关——learnBindName 只看 `IsPCIAddr(入参)` 与 sysfs 下的 net/ 目录。
func TestFixBindByBDFLearnsKernelName(t *testing.T) {
	root := t.TempDir()
	// /sys/bus/pci/devices/<pci>/net/<口名>
	netDir := filepath.Join(root, "bus", "pci", "devices", "dev-ens192", "net", "ens192")
	if err := os.MkdirAll(netDir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := newTestBinder(t, root)
	rec := writeBindings(t, legacyBindingsJSON)
	b.Bindings = rec

	// 入参是 BDF 形态（IsPCIAddr 只做语法判断，与文件系统无关）
	b.learnBindName("0000:0b:00.0", "dev-ens192")
	if pci, ok := rec.Get("ens192"); !ok || pci != "dev-ens192" {
		t.Fatalf("按 BDF 接管应从 sysfs 学到口名: %q %v", pci, ok)
	}
	// 反证：sysfs 里也没有 netdev（设备已接管/未插卡）时不编造名字
	b.learnBindName("0000:13:00.0", "dev-unknown")
	if all := rec.All(); len(all) != 1 {
		t.Fatalf("名字未知时不得编造: %v", all)
	}
	if _, ok := rec.Get("0000:13:00.0"); !ok {
		t.Fatal("名字未知的设备仍应记为「已知已接管」（按 PCI 操作要用）")
	}
}
