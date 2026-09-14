//go:build linux

package system

import "syscall"

// rootUsedPercent 根文件系统使用率（0-100）。
func rootUsedPercent(path string) (float64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	total := float64(st.Blocks) * float64(st.Bsize)
	if total <= 0 {
		return 0, false
	}
	free := float64(st.Bavail) * float64(st.Bsize)
	return (total - free) / total * 100, true
}
