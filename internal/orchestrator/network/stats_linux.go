//go:build linux

package network

// M3-7（二）[linux]：VPP stats segment 接入（接口统计 / buffer / 内存）。
// 运行时数据供 internal/state 聚合；govpp 依赖集中在本文件，与其它 *_govpp.go
// 一样由真机集成测试覆盖，不入本地覆盖率门槛。

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"go.fd.io/govpp/adapter/statsclient"
	"go.fd.io/govpp/api"
	"go.fd.io/govpp/core"

	"github.com/xzjt/nfvis/internal/state"
)

// statsConn 惰性建立的 stats segment 连接（同目录 stats.sock）。
type statsConn struct {
	mu   sync.Mutex
	conn *core.StatsConnection
}

// StatsSocketFor 由 binary API 套接字推导 stats 套接字。
func StatsSocketFor(apiSock string) string {
	if apiSock == "" {
		return "/run/vpp/stats.sock"
	}
	return filepath.Join(filepath.Dir(apiSock), "stats.sock")
}

func (m *Manager) stats() (*core.StatsConnection, error) {
	m.statsOnce.Do(func() { m.statsConn = &statsConn{} })
	m.statsConn.mu.Lock()
	defer m.statsConn.mu.Unlock()
	if m.statsConn.conn != nil {
		return m.statsConn.conn, nil
	}
	sock := StatsSocketFor(m.cfg.Socket)
	conn, err := core.ConnectStats(statsclient.NewStatsClient(sock))
	if err != nil {
		return nil, fmt.Errorf("连接 stats segment %s: %w", sock, err)
	}
	m.statsConn.conn = conn
	return conn, nil
}

// statsReset 置空缓存的 stats 连接，下次访问惰性重连。
//
// 连接不能永久缓存：VPP 重启会生成新的 stats socket 与共享内存段，
// 旧连接此后读到的是陈旧段（计数冻结/归零）或直接报错——读取失败时
// 必须置空重建，否则接口计数与 /metrics 会长期答非所问。
func (m *Manager) statsReset() {
	m.statsOnce.Do(func() { m.statsConn = &statsConn{} })
	m.statsConn.mu.Lock()
	defer m.statsConn.mu.Unlock()
	if m.statsConn.conn != nil {
		m.statsConn.conn.Disconnect()
		m.statsConn.conn = nil
	}
}

func (r *vppRuntime) InterfaceCounters(ctx context.Context, ifname string) (state.InterfaceCounters, bool) {
	conn, err := r.m.stats()
	if err != nil {
		return state.InterfaceCounters{}, false
	}
	var all api.InterfaceStats
	if err := conn.GetInterfaceStats(&all); err != nil {
		r.m.statsReset() // VPP 重启后旧段失效，置空待重连
		return state.InterfaceCounters{}, false
	}
	idx, ok := r.swIfIndex(ifname)
	if !ok {
		return state.InterfaceCounters{}, false
	}
	for _, c := range all.Interfaces {
		if uint32(c.InterfaceIndex) != idx && c.InterfaceName != ifname {
			continue
		}
		return state.InterfaceCounters{
			RxPackets: c.Rx.Packets, TxPackets: c.Tx.Packets,
			RxBytes: c.Rx.Bytes, TxBytes: c.Tx.Bytes,
			RxErrors: c.RxErrors, TxErrors: c.TxErrors,
		}, true
	}
	return state.InterfaceCounters{}, false
}

// Buffers 返回 buffer 池用量（决策 #68）。
//
// 先经 statsclient 解码；失败或全零（VPP 26.06 的值类型 v0.13.0 解码不出）时
// 回退到同版本工具 vpp_get_stats。任一来源成功均标注 Source；
// 全部不可用时给出 Reason，由调用方呈现（不静默省略）。
func (r *vppRuntime) Buffers(ctx context.Context) (state.Buffers, bool) {
	if out, ok := r.buffersViaStatsClient(); ok {
		out.Source = state.StatsSourceClient
		return out, true
	}
	tool := r.m.statsTool
	if tool == nil {
		return state.Buffers{Reason: "statsclient 解码失败且无同版本工具回退源"}, false
	}
	text, err := tool.DumpMachine(ctx, bufferPoolPattern)
	if err != nil {
		return state.Buffers{Reason: err.Error()}, false
	}
	if pools, ok := BufferPoolsFromDump(text); ok {
		return state.Buffers{Pools: pools, Source: state.StatsSourceTool}, true
	}
	return state.Buffers{Reason: "vpp_get_stats 未返回 buffer 池条目"}, false
}

// buffersViaStatsClient 经 govpp statsclient 读取 buffer 池。
func (r *vppRuntime) buffersViaStatsClient() (state.Buffers, bool) {
	conn, err := r.m.stats()
	if err != nil {
		return state.Buffers{}, false
	}
	var bs api.BufferStats
	if err := conn.GetBufferStats(&bs); err != nil {
		r.m.statsReset() // VPP 重启后旧段失效，置空待重连
		return state.Buffers{}, false
	}
	out := state.Buffers{}
	for name, p := range bs.Buffer {
		// VPP 26.06 的 buffer 统计路径与 govpp 期望的 /buffer-pools/<pool>/{used,available}
		// 不完全一致时可能只解析出池名、计数为 0；全零视为未解析，宁缺勿错。
		if p.Used == 0 && p.Available == 0 && p.Cached == 0 {
			continue
		}
		out.Pools = append(out.Pools, state.BufferPool{Name: name, Used: p.Used, Available: p.Available, Cached: p.Cached})
	}
	return out, len(out.Pools) > 0
}

func (r *vppRuntime) Memory(ctx context.Context) (state.Memory, bool) {
	conn, err := r.m.stats()
	if err != nil {
		return state.Memory{}, false
	}
	var ms api.MemoryStats
	if err := conn.GetMemoryStats(&ms); err != nil {
		r.m.statsReset() // VPP 重启后旧段失效，置空待重连
		return state.Memory{}, false
	}
	var out state.Memory
	for _, c := range ms.Main {
		out.Total += c.Total
		out.Used += c.Used
		out.Free += c.Free
	}
	if out.Total == 0 && ms.Total > 0 { // 回退旧字段
		out.Total = uint64(ms.Total)
		out.Used = uint64(ms.Used)
	}
	return out, out.Total > 0
}

// swIfIndex 解析接口名（复用 binary API dump）。
func (r *vppRuntime) swIfIndex(ifname string) (uint32, bool) {
	ch, err := r.m.APIChannel()
	if err != nil {
		return 0, false
	}
	defer ch.Close()
	idx, ok, err := (&govppL3Client{ch: ch}).SwInterfaceIndex(ifname)
	if err != nil {
		return 0, false
	}
	return idx, ok
}
