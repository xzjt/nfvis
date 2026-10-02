package network

// 决策 #339：ACL 逐规则命中计数（自解析 stats segment 的组合计数）。
//
// 由来：#68 曾记「绑定后 stats 段无逐规则计数」，round117 真机探查推翻该前提——VPP 的
// ACL 插件**本就**注册了逐规则计数，路径 `/acl/<acl-index>/matches`，**无需任何 VPP 侧
// 开关**（#68 里那条会误执行为 acl_del 的 `acl_stats_intf_counters_enable` 路径不碰）。
//
// 行格式以 **VPP 源码**定死（不照抄 round117 的转述，探查结论与源码不符处以源码为准）：
//
//	类型值 3 = STAT_DIR_TYPE_COUNTER_VECTOR_COMBINED（src/vlib/stats/shared.h 的枚举：
//	  ILLEGAL=0 / SCALAR=1 / COUNTER_VECTOR_SIMPLE=2 / COUNTER_VECTOR_COMBINED=3）。
//	src/vpp/app/vpp_get_stats.c 的 dump_stats_result() 对组合计数按线程打印
//	  `%s%d:%d:%d:%llu:%llu:%s` = 类型:元素:线程:packets:bytes:路径，
//	  即 `3:<规则下标>:<线程索引>:<hits>:<bytes>:/acl/<acl-index>/matches`
//	  （先印内层 j=元素、再印外层 k=线程）；加 `-s` 汇总时退化为
//	  `3:<规则下标>:<packets>:<bytes>:<路径>`（元素在前）。
//	src/plugins/acl/acl.c 的 validate_and_reset_acl_counters() 以
//	  `vlib_validate_combined_counter` 把 `/acl/<i>/matches` 建成**组合计数**（元素维=规则）。
//	src/plugins/acl/dataplane_node.c 以
//	  `vlib_increment_combined_counter(计数器, thread_index, ace_index, packets, bytes)`
//	  递增——元素=规则、外层=线程，两个尾数分别是 **packets 与 bytes**。
//
// ⚠️ 口径更正（如实标注）：round117 把两个尾数记为同一 64 位计数的 `hi:lo` 高/低半字，
// 与源码不符——它是 `vlib_counter_t{packets, bytes}` 两个独立 u64。故**命中数取 packets**
// （bytes 不属「命中」语义，不在此呈现），跨线程求和即「规则总命中」。字段顺序也以源码
// 为准：**规则下标在前、线程在后**（round117 记的「线程:规则」次序相反）。
//
// 边界（决策 #339 登记）：计数是**运行态**（VPP 重启归零）；只覆盖 L3 ACL 的 `matches`
// 组合计数（macip 不计）；VPP 侧无「按接口」粒度，故只到「按规则」。

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// aclMatchesPattern vpp_get_stats 的 ACL 逐规则计数路径 pattern。
const aclMatchesPattern = "/acl/*"

// aclMatchesPathPrefix / Suffix `/acl/<acl-index>/matches` 的前后缀。
const (
	aclMatchesPathPrefix = "/acl/"
	aclMatchesPathSuffix = "/matches"
)

// ParseACLCounters 解析 `vpp_get_stats dump machine '/acl/*'` 输出为
// acl-index → 规则下标 → 命中数（packets，跨线程求和）。
//
// 逐线程行（默认输出）形如
//
//	3:<规则下标>:<线程索引>:<hits>:<bytes>:/acl/<acl-index>/matches
//
// 汇总行（`-s`）形如 `3:<规则下标>:<hits>:<bytes>:/acl/<acl-index>/matches`，两种都收。
// 路径不匹配、字段数不符、数值非法的行一律跳过（malformed 不猜）。
func ParseACLCounters(out string) map[uint32]map[uint32]uint64 {
	res := map[uint32]map[uint32]uint64{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 5 {
			continue
		}
		aclIdx, ok := aclIndexFromMatchesPath(parts[len(parts)-1])
		if !ok {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimSpace(parts[0])); err != nil { // 类型段非数字
			continue
		}
		var ruleStr, hitsStr string
		switch len(parts) {
		case 5: // 汇总行：类型:规则:packets:bytes:路径
			ruleStr, hitsStr = parts[1], parts[2]
		case 6: // 逐线程行：类型:规则:线程:packets:bytes:路径
			ruleStr, hitsStr = parts[1], parts[3]
		default:
			continue
		}
		rule, err := strconv.Atoi(strings.TrimSpace(ruleStr))
		if err != nil || rule < 0 {
			continue
		}
		hits, err := strconv.ParseUint(strings.TrimSpace(hitsStr), 10, 64)
		if err != nil {
			continue
		}
		if res[aclIdx] == nil {
			res[aclIdx] = map[uint32]uint64{}
		}
		res[aclIdx][uint32(rule)] += hits
	}
	return res
}

// aclIndexFromMatchesPath 从 `/acl/<acl-index>/matches` 取 acl-index。
func aclIndexFromMatchesPath(path string) (uint32, bool) {
	rest, ok := strings.CutPrefix(path, aclMatchesPathPrefix)
	if !ok {
		return 0, false
	}
	idxStr, ok := strings.CutSuffix(rest, aclMatchesPathSuffix)
	if !ok || idxStr == "" || strings.Contains(idxStr, "/") {
		return 0, false
	}
	n, err := strconv.ParseUint(idxStr, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}

// ACLHitCounters 读取全部 ACL 的逐规则命中（原始值，按 acl-index/规则下标）。
//
// 走既有 StatsTool（vpp_get_stats，与 VPP 同版本，决策 #68）；无回退源或工具失败
// **如实上抛**（不吞、不当作 0）。
func (m *Manager) ACLHitCounters(ctx context.Context) (map[uint32]map[uint32]uint64, error) {
	if m == nil || m.statsTool == nil {
		return nil, fmt.Errorf("无同版本统计工具回退源（vpp_get_stats）")
	}
	text, err := m.statsTool.DumpMachine(ctx, aclMatchesPattern)
	if err != nil {
		return nil, err
	}
	return ParseACLCounters(text), nil
}

// ACLCounters 把 stats segment 的 ACL 逐规则命中映射到产品 ACL 名（CLI/REST 读视图用）。
//
// 映射来源是 acl.go 的下发登记（name → VPP acl index），不新造第二份事实；
// 未登记的 index（残渣/未下发）没有产品名，自然不呈现。
type ACLCounters struct {
	mgr *Manager
	acl *AclProvider
}

// NewACLCounters 构造映射器（mgr/acl 可空，方法据此如实报未接入）。
func NewACLCounters(mgr *Manager, acl *AclProvider) *ACLCounters {
	return &ACLCounters{mgr: mgr, acl: acl}
}

// ACLHitCounters 返回 ACL 名 → 规则下标 → 命中数（跨线程求和；规则下标 = 下发顺序）。
//
// 取数失败**如实上抛**——调用方呈现原因，不静默当作「无命中」。
func (c *ACLCounters) ACLHitCounters(ctx context.Context) (map[string]map[uint32]uint64, error) {
	if c == nil || c.mgr == nil {
		return nil, fmt.Errorf("VPP 未接入（编排器未装配）")
	}
	raw, err := c.mgr.ACLHitCounters(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]map[uint32]uint64{}
	if c.acl == nil {
		return out, nil
	}
	// 登记快照是 name→index；此处置换成 index→name 做一次反查（只此一份事实来源）。
	for name, idx := range c.acl.ACLIndexes() {
		if rules, ok := raw[idx]; ok {
			out[name] = rules
		}
	}
	return out, nil
}
