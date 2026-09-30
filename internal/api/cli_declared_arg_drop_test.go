package api

// 本批收口决策 #153 里**登记但未修**的两条「声明了却被静默丢弃」缺陷（round80 真机测试的延伸）。
// 两条同属「静默误答」：契约声明了参数，执行器却把参数整段丢掉、按另一种语义作答——
// 操作者以为自己拿到了「所问的那份事实」。
//
//	① `show lldp neighbors interface <ifname>`：过滤参数被 `execShowLldp` 丢掉（只透传
//	   `lldp neighbors`），问「某个口的邻居」拿到的是全量邻居表。
//	② `show configuration permissions <class>`：class 参数被忽略，直接渲染 committed 配置正文
//	   （原处置为「明说未实现」，决策 #153；后由决策 #304 落地为**生效权限视图**，本文件的
//	   ② 组用例随之改为核对该视图的判定与权限边界）。
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

// ---------- ② show configuration permissions <class>（决策 #304：生效权限视图） ----------

// TestShowConfigurationPermissionsView：`permissions <class>` 给出该 class 在命令树上的
// 逐路径判定（默认列允许路径 + 汇总；detail 附判定依据），**不得回配置正文**。
// 补全菜单的描述与执行器同源（不再写「未实现」）。
func TestShowConfigurationPermissionsView(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "set system hostname perm-node", "commit", "exit",
	)
	if body := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration").Output; !strings.Contains(body, "perm-node") {
		t.Fatalf("前置：committed 应含 perm-node: %q", body)
	}

	// read-only：show 族允许、configure 拒绝
	ro := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration permissions read-only").Output
	if strings.HasPrefix(ro, "%%") {
		t.Fatalf("super-user 查 read-only 不应报错: %q", ro)
	}
	if !strings.Contains(ro, "show version") {
		t.Fatalf("read-only 的默认视图应列出允许路径 show version: %q", ro)
	}
	if !strings.Contains(ro, "允许") || !strings.Contains(ro, "拒绝") {
		t.Fatalf("应有允许/拒绝汇总: %q", ro)
	}
	if strings.Contains(ro, "perm-node") {
		t.Fatalf("生效权限视图不得回配置正文: %q", ro)
	}

	// detail：逐路径带判定依据
	roD := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration permissions read-only detail").Output
	if !strings.Contains(roD, "[允许] show version（预置等级满足）") {
		t.Fatalf("read-only detail 应含「[允许] show version（预置等级满足）」: %q", roD)
	}
	if !strings.Contains(roD, "[拒绝] configure（预置等级不足）") {
		t.Fatalf("read-only detail 应含「[拒绝] configure（预置等级不足）」: %q", roD)
	}
	suD := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration permissions super-user detail").Output
	if !strings.Contains(suD, "[允许] configure（预置等级满足）") {
		t.Fatalf("super-user detail 应允许 configure: %q", suD)
	}

	// 补全菜单描述与执行器同源：不再写「未实现」
	for _, cand := range schema.Candidates(schema.OperRoot(), []string{"show", "configuration"}, "", nil) {
		if cand.Token == "permissions" && strings.Contains(cand.Desc, "未实现") {
			t.Errorf("permissions 候选描述不应再写「未实现」: %q", cand.Desc)
		}
	}
}

// TestShowConfigurationPermissionsBoundary：权限边界——非 super-user 只能查自己所属 class
// （不泄露他人规则），未知 class 明确报错；多余 token 按无效命令报错。
func TestShowConfigurationPermissionsBoundary(t *testing.T) {
	x, _ := newCLIKit(t)
	// read-only 查自己：可以
	if out := x.Execute("admin", aaa.ClassReadOnly, "ssh", "show configuration permissions read-only").Output; strings.HasPrefix(out, "%%") {
		t.Fatalf("read-only 查自己应可: %q", out)
	}
	// read-only 查他人：拒绝，且不泄露他人规则（不回允许路径）
	out := x.Execute("admin", aaa.ClassReadOnly, "ssh", "show configuration permissions operator").Output
	if !strings.HasPrefix(out, "%%") || !strings.Contains(out, "无权查看") {
		t.Fatalf("read-only 查他人应拒绝: %q", out)
	}
	if strings.Contains(out, "show version") {
		t.Fatalf("拒绝时不得泄露他人 class 的规则: %q", out)
	}
	// operator 查自己：可以
	if out := x.Execute("admin", aaa.ClassOperator, "ssh", "show configuration permissions operator").Output; strings.HasPrefix(out, "%%") {
		t.Fatalf("operator 查自己应可: %q", out)
	}
	// 未知 class（super-user）：明确报错
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration permissions ghost").Output
	if !strings.HasPrefix(out, "%%") || !strings.Contains(out, "未知 class") {
		t.Fatalf("未知 class 应明确报错: %q", out)
	}
	// 多余 token：无效命令
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration permissions read-only bogus").Output
	if !strings.Contains(out, "无效命令") {
		t.Fatalf("多余 token 应报无效命令: %q", out)
	}
	// 省略 <class>：语法提示
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration permissions").Output
	if !strings.Contains(out, "语法") {
		t.Fatalf("缺 <class> 应报语法: %q", out)
	}
}

// TestShowConfigurationPermissionsCustomClass：自定义 class 的 allow/deny（deny 优先）在视图中
// 如实体现，且 `| display set` 输出等价 set 语句（经既有反推机制单一实现）。
func TestShowConfigurationPermissionsCustomClass(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		`set system login class ops allow "show"`,
		`set system login class ops deny "show configuration"`,
		"commit", "exit",
	)

	// 默认：allow 命中放行、deny 命中拒绝（deny 优先）；未列出的族默认拒绝
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration permissions ops").Output
	if strings.HasPrefix(out, "%%") {
		t.Fatalf("查自定义 class 不应报错: %q", out)
	}
	if !strings.Contains(out, "自定义") || !strings.Contains(out, "show version") {
		t.Fatalf("自定义 class 默认视图应列出 allow 命中路径: %q", out)
	}
	// detail：deny 优先于 allow
	d := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration permissions ops detail").Output
	if !strings.Contains(d, "[拒绝] show configuration（deny 前缀命中）") {
		t.Fatalf("deny 应优先拒绝 show configuration: %q", d)
	}
	if !strings.Contains(d, "[允许] show version（allow 前缀命中）") {
		t.Fatalf("allow 应放行 show version: %q", d)
	}
	if !strings.Contains(d, "[拒绝] request（默认拒绝）") {
		t.Fatalf("未列出的路径应默认拒绝: %q", d)
	}

	// display set：等价 set 语句（自定义 class 有 allow/deny 路径表）
	ds := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration permissions ops | display set").Output
	if !strings.Contains(ds, "set system login class ops allow show") ||
		!strings.Contains(ds, `set system login class ops deny`) {
		t.Fatalf("自定义 class 的 display set 应输出等价 set 语句: %q", ds)
	}

	// 预置 class 的 display set：如实说明由等级判定、不编造语句
	pd := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration permissions read-only | display set").Output
	if !strings.Contains(pd, "预置 class read-only") || strings.Contains(pd, "set system login class") {
		t.Fatalf("预置 class 的 display set 应给基等级说明而非编造 set 语句: %q", pd)
	}
}
