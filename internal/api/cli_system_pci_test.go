package api

// 修 BUG-24：`show system pci [detail]` 的读视图守护。
//
// 缺陷原状：产品只有 `set virtual-machine-functions <n> pci-device <bdf>`，**没有任何命令能列出**
// PCI 设备/IOMMU 组/驱动——运维必须 ssh 进 shell 跑 lspci 才知道该填什么 BDF，且无法判断某个
// BDF 是否与无关设备同组（真机现场：`0000:0b:00.0` 落在 group 7，同组还有 8 个 PCIe root port）。
//
// 本文件覆盖四件事（都是「如实、不猜」的具体形态）：
//  ① 列表：列头 + 表头 + 口径行必须在（空清单/口径缺失即失败）；
//  ② 三个关键判定：组含桥 ⇒「需整组直通」、无组 ⇒「不可用（无 IOMMU 组）」、未绑驱动 ⇒ 驱动列 `-`；
//  ③ `detail`：每设备明细块含同组 BDF 列表与 enable/numa_node/class；
//  ④ 事实源取不到 ⇒ 报「取不到 + 原因」，**不 panic、不谎报空清单**。
//
// 事实源经包级变量 `pciInventorySource` 注入（不扩大 cliExecutor 字段/Options 注入面——
// 本命令不随数据面/编排器状态变化，与 pciExists 的取舍不同，见 cli_system_pci.go 文件头）。

import (
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/orchestrator/compute"
)

// withPCISource 临时替换 PCI 清单事实源（t.Cleanup 还原）。
func withPCISource(t *testing.T, src func() (compute.PCIInventory, error)) {
	t.Helper()
	old := pciInventorySource
	pciInventorySource = src
	t.Cleanup(func() { pciInventorySource = old })
}

// pciSampleInventory 真机现场的形态：一台网卡与 8 个 root port 同组（group 7，共 9 台）、
// 一台已绑 vfio 的独立设备、一台无 IOMMU 组的控制器（未绑驱动）。
func pciSampleInventory() compute.PCIInventory {
	group := []string{"0000:00:1c.0", "0000:00:1c.1", "0000:00:1c.2", "0000:00:1c.3",
		"0000:00:1c.4", "0000:00:1c.5", "0000:00:1c.6", "0000:00:1c.7", "0000:0b:00.0"}
	bridge := func(bdf string) compute.PCIDeviceFacts {
		return compute.PCIDeviceFacts{
			BDF: bdf, Vendor: "8086", Device: "a135", Class: "060400", Driver: "pcieport",
			IOMMUGroup: 7, GroupDevices: group, GroupSize: 9,
			GroupHasBridge: true, GroupClassesKnown: true, NUMA: 0, Enabled: 1,
		}
	}
	devs := []compute.PCIDeviceFacts{
		{
			BDF: "0000:0b:00.0", Vendor: "15ad", Device: "07b0", Class: "020000", Driver: "vmxnet3",
			IOMMUGroup: 7, GroupDevices: group, GroupSize: 9, GroupHasBridge: true, GroupClassesKnown: true,
			NUMA: 0, Enabled: 1,
		},
		{
			BDF: "0000:13:00.0", Vendor: "15ad", Device: "07b0", Class: "020000", Driver: compute.VFIOPCIDriver,
			IOMMUGroup: 8, GroupDevices: []string{"0000:13:00.0"}, GroupSize: 1, GroupClassesKnown: true,
			NUMA: 1, Enabled: 1,
		},
		{
			BDF: "0000:00:16.0", Vendor: "8086", Device: "a135", Class: "078000",
			IOMMUGroup: -1, NUMA: 0, Enabled: 1,
		},
	}
	for _, bdf := range group[:8] {
		devs = append(devs, bridge(bdf))
	}
	return compute.PCIInventory{Devices: devs, IOMMUAvailable: true}
}

// lineOf 取**内容行**（首列恰为该值的那一行）——避免表头/明细块里的同名 token 误命中。
func lineOf(out, firstCol string) string {
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, firstCol) {
			return ln
		}
	}
	return ""
}

