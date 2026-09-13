package network

// M3-8：告警表单测（FR-OPS-010）。

import (
	"testing"
	"time"
)

func fixedClock(t *testing.T) (*AlarmStore, func(time.Duration)) {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cur := base
	s := NewAlarmStore()
	s.now = func() time.Time { return cur }
	return s, func(d time.Duration) { cur = cur.Add(d) }
}

func TestAlarmRaiseDedupAndUpdate(t *testing.T) {
	s, _ := fixedClock(t)
	s.Raise(recoveryScope, SeverityWarning, AlarmUnconverged, "第一次失败", "interfaces/ens192")
	s.Raise(recoveryScope, SeverityError, AlarmUnconverged, "第二次失败", "interfaces/ens192")

	all := s.List("all")
	if len(all) != 1 {
		t.Fatalf("同 source+code 应幂等为一条，实际 %d", len(all))
	}
	if all[0].Message != "第二次失败" || all[0].Severity != SeverityError {
		t.Fatalf("重复 Raise 应更新消息/级别: %+v", all[0])
	}
	if all[0].State != AlarmActive || all[0].ResolvedAt != nil {
		t.Fatalf("应为活动告警: %+v", all[0])
	}
}

func TestAlarmStatesFilter(t *testing.T) {
	s, _ := fixedClock(t)
	s.Raise(recoveryScope, SeverityWarning, AlarmUnconverged, "a", "vrfs/v1")
	s.Raise(recoveryScope, SeverityWarning, AlarmUnconverged, "b", "vrfs/v2")
	if !s.Resolve(recoveryScope, AlarmUnconverged, "vrfs/v1") {
		t.Fatal("Resolve 应命中")
	}
	if s.Resolve(recoveryScope, AlarmUnconverged, "vrfs/v1") {
		t.Fatal("重复 Resolve 应返回 false")
	}
	if got := len(s.List("")); got != 1 { // 缺省 active
		t.Fatalf("active 应 1 条，实际 %d", got)
	}
	if got := len(s.List("resolved")); got != 1 {
		t.Fatalf("resolved 应 1 条，实际 %d", got)
	}
	if got := len(s.List("all")); got != 2 {
		t.Fatalf("all 应 2 条，实际 %d", got)
	}
	if r := s.List("resolved")[0]; r.Source != "vrfs/v1" || r.ResolvedAt == nil {
		t.Fatalf("resolved 记录不符: %+v", r)
	}
}

func TestAlarmSyncResolvesAndReactivates(t *testing.T) {
	s, _ := fixedClock(t)
	f := func(source string) Alarm {
		return Alarm{Severity: SeverityError, Code: AlarmIfaceMissing, Message: "接口缺失", Source: source}
	}
	s.Sync(recoveryScope, []Alarm{f("a"), f("b")})
	if got := len(s.List(AlarmActive)); got != 2 {
		t.Fatalf("首次 Sync 应 2 条活动，实际 %d", got)
	}
	s.Sync(recoveryScope, []Alarm{f("a")}) // b 已恢复
	if got := len(s.List(AlarmActive)); got != 1 {
		t.Fatalf("Sync 后应仅 1 条活动，实际 %d", got)
	}
	if got := len(s.List(AlarmResolved)); got != 1 {
		t.Fatalf("应 1 条 resolved，实际 %d", got)
	}

	// a 再次失败：应复用同一告警（ID 不变）并重新激活
	before := s.List(AlarmResolved)
	s.Sync(recoveryScope, []Alarm{f("a")})
	after := s.List(AlarmActive)
	if len(after) != 1 || after[0].Source != "a" || after[0].State != AlarmActive || after[0].ResolvedAt != nil {
		t.Fatalf("重新激活不符: %+v", after)
	}
	if len(before) != 1 {
		t.Fatalf("前置状态异常")
	}
}

// Sync 只收敛本作用域，不影响其它来源的告警。
func TestAlarmSyncScoped(t *testing.T) {
	s, _ := fixedClock(t)
	s.Raise("other", SeverityWarning, "OTHER", "别的告警", "x")
	s.Sync(recoveryScope, nil)
	if got := len(s.List(AlarmActive)); got != 1 {
		t.Fatalf("其它作用域告警不应被 Sync 收敛，实际活动 %d", got)
	}
}
