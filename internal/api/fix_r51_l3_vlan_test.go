package api

// round51 评审 H3b：L3 接口的 VLAN 子接口（`model.L3Interface.Vlan`）**从 CLI 不可达**，
// 且 display set 反推不出该字段 —— 于是「REST / load 写入 vlan 的配置」一旦被
// `show configuration | display set` 读到，回放自校验必然对不上，整条读命令报
// 「display set 内部错误…请报告缺陷」（操作者拿不到配置）。
//
// 三面同源的修法：
//   - 树：`l3-interface <ifname|vlan>` 下加可选 `vlan <v>` 叶子（顺序在 `ip`/`acl-in` 之前）；
//   - 别名：`virtual-switches <n> l3-interface <if> vlan <v>` 写 `li["vlan"]`（delete 清该键）；
//   - 发射器：条目有 `vlan > 0` 时发 `vlan <v>` 语句——**它自带建元素**（别名走
//     l3VrfOf+elemByField 的创建路径），故顶替裸声明位置、仍是「创建语句最先」。
//
// 本文件钉住三面与往返：CLI 写入/删除、`?` 候选含 vlan、display set 发射顺序、
// 空配置回放深等。

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/schema"
)

// r51VlanTree 构造一个「L3 交换机 + 一条带 vlan 的 l3-interface」的 JSON 树
// （模拟 REST / load 写入的形态——CLI 语句此时还表达不了）。
func r51VlanTree(t *testing.T, vlan int, addrs ...string) map[string]any {
	t.Helper()
	li := map[string]any{"interface": "ens192"}
	if vlan > 0 {
		li["vlan"] = vlan
	}
	if len(addrs) > 0 {
		arr := make([]any, 0, len(addrs))
		for _, a := range addrs {
			arr = append(arr, a)
		}
		li["addresses"] = arr
	}
	tree := map[string]any{
		"virtual_switches": []any{map[string]any{"name": "vs-l3", "type": "l3"}},
		"vrfs": []any{map[string]any{
			"name":          "vs-l3",
			"l3_interfaces": []any{li},
		}},
	}
	var cfg model.Config
	if err := fromJSONTree(tree, &cfg); err != nil {
		t.Fatalf("fixture 反序列化失败: %v", err)
	}
	if len(cfg.Vrfs) != 1 || len(cfg.Vrfs[0].L3Interfaces) != 1 || cfg.Vrfs[0].L3Interfaces[0].Vlan != vlan {
		t.Fatalf("fixture 现场不符（vlan 未落模型）: %+v", cfg.Vrfs)
	}
	// 与生产同形：display set 的输入树来自 toJSONTree（JSON 往返 ⇒ 数值一律 float64）
	return toJSONTree(cfg)
}

// r51ReplaySet 把发射出的语句在空配置上回放，返回回放后的 JSON 树。
func r51ReplaySet(t *testing.T, w *stmtWriter) map[string]any {
	t.Helper()
	var rebuilt model.Config
	for _, s := range w.out {
		if err := applyStatement(&rebuilt, s.toks); err != nil {
			t.Fatalf("反推语句 %v 回放失败: %v", s.toks, err)
		}
	}
	return toJSONTree(rebuilt)
}

