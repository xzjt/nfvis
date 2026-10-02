package network

// govpp 数据面 DNS 客户端（决策 #338）：唯一使用 dns binapi 的地方。
// binapi（govpp v0.13.0，`go doc go.fd.io/govpp/binapi/dns` 实测字段）：
//   - dns_name_server_add_del { IsIP6 uint8; IsAdd uint8; ServerAddress [16]byte }
//   - dns_enable_disable     { Enable uint8 }
// CLI/真机对照：`vppctl show dns servers` / `show dns cache`（产品读视图只读自身配置+登记，
// 与 VPP 的对照由语义套件承担）。

import (
	"fmt"
	"net"

	"go.fd.io/govpp/api"
	"go.fd.io/govpp/binapi/dns"
)

// DNSClientFunc 返回随当前连接获取 dns 客户端的工厂。
func (m *Manager) DNSClientFunc() func() (DNSClient, error) {
	return func() (DNSClient, error) {
		ch, err := m.APIChannel()
		if err != nil {
			return nil, err
		}
		return &govppDNSClient{ch: ch}, nil
	}
}

type govppDNSClient struct{ ch api.Channel }

func (g *govppDNSClient) Close() { g.ch.Close() }

// dnsServerAddress 字符串 → dns_name_server_add_del 的 16 字节地址与 is_ip6 标志。
// v4 放前 4 字节、v6 放满 16 字节（VPP 侧即 ip46_address 的原始布局）。
func dnsServerAddress(s string) (addr [16]byte, isIP6 uint8, err error) {
	ip := net.ParseIP(s)
	if ip == nil {
		return addr, 0, fmt.Errorf("非 IP 地址: %q", s)
	}
	if v4 := ip.To4(); v4 != nil {
		copy(addr[:], v4)
		return addr, 0, nil
	}
	copy(addr[:], ip.To16())
	return addr, 1, nil
}

func (g *govppDNSClient) NameServerAddDel(server string, add bool) error {
	b, isIP6, err := dnsServerAddress(server)
	if err != nil {
		return err
	}
	isAdd := uint8(0)
	if add {
		isAdd = 1
	}
	reply := &dns.DNSNameServerAddDelReply{}
	if err := g.ch.SendRequest(&dns.DNSNameServerAddDel{
		IsIP6:         isIP6,
		IsAdd:         isAdd,
		ServerAddress: b[:],
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("dns_name_server_add_del(server=%s,add=%v) retval=%d", server, add, reply.Retval)
	}
	return nil
}

func (g *govppDNSClient) EnableDisable(enable bool) error {
	e := uint8(0)
	if enable {
		e = 1
	}
	reply := &dns.DNSEnableDisableReply{}
	if err := g.ch.SendRequest(&dns.DNSEnableDisable{Enable: e}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("dns_enable_disable(enable=%v) retval=%d", enable, reply.Retval)
	}
	return nil
}
