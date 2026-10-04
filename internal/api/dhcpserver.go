package api

// DHCP 服务器读视图（决策 #359）：REST 详情/列表的 dhcp_server 对象与租约端点。
//
// 三面同源：本文件的 dhcpServerView 是「DHCP 服务器」块形状的**唯一实现**——
// REST GET /virtual-switches（列表与详情）与 CLI `show virtual-switches <n> detail`
// 都取它（Web 详情页消费 REST，间接同源）。与 openapi VirtualSwitch.dhcp_server 契约逐字段一致。

import (
	"net/http"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// DHCPServerRuntime DHCP 服务器运行态读物（决策 #359；编排器装配注入，nil = 相关端点 503）。
// *network.L2Network 天然满足本接口（装配处直接传入）；单测注入假实现。
type DHCPServerRuntime interface {
	// DHCPServerLeases 租约表（ok=false = 该交换机没有运行中的服务器：未配置/尚未收敛）。
	DHCPServerLeases(swName string) ([]network.DHCPLease, bool)
	// DHCPServerActiveLeases 生效租约数（state=active；ok=false 同上）。
	DHCPServerActiveLeases(swName string) (int, bool)
	// DHCPTapIndexes 产品自持的内置 tap sw_if_index 集合（端口读视图按它过滤——**不用名字匹配**）。
	DHCPTapIndexes() map[uint32]bool
}

// dhcpServerView 「DHCP 服务器」读视图对象（决策 #359；与 openapi VirtualSwitch.dhcp_server
// 同形）：池/租约时长（**生效值**——未配置回落缺省 86400）/DNS（**生效值**——未配置回落
// BVI 网关地址）/域名（未配置缺席）/在租数。仅配置了 pool（启用要件）才存在。
// haveLeases=false 表示运行态不可用（编排器未装配或服务器尚未收敛）：active_leases 缺席
// （不编造 0——「配了但收不到」与「0 个在租」是两个事实）。
func dhcpServerView(vs model.VirtualSwitch, activeLeases int, haveLeases bool) map[string]any {
	if !vs.DHCPServerEnabled() {
		return nil
	}
	bvi, _, _ := vs.GatewayIPv4()
	dns := vs.DhcpServerDNS
	if dns == "" && bvi != nil {
		dns = bvi.String() // 缺省下发 BVI 地址（数据面 DNS 代理 #345 的落点；手册 §8.3 如实说明）
	}
	m := map[string]any{
		"pool_start":         vs.DhcpServerPoolStart,
		"pool_end":           vs.DhcpServerPoolEnd,
		"lease_time_seconds": vs.DHCPServerLeaseSeconds(),
		"dns":                dns,
	}
	if vs.DhcpServerDomainName != "" {
		m["domain_name"] = vs.DhcpServerDomainName
	}
	if haveLeases {
		m["active_leases"] = activeLeases
	}
	return m
}

// handleGetDHCPLeases GET /api/v1/virtual-switches/{name}/dhcp-leases（决策 #359）。
//
// 交换机不存在 ⇒ 404；**未配置 dhcp-server ⇒ 409**（不是空数组——空数组会误导为
// 「配了但没租约」）；已配置但服务器尚未收敛 ⇒ 503（如实给原因，不返回空表）。
// 与 CLI `show virtual-switches <n> dhcp-leases` 同源（同一 provider 读视图）。
func (s *Server) handleGetDHCPLeases(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	found := false
	enabled := false
	for _, vs := range cfg.VirtualSwitches {
		if vs.Name == name {
			found = true
			enabled = vs.DHCPServerEnabled()
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "虚拟交换机 "+name+" 不存在", nil)
		return
	}
	if !enabled {
		writeError(w, http.StatusConflict, "DHCP_SERVER_NOT_CONFIGURED",
			"虚拟交换机 "+name+" 未配置 DHCP 服务器（set virtual-switches "+name+" dhcp-server pool <start> <end> 启用）", nil)
		return
	}
	if s.dhcpSrv == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "DHCP 服务器编排未装配，运行态不可用", nil)
		return
	}
	leases, ok := s.dhcpSrv.DHCPServerLeases(name)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE",
			"DHCP 服务器尚未收敛（数据面未连接或内置 tap 建立中），租约表暂不可用", nil)
		return
	}
	if leases == nil {
		leases = []network.DHCPLease{}
	}
	writeJSON(w, http.StatusOK, leases)
}
