package orchestrator

// AlarmRef 一条活动告警的最小识别信息（scope 内**对账清警**用）。
// 只带 code/source：对账只需知道「哪些源仍在告警」，以便逐个 Resolve；
// 不带消息/时间，故本包无需依赖网络编排包的 Alarm 结构。
type AlarmRef struct {
	Code   string
	Source string
}

// AlarmSink 恢复收敛的告警落点（实现见 network.AlarmStore；M5 换持久化告警表）。
// 计算/容器编排经此上报不可收敛项，避免直接依赖网络编排包。
type AlarmSink interface {
	Raise(scope, severity, code, message, source string)
	Resolve(scope, code, source string) bool
	// ActiveOf 列出 scope 内当前活动告警的识别信息（对账清警用；查询失败/无表时应为空）。
	ActiveOf(scope string) []AlarmRef
}

// ResolveStale 对账清警：把 scope 内 source 不在 expect 中的活动告警逐个 Resolve，返回清掉的条数。
//
// 由来（真机 round86）：检查函数只遍历「当前配置里的对象」，对象一从配置里删除，循环再也看不到
// 它，其活动告警便永远无人 Resolve（show alarms active 长期挂着已删 VNF/容器的告警）。调用方在
// **查询运行态成功之后**调用本函数，即可让告警集合与配置期望集合对齐。
func ResolveStale(sink AlarmSink, scope string, expect map[string]bool) int {
	if sink == nil {
		return 0
	}
	n := 0
	for _, a := range sink.ActiveOf(scope) {
		if expect[a.Source] {
			continue
		}
		if sink.Resolve(scope, a.Code, a.Source) {
			n++
		}
	}
	return n
}

// 恢复收敛告警作用域/码（与 network 包同名常量取值一致）。
const (
	RecoveryScopeCompute   = "recovery-compute"
	RecoveryScopeContainer = "recovery-container"
	// RecoveryUnconverged 对象下发失败（可能暂时性），warning。
	RecoveryUnconverged = "RECOVERY_UNCONVERGED"
	// VM_Crashed VM 异常退出（QEMU crash/OOM），critical（FR-CMP-017）。
	VMCrashed = "VM_CRASHED"
	// ContainerExited 容器异常退出（dead 或非零退出码），critical（FR-CMP-022）。
	ContainerExited = "CONTAINER_EXITED"
)

// 告警严重级别（与 network.AlarmStore 取值一致）。
const (
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)
