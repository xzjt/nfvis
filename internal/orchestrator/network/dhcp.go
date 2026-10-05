package network

// DHCP relay（决策 #335）：L2 交换机的 BVI 网关域通过 VPP dhcp proxy 把域内 DHCP 广播
// 中继到 server。proxy 按 rx-VRF 生效——rx/server VRF 都是网关转发域（gateway.vrf 或专属
// vr-<交换机名>），源地址自动取 BVI 的 IPv4 网关地址；表 id 与源地址与 ApplyGateway 用
// **同一来源**（GatewayVRFName/TableID + gateway.addresses），不引入第二份推导。
//
// 生命周期口径（与 QoS/SPAN 同族）：
//   - 随交换机事务收敛（网关落地之后）：SyncRelay 按声明与登记比较，新配/改 server/换网关域
//     下发 IsAdd=true，清 relay 按**登记值**发 IsAdd=false（幂等，无登记即无事可做）；
//   - 删交换机连带撤 proxy（DeleteRelay，先撤 proxy 再拆 BVI/表）；
//   - **恢复重放必须含 relay**：VPP 重启后 proxy 运行态消失，不重放即静默丢中继——
//     recovery.go 的交换机段对声明了 relay 的交换机重发同一条 proxy 消息。
//
// 范围（决策 #335 如实登记）：只做 relay（DHCP server 延后另立决策）；server 须在该转发域内
// 可达，跨 VRF 的 server 不在 v1；不做 option 82。

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/xzjt/nfvis/internal/model"
)

// DhcpClient VPP dhcp proxy binary API 的最小能力集（govpp 适配/单测假实现）。
type DhcpClient interface {
	// ProxySet 下发 dhcp_proxy_config。is_add=false 按 (rx_vrf, server) 撤销既有 proxy。
	ProxySet(rxVrfID, serverVrfID uint32, isAdd bool, server, src string) error
	// ProxyDump 列出 VPP 里**实际**的 DHCP proxy 条目（对账用；失败即返回错误，调用方跳过本轮）。
	ProxyDump() ([]ProxyEntry, error)
	Close()
}

// ProxyEntry 数据面实际的 DHCP proxy 条目（dhcp_proxy_dump 的一条，按 rx-VRF 聚合）。
type ProxyEntry struct {
	RxVrfID uint32
	Src     string // dhcp_src_address
	Servers []ProxyServer
}

// ProxyServer proxy 条目里的一个 server 条目。
type ProxyServer struct {
	VrfID  uint32
	Server string
}

// dhcpRelay 一台交换机已下发的 proxy 登记（撤销时按原值发 IsAdd=false）。
type dhcpRelay struct {
	tableID uint32
	server  string
	src     string
}

// DhcpProvider 交换机 DHCP 中继（VPP dhcp proxy）编排。
type DhcpProvider struct {
	client func() (DhcpClient, error)

	mu     sync.Mutex
	relays map[string]dhcpRelay // 交换机名 → 已下发的 proxy
}

// NewDhcpProvider 以固定客户端构造（测试）。
func NewDhcpProvider(c DhcpClient) *DhcpProvider {
	return &DhcpProvider{client: func() (DhcpClient, error) { return c, nil },
		relays: map[string]dhcpRelay{}}
}

// NewDhcpProviderFunc 以客户端工厂构造（连接可重连）。
func NewDhcpProviderFunc(f func() (DhcpClient, error)) *DhcpProvider {
	return &DhcpProvider{client: f, relays: map[string]dhcpRelay{}}
}

// reset 清空进程内登记（恢复收敛前调用）：登记只反映「本进程下发过什么」，
// VPP 重启后 proxy 已不在，按配置重放会重新下发并重建登记。
func (p *DhcpProvider) reset() {
	p.mu.Lock()
	p.relays = map[string]dhcpRelay{}
	p.mu.Unlock()
}

// relayTargetOf 由交换机网关声明推导 proxy 的表 id 与中继源地址（与 ApplyGateway 同一来源）。
func relayTargetOf(vs model.VirtualSwitch) (uint32, string, error) {
	gw := vs.Gateway
	if gw == nil {
		return 0, "", fmt.Errorf("交换机 %s 未配置网关，无法作 DHCP 中继（先 set virtual-switches %s gateway ip <ip-prefix>）",
			vs.Name, vs.Name)
	}
	name := gw.Vrf
	if name == "" {
		name = GatewayVRFName(vs.Name)
	}
	for _, a := range gw.Addresses {
		ip, _, err := net.ParseCIDR(a)
		if err != nil {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			return TableID(name), v4.String(), nil
		}
	}
	return TableID(name), "", fmt.Errorf("交换机 %s 的网关没有 IPv4 地址，无法作 DHCP 中继源", vs.Name)
}

