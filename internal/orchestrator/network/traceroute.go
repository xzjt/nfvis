package network

// M3-9 / 决策 #330：宿主侧 raw ICMP traceroute（VPP 26.06 无 traceroute 能力，附录 A #36）。
// 支持 v4（ICMP）与 v6（ICMPv6）两个协议族；仅经宿主网络栈，无法经 VPP VRF 转发
// （vrf 参数由上层拒绝，且对 v4/v6 一视同仁——不静默降级）。
//
// 需 root/CAP_NET_RAW（`ip4:icmp` / `ip6:ipv6-icmp` 原生套接字）。ICMPv6 同样需要该特权：
// 宿主为 root（nfvisd 以 root 运行）时可试；权限不足时套接字打开失败，错误文案如实说明。

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// NewICMPProber 返回宿主侧 ICMP prober（v4/v6 由 Trace 的 ipv6 形参选择）。
func NewICMPProber() TracerouteProber { return icmpProber{} }

type icmpProber struct{}

// Trace 逐跳探测到 host 的路径。ipv6=true 走 ICMPv6（Echo Request + Hop Limit），
// 否则走 ICMPv4（Echo + TTL）。
func (icmpProber) Trace(ctx context.Context, host string, ipv6Fam bool, maxTTL int, timeout time.Duration) ([]Hop, error) {
	if ipv6Fam {
		return trace6(ctx, host, maxTTL, timeout)
	}
	return trace4(ctx, host, maxTTL, timeout)
}

// trace4 宿主侧 IPv4 ICMP 路径跟踪。
func trace4(ctx context.Context, host string, maxTTL int, timeout time.Duration) ([]Hop, error) {
	dst, err := net.ResolveIPAddr("ip4", host)
	if err != nil {
		return nil, fmt.Errorf("解析目标 %s: %w", host, err)
	}
	if maxTTL <= 0 {
		maxTTL = 30
	}
	if timeout <= 0 {
		timeout = time.Second
	}
	c, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, fmt.Errorf("打开 ICMPv4 socket（需 root/CAP_NET_RAW）: %w", err)
	}
	defer c.Close()
	p4 := c.IPv4PacketConn()
	id := os.Getpid() & 0xffff

	hops := make([]Hop, 0, maxTTL)
	for ttl := 1; ttl <= maxTTL; ttl++ {
		if err := ctx.Err(); err != nil {
			return hops, err
		}
		if err := p4.SetTTL(ttl); err != nil {
			return hops, fmt.Errorf("设置 TTL=%d: %w", ttl, err)
		}
		start := time.Now()
		msg := icmp.Message{Type: ipv4.ICMPTypeEcho, Code: 0,
			Body: &icmp.Echo{ID: id, Seq: ttl, Data: []byte("nfvis")}}
		wb, err := msg.Marshal(nil)
		if err != nil {
			return hops, fmt.Errorf("编码探测包: %w", err)
		}
		if _, err := c.WriteTo(wb, dst); err != nil {
			return hops, fmt.Errorf("发送探测 TTL=%d: %w", ttl, err)
		}

		hop := Hop{TTL: ttl, Timeout: true}
		deadline := time.Now().Add(timeout)
		rb := make([]byte, 1500)
		for {
			if err := c.SetReadDeadline(deadline); err != nil {
				break
			}
			n, peer, err := c.ReadFrom(rb)
			if err != nil || ctx.Err() != nil {
				break // 超时或取消：本跳记为 *
			}
			m, err := icmp.ParseMessage(1, rb[:n]) // 1 = ICMPv4
			if err != nil {
				continue
			}
			switch m.Type {
			case ipv4.ICMPTypeTimeExceeded, ipv4.ICMPTypeEchoReply:
				hop = Hop{TTL: ttl, Addr: peer.String(), RTT: time.Since(start)}
			default:
				continue
			}
			break
		}
		hops = append(hops, hop)
		if !hop.Timeout && hop.Addr == dst.IP.String() { // 到达目标
			break
		}
	}
	return hops, nil
}

// trace6 宿主侧 IPv6 ICMPv6 路径跟踪（决策 #330）。与 v4 同构，差别只在协议族/常量：
// Echo Request + Hop Limit，ICMPv6 的 Time Exceeded / Echo Reply 类型；ParseMessage 用 58。
func trace6(ctx context.Context, host string, maxTTL int, timeout time.Duration) ([]Hop, error) {
	dst, err := net.ResolveIPAddr("ip6", host)
	if err != nil {
		return nil, fmt.Errorf("解析目标 %s: %w", host, err)
	}
	if maxTTL <= 0 {
		maxTTL = 30
	}
	if timeout <= 0 {
		timeout = time.Second
	}
	c, err := icmp.ListenPacket("ip6:ipv6-icmp", "::")
	if err != nil {
		return nil, fmt.Errorf("打开 ICMPv6 socket（需 root/CAP_NET_RAW）: %w", err)
	}
	defer c.Close()
	p6 := c.IPv6PacketConn()
	id := os.Getpid() & 0xffff

	hops := make([]Hop, 0, maxTTL)
	for ttl := 1; ttl <= maxTTL; ttl++ {
		if err := ctx.Err(); err != nil {
			return hops, err
		}
		// ICMPv6 用 Hop Limit（IPV6_UNICAST_HOPS）替代 v4 的 TTL。
		if err := p6.SetHopLimit(ttl); err != nil {
			return hops, fmt.Errorf("设置 Hop Limit=%d: %w", ttl, err)
		}
		start := time.Now()
		msg := icmp.Message{Type: ipv6.ICMPTypeEchoRequest, Code: 0,
			Body: &icmp.Echo{ID: id, Seq: ttl, Data: []byte("nfvis")}}
		wb, err := msg.Marshal(nil)
		if err != nil {
			return hops, fmt.Errorf("编码探测包: %w", err)
		}
		if _, err := c.WriteTo(wb, dst); err != nil {
			return hops, fmt.Errorf("发送探测 HopLimit=%d: %w", ttl, err)
		}

		hop := Hop{TTL: ttl, Timeout: true}
		deadline := time.Now().Add(timeout)
		rb := make([]byte, 1500)
		for {
			if err := c.SetReadDeadline(deadline); err != nil {
				break
			}
			n, peer, err := c.ReadFrom(rb)
			if err != nil || ctx.Err() != nil {
				break // 超时或取消：本跳记为 *
			}
			m, err := icmp.ParseMessage(58, rb[:n]) // 58 = ICMPv6
			if err != nil {
				continue
			}
			switch m.Type {
			case ipv6.ICMPTypeTimeExceeded, ipv6.ICMPTypeEchoReply:
				hop = Hop{TTL: ttl, Addr: peer.String(), RTT: time.Since(start)}
			default:
				continue
			}
			break
		}
		hops = append(hops, hop)
		if !hop.Timeout && hop.Addr == dst.IP.String() { // 到达目标
			break
		}
	}
	return hops, nil
}
