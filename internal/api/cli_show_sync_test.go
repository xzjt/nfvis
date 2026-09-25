package api

// round80 真机实测暴露的「命令树 ⇄ 执行器不同源」缺陷的守护（决策 #153，FR-CLI-001/002/004）：
//
//  1. **静默误答**：`show configuration <任意多余 token>` 静默返回 committed 配置正文——
//     `show configuration sessions` 本应等价于 `show system configuration sessions`，
//     `show configuration bogus` 本应报错。根因是执行器只识别 compare/history/candidate，
//     其余一律落到「读 committed」。
//  2. **补全漂移（树里没有、执行器能跑）**：`show interfaces <ifname> detail|statistics|sriov`
//     （`physical` 可省）、`show protocols lldp neighbors`、`show configuration compare rollback <n>`
//     执行器都支持，命令树里却没有 → `?`/Tab 补不出来。
//
// 同族（同一处代码里查出的第三例，一并收口）：`show interfaces physical <ifname> <子命令>`
// 把子命令**整段丢掉**（只按单口列表渲染）——契约声明 `physical` 可省即两种写法等价，
// 而带 `physical` 的那侧 `statistics`/`sriov` 打的是同一张表（静默误答）。
//
// 判据一律取**同一事实源对照**：等价写法必须逐字相同，未知子命令必须报错且**不含配置正文**。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/schema"
)

// TestShowConfigurationSessionsSameAsSystemForm：`show configuration sessions` 与
// `show system configuration sessions` 必须逐字相同（同一读物 = `Engine.Sessions`），
// 且**不得**回落到配置正文。
func TestShowConfigurationSessionsSameAsSystemForm(t *testing.T) {
	x, _ := newCLIKit(t)
	// 空态（操作模式）：两侧都应是「（无持锁会话）」
	empty1 := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration sessions").Output
	empty2 := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system configuration sessions").Output
	if empty1 != empty2 {
		t.Fatalf("空态两侧应一致:\n  show configuration sessions: %q\n  show system … sessions: %q", empty1, empty2)
	}

	// 持锁态（操作模式）：锁由另一来源（api）持有，ssh 侧仍在操作模式，两写法必须逐字相同
	if out := x.Execute("admin", aaa.ClassSuperUser, "api", "configure").Output; strings.Contains(out, "%") {
		t.Fatalf("configure 失败: %s", out)
	}
	got := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration sessions").Output
	want := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system configuration sessions").Output
	if got != want {
		t.Fatalf("两侧输出必须逐字相同（同一读物）:\n  show configuration sessions: %q\n  show system … sessions: %q", got, want)
	}
	for _, w := range []string{"Holder", "admin@api"} {
		if !strings.Contains(got, w) {
			t.Fatalf("持锁会话列表应含 %q: %q", w, got)
		}
	}
	if strings.Contains(got, "password-hash") || strings.Contains(got, "system {") {
		t.Fatalf("会话列表不得是配置正文: %q", got)
	}

	// 配置模式下同一写法也要通（`show configuration …` 委托操作模式，与操作模式同源）
	if out := x.Execute("admin", aaa.ClassSuperUser, "api", "exit").Output; strings.Contains(out, "%") {
		t.Fatalf("exit 失败: %s", out) // 配置模式 exit 释放锁
	}
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "configure")
	inCfg := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration sessions").Output
	oper := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system configuration sessions").Output
	_ = oper // 配置模式下 `show system …` 是配置路径渲染（§3：配置模式 show = candidate），此处只断言委托写法
	if !strings.Contains(inCfg, "Holder") || !strings.Contains(inCfg, "admin@ssh") {
		t.Fatalf("配置模式下应委托同一实现（持锁会话表）: %q", inCfg)
	}
	if strings.Contains(inCfg, "password-hash") {
		t.Fatalf("配置模式下也不得回显配置正文: %q", inCfg)
	}
}

// TestShowConfigurationUnknownSubcommandErrors：未知子命令必须报错（列出可用子命令），
// **不得**静默返回 committed 配置正文。
func TestShowConfigurationUnknownSubcommandErrors(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "set system hostname sync-node", "commit", "exit",
	)
	body := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration").Output
	if !strings.Contains(body, "sync-node") {
		t.Fatalf("前置：committed 应含 sync-node: %q", body)
	}

	for _, cmd := range []string{
		"show configuration bogus",
		"show configuration bogus-token",
		"show configuration history bogus",
		"show configuration sessions bogus",
		"show configuration candidate bogus",
		"show configuration compare bogus 1",
		"show configuration compare rollback 1 bogus",
	} {
		cmd := cmd
		t.Run(cmd, func(t *testing.T) {
			out := x.Execute("admin", aaa.ClassSuperUser, "ssh", cmd).Output
			if !strings.Contains(out, "无效命令") && !strings.Contains(out, "语法") {
				t.Fatalf("未知子命令应报错（可操作提示）: %q", out)
			}
			if strings.Contains(out, "可用") && !strings.Contains(out, "sessions") {
				t.Fatalf("提示应列出可用子命令（含 sessions）: %q", out)
			}
			if strings.Contains(out, "sync-node") {
				t.Fatalf("报错时不得回显配置正文: %q", out)
			}
		})
	}

	// 合法写法仍然可用（回归保护）：省略子命令 = 读 committed
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration").Output; !strings.Contains(out, "sync-node") {
		t.Fatalf("show configuration 仍应渲染 committed: %q", out)
	}
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration candidate").Output; strings.Contains(out, "无效命令") {
		t.Fatalf("show configuration candidate 不应被判无效: %q", out)
	}
}

