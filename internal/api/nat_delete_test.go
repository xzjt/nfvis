package api

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

// NAT 删除语义回归（round84 R84-25 / R84-26）。
//
//   - R84-25：`delete nat rules <seq> action source-pool <名>` 只想摘掉池引用，
//     却把整条规则（连 match 与 action interface）一起删掉。
//   - R84-26：删光 NAT 池/规则/静态映射后配置里留下空 `nat` 对象，
//     `show configuration | display set` 反推为空语句、回放对不上而报内部错误。
//
// 最小合法 NAT 配置：出接口必须是某 VRF 的 l3-interface 且已配地址（model.checkNat），
// 故先声明物理口 ens192 并把它配成 L3 交换机的 l3-interface，再建池与规则
// （match / action source-pool / action interface 三条叶子，删除动作针对其中一条）。

// natFixture 最小合法 NAT 配置的语句集（不含 configure/commit）。
func natFixture() []string {
	return []string{
		"set interfaces ens192 mtu 9000",
		"set virtual-switches vs-l3 type l3",
		"set virtual-switches vs-l3 l3-interface ens192 ip address 10.0.0.1/24",
		"set nat source-pool pool-a address-range 100.64.0.10 to 100.64.0.20",
		"set nat rules 10 match source 192.168.100.0/24 virtual-switch vs-l3",
		"set nat rules 10 action source-pool pool-a",
		"set nat rules 10 action interface ens192",
	}
}

// execOK 执行一条命令并要求成功（失败即红）。
func execOK(t *testing.T, x *cliExecutor, line string) string {
	t.Helper()
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", line)
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("命令 %q 失败: %s", line, res.Output)
	}
	return res.Output
}

// TestNatDeleteActionSourcePoolKeepsRule（R84-25）：删除 action source-pool 叶子后
// 规则必须仍在，且 match 与 action interface 两条叶子完整保留。
func TestNatDeleteActionSourcePoolKeepsRule(t *testing.T) {
	x, eng := newCLIKit(t)
	lines := append([]string{"configure"}, natFixture()...)
	lines = append(lines, "commit")
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", lines...)

	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "delete nat rules 10 action source-pool pool-a", "commit")

	cfg, err := eng.Committed()
	if err != nil {
		t.Fatalf("Committed: %v", err)
	}
	if cfg.Nat == nil || len(cfg.Nat.Rules) != 1 {
		t.Fatalf("删除 action source-pool 后规则应仍在（当前 %+v）", cfg.Nat)
	}
	r := cfg.Nat.Rules[0]
	if r.Seq != 10 || r.MatchSource != "192.168.100.0/24" || r.VirtualSwitch != "vs-l3" {
		t.Fatalf("规则匹配条件应完整保留: %+v", r)
	}
	if r.Action.Interface != "ens192" {
		t.Fatalf("action interface 应完整保留: %+v", r.Action)
	}
	if r.Action.SourcePool != "" {
		t.Fatalf("action source-pool 应已清除: %+v", r.Action)
	}
	// 其余叶子仍在 ⇒ 反推（display set）也必须仍然成立
	out := execOK(t, x, "show configuration | display set")
	if !strings.Contains(out, "set nat rules 10 match source 192.168.100.0/24 virtual-switch vs-l3") ||
		!strings.Contains(out, "set nat rules 10 action interface ens192") {
		t.Fatalf("display set 应仍能反推该规则:\n%s", out)
	}
}

// TestNatDeleteActionInterfaceKeepsRule：同理，删除 action interface 叶子只清该叶子。
func TestNatDeleteActionInterfaceKeepsRule(t *testing.T) {
	x, eng := newCLIKit(t)
	lines := append([]string{"configure"}, natFixture()...)
	lines = append(lines, "commit")
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", lines...)

	// 出接口是 commit 的硬要求，故删除后不 commit，只查 candidate 语义
	execOK(t, x, "configure")
	execOK(t, x, "delete nat rules 10 action interface ens192")

	cand, _, err := eng.Candidate()
	if err != nil {
		t.Fatalf("Candidate: %v", err)
	}
	if cand.Nat == nil || len(cand.Nat.Rules) != 1 {
		t.Fatalf("删除 action interface 后规则应仍在（当前 %+v）", cand.Nat)
	}
	if cand.Nat.Rules[0].Action.Interface != "" {
		t.Fatalf("action interface 应已清除: %+v", cand.Nat.Rules[0].Action)
	}
	if cand.Nat.Rules[0].MatchSource != "192.168.100.0/24" || cand.Nat.Rules[0].Action.SourcePool != "pool-a" {
		t.Fatalf("其余叶子应完整保留: %+v", cand.Nat.Rules[0])
	}
}

