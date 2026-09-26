package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

// 契约 §1.1（决策 #154）：`show interfaces <ifname>` 的动态候选来自 VPP 运行态清单
// （`vpp-ifnames`，决策 #83），**候选里的名字必须答得上来**——未在配置声明、但在清单里
// 的口（派生口 bvi0/vh-*、未声明的 DPDK 口）两种写法（裸 / `physical <ifname>`）都回
// 运行态单口视图（与 `show interfaces physical <ifname>` 同一实现）；两侧都不在
// （清单查询成功才可判）才报「未在配置中声明、也不在 VPP 接口清单中」。
//
// 背景：bvi0（虚拟交换机/NAT 应用时经 bvi_create 派生）、vh-<vm>-<nic>（VNF 网卡 attach
// 派生）永远不会出现在 cfg.Interfaces 里，`showOneInterface` 只认配置时，照着候选敲
// 三条全报错——候选 advertise 了执行器兑现不了的名字（决策 #153 同族）。

// undeclaredKit 端口清单里有派生口与物理口、配置里什么都没声明的执行器。
func undeclaredKit(t *testing.T) *cliExecutor {
	t.Helper()
	x, _ := newCLIKit(t)
	x.setPorts(fakePorts{vpp: []string{"bvi0", "vh-vnf-a-eth0", "vh-vnf-b-eth0", "ens192", "ens224"}})
	x.setVppState(fakeVppState{ifs: map[string]InterfaceState{
		"bvi0": {AdminUp: true, LinkUp: true, LinkSpeed: 10_000_000, DevType: "bvi"},
	}})
	return x
}

// TestShowInterfacesUndeclaredRuntimePortAnswers：未声明但在清单里的口必须答运行态视图，
// 裸写法与 detail/statistics 三写法逐字相同（未声明即无配置视图，计数就在行里）。
func TestShowInterfacesUndeclaredRuntimePortAnswers(t *testing.T) {
	x := undeclaredKit(t)
	bare := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces bvi0").Output
	if !strings.Contains(bare, "（接口 bvi0 未在配置中声明，以下为运行态视图）") {
		t.Fatalf("未声明口应回运行态视图并注明口径: %q", bare)
	}
	if !strings.Contains(bare, "up") || !strings.Contains(bare, "10G") || !strings.Contains(bare, "bvi") {
		t.Fatalf("运行态行应含 Admin/Link/Speed/Driver（取自 ifaceStates）: %q", bare)
	}
	if strings.Contains(bare, "也不在 VPP 接口清单中") {
		t.Fatalf("清单里的名字不得报「不在清单」: %q", bare)
	}
	if strings.HasPrefix(bare, "%") || strings.Contains(bare, "\n%") {
		t.Fatalf("运行态视图是正常作答，不得以错误行呈现: %q", bare)
	}
	for _, sub := range []string{"detail"} {
		got := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces bvi0 "+sub).Output
		if got != bare {
			t.Fatalf("未声明口 %s 应与裸写法逐字相同\n  %s: %q\n  裸: %q", sub, sub, got, bare)
		}
	}
	// statistics 一律回计数单行（决策 #155 统一口径）
	stat := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces bvi0 statistics").Output
	if !strings.Contains(stat, "interface bvi0 statistics:") && !strings.Contains(stat, "统计运行态不可用") {
		t.Fatalf("statistics 应回计数单行或如实说明不可用: %q", stat)
	}
}

// TestShowInterfacesPhysicalFormSameAsBareForUndeclared：`physical <ifname>`（不带子命令）
// 对未声明口与裸写法走同一实现，输出逐字相同（决策 #153「同一实现」口径的延伸）。
func TestShowInterfacesPhysicalFormSameAsBareForUndeclared(t *testing.T) {
	x := undeclaredKit(t)
	with := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces physical bvi0").Output
	without := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces bvi0").Output
	if with != without {
		t.Fatalf("未声明口两种写法应逐字相同\n  带 physical: %q\n  裸: %q", with, without)
	}
	if strings.Contains(with, "物理口 bvi0 未在配置中声明") {
		t.Fatalf("physical <ifname> 对清单里的名字不得报「物理口未声明」: %q", with)
	}
}

// TestShowInterfacesSriovAndInvalidSubForUndeclared：sriov 是空态（非错误），
// 未知子命令照旧报「无效命令」。
func TestShowInterfacesSriovAndInvalidSubForUndeclared(t *testing.T) {
	x := undeclaredKit(t)
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces bvi0 sriov").Output
	if !strings.Contains(out, "（接口 bvi0 未在配置中声明，无 SR-IOV 配置）") {
		t.Fatalf("未声明口的 sriov 应为空态并注明口径: %q", out)
	}
	if strings.HasPrefix(out, "%") {
		t.Fatalf("sriov 空态不是错误: %q", out)
	}
	bad := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces bvi0 bogus").Output
	if !strings.Contains(bad, "% 无效命令: show interfaces bvi0 bogus（可用：detail|statistics|sriov）") {
		t.Fatalf("未知子命令应报无效命令: %q", bad)
	}
}

