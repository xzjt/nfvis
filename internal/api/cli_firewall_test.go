package api

// 决策 #388：管理面主机防火墙的语句映射（别名）、CLI 读视图渲染与 REST 读视图。

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
	ksys "github.com/xzjt/nfvis/internal/system"
)

// 语句 → 模型：default-policy、规则（任意键序）+ 三种删除形态。
func TestFirewallStatementMapping(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set system firewall default-policy drop",
		"set system firewall rule 100 action accept source 192.0.2.0/24 protocol tcp port 9999",
		// 键值对任意顺序（action 可不在首位）
		"set system firewall rule 200 source 10.0.0.1 protocol icmp action drop",
	)
	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	fw := cfg.FirewallOf()
	if fw == nil || fw.DefaultPolicy != "drop" || len(fw.Rules) != 2 {
		t.Fatalf("防火墙落点异常: %+v", fw)
	}
	r100, r200 := fw.Rules[0], fw.Rules[1]
	if r100.Seq != 100 || r100.Action != "accept" || r100.Source != "192.0.2.0/24" ||
		r100.Protocol != "tcp" || r100.Port != 9999 {
		t.Fatalf("规则 100 落点异常: %+v", r100)
	}
	if r200.Seq != 200 || r200.Action != "drop" || r200.Source != "10.0.0.1" || r200.Protocol != "icmp" {
		t.Fatalf("规则 200 落点异常: %+v", r200)
	}
	// 逐叶子删除：只清 source，其余保留
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "delete system firewall rule 100 source")
	cfg, _, _ = engine.Candidate()
	if got := cfg.FirewallOf().Rules[0]; got.Source != "" || got.Action != "accept" || got.Port != 9999 {
		t.Fatalf("逐叶子删除应只清该叶子: %+v", got)
	}
	// 裸 delete＝整条
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "delete system firewall rule 100")
	cfg, _, _ = engine.Candidate()
	if len(cfg.FirewallOf().Rules) != 1 || cfg.FirewallOf().Rules[0].Seq != 200 {
		t.Fatalf("裸 delete 应删整条: %+v", cfg.FirewallOf().Rules)
	}
	// 默认策略回落缺省（删空后不留空壳——display set 回放依赖这一点）
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "delete system firewall default-policy")
	cfg, _, _ = engine.Candidate()
	if cfg.FirewallOf() == nil || cfg.FirewallOf().DefaultPolicy != "" {
		t.Fatalf("默认策略应回落缺省: %+v", cfg.FirewallOf())
	}
}

// 语句层的即时拒绝（比提交校验更贴语句的文案）：至少一条匹配条件 / port 与协议组合 /
// 缺 action / 非法取值。
func TestFirewallStatementRejects(t *testing.T) {
	cases := []struct{ stmt, want string }{
		{"set system firewall rule 300 action accept", "至少给一条匹配条件"},
		{"set system firewall rule 300 action accept protocol any", "至少给一条匹配条件"},
		{"set system firewall rule 300 action accept source 10.0.0.0/8 protocol icmp port 22", "port 仅 tcp/udp"},
		{"set system firewall rule 300 action accept source 10.0.0.0/8 port 22", "port 仅 tcp/udp"},
		{"set system firewall rule 300 source 10.0.0.0/8", "缺少 action"},
		{"set system firewall rule 300 action permit source 10.0.0.0/8", "action 必须为 accept|drop"},
		{"set system firewall rule 300 action accept protocol gre", "protocol 必须为 tcp|udp|icmp|any"},
		{"set system firewall rule 300 action accept source 10.0.0.0/8 port 70000 protocol tcp", "端口超出 1-65535"},
		{"set system firewall rule 300 action accept source 10.0.0.0/8 nosuch 1", "未知 system firewall rule 参数"},
		{"set system firewall default-policy deny", "默认策略必须为 accept|drop"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.stmt, func(t *testing.T) {
			x, _ := newCLIKit(t)
			run(t, x, "admin", aaa.ClassSuperUser, "ssh", "configure")
			res := x.Execute("admin", aaa.ClassSuperUser, "ssh", c.stmt)
			if !strings.Contains(res.Output, "%%") || !strings.Contains(res.Output, c.want) {
				t.Fatalf("应报 %q，实际: %s", c.want, res.Output)
			}
		})
	}
}

