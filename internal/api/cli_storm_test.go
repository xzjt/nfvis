package api

// 决策 #385：接口入向风暴抑制（storm control）的 CLI 端到端回归。
//
// 覆盖：语句 → 模型 → display set 反推（回放自校验）→ 逐类/整段删除；未声明接口、
// unknown-unicast、值域三类拒绝；读视图块（配置 + 数据面实测，用假读数注入）。

import (
	"context"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// fakeStormRuntime StormRuntime 的假实现（读视图注入用）。
type fakeStormRuntime struct {
	dp network.StormDataplane
	ok bool
}

func (f fakeStormRuntime) StormDataplane(context.Context, string) (network.StormDataplane, bool) {
	return f.dp, f.ok
}

// stormExec 逐行执行并拼接输出（不判定失败——拒绝类用例要自己看文案）。
func stormExec(t *testing.T, x *cliExecutor, lines ...string) string {
	t.Helper()
	var out strings.Builder
	for _, line := range lines {
		out.WriteString(x.Execute("admin", aaaClassSU, "ssh", line).Output)
	}
	return out.String()
}

func TestCLIStormControlStatementAndView(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set interfaces ens224 description storm-port", // 声明口（storm-control 只配在已声明的口上）
		"set interfaces ens224 storm-control broadcast 8000",
		"set interfaces ens224 storm-control multicast 20000",
		"commit",
		"exit", // 回操作模式（show interfaces … detail 是运行态读视图）
	)
	cfg, err := engine.Committed()
	if err != nil {
		t.Fatalf("读取 committed: %v", err)
	}
	if len(cfg.Interfaces) != 1 || cfg.Interfaces[0].StormControl == nil ||
		cfg.Interfaces[0].StormControl.BroadcastKbps != 8000 || cfg.Interfaces[0].StormControl.MulticastKbps != 20000 {
		t.Fatalf("storm-control 应写入模型: %+v", cfg.Interfaces)
	}

	// display set 反推（含回放自校验：还原不出来会报内部错误）；两类各一条语句
	res := x.Execute("admin", aaaClassSU, "ssh", "show configuration | display set")
	if strings.Contains(res.Output, "内部错误") {
		t.Fatalf("display set 回放自校验失败:\n%s", res.Output)
	}
	for _, want := range []string{"set interfaces ens224 storm-control broadcast 8000",
		"set interfaces ens224 storm-control multicast 20000"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("display set 应反推出 %q:\n%s", want, res.Output)
		}
	}

	// 读视图（假读数）：配置 + 数据面实测（policer/分类表/计数）三样都要在
	x.setStorm(fakeStormRuntime{ok: true, dp: network.StormDataplane{
		Available: true, Attached: true, AttachedL2Table: 9,
		Kinds: map[string]network.StormKindDataplane{
			network.StormKindBroadcast: {PolicerPresent: true, CirKbps: 8000,
				Table:    &network.StormTableInfo{Index: 9, Mask: "ffffffffffff00000000000000000000", Sessions: 1},
				Counters: &network.StormCounters{ConformPackets: 189, ViolatePackets: 9}},
			network.StormKindMulticast: {PolicerPresent: true, CirKbps: 20000,
				CountersReason: "stats segment 未返回该 policer 的计数路径"},
		},
	}})
	out := x.Execute("admin", aaaClassSU, "ssh", "show interfaces ens224 detail").Output
	for _, want := range []string{"Storm control: 广播 8000 kbps / 组播 20000 kbps",
		"cir 8000 kbps", "会话 1", "conform 189 / exceed 0 / violate 9", "组播 不可读"} {
		if !strings.Contains(out, want) {
			t.Fatalf("接口详情应包含 %q:\n%s", want, out)
		}
	}
	// 结构化输出：配置对象与 REST 同形 + 实测另置（供 | display json 消费）
	entry := x.structured.(map[string]any)["interfaces"].([]any)[0].(map[string]any)
	scv, _ := entry["storm_control"].(map[string]any)
	if scv["broadcast_kbps"] != 8000 || scv["multicast_kbps"] != 20000 {
		t.Fatalf("结构化 storm_control 应含两类配置: %v", entry["storm_control"])
	}
	if _, ok := entry["storm_control_runtime"]; !ok {
		t.Fatalf("结构化输出应含数据面实测（storm_control_runtime）: %v", entry)
	}

	// 逐类删除：只清掉给的那类，另一类保留
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"delete interfaces ens224 storm-control broadcast",
		"commit",
		"exit",
	)
	cfg, _ = engine.Committed()
	if cfg.Interfaces[0].StormControl == nil || cfg.Interfaces[0].StormControl.BroadcastKbps != 0 ||
		cfg.Interfaces[0].StormControl.MulticastKbps != 20000 {
		t.Fatalf("delete broadcast 应只清该类: %+v", cfg.Interfaces[0].StormControl)
	}
	res = x.Execute("admin", aaaClassSU, "ssh", "show configuration | display set")
	if strings.Contains(res.Output, "storm-control broadcast") || !strings.Contains(res.Output, "storm-control multicast 20000") {
		t.Fatalf("display set 应只剩组播语句:\n%s", res.Output)
	}

	// 裸 delete：两类都清（且不留空壳——反推不出语句会报内部错误）
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"delete interfaces ens224 storm-control",
		"commit",
		"exit",
	)
	cfg, _ = engine.Committed()
	if cfg.Interfaces[0].StormControl != nil {
		t.Fatalf("裸 delete 应清掉整段: %+v", cfg.Interfaces[0].StormControl)
	}
	res = x.Execute("admin", aaaClassSU, "ssh", "show configuration | display set")
	if strings.Contains(res.Output, "storm-control") || strings.Contains(res.Output, "内部错误") {
		t.Fatalf("清空后不应再有 storm-control 语句/空壳:\n%s", res.Output)
	}
}

