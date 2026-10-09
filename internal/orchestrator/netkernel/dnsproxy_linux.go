//go:build linux

package netkernel

// 内核数据面 DNS 代理域落点 socket 的**真实现**（[linux]）：UDP/53 绑该域落点的**具体地址**，
// 并**把 socket 绑到该域的内核 VRF 设备**（`SO_BINDTODEVICE`，与 DHCP 中继上行 / DHCP 服务器
// 单播接收同一绑定法）。
//
// 为什么绑**具体地址**：绑具体地址的 socket 只收目的地址＝该地址的单播查询——产品只接管
// 「指向产品域内地址的查询」，管理地址与主机上其它监听者不受影响（`0.0.0.0:53` 会把别处
// 指向本机的查询一并截走）；域的 IPv4 地址（BVI 网关 / l3-interface）在内核侧就是本机地址，
// 域内客户端查它即命中本 socket，应答是普通 UDP 回包（源地址自动＝落点、源端口 53）。
//
// 为什么必须绑 VRF 设备（l3mdev 语义）：域名下的落点地址落在 `vr-<交换机名>` 这类内核 VRF 里，
// 只绑地址不绑设备时内核按主表/默认域做本地查找与绑定检查，包到不了本 socket（真机上表现为
// 绑定失败 EADDRNOTAVAIL 或收不到查询）；绑 VRF 设备后内核按「入向设备是该 VRF 的从属」匹配
// 回本 socket。绑定在 bind 之前执行（ListenConfig.Control 的时序）。
//
// 阻塞读由 net 包的 Close 直接唤醒（Go 运行时保证），不需要 SO_RCVTIMEO 轮转。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"
)

// defaultDNSProxyLayer 真实现（非 Linux 平台见 dnsproxy_other.go 的兜底）。
func defaultDNSProxyLayer() dnsProxyLayer { return dnsProxySysLayer{} }

type dnsProxySysLayer struct{}

// Open 打开一个域落点 socket：本地 = 落点地址 : 53，作用域 = 该域 VRF 的内核设备。
func (dnsProxySysLayer) Open(addr net.IP, vrfDevice string) (dnsProxyIO, error) {
	v4 := addr.To4()
	if v4 == nil {
		return nil, fmt.Errorf("DNS 代理落点 %v 不是 IPv4 地址", addr)
	}
	if vrfDevice == "" {
		return nil, fmt.Errorf("DNS 代理域 VRF 的内核设备名为空（无法确定服务作用域）")
	}
	lc := net.ListenConfig{Control: func(_ string, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, vrfDevice)
		}); err != nil {
			return err
		}
		if serr != nil {
			return fmt.Errorf("绑定到 VRF 设备 %s: %w（该域的 VRF 设备是否已收敛？）", vrfDevice, serr)
		}
		return nil
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4",
		net.JoinHostPort(v4.String(), strconv.Itoa(dnsProxyPort)))
	if err != nil {
		return nil, err
	}
	conn, ok := pc.(*net.UDPConn)
	if !ok {
		_ = pc.Close()
		return nil, fmt.Errorf("DNS 代理落点 socket 类型异常（%T）", pc)
	}
	return &dnsProxyUDPSocket{conn: conn}, nil
}

// dnsProxyUDPSocket UDP/53 域落点实现。
type dnsProxyUDPSocket struct{ conn *net.UDPConn }

func (s *dnsProxyUDPSocket) Recv(buf []byte) (int, *net.UDPAddr, error) {
	n, from, err := s.conn.ReadFromUDP(buf)
	if err != nil {
		if errors.Is(err, net.ErrClosed) {
			return 0, nil, net.ErrClosed
		}
		return 0, nil, err
	}
	return n, from, nil
}

func (s *dnsProxyUDPSocket) Send(b []byte, to *net.UDPAddr) error {
	_, err := s.conn.WriteToUDP(b, to)
	return err
}

func (s *dnsProxyUDPSocket) Close() error { return s.conn.Close() }
