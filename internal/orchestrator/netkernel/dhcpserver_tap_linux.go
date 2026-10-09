//go:build linux

package netkernel

// 内核 DHCP 内置 tap 持有底座的**真实现**（[linux]）：/dev/net/tun + TUNSETIFF(IFF_TAP|IFF_NO_PI)
// 认领该设备（＝挂上队列，carrier 随之上来），收发直接读写该 fd。语义与不变量见 dhcpserver_tap.go 文件头。

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
	"unsafe"
)

// defaultDHCPServerTapLayer 真实现（非 Linux 平台见 dhcpserver_tap_other.go 的兜底）。
func defaultDHCPServerTapLayer() dhcpTapLayer { return kernelTapLayer{ops: syscallTunOps{}} }

// syscallTunOps 系统调用实现。
type syscallTunOps struct{}

func (syscallTunOps) IfaceMAC(name string) (net.HardwareAddr, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	return iface.HardwareAddr, nil
}

func (syscallTunOps) OpenCharDevice(path string) (int, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	return fd, nil
}

// ClaimIface TUNSETIFF 认领设备（把本 fd 挂成该 tun/tap 的一个队列）——**这一步才是「持有」**：
// 挂上后内核把 carrier 置 up（最后一个持有者关闭时再置 down）；没有持有者的持久化 tap 是
// `NO-CARRIER, state DOWN`，bridge 不会把洪泛帧投给它（真机实证的静默失效）。
// IFF_NO_PI ⇒ 读到的就是**裸以太帧**（不带 4 字节 tun_pi 头），与 AF_PACKET 底座口径一致。
//
// ifreq 布局（linux/uapi/linux/if.h）：`char ifr_name[IFNAMSIZ=16]; short ifr_flags; …`；
// ifr_flags 是**主机字节序的 short**——本产品发布架构为 linux/amd64 与 linux/arm64，均为小端。
func (syscallTunOps) ClaimIface(fd int, name string) error {
	if len(name) >= 16 {
		return fmt.Errorf("接口名 %q 超过内核上限 15 字符", name)
	}
	var ifr [40]byte
	copy(ifr[:16], name)
	binary.LittleEndian.PutUint16(ifr[16:18], uint16(syscall.IFF_TAP|syscall.IFF_NO_PI))
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TUNSETIFF),
		uintptr(unsafe.Pointer(&ifr[0]))); errno != 0 {
		return fmt.Errorf("ioctl(TUNSETIFF, %s): %w", name, errno)
	}
	return nil
}

// WaitReadable select(2) 等该 fd 可读（timeout 内）；被信号打断按「本次未就绪」处理（不是失效）。
func (syscallTunOps) WaitReadable(fd int, timeout time.Duration) (bool, error) {
	var fds syscall.FdSet
	if fd < 0 || fd/64 >= len(fds.Bits) {
		return false, fmt.Errorf("fd %d 超出 select 位图", fd)
	}
	fds.Bits[fd/64] |= 1 << (uint(fd) % 64)
	tv := syscall.NsecToTimeval(int64(timeout))
	n, err := syscall.Select(fd+1, &fds, nil, nil, &tv)
	if err != nil {
		if errors.Is(err, syscall.EINTR) {
			return false, nil
		}
		return false, err
	}
	return n > 0, nil
}

// ReadFrame 读一帧；被信号打断重试，暂不可读（EAGAIN）返回 (0, nil)（调用方回到循环）。
func (syscallTunOps) ReadFrame(fd int, buf []byte) (int, error) {
	for {
		n, err := syscall.Read(fd, buf)
		if err == nil {
			return n, nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			return 0, nil
		}
		return 0, fmt.Errorf("read: %w", err)
	}
}

// WriteFrame 写一帧（以「该端口收帧」的形态注入 bridge）。
func (syscallTunOps) WriteFrame(fd int, frame []byte) (int, error) {
	for {
		n, err := syscall.Write(fd, frame)
		if err == nil {
			return n, nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		return 0, fmt.Errorf("write: %w", err)
	}
}

func (syscallTunOps) CloseFD(fd int) error { return syscall.Close(fd) }
