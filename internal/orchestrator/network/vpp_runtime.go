package network

// M3-7：VPP 运行态读取（internal/state.Runtime 的 VPP 实现）。

import (
	"context"
	"fmt"

	"go.fd.io/govpp/binapi/vlib"

	"github.com/xzjt/nfvis/internal/state"
)

// Runtime 返回 VPP 运行态数据源（供 internal/state 聚合，API/CLI 使用）。
func (m *Manager) Runtime() state.Runtime { return &vppRuntime{m: m} }

type vppRuntime struct{ m *Manager }

func (r *vppRuntime) Threads(ctx context.Context) ([]state.Thread, error) {
	ch, err := r.m.APIChannel()
	if err != nil {
		return nil, err
	}
	defer ch.Close()
	reply := &vlib.ShowThreadsReply{}
	if err := ch.SendRequest(&vlib.ShowThreads{}).ReceiveReply(reply); err != nil {
		return nil, err
	}
	if reply.Retval != 0 {
		return nil, fmt.Errorf("show_threads retval=%d", reply.Retval)
	}
	out := make([]state.Thread, 0, len(reply.ThreadData))
	for _, t := range reply.ThreadData {
		out = append(out, state.Thread{ID: t.ID, Name: t.Name, Type: t.Type, Core: t.CPUID})
	}
	return out, nil
}
