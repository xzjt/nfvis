package state

import (
	"context"
	"encoding/json"
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

func (f *fakeRuntime) RuntimeStats(context.Context) (RuntimeStats, bool) {
	return RuntimeStats{VectorRate: 12, WorkerThreads: 1,
		Threads: []RuntimeThread{{ID: 1, VectorRate: 12, LoopsRate: 34}}}, true
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

// ---------- 决策 #504：丢弃计数的两种口径 ----------
//
// VPP 只有方向无关的总数（`drops`），内核网络数据面只有按方向的两个真值
// （`rx_drops`/`tx_drops`）。JSON 出口按 DirectionalDrops 只发适用者——不适用的键**缺席**
// 而不是发 0（0 是「真没丢弃」，缺席是「本数据面不提供该口径」，两件事不能混）。

// fakeCounters 只实现 Counters（内核数据面的窄接口）。
type fakeCounters struct {
	c  InterfaceCounters
	ok bool
}

func (f fakeCounters) InterfaceCounters(context.Context, string) (InterfaceCounters, bool) {
	return f.c, f.ok
}

func decodeJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return m
}

// VPP 口径：发 drops，不发 rx_drops/tx_drops（旧实现两方向键恒 0 且无 drops）。
func TestInterfaceCountersJSONVPPShape(t *testing.T) {
	m := decodeJSON(t, InterfaceCounters{RxDrops: 13, TxDrops: 14, Drops: 148})
	if v, ok := m["drops"]; !ok || v.(float64) != 148 {
		t.Fatalf("VPP 口径应发 drops=148：%v", m)
	}
	for _, k := range []string{"rx_drops", "tx_drops"} {
		if _, ok := m[k]; ok {
			t.Fatalf("VPP 口径不得发 %q（结构体里就算有值也不发——那是内核口径）：%v", k, m)
		}
	}
}

// 内核口径：发 rx_drops/tx_drops，不发 drops（两方向不相加冒充总数）。
func TestInterfaceCountersJSONKernelShape(t *testing.T) {
	m := decodeJSON(t, InterfaceCounters{Drops: 30, RxDrops: 7, TxDrops: 8, DirectionalDrops: true})
	if v, ok := m["rx_drops"]; !ok || v.(float64) != 7 {
		t.Fatalf("内核口径应发 rx_drops=7：%v", m)
	}
	if v, ok := m["tx_drops"]; !ok || v.(float64) != 8 {
		t.Fatalf("内核口径应发 tx_drops=8：%v", m)
	}
	if _, ok := m["drops"]; ok {
		t.Fatalf("内核口径不得发 drops（即使字段有值）：%v", m)
	}
}

// 0 是有效读数：两种口径下 0 都要发出去。
func TestInterfaceCountersJSONZeroIsReal(t *testing.T) {
	vpp := decodeJSON(t, InterfaceCounters{})
	if v, ok := vpp["drops"]; !ok || v.(float64) != 0 {
		t.Fatalf("VPP 口径 drops=0 应如实发出：%v", vpp)
	}
	kern := decodeJSON(t, InterfaceCounters{DirectionalDrops: true})
	for _, k := range []string{"rx_drops", "tx_drops"} {
		if v, ok := kern[k]; !ok || v.(float64) != 0 {
			t.Fatalf("内核口径 %s=0 应如实发出：%v", k, kern)
		}
	}
}

// NewCounters：内核数据面的聚合器——只有接口计数可取，其余（线程/buffer/主堆/运行态）
// 一律不可用（内核没有 VPP stats segment，读视图按数据面点名原因）。
func TestStateNewCountersOnlyServesInterfaceCounters(t *testing.T) {
	s := NewCounters(fakeCounters{ok: true, c: InterfaceCounters{RxDrops: 7, TxDrops: 8, DirectionalDrops: true}})
	c, ok := s.InterfaceCounters(context.Background(), "ens192")
	if !ok || c.RxDrops != 7 || c.TxDrops != 8 {
		t.Fatalf("内核聚合器应取到方向丢弃：%+v %v", c, ok)
	}
	if got := s.Threads(context.Background()); len(got) != 0 {
		t.Fatalf("内核聚合器不得伪造 VPP 线程：%+v", got)
	}
	if _, ok := s.Buffers(context.Background()); ok {
		t.Fatal("内核聚合器不得报 buffer 统计可用")
	}
	if _, ok := s.Memory(context.Background()); ok {
		t.Fatal("内核聚合器不得报内存统计可用")
	}
	if _, ok := s.RuntimeStats(context.Background()); ok {
		t.Fatal("内核聚合器不得报运行态可用")
	}
	// 计数源取不到时同样 ok=false（不回落、不编造 0）。
	if _, ok := NewCounters(fakeCounters{ok: false}).InterfaceCounters(context.Background(), "ens192"); ok {
		t.Fatal("计数源取不到时应 ok=false")
	}
	if _, ok := NewCounters(nil).InterfaceCounters(context.Background(), "ens192"); ok {
		t.Fatal("nil 计数源应 ok=false")
	}
}

// 两条数据面的来源互斥装配：同一个 State 只走一个来源，不会「VPP 有值用 VPP、否则回落内核」
// （那会把两种口径混进同一个读数）。
func TestStateCountersSourceIsExclusive(t *testing.T) {
	// 只装了 VPP 侧 Runtime：走 stats（Drops）。
	s := New(&fakeRuntime{})
	if c, ok := s.InterfaceCounters(context.Background(), "ens192"); !ok || c.RxPackets != 7 {
		t.Fatalf("VPP 侧应走 Runtime：%+v %v", c, ok)
	}
}
