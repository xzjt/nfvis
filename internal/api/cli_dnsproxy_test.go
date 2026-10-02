package api

// 决策 #338：数据面 DNS 代理的语句 → 模型 → 提交 → 读视图（CLI/REST 同源）与 display set 往返。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/schema"
)

// TestCLIDNSProxyStatementAndView：`set/delete system dns proxy server …` 落模型 vpp.dns_proxy_servers，
// CLI `show dns proxy` 与 display set 反推同源。
func TestCLIDNSProxyStatementAndView(t *testing.T) {
	x, engine := newCLIKit(t)

	// set（主 + 备用一次写两条）
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set system dns proxy server 8.8.8.8 secondary 8.8.4.4",
	)
	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读取 candidate: %v", err)
	}
	if cfg.Vpp == nil || len(cfg.Vpp.DNSProxyServers) != 2 ||
		cfg.Vpp.DNSProxyServers[0] != "8.8.8.8" || cfg.Vpp.DNSProxyServers[1] != "8.8.4.4" {
		t.Fatalf("上游应写入 vpp.dns_proxy_servers: %+v", cfg.Vpp)
	}
	if cfg.System != nil && len(cfg.System.DNSServers) != 0 {
		t.Fatalf("数据面代理不得动宿主解析器 dns_servers: %+v", cfg.System.DNSServers)
	}

	// 提交 + 退出配置模式
	run(t, x, "admin", aaaClassSU, "ssh", "commit", "exit")

	// CLI 读视图：启用态 + 两条上游
	out := x.Execute("admin", aaaClassSU, "ssh", "show dns proxy").Output
	if !strings.Contains(out, "启用") || !strings.Contains(out, "8.8.8.8") || !strings.Contains(out, "8.8.4.4") {
		t.Fatalf("show dns proxy 应显示启用与两条上游:\n%s", out)
	}

	// display set 反推：逐条 `set system dns proxy server <ip>`
	res := x.Execute("admin", aaaClassSU, "ssh", "show configuration | display set")
	if !strings.Contains(res.Output, "set system dns proxy server 8.8.8.8") ||
		!strings.Contains(res.Output, "set system dns proxy server 8.8.4.4") {
		t.Fatalf("display set 应反推出数据面 DNS 代理语句:\n%s", res.Output)
	}

	// delete（按值移除一条）
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"delete system dns proxy server 8.8.4.4",
		"commit",
		"exit",
	)
	cfg, _ = engine.Committed()
	if len(cfg.Vpp.DNSProxyServers) != 1 || cfg.Vpp.DNSProxyServers[0] != "8.8.8.8" {
		t.Fatalf("delete 应按值移除备用上游: %+v", cfg.Vpp.DNSProxyServers)
	}

	// delete（不带取值即清空全部）
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"delete system dns proxy server",
		"commit",
		"exit",
	)
	cfg, _ = engine.Committed()
	if cfg.Vpp != nil && len(cfg.Vpp.DNSProxyServers) != 0 {
		t.Fatalf("清空应移除全部上游: %+v", cfg.Vpp.DNSProxyServers)
	}
	out = x.Execute("admin", aaaClassSU, "ssh", "show dns proxy").Output
	if !strings.Contains(out, "未配置") {
		t.Fatalf("清空后应显示未配置:\n%s", out)
	}
}

// TestDNSProxyInvalidAddressRejected：非法上游在提交校验被拒（不静默下发）。
func TestDNSProxyInvalidAddressRejected(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set system dns proxy server not-an-ip",
	)
	res := x.Execute("admin", aaaClassSU, "ssh", "commit")
	if !strings.Contains(res.Output, "校验失败") || !strings.Contains(res.Output, "有效 IP") {
		t.Fatalf("非法上游应提交失败并说明:\n%s", res.Output)
	}
}

// TestDNSProxyCandidates（决策 #338）：候选与语句树同源——`set system dns proxy ?` 能补到 `server`。
func TestDNSProxyCandidates(t *testing.T) {
	node, _, err := schema.Match(schema.ConfigPathTree(), []string{"system", "dns", "proxy"})
	if err != nil {
		t.Fatalf("system dns proxy 应在语句树里可解析到: %v", err)
	}
	var kids []string
	for _, c := range node.Children {
		kids = append(kids, c.Name)
	}
	if len(kids) != 1 || kids[0] != "server" {
		t.Fatalf("system dns proxy 下应恰有 server 子节点: %v", kids)
	}
}

// TestRESTDNSProxyView：GET /dns/proxy 与 CLI 同源（enabled 由列表推导，servers 恒为数组）。
func TestRESTDNSProxyView(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// 初始：未配置 → enabled=false、servers=[]（不发 null）
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/dns/proxy", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /dns/proxy: %d %s", status, body)
	}
	var view struct {
		Enabled bool     `json:"enabled"`
		Servers []string `json:"servers"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("响应非对象: %v %s", err, body)
	}
	if view.Enabled || view.Servers == nil || len(view.Servers) != 0 {
		t.Fatalf("未配置时 enabled=false、servers 应为空数组: %s", body)
	}

	// 写入上游（走候选整文档 PUT + 自动提交）
	status, _, body = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		map[string]any{
			"system": map[string]any{"login": map[string]any{"users": []map[string]any{superUserDoc()}}},
			"vpp":    map[string]any{"dns_proxy_servers": []string{"8.8.8.8"}},
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
		t.Fatalf("配置上游后 enabled=true、servers=[8.8.8.8]: %s", body)
	}
}
