//go:build !linux

package systemd

import "time"

// Notify 非 Linux 平台空操作（本地开发/CI 用）。
func Notify(state string) error { return nil }

// WatchdogInterval 非 Linux 平台未启用看门狗。
func WatchdogInterval() (time.Duration, bool) { return 0, false }
