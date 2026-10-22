package api

// 真机缺陷：删掉最后一条 l3-interface 之后，配置树里留下只剩 `name` 的 `vrfs` 空壳，
// 使 `show configuration | display set` **永久**读不出来（如实报内部错误）：
//
//	delete virtual-switches vs-lan l3-interface ens224 + commit
//	show configuration | display set
//	%% display set 内部错误：生成语句未能完整还原配置……
//
// 成因：空壳里没有任何可发射的语句（`vrfs` 在命令树里没有关键字，只由
// `virtual-switches … type l3` / `l3-interface` / `static-routes` 派生），回放自校验因此
// 对不上——空 `vrfs` 条目与「从未配置」在读视图里不可区分，属同一类空壳（management /
// metrics / firewall / protocols 已在 pruneEmptySingleton 登记，vrfs 漏登）。
//
// 判据（与 Vrf 模型的字段一一对应：name / description / l3_interfaces / routes）：
// 除身份字段外只剩 nil 或空集合即视为空壳。**但**同名的 type=l3 虚拟交换机要求该条目存在
// （模型校验「type=l3 交换机需要同名 VRF 条目承载 L3 配置」），故这种空条目必须保留——
// 它可被 `type l3` 语句完整还原，不是残渣。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
)

// vrfsElems 取树里的 vrfs 数组（不存在返回 nil）。
func vrfsElems(tree map[string]any) []any {
	arr, _ := tree["vrfs"].([]any)
	return arr
}

// vrfElemNamed 取 vrfs 数组里 name 匹配的元素（不存在返回 nil）。
func vrfElemNamed(tree map[string]any, name string) map[string]any {
	em, _ := selectElement(vrfsElems(tree), "name", name)
	return em
}

// TestPruneVrfsShellRemoved：只剩 name 的 vrfs 元素回收；元素全被回收时 vrfs 键本身消失。
func TestPruneVrfsShellRemoved(t *testing.T) {
	tree := map[string]any{"vrfs": []any{map[string]any{"name": "vs-lan"}}}
	pruneEmptySingleton(tree)
	if _, ok := tree["vrfs"]; ok {
		t.Fatalf("只剩 name 的 vrfs 元素应被回收（且不留空数组）：%v", tree["vrfs"])
	}
}

// TestPruneVrfsOnlyEmptyCollectionsRemoved：仅剩空集合（l3_interfaces / routes）同样是空壳。
func TestPruneVrfsOnlyEmptyCollectionsRemoved(t *testing.T) {
	tree := map[string]any{"vrfs": []any{map[string]any{
		"name":          "vs-lan",
		"l3_interfaces": []any{},
		"routes":        []any{},
		"description":   nil,
	}}}
	pruneEmptySingleton(tree)
	if _, ok := tree["vrfs"]; ok {
		t.Fatalf("只剩空集合的 vrfs 元素应被回收：%v", tree["vrfs"])
	}
}

// TestPruneVrfsKeepsMeaningful：带任一条有内容字段（路由/L3 接口/描述）的一律保留。
func TestPruneVrfsKeepsMeaningful(t *testing.T) {
	cases := []struct {
		name string
		el   map[string]any
	}{
		{"带静态路由", map[string]any{"name": "vs-l3", "routes": []any{
			map[string]any{"prefix": "10.0.0.0/8", "next_hop": "10.10.0.1"}}}},
		{"带 L3 接口", map[string]any{"name": "vs-l3", "l3_interfaces": []any{
			map[string]any{"interface": "ens192", "addresses": []any{"10.10.0.1/24"}}}}},
		{"无地址的 L3 接口（决策 #472 有数据面语义）", map[string]any{"name": "vs-l3", "l3_interfaces": []any{
			map[string]any{"interface": "ens192"}}}},
		{"带描述", map[string]any{"name": "vs-l3", "description": "lab"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tree := map[string]any{"vrfs": []any{c.el}}
			pruneEmptySingleton(tree)
			if vrfElemNamed(tree, c.el["name"].(string)) == nil {
				t.Fatalf("有内容的 vrfs 元素不得被回收: %v", tree["vrfs"])
			}
		})
	}
}

// TestPruneVrfsKeepsShellRequiredByL3Switch：空条目若是 type=l3 交换机的同名 VRF 则保留
// （模型校验要求它存在；`virtual-switches <n> type l3` 语句能把它完整还原，不是残渣）。
func TestPruneVrfsKeepsShellRequiredByL3Switch(t *testing.T) {
	tree := map[string]any{
		"virtual_switches": []any{map[string]any{"name": "vs-l3", "type": "l3"}},
		"vrfs":             []any{map[string]any{"name": "vs-l3"}},
	}
	pruneEmptySingleton(tree)
	if vrfElemNamed(tree, "vs-l3") == nil {
		t.Fatalf("type=l3 交换机要求的同名空 VRF 不得被回收：%v", tree["vrfs"])
	}
	// 对照：同名的 L2 交换机不要求 VRF 存在，空条目照常回收。
	tree = map[string]any{
		"virtual_switches": []any{map[string]any{"name": "vs-l2", "type": "l2"}},
		"vrfs":             []any{map[string]any{"name": "vs-l2"}},
	}
	pruneEmptySingleton(tree)
	if _, ok := tree["vrfs"]; ok {
		t.Fatalf("L2 交换机的同名空 VRF 是残渣，应回收：%v", tree["vrfs"])
	}
}

