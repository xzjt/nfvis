//go:build linux

package netkernel

// 内核数据面 LLDP 收发 socket 的**真实现**（[linux]）：每个启用接口一个 **AF_PACKET**
// （SOCK_RAW, ETH_P_LLDP = 0x88CC）绑该口：
//   - 收：socket 按协议号过滤，只把该口的 LLDP 以太帧交给用户态；
//   - 发：写完整以太帧（目的＝LLDP 保留组播 01:80:C2:00:00:0E、源＝本口 MAC）到
//     SockaddrLinklayer{Ifindex: 该口}，由该口直接发出（不查路由）。
//
// 阻塞读用 SO_RCVTIMEO（500ms）轮转：Close 先置标志再关 fd，避免 close 不中断已阻塞的
// recvfrom 导致收包协程永久卡住（与 DHCP 中继的 bridge 侧、DHCP 服务器 tap 同法）。

import (
	"errors"
	"fmt"
	"net"
	"syscall"
)

// defaultLLDPLayer 真实现（非 Linux 平台见 lldp_other.go 的兜底）。
func defaultLLDPLayer() lldpLayer { return lldpSysLayer{} }

type lldpSysLayer struct{}

// Open 打开绑该口的 AF_PACKET socket（协议号＝LLDP 以太类型），并如实返回该口的 MAC。
func (lldpSysLayer) Open(ifname string) (lldpIO, net.HardwareAddr, error) {
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return nil, nil, fmt.Errorf("内核接口 %s 不存在: %w（该口是否已声明并交数据面？）", ifname, err)
	}
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(relayHtons16(lldpEtherType)))
	if err != nil {
		return nil, nil, fmt.Errorf("创建 AF_PACKET 套接字: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrLinklayer{
		Protocol: relayHtons16(lldpEtherType), Ifindex: iface.Index,
	}); err != nil {
		syscall.Close(fd)
		return nil, nil, fmt.Errorf("绑定 AF_PACKET 到 %s: %w", ifname, err)
	}
	tv := syscall.Timeval{Sec: 0, Usec: 500000}
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		syscall.Close(fd)
		return nil, nil, fmt.Errorf("设置 %s 的接收超时: %w", ifname, err)
	}
	return &lldpSocket{fd: fd, name: ifname, index: iface.Index, closed: make(chan struct{})}, iface.HardwareAddr, nil
}

// lldpSocket AF_PACKET 帧收发实现（绑单个接口）。
type lldpSocket struct {
	fd     int
	name   string
	index  int
	closed chan struct{}
}

// Recv 收一个以太帧（收进调用方缓冲）；Close 后返回 net.ErrClosed；SO_RCVTIMEO 超时与
// EINTR 回到循环（检查关闭标志），其它错误如实返回。
func (s *lldpSocket) Recv(buf []byte) (int, error) {
	for {
		select {
		case <-s.closed:
			return 0, net.ErrClosed
		default:
		}
		n, _, err := syscall.Recvfrom(s.fd, buf, 0)
		if err == nil {
			if n == 0 {
				continue
			}
			return n, nil
		}
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR) {
			continue // 超时/被打断：回到循环检查 closed
		}
		select {
		case <-s.closed:
			return 0, net.ErrClosed
		default:
		}
		return 0, fmt.Errorf("从 %s 收 LLDP 帧: %w", s.name, err)
	}
}

// Send 写一个完整以太帧：目的 MAC 取自帧头（LLDP 广告固定为保留组播 01:80:C2:00:00:0E），
// 出接口＝本 socket 绑定的 ifindex。
func (s *lldpSocket) Send(frame []byte) error {
	select {
	case <-s.closed:
		return net.ErrClosed
	default:
	}
	if len(frame) < 14 {
		return fmt.Errorf("LLDP 帧过短（%d 字节，至少需要以太头）", len(frame))
	}
	var dst [8]byte
	copy(dst[:], frame[:6])
	if err := syscall.Sendto(s.fd, frame, 0, &syscall.SockaddrLinklayer{
		Protocol: relayHtons16(lldpEtherType), Ifindex: s.index, Halen: 6, Addr: dst,
	}); err != nil {
		return fmt.Errorf("往 %s 发 LLDP 帧: %w", s.name, err)
	}
	return nil
}

func (s *lldpSocket) Close() error {
	select {
	case <-s.closed:
		return nil
	default:
	}
	close(s.closed)
	return syscall.Close(s.fd)
}
