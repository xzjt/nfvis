// Package state 提供 show/API 的运行态数据源（工程骨架 §2 internal/state）。
//
// M3-7：运行态聚合。数据来源藏在 Runtime 接口后（VPP 实现见
// internal/orchestrator/network，单测用假实现）；本包不 import 底座包。
package state

import (
	"context"
	"encoding/json"
)

// Thread 数据面线程（运行态，对应 VPP show_threads）。
type Thread struct {
	ID   uint32 `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Core uint32 `json:"core"` // 绑核（CPU）
}

// InterfaceCounters 单接口统计（契约 Interface.statistics）。
//
// 丢弃计数有两种**不可互相换算**的口径（决策 #504），故三个字段并存、由数据面决定发哪些：
//
//   - Drops：方向无关的丢弃总数，**VPP 数据面**提供（stats segment 的
//     `/interfaces/<if>/drops` 本身不区分方向，摊给 rx 或 tx 都属编造）；
//   - RxDrops/TxDrops：按方向的丢弃，**仅内核网络数据面**提供（内核 sysfs 有
//     `statistics/rx_dropped` 与 `tx_dropped` 两个真值）。
//
// DirectionalDrops 标记本组计数走哪种口径，JSON 出口据此**只发适用者**（VPP 发 `drops`、
// 内核发 `rx_drops`+`tx_drops`）；不适用的键**缺席**而不是发 0——宁缺不谎报。
type InterfaceCounters struct {
	RxPackets uint64 `json:"rx_packets"`
	TxPackets uint64 `json:"tx_packets"`
	RxBytes   uint64 `json:"rx_bytes"`
	TxBytes   uint64 `json:"tx_bytes"`
	RxErrors  uint64 `json:"rx_errors"`
	TxErrors  uint64 `json:"tx_errors"`
	Drops     uint64 `json:"drops"`
	RxDrops   uint64 `json:"rx_drops"`
	TxDrops   uint64 `json:"tx_drops"`

	// DirectionalDrops 丢弃计数是否**按方向**提供（内核网络数据面 true；VPP 数据面 false，
	// 它只有方向无关的总数）。不参与 JSON：出口形态由 MarshalJSON 按它决定。
	DirectionalDrops bool `json:"-"`
}

// MarshalJSON 按丢弃口径只发适用者（决策 #504）：内核数据面发 `rx_drops`/`tx_drops`、
// **不发** `drops`（把两个方向相加冒充总数会让同一事实有两种口径）；VPP 数据面反过来。
// 其余计数两口径同形。0 是有效读数，故用指针 + omitempty 表达「缺席」，不靠零值。
func (c InterfaceCounters) MarshalJSON() ([]byte, error) {
	out := struct {
		RxPackets uint64  `json:"rx_packets"`
		TxPackets uint64  `json:"tx_packets"`
		RxBytes   uint64  `json:"rx_bytes"`
		TxBytes   uint64  `json:"tx_bytes"`
		RxErrors  uint64  `json:"rx_errors"`
		TxErrors  uint64  `json:"tx_errors"`
		Drops     *uint64 `json:"drops,omitempty"`
		RxDrops   *uint64 `json:"rx_drops,omitempty"`
		TxDrops   *uint64 `json:"tx_drops,omitempty"`
	}{
		RxPackets: c.RxPackets, TxPackets: c.TxPackets,
		RxBytes: c.RxBytes, TxBytes: c.TxBytes,
		RxErrors: c.RxErrors, TxErrors: c.TxErrors,
	}
	if c.DirectionalDrops {
		out.RxDrops, out.TxDrops = &c.RxDrops, &c.TxDrops
	} else {
		out.Drops = &c.Drops
	}
	return json.Marshal(out)
}

// 统计来源标识（决策 #68）：statsclient 解码 / VPP 自带同版本工具回退。
const (
	StatsSourceClient = "statsclient"
	StatsSourceTool   = "vpp_get_stats"
)

// Buffers 数据面 buffer 池用量（每 NUMA/池）。
//
// Source 标注来源（StatsSourceClient/StatsSourceTool）；不可用时 Pools 为空且
// Reason 给出原因——调用方不得静默省略（决策 #68）。
type Buffers struct {
	Pools  []BufferPool `json:"pools"`
	Source string       `json:"source,omitempty"`
	Reason string       `json:"reason,omitempty"`
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

// RuntimeThread 单线程的运行态（向量率与主循环速率）。
//
// 数据来源：VPP stats segment 的 `/sys/vector_rate_per_worker` 与 `/sys/loops_per_worker`
// （**组合计数**，按线程索引一条；索引 0 = 主线程，1..N = 工作线程），与 `vppctl show runtime`
// 每线程汇总行是同一量（实测量级吻合）。
type RuntimeThread struct {
	ID         uint32  `json:"id"`
	Core       uint32  `json:"core,omitempty"`
	Name       string  `json:"name,omitempty"`
	VectorRate float64 `json:"vector_rate"`
	LoopsRate  float64 `json:"loops_rate"`
}

// RuntimeStats 数据面运行态（**线程级**；决策 #200）。
//
// 口径说明：VPP 26.06 的**按节点**明细（`show runtime` 主体那张 Calls/Vectors/Suspends/
// Packet-Clocks 表）既不在 stats segment 里、也没有二进制 API（govpp 无 runtime binapi），
// 产品不解析 vppctl 文本，故本结构只承载线程级运行态；Source 标注来源，
// 不可用时 Reason 给原因（不静默省略）。
type RuntimeStats struct {
	Threads       []RuntimeThread `json:"threads"`
	VectorRate    float64         `json:"vector_rate"`              // 整机向量率（/sys/vector_rate）
	WorkerThreads float64         `json:"worker_threads,omitempty"` // 工作线程数（/sys/num_worker_threads）
	UptimeSeconds float64         `json:"uptime_seconds,omitempty"` // 数据面运行时长（now - /sys/boottime）
	Source        string          `json:"source,omitempty"`
	Reason        string          `json:"reason,omitempty"`
}

// Runtime 运行态数据源。
type Runtime interface {
	Threads(ctx context.Context) ([]Thread, error)
	InterfaceCounters(ctx context.Context, ifname string) (InterfaceCounters, bool)
	Buffers(ctx context.Context) (Buffers, bool)
	Memory(ctx context.Context) (Memory, bool)
	RuntimeStats(ctx context.Context) (RuntimeStats, bool)
}

// Counters 单接口计数源（**内核网络数据面**的实现）。
//
// 为什么与 Runtime 分开：Runtime 是 VPP stats segment 的方法面（线程/buffer/主堆/运行态
// 只有 VPP 有），内核数据面一样都给不出；内核能给真值的只有接口计数——而那个真值比 VPP
// **更细**（sysfs 的 rx_dropped/tx_dropped 分方向）。故单列一个窄接口，不让内核实现去
// 假装有 VPP 运行态（决策 #504）。
type Counters interface {
	InterfaceCounters(ctx context.Context, ifname string) (InterfaceCounters, bool)
}

// State 运行态聚合器（Runtime / Counters 可为 nil，方法安全返回空）。
type State struct {
	vpp      Runtime  // VPP 数据面：stats segment
	counters Counters // 内核网络数据面：只有接口计数（实参）
}

// New 构造聚合器。
func New(vpp Runtime) *State { return &State{vpp: vpp} }

// NewCounters 构造「只有接口计数」的聚合器（内核网络数据面，决策 #504）。
//
// 线程/buffer/主堆/运行态一律「不可用」（内核没有 VPP stats segment，读视图据此按数据面
// 点名原因），接口计数取 sysfs 的按方向真值。
func NewCounters(c Counters) *State { return &State{counters: c} }

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
//
// 两条数据面各有自己的计数源：VPP 走 stats segment（Runtime），内核走 sysfs（Counters）。
// 两者互斥装配，故同一个 `State` 只会有一个来源——不会出现「VPP 有值就用 VPP、否则回落内核」
// 这种把两种口径混进同一个读数的行为（决策 #504）。
func (s *State) InterfaceCounters(ctx context.Context, ifname string) (InterfaceCounters, bool) {
	if s == nil {
		return InterfaceCounters{}, false
	}
	if s.counters != nil {
		return s.counters.InterfaceCounters(ctx, ifname)
	}
	if s.vpp == nil {
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

// RuntimeStats 返回线程级运行态（不可用返回 ok=false）。
func (s *State) RuntimeStats(ctx context.Context) (RuntimeStats, bool) {
	if s == nil || s.vpp == nil {
		return RuntimeStats{}, false
	}
	return s.vpp.RuntimeStats(ctx)
}
