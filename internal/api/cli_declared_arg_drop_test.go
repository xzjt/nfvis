package api

// 本批收口决策 #153 里**登记但未修**的两条「声明了却被静默丢弃」缺陷（round80 真机测试的延伸）。
// 两条同属「静默误答」：契约声明了参数，执行器却把参数整段丢掉、按另一种语义作答——
// 操作者以为自己拿到了「所问的那份事实」。
//
//	① `show lldp neighbors interface <ifname>`：过滤参数被 `execShowLldp` 丢掉（只透传
//	   `lldp neighbors`），问「某个口的邻居」拿到的是全量邻居表。
//	② `show configuration permissions <class>`：class 参数被忽略，直接渲染 committed 配置正文。
//
// 判据取「同一事实源对照」：过滤必须**真的改变结果**；未实现必须**明说**，不得静默换语义。
//
// 为什么 ① 只能用桩：真机（nfvis-vm）无 LLDP 对端，邻居表恒为空（`docs/evidence/m5/t07-span-lldp.txt`），
// 空表上「过滤生效」与「过滤被丢」输出完全相同——所以过滤规则抽成纯函数单测，
// 执行器侧用可注入的邻居来源（`LldpRuntime`）与端口清单（`PortInventory`）桩验证。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/schema"
)

// lldpStub 假的 LLDP 邻居来源（真机恒为空表，故过滤只能靠桩验证）。
type lldpStub struct {
	rows []LldpNeighborRow
	err  error
}

func (f *lldpStub) Neighbors(context.Context) ([]LldpNeighborRow, error) {
	return f.rows, f.err
}

// lldpStubRows 两个口各有邻居的邻居表（chassis 用不同名字，便于断言「哪条被过滤掉」）。
func lldpStubRows() []LldpNeighborRow {
	return []LldpNeighborRow{
		{Interface: "ens192", ChassisID: "sw-alpha", PortID: "Gi0/1", TTL: 120},
		{Interface: "ens224", ChassisID: "sw-beta", PortID: "Gi0/2", TTL: 90},
	}
}

// ---------- ① show lldp neighbors interface <ifname> ----------

// TestFilterLLDPNeighborsPureFunction：过滤规则本身（纯函数，不碰底座/执行器状态）。
func TestFilterLLDPNeighborsPureFunction(t *testing.T) {
	rows := lldpStubRows()
	cases := []struct {
		name   string
		ifname string
		want   []string // 期望保留的 chassis（顺序同入参）
	}{
		{"空接口名 = 不过滤（全量）", "", []string{"sw-alpha", "sw-beta"}},
		{"按接口只留该口", "ens224", []string{"sw-beta"}},
		{"另一个口", "ens192", []string{"sw-alpha"}},
		{"无匹配返回空，而不是回全量", "ens999", nil},
		{"接口名原样精确匹配（VPP 接口名大小写敏感）", "ENS224", nil},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := filterLLDPNeighbors(rows, c.ifname)
			if len(got) != len(c.want) {
				t.Fatalf("ifname=%q 应保留 %d 条，实得 %d 条（%v）", c.ifname, len(c.want), len(got), got)
			}
			for i, w := range c.want {
				if got[i].ChassisID != w {
					t.Fatalf("ifname=%q 第 %d 条应为 %q，实得 %q", c.ifname, i, w, got[i].ChassisID)
				}
			}
		})
	}
	// 空表/未接入（nil）也要安全
	if got := filterLLDPNeighbors(nil, "ens224"); len(got) != 0 {
		t.Fatalf("nil 表过滤应得空：%v", got)
	}
}

// TestShowLldpNeighborsInterfaceFilterApplies：过滤参数必须**真的生效**
// （此前被丢，`interface ens192` 与不过滤输出完全相同），且等价写法仍逐字相同。
func TestShowLldpNeighborsInterfaceFilterApplies(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setNetRuntime(nil, nil, &lldpStub{rows: lldpStubRows()}, nil, nil)
	x.setPorts(fakePorts{vpp: []string{"ens192", "ens224"}})

	all := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show lldp neighbors").Output
	for _, w := range []string{"sw-alpha", "sw-beta"} {
		if !strings.Contains(all, w) {
			t.Fatalf("前置：不过滤应含 %q: %q", w, all)
		}
	}
	// 契约 §1.1：`show protocols lldp neighbors` 是同一读物的等价写法 → 逐字相同
	if eq := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show protocols lldp neighbors").Output; eq != all {
		t.Fatalf("等价写法应逐字相同:\n  不写 protocols: %q\n  写 protocols: %q", all, eq)
	}

	only := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show lldp neighbors interface ens192").Output
	if !strings.Contains(only, "sw-alpha") {
		t.Fatalf("应保留 ens192 的邻居: %q", only)
	}
	if strings.Contains(only, "sw-beta") {
		t.Fatalf("过滤未生效：仍含另一口（ens224）的邻居: %q", only)
	}
	if only == all {
		t.Fatalf("过滤后与全量逐字相同（参数被丢）: %q", only)
	}
	// 另一个方向也要真的按口过滤（避免「只过滤 ens192」这类硬编码）
	other := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show lldp neighbors interface ens224").Output
	if !strings.Contains(other, "sw-beta") || strings.Contains(other, "sw-alpha") {
		t.Fatalf("ens224 的过滤结果不对: %q", other)
	}
}