// TestFixR51DisplaySetL3VlanRoundTrip：带 vlan 的 l3-interface 必须能反推（修复前必红：
// 回放自校验「生成语句未能完整还原配置」）且回放还原原配置。
func TestFixR51DisplaySetL3VlanRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		vlan  int
		addrs []string
	}{
		{"仅有 vlan（无地址、无 ACL）", 100, nil},
		{"vlan + 地址", 200, []string{"10.0.0.1/24", "fd00::1/64"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := r51VlanTree(t, tc.vlan, tc.addrs...)
			w, err := generateSetStmts(orig, nil)
			if err != nil {
				t.Fatalf("带 vlan 的 l3-interface 反推失败（修复前必红）: %v\n原配置: %v", err, orig)
			}
			lines := stmtLines472(w)
			joined := strings.Join(lines, "\n")
			wantVlan := "set virtual-switches vs-l3 l3-interface ens192 vlan " + strconv.Itoa(tc.vlan)
			hasVlan := false
			for _, l := range lines {
				if l == wantVlan {
					hasVlan = true
				}
			}
			if !hasVlan {
				t.Fatalf("应发射 %q，实际:\n%s", wantVlan, joined)
			}
			// 创建语句最先：vlan 行（或裸声明行）必须排在 acl-in / ip address 行之前
			iVlan, iFirstLeaf := -1, -1
			for i, l := range lines {
				switch {
				case l == wantVlan:
					iVlan = i
				case strings.HasPrefix(l, "set virtual-switches vs-l3 l3-interface ens192 acl-in"),
					strings.HasPrefix(l, "set virtual-switches vs-l3 l3-interface ens192 ip address"):
					if iFirstLeaf < 0 {
						iFirstLeaf = i
					}
				}
			}
			if iVlan < 0 || (iFirstLeaf >= 0 && iVlan > iFirstLeaf) {
				t.Fatalf("vlan 语句（创建语句）须最先，实际:\n%s", joined)
			}
			// 自足：空配置回放 == 原配置
			want := normalizeJSON(stripUnreplayable(any(orig)))
			got := normalizeJSON(stripUnreplayable(any(r51ReplaySet(t, w))))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("往返不等：\n原配置   %v\n反推语句 %v\n回放结果 %v", orig, lines, got)
			}
			// 回放后 vlan 真的还原（不靠 stripNonEmittable 抹平）
			rebuilt := mustConfig(t, r51ReplaySet(t, w))
			if len(rebuilt.Vrfs) != 1 || len(rebuilt.Vrfs[0].L3Interfaces) != 1 ||
				rebuilt.Vrfs[0].L3Interfaces[0].Vlan != tc.vlan {
				t.Fatalf("回放后 vlan 未还原: %+v", rebuilt.Vrfs)
			}
		})
	}
}