// TestShowSystemPCIListsColumnsAndVerdicts ① + ②：表头、口径行与三条关键判定。
func TestShowSystemPCIListsColumnsAndVerdicts(t *testing.T) {
	withPCISource(t, func() (compute.PCIInventory, error) { return pciSampleInventory(), nil })
	x, _ := newCLIKit(t)

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system pci").Output
	if strings.HasPrefix(out, "%") {
		t.Fatalf("show system pci 不该报错：%s", out)
	}
	for _, want := range []string{"BDF", "厂商:设备", "类型", "IOMMU 组", "组内设备", "驱动", "直通"} {
		if !strings.Contains(out, want) {
			t.Errorf("表头缺 %q：\n%s", want, out)
		}
	}
	// 来源行与口径行：来源写清 sysfs 落点；口径写清「组含桥要整组隔离」「最终以 libvirt 报错为准」
	// 以及名称为什么只有 ID（不承诺做不到的）。
	for _, want := range []string{
		compute.PCIDevicesDir, compute.IOMMUGroupsDir,
		"含桥/root port", "最终以 libvirt 报错为准", "pci.ids",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("口径/来源缺 %q：\n%s", want, out)
		}
	}

	// 组含桥（真机现场形态）：组号 7、组内 9 台、直通「需整组直通」。
	nic := lineOf(out, "0000:0b:00.0")
	if nic == "" {
		t.Fatalf("清单缺 0000:0b:00.0：\n%s", out)
	}
	for _, want := range []string{"15ad:07b0", "Ethernet", "7 ", "9 ", "vmxnet3", "可用（同组含桥，需整组直通）"} {
		if !strings.Contains(nic, want) {
			t.Errorf("网卡行缺 %q：%q", want, nic)
		}
	}
	// 已绑 vfio：如实标「已在直通（vfio-pci）」。
	vfio := lineOf(out, "0000:13:00.0")
	if !strings.Contains(vfio, "已在直通（"+compute.VFIOPCIDriver+"）") {
		t.Errorf("已绑 vfio 的设备应标已在直通：%q", vfio)
	}
	// 无 IOMMU 组 + 未绑驱动：组列 `-`，建议「不可用（无 IOMMU 组）」，驱动列 `-`。
	nogroup := lineOf(out, "0000:00:16.0")
	if strings.Contains(nogroup, "\t") {
		t.Fatalf("列应是空格对齐（不出现制表符）：%q", nogroup)
	}
	if !strings.Contains(nogroup, "不可用（无 IOMMU 组）") {
		t.Errorf("无组设备应判不可用：%q", nogroup)
	}
	// 该行第一个 `-` 应是组列（组号取不到）；驱动列同为零值 → 行内至少出现两处 `-`。
	if strings.Count(nogroup, "-") < 3 { // 「不可用（无 IOMMU 组）」里的 `-` 不算在列上，故看列位置另判
		t.Fatalf("无组且未绑驱动的行应有多处 `-`：%q", nogroup)
	}
	fields := strings.Fields(nogroup)
	if len(fields) < 6 {
		t.Fatalf("行字段不足以判定列：%q", nogroup)
	}
	// fields: BDF 厂商:设备 类型 IOMMU组 组内设备 驱动 直通…
	if fields[1] != "8086:a135" || fields[3] != "-" || fields[4] != "-" || fields[5] != "-" {
		t.Errorf("无组/未绑驱动行的列应为 <id> 类型 - - -：%q", nogroup)
	}
}

// TestShowSystemPCIDetailBlock ③：detail 追加明细块，含 class/enable/numa_node/同组 BDF 列表。
func TestShowSystemPCIDetailBlock(t *testing.T) {
	withPCISource(t, func() (compute.PCIInventory, error) { return pciSampleInventory(), nil })
	x, _ := newCLIKit(t)

	plain := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system pci").Output
	detail := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system pci detail").Output
	if strings.HasPrefix(detail, "%") {
		t.Fatalf("show system pci detail 不该报错：%s", detail)
	}
	if len(detail) <= len(plain) {
		t.Fatalf("detail 应比列表多出明细块：plain=%d detail=%d", len(plain), len(detail))
	}
	for _, want := range []string{
		"iommu_group", "passthrough", "numa_node", "enable", "vendor:device",
		"020000",                          // class 原文 + 类型文本
		"（9 台：0000:00:1c.0, 0000:00:1c.1", // 同组 BDF 列表
		"0000:00:1c.7, 0000:0b:00.0）",     // 同组列表里含自身（按 BDF 升序排末位）
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail 明细缺 %q：\n%s", want, detail)
		}
	}
	// 列表形态**不该**出现明细块（两种形态必须可区分）。判据用明细行的形态（缩进两格的字段行），
	// 不用裸字段名——口径行里本来就写着 `/sys/kernel/iommu_groups/`。
	if strings.Contains(plain, "\n  vendor:device") || strings.Contains(plain, "\n  passthrough") {
		t.Errorf("非 detail 形态不该输出明细块：\n%s", plain)
	}
}

