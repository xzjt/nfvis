package api

// 决策 #345：数据面 DNS 代理（自研域内转发器）的语句 → 模型 → 提交 → 读视图（CLI/REST 同源）
// 与 display set 往返。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/schema"
)

// TestCLIDNSProxyStatementAndView：`set/delete system dns proxy server …` 落 vpp.dns_proxy_servers、
// `set/delete virtual-switches <vs> dns proxy server …` 落 vs.dns_proxy_servers；CLI 读视图与
// display set 反推同源。
func TestCLIDNSProxyStatementAndView(t *testing.T) {
	x, engine := newCLIKit(t)

	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set system dns proxy server 8.8.8.8 secondary 8.8.4.4", // 全局（主 + 备用一次写两条）
		"set virtual-switches vs-dns type l2",
		"set virtual-switches vs-dns dns proxy server 10.0.0.53", // 按域上游
	)
	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读取 candidate: %v", err)
	}
	if cfg.Vpp == nil || len(cfg.Vpp.DNSProxyServers) != 2 ||
		cfg.Vpp.DNSProxyServers[0] != "8.8.8.8" || cfg.Vpp.DNSProxyServers[1] != "8.8.4.4" {
		t.Fatalf("全局上游应写入 vpp.dns_proxy_servers: %+v", cfg.Vpp)
	}
	if len(cfg.VirtualSwitches) != 1 || len(cfg.VirtualSwitches[0].DNSProxyServers) != 1 ||
		cfg.VirtualSwitches[0].DNSProxyServers[0] != "10.0.0.53" {
		t.Fatalf("按域上游应写入 virtual-switches[vs-dns].dns_proxy_servers: %+v", cfg.VirtualSwitches)
	}
	if cfg.System != nil && len(cfg.System.DNSServers) != 0 {
		t.Fatalf("数据面代理不得动宿主解析器 dns_servers: %+v", cfg.System.DNSServers)
	}

	run(t, x, "admin", aaaClassSU, "ssh", "commit", "exit")

	out := x.Execute("admin", aaaClassSU, "ssh", "show dns proxy").Output
	if !strings.Contains(out, "启用") || !strings.Contains(out, "8.8.8.8") ||
		!strings.Contains(out, "8.8.4.4") || !strings.Contains(out, "vs-dns") || !strings.Contains(out, "10.0.0.53") {
		t.Fatalf("show dns proxy 应显示启用态 + 全局与按域上游:\n%s", out)
	}

	res := x.Execute("admin", aaaClassSU, "ssh", "show configuration | display set")
	for _, want := range []string{
		"set system dns proxy server 8.8.8.8",
		"set system dns proxy server 8.8.4.4",
		"set virtual-switches vs-dns dns proxy server 10.0.0.53",
	} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("display set 应反推出 %q:\n%s", want, res.Output)
		}
	}

	// delete（按值移除一条全局）
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure", "delete system dns proxy server 8.8.4.4", "commit", "exit")
	cfg, _ = engine.Committed()
	if len(cfg.Vpp.DNSProxyServers) != 1 || cfg.Vpp.DNSProxyServers[0] != "8.8.8.8" {
		t.Fatalf("delete 应按值移除备用上游: %+v", cfg.Vpp.DNSProxyServers)
	}

	// delete（不带取值即清空本域，回落全局）
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure", "delete virtual-switches vs-dns dns proxy server", "commit", "exit")
	cfg, _ = engine.Committed()
	for _, vs := range cfg.VirtualSwitches {
		if len(vs.DNSProxyServers) != 0 {
			t.Fatalf("清空本域后 vs 不应再有按域上游: %+v", vs.DNSProxyServers)
		}
	}

	// delete（不带取值即清空全部全局）⇒ 全空 ⇒ 未配置
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure", "delete system dns proxy server", "commit", "exit")
	cfg, _ = engine.Committed()
	if cfg.Vpp != nil && len(cfg.Vpp.DNSProxyServers) != 0 {
		t.Fatalf("清空应移除全部全局上游: %+v", cfg.Vpp.DNSProxyServers)
	}
	out = x.Execute("admin", aaaClassSU, "ssh", "show dns proxy").Output
	if !strings.Contains(out, "未配置") {
		t.Fatalf("全空后应显示未配置:\n%s", out)
	}
}

