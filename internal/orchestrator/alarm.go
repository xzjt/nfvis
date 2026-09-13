package orchestrator

// AlarmSink 恢复收敛的告警落点（实现见 network.AlarmStore；M5 换持久化告警表）。
// 计算/容器编排经此上报不可收敛项，避免直接依赖网络编排包。
type AlarmSink interface {
	Raise(scope, severity, code, message, source string)
	Resolve(scope, code, source string) bool
}

// 恢复收敛告警作用域/码（与 network 包同名常量取值一致）。
const (
	RecoveryScopeCompute   = "recovery-compute"
	RecoveryScopeContainer = "recovery-container"
	// RecoveryUnconverged 对象下发失败（可能暂时性），warning。
	RecoveryUnconverged = "RECOVERY_UNCONVERGED"
)