// TestShowSystemPCISourceUnavailableFailsUp ④：取不到事实源 ⇒ 报「取不到 + 原因」，
// 不返回空清单（空清单会被读成「本机没有 PCI 设备」），也不 panic。
func TestShowSystemPCISourceUnavailableFailsUp(t *testing.T) {
	withPCISource(t, func() (compute.PCIInventory, error) {
		return compute.PCIInventory{}, errors.New("PCI 设备目录 /sys/bus/pci/devices 不可读: permission denied")
	})
	x, _ := newCLIKit(t)

	for _, cmd := range []string{"show system pci", "show system pci detail"} {
		out := x.Execute("admin", aaa.ClassSuperUser, "ssh", cmd).Output
		if !strings.HasPrefix(out, "%%") {
			t.Fatalf("%s 应在取不到事实源时报错：%q", cmd, out)
		}
		if !strings.Contains(out, "取不到") || !strings.Contains(out, "permission denied") {
			t.Errorf("%s 的报错应含「取不到」与原因：%q", cmd, out)
		}
		if strings.Contains(out, "BDF") {
			t.Errorf("%s 取不到时不该打出表头（会被读成空清单）：%q", cmd, out)
		}
	}
}

// TestShowSystemPCIUnreadableIOMMUFallsUp 组信息读取失败（目录在但读不了）：
// 设备仍列（PCI 事实可读），页脚如实说明 IOMMU 取不到，全部按「无组」呈现——不据此断言可直通。
func TestShowSystemPCIUnreadableIOMMUFallsUp(t *testing.T) {
	inv := compute.PCIInventory{
		Devices: []compute.PCIDeviceFacts{
			{BDF: "0000:0b:00.0", Vendor: "15ad", Device: "07b0", Class: "020000", Driver: "vmxnet3", IOMMUGroup: -1},
		},
		IOMMUAvailable: false,
		IOMMUError:     "IOMMU 组目录 /sys/kernel/iommu_groups 不可读: permission denied",
	}
	withPCISource(t, func() (compute.PCIInventory, error) { return inv, nil })
	x, _ := newCLIKit(t)

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system pci").Output
	if strings.HasPrefix(out, "%") {
		t.Fatalf("PCI 事实可读时不该整条报错：%s", out)
	}
	if !strings.Contains(out, "IOMMU：取不到组信息") || !strings.Contains(out, "permission denied") {
		t.Errorf("IOMMU 取不到时应如实说明原因：\n%s", out)
	}
	if !strings.Contains(out, "不可用（无 IOMMU 组）") {
		t.Errorf("组信息取不到时按无组呈现（不猜可直通）：\n%s", out)
	}
}

// TestShowSystemPCIEmptyInventoryIsStated 空清单（目录可读但无设备）如实报，不静默空白。
func TestShowSystemPCIEmptyInventoryIsStated(t *testing.T) {
	withPCISource(t, func() (compute.PCIInventory, error) {
		return compute.PCIInventory{IOMMUAvailable: true}, nil
	})
	x, _ := newCLIKit(t)
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system pci").Output
	if strings.HasPrefix(out, "%") {
		t.Fatalf("空清单不是错误：%s", out)
	}
	if !strings.Contains(out, "未枚举到 PCI 设备") {
		t.Fatalf("空清单应如实说明：%s", out)
	}
	if !strings.Contains(out, "pci.ids") {
		t.Errorf("空态也要给口径行（名称只有 ID 的原因）：\n%s", out)
	}
}

// TestShowSystemPCIInvalidSubcommand 多余/未知 token 如实报错并给可用形态（不静默当列表作答）。
func TestShowSystemPCIInvalidSubcommand(t *testing.T) {
	withPCISource(t, func() (compute.PCIInventory, error) { return pciSampleInventory(), nil })
	x, _ := newCLIKit(t)
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system pci bogus").Output
	if !strings.HasPrefix(out, "%%") || !strings.Contains(out, "detail") {
		t.Fatalf("未知子命令应报错并列出可用形态：%q", out)
	}
	if strings.Contains(out, "0000:0b:00.0") {
		t.Errorf("报错时不得顺带输出清单（静默误答）：%q", out)
	}
}

