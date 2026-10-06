//go:build linux

package network

// M3-7（二）[linux]：VPP stats segment 接入（接口统计 / buffer / 内存）。
// 运行时数据供 internal/state 聚合；govpp 依赖集中在本文件，与其它 *_govpp.go
// 一样由真机集成测试覆盖，不入本地覆盖率门槛。
//
// 连接策略（R84-3）：stats segment 的连接**会随 VPP 重启而失效**，取数失败即丢弃重连
// （见 connCache，vpp_govpp.go）。修复前连接只建一次，VPP 重启后统计永久不可用。

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"go.fd.io/govpp/adapter/statsclient"
	"go.fd.io/govpp/api"
	"go.fd.io/govpp/core"

	"github.com/xzjt/nfvis/internal/state"
)

// statsConn stats segment 连接缓存（同目录 stats.sock）：
// 惰性建立 + 「取数失败即失效重连」，句柄与策略见 vpp_govpp.go 的 connCache。
type statsConn = connCache[*core.StatsConnection]

// StatsSocketFor 由 binary API 套接字推导 stats 套接字。
func StatsSocketFor(apiSock string) string {
	if apiSock == "" {
		return "/run/vpp/stats.sock"
	}
	return filepath.Join(filepath.Dir(apiSock), "stats.sock")
}

// statsConnRef 返回连接缓存（惰性初始化一次，statsOnce 保证并发下只建一个）。
func (m *Manager) statsConnRef() *statsConn {
	m.statsOnce.Do(func() {
		m.statsConn = newConnCache(m.connectStats, closeStatsConn)
	})
	return m.statsConn
}

// connectStats 建立到 stats segment 的连接。
func (m *Manager) connectStats() (*core.StatsConnection, error) {
	sock := StatsSocketFor(m.cfg.Socket)
	conn, err := core.ConnectStats(statsclient.NewStatsClient(sock))
	if err != nil {
		return nil, fmt.Errorf("连接 stats segment %s: %w", sock, err)
	}
	return conn, nil
}

// closeStatsConn 关闭被丢弃的陈旧连接（关闭失败无补救动作：它已不再被复用）。
func closeStatsConn(conn *core.StatsConnection) {
	if conn != nil {
		conn.Disconnect()
	}
}

// statsReadTimeout 单次 stats 取数的硬上界（决策 #362）：stats segment 读取
// （govpp statsclient）没有自己的超时，对「VPP 楔死/段不可用」会永久阻塞采集。
// 取 2s：正常取数是本地毫秒级；一次 statsRead 内至多两段执行（陈旧失败后的重连
// 重试各一段），合计 ≤4s，不超过采样侧的单轮采集上界 metricsGatherTimeout（5s，
// cmd/nfvisd/metrics_history.go）——采集上界 ≥ 取数上界，采样线程不会被本层拖穿。
const statsReadTimeout = 2 * time.Second

// statsRead 在 stats 连接上取一次数：连接陈旧（典型是 VPP 刚重启）时由缓存失效重连后重试，
// 重连后仍失败才报错——调用方据此降级为「统计不可用」。
//
// 单次执行另受 statsReadTimeout 硬上界（决策 #362）：到期即关闭并丢弃挂起连接、
// 返回可辨识超时错误（上界路径不重连重试），下一次调用重建连接。
func (m *Manager) statsRead(read func(*core.StatsConnection) error) error {
	return m.statsConnRef().useBounded(statsReadTimeout, read)
}

func (r *vppRuntime) InterfaceCounters(ctx context.Context, ifname string) (state.InterfaceCounters, bool) {
	var all api.InterfaceStats
	if err := r.m.statsRead(func(conn *core.StatsConnection) error {
		all = api.InterfaceStats{} // 重试前清空，避免新旧两次取数的条目混在一起
		return conn.GetInterfaceStats(&all)
	}); err != nil {
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
	var bs api.BufferStats
	if err := r.m.statsRead(func(conn *core.StatsConnection) error {
		bs = api.BufferStats{}
		return conn.GetBufferStats(&bs)
	}); err != nil {
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

// RuntimeStats 返回线程级运行态（决策 #200）。
//
// 走同版本工具 vpp_get_stats 而不是 statsclient：本命令要的 `/sys/*_per_worker` 是
// **组合计数**，正是 statsclient 在 VPP 26.06 上解不出的那类值类型（见 Buffers 的注释）；
// 工具的解码与 VPP 同版本，实测 /sys/loops_per_worker 与 `vppctl show runtime` 各线程
// loops/sec 量级吻合。按节点明细无结构化来源，故不在此列（见 runtime_stats.go 的口径说明）。
func (r *vppRuntime) RuntimeStats(ctx context.Context) (state.RuntimeStats, bool) {
	tool := r.m.statsTool
	if tool == nil {
		return state.RuntimeStats{Reason: "无同版本统计工具回退源（vpp_get_stats）"}, false
	}
	text, err := tool.DumpMachine(ctx, runtimeSysPattern)
	if err != nil {
		return state.RuntimeStats{Reason: err.Error()}, false
	}
	out, ok := RuntimeStatsFromDump(text, time.Now())
	if !ok {
		return state.RuntimeStats{Reason: "vpp_get_stats 未返回 /sys 运行态计数"}, false
	}
	return out, true
}

// StormCounters 读某 policer 的计数（决策 #385；StormCountersReader 的真机实现）。
// 走同版本工具 vpp_get_stats 的组合计数（policer 计数不在 binary API 里）；读不到时
// 返回可读的原因串（调用方在 CLI/读视图里如实显示，不编造数字）。
func (m *Manager) StormCounters(ctx context.Context, policerIndex uint32, policerName string) (StormCounters, bool, string) {
	tool := m.statsTool
	if tool == nil {
		return StormCounters{}, false, "无同版本统计工具回退源（vpp_get_stats）"
	}
	text, err := tool.DumpMachine(ctx, stormPolicerStatsPattern)
	if err != nil {
		return StormCounters{}, false, err.Error()
	}
	c, ok := StormCountersFromDump(text, policerIndex, policerName)
	if !ok {
		return StormCounters{}, false, "stats segment 未返回该 policer 的计数路径（" + stormPolicerStatsPattern + "）"
	}
	return c, true, ""
}

func (r *vppRuntime) Memory(ctx context.Context) (state.Memory, bool) {
	var ms api.MemoryStats
	if err := r.m.statsRead(func(conn *core.StatsConnection) error {
		ms = api.MemoryStats{}
		return conn.GetMemoryStats(&ms)
	}); err != nil {
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
