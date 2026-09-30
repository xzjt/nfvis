package network

// 决策 #307：告警侧补 `time_synced` 三态字段（对齐审计侧，单一事实源 clocksync.Mark）。
//
// 三态：true 已同步 / false 未同步 / nil 未知（探针未注入）。重新激活（Raise/Sync 命中已
// resolved 项）会刷新 raised_at，故按**同一时刻的事实**重打标，不沿用旧值。

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/config"
)

// TestAlarmTimeSyncedThreeStates：探针 true/false/未注入 → 告警标记 true/false/nil。
func TestAlarmTimeSyncedThreeStates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe func() bool
		want  *bool
	}{
		{"未注入探针保持未知", nil, nil},
		{"已同步", func() bool { return true }, boolPtr(true)},
		{"未同步", func() bool { return false }, boolPtr(false)},
	} {
		s := NewAlarmStore()
		s.SetClockProbe(tc.probe)
		s.Raise(recoveryScope, SeverityWarning, AlarmUnconverged, "m", "interfaces/ens192")
		got := s.List(AlarmActive)[0].TimeSynced
		assertMark(t, tc.name, got, tc.want)
	}
}

// TestAlarmSyncStampsTimeSynced：Sync 路径（恢复收敛）同样打标。
func TestAlarmSyncStampsTimeSynced(t *testing.T) {
	s := NewAlarmStore()
	s.SetClockProbe(func() bool { return false })
	s.Sync(recoveryScope, []Alarm{{Severity: SeverityError, Code: AlarmIfaceMissing, Message: "m", Source: "a"}})
	assertMark(t, "Sync 新建", s.List(AlarmActive)[0].TimeSynced, boolPtr(false))
}

// TestAlarmReactivateRestampsTimeSynced：重新激活按当时探针重打标，不沿用旧值。
func TestAlarmReactivateRestampsTimeSynced(t *testing.T) {
	s := NewAlarmStore()
	synced := false
	s.SetClockProbe(func() bool { return synced })
	s.Raise(recoveryScope, SeverityWarning, AlarmUnconverged, "首次", "src")
	assertMark(t, "首次未同步", s.List(AlarmActive)[0].TimeSynced, boolPtr(false))

	if !s.Resolve(recoveryScope, AlarmUnconverged, "src") {
		t.Fatal("Resolve 应命中")
	}
	synced = true
	s.Raise(recoveryScope, SeverityWarning, AlarmUnconverged, "再次", "src")
	assertMark(t, "重新激活应为已同步", s.List(AlarmActive)[0].TimeSynced, boolPtr(true))
}

// TestTimeSyncedSingleSourceAcrossAuditAndAlarm：审计与告警对**同一探针取值**得出相同标记，
// 且 nil 探针两侧同为未知——证明两处走的是同一个 clocksync.Mark（决策 #307 的单一事实源）。
func TestTimeSyncedSingleSourceAcrossAuditAndAlarm(t *testing.T) {
	checkBoth := func(t *testing.T, probe func() bool, want *bool) {
		t.Helper()
		// 告警侧
		alarms := NewAlarmStore()
		alarms.SetClockProbe(probe)
		alarms.Raise(recoveryScope, SeverityWarning, AlarmUnconverged, "m", "src")
		alarmMark := alarms.List(AlarmActive)[0].TimeSynced
		assertMark(t, "告警侧", alarmMark, want)

		// 审计侧（同一探针）
		store, err := config.OpenStore(filepath.Join(t.TempDir(), "nfvis.db"))
		if err != nil {
			t.Fatalf("OpenStore: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		eng, err := config.NewEngine(store, nil, config.Options{Now: time.Now, TimeSynced: probe})
		if err != nil {
			t.Fatalf("NewEngine: %v", err)
		}
		t.Cleanup(eng.Close)
		eng.Audit("admin", "act", "detail", "success")
		entries, err := store.ListAudit(1, 0)
		if err != nil || len(entries) != 1 {
			t.Fatalf("ListAudit: %v %d", err, len(entries))
		}
		auditMark := entries[0].TimeSynced
		assertMark(t, "审计侧", auditMark, want)

		// 两侧同结论
		if (alarmMark == nil) != (auditMark == nil) || (alarmMark != nil && *alarmMark != *auditMark) {
			t.Errorf("审计与告警标记不一致：audit=%v alarm=%v", fmtMark(auditMark), fmtMark(alarmMark))
		}
	}
	checkBoth(t, func() bool { return true }, boolPtr(true))
	checkBoth(t, func() bool { return false }, boolPtr(false))
	checkBoth(t, nil, nil)
}

func boolPtr(b bool) *bool { return &b }

func assertMark(t *testing.T, who string, got, want *bool) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("%s：应为未知(nil)，实得 %v", who, *got)
	case want != nil && got == nil:
		t.Errorf("%s：应为 %v，实得未知(nil)", who, *want)
	case want != nil && got != nil && *got != *want:
		t.Errorf("%s：应为 %v，实得 %v", who, *want, *got)
	}
}

func fmtMark(m *bool) string {
	if m == nil {
		return "未知(nil)"
	}
	if *m {
		return "已同步(true)"
	}
	return "未同步(false)"
}
