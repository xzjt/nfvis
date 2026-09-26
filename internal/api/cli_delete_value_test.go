package api

import (
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// 决策 #158：delete 值叶子/标量参数**带取值**时必须执行删除——此前 applyTokens 的
// pendingKey 与标量参数分支缺 isSet 判定，delete 带取值会反向写入（真机实测：
// delete interfaces ens224 description orig + commit 后描述仍是 orig）。
// 不带取值的 delete（关键字收尾）本来就正确，一并回归。

func deleteTree(t *testing.T, cfg model.Config, stmts ...string) map[string]any {
	t.Helper()
	for _, line := range stmts {
		toks := splitFieldsQuoted(line)
		var err error
		if toks[0] == "delete" {
			err = deleteStatement(&cfg, toks[1:]) // delete 走 deleteStatement（applyStatement 只做 set）
		} else {
			err = applyStatement(&cfg, toks[1:])
		}
		if err != nil {
			t.Fatalf("语句 %q: %v", line, err)
		}
	}
	return toJSONTree(cfg)
}

func TestDeleteValueLeafWithValueToken(t *testing.T) {
	var cfg model.Config
	for _, line := range []string{
		"configure",
		"set interfaces ens224 description orig",
		"set interfaces ens224 mtu 9000",
		"commit",
	} {
		toks := splitFieldsQuoted(line)
		if line == "configure" || line == "commit" {
			continue
		}
		if err := applyStatement(&cfg, toks[1:]); err != nil {
			t.Fatalf("前置 %q: %v", line, err)
		}
	}
	tree := deleteTree(t, cfg,
		"delete interfaces ens224 description orig", // setup 已保证在场；delete 带取值须删叶
	)
	ifc := tree["interfaces"].([]any)[0].(map[string]any)
	if _, ok := ifc["description"]; ok {
		t.Fatalf("delete 带取值应删除 description 叶: %v", ifc)
	}
	if ifc["mtu"] == nil {
		t.Fatalf("同元素其它字段不得被误删: %v", ifc)
	}
	// 不带取值的 delete 回归（本来就正确）
	tree = deleteTree(t, cfg, "delete interfaces ens224 description")
	ifc = tree["interfaces"].([]any)[0].(map[string]any)
	if _, ok := ifc["description"]; ok {
		t.Fatalf("delete 不带取值应删除 description 叶: %v", ifc)
	}
}

func TestDeleteValueLeafArrayPerValue(t *testing.T) {
	var cfg model.Config
	if err := applyStatement(&cfg, []string{"resource-pools", "cpu", "isolated-cores", "2-5"}); err != nil {
		t.Fatalf("前置: %v", err)
	}
	// delete 带取值：按值移除（范围展开），非全删
	tree := deleteTree(t, cfg, "delete resource-pools cpu isolated-cores 3")
	cpu := tree["resource_pools"].(map[string]any)["cpu"].(map[string]any)
	cores, _ := cpu["isolated_cores"].([]any)
	if len(cores) != 3 {
		t.Fatalf("按值移除后应剩 3 个核: %v", cores)
	}
	tree = deleteTree(t, cfg, "delete resource-pools cpu isolated-cores 2,3,4,5")
	cpu = tree["resource_pools"].(map[string]any)["cpu"].(map[string]any)
	if _, ok := cpu["isolated_cores"]; ok {
		t.Fatalf("全部移除后应删键: %v", cpu)
	}
}

func TestDeleteScalarParamWithValueToken(t *testing.T) {
	var cfg model.Config
	// 交换机端口成员：ports <seq> interface <if> 是 SP 标量参数（branch 2a）
	for _, line := range []string{
		"set virtual-switches vs1 type l2",
		"set virtual-switches vs1 ports 1 interface ens224",
	} {
		toks := splitFieldsQuoted(line)
		if err := applyStatement(&cfg, toks[1:]); err != nil {
			t.Fatalf("前置 %q: %v", line, err)
		}
	}
	tree := deleteTree(t, cfg, "delete virtual-switches vs1 ports 1 interface ens224")
	vs := tree["virtual_switches"].([]any)[0].(map[string]any)
	ports := vs["ports"].([]any)
	p0 := ports[0].(map[string]any)
	if _, ok := p0["interface"]; ok {
		t.Fatalf("delete 标量参数带取值应删除字段: %v", p0)
	}
}