// TestShowSystemPCIColumnAlignment 列对齐按**显示宽度**（中文算 2 列）：六列之后「直通」列
// 在所有数据行与表头上必须起于同一显示列——纯 rune 补齐会把中文行后面的列挤歪，读起来像串行。
func TestShowSystemPCIColumnAlignment(t *testing.T) {
	withPCISource(t, func() (compute.PCIInventory, error) { return pciSampleInventory(), nil })
	x, _ := newCLIKit(t)
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system pci").Output

	// 「直通」列的起始显示列 = 前六列宽度之和（15+1+11+1+14+1+10+1+10+1+14+1 = 80）。
	const want = 80
	checked := 0
	for _, ln := range strings.Split(out, "\n") {
		if !strings.HasPrefix(ln, "0000:") {
			continue
		}
		if got := pciDisplayWidth(ln[:byteIndexAtWidth(ln, want)]); got != want {
			t.Errorf("行的「直通」列未起于第 %d 显示列（实得 %d）：%q", want, got, ln)
		}
		checked++
	}
	if checked < 3 {
		t.Fatalf("只检查到 %d 行（解析器可能失效）：\n%s", checked, out)
	}
	// 表头同一口径。
	head := lineOf(out, "BDF ")
	if head == "" {
		t.Fatalf("缺表头：\n%s", out)
	}
	if got := pciDisplayWidth(head[:byteIndexAtWidth(head, want)]); got != want {
		t.Errorf("表头的「直通」列未起于第 %d 显示列（实得 %d）：%q", want, got, head)
	}
}

// byteIndexAtWidth 返回「显示宽度首次达到 w 列」的字节下标（够不到则返回串长）。
func byteIndexAtWidth(s string, w int) int {
	n := 0
	for i, r := range s {
		if n >= w {
			return i
		}
		n += pciRuneWidth(r)
	}
	return len(s)
}

// pciDisplayWidth 一串文本的显示宽度（中文/全角算 2 列）。
func pciDisplayWidth(s string) int {
	n := 0
	for _, r := range s {
		n += pciRuneWidth(r)
	}
	return n
}

// TestShowSystemPCIPadHelper pciPad 的边界：够宽原样返回（不截断）、不足补到显示宽度、中文算 2。
func TestShowSystemPCIPadHelper(t *testing.T) {
	cases := []struct {
		in   string
		w    int
		want string
	}{
		{"abc", 5, "abc  "},
		{"abcde", 5, "abcde"},
		{"abcdef", 5, "abcdef"},    // 不截断
		{"通信控制器", 14, "通信控制器    "}, // 6 个全角 = 12 列 → 补 2 个空格
		{"-", 2, "- "},
		{"", 3, "   "},
	}
	for _, c := range cases {
		if got := pciPad(c.in, c.w); got != c.want {
			t.Errorf("pciPad(%q, %d) = %q，期望 %q", c.in, c.w, got, c.want)
		}
	}
}

// TestShowSystemSyntaxHintListsAcceptedSubcommands `show system` 诊断分发器在没有子命令时的
// 语法提示串，必须列出它**实际接受**的主要子命令——本轮补上此前漏列的 `software`，
// 并加入新增的 `pci`。
//
// 注：`show system` 单独敲时由操作树判定（`show system` 是容器节点、孩子都是必选子命令），
// 落到 cliexec 的通用 fallback；本用例直接调分派助手，钉的是**这串自描述不腐化**——
// 同一事实两个视图（提示串 vs case 分支），漏一个就等于让操作者以为没有这条命令。
func TestShowSystemSyntaxHintListsAcceptedSubcommands(t *testing.T) {
	x, _ := newCLIKit(t)
	out := x.execShowSystemDiag(nil)
	if !strings.Contains(out, "语法: show system") {
		t.Fatalf("无子命令时应报语法提示：%q", out)
	}
	for _, want := range []string{"uptime", "hugepages", "kernel", "hardware", "firewall", "pci", "software", "core-dumps", "tech-support"} {
		if !strings.Contains(out, want) {
			t.Errorf("语法提示串缺 %q（分发器实际接受它，漏列会让操作者以为没这条命令）：%q", want, out)
		}
	}
	// 反向：提示里列出的每个子命令都必须是分发器真认的（不写实现没有的）。
	// `metrics` 是**容器关键字**（真实形态是 `metrics history …`），故按它的完整形态探针。
	probe := map[string][]string{"metrics": {"metrics", "history"}}
	body := strings.TrimSuffix(strings.TrimPrefix(out, "%% 语法: show system <"), ">\n")
	body = strings.TrimPrefix(body, "%% 语法: show system <")
	for _, tok := range strings.Split(body, "|") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		args, ok := probe[tok]
		if !ok {
			args = []string{tok}
		}
		if got := x.execShowSystemDiag(args); strings.Contains(got, "无效命令") {
			t.Errorf("语法提示列出了 %q，但分发器判它无效——提示串与实现漂移", tok)
		}
	}
}

