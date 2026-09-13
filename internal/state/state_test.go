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
