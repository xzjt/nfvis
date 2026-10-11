package network

// 数据面代际变化后的「强制全量重建」门控与「未重放」可见性。
//
// 数据面实现（vpp/kernel）是**配置 + 整机重启**才切换的；切换后配置未变，提交差分引擎给出
// 零计划，恢复重放若又被跳过，recovery 作用域的活动告警会被 Sync 的「不在 failures 即消解」
// 静默清空——未收敛项从 `show alarms` 消失，而数据面此刻与配置并不一致。
//
// 本门控把「必须先做一次全量重放」记成显式待办：
//   - Require：装配期检测到数据面代际变化时标记；
//   - Satisfy：一次真正的全量重放完成后清除；
//   - Pending：15s 巡检据此建 RECOVERY_REPLAY_REQUIRED 告警（未重放不再静默）。
//
// SyncRecoveryAlarms 在待办未清时**不清空** recovery 作用域告警（未收敛项保持可见）。

import (
	"strings"
	"sync"
)

// replayGateSource 未重放告警的固定 source（描述「代际重建」这件事本身，不随某个对象变化）。
const replayGateSource = "dataplane"

// ReplayGate 数据面代际变化的「强制全量重建」待办（并发安全）。
//
// 两数据面共用同一份实现：装配期注入（SetReplayGate），恢复收敛在真正重放时清除待办，
// 15s 巡检按待办建/消「未重放」告警。nil 安全：未注入时所有方法退化为「无待办」，
// 行为与引入本门控之前逐字一致（Sync 照旧覆盖）。
type ReplayGate struct {
	mu      sync.Mutex
	pending bool
	reason  string
}

// NewReplayGate 构造空门控（无待办）。
func NewReplayGate() *ReplayGate { return &ReplayGate{} }

// Require 标记「需要一次全量重放」并记录原因（幂等：重复标记覆盖原因）。
func (g *ReplayGate) Require(reason string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.pending, g.reason = true, reason
	g.mu.Unlock()
}

// Satisfy 清除待办（一次真正的全量重放完成后调用）。返回此前是否有待办。
func (g *ReplayGate) Satisfy() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	was := g.pending
	g.pending, g.reason = false, ""
	g.mu.Unlock()
	return was
}

// Pending 当前是否有未完成的全量重放待办及其原因（无门控 ⇒ false）。
func (g *ReplayGate) Pending() (bool, string) {
	if g == nil {
		return false, ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pending, g.reason
}

// SyncRecoveryAlarms 按门控覆盖 recovery 作用域告警：
//   - 无门控 / 无待办 ⇒ 照旧 Sync（不在 failures 的活动项消解）；
//   - 有待办（重放尚未发生）⇒ **不清空**，只把 failures 里的项按需 Raise/更新——未收敛项
//     保持可见，绝不被「未真正重放」的一次对账静默清空。
func SyncRecoveryAlarms(store *AlarmStore, gate *ReplayGate, failures []Alarm) {
	if store == nil {
		return
	}
	if pending, _ := gate.Pending(); pending {
		for _, f := range failures {
			store.Raise(recoveryScope, f.Severity, f.Code, f.Message, f.Source)
		}
		return
	}
	store.Sync(recoveryScope, failures)
}

// ReplayGateAlarm 把「是否仍有未完成的全量重放」落到告警表（15s 巡检调用）：待办在 ⇒ Raise
// RECOVERY_REPLAY_REQUIRED；已清（重放完成）⇒ Resolve（幂等）。reason 为空时给兜底文案。
func ReplayGateAlarm(store *AlarmStore, gate *ReplayGate) {
	if store == nil {
		return
	}
	pending, reason := gate.Pending()
	if !pending {
		store.Resolve(recoveryScope, AlarmReplayRequired, replayGateSource)
		return
	}
	if strings.TrimSpace(reason) == "" {
		reason = "数据面实现发生变化"
	}
	store.Raise(recoveryScope, SeverityWarning, AlarmReplayRequired,
		"数据面实现发生变化后尚未完成一次全量重建（"+reason+"）：配置声明可能未在新数据面上收敛，"+
			"数据面与配置可能不一致。产品会按既有节奏重试；该告警在重建完成后自动消解，"+
			"若长期不消解请重启 nfvis 服务触发启动重建",
		replayGateSource)
}