// TestFixR51DisplaySetL3NoVlanOutputUnchanged：不带 vlan 的条目输出逐字不变（对照既有锚定）。
func TestFixR51DisplaySetL3NoVlanOutputUnchanged(t *testing.T) {
	orig := runMixed472(t,
		"set virtual-switches vs-l3 type l3",
		"set virtual-switches vs-l3 l3-interface ens192 acl-in acl1",
		"set virtual-switches vs-l3 l3-interface ens192 ip address 10.0.0.1/24",
	)
	lines, err := renderSetStatements(orig, nil)
	if err != nil {
		t.Fatalf("反推失败: %v", err)
	}
	want := []string{
		"set virtual-switches vs-l3",
		"set virtual-switches vs-l3 type l3",
		"set virtual-switches vs-l3 l3-interface ens192 acl-in acl1",
		"set virtual-switches vs-l3 l3-interface ens192 ip address 10.0.0.1/24",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("无 vlan 条目的输出应与修复前逐字一致：\n实际:\n%s\n期望:\n%s",
			strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// TestFixR51CLIL3InterfaceVlan：三面同源的 CLI 面——`?` 候选含 vlan、set 写模型、delete 清字段。
func TestFixR51CLIL3InterfaceVlan(t *testing.T) {
	x, engine := newCLIKit(t)
	// `?` 候选（与命令树/服务端 /cli/candidates 同源函数）：给定 `l3-interface <if>` 之后应提示 vlan
	cands := schema.Candidates(schema.ConfigPathTree(),
		[]string{"virtual-switches", "vs-l3", "l3-interface", "ens192"}, "", nil)
	hasVlan := false
	for _, c := range cands {
		if c.Token == "vlan" {
			hasVlan = true
		}
	}
	if !hasVlan {
		t.Fatalf("`l3-interface <if> ?` 候选应含 vlan，实得: %+v", cands)
	}

	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set virtual-switches vs-l3 type l3",
		"set interfaces ens192",
		"set virtual-switches vs-l3 l3-interface ens192 vlan 100",
		"set virtual-switches vs-l3 l3-interface ens192 ip address 10.0.0.1/24",
	)
	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Vrfs) != 1 || len(cfg.Vrfs[0].L3Interfaces) != 1 {
		t.Fatalf("vlan 语句未落模型: %+v", cfg.Vrfs)
	}
	li := cfg.Vrfs[0].L3Interfaces[0]
	if li.Vlan != 100 || li.Interface != "ens192" || len(li.Addresses) != 1 {
		t.Fatalf("vlan 未落到 L3Interface.Vlan: %+v", li)
	}
	// 读视图：`show configuration` 读**committed**（契约 §1.1「省略子命令才是读 committed」），
	// 故先提交再看 display set
	run(t, x, "admin", aaaClassSU, "ssh", "commit")
	view := x.Execute("admin", aaaClassSU, "ssh", "show configuration | display set").Output
	if strings.Contains(view, "内部错误") {
		t.Fatalf("display set 反推带 vlan 的配置失败:\n%s", view)
	}
	if !strings.Contains(view, "l3-interface ens192 vlan 100") {
		t.Fatalf("display set 应给出 vlan 语句:\n%s", view)
	}
	// delete 清字段（条目保留）
	run(t, x, "admin", aaaClassSU, "ssh", "delete virtual-switches vs-l3 l3-interface ens192 vlan 100")
	cfg, _, err = engine.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Vrfs[0].L3Interfaces[0].Vlan != 0 {
		t.Fatalf("delete 应清掉 vlan: %+v", cfg.Vrfs[0].L3Interfaces[0])
	}
	// delete 不得凭空建条目（与 H3 同判据）
	x2, _ := newCLIKit(t)
	run(t, x2, "admin", aaaClassSU, "ssh", "configure", "set virtual-switches vs-z type l3")
	res := x2.Execute("admin", aaaClassSU, "ssh", "delete virtual-switches vs-z l3-interface ens224 vlan 100")
	if !strings.Contains(res.Output, "无匹配配置") {
		t.Fatalf("条目不存在时删 vlan 应报「无匹配配置」: %q", res.Output)
	}
	// 越界取值在语句层挡下（1-4094；编排层按 uint16 截断，放过去就是静默错 VLAN）
	res = x2.Execute("admin", aaaClassSU, "ssh", "set virtual-switches vs-z l3-interface ens224 vlan 5000")
	if !strings.Contains(res.Output, "1-4094") {
		t.Fatalf("越界 vlan 应被拒并点名范围: %q", res.Output)
	}
}

// 组合形态（round51 真机首验发现，决策 #525 追加）：`… l3-interface <if> vlan <v> ip address <p>`
// 与 `… vlan <v> acl-in <a>` 必须可用。
//
// 由来：命令树枚举出的**规范序**就是 `l3-interface <if> vlan <v> ip address <p>`（vlan 叶子声明
// 在 ip 之前，与 BUG-21「可选修饰排在必需参数之后」同序），也是用户手册里推荐的写法；但别名表
// 按 token 数精确匹配，这两条组合形态先前无规则可命中，于是落到通用树遍历——而模型把
// l3-interface 落在 `l3_interfaces`（复数数组），遍历必然报「配置中不存在字段 "l3_interface"」。
// 真机实测：`set … l3-interface ens224 vlan 100 ip address 10.99.0.1/24` 整条失败，
// 而分步写（`… vlan 100` 一条、`… ip address …` 一条）成功——文档推荐写法不可用即为缺陷。
func TestFixR51CLIL3InterfaceVlanCombinedForms(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set virtual-switches vs-c type l3",
		"set interfaces ens224",
		"set virtual-switches vs-c l3-interface ens224 vlan 100 ip address 10.99.0.1/24",
		"set virtual-switches vs-c l3-interface ens224 vlan 100 acl-in acl-x",
	)
	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Vrfs) != 1 || len(cfg.Vrfs[0].L3Interfaces) != 1 {
		t.Fatalf("组合形态未落模型: %+v", cfg.Vrfs)
	}
	li := cfg.Vrfs[0].L3Interfaces[0]
	if li.Vlan != 100 || li.Interface != "ens224" || len(li.Addresses) != 1 || li.Addresses[0] != "10.99.0.1/24" {
		t.Fatalf("组合形态的 vlan/地址未落模型: %+v", li)
	}
	if li.AclIn != "acl-x" {
		t.Fatalf("组合形态的 acl-in 未落模型: %+v", li)
	}
	// 组合形态与分步写法必须**逐字等价**（同一棵候选树比较）
	x2, engine2 := newCLIKit(t)
	run(t, x2, "admin", aaaClassSU, "ssh",
		"configure",
		"set virtual-switches vs-c type l3",
		"set interfaces ens224",
		"set virtual-switches vs-c l3-interface ens224 vlan 100",
		"set virtual-switches vs-c l3-interface ens224 ip address 10.99.0.1/24",
		"set virtual-switches vs-c l3-interface ens224 acl-in acl-x",
	)
	got1, _, err := engine.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	got2, _, err := engine2.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got1.Vrfs, got2.Vrfs) {
		t.Fatalf("组合形态与分步写法不等价:\n组合=%+v\n分步=%+v", got1.Vrfs, got2.Vrfs)
	}
}