// TestCommandTreeCoversShowEquivalences：新增写法必须真的在命令树里（`?`/Tab 才补得出来），
// 且 class 仍为 R（`show` 族不得因本次改动升权）。
func TestCommandTreeCoversShowEquivalences(t *testing.T) {
	cases := []struct {
		toks []string
		last string // 末节点的规范名
	}{
		{[]string{"show", "interfaces", "ens224", "detail"}, "detail"},
		{[]string{"show", "interfaces", "ens224", "statistics"}, "statistics"},
		{[]string{"show", "interfaces", "ens224", "sriov"}, "sriov"},
		{[]string{"show", "interfaces", "physical", "ens224", "detail"}, "detail"},
		{[]string{"show", "interfaces", "physical", "ens224", "sriov"}, "sriov"},
		{[]string{"show", "protocols", "lldp", "neighbors"}, "neighbors"},
		{[]string{"show", "configuration", "sessions"}, "sessions"},
		{[]string{"show", "configuration", "compare", "rollback", "3"}, "<n>"},
	}
	for _, c := range cases {
		n, depth, err := schema.Match(schema.OperRoot(), c.toks)
		if err != nil {
			t.Errorf("%v 在命令树里解析不到（`?`/Tab 补不出来）：%v", c.toks, err)
			continue
		}
		if depth != len(c.toks) {
			t.Errorf("%v 只消耗了 %d 个 token", c.toks, depth)
		}
		if n.Name != c.last {
			t.Errorf("%v 末节点应为 %q，实得 %q", c.toks, c.last, n.Name)
		}
		if got := n.RequiredClass().String(); got != "R" {
			t.Errorf("%v 的 class 应为 R（show 族只读），实得 %s", c.toks, got)
		}
	}

	// `?` 候选：新关键字/参数必须列得出来（与执行器同源的那一层）
	for _, c := range []struct {
		toks    []string
		partial string
		want    []string
	}{
		{[]string{"show"}, "", []string{"protocols"}},
		{[]string{"show", "configuration"}, "", []string{"sessions", "compare"}},
		{[]string{"show", "interfaces"}, "", []string{"<ifname>"}},
		{[]string{"show", "interfaces", "ens224"}, "", []string{"detail", "statistics", "sriov"}},
		{[]string{"show", "protocols", "lldp"}, "", []string{"neighbors"}},
	} {
		got := map[string]bool{}
		for _, cand := range schema.Candidates(schema.OperRoot(), c.toks, c.partial, nil) {
			got[cand.Token] = true
		}
		for _, w := range c.want {
			if !got[w] {
				t.Errorf("`%s ?` 候选应含 %q，实得 %v", strings.Join(c.toks, " "), w, got)
			}
		}
	}
}

// TestShowInterfacesPhysicalSubNotDropped：契约声明 `physical` 可省（两种写法等价），
// 带 `physical` 的一侧**不得**把子命令丢掉（此前 statistics/sriov 打的是同一张单口列表）。
func TestShowInterfacesPhysicalSubNotDropped(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set interfaces ens2f0 mtu 9000",
		"set interfaces ens2f0 description to-TOR",
		"commit", "exit",
	)
	for _, sub := range []string{"detail", "statistics", "sriov"} {
		with := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces physical ens2f0 "+sub).Output
		without := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces ens2f0 "+sub).Output
		if with != without {
			t.Errorf("`physical` 可省：两侧应逐字相同\n  带 physical: %q\n  省略 physical: %q", with, without)
		}
	}
	// 子命令真的生效了（而不是回落到单口列表）：统计走运行态分支、sriov 走 VF 分支
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces physical ens2f0 statistics").Output; !strings.Contains(out, "统计运行态不可用") {
		t.Fatalf("statistics 应走计数分支（此前被丢成单口列表）: %q", out)
	}
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces physical ens2f0 sriov").Output; !strings.Contains(out, "SR-IOV") {
		t.Fatalf("sriov 应走 VF 分支（此前被丢成单口列表）: %q", out)
	}
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces physical ens2f0 bogus").Output; !strings.Contains(out, "无效命令") {
		t.Fatalf("未知子命令应报错: %q", out)
	}
}
