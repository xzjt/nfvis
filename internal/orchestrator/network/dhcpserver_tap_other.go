//go:build !linux

package network

// DHCP 内置 tap 的收发在非 Linux 上不可用（AF_PACKET 是 Linux 专有）。
// 产品发布形态是 Linux（debian 包）；本文件只为让开发机（Windows/macOS）能编译与跑单测——
// 单测注入内存传输（SetTapOpen），不依赖本实现。

import (
	"fmt"
	"net"
)

// dhcpTapTransport 内核侧 tap 的以太帧收发（真实=AF_PACKET[linux]；单测注入内存实现）。
type dhcpTapTransport interface {
	Recv() ([]byte, error) // 一个完整以太帧；Close 后返回 net.ErrClosed
	Send(frame []byte) error
	MAC() net.HardwareAddr // 内核侧 tap 的 MAC（服务器以太源）
	Close() error
}

func openDHCPTap(name string) (dhcpTapTransport, error) {
	return nil, fmt.Errorf("DHCP 服务器的内置 tap 收发仅支持 Linux（内核侧 AF_PACKET）：当前平台不可用")
}
