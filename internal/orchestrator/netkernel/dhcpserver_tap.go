package netkernel

// 内核数据面 DHCP 内置 tap 的**持有与收发**（决策 #438 的内核专有传输面，本轮真机修复）。
//
// **为什么必须由产品自己持有该 tap**：TUN/TAP 的 carrier 来自「有没有进程打开 /dev/net/tun 并
// TUNSETIFF 挂上队列」——`ip tuntap add` 只建**持久化设备**、不产生持有者，此时设备是
// `NO-CARRIER, state DOWN`，而内核 bridge **不会把洪泛帧交给 carrier 为 down 的端口**（真机实证：
// tap 建了、命令全成功、客户端永远收不到 DISCOVER、租约表恒空——典型静默失效）。
// VPP 模式能工作，正是因为那边的 tap 由 VPP 持有（tapv2 之后 VPP 占着 fd）。
//
// **为什么收发走该 fd 而不是 AF_PACKET**：tap 的「线」就是持有它的那个 fd。AF_PACKET 往 tap 写帧
// 的语义是「把帧交给持有者」（VPP 模式正是靠这一条把应答交给 VPP 再进 BD）；当**我们自己**就是
// 持有者时，那条路只会把帧送回自己的队列。故这里直接读写 fd：
//   - `read`  = bridge 洪泛到该端口的帧（客户端 DISCOVER 等）；
//   - `write` = 以「该端口收到一帧」的形态注入 bridge（服务器应答，bridge 据此转发/洪泛给客户端）。
//
// 设备仍由 `ip tuntap add dev <nfvisdhXXXX> mode tap`（TUNSETPERSIST）创建、按名复用与核对身份
// （见 kernelDHCPServerClient），本文件只负责**认领它并长期持有**，fd 与服务器实例同生共死
// （provider 在启用时打开、停用/reset/进程退出时关闭——不泄漏，也不让 carrier 在停用后仍挂着）。
//
// 底座可注入（SetDHCPServerTapLayer）：真实现是 linux 系统调用（dhcpserver_tap_linux.go），
// 单测注入内存实现，从而在**任意平台**断言两条不变量：①「打开 /dev/net/tun + TUNSETIFF 认领」
// 这一步在场（＝有持有者，carrier 有保证）；② 收发确实走该 fd。

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// tunDevicePath 认领 tap 的字符设备（Linux）。
const tunDevicePath = "/dev/net/tun"

// dhcpTapReadTimeout 单次等可读的上界（与既有 AF_PACKET 底座的 SO_RCVTIMEO 同法：轮转回到循环
// 检查 closed，Close 不因阻塞读而卡住）。
const dhcpTapReadTimeout = 500 * time.Millisecond

// dhcpTapLayer 内置 tap 的**持有底座**：打开并长期持有该设备，返回可读写的以太帧传输。
type dhcpTapLayer interface {
	Open(name string) (network.TapTransport, error)
}

// dhcpTunOps 持有内置 tap 所需的原始操作（真实现 = linux 系统调用 + net.InterfaceByName；
// 单测注入内存实现）。拆到这一层是为了让「持有步骤」与「fd 收发」在单测里可断言、不碰真设备。
type dhcpTunOps interface {
	// IfaceMAC 读该内核接口的 MAC（应答帧的以太源）。设备不存在/读不到即报错。
	IfaceMAC(name string) (net.HardwareAddr, error)
	// OpenCharDevice 打开字符设备（= tunDevicePath），返回 fd。
	OpenCharDevice(path string) (int, error)
	// ClaimIface 在 fd 上认领该设备（TUNSETIFF：IFF_TAP|IFF_NO_PI）——这一步才是「持有」。
	ClaimIface(fd int, name string) error
	// WaitReadable 等 fd 可读（timeout 内），超时返回 (false, nil)。
	WaitReadable(fd int, timeout time.Duration) (bool, error)
	// ReadFrame 读一帧以太帧；**本次无可读帧返回 (0, nil)**（调用方回到循环重试，不算错误）。
	ReadFrame(fd int, buf []byte) (int, error)
	// WriteFrame 写一帧以太帧（注入 bridge）。
	WriteFrame(fd int, frame []byte) (int, error)
	// CloseFD 关闭 fd（释放持有：carrier 随之落下；设备本身是持久化的，下次启动复用）。
	CloseFD(fd int) error
}

// kernelTapLayer 真实现：认领（打开 /dev/net/tun + TUNSETIFF）并长期持有该 tap，收发走该 fd。
type kernelTapLayer struct{ ops dhcpTunOps }

