//go:build linux

package network

// DHCP 内置 tap 的内核侧以太帧收发（决策 #359，[linux]）——唯一使用 AF_PACKET 的地方。
//
// 形态（round140 实测）：VPP 建的 tap 在内核侧是一个普通 netdev（名由 host-if-name 指定）；
// 宿主用 AF_PACKET（SOCK_RAW, ETH_P_ALL）绑该接口读/写以太帧——收是广播入径（DISCOVER 等
// 经 BD 洪泛到 tap），发是应答回程（写以太帧，由 BD 按目的 MAC 交换/洪泛回客户端）。
//
// 阻塞读用 SO_RCVTIMEO（500ms）轮转：Close 先置标志再关 fd，避免 close 不中断已阻塞的
// recvfrom 导致收包协程永久卡住。

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
)

// dhcpTapTransport 内核侧 tap 的以太帧收发（真实=AF_PACKET；单测注入内存实现）。
type dhcpTapTransport interface {
	Recv() ([]byte, error) // 一个完整以太帧；Close 后返回 net.ErrClosed
	Send(frame []byte) error
	MAC() net.HardwareAddr // 内核侧 tap 的 MAC（服务器以太源）
	Close() error
}

// packetTap AF_PACKET 传输实现。
type packetTap struct {
	fd     int
	name   string
	mac    net.HardwareAddr
	closed chan struct{}
}

// openDHCPTap 打开（绑定）内核侧 tap。tap 由 VPP 刚创建时内核 netdev 可能稍晚出现，
// 故按 200ms × 最多 15 次重试（有界，失败如实报错）。
func openDHCPTap(name string) (dhcpTapTransport, error) {
	var (
		iface *net.Interface
		err   error
	)
	for i := 0; i < 15; i++ {
		iface, err = net.InterfaceByName(name)
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		return nil, fmt.Errorf("内核侧接口 %s 不存在: %w", name, err)
	}
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(htons16(syscall.ETH_P_ALL)))
	if err != nil {
		return nil, fmt.Errorf("创建 AF_PACKET 套接字: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrLinklayer{
		Protocol: htons16(syscall.ETH_P_ALL), Ifindex: iface.Index,
	}); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("绑定 AF_PACKET 到 %s: %w", name, err)
	}
	tv := syscall.Timeval{Sec: 0, Usec: 500000}
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("设置 %s 的接收超时: %w", name, err)
	}
	return &packetTap{fd: fd, name: name, mac: iface.HardwareAddr, closed: make(chan struct{})}, nil
}

func (t *packetTap) MAC() net.HardwareAddr { return t.mac }

func (t *packetTap) Recv() ([]byte, error) {
	for {
		select {
		case <-t.closed:
			return nil, net.ErrClosed
		default:
		}
		buf := make([]byte, 65536)
		n, _, err := syscall.Recvfrom(t.fd, buf, 0)
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
		case <-t.closed:
			return nil, net.ErrClosed
		default:
		}
		return nil, fmt.Errorf("从 %s 收包: %w", t.name, err)
	}
}

// Send 写一个完整以太帧到绑定的内核侧 tap（帧自带以太头，目的 MAC 由构造方给出）。
func (t *packetTap) Send(frame []byte) error {
	select {
	case <-t.closed:
		return net.ErrClosed
	default:
	}
	if _, err := syscall.Write(t.fd, frame); err != nil {
		return fmt.Errorf("往 %s 写帧: %w", t.name, err)
	}
	return nil
}

func (t *packetTap) Close() error {
	select {
	case <-t.closed:
		return nil
	default:
	}
	close(t.closed)
	return syscall.Close(t.fd)
}

// htons16 16 位主机序 → 网络序（AF_PACKET 的协议号是网络序）。
func htons16(v uint16) uint16 { return v<<8 | v>>8 }
