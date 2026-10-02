package main

// 决策 #346：大页池巡检告警（HUGEPAGE_POOL_ORPHAN）的建/消守护。
//
// 无主占用页（在用 > 实际持有）由既有 60s 巡检按内核实况**重建**告警（不靠进程内记忆，
// 跨 nfvisd 重启仍可见）、收敛后**自动消解**；与 #329 的 HUGEPAGE_POOL_SURPLUS 同一对账
// 位置与口径，只是 scope 各自独立。这里用假告警表锁住建/消判定。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/system"
)

// fakeAlarmSink 记录 Raise/Resolve（不碰真实告警表）。
type fakeAlarmSink struct {
	raised   []string // "scope|code|message"
	resolved []string // "scope|code"
}

func (f *fakeAlarmSink) Raise(scope, severity, code, message, source string) {
	f.raised = append(f.raised, scope+"|"+code+"|"+message)
}

func (f *fakeAlarmSink) Resolve(scope, code, source string) bool {
	f.resolved = append(f.resolved, scope+"|"+code)
	return true
}

func hasRaise(s *fakeAlarmSink, prefix string) bool {
	for _, r := range s.raised {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

func hasResolve(s *fakeAlarmSink, v string) bool {
	for _, r := range s.resolved {
		if r == v {
			return true
		}
	}
	return false
}

func TestHugepageAlarmsOrphanRaised(t *testing.T) {
	// 进入对账时观测到无主占用（在用 2、持有 1）→ Raise HUGEPAGE_POOL_ORPHAN（独立 scope）。
	res := system.HugepageReconcileResult{Pools: []system.HugepagePoolResult{
		{PageSize: "1G", Declared: 2, ActualAfter: 2, InUse: 2, Held: 1, Orphan: 1},
	}}
	s := &fakeAlarmSink{}
	hugepageAlarms(s, res)

	if !hasRaise(s, "hugepages_orphan|"+system.HugepageOrphanAlarmCode+"|") {
		t.Fatalf("应 Raise HUGEPAGE_POOL_ORPHAN 于独立 scope：%+v", s.raised)
	}
	// 已收敛的池不该报 SURPLUS。
	if hasRaise(s, "hugepages|"+system.HugepageSurplusAlarmCode+"|") {
		t.Fatalf("已收敛时不该 Raise HUGEPAGE_POOL_SURPLUS：%+v", s.raised)
	}
	if !hasResolve(s, "hugepages|"+system.HugepageSurplusAlarmCode) {
		t.Fatalf("已收敛应 Resolve HUGEPAGE_POOL_SURPLUS：%+v", s.resolved)
	}
}

func TestHugepageAlarmsOrphanResolvedWhenGone(t *testing.T) {
	// 内核实况已无无主占用（在用 == 持有）→ 自动消解 ORPHAN（跨重启重建的收敛态）。
	res := system.HugepageReconcileResult{Pools: []system.HugepagePoolResult{
		{PageSize: "1G", Declared: 2, ActualAfter: 2, InUse: 2, Held: 2, Orphan: 0},
	}}
	s := &fakeAlarmSink{}
	hugepageAlarms(s, res)

	if hasRaise(s, "hugepages_orphan|") {
		t.Fatalf("无无主占用时不该 Raise ORPHAN：%+v", s.raised)
	}
	if !hasResolve(s, "hugepages_orphan|"+system.HugepageOrphanAlarmCode) {
		t.Fatalf("无无主占用时应 Resolve ORPHAN：%+v", s.resolved)
	}
}

func TestHugepageAlarmsSurplusStillRaised(t *testing.T) {
	// 未收敛（实际 768 > 声明 64，全在用且有持有者）→ Raise SURPLUS（#329 口径不回归）。
	res := system.HugepageReconcileResult{Pools: []system.HugepagePoolResult{
		{PageSize: "2M", Declared: 64, ActualAfter: 768, InUse: 768, Held: 768, Orphan: 0},
	}}
	s := &fakeAlarmSink{}
	hugepageAlarms(s, res)

	if !hasRaise(s, "hugepages|"+system.HugepageSurplusAlarmCode+"|") {
		t.Fatalf("未收敛时应 Raise HUGEPAGE_POOL_SURPLUS：%+v", s.raised)
	}
	if !hasResolve(s, "hugepages_orphan|"+system.HugepageOrphanAlarmCode) {
		t.Fatalf("无无主占用时应 Resolve ORPHAN：%+v", s.resolved)
	}
}

// 未托管/不可读的池不该被算作告警（与 Converged/Orphaned 口径一致）。
func TestHugepageAlarmsIgnoresUnmanagedUnreadable(t *testing.T) {
	res := system.HugepageReconcileResult{Pools: []system.HugepagePoolResult{
		{PageSize: "1G", Action: system.HugepageActionUnmanaged, ActualAfter: 4, Declared: 0, Held: -1, Orphan: -1},
		{PageSize: "2M", Action: system.HugepageActionUnreadable, ActualAfter: -1, Declared: 64, Held: -1, Orphan: -1},
	}}
	s := &fakeAlarmSink{}
	hugepageAlarms(s, res)

	if len(s.raised) != 0 {
		t.Fatalf("未托管/不可读不该建告警：%+v", s.raised)
	}
	if !hasResolve(s, "hugepages|"+system.HugepageSurplusAlarmCode) ||
		!hasResolve(s, "hugepages_orphan|"+system.HugepageOrphanAlarmCode) {
		t.Fatalf("应把两条告警都 Resolve：%+v", s.resolved)
	}
}