// 决策 #401（R176-2）：policer 在、但接口 L2 槽上没有本类分类表（实况未挂，例如被
// port-security 的 macip 表占用）时，读视图如实报「未挂」，不再笼统报「登记缺失」。
func TestCLIStormControlReadViewNotAttached(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set interfaces ens224 description storm-port",
		"set interfaces ens224 storm-control broadcast 8000",
		"commit",
		"exit",
	)
	x.setStorm(fakeStormRuntime{ok: true, dp: network.StormDataplane{
		Available: true, Attached: true, AttachedL2Table: 24,
		Kinds: map[string]network.StormKindDataplane{
			network.StormKindBroadcast: {PolicerPresent: true, CirKbps: 8000},
		},
	}})
	out := x.Execute("admin", aaaClassSU, "ssh", "show interfaces ens224 detail").Output
	if !strings.Contains(out, "分类表未挂") {
		t.Fatalf("槽上无本类表时应如实报「未挂」:\n%s", out)
	}
	if strings.Contains(out, "分类表 #") {
		t.Fatalf("实况未挂时不得报「在位」（分类表 #N）:\n%s", out)
	}
}

// TestCLIStormControlReadViewDeclaredBinding（决策 #421④）：本底座 policer-classify 绑定不可
// 回读（实况阴性、登记在位）时，读视图按**登记**如实呈现「分类表 #N 按登记在位」，与
// 「实况回读」（分类表 #N 实况回读）和「未挂」都区分开；结构化输出给 binding / declared_l2_table /
// table_declared，且**不发射** attached_l2_table（那是实况字段，不得被登记冒名）。
func TestCLIStormControlReadViewDeclaredBinding(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set interfaces ens224 description storm-port",
		"set interfaces ens224 storm-control broadcast 8000",
		"commit",
		"exit",
	)
	x.setStorm(fakeStormRuntime{ok: true, dp: network.StormDataplane{
		Available: true, Binding: network.StormBindingDeclared, DeclaredTable: 9,
		Kinds: map[string]network.StormKindDataplane{
			network.StormKindBroadcast: {PolicerPresent: true, CirKbps: 8000,
				TableByRegistration: &network.StormTableInfo{Index: 9, Mask: "ffffffffffff00000000000000000000", Sessions: 1},
				Counters:            &network.StormCounters{ConformPackets: 12}},
		},
	}})
	out := x.Execute("admin", aaaClassSU, "ssh", "show interfaces ens224 detail").Output
	for _, want := range []string{"cir 8000 kbps", "分类表 #9 按登记在位", "读不到该绑定", "conform 12"} {
		if !strings.Contains(out, want) {
			t.Fatalf("按登记态 detail 应含 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "分类表未挂") {
		t.Fatalf("登记在位时不得报「未挂」:\n%s", out)
	}
	if strings.Contains(out, "实况回读") {
		t.Fatalf("按登记态不得冒充实况回读:\n%s", out)
	}
	rt := detailEntryOf(t, x, "ens224")["storm_control_runtime"].(map[string]any)
	if rt["binding"] != network.StormBindingDeclared {
		t.Fatalf("结构化 binding 应为 declared: %v", rt)
	}
	if !sameNumber(rt["declared_l2_table"], 9) {
		t.Fatalf("结构化应按登记给 declared_l2_table=9: %v", rt)
	}
	if _, ok := rt["attached_l2_table"]; ok {
		t.Fatalf("按登记态不得发射实况槽索引（attached_l2_table）: %v", rt)
	}
	bd := rt["kinds"].(map[string]any)["broadcast"].(map[string]any)
	if _, ok := bd["table"]; ok {
		t.Fatalf("按登记态不得把它写进实况字段 table: %v", bd)
	}
	if td, ok := bd["table_declared"].(*network.StormTableInfo); !ok || td.Index != 9 {
		t.Fatalf("按登记态应给 table_declared（登记表 9）: %v", bd)
	}
}

// TestCLIStormControlReadViewAdoptedAndCandidates（决策 #421 收口③）：四态里的「自认领」
// 单独成态——文本给依据（policer 按名在场 + 表链匹配），结构化给 binding_basis /
// adopted_l2_table / table_adopted / table_source，且**不发射**实况字段（attached_l2_table）
// 与按登记字段（declared_l2_table）；只识别不删的分类表候选如实列出。
func TestCLIStormControlReadViewAdoptedAndCandidates(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set interfaces ens224 description storm-port",
		"set interfaces ens224 storm-control broadcast 8000",
		"set interfaces ens224 storm-control multicast 20000",
		"commit",
		"exit",
	)
	x.setStorm(fakeStormRuntime{ok: true, dp: network.StormDataplane{
		Available: true, Binding: network.StormBindingAdopted, AdoptedTable: 1,
		OrphanCandidates: []uint32{7, 8},
		Kinds: map[string]network.StormKindDataplane{
			network.StormKindBroadcast: {PolicerPresent: true, CirKbps: 8000,
				TableAdopted: &network.StormTableInfo{Index: 1, Mask: "ffffffffffff00000000000000000000", Sessions: 1},
				Counters:     &network.StormCounters{ConformPackets: 3}},
			network.StormKindMulticast: {PolicerPresent: true, CirKbps: 20000,
				TableAdopted: &network.StormTableInfo{Index: 0, Mask: "01000000000000000000000000000000", Sessions: 1},
				Counters:     &network.StormCounters{ConformPackets: 1}},
		},
	}})
	out := x.Execute("admin", aaaClassSU, "ssh", "show interfaces ens224 detail").Output
	for _, want := range []string{
		"cir 8000 kbps", "分类表 #1 自认领在位", "依据 policer 按名在场 + 表链匹配",
		"分类表候选: #7、#8", "只识别不删",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("自认领态 detail 应含 %q：\n%s", want, out)
		}
	}
	for _, bad := range []string{"分类表未挂", "实况回读", "按登记在位"} {
		if strings.Contains(out, bad) {
			t.Fatalf("自认领态不得出现 %q（不得与其它的态混同）：\n%s", bad, out)
		}
	}
	rt := detailEntryOf(t, x, "ens224")["storm_control_runtime"].(map[string]any)
	if rt["binding"] != network.StormBindingAdopted {
		t.Fatalf("结构化 binding 应为 adopted：%v", rt)
	}
	if basis, _ := rt["binding_basis"].(string); !strings.Contains(basis, "自认领") {
		t.Fatalf("结构化应给认领依据：%v", rt)
	}
	if !sameNumber(rt["adopted_l2_table"], 1) {
		t.Fatalf("结构化应按认领给 adopted_l2_table=1：%v", rt)
	}
	for _, bad := range []string{"attached_l2_table", "declared_l2_table"} {
		if _, ok := rt[bad]; ok {
			t.Fatalf("自认领态不得发射 %s：%v", bad, rt)
		}
	}
	ids, ok := rt["orphan_candidates"].([]uint32)
	if !ok || len(ids) != 2 || ids[0] != 7 || ids[1] != 8 {
		t.Fatalf("结构化应给分类表候选（只识别）：%v", rt["orphan_candidates"])
	}
	bd := rt["kinds"].(map[string]any)["broadcast"].(map[string]any)
	if _, ok := bd["table"]; ok {
		t.Fatalf("自认领态不得写进实况字段 table：%v", bd)
	}
	if td, ok := bd["table_adopted"].(*network.StormTableInfo); !ok || td.Index != 1 {
		t.Fatalf("自认领态应给 table_adopted（表 1）：%v", bd)
	}
	if src, _ := bd["table_source"].(string); !strings.Contains(src, "自认领") {
		t.Fatalf("自认领态应给来源依据 table_source：%v", bd)
	}
}