// TestNatDeleteAllLeavesNoEmptyNode（R84-26）：删光池与规则后不得留下空 `nat` 对象，
// 且 `show configuration | display set` 必须成功（此前报「未能完整还原配置」内部错误）。
func TestNatDeleteAllLeavesNoEmptyNode(t *testing.T) {
	x, eng := newCLIKit(t)
	lines := append([]string{"configure"}, natFixture()...)
	lines = append(lines, "commit")
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", lines...)

	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "delete nat rules 10", "delete nat source-pool pool-a", "commit")

	cfg, err := eng.Committed()
	if err != nil {
		t.Fatalf("Committed: %v", err)
	}
	if cfg.Nat != nil {
		t.Fatalf("删光 NAT 后不应留下空 nat 对象: %+v", *cfg.Nat)
	}
	out := execOK(t, x, "show configuration | display set")
	if strings.Contains(out, "nat") {
		t.Fatalf("删光 NAT 后 display set 不应出现 nat 语句:\n%s", out)
	}
}

// TestNatDeleteMatchSourceKeepsOtherLeaves：match 也是叶子级删除——只清 match_source，
// virtual-switch 与 action 两条叶子保留（match_source 清空后 commit 校验会拒绝，
// 故只查 candidate；操作者要么补回匹配条件、要么再删 virtual-switch）。
func TestNatDeleteMatchSourceKeepsOtherLeaves(t *testing.T) {
	x, eng := newCLIKit(t)
	lines := append([]string{"configure"}, natFixture()...)
	lines = append(lines, "commit")
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", lines...)

	execOK(t, x, "configure")
	execOK(t, x, "delete nat rules 10 match source 192.168.100.0/24")

	cand, _, err := eng.Candidate()
	if err != nil {
		t.Fatalf("Candidate: %v", err)
	}
	if cand.Nat == nil || len(cand.Nat.Rules) != 1 {
		t.Fatalf("删除 match source 后规则应仍在（当前 %+v）", cand.Nat)
	}
	r := cand.Nat.Rules[0]
	if r.MatchSource != "" {
		t.Fatalf("match source 应已清除: %+v", r)
	}
	if r.VirtualSwitch != "vs-l3" || r.Action.Interface != "ens192" || r.Action.SourcePool != "pool-a" {
		t.Fatalf("其余叶子应完整保留: %+v", r)
	}
	// 该状态不可 commit（match_source 空串过不了校验），但反推必须仍成立——直接对 candidate
	// 树跑 display set 的生成+回放自校验（配置模式的 `show configuration` 取的是 committed，
	// 拿不到 candidate，故用同一入口 generateSetStmts）。
	if _, err := generateSetStmts(toJSONTree(cand), nil); err != nil {
		t.Fatalf("清掉 match source 后 display set 应仍能反推: %v", err)
	}
}

// TestNatDeleteWholeRuleKeepsOtherContent：`delete nat rules <seq>`（无尾随 token）仍是
// 整条规则删除；同层还有池时不得连 nat 节点一起剪掉（空壳剪枝只剪"已无内容"的容器）。
func TestNatDeleteWholeRuleKeepsOtherContent(t *testing.T) {
	x, eng := newCLIKit(t)
	lines := append([]string{"configure"}, natFixture()...)
	lines = append(lines, "commit")
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", lines...)

	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "delete nat rules 10", "commit")

	cfg, err := eng.Committed()
	if err != nil {
		t.Fatalf("Committed: %v", err)
	}
	if cfg.Nat == nil || len(cfg.Nat.Rules) != 0 || len(cfg.Nat.SourcePools) != 1 {
		t.Fatalf("整条规则应删除、池应保留（当前 %+v）", cfg.Nat)
	}
}

