//go:build !linux

package system

// rootUsedPercent 非 Linux 平台不提供 statfs 语义（本地开发/CI 返回不可用）。
func rootUsedPercent(path string) (float64, bool) { return 0, false }
