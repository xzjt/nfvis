package api

// 决策 #472：无地址 l3-interface 的 display set 自足化（round25 R25-1，#452 同族第三形态）。
//
// 真机现场：`delete virtual-switches <n> l3-interface <if> ip address <p>` **只删地址叶**时
// 留下「无地址的 l3-interface」条目——条目本身有数据面语义（该接口仍属此 VRF/表，ApplyVRF
// 按接口清单驱动成员关系），不能靠删条目或剥离字段绕过；而该形态此前**任何 set 语句都表达
// 不了**（裸 set 被「配置不完整，缺少取值」拒绝），display set 回放自校验必失败。
//
// 本文件钉住：① 裸 set ＝确保条目存在（幂等，重复执行落 #190 的 noChangeError 提示口径）；
// ② delete 分支逐字不动（删地址保留条目、删条目后无匹配报错）；③ 生成器对无地址条目发射
// 裸形态行、有地址条目输出逐字不变；④ 生成的语句回放能还原原配置（自足）。

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// runMixed472 逐行按首 token（set/delete）回放，返回配置 JSON 树。
// display set 的 fixture 常同时含两条路径（set 建条目 + delete 删地址叶），
// 既有 runDisplaySetRoundTrip 一律按 set 处理，不适用于混合 fixture。
func runMixed472(t *testing.T, lines ...string) map[string]any {
	t.Helper()
	var cfg model.Config
	for _, line := range lines {
		toks := splitFieldsQuoted(line)
		var err error
		switch toks[0] {
		case "set":
			err = applyStatement(&cfg, toks[1:])
		case "delete":
			err = deleteStatement(&cfg, toks[1:])
		default:
			t.Fatalf("fixture 行首 token 须为 set/delete: %q", line)
		}
		if err != nil {
			t.Fatalf("fixture 语句 %q 失败（fixture 本身必须合法）: %v", line, err)
		}
	}
	return toJSONTree(cfg)
}

// stmtLines472 setStmt 列表 → 输出行（与 renderSetStatements 同一拼接口径）。
func stmtLines472(w *stmtWriter) []string {
	lines := make([]string, 0, len(w.out)+len(w.notes))
	for _, s := range w.out {
		lines = append(lines, joinStatementTokens(append([]string{"set"}, s.toks...)))
	}
	for _, n := range w.notes {
		lines = append(lines, "# "+n)
	}
	return lines
}

// TestBareL3InterfaceSetEnsuresEntry（#472 ①）：裸 set 建出条目（不带地址）、重复执行幂等
// 落 noChangeError（交互/脚本层即 #190「值未变化」提示）。
func TestBareL3InterfaceSetEnsuresEntry(t *testing.T) {
	var cfg model.Config
	if err := applyStatement(&cfg, splitFieldsQuoted("set virtual-switches vs-l3 type l3")[1:]); err != nil {
		t.Fatalf("fixture 语句失败: %v", err)
	}
	if err := applyStatement(&cfg, splitFieldsQuoted("set virtual-switches vs-l3 l3-interface ens192")[1:]); err != nil {
		t.Fatalf("裸 set 应建出 l3-interface 条目: %v", err)
	}
	if len(cfg.Vrfs) != 1 || len(cfg.Vrfs[0].L3Interfaces) != 1 {
		t.Fatalf("条目未按期望建出: %+v", cfg.Vrfs)
	}
	li := cfg.Vrfs[0].L3Interfaces[0]
	if li.Interface != "ens192" {
		t.Fatalf("条目身份字段 = %q，期望 ens192", li.Interface)
	}
	if len(li.Addresses) != 0 || li.AclIn != "" {
		t.Fatalf("裸形态只声明条目、不应带地址/ACL: %+v", li)
	}
	err := applyStatement(&cfg, splitFieldsQuoted("set virtual-switches vs-l3 l3-interface ens192")[1:])
	var nc *noChangeError
	if !errors.As(err, &nc) {
		t.Fatalf("重复裸 set 应落 noChangeError（#190 提示口径），实际: %v", err)
	}
	// 地址仍可逐条追加（裸形态与地址叶共存）。
	if err := applyStatement(&cfg, splitFieldsQuoted("set virtual-switches vs-l3 l3-interface ens192 ip address 10.0.0.1/24")[1:]); err != nil {
		t.Fatalf("裸形态后追加地址失败: %v", err)
	}
	if got := cfg.Vrfs[0].L3Interfaces[0].Addresses; len(got) != 1 || got[0] != "10.0.0.1/24" {
		t.Fatalf("地址追加结果 = %v", got)
	}
}