// TestNatDeleteAbsentLeafReportsNoMatch：取值 token 容忍（照 set 行抄写、取值与现值不一致
// 也照删，与 applyTokens 删值叶子同口径）；叶子不在场则报「无匹配配置」，不得静默成功。
func TestNatDeleteAbsentLeafReportsNoMatch(t *testing.T) {
	x, eng := newCLIKit(t)
	lines := append([]string{"configure"}, natFixture()...)
	lines = append(lines, "commit")
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", lines...)

	execOK(t, x, "configure")
	for _, line := range []string{
		"delete nat rules 10 action interface nosuch0",  // 值不一致：仍清该叶
		"delete nat rules 10 action source-pool pool-a", // 真删
	} {
		execOK(t, x, line) // 第一次：真删（取值 token 容忍）
		if res := x.Execute("admin", aaa.ClassSuperUser, "ssh", line); !strings.Contains(res.Output, "无匹配配置") {
			t.Fatalf("删不存在的叶子应报无匹配配置: %q => %q", line, res.Output)
		}
	}
	cand, _, err := eng.Candidate()
	if err != nil {
		t.Fatalf("Candidate: %v", err)
	}
	if len(cand.Nat.Rules) != 1 {
		t.Fatalf("叶子级删除不得连规则一起删: %+v", cand.Nat)
	}
	if cand.Nat.Rules[0].Action.Interface != "" || cand.Nat.Rules[0].Action.SourcePool != "" {
		t.Fatalf("两条 action 叶子应已清除: %+v", cand.Nat.Rules[0].Action)
	}
	if cand.Nat.Rules[0].MatchSource != "192.168.100.0/24" {
		t.Fatalf("match 叶子应保留: %+v", cand.Nat.Rules[0])
	}
}

// TestPruneEmptySingletonScope：空壳剪枝的判据与边界——nat 全空才剪，
// 还剩任何一条叶子就不剪；既有 system.management 剪枝不回退；未登记的容器不动
// （宁可不剪，也不越界改其它家族的行为）。
func TestPruneEmptySingletonScope(t *testing.T) {
	cases := []struct {
		name    string
		tree    map[string]any
		wantNat bool // true = 剪枝后仍应有 nat 键
	}{
		{"空对象（模型序列化后的空壳）",
			map[string]any{"nat": map[string]any{}}, false},
		{"只剩空数组（别名删除后、序列化前的树形态）",
			map[string]any{"nat": map[string]any{"rules": []any{}, "source_pools": []any{}, "static": []any{}}}, false},
		{"还有池",
			map[string]any{"nat": map[string]any{"source_pools": []any{map[string]any{"name": "p"}}, "rules": []any{}}}, true},
		{"还有规则",
			map[string]any{"nat": map[string]any{"rules": []any{map[string]any{"seq": float64(1)}}}}, true},
		{"未知键视为有内容（防御：宁可不剪）",
			map[string]any{"nat": map[string]any{"future_field": "x"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pruneEmptySingleton(c.tree)
			if _, ok := c.tree["nat"]; ok != c.wantNat {
				t.Fatalf("nat 剪枝结果不符（want 保留=%v）: %v", c.wantNat, c.tree)
			}
		})
	}

	// 既有口径不回退：system.management 空壳仍回收
	tree := map[string]any{"system": map[string]any{"management": map[string]any{}, "hostname": "n1"}}
	pruneEmptySingleton(tree)
	if _, ok := tree["system"].(map[string]any)["management"]; ok {
		t.Fatalf("system.management 空壳应照旧回收: %v", tree)
	}
	// 未登记的容器不动（本函数只收敛**逐个登记**的容器：management/metrics/firewall/nat/protocols；
	// 其它家族的空壳行为不在范围内——登记制而非通用扫描，避免误剪将来可能引入的
	// 「显式空 = 有意义」的容器）
	tree = map[string]any{"vpp": map[string]any{}}
	pruneEmptySingleton(tree)
	if _, ok := tree["vpp"]; !ok {
		t.Fatalf("未登记的容器不应被剪: %v", tree)
	}
	// 已登记：protocols 空壳（LLDP 删光，决策 #440）——外层与内层空 lldp 都回收
	tree = map[string]any{"protocols": map[string]any{"lldp": map[string]any{"interfaces": []any{}}}}
	pruneEmptySingleton(tree)
	if _, ok := tree["protocols"]; ok {
		t.Fatalf("已登记的 protocols 空壳应被剪: %v", tree)
	}
}

// TestNatDisplaySetWithContent（不回退）：有内容的 NAT 配置 display set 仍能反推、
// 自校验通过，且语句可原样回放（round81 建立的框架不回退）。
func TestNatDisplaySetWithContent(t *testing.T) {
	x, _ := newCLIKit(t)
	lines := append([]string{"configure"}, natFixture()...)
	lines = append(lines, "commit")
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", lines...)

	out := execOK(t, x, "show configuration | display set")
	for _, want := range []string{
		"set nat source-pool pool-a address-range 100.64.0.10 to 100.64.0.20",
		"set nat rules 10 match source 192.168.100.0/24 virtual-switch vs-l3",
		"set nat rules 10 action source-pool pool-a",
		"set nat rules 10 action interface ens192",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("display set 缺少语句 %q:\n%s", want, out)
		}
	}
}
