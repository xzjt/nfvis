//go:build !linux

package netkernel

// 内核 DHCP tap 的**持有**在非 Linux 上不可用（/dev/net/tun 与 TUNSETIFF 是 Linux 专有；产品发布
// 形态是 Linux）。本文件只为让开发机（Windows/macOS）能编译与跑单测——单测注入内存底座
// （SetDHCPServerTapLayer），不依赖本实现。

import (
	"fmt"

	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// defaultDHCPServerTapLayer 非 Linux 兜底：如实报不可用（文案里点名 /dev/net/tun，便于装配侧
// 的守护用例在任意平台识别「生产走的是持有底座」）。
func defaultDHCPServerTapLayer() dhcpTapLayer { return unsupportedTapLayer{} }

type unsupportedTapLayer struct{}

func (unsupportedTapLayer) Open(name string) (network.TapTransport, error) {
	return nil, fmt.Errorf("持有内核 DHCP tap %s: %w", name, errNoTapHolder)
}
