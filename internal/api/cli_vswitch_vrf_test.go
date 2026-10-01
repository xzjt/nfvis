package api

import (
	"slices"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/schema"
)

// R84-2（round84 装机走查）：CLI **整节点**删除虚拟交换机（`delete virtual-switches <名>`）
// 必须一并移除同名 Vrf 条目。L3 交换机与同名 VRF 条目互为映射（附录 B），REST
// `DELETE /virtual-switches/{name}` 即如此（resources.go handleDeleteVSwitch）。
// 此前 CLI 侧只有 `type l2` 这类别名规则清理 VRF，整节点删除会留下空壳条目：
// `show configuration | display set` 反推不出（操作者看不见）、数据面 VRF 表跨
// VPP/守护进程重启滞留，且没有任何命令能删掉它。

// vrfNameSet 取配置里的 VRF 名集合。
func vrfNameSet(cfg model.Config) map[string]bool {
	out := make(map[string]bool, len(cfg.Vrfs))
	for _, v := range cfg.Vrfs {
		out[v.Name] = true
	}
	return out
}

// mustConfig 把 JSON 树回填成强类型配置（断言用）。
func mustConfig(t *testing.T, tree map[string]any) model.Config {
	t.Helper()
	var cfg model.Config
	if err := fromJSONTree(tree, &cfg); err != nil {
		t.Fatalf("回填配置失败: %v", err)
	}
	return cfg
}

// hasVSwitch 判断树里是否还有指定名字的虚拟交换机。
func hasVSwitch(t *testing.T, tree map[string]any, name string) bool {
	t.Helper()
	for _, vs := range mustConfig(t, tree).VirtualSwitches {
		if vs.Name == name {
			return true
		}
	}
	return false
}

// TestCLIDeleteL3VSwitchDropsSameNameVrf：建 l3 交换机 → 删整节点 → 同名 Vrf 不得残留。
func TestCLIDeleteL3VSwitchDropsSameNameVrf(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set interfaces ens192",
		"set virtual-switches vs-l3 type l3",
		"set virtual-switches vs-l3 l3-interface ens192 ip address 10.10.0.1/24",
		"commit",
	)
	cfg, err := engine.Committed()
	if err != nil {
		t.Fatalf("读取 committed: %v", err)
	}
	if !vrfNameSet(cfg)["vs-l3"] {
		t.Fatalf("前置不成立：l3 交换机应带同名 VRF 条目，实际 %+v", cfg.Vrfs)
	}

	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"delete virtual-switches vs-l3",
		"commit",
	)
	cfg, err = engine.Committed()
	if err != nil {
		t.Fatalf("读取 committed: %v", err)
	}
	if len(cfg.VirtualSwitches) != 0 {
		t.Fatalf("虚拟交换机应已删除: %+v", cfg.VirtualSwitches)
	}
	if vrfNameSet(cfg)["vs-l3"] {
		t.Fatalf("整节点删除后不得残留同名 VRF 条目: %+v", cfg.Vrfs)
	}
}

// TestCLIDeleteL2VSwitchKeepsOtherVrf：删 l2 交换机不得误删无关 VRF 条目。
func TestCLIDeleteL2VSwitchKeepsOtherVrf(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set virtual-switches vs-l3 type l3",
		"set virtual-switches vs-l2 type l2",
		"commit",
	)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"delete virtual-switches vs-l2",
		"commit",
	)
	cfg, err := engine.Committed()
	if err != nil {
		t.Fatalf("读取 committed: %v", err)
	}
	for _, vs := range cfg.VirtualSwitches {
		if vs.Name == "vs-l2" {
			t.Fatalf("l2 交换机应已删除: %+v", cfg.VirtualSwitches)
		}
	}
	if !vrfNameSet(cfg)["vs-l3"] {
		t.Fatalf("删 l2 交换机不得动无关的 VRF 条目: %+v", cfg.Vrfs)
	}
}

