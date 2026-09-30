package compute

import "strconv"

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
const (
	shutoffReasonCrashed   = 3 // VIR_DOMAIN_SHUTOFF_CRASHED
	shutoffReasonFailed    = 6 // VIR_DOMAIN_SHUTOFF_FAILED
	shutoffReasonDestroyed = 2 // VIR_DOMAIN_SHUTOFF_DESTROYED（被 destroy）
)

// libvirt paused reason 常量（virDomainPausedReason，实装 libvirt 12）：
// 启动失败最典型的是 STARTING_UP（QEMU 等 vhost-user 后端就绪而暂停）。
const (
	pausedReasonIoerror    = 5  // VIR_DOMAIN_PAUSED_IOERROR
	pausedReasonWatchdog   = 6  // VIR_DOMAIN_PAUSED_WATCHDOG
	pausedReasonCrashed    = 10 // VIR_DOMAIN_PAUSED_CRASHED
	pausedReasonStartingUp = 11 // VIR_DOMAIN_PAUSED_STARTING_UP
)

// VMStateFromLibvirtReason 带 reason 的状态映射（FR-CMP-017）：
// 外部 kill QEMU 时 libvirt 报 SHUTOFF+CRASHED（on_crash=preserve 之外的路径），
// 应映射为契约的 crashed 而非 shutoff，才能触发 critical 告警。
func VMStateFromLibvirtReason(state, reason int) string {
	if (state == domShutoff || state == domShutdown) && reason == shutoffReasonCrashed {
		return "crashed"
	}
	return VMStateFromLibvirt(state)
}

// StateReasonText 把 libvirt 的 state+reason 映射为可读短句（决策 #311），如
// `paused (starting up)`、`crashed`、`shutoff (failed)`。
//
// 只报**事实**（libvirt 给的 state/reason），不臆测原因；未知 reason 如实给出编号
// （`paused (reason 12)`），不编造短语。
func StateReasonText(state, reason int) string {
	base := VMStateFromLibvirtReason(state, reason)
	switch state {
	case domPaused, domPMSuspended:
		if s := pausedReasonText(reason); s != "" {
			return base + " (" + s + ")"
		}
		return base + " (reason " + strconv.Itoa(reason) + ")"
	case domShutoff, domShutdown:
		// base 已是 crashed（reason=crashed）时不再重复括注。
		if base == "shutoff" {
			switch reason {
			case shutoffReasonDestroyed:
				return base + " (destroyed)"
			case shutoffReasonFailed:
				return base + " (failed)"
			}
		}
	}
	return base
}

// pausedReasonText 已知 paused reason → 可读短语（未知返回空串，由调用方给编号）。
func pausedReasonText(reason int) string {
	switch reason {
	case 1:
		return "user"
	case 5:
		return "I/O error"
	case 6:
		return "watchdog"
	case 10:
		return "crashed"
	case 11:
		return "starting up"
	default:
		return ""
	}
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
