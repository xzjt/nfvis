package network

// govpp DHCP 客户端（决策 #335）：唯一使用 dhcp binapi 的地方。
// binapi `DHCPProxyConfig{RxVrfID, ServerVrfID, IsAdd, DHCPServer, DHCPSrcAddress}`：
// proxy 按 rx-VRF 生效（26.06 真机探明可用，`show dhcp proxy` 可回读）。

import (
	"fmt"

	"go.fd.io/govpp/api"
	"go.fd.io/govpp/binapi/dhcp"
	ip_types "go.fd.io/govpp/binapi/ip_types"
)

// DhcpClientFunc 返回随当前连接获取 DHCP proxy 客户端的工厂。
func (m *Manager) DhcpClientFunc() func() (DhcpClient, error) {
	return func() (DhcpClient, error) {
		ch, err := m.APIChannel()
		if err != nil {
			return nil, err
		}
		return &govppDhcpClient{ch: ch}, nil
	}
}

type govppDhcpClient struct{ ch api.Channel }

func (g *govppDhcpClient) Close() { g.ch.Close() }

// v4Address 字符串 → binapi v4 地址（非 v4/解析失败报错，不静默发零值）。
func v4Address(s string) (ip_types.Address, error) {
	a, err := ip_types.ParseIP4Address(s)
	if err != nil {
		return ip_types.Address{}, fmt.Errorf("非 IPv4 地址: %q", s)
	}
	return ip_types.Address{Af: ip_types.ADDRESS_IP4, Un: ip_types.AddressUnionIP4(a)}, nil
}

func (g *govppDhcpClient) ProxySet(rxVrfID, serverVrfID uint32, isAdd bool, server, src string) error {
	srv, err := v4Address(server)
	if err != nil {
		return err
	}
	srcAddr, err := v4Address(src)
	if err != nil {
		return err
	}
	reply := &dhcp.DHCPProxyConfigReply{}
	if err := g.ch.SendRequest(&dhcp.DHCPProxyConfig{
		RxVrfID:        rxVrfID,
		ServerVrfID:    serverVrfID,
		IsAdd:          isAdd,
		DHCPServer:     srv,
		DHCPSrcAddress: srcAddr,
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("dhcp_proxy_config(rx_vrf=%d,server_vrf=%d,add=%v,server=%s) retval=%d",
			rxVrfID, serverVrfID, isAdd, server, reply.Retval)
	}
	return nil
}
