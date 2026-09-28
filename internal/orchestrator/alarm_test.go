package orchestrator

// 对账清警（round86 缺陷 1）：ResolveStale 把 scope 内源已不在期望集合的活动告警清掉。

import "testing"

type stubSink struct {
	active   []AlarmRef
	resolved []string
}

func (s *stubSink) Raise(_, _, _, _, _ string) {}

func (s *stubSink) Resolve(_, code, source string) bool {
	for i, a := range s.active {
		if a.Code == code && a.Source == source {
			s.active = append(s.active[:i], s.active[i+1:]...)
			s.resolved = append(s.resolved, source)
			return true
		}
	}
	return false
}

func (s *stubSink) ActiveOf(string) []AlarmRef { return s.active }

func TestResolveStaleClearsSourcesOutsideExpect(t *testing.T) {
	s := &stubSink{active: []AlarmRef{
		{Code: "C1", Source: "keep"},
		{Code: "C2", Source: "gone"},
		{Code: "C3", Source: "gone2"},
	}}
	if n := ResolveStale(s, "scope", map[string]bool{"keep": true}); n != 2 {
		t.Fatalf("应清掉 2 条，实际 %d", n)
	}
	if len(s.active) != 1 || s.active[0].Source != "keep" {
		t.Fatalf("期望集合内的告警不应被动: %+v", s.active)
	}
	if n := ResolveStale(s, "scope", map[string]bool{"keep": true}); n != 0 {
		t.Fatalf("已清过的告警不应重复计数，实际 %d", n)
	}

	// 期望集合为空（对象全被删除）→ 清空全部
	s2 := &stubSink{active: []AlarmRef{{Code: "C1", Source: "a"}, {Code: "C1", Source: "b"}}}
	if n := ResolveStale(s2, "scope", nil); n != 2 || len(s2.active) != 0 {
		t.Fatalf("空期望集合应清空: n=%d active=%+v", n, s2.active)
	}

	// nil sink（未注入告警表）→ 安全空操作
	if n := ResolveStale(nil, "scope", nil); n != 0 {
		t.Fatalf("nil sink 应为空操作，实际 %d", n)
	}
}
