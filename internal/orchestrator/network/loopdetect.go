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
//   - entries / learnLimit：学习表条目数与交换机声明的 learn-limit（0=未配置则不判逼近上限）。
//
// 抖动判据（保守）：同一 MAC **连续 ≥2 轮**落在不同成员口，**且出现回跳**（本轮成员口等于
// 两轮前所在的口，即 A→B→A 形态）——只单向移动一次不报，避免把正常的 MAC 迁移当成环路。
func (d *loopDetector) observe(sw string, macPorts map[string]string, entries, learnLimit int) loopVerdict {
	d.mu.Lock()
	defer d.mu.Unlock()

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

	st := d.sw[sw]
	if st == nil {
		st = &loopSwitchState{}
		d.sw[sw] = st
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
// 比较，疑似环路/学习表逼近上限时 Raise LOOP_SUSPECTED（独立 scope "loop"），连续多轮平静后 Resolve。
//
// 读路径复用既有的 `L2Provider`/`L2Client.MACTable`（FR-NET-015 的 MAC 表读物，不新造一套）。
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
		v := n.loop.observe(vs.Name, macPorts, len(rows), vs.LearnLimit)
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