// TestDeleteStatementVSwitchVrfForms：语句层形态回归——整节点删除清同名 VRF，
// 子节点删除（`… type`）只删该子节点，不越界。
func TestDeleteStatementVSwitchVrfForms(t *testing.T) {
	var cfg model.Config
	for _, line := range []string{
		"set virtual-switches vs-l3 type l3",
		"set virtual-switches vs-l2 type l2",
	} {
		if err := applyStatement(&cfg, splitFieldsQuoted(line)[1:]); err != nil {
			t.Fatalf("前置 %q: %v", line, err)
		}
	}

	// 子节点删除（`… type`）：元素仍在（仅 type 清除），且不得动 vs-l3 的 VRF。
	tree := deleteTree(t, cfg, "delete virtual-switches vs-l2 type")
	if !hasVSwitch(t, tree, "vs-l2") {
		t.Fatalf("删 type 不应删除交换机元素本身: %v", tree["virtual_switches"])
	}
	if !vrfNameSet(mustConfig(t, tree))["vs-l3"] {
		t.Fatalf("删 vs-l2 的 type 不得误删 vs-l3 的 VRF: %v", tree["vrfs"])
	}

	// 整节点删除：同名 VRF 一并清除。
	tree = deleteTree(t, cfg, "delete virtual-switches vs-l3")
	if hasVSwitch(t, tree, "vs-l3") {
		t.Fatalf("整节点删除应移除交换机元素: %v", tree["virtual_switches"])
	}
	if vrfNameSet(mustConfig(t, tree))["vs-l3"] {
		t.Fatalf("整节点删除应清同名 VRF: %v", tree["vrfs"])
	}
}

// TestCLIVSwitchDhcpRelayStatementAndView（决策 #335）：语句 → 模型 → 提交校验 → 读视图
// 与 display set 反推的端到端回归。
func TestCLIVSwitchDhcpRelayStatementAndView(t *testing.T) {
	x, engine := newCLIKit(t)

	// 未配网关：提交校验拒绝，文案指向先 set gateway ip
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set virtual-switches vs-relay type l2",
		"set virtual-switches vs-relay dhcp-relay server 192.168.100.2",
	)
	res := x.Execute("admin", aaaClassSU, "ssh", "commit")
	if !strings.Contains(res.Output, "校验失败") || !strings.Contains(res.Output, "gateway ip") {
		t.Fatalf("无网关配 relay 应校验失败并指向 gateway ip:\n%s", res.Output)
	}

	// 补网关后提交成功，committed 保留 server 值
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set virtual-switches vs-relay gateway ip 192.168.100.1/24",
		"commit",
		"exit", // 回操作模式（show virtual-switches … detail 是运行态读视图）
	)
	cfg, err := engine.Committed()
	if err != nil {
		t.Fatalf("读取 committed: %v", err)
	}
	if len(cfg.VirtualSwitches) != 1 || cfg.VirtualSwitches[0].DhcpRelayServer != "192.168.100.2" {
		t.Fatalf("dhcp-relay server 应写入模型: %+v", cfg.VirtualSwitches)
	}

	// display set 反推：dhcp-relay 语句必须可还原（决策 #155 往返口径）
	res = x.Execute("admin", aaaClassSU, "ssh", "show configuration | display set")
	if !strings.Contains(res.Output, "dhcp-relay server 192.168.100.2") {
		t.Fatalf("display set 应反推出 dhcp-relay 语句:\n%s", res.Output)
	}

	// 读视图：详情在配置了 relay 时带 dhcp_relay.server（与 REST 详情同源、同形状）
	x.setVppState(fakeVppState{bds: []BridgeDomainState{
		{ID: 9, Name: "vs-relay", Learn: true, Flood: true},
	}})
	out := x.Execute("admin", aaaClassSU, "ssh", "show virtual-switches vs-relay detail").Output
	if !strings.Contains(out, "dhcp-relay") || !strings.Contains(out, "192.168.100.2") {
		t.Fatalf("交换机详情应显示 DHCP 中继:\n%s", out)
	}

	// delete 语句：值清空；此后详情不再出现 dhcp_relay
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"delete virtual-switches vs-relay dhcp-relay",
		"commit",
		"exit",
	)
	cfg, err = engine.Committed()
	if err != nil {
		t.Fatalf("读取 committed: %v", err)
	}
	if cfg.VirtualSwitches[0].DhcpRelayServer != "" {
		t.Fatalf("delete dhcp-relay 应清空 server: %+v", cfg.VirtualSwitches)
	}
	out = x.Execute("admin", aaaClassSU, "ssh", "show virtual-switches vs-relay detail").Output
	if strings.Contains(out, "dhcp-relay") {
		t.Fatalf("未配置时详情不得出现 dhcp_relay:\n%s", out)
	}
}