// TestShowSystemPCIStructured 管道形态（`| display json` 走 x.structured）与列同源。
func TestShowSystemPCIStructured(t *testing.T) {
	withPCISource(t, func() (compute.PCIInventory, error) { return pciSampleInventory(), nil })
	x, _ := newCLIKit(t)
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system pci")
	if res.Output == "" {
		t.Fatal("输出为空")
	}
	tree, ok := x.structured.(map[string]any)
	if !ok {
		t.Fatalf("structured 应为 map[string]any，实得 %T", x.structured)
	}
	items, ok := tree["pci_devices"].([]any)
	if !ok || len(items) != len(pciSampleInventory().Devices) {
		t.Fatalf("structured 应含全部设备条目：%T %v", tree["pci_devices"], tree)
	}
	first, _ := items[0].(map[string]any)
	for _, k := range []string{"bdf", "vendor", "device", "class", "driver", "iommu_group", "group_devices", "group_size", "passthrough"} {
		if _, ok := first[k]; !ok {
			t.Errorf("structured 条目缺字段 %q：%v", k, first)
		}
	}
	if tree["iommu_available"] != true {
		t.Errorf("iommu_available 应为 true：%v", tree["iommu_available"])
	}
}

// TestShowSystemPCIClassesAndDriverDash 类型列映射与 `-` 占位（空值不渲染成空串）。
func TestShowSystemPCIClassesAndDriverDash(t *testing.T) {
	cases := map[string]string{
		"020000": "Ethernet",
		"028000": "网络控制器",
		"060400": "PCI 桥",
		"030000": "显示控制器",
		"0c0330": "串行总线",
		"ff0000": "其它",
		"":       "-",
	}
	for class, want := range cases {
		if got := pciClassText(class); got != want {
			t.Errorf("class %q 的类型列应为 %q，实得 %q", class, want, got)
		}
	}
	if got := pciDash(""); got != "-" {
		t.Errorf("空值占位应为 -：%q", got)
	}
	if got := pciVendorDevice(compute.PCIDeviceFacts{Vendor: "15ad"}); got != "15ad:-" {
		t.Errorf("半个 ID 应显式标出缺哪一侧（不拼半截）：%q", got)
	}
	if got := pciVendorDevice(compute.PCIDeviceFacts{}); got != "-" {
		t.Errorf("两侧都取不到应为 -：%q", got)
	}
}

// TestShowSystemPCIDirectSourceNotInjected 方案说明的守护：本命令的 PCI 事实源是**包级变量**
// 而非 cliExecutor 字段——装配侧（server.go / cliexec.go）不参与，故 `newCLIExecutor` 造出的
// 执行器即可用（零点组装配也不会「未接入」）。这条钉住「最小改动方案」不回退成注入面扩大。
func TestShowSystemPCIDirectSourceNotInjected(t *testing.T) {
	withPCISource(t, func() (compute.PCIInventory, error) { return pciSampleInventory(), nil })
	x, _ := newCLIKit(t) // 只装 engine/authz，未装配任何运行态
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system pci").Output
	if strings.HasPrefix(out, "%") {
		t.Fatalf("本命令不依赖编排器装配，零点组装配也应可读：%s", out)
	}
	if !strings.Contains(out, "0000:0b:00.0") {
		t.Fatalf("应列出设备：%s", out)
	}
}
