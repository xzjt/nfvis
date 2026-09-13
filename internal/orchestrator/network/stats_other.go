//go:build !linux

package network

// M3-7（二）：stats segment 依赖 statsclient（内部用 Linux 专属 syscall），
// 非 Linux 平台（日常开发 Windows）编译为不可用桩；真机为 Linux。
// 覆盖率门槛排除 *_govpp.go，本文件不在排除列表，但仅在非 Linux 生效。

import (
	"context"
	"fmt"

	"github.com/xzjt/nfvis/internal/state"
)

type statsConn struct{}

func (m *Manager) stats() (interface{}, error) {
	return nil, fmt.Errorf("stats segment 仅在 Linux 可用")
}

func (r *vppRuntime) InterfaceCounters(ctx context.Context, ifname string) (state.InterfaceCounters, bool) {
	return state.InterfaceCounters{}, false
}

func (r *vppRuntime) Buffers(ctx context.Context) (state.Buffers, bool) {
	return state.Buffers{}, false
}

func (r *vppRuntime) Memory(ctx context.Context) (state.Memory, bool) {
	return state.Memory{}, false
}
