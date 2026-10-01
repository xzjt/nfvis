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
	Close()
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
//   - 声明了 server：与登记一致则幂等空操作；新配/改 server 下发 IsAdd=true；
//     网关域换了（表 id 变）先按登记撤旧域的 proxy 再下发；
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
	if had && rec.tableID != tableID {
		// 网关域换了：旧域的 proxy 先撤（IsAdd=false 幂等），否则旧表里留一个无人管的 proxy
		if err := c.ProxySet(rec.tableID, rec.tableID, false, rec.server, rec.src); err != nil {
			return fmt.Errorf("撤销交换机 %s 原转发域的 DHCP 中继: %w", vs.Name, err)
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
