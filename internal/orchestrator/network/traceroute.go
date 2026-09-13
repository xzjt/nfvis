package network

// M3-9：宿主侧 ICMP traceroute（VPP 26.06 无 traceroute 能力，附录 A #36）。
// 需 root/CAP_NET_RAW；仅经宿主网络栈，无法经 VPP VRF 转发（vrf 参数由上层拒绝）。

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// NewICMPProber 返回宿主侧 IPv4 ICMP prober。
func NewICMPProber() TracerouteProber { return icmpProber{} }

type icmpProber struct{}

func (icmpProber) Trace(ctx context.Context, host string, maxTTL int, timeout time.Duration) ([]Hop, error) {
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
		return nil, fmt.Errorf("打开 ICMP socket（需 root/CAP_NET_RAW）: %w", err)
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
