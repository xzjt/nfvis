package api

// 数据面 DNS 代理读视图（决策 #338，FR-NET-010）。
//
// 三面同源：
//   · CLI  `show dns proxy`
//   · REST `GET /dns/proxy`（响应 {enabled, servers}）
//   · Web  控制台系统页 DNS 小节（只读两行）
//
// 口径：**只读产品自身**的配置声明（Vpp.DNSProxyServers，非空即启用）——不解析 `vppctl`
// 文本，也不去回读 VPP 运行态。产品 `show dns proxy` 与 `vppctl show dns servers` 的一致
// 由语义套件（真机）对照；本视图如实呈现配置意图，取不到就说取不到。

import (
	"net/http"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// dnsProxyView 数据面 DNS 代理读视图（CLI 与 REST 共用，形状一致）。enabled 由上游列表
// 非空推导（与模型「非空即启用」同口径），servers 恒为数组（空时为空数组，不发 null）。
func dnsProxyView(cfg model.Config) map[string]any {
	var servers []string
	if cfg.Vpp != nil {
		servers = append(servers, cfg.Vpp.DNSProxyServers...)
	}
	if servers == nil {
		servers = []string{}
	}
	return map[string]any{
		"enabled": len(servers) > 0,
		"servers": servers,
	}
}

// handleGetDNSProxy GET /api/v1/dns/proxy：数据面 DNS 代理的启用态与上游列表（决策 #338）。
func (s *Server) handleGetDNSProxy(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dnsProxyView(cfg))
}

// renderDNSProxy `show dns proxy`：上游列表 + 启用态（与 GET /dns/proxy 同源）。
func (x *cliExecutor) renderDNSProxy() string {
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	view := dnsProxyView(cfg)
	servers, _ := view["servers"].([]string)
	enabled, _ := view["enabled"].(bool)

	var b strings.Builder
	if enabled {
		b.WriteString("数据面 DNS 代理: 启用（域内客户端把 resolver 指向网关即可解析；上游须在 VPP FIB 内可达）\n")
	} else {
		b.WriteString("数据面 DNS 代理: 未配置（用 set system dns proxy server <ip> 启用）\n")
	}
	b.WriteString("上游服务器:\n")
	if len(servers) == 0 {
		b.WriteString("  （无）\n")
	}
	for _, s := range servers {
		b.WriteString("  " + s + "\n")
	}
	b.WriteString("说明: 本视图只读产品配置声明；与数据面实况的对照见 vppctl show dns servers\n")
	return b.String()
}