// TestShowLldpNeighborsNoMatchMessage：过滤无匹配时给**明确文案**
// ——不回全量、也不是静默空表。
func TestShowLldpNeighborsNoMatchMessage(t *testing.T) {
	x, _ := newCLIKit(t)
	// ens224 在本机（VPP 端口清单里），但邻居表里没有它的条目
	x.setNetRuntime(nil, nil, &lldpStub{rows: []LldpNeighborRow{
		{Interface: "ens192", ChassisID: "sw-alpha", PortID: "Gi0/1", TTL: 120},
	}}, nil, nil)
	x.setPorts(fakePorts{vpp: []string{"ens192", "ens224"}})

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show lldp neighbors interface ens224").Output
	if !strings.Contains(out, "接口 ens224 无 LLDP 邻居") {
		t.Fatalf("无匹配应给明确文案（含所问接口名）: %q", out)
	}
	if strings.Contains(out, "sw-alpha") {
		t.Fatalf("无匹配不得回全量邻居表: %q", out)
	}
	if strings.HasPrefix(out, "%") {
		t.Fatalf("「该口无邻居」是空态而非错误（与既有「（无 LLDP 邻居）」同族）: %q", out)
	}
}

// TestShowLldpNeighborsUnknownInterfaceMessage：接口名不在环境里（既没在配置中声明、
// 也不在 VPP 接口清单中）时，按同文件既有风格报「未在配置中声明」，
// **不能**谎称「该口无邻居」（那会把「名字写错了」说成「这个口就是没邻居」）。
func TestShowLldpNeighborsUnknownInterfaceMessage(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setNetRuntime(nil, nil, &lldpStub{rows: lldpStubRows()}, nil, nil)
	x.setPorts(fakePorts{vpp: []string{"ens192", "ens224"}})

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show lldp neighbors interface ens999").Output
	if !strings.Contains(out, "未在配置中声明") {
		t.Fatalf("未知接口应按既有风格报「未在配置中声明」: %q", out)
	}
	if !strings.HasPrefix(out, "%") {
		t.Fatalf("未知接口是错误（应带 %% 前缀）: %q", out)
	}
	if strings.Contains(out, "无 LLDP 邻居") {
		t.Fatalf("不得把「名字不存在」说成「该口无邻居」: %q", out)
	}
	if strings.Contains(out, "sw-alpha") {
		t.Fatalf("不得回全量邻居表: %q", out)
	}

	// 端口清单查询失败时**不据此判未知**（宁可少报错，不冤枉真实存在的口）
	x.setPorts(fakePorts{vppErr: errors.New("VPP 未接入")})
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show lldp neighbors interface ens224").Output
	if strings.Contains(out, "未在配置中声明") {
		t.Fatalf("端口清单不可用时不得判「未知接口」: %q", out)
	}
	if !strings.Contains(out, "sw-beta") {
		t.Fatalf("端口清单不可用时仍应按邻居表作答: %q", out)
	}
}

// TestShowLldpInvalidFormsError：多余/缺失 token 一律报错并给出可用写法
// （决策 #153 的口径：未知或多余 token **不得**静默回落）。
// `show protocols lldp neighbors` 一侧树里没有 `interface` 参数（过滤只声明在等价写法
// `show lldp neighbors` 上），故那里的多余 token 也必须报错并指向等价写法。
func TestShowLldpInvalidFormsError(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setNetRuntime(nil, nil, &lldpStub{rows: lldpStubRows()}, nil, nil)
	x.setPorts(fakePorts{vpp: []string{"ens192", "ens224"}})

	for _, cmd := range []string{
		"show lldp bogus",                                // 未知子命令（回归保护）
		"show lldp neighbors interface",                  // 缺 <ifname>
		"show lldp neighbors interface ens192 extra",     // 多余 token
		"show protocols lldp neighbors interface ens192", // 树里没有该参数（过滤只声明在等价写法上）
	} {
		cmd := cmd
		t.Run(cmd, func(t *testing.T) {
			out := x.Execute("admin", aaa.ClassSuperUser, "ssh", cmd).Output
			if !strings.Contains(out, "无效命令") {
				t.Fatalf("应报「无效命令」并给可用写法: %q", out)
			}
			if strings.Contains(out, "sw-alpha") || strings.Contains(out, "sw-beta") {
				t.Fatalf("报错时不得回全量邻居表: %q", out)
			}
		})
	}
	// 指向等价写法：按接口过滤只有一种写法（`show lldp neighbors interface <ifname>`）
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show protocols lldp neighbors interface ens192").Output
	if !strings.Contains(out, "show lldp neighbors interface <ifname>") {
		t.Fatalf("应指向等价写法，便于直接照敲: %q", out)
	}
}