// SyncRelay 把一台交换机的 dhcp-relay 声明收敛到 VPP（决策 #335）：
//   - 声明了 server：与登记一致则幂等空操作；记录发生变化（新配/改 server/换网关域）先按旧记录
//     撤掉 VPP 里的旧条目（IsAdd=false 幂等）再下发新条目——同域改 server 也必须撤旧（决策 #380）；
//   - 未声明（清 relay）：按登记发 IsAdd=false（幂等；无登记即无事可做）。
func (p *DhcpProvider) SyncRelay(ctx context.Context, vs model.VirtualSwitch) error {
	p.mu.Lock()
	rec, had := p.relays[vs.Name]
	p.mu.Unlock()
	if vs.DhcpRelayServer == "" {
		if !had {
			return nil
		}
		c, err := p.client()
		if err != nil {
			return err
		}
		defer c.Close()
		if err := c.ProxySet(rec.tableID, rec.tableID, false, rec.server, rec.src); err != nil {
			return fmt.Errorf("撤销交换机 %s 的 DHCP 中继: %w", vs.Name, err)
		}
		p.mu.Lock()
		delete(p.relays, vs.Name)
		p.mu.Unlock()
		return nil
	}
	tableID, src, err := relayTargetOf(vs)
	if err != nil {
		return err
	}
	if had && rec == (dhcpRelay{tableID: tableID, server: vs.DhcpRelayServer, src: src}) {
		return nil
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	if had {
		// 记录已变（同域改 server 或换转发域）：先用**旧记录**撤掉 VPP 里的旧条目（IsAdd=false 幂等），
		// 再下发新条目。**同域改 server 也必须撤旧**——只 add 不撤会让旧条目残留在同一张表里
		// （`vppctl show dhcp proxy` 出现两条，此后删除 relay 只撤最新一条，旧条目永久残留、该 FIB
		// 仍被 proxy 节点吞包——决策 #380/R140-1 真机复现）。此处只可能是「记录已变」：
		// 值未变的幂等情形已在上面早退。
		if err := c.ProxySet(rec.tableID, rec.tableID, false, rec.server, rec.src); err != nil {
			return fmt.Errorf("撤销交换机 %s 原 DHCP 中继: %w", vs.Name, err)
		}
	}
	if err := c.ProxySet(tableID, tableID, true, vs.DhcpRelayServer, src); err != nil {
		return fmt.Errorf("配置交换机 %s 的 DHCP 中继（server %s）: %w", vs.Name, vs.DhcpRelayServer, err)
	}
	p.mu.Lock()
	p.relays[vs.Name] = dhcpRelay{tableID: tableID, server: vs.DhcpRelayServer, src: src}
	p.mu.Unlock()
	return nil
}

// DeleteRelay 撤销一台交换机的 proxy（删交换机时一并撤；无登记即无事可做——幂等）。
func (p *DhcpProvider) DeleteRelay(ctx context.Context, name string) error {
	p.mu.Lock()
	rec, ok := p.relays[name]
	delete(p.relays, name)
	p.mu.Unlock()
	if !ok {
		return nil
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.ProxySet(rec.tableID, rec.tableID, false, rec.server, rec.src); err != nil {
		return fmt.Errorf("撤销交换机 %s 的 DHCP 中继: %w", name, err)
	}
	return nil
}

// ReconcileProxy 对账 VPP 里实际的 DHCP proxy 与配置声明（决策 #380/R140-1）。
//
// declared 为**声明了 relay** 的交换机集合（rx 表 → server 由 relayTargetOf 推出）。
// 用 VPP `dhcp_proxy_dump` 取数据面实际条目，与声明比对：**未声明**的 server（rx 表不在声明集，
// 或该表数据面上多出声明之外的 server）逐个 ProxySet(IsAdd=false) 清除；清除失败如实返回。
// 这是**不靠进程内登记**的安全网——带外改动/进程重启/旧版本残留的条目仍能被清掉。
//
// 如实边界：dump 失败即返回错误（调用方跳过本轮、不误撤）；`relayTargetOf` 报错（网关无 IPv4）
// 的交换机**不计入声明集**——它本也无法下发 relay，其表上的残留条目因此会被视为未声明而清除
// （**无法判定即不能声称已声明**）；声明表里与声明一致的 server 不动。
func (p *DhcpProvider) ReconcileProxy(declared []model.VirtualSwitch) error {
	// 声明集：rx 表 id → 声明的 server。同一张表被多个交换机声明时以任一为准（产品语义同表同 server）。
	want := map[uint32]string{}
	for _, vs := range declared {
		if vs.DhcpRelayServer == "" {
			continue
		}
		tableID, _, err := relayTargetOf(vs)
		if err != nil {
			continue // 网关无 IPv4 ⇒ 表不可判定；不计入声明集（见上「如实边界」）
		}
		want[tableID] = vs.DhcpRelayServer
	}

	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	entries, err := c.ProxyDump()
	if err != nil {
		return fmt.Errorf("读取 DHCP proxy 运行态: %w", err)
	}
	for _, e := range entries {
		server, declaredTable := want[e.RxVrfID]
		for _, s := range e.Servers {
			// 声明表且 server 与声明相符 ⇒ 保留；其余（未声明表 / 声明表上的陈旧或多余 server）清除。
			if declaredTable && s.Server == server {
				continue
			}
			if err := c.ProxySet(e.RxVrfID, s.VrfID, false, s.Server, e.Src); err != nil {
				return fmt.Errorf("清除未声明的 DHCP proxy（rx_vrf=%d, server=%s）: %w", e.RxVrfID, s.Server, err)
			}
		}
	}
	return nil
}
