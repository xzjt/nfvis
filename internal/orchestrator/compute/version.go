package compute

import "fmt"

// FormatLibVersion 把 libvirt 的版本号编码换算为 "major.minor.release"。
// libvirt 约定：major*1_000_000 + minor*1_000 + release（如 12.0.0 → 12000000）。
// 纯函数，单测覆盖；供连接版本探测（M4-1）与真机版本比对使用。
func FormatLibVersion(v uint64) string {
	return fmt.Sprintf("%d.%d.%d", v/1_000_000, (v/1_000)%1_000, v%1_000)
}
