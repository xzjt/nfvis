package network

// 采样式 L2 环路检测（决策 #337；缓解手段 = 交换机 learn-limit，见 l2.go 的 syncLearnLimit）。
//
// 定位与边界（如实登记，勿把它当 STP）：
//   - **只检测、只告警，不阻断**：产品不下发任何破环动作（不关端口、不改转发），
//     发现疑似环路由操作者按告警里的成员口人工处置；
//   - **采样式**：每轮（60s 巡检）读一次各 L2 交换机的 MAC 学习表，与上一轮快照比较。
//     周期性采样天然会漏「快抖」（两次采样之间来回跳完）与「慢抖」（周期远大于采样间隔），
//     故判据保守（连续多轮 + 回跳），宁可晚报也不谎报；
//   - **进程内状态**：快照与运动历史存在 nfvisd 进程内 map，**跨进程重启重置**（重启后需重新
//     积累轮次才能判抖动）；这不影响「学习表逼近上限」这一类**单轮即可判**的判据。
//
// STP/RSTP 属 v3（本轮不做）；cross-connect 直通无 MAC 学习，本检测对其无效（见手册 §8.11 告警）。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xzjt/nfvis/internal/model"
)

// loopScope 采样式环路检测的告警作用域（决策 #337）：与 recovery/interface-link/vnf-port
// **独立**，按事实对账、不与其他 scope 的全量 Sync 互相误伤。
const loopScope = "loop"

// AlarmLoopSuspected L2 环路疑似（采样式）：同一 MAC 在成员口之间来回抖动（连续多轮 + 回跳），
// 或学习表条目逼近交换机 learn-limit 上限。严重级别 warning；仅告警不阻断。
const AlarmLoopSuspected = "LOOP_SUSPECTED"

// loopCalmRounds 消解所需的连续「无嫌疑」轮次：连续 2 轮无抖动且条目回落即 Resolve。
const loopCalmRounds = 2

// loopMinFlips 判定抖动所需的「连续落在不同成员口」的最小轮数（≥2 轮 ⇒ 至少 3 次观测）。
const loopMinFlips = 2

// loopStormPps 判据③「成员口同时被广播风暴打满」的速率阈值（pps）。
//
// 由来（round115 真机实证，不得省的教训）：经典两口环（双 vNIC 的 VNF + guest 内 bridge）
// 实测两个成员口各 ~137k pps；取 50000 为门槛，既远高于正常业务/突发（单台忙 VM 只打满
// 自己的口），又低于实测环路量级，留足采样抖动余量。改此常量即调整该判据。
const loopStormPps = 50000

// loopStormRounds 判据③所需的连续「全员高负载」**可计算**轮次。
// 首轮仅建 rx 基线（无上一轮样本算不出 pps），故实际至少需 3 次采样；连续 2 轮同时高负载
// 才 Raise，避免单次采样尖峰误报。
const loopStormRounds = 2

// loopDetector 采样式 L2 环路检测的进程内状态（跨 nfvisd 重启重置）。
type loopDetector struct {
	mu   sync.Mutex
	macs map[string]*loopMacMotion // "<交换机>\x00<MAC>" → 该 MAC 的运动历史
	sw   map[string]*loopSwitchState
}

type loopMacMotion struct {
	port   string // 最近一轮所在成员口
	prev   string // 上一轮所在成员口（判「回跳」用）
	streak int    // 连续与上一轮不同口的次数
}

type loopSwitchState struct {
	calm int // 连续无嫌疑轮次（达到 loopCalmRounds 即消警）
	// 判据③（决策 #337 修订）：上一轮各成员口 rx 采样，本轮据此换算 pps。
	prev        map[string]loopCounterSample
	stormStreak int // 连续「全员高负载」轮次
}

// loopCounterSample 上一轮单个成员口的 rx 累计计数与采样时刻。
type loopCounterSample struct {
	rx uint64
	at time.Time
}