func (l kernelTapLayer) Open(name string) (network.TapTransport, error) {
	mac, err := l.ops.IfaceMAC(name)
	if err != nil {
		// 这里必须把「为什么要持有」写进文案：不持有 ⇒ carrier down ⇒ bridge 不投递（真机实证）。
		return nil, fmt.Errorf("内核侧 tap %s 不存在或读不到（内置 tap 是否已创建？）；"+
			"本数据面由产品打开 %s 并长期持有它（TUNSETIFF）以保证 carrier: %w", name, tunDevicePath, err)
	}
	fd, err := l.ops.OpenCharDevice(tunDevicePath)
	if err != nil {
		return nil, fmt.Errorf("打开 %s 以持有 tap %s（carrier 由持有者保证）: %w", tunDevicePath, name, err)
	}
	if err := l.ops.ClaimIface(fd, name); err != nil {
		_ = l.ops.CloseFD(fd) // 认领失败不留下已打开的 fd
		return nil, fmt.Errorf("认领内核 tap %s（TUNSETIFF，%s）: %w", name, tunDevicePath, err)
	}
	return &tunTapTransport{ops: l.ops, fd: fd, name: name, mac: mac, closed: make(chan struct{})}, nil
}

// tunTapTransport 持有中的 tap：read = bridge 洪泛到该端口的帧；write = 注入 bridge（见文件头）。
type tunTapTransport struct {
	ops       dhcpTunOps
	fd        int
	name      string
	mac       net.HardwareAddr
	closed    chan struct{}
	closeOnce sync.Once
}

func (t *tunTapTransport) MAC() net.HardwareAddr { return t.mac }

func (t *tunTapTransport) Recv() ([]byte, error) {
	for {
		select {
		case <-t.closed:
			return nil, net.ErrClosed
		default:
		}
		ready, err := t.ops.WaitReadable(t.fd, dhcpTapReadTimeout)
		if err != nil {
			select {
			case <-t.closed:
				return nil, net.ErrClosed
			default:
			}
			return nil, fmt.Errorf("等待 %s 可读: %w", t.name, err)
		}
		if !ready {
			continue // 超时：回到循环检查 closed
		}
		buf := make([]byte, 65536)
		n, err := t.ops.ReadFrame(t.fd, buf)
		if err == nil {
			if n == 0 {
				continue // 本次无可读帧（EAGAIN/被信号打断）：继续
			}
			return append([]byte{}, buf[:n]...), nil
		}
		select {
		case <-t.closed:
			return nil, net.ErrClosed
		default:
		}
		return nil, fmt.Errorf("从 %s 收帧: %w", t.name, err)
	}
}

// Send 写一帧到持有的 fd ⇒ 以「该端口收帧」的形态进入 bridge（由 bridge 转发/洪泛给客户端）。
func (t *tunTapTransport) Send(frame []byte) error {
	select {
	case <-t.closed:
		return net.ErrClosed
	default:
	}
	if _, err := t.ops.WriteFrame(t.fd, frame); err != nil {
		return fmt.Errorf("往 %s 写帧: %w", t.name, err)
	}
	return nil
}

// Close 释放持有：先置停止位（阻塞读的轮转随之退出），再关 fd（carrier 落下，设备保留复用）。
func (t *tunTapTransport) Close() error {
	var err error
	t.closeOnce.Do(func() {
		close(t.closed)
		err = t.ops.CloseFD(t.fd)
	})
	return err
}

// dhcpTapLayerOrDefault 生效的持有底座（未注入 = 平台真实现；非 Linux 见 dhcpserver_tap_other.go）。
func (p *Provider) dhcpTapLayerOrDefault() dhcpTapLayer {
	if p.dhcpTapLayer != nil {
		return p.dhcpTapLayer
	}
	return defaultDHCPServerTapLayer()
}

// SetDHCPServerTapLayer 注入内置 tap 的持有底座（**单测专用**：不碰真设备；须在任何 DHCP 服务器
// 收敛之前调用）。nil 不改变平台默认实现。
func (p *Provider) SetDHCPServerTapLayer(l dhcpTapLayer) {
	if l == nil {
		return
	}
	p.dhcpMu.Lock()
	defer p.dhcpMu.Unlock()
	p.dhcpTapLayer = l
}

// errNoTapHolder 非 Linux 的兜底错误（同 dhcpserver_socket_other.go 的口径）。
var errNoTapHolder = errors.New("内核 DHCP tap 的持有（/dev/net/tun + TUNSETIFF）仅支持 Linux")
