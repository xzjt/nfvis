//go:build !linux

package netkernel

// 内核数据面 DNS 代理的域落点 socket 在非 Linux 上不可用（`SO_BINDTODEVICE` 与 VRF 语义是
// Linux 专有；产品发布形态是 Linux）。本文件只为让开发机（Windows/macOS）能编译与跑单测——
// 单测注入内存底座（SetDNSProxyLayer），不依赖本实现。

import (
	"fmt"
	"net"
)

type dnsProxySysLayer struct{}

func defaultDNSProxyLayer() dnsProxyLayer { return dnsProxySysLayer{} }

func (dnsProxySysLayer) Open(net.IP, string) (dnsProxyIO, error) {
	return nil, fmt.Errorf("数据面 DNS 代理的域落点 socket 仅支持 Linux（UDP/53 + VRF 绑定）：当前平台不可用")
}