// TestCLIStormControlReadViewOrphanPolicers（决策 #433）：只识别不删的孤儿 policer 候选如实列出
// （文本行 + 结构化 orphan_policers）；无候选时不发射该字段（与 orphan_candidates 同口径）。
func TestCLIStormControlReadViewOrphanPolicers(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set interfaces ens224 description storm-port",
		"set interfaces ens224 storm-control broadcast 8000",
		"commit",
		"exit",
	)
	x.setStorm(fakeStormRuntime{ok: true, dp: network.StormDataplane{
		Available: true, Binding: network.StormBindingAdopted, AdoptedTable: 1,
		OrphanPolicers: []string{"nfvis-storm-ghost-broadcast", "nfvis-storm-ghost-multicast"},
		Kinds: map[string]network.StormKindDataplane{
			network.StormKindBroadcast: {PolicerPresent: true, CirKbps: 8000,
				TableAdopted: &network.StormTableInfo{Index: 1, Mask: "ffffffffffff00000000000000000000", Sessions: 1}},
		},
	}})
	out := x.Execute("admin", aaaClassSU, "ssh", "show interfaces ens224 detail").Output
	if !strings.Contains(out, "Storm control policer 候选: nfvis-storm-ghost-broadcast、nfvis-storm-ghost-multicast") {
		t.Fatalf("detail 应列出孤儿 policer 候选（只识别）:\n%s", out)
	}
	if !strings.Contains(out, "未被声明或登记覆盖") || !strings.Contains(out, "只识别不删") {
		t.Fatalf("候选行应写明依据（未被声明或登记覆盖 / 只识别不删）:\n%s", out)
	}
	rt := detailEntryOf(t, x, "ens224")["storm_control_runtime"].(map[string]any)
	names, ok := rt["orphan_policers"].([]string)
	if !ok || len(names) != 2 || names[0] != "nfvis-storm-ghost-broadcast" || names[1] != "nfvis-storm-ghost-multicast" {
		t.Fatalf("结构化应给 orphan_policers：%v", rt["orphan_policers"])
	}
	// 无候选时不发射该字段。
	x.setStorm(fakeStormRuntime{ok: true, dp: network.StormDataplane{
		Available: true, Binding: network.StormBindingAdopted, AdoptedTable: 1,
		Kinds: map[string]network.StormKindDataplane{
			network.StormKindBroadcast: {PolicerPresent: true, CirKbps: 8000,
				TableAdopted: &network.StormTableInfo{Index: 1, Mask: "ffffffffffff00000000000000000000", Sessions: 1}},
		},
	}})
	out2 := x.Execute("admin", aaaClassSU, "ssh", "show interfaces ens224 detail").Output
	if strings.Contains(out2, "policer 候选") {
		t.Fatalf("无候选时不得发射候选行:\n%s", out2)
	}
	rt2 := detailEntryOf(t, x, "ens224")["storm_control_runtime"].(map[string]any)
	if _, ok := rt2["orphan_policers"]; ok {
		t.Fatalf("无候选时不得发射 orphan_policers：%v", rt2)
	}
}

