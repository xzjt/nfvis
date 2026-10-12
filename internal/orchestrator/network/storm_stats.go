package network

// 决策 #476：storm control 的 policer 计数读取（stats segment 侧）。
//
// 口径（不猜）：VPP 26.06 的 `policer_dump` **只含配置字段**（Name/Cir/Cb/Type/动作…），
// 不含 conform/exceed/violate 计数（govpp 同版的 policer_details 绑定可为证），故计数只能
// 来自 stats segment。真机 spike（round26）实测：policer 计数在 stats segment 里是**组合计数**
//
//	<type>:<index>:<thread>:<packets>:<bytes>:<path>
//
// path ∈ `/net/policer/{conform,exceed,violate}`——按 **policer 索引**分维度，**不含 policer 名**；
// 同一索引**每个 VPP 线程一行**（thread 字段不同），故需**按索引跨线程求和** packets/bytes。
//
// **匹配一律按 policer 索引**（决策 #476，修正 #429② 的「按名」口径）：`policer_dump` 的
// `PolicerDetails` **无索引字段**（binapi 实测），故「按名建索引」不可行；产品能拿到的索引只有
// 一个来源——本进程 `PolicerAddDel` 应答里的 `PolicerIndex`（登记 `stormKindRT.policerIdx`）。
// 索引不可得（哨兵：nfvisd 重启后按名自认领、无 add 应答）时**不猜**、调用方如实报「不可读」。
//
// 底座调用在 stats_linux.go（vpp_get_stats，与 VPP 同版本），本文件只放纯解析（可跨平台单测）。

import (
	"strconv"
	"strings"
)

// stormPolicerStatsPattern vpp_get_stats 的 policer 计数路径 pattern。
const stormPolicerStatsPattern = "/net/policer/*"

// stormPolicerStatsPrefix stats segment 里 policer 计数路径前缀。
const stormPolicerStatsPrefix = "/net/policer/"

// StormCountersFromDump 从 `vpp_get_stats dump machine` 文本提取某 policer 索引的计数。
//
// 匹配口径（决策 #476）：行是**组合计数** `<type>:<index>:<thread>:<packets>:<bytes>:<path>`，
// path 命中 `/net/policer/{conform,exceed,violate}` 之一、且行首索引 == 目标索引才计入，
// **跨线程按 packets/bytes 求和**（同一索引每线程一行）。
//
// 其它 path（`/err/…`、`/sys/…`、buffer 池等）与畸形行（段数不符、索引/计数非数字）一律跳过；
// 三族行都未出现时 ok=false（调用方据此如实说明不可读，不返回零值当真值）。
func StormCountersFromDump(out string, policerIndex uint32) (StormCounters, bool) {
	// 哨兵（索引未记录）不是任何真实索引：直接判不可读（调用方通常已前置判断，这里兜底）。
	if policerIndex == stormPolicerIdxUnknown {
		return StormCounters{}, false
	}
	var c StormCounters
	found := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 组合计数固定 6 段：type:index:thread:packets:bytes:path（path 不含 ':'）。
		// 简单计数（3 段）与错误计数（5 段，`2:0:0:0:/err/…`）段数不符，自然被跳过。
		parts := strings.SplitN(line, ":", 6)
		if len(parts) != 6 {
			continue
		}
		field, ok := strings.CutPrefix(parts[5], stormPolicerStatsPrefix)
		if !ok {
			continue // 非 policer 路径（/err/…、/sys/…、buffer 池…）
		}
		idx, err := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 32)
		if err != nil || uint32(idx) != policerIndex {
			continue // 畸形索引，或不是目标索引（多索引并存不混淆）
		}
		packets, errP := strconv.ParseUint(strings.TrimSpace(parts[3]), 10, 64)
		bytes, errB := strconv.ParseUint(strings.TrimSpace(parts[4]), 10, 64)
		if errP != nil || errB != nil {
			continue // 畸形计数
		}
		switch field {
		case "conform":
			c.ConformPackets += packets
			c.ConformBytes += bytes
			found = true
		case "exceed":
			c.ExceedPackets += packets
			found = true
		case "violate":
			c.ViolatePackets += packets
			c.ViolateBytes += bytes
			found = true
		}
	}
	return c, found
}
