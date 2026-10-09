//go:build !linux

package netkernel

// 容器 vNIC 宿主端 veth 在非 Linux 上不可用（veth/netns/`nsenter` 都是 Linux 专有；产品发布
// 形态是 Linux）。本文件只为让开发机（Windows/macOS）能编译与跑单测——单测注入内存底座
// （SetContainerVethLayer），不依赖本实现。

import "fmt"

type ctVethSysLayer struct{}

func defaultCtVethLayer(*Provider) ctVethLayer { return ctVethSysLayer{} }

func (ctVethSysLayer) Open() (ctVethIO, error) {
	return nil, fmt.Errorf("内核数据面容器 vNIC 的 veth 接入仅支持 Linux（需要网络命名空间与 nsenter）：本平台无法提供该底座")
}
