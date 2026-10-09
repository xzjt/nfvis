//go:build linux

package netkernel

// DHCP 服务器单播接收 socket 的**真实现**（[linux]）：UDP/67 绑该交换机 BVI 网关地址，
// 并**把 socket 绑到该域的 VRF 设备**（`SO_BINDTODEVICE`，与既有 DHCP 中继上行同法）。
//
// 为什么必须绑 VRF 设备：BVI 地址落在 `vr-<交换机名>` 这类内核 VRF 里，只绑地址不绑设备时
// 内核的 socket 查找按主表/默认域进行，包到不了本 socket；绑 VRF 设备后内核按「入向设备是
// 该 VRF 的从属（bridge 已 enslave 到 VRF）」匹配回本 socket（l3mdev 语义），绑定地址本身的
// 本地性检查也按该 VRF 的表判定。绑定早于 bind（ListenConfig.Control 在 bind 之前执行）。
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

// defaultDHCPUnicastLayer 真实现（非 Linux 平台见 dhcpserver_socket_other.go 的兜底）。
func defaultDHCPUnicastLayer() dhcpUnicastLayer { return dhcpUnicastSysLayer{} }

type dhcpUnicastSysLayer struct{}

// Open 打开单播接收 socket：本地 = BVI 地址 : 67，作用域 = 该域 VRF 的内核设备。
func (dhcpUnicastSysLayer) Open(bvi net.IP, vrfDevice string) (dhcpUnicastIO, error) {
	v4 := bvi.To4()
	if v4 == nil {
		return nil, fmt.Errorf("BVI 网关地址 %v 不是 IPv4 地址", bvi)
	}
	if vrfDevice == "" {
		return nil, fmt.Errorf("交换机域 VRF 的内核设备名为空（无法确定接收作用域）")
	}
	lc := net.ListenConfig{Control: func(_ string, _ string, c syscall.RawConn) error {
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
	}}
	// 绑**具体地址**（不是 INADDR_ANY）：只收目的地址＝该 BVI 的单播（续租/释放），域内广播
	// 不进这条路径、只走内置 tap 的洪泛路径——两条入径互补（见 dhcpserver.go 文件头）。
	pc, err := lc.ListenPacket(context.Background(), "udp4",
		net.JoinHostPort(v4.String(), strconv.Itoa(dhcpUnicastPort)))
	if err != nil {
		return nil, err
	}
	conn, ok := pc.(*net.UDPConn)
	if !ok {
		_ = pc.Close()
		return nil, fmt.Errorf("单播接收 socket 类型异常（%T）", pc)
	}
	return &dhcpUnicastUDPSocket{conn: conn}, nil
}

// dhcpUnicastUDPSocket UDP/67 单播接收实现。
type dhcpUnicastUDPSocket struct{ conn *net.UDPConn }

func (s *dhcpUnicastUDPSocket) Recv(buf []byte) (int, *net.UDPAddr, error) {
	n, from, err := s.conn.ReadFromUDP(buf)
	if err != nil {
		if errors.Is(err, net.ErrClosed) {
			return 0, nil, net.ErrClosed
		}
		return 0, nil, err
	}
	return n, from, nil
}

func (s *dhcpUnicastUDPSocket) Close() error { return s.conn.Close() }
