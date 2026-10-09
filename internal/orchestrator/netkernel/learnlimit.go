package netkernel

import (
	"context"
	"fmt"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// CheckLearnLimits 内核数据面下 learn-limit 的巡检（决策 #435）。
//
// 口径（严格照契约）：内核 bridge 没有「学习条数上限」原语（不像 VPP 的
// `bridge_domain_set_learn_limit`），故产品把 `learn-limit <n>` 实现为**阈值 + 告警**
// （**不**强制限制学习、也不伪造「已限速」）：
//
//   - 对**声明了 learn-limit 的 L2 交换机**数 `bridge fdb` 学到的条目数（复用既有
//     `Runtime.MACTable`——它已按「排除 bridge 自身条目与 `self` 条目」过滤，直接用其长度）；
//   - 计数 **≥ n** 时建告警 `BRIDGE_FDB_LIMIT_REACHED`（warning，source=交换机名）；
//   - 计数降到 < n、或声明/对象消失即自动消解（沿用既有对账清警口径：`ResolveStale` 按
//     声明集合清理滞留告警，与 #333/#188 同族）；
//   - 计数**取不到**（命令失败/解析失败）时**不误报、也不据此消警**——该交换机仍在声明集合里，
//     其既有告警由 `ResolveStale` 保留（不会被当成「声明已删」清掉），只把读取失败如实报出。
//
// 返回的错误只用于日志（读取失败等），告警一律走 AlarmStore（不当成 error 返回）。
func (p *Provider) CheckLearnLimits(ctx context.Context, cfg model.Config) []error {
	if p.alarms == nil {
		return nil
	}
	var errs []error
	// expect 收集「当前声明了 learn-limit 的交换机名」——对账清警据此判定哪些源仍应保留。
	// 计数取不到的交换机**也进 expect**：它只是「暂时判不了」，不是「声明已删」，不能据此消警。
	expect := map[string]bool{}
	for _, vs := range cfg.VirtualSwitches {
		if vs.Type == "l3" || vs.LearnLimit <= 0 {
			continue
		}
		expect[vs.Name] = true
		rows, err := p.rt().MACTable(ctx, vs.Name)
		if err != nil {
			// 计数取不到：如实说明不可读，不建警、也不消警（保留既有告警）。
			errs = append(errs, fmt.Errorf(
				"交换机 %s 的 MAC 学习表读取失败，learn-limit 阈值暂不可判定: %w", vs.Name, err))
			continue
		}
		if len(rows) >= vs.LearnLimit {
			p.alarms.Raise(alarmScopeLearnLimit, network.SeverityWarning, AlarmBridgeFdbLimitReached,
				fmt.Sprintf("交换机 %s 的 MAC 学习表已学 %d 条，达到/超过声明的 learn-limit 阈值 %d"+
					"（内核数据面下为阈值告警、非强制上限；自查：bridge fdb show br %s）",
					vs.Name, len(rows), vs.LearnLimit, LinkName(vs.Name)), vs.Name)
			continue
		}
		// 计数回落到阈值以下：自动消解（同一 scope/code/source）。
		p.alarms.Resolve(alarmScopeLearnLimit, AlarmBridgeFdbLimitReached, vs.Name)
	}
	// 对账清警：声明已删/对象消失的交换机，其滞留告警一并消解（round86/#188 口径）。
	orchestrator.ResolveStale(p.alarms, alarmScopeLearnLimit, expect)
	return errs
}