// loopPortSample 单个已声明成员口本轮的 rx 计数采样（判据③输入，由 CheckLoop 填充）。
type loopPortSample struct {
	port string    // VPP 侧接口名（与 #326 读视图同源）
	rx   uint64    // 累计 rx 包数
	at   time.Time // 采样时刻（换算 pps 用）
}

// loopPortPps 单个成员口本轮换算出的 rx 速率（供告警消息）。
type loopPortPps struct {
	port string
	pps  float64
}

// loopStormResult 一轮「成员口同时高负载」判据的结果。
type loopStormResult struct {
	allHigh bool          // 所有成员口 pps 均 ≥ loopStormPps
	ok      bool          // 本轮可判定（≥2 口、有上轮样本、时间前进、计数未回绕）
	pps     []loopPortPps // 各成员口 pps（按接口名升序，消息确定性）
}

// stormRound 计算本轮「成员口同时高负载」判据，并把本轮采样滚动为下一轮的基线。
//   - 口数 <2（单口不可能成环）、成员集与上轮不同（缺上轮样本）、采样时间未前进、
//     rx 计数回绕（VPP 重启/统计清零）——一律 ok=false（不可判定，宁缺勿错、不报）；
//   - 所有口 pps 均 ≥ loopStormPps 时 allHigh=true。
func (st *loopSwitchState) stormRound(cur []loopPortSample) loopStormResult {
	defer func() { // 无论是否可判，本轮采样都成为下一轮基线（缺失即下一轮重新建基线）
		m := make(map[string]loopCounterSample, len(cur))
		for _, s := range cur {
			m[s.port] = loopCounterSample{rx: s.rx, at: s.at}
		}
		st.prev = m
	}()
	var res loopStormResult
	if len(cur) < 2 {
		return res
	}
	sorted := append([]loopPortSample(nil), cur...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].port < sorted[j].port })
	for _, s := range sorted {
		p, ok := st.prev[s.port]
		if !ok {
			return loopStormResult{} // 成员集变化：本轮不可判定
		}
		dt := s.at.Sub(p.at).Seconds()
		if dt <= 0 || s.rx < p.rx {
			return loopStormResult{} // 时间未前进 / 计数回绕：不可判定
		}
		res.pps = append(res.pps, loopPortPps{port: s.port, pps: float64(s.rx-p.rx) / dt})
	}
	res.ok = true
	res.allHigh = true
	for _, e := range res.pps {
		if e.pps < loopStormPps {
			res.allHigh = false
		}
	}
	return res
}

func newLoopDetector() *loopDetector {
	return &loopDetector{macs: map[string]*loopMacMotion{}, sw: map[string]*loopSwitchState{}}
}

// loopVerdict 单轮判决结果。
type loopVerdict struct {
	suspected   bool
	message     string
	calmRounds  int  // 本轮结束时该交换机的连续平静轮次
	calmReached bool // 是否达到消解阈值
}

