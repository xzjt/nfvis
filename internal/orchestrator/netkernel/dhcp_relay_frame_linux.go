//go:build linux

package netkernel

// DHCP 中继收发底座的**真实现**（[linux]）。两块：
//
//  1. bridge 侧 = **AF_PACKET**（SOCK_RAW, ETH_P_IP）绑该 bridge：客户端此刻无 IP，回注不能靠
//     路由——写完整以太帧由 bridge 按目的 MAC 查 fdb 交换（未学到则洪泛），与产品自带 DHCP
//     服务器的回程同法。协议按**以太类型过滤到 IPv4**（ARP/IPv6 ND 等噪声帧不进用户态；
//     UDP/67 的判断在用户态做——不挂 cBPF，过滤口径简单可读，DHCP 速率极低）。
//  2. server 侧 = **连接式 UDP**：本地 = 中继源（BVI 的 v4 网关地址）: 67、对端 = server: 67。
//     连接式让内核按对端过滤——只有配置的 server 从 67 发来的报文才进用户态。
//
// 阻塞读用 SO_RCVTIMEO（500ms）轮转：Close 先置标志再关 fd，避免 close 不中断已阻塞的
// recvfrom 导致收包协程永久卡住（与 dhcpserver_tap_linux.go 同法）。UDP 侧由 net 包的
// Close 直接唤醒阻塞读（Go 运行时保证）。

import (
	"errors"
	"fmt"
	"net"
	"syscall"
)

// defaultRelaySocketLayer 真实现（非 Linux 平台见 dhcp_relay_frame_other.go 的兜底）。
func defaultRelaySocketLayer() relaySocketLayer { return relaySysLayer{} }

type relaySysLayer struct{}

// OpenBridge 打开绑该 bridge 的 AF_PACKET 套接字。
func (relaySysLayer) OpenBridge(bridge string) (relayFrameIO, error) {
	iface, err := net.InterfaceByName(bridge)
	if err != nil {
		return nil, fmt.Errorf("内核接口 %s 不存在: %w（交换机内核 bridge 是否已收敛？）", bridge, err)
	}
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(relayHtons16(syscall.ETH_P_IP)))
	if err != nil {
		return nil, fmt.Errorf("创建 AF_PACKET 套接字: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrLinklayer{
		Protocol: relayHtons16(syscall.ETH_P_IP), Ifindex: iface.Index,
	}); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("绑定 AF_PACKET 到 %s: %w", bridge, err)
	}
	tv := syscall.Timeval{Sec: 0, Usec: 500000}
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("设置 %s 的接收超时: %w", bridge, err)
	}
	return &relayFrameSocket{fd: fd, name: bridge, mac: iface.HardwareAddr, closed: make(chan struct{})}, nil
}

// OpenUplink 打开到 server:67 的连接式 UDP：本地 = 中继源地址 : 67，并**把 socket 绑到该域的
// VRF 设备**（`SO_BINDTODEVICE`，与既有 `ip vrf exec <vrf>` 同一口径）。
//
// 为什么必须绑 VRF 设备：BVI 地址落在 `vr-<交换机名>` 这类内核 VRF 里，域内路由也只在该 VRF 的
// 表里——只把 socket 绑到源地址时，本地发起的路由查找走**主表**（域内网段不在主表），`connect()`
// 阶段就会 ENETUNREACH；绑 VRF 设备后查找走该 VRF 的表（内核 l3mdev 语义：oif 为 l3mdev 主设备
// 时按其表查找、且不限制出接口），收包侧由内核按「入向设备是该 VRF 的从属」匹配回本 socket
// （bridge 作为该 VRF 的从属设备）。绑定早于 bind/connect（Dialer.Control 在两者之前执行）。
func (relaySysLayer) OpenUplink(src, server net.IP, vrfDevice string) (relayUplinkIO, error) {
	if src.To4() == nil || server.To4() == nil {
		return nil, fmt.Errorf("中继源地址 %s 与 server %s 都必须显式给出 IPv4 地址", src, server)
	}
	if vrfDevice == "" {
		return nil, fmt.Errorf("交换机域 VRF 的内核设备名为空（无法确定上行路由作用域）")
	}
	d := net.Dialer{
		LocalAddr: &net.UDPAddr{IP: src.To4(), Port: relayServerPort},
		Control: func(network, address string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, vrfDevice)
			}); err != nil {
				return err
			}
			if serr != nil {
				return fmt.Errorf("绑定到 VRF 设备 %s: %w（该域的网关 VRF 是否已收敛？）", vrfDevice, serr)
			}
			return nil
		},
	}
	conn, err := d.Dial("udp4", (&net.UDPAddr{IP: server.To4(), Port: relayServerPort}).String())
	if err != nil {
		return nil, err
	}
	return &relayUDPSocket{conn: conn.(*net.UDPConn)}, nil
}

// relayFrameSocket AF_PACKET 帧收发实现。
type relayFrameSocket struct {
	fd     int
	name   string
	mac    net.HardwareAddr
	closed chan struct{}
}

func (s *relayFrameSocket) MAC() net.HardwareAddr { return s.mac }

func (s *relayFrameSocket) Recv() ([]byte, error) {
	for {
		select {
		case <-s.closed:
			return nil, net.ErrClosed
		default:
		}
		buf := make([]byte, 65536)
		n, _, err := syscall.Recvfrom(s.fd, buf, 0)
		if err == nil {
			if n == 0 {
				continue
			}
			return append([]byte{}, buf[:n]...), nil
		}
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR) {
			continue // 超时/被打断：回到循环检查 closed
		}
		select {
		case <-s.closed:
			return nil, net.ErrClosed
		default:
		}
		return nil, fmt.Errorf("从 %s 收帧: %w", s.name, err)
	}
}

// Send 写一个完整以太帧（帧自带以太头；由 bridge 按目的 MAC 查 fdb 交换/洪泛）。
func (s *relayFrameSocket) Send(frame []byte) error {
	select {
	case <-s.closed:
		return net.ErrClosed
	default:
	}
	if _, err := syscall.Write(s.fd, frame); err != nil {
		return fmt.Errorf("往 %s 写帧: %w", s.name, err)
	}
	return nil
}

func (s *relayFrameSocket) Close() error {
	select {
	case <-s.closed:
		return nil
	default:
	}
	close(s.closed)
	return syscall.Close(s.fd)
}

// relayUDPSocket 连接式 UDP（本地 = 中继源地址 : 67 ↔ server : 67）。
type relayUDPSocket struct{ conn *net.UDPConn }

func (u *relayUDPSocket) Send(payload []byte) error {
	_, err := u.conn.Write(payload)
	return err
}

func (u *relayUDPSocket) Recv(buf []byte) (int, error) {
	n, err := u.conn.Read(buf)
	if err != nil {
		if errors.Is(err, net.ErrClosed) {
			return 0, net.ErrClosed
		}
		return 0, err
	}
	return n, nil
}

func (u *relayUDPSocket) Close() error { return u.conn.Close() }

// relayHtons16 16 位主机序 → 网络序（AF_PACKET 的协议号是网络序）。
func relayHtons16(v uint16) uint16 { return v<<8 | v>>8 }
