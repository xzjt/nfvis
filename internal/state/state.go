// Package state 提供 show/API 的运行态数据源（工程骨架 §2 internal/state）。
//
// M3-7：运行态聚合。数据来源藏在 Runtime 接口后（VPP 实现见
// internal/orchestrator/network，单测用假实现）；本包不 import 底座包。
package state

import "context"

// Thread 数据面线程（运行态，对应 VPP show_threads）。
type Thread struct {
	ID   uint32 `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Core uint32 `json:"core"` // 绑核（CPU）
}

// Runtime 运行态数据源。
type Runtime interface {
	Threads(ctx context.Context) ([]Thread, error)
}

// State 运行态聚合器（Runtime 可为 nil，方法安全返回空）。
type State struct{ vpp Runtime }

// New 构造聚合器。
func New(vpp Runtime) *State { return &State{vpp: vpp} }

// Threads 返回数据面线程（未接入/不可用时返回 nil 而非错误）。
func (s *State) Threads(ctx context.Context) []Thread {
	if s == nil || s.vpp == nil {
		return nil
	}
	rows, err := s.vpp.Threads(ctx)
	if err != nil {
		return nil
	}
	return rows
}
