package network

// 数据面代际变化的强制重建门控（R12-8）：
//   - 重放未发生（待办未清）时，对账**不清空** recovery 作用域告警（未收敛项保持可见）；
//   - 一次真正的重放（Satisfy）后才按 failures 覆盖；
//   - 「未重放」告警由 15s 巡检按待办建/消。

import (
	"testing"
)

func TestSyncRecoveryAlarmsKeepsAlarmsWhileReplayPending(t *testing.T) {
	store := NewAlarmStore()
	gate := NewReplayGate()
	// 上一代数据面留下的未收敛项。
	store.Raise(recoveryScope, SeverityWarning, AlarmUnconverged, "旧未收敛", "interfaces/ens192")
	gate.Require("数据面由 vpp 变为 kernel")

	// 未重放：即使 failures 为空也不得清空既有告警。
	SyncRecoveryAlarms(store, gate, nil)
	if got := activeRecoverySources(store); len(got) != 1 {
		t.Fatalf("重放未发生时不得清空未收敛项，实际：%v", got)
	}

	// 重放完成：按 failures 覆盖，不在 failures 的活动项消解。
	gate.Satisfy()
	SyncRecoveryAlarms(store, gate, nil)
	if got := activeRecoverySources(store); len(got) != 0 {
		t.Fatalf("重放完成后不在 failures 的告警应消解，实际：%v", got)
	}
}

// 无门控时行为与引入门控之前逐字一致（照旧 Sync）。
func TestSyncRecoveryAlarmsWithoutGateBehavesAsSync(t *testing.T) {
	store := NewAlarmStore()
	store.Raise(recoveryScope, SeverityWarning, AlarmUnconverged, "旧未收敛", "vrfs/v1")
	SyncRecoveryAlarms(store, nil, nil)
	if got := activeRecoverySources(store); len(got) != 0 {
		t.Fatalf("无门控时应照旧 Sync（消解不在 failures 的活动项），实际：%v", got)
	}
}

// 待办未清时仍上报本次 failures（新失败项要可见），只是不清空既有项。
func TestSyncRecoveryAlarmsStillRaisesNewFailuresWhilePending(t *testing.T) {
	store := NewAlarmStore()
	gate := NewReplayGate()
	store.Raise(recoveryScope, SeverityWarning, AlarmUnconverged, "旧未收敛", "interfaces/ens192")
	gate.Require("数据面由 kernel 变为 vpp")

	SyncRecoveryAlarms(store, gate, []Alarm{
		{Severity: SeverityError, Code: AlarmIfaceMissing, Message: "新缺失", Source: "interfaces/ens224"},
	})
	got := activeRecoverySources(store)
	if len(got) != 2 {
		t.Fatalf("既有项应保留、新失败项应上报，实际：%v", got)
	}
}

// 「未重放」告警：待办在 ⇒ Raise；待办清（重放完成）⇒ Resolve（幂等）。
func TestReplayGateAlarmRaiseAndResolve(t *testing.T) {
	store := NewAlarmStore()
	gate := NewReplayGate()
	gate.Require("数据面由 kernel 变为 vpp")

	ReplayGateAlarm(store, gate)
	if !replayRequiredActive(store) {
		t.Fatalf("待办未清应建 RECOVERY_REPLAY_REQUIRED 告警；活动：%+v", store.List("active"))
	}

	gate.Satisfy()
	ReplayGateAlarm(store, gate)
	if replayRequiredActive(store) {
		t.Fatal("重放完成后该告警应消解")
	}
}

func activeRecoverySources(store *AlarmStore) []string {
	var out []string
	for _, a := range store.ActiveOf(recoveryScope) {
		out = append(out, a.Code+"/"+a.Source)
	}
	return out
}

func replayRequiredActive(store *AlarmStore) bool {
	for _, a := range store.List("active") {
		if a.Code == AlarmReplayRequired {
			return true
		}
	}
	return false
}
