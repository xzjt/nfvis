//go:build !linux

package systemd

// WatchHangup 非 Linux 平台空操作：这些平台没有「控制终端挂断发 SIGHUP」的语义
// （Windows 下 SIGHUP 甚至不是可投递的信号），故无需免疫。
// 见 hangup_linux.go 的说明（决策 #183）。
func WatchHangup(func()) (stop func()) { return func() {} }