// TestL3InterfaceDeleteSemanticsUnchanged（#472 边界）：delete 分支逐字不动——
// 删地址叶保留条目（残形即本决策的现场）、删条目整条移除、再删无匹配报错。
func TestL3InterfaceDeleteSemanticsUnchanged(t *testing.T) {
	tree := runMixed472(t,
		"set virtual-switches vs-l3 type l3",
		"set virtual-switches vs-l3 l3-interface ens192 ip address 10.0.0.1/24",
		"delete virtual-switches vs-l3 l3-interface ens192 ip address 10.0.0.1/24",
	)
	var cfg model.Config
	if err := fromJSONTree(tree, &cfg); err != nil {
		t.Fatalf("反序列化 fixture 失败: %v", err)
	}
	if len(cfg.Vrfs) != 1 || len(cfg.Vrfs[0].L3Interfaces) != 1 || cfg.Vrfs[0].L3Interfaces[0].Interface != "ens192" {
		t.Fatalf("删地址叶应保留条目: %+v", cfg.Vrfs)
	}
	if len(cfg.Vrfs[0].L3Interfaces[0].Addresses) != 0 {
		t.Fatalf("地址应已删除: %+v", cfg.Vrfs[0].L3Interfaces[0])
	}
	// 删条目：整条移除；再删无匹配报错（既有语义）
	if err := deleteStatement(&cfg, splitFieldsQuoted("delete virtual-switches vs-l3 l3-interface ens192")[1:]); err != nil {
		t.Fatalf("删条目失败: %v", err)
	}
	if len(cfg.Vrfs[0].L3Interfaces) != 0 {
		t.Fatalf("删条目后 L3Interfaces 应清空: %+v", cfg.Vrfs[0])
	}
	err := deleteStatement(&cfg, splitFieldsQuoted("delete virtual-switches vs-l3 l3-interface ens192")[1:])
	if err == nil || !strings.Contains(err.Error(), "无匹配配置") {
		t.Fatalf("再删应报「无匹配配置」，实际: %v", err)
	}
}

// TestDisplaySetNoAddressL3InterfaceRoundTrip（#472 ②）：删地址叶留下的无地址条目必须能
// 被 display set 反推（发射裸形态行）并回放还原——修复前此处必红（回放自校验「生成语句
// 未能完整还原配置」）。
func TestDisplaySetNoAddressL3InterfaceRoundTrip(t *testing.T) {
	orig := runMixed472(t,
		"set virtual-switches vs-l3 type l3",
		"set virtual-switches vs-l3 l3-interface ens192 ip address 10.0.0.1/24",
		"delete virtual-switches vs-l3 l3-interface ens192 ip address 10.0.0.1/24",
	)
	// 前提：残形确为「有条目、无地址」（断言 fixture 现场成立）
	vrfs, _ := orig["vrfs"].([]any)
	if len(vrfs) != 1 {
		t.Fatalf("fixture 现场不符：vrfs = %v", orig["vrfs"])
	}
	lis, _ := vrfs[0].(map[string]any)["l3_interfaces"].([]any)
	if len(lis) != 1 {
		t.Fatalf("fixture 现场不符：l3_interfaces = %v", vrfs[0])
	}
	if _, has := lis[0].(map[string]any)["addresses"]; has {
		t.Fatalf("fixture 前置失效：地址应已删除: %v", lis[0])
	}

	w, err := generateSetStmts(orig, nil)
	if err != nil {
		t.Fatalf("无地址条目的 display set 反推失败（#472 修复前必红）: %v\n原配置: %v", err, orig)
	}
	lines := stmtLines472(w)
	joined := strings.Join(lines, "\n")
	t.Logf("无地址条目的 display set 输出:\n%s", joined)
	// 输出须含裸形态行（精确整行，避免被 ip address 行前缀误匹配）
	bare := "set virtual-switches vs-l3 l3-interface ens192"
	hasBare := false
	for _, l := range lines {
		if l == bare {
			hasBare = true
		}
	}
	if !hasBare {
		t.Fatalf("应发射裸形态行 %q，实际:\n%s", bare, joined)
	}
	// 自足：生成的语句在空配置上回放，结果与原配置深等
	var rebuilt model.Config
	for _, s := range w.out {
		if err := applyStatement(&rebuilt, s.toks); err != nil {
			t.Fatalf("反推语句 %v 回放失败: %v", s.toks, err)
		}
	}
	want := normalizeJSON(stripUnreplayable(any(orig)))
	got := normalizeJSON(stripUnreplayable(any(toJSONTree(rebuilt))))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("往返不等：\n原配置   %v\n反推语句 %v\n回放结果 %v", orig, lines, got)
	}
	if len(rebuilt.Vrfs) != 1 || len(rebuilt.Vrfs[0].L3Interfaces) != 1 ||
		rebuilt.Vrfs[0].L3Interfaces[0].Interface != "ens192" {
		t.Fatalf("回放后 VRF 成员应还原（接口属该 VRF/表）: %+v", rebuilt.Vrfs)
	}
}

