//go:build !linux

package network

// 决策 #68：vpp_get_stats 回退源的非 Linux 桩（日常开发平台无 VPP 工具）。

// newDefaultStatsTool 非 Linux 平台无回退源（返回 nil，Buffers 报明确原因）。
func newDefaultStatsTool(string) StatsTool { return nil }
