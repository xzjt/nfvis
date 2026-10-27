package network

// M3-7：VPP 运行态读取（internal/state.Runtime 的 VPP 实现）。

import (
	"context"
	"fmt"

	"go.fd.io/govpp/binapi/vlib"

	"github.com/xzjt/nfvis/internal/state"
)

// Runtime 返回 VPP 运行态数据源（供 internal/state 聚合，API/CLI 使用）。
//
// 决策 #521：**nil 管理器返回空数据源**。此前无条件返回 `&vppRuntime{m: m}`——Linux 构建下
// `vppRuntime` 的读法（stats_linux.go）会解引用 `r.m`，于是「管理器为 nil 但按 VPP 分支装配」
// 的路径不再是「读不到」而是**空指针崩溃**；非 Linux 构建的桩恰好不解引用，故这个缺陷只在
// Linux（CI）暴露（round48 的装配守护 `TestStateRuntimeForVPPDoesNotServeKernelCounters`
// 用 nil 管理器覆盖 VPP 分支，自 v3.0.31 起 CI 全红，本地 Windows 全绿）。
// 返回 nil 后 `state.New(nil)` 的一切读数如实「不可用」，既不再崩溃，也**不会**回落到内核计数
// （两种口径仍互斥，见 internal/state 的 InterfaceCounters）。
func (m *Manager) Runtime() state.Runtime {
	if m == nil {
		return nil
	}
	return &vppRuntime{m: m}
}

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