// observe 用本轮快照更新一台交换机的检测状态并给出判决。
//   - macPorts：MAC → 成员口展示名（本轮学习表）；
//   - entries / learnLimit：学习表条目数与交换机声明的 learn-limit（0=未配置则不判逼近上限）；
//   - ports：各已声明成员口的 rx 采样（判据③；读数不可用/成员口不足 2 个时为空切片，该判据跳过）。
//
// 抖动判据（保守）：同一 MAC **连续 ≥2 轮**落在不同成员口，**且出现回跳**（本轮成员口等于
// 两轮前所在的口，即 A→B→A 形态）——只单向移动一次不报，避免把正常的 MAC 迁移当成环路。
func (d *loopDetector) observe(sw string, macPorts map[string]string, entries, learnLimit int, ports []loopPortSample) loopVerdict {
	d.mu.Lock()
	defer d.mu.Unlock()

	st := d.sw[sw]
	if st == nil {
		st = &loopSwitchState{}
		d.sw[sw] = st
	}

	var reasons []string

	// 1) MAC 成员口抖动。
	keys := make([]string, 0, len(macPorts))
	for m := range macPorts {
		keys = append(keys, m)
	}
	sort.Strings(keys) // 判据与消息顺序确定（可复跑的确定性）
	for _, mac := range keys {
		port := macPorts[mac]
		key := sw + "\x00" + mac
		m := d.macs[key]
		if m == nil {
			d.macs[key] = &loopMacMotion{port: port}
			continue
		}
		if port == m.port {
			m.streak = 0
			continue
		}
		m.streak++
		if m.streak >= loopMinFlips && m.prev == port { // 回跳
			reasons = append(reasons, fmt.Sprintf("MAC %s 在成员口 %s 与 %s 之间来回抖动（连续 %d 轮落在不同口）",
				mac, m.port, port, m.streak+1))
		}
		m.prev, m.port = m.port, port
	}

	// 2) 学习表逼近上限（仅配了 learn-limit 时判；单轮即可判，不受采样历史影响）。
	//    ≤16777216 的乘法不会溢出 int64；判据是「条目数 ≥ 上限的 90%」。
	if learnLimit > 0 && entries > 0 && int64(entries)*10 >= int64(learnLimit)*9 {
		reasons = append(reasons, fmt.Sprintf("学习表条目 %d 已达上限 %d 的 90%%（越限后新 MAC 不再被学习，可能是环路/扫描）",
			entries, learnLimit))
	}

	// 3) 成员口同时高负载（判据③，决策 #337 修订）。round115 真机实证：两口环下 MAC 稳定
	//    落单口（判据①不命中）、学习数远离上限（判据②不命中），但两成员口各 ~137k pps——
	//    该形态下 ①② 结构性漏报，故补此判据：所有成员口（≥2）同时 ≥ loopStormPps 且
	//    连续 loopStormRounds 轮 → 判嫌疑；单台忙 VM 只打满自己的口，不构成「同时高」。
	sr := st.stormRound(ports)
	if sr.ok && sr.allHigh {
		st.stormStreak++
		if st.stormStreak >= loopStormRounds {
			parts := make([]string, 0, len(sr.pps))
			for _, e := range sr.pps {
				parts = append(parts, fmt.Sprintf("%s≈%.0f pps", e.port, e.pps))
			}
			reasons = append(reasons, fmt.Sprintf(
				"疑似广播风暴/环路（成员口同时高负载）：%s；连续 %d 轮 ≥ %d pps",
				strings.Join(parts, "、"), st.stormStreak, loopStormPps))
		}
	} else {
		st.stormStreak = 0
	}

	if len(reasons) > 0 {
		st.calm = 0
		return loopVerdict{suspected: true, message: strings.Join(reasons, "；")}
	}
	st.calm++
	return loopVerdict{calmRounds: st.calm, calmReached: st.calm >= loopCalmRounds}
}

// forget 清除一台交换机的全部检测状态（对象已从配置移除时调用，避免进程内状态泄漏）。
func (d *loopDetector) forget(sw string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.sw, sw)
	prefix := sw + "\x00"
	for k := range d.macs {
		if strings.HasPrefix(k, prefix) {
			delete(d.macs, k)
		}
	}
}

