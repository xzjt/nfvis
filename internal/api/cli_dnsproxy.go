package api

// 数据面 DNS 代理读视图（决策 #345，FR-NET-010）。
//
// 三面同源：
//   · CLI  `show dns proxy`
//   · REST `GET /dns/proxy`（响应 {enabled, servers, switches}）
//   · Web  控制台系统页 DNS 小节（只读两行）
//
// 口径：**只读产品自身**的配置声明（VppConfig.DNSProxyServers 全局 + 各 VirtualSwitch.DNSProxyServers
// 按域）——不解析 `vppctl` 文本，也不去回读 VPP 运行态。`enabled` 由「全局或任一所声明交换机非空」
// 推导，与数据面的启用判据同口径（全局或任一交换机非空 ⇒ 注册 punt 并起转发器；全空 ⇒ 注销）。
//
// 边界（如实登记，不编造）：
//   · **纯 UDP 转发**——客户端用 TCP 查 DNS 不生效（punt 只注册 UDP 53；TCP 查询不在覆盖内）；
//   · **不缓存**——每个查询都直接转发上游，无本地缓存；
//   · **不预检上游可达性**——上游由 nfvisd 用宿主网络栈发起，不可达/超时按运行期如实回 **SERVFAIL**
//     并计数（不对配置做可达性预检）；
//   · **启用期间指向产品地址的 UDP/53 由 nfvisd 独占**——nfvisd 不在（崩溃/被停）时这些包被 VPP
//     punt 节点丢弃（该节点 flags=IS_DROP），域内 DNS 中断，直到 nfvisd 回来或停用注销；
//   · **覆盖 IPv4/UDP/53**——punt 注册按地址族，IPv6 的解析查询（指向产品 IPv6 地址）尚未覆盖
//     （v6 的注册与回注路径需真机验证后启用，不先做成假能力）。
//
// 本视图如实呈现配置意图与上述边界；取不到配置就说取不到（不静默回空）。

import (
	"net/http"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// dnsProxySwitchView 一台交换机的按域上游（只列**已声明且真配了上游**的交换机）。
type dnsProxySwitchView struct {
	Name    string   `json:"name"`
	Servers []string `json:"servers"`
}

// dnsProxyViewShape 数据面 DNS 代理读视图（CLI 与 REST 共用，形状一致）。
// servers/switches 恒为数组（空时为空数组，不发 null）。
type dnsProxyViewShape struct {
	Enabled  bool                 `json:"enabled"`
	Servers  []string             `json:"servers"`
	Switches []dnsProxySwitchView `json:"switches"`
}

// dnsProxyView 从配置声明构建读视图（与数据面启用判据同口径）。
func dnsProxyView(cfg model.Config) dnsProxyViewShape {
	view := dnsProxyViewShape{Servers: []string{}, Switches: []dnsProxySwitchView{}}
	if cfg.Vpp != nil {
		view.Servers = append(view.Servers, cfg.Vpp.DNSProxyServers...)
	}
	for _, vs := range cfg.VirtualSwitches {
		if len(vs.DNSProxyServers) == 0 {
			continue // 未配上游的交换机不出现（不编造空条目）
		}
		view.Switches = append(view.Switches, dnsProxySwitchView{
			Name:    vs.Name,
			Servers: append([]string{}, vs.DNSProxyServers...),
		})
	}
	view.Enabled = len(view.Servers) > 0 || len(view.Switches) > 0
	return view
}

// handleGetDNSProxy GET /api/v1/dns/proxy：数据面 DNS 代理的启用态、全局上游与各域覆盖（决策 #345）。
func (s *Server) handleGetDNSProxy(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dnsProxyView(cfg))
}

// renderDNSProxy `show dns proxy`：启用态 + 全局上游 + 各域覆盖（与 GET /dns/proxy 同源）。
func (x *cliExecutor) renderDNSProxy() string {
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	view := dnsProxyView(cfg)

	var b strings.Builder
	if view.Enabled {
		b.WriteString("数据面 DNS 代理: 启用（域内客户端把 resolver 指向网关即可解析；上游经宿主网络栈发起）\n")
	} else {
		b.WriteString("数据面 DNS 代理: 未配置（用 set system dns proxy server <ip> 或 set virtual-switches <vs> dns proxy server <ip> 启用）\n")
	}
	b.WriteString("全局上游:\n")
	writeServerList(&b, view.Servers)
	if len(view.Switches) > 0 {
		b.WriteString("按域上游:\n")
		for _, sw := range view.Switches {
			b.WriteString("  交换机 " + sw.Name + ":\n")
			writeServerList(&b, sw.Servers)
		}
	}
	b.WriteString("说明: 本视图只读产品配置声明。边界：纯 UDP 转发（客户端用 TCP 查 DNS 不生效）；不缓存；\n")
	b.WriteString("      不预检上游可达性（上游不可达/超时运行期回 SERVFAIL 并计数）；启用期间指向产品地址的\n")
	b.WriteString("      UDP/53 由 nfvisd 独占，nfvisd 不在时这些包被 VPP punt 节点丢弃（域内 DNS 中断）；\n")
	b.WriteString("      覆盖 IPv4/UDP/53（IPv6 的解析查询尚未覆盖——punt 注册按地址族）。\n")
	return b.String()
}

// writeServerList 打印一份上游列表（空时如实「（无）」，不省略）。
func writeServerList(b *strings.Builder, servers []string) {
	if len(servers) == 0 {
		b.WriteString("  （无）\n")
		return
	}
	for _, s := range servers {
		b.WriteString("  " + s + "\n")
	}
}