// TestCLIVSwitchDhcpRelayCandidates（决策 #335 边界）：候选语义与语句树同源——
// `set virtual-switches <n> dhcp-relay ?` 必须能补到 `server`，值位置无候选（自由取值）。
func TestCLIVSwitchDhcpRelayCandidates(t *testing.T) {
	relayNode, _, err := schema.Match(schema.ConfigPathTree(),
		[]string{"virtual-switches", "vs1", "dhcp-relay"})
	if err != nil {
		t.Fatalf("dhcp-relay 应在语句树里可解析到: %v", err)
	}
	var kids []string
	for _, c := range relayNode.Children {
		kids = append(kids, c.Name)
	}
	if !slices.Contains(kids, "server") {
		t.Fatalf("dhcp-relay 下应能补到 server，实际: %v", kids)
	}
}

// TestCLIVSwitchLearnLimitStatementAndView（决策 #337）：语句 → 模型 → display set 反推 →
// 读视图（detail 带 learn_limit）→ delete 清除的端到端回归。
func TestCLIVSwitchLearnLimitStatementAndView(t *testing.T) {
	x, engine := newCLIKit(t)

	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set virtual-switches vs-ll type l2",
		"set virtual-switches vs-ll learn-limit 8192",
		"commit",
		"exit", // 回操作模式（show virtual-switches … detail 是运行态读视图）
	)
	cfg, err := engine.Committed()
	if err != nil {
		t.Fatalf("读取 committed: %v", err)
	}
	if len(cfg.VirtualSwitches) != 1 || cfg.VirtualSwitches[0].LearnLimit != 8192 {
		t.Fatalf("learn-limit 应写入模型: %+v", cfg.VirtualSwitches)
	}

	// display set 反推：learn-limit 语句必须可还原（决策 #155 往返口径）
	res := x.Execute("admin", aaaClassSU, "ssh", "show configuration | display set")
	if !strings.Contains(res.Output, "learn-limit 8192") {
		t.Fatalf("display set 应反推出 learn-limit 语句:\n%s", res.Output)
	}

	// 读视图：详情在配置了 learn-limit 时带「学习上限」行（渲染为 learn-limit，与 REST 详情同源）
	x.setVppState(fakeVppState{bds: []BridgeDomainState{
		{ID: 11, Name: "vs-ll", Learn: true, Flood: true},
	}})
	out := x.Execute("admin", aaaClassSU, "ssh", "show virtual-switches vs-ll detail").Output
	if !strings.Contains(out, "learn-limit") || !strings.Contains(out, "8192") {
		t.Fatalf("交换机详情应显示学习上限:\n%s", out)
	}

	// delete 语句：值清空；此后详情不再出现学习上限
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"delete virtual-switches vs-ll learn-limit",
		"commit",
		"exit",
	)
	cfg, err = engine.Committed()
	if err != nil {
		t.Fatalf("读取 committed: %v", err)
	}
	if cfg.VirtualSwitches[0].LearnLimit != 0 {
		t.Fatalf("delete learn-limit 应清空: %+v", cfg.VirtualSwitches)
	}
	out = x.Execute("admin", aaaClassSU, "ssh", "show virtual-switches vs-ll detail").Output
	if strings.Contains(out, "learn-limit") {
		t.Fatalf("未配置时详情不得出现 learn-limit:\n%s", out)
	}
}