// TestPruneVrfsKeepsGatewayReferencedShell：空条目被交换机 `gateway vrf <名>` 显式引用时保留
// （模型校验要求该 VRF 存在；剪掉会让配置自相矛盾）。
func TestPruneVrfsKeepsGatewayReferencedShell(t *testing.T) {
	tree := map[string]any{
		"virtual_switches": []any{map[string]any{
			"name": "vs-l2", "type": "l2",
			"gateway": map[string]any{"vrf": "vrf-ext"},
		}},
		"vrfs": []any{map[string]any{"name": "vrf-ext"}},
	}
	pruneEmptySingleton(tree)
	if vrfElemNamed(tree, "vrf-ext") == nil {
		t.Fatalf("被 gateway vrf 引用的空 VRF 不得被回收：%v", tree["vrfs"])
	}
}

// TestPruneVrfsMixedArray：数组里既有残渣又有真数据时只摘残渣，vrfs 键保留。
func TestPruneVrfsMixedArray(t *testing.T) {
	tree := map[string]any{"vrfs": []any{
		map[string]any{"name": "vs-shell"},
		map[string]any{"name": "vs-live", "routes": []any{
			map[string]any{"prefix": "0.0.0.0/0", "next_hop": "10.10.0.1"}}},
	}}
	pruneEmptySingleton(tree)
	if vrfElemNamed(tree, "vs-shell") != nil {
		t.Fatalf("空壳元素应被摘掉：%v", tree["vrfs"])
	}
	if vrfElemNamed(tree, "vs-live") == nil {
		t.Fatalf("有数据的元素必须保留：%v", tree["vrfs"])
	}
	if len(vrfsElems(tree)) != 1 {
		t.Fatalf("应只剩 1 条 vrfs 元素：%v", tree["vrfs"])
	}
}

// TestDeleteLastL3InterfaceLeavesNoVrfsShell：真机复现的语句序列——删最后一条 l3-interface
// 后配置里不得残留 vrfs 空壳（L2 交换机形态）。
func TestDeleteLastL3InterfaceLeavesNoVrfsShell(t *testing.T) {
	var cfg model.Config
	for _, line := range []string{
		"set interfaces ens224",
		"set virtual-switches vs-lan type l2",
		"set virtual-switches vs-lan l3-interface ens224 ip address 10.10.0.1/24",
	} {
		if err := applyStatement(&cfg, splitFieldsQuoted(line)[1:]); err != nil {
			t.Fatalf("前置 %q: %v", line, err)
		}
	}
	if len(cfg.Vrfs) != 1 || len(cfg.Vrfs[0].L3Interfaces) != 1 {
		t.Fatalf("前置不成立：应有一条 l3-interface，实际 %+v", cfg.Vrfs)
	}
	if err := deleteStatement(&cfg, splitFieldsQuoted("delete virtual-switches vs-lan l3-interface ens224")[1:]); err != nil {
		t.Fatalf("删 l3-interface: %v", err)
	}
	if len(cfg.Vrfs) != 0 {
		t.Fatalf("删掉最后一条 l3-interface 后不得残留 vrfs 条目：%+v", cfg.Vrfs)
	}
	if _, ok := toJSONTree(cfg)["vrfs"]; ok {
		t.Fatalf("配置树里不得留下 vrfs 键：%v", toJSONTree(cfg))
	}
}

// TestDisplaySetAfterDeletingLastL3Interface：display set 必须仍可读——空 `vrfs` 壳会让
// 回放自校验对不上而报内部错误（真机缺陷的可见症状）。
func TestDisplaySetAfterDeletingLastL3Interface(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set interfaces ens224",
		"set virtual-switches vs-lan type l2",
		"set virtual-switches vs-lan l3-interface ens224 ip address 10.10.0.1/24",
		"commit",
	)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"delete virtual-switches vs-lan l3-interface ens224",
		"commit",
		"exit",
	)
	cfg, err := engine.Committed()
	if err != nil {
		t.Fatalf("读取 committed: %v", err)
	}
	if len(cfg.Vrfs) != 0 {
		t.Fatalf("committed 里不得残留 vrfs 空壳：%+v", cfg.Vrfs)
	}
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration | display set")
	if strings.Contains(res.Output, "内部错误") {
		t.Fatalf("display set 必须在无 vrfs 空壳的配置上可读：%s", res.Output)
	}
	if !strings.Contains(res.Output, "set virtual-switches vs-lan type l2") {
		t.Fatalf("display set 应照常反推出 L2 交换机语句：%s", res.Output)
	}
}
