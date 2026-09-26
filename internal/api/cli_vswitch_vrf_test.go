package api

import (
	"testing"

	"github.com/xzjt/nfvis/internal/model"
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
