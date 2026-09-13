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

// InterfaceCounters 单接口统计（契约 Interface.statistics）。
type InterfaceCounters struct {
	RxPackets uint64 `json:"rx_packets"`
	TxPackets uint64 `json:"tx_packets"`
	RxBytes   uint64 `json:"rx_bytes"`
	TxBytes   uint64 `json:"tx_bytes"`
	RxErrors  uint64 `json:"rx_errors"`
	TxErrors  uint64 `json:"tx_errors"`
	RxDrops   uint64 `json:"rx_drops"`
	TxDrops   uint64 `json:"tx_drops"`
}

// Buffers 数据面 buffer 池用量（每 NUMA/池）。
type Buffers struct {
	Pools []BufferPool `json:"pools"`
}

// BufferPool 单个 buffer 池。
type BufferPool struct {
	Name      string  `json:"name"`
	Used      float64 `json:"used"`
	Available float64 `json:"available"`
	Cached    float64 `json:"cached"`
}

// Memory 数据面内存占用（main heap 合计）。
type Memory struct {
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
	Free  uint64 `json:"free"`
}

// Runtime 运行态数据源。
type Runtime interface {
	Threads(ctx context.Context) ([]Thread, error)
	InterfaceCounters(ctx context.Context, ifname string) (InterfaceCounters, bool)
	Buffers(ctx context.Context) (Buffers, bool)
	Memory(ctx context.Context) (Memory, bool)
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

// InterfaceCounters 返回接口统计（不可用返回 ok=false）。
func (s *State) InterfaceCounters(ctx context.Context, ifname string) (InterfaceCounters, bool) {
	if s == nil || s.vpp == nil {
		return InterfaceCounters{}, false
	}
	return s.vpp.InterfaceCounters(ctx, ifname)
}

// Buffers 返回 buffer 池用量（不可用返回 ok=false）。
func (s *State) Buffers(ctx context.Context) (Buffers, bool) {
	if s == nil || s.vpp == nil {
		return Buffers{}, false
	}
	return s.vpp.Buffers(ctx)
}

// Memory 返回内存占用（不可用返回 ok=false）。
func (s *State) Memory(ctx context.Context) (Memory, bool) {
	if s == nil || s.vpp == nil {
		return Memory{}, false
	}
	return s.vpp.Memory(ctx)
}
