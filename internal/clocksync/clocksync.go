// Package clocksync 提供 NFR-006「事件时间戳依赖 NTP，未同步时带未同步标记」的
// **三态标记单一事实源**。
//
// 审计（internal/config）与告警（internal/orchestrator/network）都要在**事件产生的那一刻**
// 记下宿主时钟是否可信：
//
//	true  —— 已与 NTP 同步
//	false —— 未同步（时间戳可能不准）
//	nil   —— 未知（**没有探针可问**）
//
// nil 只在「探针未接入」时出现，而不是把未知当成已同步——宁可疑，也不让来源不明的时钟
// 看起来是同步的。两处共用 Mark，避免同一口径各写一份而漂移（本包的由来）。
package clocksync

// Mark 由探针计算三态标记：probe 为 nil ⇒ nil（未知，不谎称已同步）；否则取探针结论的地址。
func Mark(probe func() bool) *bool {
	if probe == nil {
		return nil
	}
	synced := probe()
	return &synced
}
