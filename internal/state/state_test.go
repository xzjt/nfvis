package state

import (
	"context"
	"errors"
	"testing"
)

type fakeRuntime struct {
	rows []Thread
	err  error
}

func (f *fakeRuntime) Threads(context.Context) ([]Thread, error) { return f.rows, f.err }
func (f *fakeRuntime) InterfaceCounters(context.Context, string) (InterfaceCounters, bool) {
	return InterfaceCounters{RxPackets: 7, TxPackets: 3}, true
}
func (f *fakeRuntime) Buffers(context.Context) (Buffers, bool) {
	return Buffers{Pools: []BufferPool{{Name: "default-numa-0", Used: 10, Available: 90}}}, true
}
func (f *fakeRuntime) Memory(context.Context) (Memory, bool) {
	return Memory{Total: 100, Used: 40}, true
}

func TestStateThreads(t *testing.T) {
	s := New(&fakeRuntime{rows: []Thread{{ID: 0, Name: "vpp_main", Core: 4}, {ID: 1, Name: "vpp_wk_0", Type: "workers", Core: 5}}})
	rows := s.Threads(context.Background())
	if len(rows) != 2 || rows[1].Name != "vpp_wk_0" || rows[1].Core != 5 {
		t.Fatalf("Threads: %+v", rows)
	}
	// 错误/未接入 → 空（不 panic）
	if got := New(&fakeRuntime{err: errors.New("x")}).Threads(context.Background()); len(got) != 0 {
		t.Fatalf("错误应返回空: %+v", got)
	}
	if got := New(nil).Threads(context.Background()); len(got) != 0 {
		t.Fatalf("nil runtime 应返回空: %+v", got)
	}
	var nilState *State
	if got := nilState.Threads(context.Background()); len(got) != 0 {
		t.Fatalf("nil State 应返回空")
	}
}

func TestStateCountersAndBuffers(t *testing.T) {
	s := New(&fakeRuntime{})
	if st, ok := s.InterfaceCounters(context.Background(), "ens192"); !ok || st.RxPackets != 7 {
		t.Fatalf("接口统计: %+v %v", st, ok)
	}
	if b, ok := s.Buffers(context.Background()); !ok || len(b.Pools) != 1 {
		t.Fatalf("buffers: %+v %v", b, ok)
	}
	if m, ok := s.Memory(context.Background()); !ok || m.Used != 40 {
		t.Fatalf("memory: %+v %v", m, ok)
	}
	// 未接入 → false
	if _, ok := New(nil).Buffers(context.Background()); ok {
		t.Fatalf("nil runtime 应不可用")
	}
}
