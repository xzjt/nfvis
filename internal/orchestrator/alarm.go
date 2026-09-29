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
	// SeverityError 数据面已与配置偏离（须人工处置）：提交补偿未完成留下的残渣。
	SeverityError = "error"
)

// 提交期（apply）残渣与延后处置的告警作用域/码。
//
// 由来（round86 真机登记 R86-9/R86-10，round87 收口）：
//   - **补偿未完成**：提交失败后逆序补偿，若某一步补偿也失败，残渣（新建的表滞留、已摘除的
//     地址/特性没被恢复）只出现在**当次提交输出**里，`show alarms` 与 Web 总览页都看不见
//     ——事后数据面已与配置不同，而没有任何一处记载这件事。
//   - **删表延后**：L3 交换机的 IP 表一旦被 NAT44 用过（VPP 在该表上留 `nat44-ei-hi` 引用），
//     `ip_table_add_del(del)` 会**返回 0 却删不掉**，只有数据面重启才释放该引用。此前同一提交里
//     「改 NAT 出接口 + 删旧 L3 交换机」必然整体失败并回滚；现按产品既有的「延后收敛」口径
//     （决策 #100/#186）处理：提交成功 + 告警留痕，表由数据面重启后的恢复收敛清理。
const (
	// CommitScope 提交期残渣/延后处置的告警作用域（与恢复收敛的 "recovery" 分开：后者用
	// Sync 对账，会把不在本轮失败清单里的项当成陈旧告警清掉）。
	CommitScope = "commit"
	// CommitCompensationFailed 提交失败后的补偿未完成：配置已回滚，数据面可能残留中间状态
	// （多出来的对象，或没被恢复地址/归属的对象）。重试同一提交、或按提示重启数据面后再提交
	// 可复原；同一对象的计划操作下一次成功执行时自动消警。
	CommitCompensationFailed = "COMMIT_COMPENSATION_FAILED"
	// CommitVrfDeleteDeferred L3 交换机的表在数据面仍存在（NAT 用过的表 VPP 不释放引用）；
	// 配置侧已删除，数据面清理由数据面重启后的恢复收敛完成，清理完成自动消警。
	CommitVrfDeleteDeferred = "COMMIT_VRF_DELETE_DEFERRED"
)

// CommitAlarmSink 提交期残渣/延后处置的告警落点（实现见 network.AlarmStore）。
// 只含提交编排用到的两个动作；恢复收敛的通用落点见 AlarmSink。
type CommitAlarmSink interface {
	Raise(scope, severity, code, message, source string)
	Resolve(scope, code, source string) bool
}

// VrfOpDesc 一台 L3 交换机的「建立/收敛」计划操作描述（告警 source 与计划操作文案同源）。
func VrfOpDesc(name string) string { return "vrf[" + name + "]" }

// VrfDeleteOpDesc 一台 L3 交换机的「删除」计划操作描述。
func VrfDeleteOpDesc(name string) string { return "del-vrf[" + name + "]" }