// CheckLoop 采样式 L2 环路检测（决策 #337）：对每个 L2 交换机读一次 MAC 学习表并与上一轮快照
// 比较，疑似环路/学习表逼近上限/成员口同时被风暴打满时 Raise LOOP_SUSPECTED（独立 scope
// "loop"），连续多轮平静后 Resolve。
//
// 读路径复用既有读物（不新造一套）：MAC 表走 `L2Provider`/`L2Client.MACTable`（FR-NET-015）；
// 成员口清单走 `model.DerivedSwitchPorts`（#326 读视图，config/vnf/container 并集）；成员口
// rx 计数走 `InterfaceCounterReader.InterfaceCounters`（#326 的运行态读数路径）。
// 接线：cmd/nfvisd 的 60s 巡检块（与硬件/大页告警同循环）；查询失败如实回错误（不谎报「无环路」）。
func (n *L2Network) CheckLoop(ctx context.Context, cfg model.Config) []error {
	if n == nil || n.l2 == nil {
		return nil
	}
	c, err := n.l2.client()
	if err != nil {
		return []error{err}
	}
	defer c.Close()
	names, err := c.SwInterfaceNames()
	if err != nil {
		return []error{fmt.Errorf("环路检测：查询接口状态: %w", err)}
	}
	if n.loop == nil {
		n.loop = newLoopDetector()
	}

	var errs []error
	declared := make(map[string]bool, len(cfg.VirtualSwitches))
	for _, vs := range cfg.VirtualSwitches {
		if vs.Type != "l2" {
			continue // L3 交换机不经 bridge-domain（无 MAC 学习表）
		}
		declared[vs.Name] = true
		rows, err := c.MACTable(BDID(vs.Name))
		if err != nil {
			errs = append(errs, fmt.Errorf("交换机 %s: 读取 MAC 学习表: %w", vs.Name, err))
			continue
		}
		macPorts := make(map[string]string, len(rows))
		for _, r := range rows {
			port := names[r.SwIfIndex].Name
			if port == "" {
				port = fmt.Sprintf("sw_if_index %d", r.SwIfIndex)
			}
			macPorts[r.MAC] = port
		}
		ports := n.loopPortSamples(ctx, cfg, vs.Name, &errs)
		v := n.loop.observe(vs.Name, macPorts, len(rows), vs.LearnLimit, ports)
		if n.alarms == nil {
			continue // 未注入告警表（如无 VPP 路径的部署）：仅检测不落告警
		}
		switch {
		case v.suspected:
			n.alarms.Raise(loopScope, SeverityWarning, AlarmLoopSuspected, v.message, vs.Name)
		case v.calmReached:
			n.alarms.Resolve(loopScope, AlarmLoopSuspected, vs.Name)
		}
	}

	// 对象消失的清警（round86 同族口径）：committed 里已无该交换机 → 消解其环路告警并清状态，
	// 否则告警会永久滞留（检查函数不再遍历到它）。
	if n.alarms != nil {
		for _, ref := range n.alarms.ActiveOf(loopScope) {
			if ref.Code != AlarmLoopSuspected || declared[ref.Source] {
				continue
			}
			n.alarms.Resolve(loopScope, AlarmLoopSuspected, ref.Source)
			n.loop.forget(ref.Source)
		}
	}
	return errs
}

// loopPortSamples 读该交换机全部**已声明**成员口的 rx 计数采样（判据③，决策 #337 修订）。
//
// 成员口清单复用 #326 的读视图 `model.DerivedSwitchPorts`（静态 ports 与 VNF/容器 vNIC 声明的
// 并集），计数读数复用 #326 的运行态路径 `InterfaceCounterReader.InterfaceCounters`——不新造
// VPP 查询。无法解析到 VPP 接口名的成员口（如 SR-IOV VF）不计数、不编造。
//
// 读数不完整即**放弃本轮风暴判定**（返回 nil）并把失败如实 append 到 errs：宁可漏报也不误报
// （计数读不到时无法断言「同时高负载」）。未注入读数源（n.counters == nil）时同样返回 nil，
// 但**不报错**（与未注入告警表同口径：该判据在无运行态读数的部署上静默跳过）。
func (n *L2Network) loopPortSamples(ctx context.Context, cfg model.Config, sw string, errs *[]error) []loopPortSample {
	if n.counters == nil {
		return nil
	}
	now := time.Now()
	var out []loopPortSample
	for _, p := range model.DerivedSwitchPorts(cfg, sw) {
		name := p.Port
		if name == "" {
			continue // 无法确定 VPP 接口名：计数不可得
		}
		c, ok := n.counters.InterfaceCounters(ctx, name)
		if !ok {
			*errs = append(*errs, fmt.Errorf("交换机 %s: 读取成员口 %s 计数失败", sw, name))
			return nil // 读数不完整：本轮不做风暴判定（错误已如实上报）
		}
		out = append(out, loopPortSample{port: name, rx: c.RxPackets, at: now})
	}
	return out
}