// CLI 读视图：已配置 + 计数可读；未配置的如实文案；未收敛原因如实呈现。
func TestFirewallShowRender(t *testing.T) {
	x, _ := newCLIKit(t)
	// 已配置、已收敛、计数逐规则可读
	x.setFirewall(func() map[string]any {
		return map[string]any{
			"enabled": true, "default_policy": "drop", "mgmt_interface": "ens160", "applied": true,
			"rules": []any{map[string]any{"seq": 100, "action": "accept", "source": "192.168.1.0/24",
				"protocol": "tcp", "port": 22, "packets": 123, "bytes": 4567}},
			"reserved":      ksys.FirewallReserved,
			"counters_note": ksys.FirewallCountersNote,
		}
	})
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system firewall").Output
	for _, want := range []string{"已配置", "drop", "ens160", "已收敛", "192.168.1.0/24", "123 / 4567", "计数由数据面"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show system firewall 输出缺 %q:\n%s", want, out)
		}
	}
	// 结构化输出（display json 用）与视图同源
	if m, ok := x.structured.(map[string]any); !ok || m["applied"] != true {
		t.Fatalf("structured 应为读视图: %#v", x.structured)
	}

	// 未配置：如实报「未配置」+ 前置提示（不编造规则/计数）
	x.setFirewall(func() map[string]any {
		return map[string]any{"enabled": false, "default_policy": "accept", "applied": true,
			"rules": []any{}, "reserved": ksys.FirewallReserved, "counters_note": ksys.FirewallCountersNote}
	})
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system firewall").Output
	if !strings.Contains(out, "未配置") || !strings.Contains(out, "set system management interface") {
		t.Fatalf("未配置应如实报 + 给前置提示:\n%s", out)
	}

	// 未收敛：原因必须在输出里（不把说不清说成已收敛）
	x.setFirewall(func() map[string]any {
		return map[string]any{"enabled": true, "default_policy": "accept", "applied": false,
			"error": "表不存在（配置已启用但数据面没有 inet nfvis-firewall 表）",
			"rules": []any{}, "reserved": ksys.FirewallReserved, "counters_note": ksys.FirewallCountersNote}
	})
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system firewall").Output
	if !strings.Contains(out, "未收敛") || !strings.Contains(out, "表不存在") {
		t.Fatalf("未收敛原因应如实呈现:\n%s", out)
	}
}

// fakeFirewallRuntime 固定读数的数据面文案（REST 测试用）。
type fakeFirewallRuntime struct {
	state ksys.FirewallState
	seen  model.Config
}

func (f *fakeFirewallRuntime) Read(ctx context.Context, cfg model.Config) ksys.FirewallState {
	f.seen = cfg
	return f.state
}

// REST 读视图：配置面字段 + 数据面读数（逐规则计数）同源；未接入时如实报 error。
func TestFirewallRESTView(t *testing.T) {
	rt := &fakeFirewallRuntime{state: ksys.FirewallState{
		Applied:  true,
		Counters: map[int]ksys.FirewallCounter{100: {Packets: 3, Bytes: 180}},
	}}
	ts := newTestServerOpts(t, Options{Firewall: rt})
	token := loginAdmin(t, ts)

	// 种一份带防火墙的 committed 配置：直提写（Auto-Commit）会被防火墙自锁守卫拒绝，
	// 故走「写候选 → confirmed 提交 → 确认」三步（与 CLI 的 commit confirmed 同路径）。
	doc := map[string]any{"system": map[string]any{
		"management": map[string]any{"interface": "ens160"},
		"login":      map[string]any{"users": []map[string]any{superUserDoc()}},
		"firewall": map[string]any{"default_policy": "drop", "rules": []map[string]any{
			{"seq": 100, "action": "accept", "source": "192.168.1.0/24", "protocol": "tcp", "port": 22},
		}},
	}}
	if status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, doc, nil); status != http.StatusOK {
		t.Fatalf("写候选: %d %s", status, body)
	}
	if status, _, body := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/configuration/commit", token,
		map[string]any{"confirmed_minutes": 5}, nil); status != http.StatusOK {
		t.Fatalf("confirmed 提交: %d %s", status, body)
	}
	if status, _, body := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/configuration/commit:confirm", token, nil, nil); status != http.StatusOK {
		t.Fatalf("确认: %d %s", status, body)
	}

	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/firewall", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /system/firewall: %d %s", status, body)
	}
	view := responseObject(t, body)
	if view["enabled"] != true || view["default_policy"] != "drop" || view["applied"] != true ||
		view["mgmt_interface"] != "ens160" {
		t.Fatalf("视图字段异常: %v", view)
	}
	if _, ok := view["error"]; ok {
		t.Fatalf("已收敛不应给 error: %v", view)
	}
	rules, _ := view["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("rules 应有 1 条: %v", view["rules"])
	}
	r0, _ := rules[0].(map[string]any)
	if r0["seq"] != float64(100) || r0["action"] != "accept" || r0["packets"] != float64(3) || r0["bytes"] != float64(180) {
		t.Fatalf("规则读数异常: %v", r0)
	}
	if reserved, _ := view["reserved"].([]any); len(reserved) != 3 {
		t.Fatalf("reserved 应恒为保留项三条: %v", view["reserved"])
	}
	if note, _ := view["counters_note"].(string); note == "" {
		t.Fatal("counters_note 应为计数口径说明")
	}
	// 运行时确实拿到了 committed 配置（读视图与配置面同源）
	if rt.seen.FirewallOf() == nil || rt.seen.MgmtInterfaceOf() != "ens160" {
		t.Fatalf("运行时未见 committed 配置: %+v", rt.seen)
	}
}

// 未接入运行态：仍 200，applied=false + 原因（不把「读不出来」答成「没有」）。
func TestFirewallRESTViewNotWired(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/firewall", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("未接入也应 200: %d", status)
	}
	view := responseObject(t, body)
	if view["applied"] != false {
		t.Fatalf("未接入应 applied=false: %v", view)
	}
	if errText, _ := view["error"].(string); !strings.Contains(errText, "未接入") {
		t.Fatalf("应如实说明未接入: %v", view)
	}
}
