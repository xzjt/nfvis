//go:build !linux

package netkernel

// DHCP 中继的收发底座在非 Linux 上不可用（AF_PACKET 是 Linux 专有；产品发布形态是 Linux）。
// 本文件只为让开发机（Windows/macOS）能编译与跑单测——单测注入内存底座（SetRelaySocketLayer），
// 不依赖本实现。

import (
	"fmt"
	"net"
)

type relaySysLayer struct{}

func defaultRelaySocketLayer() relaySocketLayer { return relaySysLayer{} }

func (relaySysLayer) OpenBridge(bridge string) (relayFrameIO, error) {
	return nil, fmt.Errorf("DHCP 中继的 bridge 收发仅支持 Linux（AF_PACKET）：当前平台不可用")
}

func (relaySysLayer) OpenUplink(src, server net.IP, vrfDevice string) (relayUplinkIO, error) {
	return nil, fmt.Errorf("DHCP 中继的 server 收仅支持 Linux：当前平台不可用")
}
