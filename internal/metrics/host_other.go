//go:build !linux

package metrics

// HostMetrics 非 Linux 平台无 /proc 与 statfs 语义，返回空（本地开发/CI 用）。
func HostMetrics() []Sample { return nil }
