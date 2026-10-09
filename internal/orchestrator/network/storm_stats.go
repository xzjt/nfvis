package network

// 决策 #385：storm control 的 policer 计数读取（stats segment 侧）。
//
// 口径（不猜）：VPP 26.06 的 `policer_dump` **只含配置字段**（Name/Cir/Cb/Type/动作…），
// 不含 conform/exceed/violate 计数（govpp 同版的 policer_details 绑定可为证），故计数只能
// 来自 stats segment。这里按 stats segment 的组合计数路径 `/net/policer/<policer>/<字段>`
// 读取，字段名取 VPP 惯例的 conform/exceed/violate 三族的 packets/bytes。
//
// **匹配一律按 policer 名**（决策 #429②）：policer 索引在 `policer_dump` 里不可回读（自认领态
// 只能以哨兵占位）、且可能被底座复用——按索引匹配会把别的 policer 的计数读成本产品的计数。
// 名字在 stats segment 里查不到即 ok=false（调用方如实报「计数不可读」+ 原因），**不按索引猜**、
// 也不编造数字（宁缺不谎报，与 /metrics 的聚合口径一致）。
//
// 底座调用在 stats_linux.go（vpp_get_stats，与 VPP 同版本），本文件只放纯解析（可跨平台单测）。

import (
	"strings"
)

// stormPolicerStatsPattern vpp_get_stats 的 policer 计数路径 pattern。
const stormPolicerStatsPattern = "/net/policer/*"

// stormPolicerStatsPrefix stats segment 里 policer 计数路径前缀。
const stormPolicerStatsPrefix = "/net/policer/"

// StormCountersFromDump 从 `vpp_get_stats dump machine` 文本提取某 policer 的计数。
// 匹配口径：路径 `/net/policer/<policer>/<字段>` 且 <policer> **等于该 policer 名**（决策 #429②：
// 一律按名，不按索引猜）。未发现任何相关字段时 ok=false（调用方据此如实说明不可读，不返回零值当真值）。
func StormCountersFromDump(out string, policerName string) (StormCounters, bool) {
	if policerName == "" {
		return StormCounters{}, false
	}
	var c StormCounters
	found := false
	for path, v := range ParseStatsMachine(out) {
		rest, ok := strings.CutPrefix(path, stormPolicerStatsPrefix)
		if !ok {
			continue
		}
		id, field, ok := strings.Cut(rest, "/")
		if !ok || id != policerName {
			continue
		}
		u := uint64(v)
		switch field {
		case "conform_packets":
			c.ConformPackets, found = u, true
		case "conform_bytes":
			c.ConformBytes, found = u, true
		case "exceed_packets":
			c.ExceedPackets, found = u, true
		case "violate_packets":
			c.ViolatePackets, found = u, true
		case "violate_bytes":
			c.ViolateBytes, found = u, true
		}
	}
	return c, found
}