// TestShowInterfacesUnknownNameNeedsInventorySuccess：判「不在清单」必须有**成功的**
// 清单查询——清单未接入或查询失败时维持既有文案（只陈述未声明，不否认存在），
// 与 interfaceKnown「宁可少报错」同取向。
func TestShowInterfacesUnknownNameNeedsInventorySuccess(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setPorts(fakePorts{vpp: []string{"ens192", "ens224"}})
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces ens999").Output
	if !strings.HasPrefix(out, "%") || !strings.Contains(out, "未在配置中声明、也不在 VPP 接口清单中") {
		t.Fatalf("清单查询成功且名字不在其中才可判「不在清单」: %q", out)
	}

	// 清单未接入：无从核对运行态，不得宣称「不在清单」
	x2, _ := newCLIKit(t)
	out = x2.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces ens999").Output
	if !strings.Contains(out, "未在配置中声明") {
		t.Fatalf("清单未接入仍应报未声明: %q", out)
	}
	if strings.Contains(out, "也不在 VPP 接口清单中") {
		t.Fatalf("清单未接入时不得判「不在清单」: %q", out)
	}

	// 清单查询失败：同上
	x3, _ := newCLIKit(t)
	x3.setPorts(fakePorts{vppErr: errors.New("VPP 未接入")})
	out = x3.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces ens999").Output
	if strings.Contains(out, "也不在 VPP 接口清单中") {
		t.Fatalf("清单查询失败时不得判「不在清单」: %q", out)
	}
}

// TestShowInterfacesDeclaredRuntimeView：决策 #155 接口族全运行态——已声明的口
// 裸写法也回运行态单口视图（与 physical <name> 同一实现），声明描述进描述列；
// 声明口已出现在运行态时不加注记（正常态不添噪）。
func TestShowInterfacesDeclaredRuntimeView(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setPorts(fakePorts{vpp: []string{"ens2f0"}})
	x.setVppState(fakeVppState{ifs: map[string]InterfaceState{
		"ens2f0": {AdminUp: true, LinkUp: true, DevType: "dpdk"},
	}})
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set interfaces ens2f0 mtu 9000",
		"set interfaces ens2f0 description to-TOR",
		"commit", "exit",
	)
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces ens2f0").Output
	if strings.HasPrefix(out, "（接口") || strings.HasPrefix(out, "%") {
		t.Fatalf("声明口且运行态可见：应回纯净运行态视图（无注记、无错误）: %q", out)
	}
	if !strings.Contains(out, "to-TOR") || !strings.Contains(out, "dpdk") {
		t.Fatalf("运行态视图应含声明描述与运行态驱动: %q", out)
	}
	with := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces physical ens2f0").Output
	if with != out {
		t.Fatalf("physical <ifname> 与裸写法应逐字相同\n  带: %q\n  裸: %q", with, out)
	}
}

// TestShowInterfacesDeclaredAbsentFromRuntime：声明了但 VPP 运行态里没有的口——
// 状态列 - 并如实注记，不得谎报 up（#84「字段取配置」残留的反面教材）。
func TestShowInterfacesDeclaredAbsentFromRuntime(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setPorts(fakePorts{vpp: []string{"ens192"}})
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set interfaces ens2f0 description pending",
		"commit", "exit",
	)
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces ens2f0").Output
	if !strings.Contains(out, "已声明，未在 VPP 运行态出现") {
		t.Fatalf("声明未生效应如实注记: %q", out)
	}
	if !strings.Contains(out, "pending") {
		t.Fatalf("描述列应取声明值: %q", out)
	}
}

// TestShowInterfacesListIsRuntimeInventory：无参清单 = 配置声明 ∪ VPP 运行态口
// （决策 #155），来源列标注「已声明未生效 / 未声明」，驱动等取运行态。
func TestShowInterfacesListIsRuntimeInventory(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setPorts(fakePorts{vpp: []string{"bvi0", "ens192"}})
	x.setVppState(fakeVppState{ifs: map[string]InterfaceState{
		"ens192": {AdminUp: false, LinkUp: false, DevType: "dpdk"},
	}})
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set interfaces ens2f0 description pending",
		"commit", "exit",
	)
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces").Output
	for _, want := range []string{"ens2f0", "ens192", "bvi0", "已声明未生效", "未声明"} {
		if !strings.Contains(out, want) {
			t.Fatalf("运行态清单应含 %q: %q", want, out)
		}
	}
	if !strings.Contains(out, "dpdk") {
		t.Fatalf("驱动列应取运行态: %q", out)
	}
	// physical 聚合与裸摘要等价（决策 #155）
	if out2 := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces physical").Output; out2 != out {
		t.Fatalf("physical 聚合与裸摘要应逐字相同\n  physical: %q\n  裸: %q", out2, out)
	}
}