// 未声明接口 / unknown-unicast / 值域三类拒绝（各给能照做的报错或明确的校验错误）。
func TestCLIStormControlRejections(t *testing.T) {
	// 未声明接口：语句期拒绝并给出声明写法（不允许本语句代建声明）
	x, _ := newCLIKit(t)
	res := stormExec(t, x, "configure", "set interfaces ens999 storm-control broadcast 8000")
	if !strings.Contains(res, "%%") || !strings.Contains(res, "未在配置中声明") ||
		!strings.Contains(res, "description") {
		t.Fatalf("未声明接口应被拒绝并给照做指引: %s", res)
	}
	// unknown-unicast（本版本有意不做）：set/delete 两种写法都拦、说明原因（L2 掩码表达不了）
	for _, lines := range [][]string{
		{"configure", "set interfaces ens224 description p", "set interfaces ens224 storm-control unknown-unicast 8000"},
		{"configure", "set interfaces ens224 description p", "delete interfaces ens224 storm-control unknown-unicast"},
	} {
		x, _ := newCLIKit(t)
		res := stormExec(t, x, lines...)
		if !strings.Contains(res, "%%") || !strings.Contains(res, "unknown-unicast") ||
			!strings.Contains(res, "可匹配位") {
			t.Fatalf("unknown-unicast 应被拒绝并说明原因（%v）: %s", lines, res)
		}
	}
	// 非正整数：语句期拒绝（接口已声明）
	x2, _ := newCLIKit(t)
	res = stormExec(t, x2, "configure",
		"set interfaces ens224 description p", "set interfaces ens224 storm-control broadcast 0")
	if !strings.Contains(res, "%%") || !strings.Contains(res, "整数") {
		t.Fatalf("0 应被语句期拒绝: %s", res)
	}
	// 越界：语句期收下、提交期由模型校验拒绝（candidate 保留、不落库）
	x3, engine := newCLIKit(t)
	run(t, x3, "admin", aaaClassSU, "ssh",
		"configure",
		"set interfaces ens224 description p",
		"set interfaces ens224 storm-control broadcast 100000001",
	)
	out := x3.Execute("admin", aaaClassSU, "ssh", "commit").Output
	if !strings.Contains(out, "校验失败") || !strings.Contains(out, "1-100000000") {
		t.Fatalf("越界值应在提交期被拒: %s", out)
	}
	if cfg, err := engine.Committed(); err != nil || len(cfg.Interfaces) != 0 {
		t.Fatalf("提交失败不得落库: %v %+v", err, cfg.Interfaces)
	}
}
