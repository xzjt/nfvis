package main

// 决策 #504：运行态聚合器的**装配分叉**守护。
//
// 由来（真机 round47）：修复前 `stateRuntimeFor(kernel, …)` 直接返回 nil，于是内核数据面下
// `show interfaces <口> statistics` 与 `GET /interfaces/{口}` 的 statistics **恒无输出**——
// 而内核 sysfs 本就记着 rx_dropped/tx_dropped 两个真值。改法是把内核数据面接上**只有接口
// 计数**的窄来源（state.NewCounters），其余读视图仍按数据面点名不可用。
//
// 分叉本身在此用假计数源验证；sysfs 逐项读取（缺文件/脏值/未知口）由
// internal/orchestrator/netkernel/counters_test.go 覆盖（那边能改 sysfs 根）。

import (
	"context"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/state"
)

// stubCounters 内核数据面的假计数源（只有接口计数）。
type stubCounters struct {
	c  state.InterfaceCounters
	ok bool
}

func (s stubCounters) InterfaceCounters(context.Context, string) (state.InterfaceCounters, bool) {
	return s.c, s.ok
}

// TestStateRuntimeForKernelServesInterfaceCounters 内核分支**必须**给出接口计数
// （修复前返回 nil ⇒ 所有出口无 statistics），且不伪造 VPP 运行态。
func TestStateRuntimeForKernelServesInterfaceCounters(t *testing.T) {
	st := stateRuntimeFor(model.DataPlaneKernel, nil, stubCounters{
		ok: true,
		c:  state.InterfaceCounters{RxDrops: 7, TxDrops: 8, DirectionalDrops: true},
	})
	if st == nil {
		t.Fatal("内核数据面也必须给出运行态聚合器（只有接口计数，不是 nil）")
	}
	c, ok := st.InterfaceCounters(context.Background(), "ens192")
	if !ok || c.RxDrops != 7 || c.TxDrops != 8 {
		t.Fatalf("内核数据面应按方向给出丢弃（rx=7 tx=8）：%+v %v", c, ok)
	}
	// 不得伪造 VPP 运行态（内核没有 stats segment）。
	if rows := st.Threads(context.Background()); len(rows) != 0 {
		t.Fatalf("内核数据面不得给出 VPP 线程：%+v", rows)
	}
	if _, ok := st.Buffers(context.Background()); ok {
		t.Fatal("内核数据面不得报 buffer 统计可用")
	}
	if _, ok := st.Memory(context.Background()); ok {
		t.Fatal("内核数据面不得报内存统计可用")
	}
	if _, ok := st.RuntimeStats(context.Background()); ok {
		t.Fatal("内核数据面不得报运行态可用")
	}
}

// TestStateRuntimeForKernelWithoutProvider 内核编排未装配（nil）时聚合器仍在、但计数不可用
// （不 panic、不编造 0）——读视图据此按既有文案如实说明。
func TestStateRuntimeForKernelWithoutProvider(t *testing.T) {
	st := stateRuntimeFor(model.DataPlaneKernel, nil, nil)
	if st == nil {
		t.Fatal("即使编排未装配也应给出聚合器（调用方不必分支）")
	}
	if _, ok := st.InterfaceCounters(context.Background(), "ens192"); ok {
		t.Fatal("无计数源时不得报计数（不编造）")
	}
}

// TestStateRuntimeForVPPDoesNotServeKernelCounters vpp 分支走 stats segment（mgr.Runtime()），
// 不消费内核计数源：即便同时传了内核计数，vpp 分支也必须忽略它（两口径不得混）。
func TestStateRuntimeForVPPDoesNotServeKernelCounters(t *testing.T) {
	st := stateRuntimeFor(model.DataPlaneVPP, nil, stubCounters{
		ok: true,
		c:  state.InterfaceCounters{RxDrops: 7, TxDrops: 8, DirectionalDrops: true},
	})
	if st == nil {
		t.Fatal("vpp 分支应给出聚合器")
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("vpp 分支（mgr 为 nil）不得 panic：%v", r)
		}
	}()
	// mgr 为 nil ⇒ stats segment 不可用；关键断言是**没有**回落到内核计数（7/8 不得出现）。
	if c, ok := st.InterfaceCounters(context.Background(), "ens192"); ok {
		t.Fatalf("vpp 分支不得回落到内核计数（混了两种口径）：%+v", c)
	}
}
