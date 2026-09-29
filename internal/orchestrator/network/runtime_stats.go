package network

// 决策 #200：`show vpp runtime` 的数据源与口径。
//
// VPP 的**按节点**运行态明细（`show runtime` 主体那张 Calls/Vectors/Suspends/Packet-Clocks 表）
// 只存在于 vlib 主堆里：既没有二进制 API（govpp 里根本没有 runtime binapi 包），
// 也不在 stats segment（真机实测：全量 dump 18090 条里没有任何 `/sys/node/*` 路径），
// 只有 vppctl 读得到。产品不解析 vppctl 文本（表头随版本漂移，解析错就是静默错值），
// 因此这里只提供**线程级**运行态——它同样在 stats segment 里，且与 `show runtime`
// 每线程汇总行是同一量（实测两者量级吻合）：
//
//	/sys/vector_rate_per_worker  组合计数，按线程索引一条（索引 0 = 主线程，1..N = 工作线程）
//	/sys/loops_per_worker        同上
//	/sys/vector_rate             整机向量率
//	/sys/num_worker_threads      工作线程数
//	/sys/boottime                VPP 启动时刻（epoch 秒）→ 数据面运行时长
//
// 解析为纯函数（可跨平台单测）；底座调用经既有 StatsTool（vpp_get_stats，与 VPP 同版本）。

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xzjt/nfvis/internal/state"
)

// runtimeSysPattern stats segment 里运行态计数的 pattern。
const runtimeSysPattern = "/sys/*"

// 本实现认识的四条运行态计数路径。
const (
	statsPathVectorRate       = "/sys/vector_rate"
	statsPathVectorRateWorker = "/sys/vector_rate_per_worker"
	statsPathLoopsWorker      = "/sys/loops_per_worker"
	statsPathNumWorkers       = "/sys/num_worker_threads"
	statsPathBoottime         = "/sys/boottime"
)

// ParseStatsPerThread 解析 `vpp_get_stats dump machine` 输出里的**组合计数**行为
// path → 线程索引 → 值。
//
// 组合计数行形如 `2:<index>:<count>:<value>:<path>`（第 4 段是值：真机实测
// /sys/loops_per_worker 索引 0/1 的值与 `vppctl show runtime` 各线程 loops/sec 量级吻合）。
// 简单计数是 `<type>:<value>:<path>` 三段，字段数不同，按段数分派、互不干扰；
// 错误计数一类同样是组合计数（`2:0:0:0:/err/...`），解析进来无害——调用方只取自己认识的路径。
func ParseStatsPerThread(out string) map[string]map[int]float64 {
	res := map[string]map[int]float64{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 5 {
			continue
		}
		path := parts[len(parts)-1]
		if !strings.HasPrefix(path, "/") {
			continue
		}
		idx, errIdx := strconv.Atoi(strings.TrimSpace(parts[1]))
		val, errVal := strconv.ParseFloat(strings.TrimSpace(parts[len(parts)-2]), 64)
		if errIdx != nil || errVal != nil {
			continue
		}
		if res[path] == nil {
			res[path] = map[int]float64{}
		}
		res[path][idx] = val
	}
	return res
}

// RuntimeStatsFromDump 由 `vpp_get_stats dump machine /sys/*` 文本组装线程级运行态
// （纯函数）。now 用于把 /sys/boottime（epoch 秒）换算成运行时长。
// 一个可识别的线程计数都没有时 ok=false（宁缺勿错，不编造空表）。
func RuntimeStatsFromDump(out string, now time.Time) (state.RuntimeStats, bool) {
	simple := ParseStatsMachine(out)
	perThread := ParseStatsPerThread(out)
	loops := perThread[statsPathLoopsWorker]
	rates := perThread[statsPathVectorRateWorker]
	if len(loops) == 0 && len(rates) == 0 {
		return state.RuntimeStats{}, false
	}
	seen := map[int]bool{}
	for i := range loops {
		seen[i] = true
	}
	for i := range rates {
		seen[i] = true
	}
	ids := make([]int, 0, len(seen))
	for i := range seen {
		if i >= 0 {
			ids = append(ids, i)
		}
	}
	sort.Ints(ids)
	res := state.RuntimeStats{
		VectorRate:    simple[statsPathVectorRate],
		WorkerThreads: simple[statsPathNumWorkers],
		Source:        state.StatsSourceTool,
	}
	for _, i := range ids {
		res.Threads = append(res.Threads, state.RuntimeThread{
			ID: uint32(i), VectorRate: rates[i], LoopsRate: loops[i],
		})
	}
	if boot, ok := simple[statsPathBoottime]; ok && boot > 0 {
		if up := now.Unix() - int64(boot); up > 0 {
			res.UptimeSeconds = float64(up)
		}
	}
	return res, true
}
