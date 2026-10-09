//go:build !linux

package netkernel

// DHCP 服务器单播接收 socket 在非 Linux 上不可用（`SO_BINDTODEVICE` 与 VRF 语义是 Linux 专有；
// 产品发布形态是 Linux）。本文件只为让开发机（Windows/macOS）能编译与跑单测——单测注入内存
// 底座（SetDHCPServerSocketLayer），不依赖本实现。

import (
	"fmt"
	"net"
)

type dhcpUnicastSysLayer struct{}

func defaultDHCPUnicastLayer() dhcpUnicastLayer { return dhcpUnicastSysLayer{} }

func (dhcpUnicastSysLayer) Open(net.IP, string) (dhcpUnicastIO, error) {
	return nil, fmt.Errorf("DHCP 服务器的单播接收 socket 仅支持 Linux（UDP/67 + VRF 绑定）：当前平台不可用")
}