// TestDisplaySetNoAddressL3InterfaceWithACLRoundTrip（#472 ②）：无地址但带 acl-in 的条目
// 同样自足——裸声明须在 ACL 叶**之前**（回放时空配置从裸声明建出条目；顺序颠倒则裸声明
// 撞「值未变化」而回放失败）。一并往返有地址条目，覆盖混合形态。
func TestDisplaySetNoAddressL3InterfaceWithACLRoundTrip(t *testing.T) {
	orig := runMixed472(t,
		"set virtual-switches vs-l3 type l3",
		"set virtual-switches vs-l3 l3-interface ens192 acl-in acl1",
		"set virtual-switches vs-l3 l3-interface ens224 ip address 10.0.1.1/24",
		"delete virtual-switches vs-l3 l3-interface ens224 ip address 10.0.1.1/24",
	)
	w, err := generateSetStmts(orig, nil)
	if err != nil {
		t.Fatalf("混合形态反推失败: %v", err)
	}
	lines := stmtLines472(w)
	joined := strings.Join(lines, "\n")
	// 裸声明在 acl-in 行之前
	bare := "set virtual-switches vs-l3 l3-interface ens192"
	aclLine := bare + " acl-in acl1"
	iBare, iAcl := -1, -1
	for i, l := range lines {
		switch l {
		case bare:
			iBare = i
		case aclLine:
			iAcl = i
		}
	}
	if iBare < 0 || iAcl < 0 || iBare > iAcl {
		t.Fatalf("裸声明应存在且在 acl-in 行之前（回放序要求），实际:\n%s", joined)
	}
	var rebuilt model.Config
	for _, s := range w.out {
		if err := applyStatement(&rebuilt, s.toks); err != nil {
			t.Fatalf("反推语句 %v 回放失败: %v", s.toks, err)
		}
	}
	want := normalizeJSON(stripUnreplayable(any(orig)))
	got := normalizeJSON(stripUnreplayable(any(toJSONTree(rebuilt))))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("往返不等：\n原配置   %v\n反推语句 %v\n回放结果 %v", orig, lines, got)
	}
}

// TestDisplaySetWithAddressL3InterfaceOutputUnchanged（#472 ②）：有地址条目的输出与修复前
// 逐字一致（acl-in 行在前、ip address 行随后，不掺裸形态行）——对照修复前同配置输出锚定。
func TestDisplaySetWithAddressL3InterfaceOutputUnchanged(t *testing.T) {
	orig := runMixed472(t,
		"set virtual-switches vs-l3 type l3",
		"set virtual-switches vs-l3 l3-interface ens192 acl-in acl1",
		"set virtual-switches vs-l3 l3-interface ens192 ip address 10.0.0.1/24",
		"set virtual-switches vs-l3 l3-interface ens192 ip address fd00::1/64",
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
		"set virtual-switches vs-l3 l3-interface ens192 ip address fd00::1/64",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("有地址条目输出应与修复前逐字一致：\n实际:\n%s\n期望:\n%s",
			strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}
