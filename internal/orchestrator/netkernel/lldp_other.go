//go:build !linux

package netkernel

// 内核数据面 LLDP 的收发 socket 在非 Linux 上不可用（AF_PACKET 与 SockaddrLinklayer 是 Linux
// 专有；产品发布形态是 Linux）。本文件只为让开发机（Windows/macOS）能编译与跑单测——单测注入
// 内存底座（SetLLDPLayer），不依赖本实现。

import (
	"fmt"
	"net"
)

type lldpSysLayer struct{}

func defaultLLDPLayer() lldpLayer { return lldpSysLayer{} }

func (lldpSysLayer) Open(ifname string) (lldpIO, net.HardwareAddr, error) {
	return nil, nil, fmt.Errorf("内核数据面 LLDP 的收发 socket 仅支持 Linux（AF_PACKET）：接口 %s 无法打开", ifname)
}
