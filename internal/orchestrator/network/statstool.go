package network

// D-1 第一层（决策 #68）：VPP 自带同版本统计工具（vpp_get_stats）回退源。
//
// 背景：govpp v0.13.0 的 statsclient 对 VPP 26.06 的 buffer 池值类型解码不出——
// 池名能解析但 used/available/cached 全为 0。VPP 自带 vpp_get_stats 与 VPP 同版本，
// 解码保证正确（实测 /buffer-pools/default-numa-0/available = 430184）。
//
// 底座调用藏在 StatsTool 接口后；解析为纯函数，可跨平台单测。

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/xzjt/nfvis/internal/state"
)

// bufferPoolPattern vpp_get_stats 的 buffer 池路径 pattern。
const bufferPoolPattern = "/buffer-pools/*"

// bufferPoolPrefix stats segment 中 buffer 池路径前缀。
const bufferPoolPrefix = "/buffer-pools/"

// StatsTool 执行 VPP 自带 stats 工具。
type StatsTool interface {
	// DumpMachine 返回 `dump machine` 文本（每行 <type>:<value>:<path>）。
	DumpMachine(ctx context.Context, pattern string) (string, error)
}

// ParseStatsMachine 解析 `vpp_get_stats dump machine` 输出为 path → value。
//
// 行格式为 `<type>:<value>:<path>`，例如
//
//	9:430184.00:/buffer-pools/default-numa-0/available
//
// 组合计数（错误计数）行形如 `2:0:0:0:/err/...`，字段数与形状不符，自然被跳过；
// 非数字 type/value 与非绝对路径同样跳过。
func ParseStatsMachine(out string) map[string]float64 {
	res := map[string]float64{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// path 不含 ':'，故 SplitN 三段即可；组合计数的多余字段落在 path 段被前缀判定挡掉。
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if _, err := strconv.Atoi(parts[0]); err != nil {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil {
			continue
		}
		if !strings.HasPrefix(parts[2], "/") {
			continue
		}
		res[parts[2]] = v
	}
	return res
}

// BufferPoolsFromDump 从 dump 文本提取 buffer 池（按池名聚合、按名排序保证输出稳定）。
// 未发现任何 buffer 池条目时返回 ok=false。
func BufferPoolsFromDump(out string) ([]state.BufferPool, bool) {
	type pool struct{ used, available, cached float64 }
	pools := map[string]*pool{}
	for path, v := range ParseStatsMachine(out) {
		rest, ok := strings.CutPrefix(path, bufferPoolPrefix)
		if !ok {
			continue
		}
		name, field, ok := strings.Cut(rest, "/")
		if !ok || name == "" {
			continue
		}
		p := pools[name]
		if p == nil {
			p = &pool{}
			pools[name] = p
		}
		switch field {
		case "used":
			p.used = v
		case "available":
			p.available = v
		case "cached":
			p.cached = v
		}
	}
	if len(pools) == 0 {
		return nil, false
	}
	names := make([]string, 0, len(pools))
	for name := range pools {
		names = append(names, name)
	}
	sort.Strings(names)
	out2 := make([]state.BufferPool, 0, len(names))
	for _, name := range names {
		p := pools[name]
		out2 = append(out2, state.BufferPool{
			Name: name, Used: p.used, Available: p.available, Cached: p.cached,
		})
	}
	return out2, true
}