// TestDNSProxyInvalidAddressRejected：非法上游在提交校验被拒（不静默下发）。
func TestDNSProxyInvalidAddressRejected(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh", "configure", "set system dns proxy server not-an-ip")
	res := x.Execute("admin", aaaClassSU, "ssh", "commit")
	if !strings.Contains(res.Output, "校验失败") || !strings.Contains(res.Output, "有效 IP") {
		t.Fatalf("非法上游应提交失败并说明:\n%s", res.Output)
	}
}

// TestDNSProxyCandidates：候选与语句树同源——`set system dns proxy ?` / `set virtual-switches x dns proxy ?`。
func TestDNSProxyCandidates(t *testing.T) {
	node, _, err := schema.Match(schema.ConfigPathTree(), []string{"system", "dns", "proxy"})
	if err != nil {
		t.Fatalf("system dns proxy 应在语句树里可解析到: %v", err)
	}
	if kids := childNames(node); len(kids) != 1 || kids[0] != "server" {
		t.Fatalf("system dns proxy 下应恰有 server 子节点: %v", kids)
	}
	vsNode, _, err := schema.Match(schema.ConfigPathTree(), []string{"virtual-switches", "vs-x", "dns", "proxy"})
	if err != nil {
		t.Fatalf("virtual-switches dns proxy 应可解析到: %v", err)
	}
	if kids := childNames(vsNode); len(kids) != 1 || kids[0] != "server" {
		t.Fatalf("virtual-switches dns proxy 下应恰有 server 子节点: %v", kids)
	}
}

func childNames(n *schema.Node) []string {
	var out []string
	for _, c := range n.Children {
		out = append(out, c.Name)
	}
	return out
}

// TestRESTDNSProxyView：GET /dns/proxy 与 CLI 同源（enabled 由声明推导，servers/switches 恒为数组）。
func TestRESTDNSProxyView(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// 初始：未配置 → enabled=false、servers=[]、switches=[]（不发 null）
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/dns/proxy", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /dns/proxy: %d %s", status, body)
	}
	var view struct {
		Enabled  bool `json:"enabled"`
		Servers  []string
		Switches []struct {
			Name    string
			Servers []string
		}
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("响应非对象: %v %s", err, body)
	}
	if view.Enabled || view.Servers == nil || len(view.Servers) != 0 {
		t.Fatalf("未配置时 enabled=false、servers 应为空数组: %s", body)
	}
	if view.Switches == nil || len(view.Switches) != 0 {
		t.Fatalf("未配置时 switches 应为空数组: %s", body)
	}

	// 写入全局 + 一台交换机的按域上游（走候选整文档 PUT + 自动提交）
	status, _, body = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		map[string]any{
			"system": map[string]any{"login": map[string]any{"users": []map[string]any{superUserDoc()}}},
			"vpp":    map[string]any{"dns_proxy_servers": []string{"8.8.8.8"}},
			"virtual_switches": []map[string]any{
				{"name": "vs-dns", "type": "l2", "dns_proxy_servers": []string{"10.0.0.53"}},
			},
		},
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("写入上游: %d %s", status, body)
	}

	status, _, body = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/dns/proxy", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("二次 GET /dns/proxy: %d %s", status, body)
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("响应非对象: %v %s", err, body)
	}
	if !view.Enabled || len(view.Servers) != 1 || view.Servers[0] != "8.8.8.8" {
		t.Fatalf("配置全局上游后 enabled=true、servers=[8.8.8.8]: %s", body)
	}
	if len(view.Switches) != 1 || view.Switches[0].Name != "vs-dns" ||
		len(view.Switches[0].Servers) != 1 || view.Switches[0].Servers[0] != "10.0.0.53" {
		t.Fatalf("按域上游应在 switches 里给出: %s", body)
	}
}
