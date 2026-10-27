package network

import "testing"

// 决策 #521：nil 管理器不得造出「会解引用它」的运行态数据源。
//
// 该缺陷只在 Linux 构建暴露：`stats_other.go`（非 Linux）的桩不碰 `r.m`，而
// `stats_linux.go` 的 `vppRuntime.InterfaceCounters` 会经 `r.m.statsRead(...)` 解引用，
// 于是「nil 管理器 + VPP 分支」在 Linux 上从「读不到」变成空指针崩溃（round48 的装配守护
// 因此自 v3.0.31 起让 CI 全红，本地 Windows 全绿）。本用例与平台无关地钉住返回空。
func TestManagerRuntimeNilManagerReturnsNoSource(t *testing.T) {
	var m *Manager
	if r := m.Runtime(); r != nil {
		t.Fatalf("nil 管理器应返回空数据源（避免 Linux 侧读法解引用崩溃），实得 %#v", r)
	}
}