// TestShowLldpNeighborsRuntimeErrorPropagates：邻居来源报错时如实回错（不吞成空表）。
func TestShowLldpNeighborsRuntimeErrorPropagates(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setNetRuntime(nil, nil, &lldpStub{err: errors.New("VPP 未接入")}, nil, nil)
	x.setPorts(fakePorts{vpp: []string{"ens192"}})

	for _, cmd := range []string{"show lldp neighbors", "show lldp neighbors interface ens192"} {
		out := x.Execute("admin", aaa.ClassSuperUser, "ssh", cmd).Output
		if !strings.HasPrefix(out, "%") || !strings.Contains(out, "VPP 未接入") {
			t.Fatalf("%s 应如实回错: %q", cmd, out)
		}
	}
}

// ---------- ② show configuration permissions <class> ----------

// TestShowConfigurationPermissionsNotImplemented：`permissions <class>` 的「按 class 视角」
// 没有权威语义（契约 §3 对照表未定义、产品也没有按 class 渲染配置的机制），
// 故三档 class **一律明说未实现**——不得静默渲染 committed 正文冒充「class 视角」。
func TestShowConfigurationPermissionsNotImplemented(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "set system hostname perm-node", "commit", "exit",
	)
	if body := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration").Output; !strings.Contains(body, "perm-node") {
		t.Fatalf("前置：committed 应含 perm-node: %q", body)
	}

	for _, class := range []string{aaa.ClassSuperUser, aaa.ClassOperator, aaa.ClassReadOnly} {
		class := class
		t.Run(class, func(t *testing.T) {
			out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration permissions "+class).Output
			if !strings.Contains(out, "未实现") {
				t.Fatalf("应明确提示未实现: %q", out)
			}
			if !strings.HasPrefix(out, "%") {
				t.Fatalf("未实现应带 %% 前缀（与既有提示同族）: %q", out)
			}
			if !strings.Contains(out, class) {
				t.Fatalf("应回显所问的 class（证明参数没被丢）: %q", out)
			}
			if strings.Contains(out, "perm-node") {
				t.Fatalf("不得静默回 committed 配置正文: %q", out)
			}
			if strings.Contains(out, "无效命令") {
				t.Fatalf("命令形式是合法的（只是未实现），不该判「无效命令」: %q", out)
			}
		})
	}

	// 补全菜单（`?`）与执行器同源：描述里也必须如实标注未实现——
	// 写着「按 class 视角显示」而执行器回「暂未实现」，同样是树 ⇄ 执行器不同源。
	found := false
	for _, cand := range schema.Candidates(schema.OperRoot(), []string{"show", "configuration"}, "", nil) {
		if cand.Token != "permissions" {
			continue
		}
		found = true
		if !strings.Contains(cand.Desc, "未实现") {
			t.Errorf("`show configuration ?` 的 permissions 描述应标注未实现，实得 %q", cand.Desc)
		}
	}
	if !found {
		t.Fatalf("`show configuration ?` 候选里应有 permissions")
	}
}

// TestShowConfigurationPermissionsMissingClassSyntax：省略 <class> 是语法错误
// （树里 `permissions` 的参数是必填的 `P("<class>")`），且同样不得回配置正文。
func TestShowConfigurationPermissionsMissingClassSyntax(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "set system hostname perm-node", "commit", "exit",
	)
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration permissions").Output
	if !strings.Contains(out, "语法") {
		t.Fatalf("缺 <class> 应报语法并给用法: %q", out)
	}
	if strings.Contains(out, "perm-node") {
		t.Fatalf("不得回配置正文: %q", out)
	}
	// 多余 token 仍按决策 #153 的「无效命令」口径（回归保护）
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration permissions super-user bogus").Output
	if !strings.Contains(out, "无效命令") || strings.Contains(out, "perm-node") {
		t.Fatalf("多余 token 应报「无效命令」且不回配置正文: %q", out)
	}
}
