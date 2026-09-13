package compute

// libvirt 域状态常量（virDomainState，与实装 libvirt 12.0.0 一致）。
// 参考 libvirt domain enums；映射到契约 VMFunction.state 枚举。
const (
	domNoState     = 0
	domRunning     = 1
	domBlocked     = 2
	domPaused      = 3
	domShutdown    = 4
	domShutoff     = 5
	domCrashed     = 6
	domPMSuspended = 7
)

// VMStateFromLibvirt 把 libvirt 状态映射为契约 state 枚举
// （running/shutoff/crashed/paused，见 OpenAPI VMFunction.state）。
// BLOCKED（等待 I/O）在语义上仍是运行中；SHUTDOWN（正在关机）视为 shutoff。
func VMStateFromLibvirt(state int) string {
	switch state {
	case domRunning, domBlocked:
		return "running"
	case domPaused, domPMSuspended:
		return "paused"
	case domCrashed:
		return "crashed"
	default: // NOSTATE / SHUTDOWN / SHUTOFF（含未知值，保守归 shutoff）
		return "shutoff"
	}
}

// libvirt shutoff reason 常量（virDomainShutoffReason）。
const shutoffReasonCrashed = 3 // VIR_DOMAIN_SHUTOFF_CRASHED

// VMStateFromLibvirtReason 带 reason 的状态映射（FR-CMP-017）：
// 外部 kill QEMU 时 libvirt 报 SHUTOFF+CRASHED（on_crash=preserve 之外的路径），
// 应映射为契约的 crashed 而非 shutoff，才能触发 critical 告警。
func VMStateFromLibvirtReason(state, reason int) string {
	if (state == domShutoff || state == domShutdown) && reason == shutoffReasonCrashed {
		return "crashed"
	}
	return VMStateFromLibvirt(state)
}

// isActiveState libvirt 语义下域是否处于活动状态（需先 destroy 才能 undefine）。
func isActiveState(state int) bool {
	switch state {
	case domRunning, domBlocked, domPaused, domPMSuspended:
		return true
	default:
		return false
	}
}
