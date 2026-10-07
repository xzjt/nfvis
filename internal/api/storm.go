package api

// 决策 #385：接口入向风暴抑制（storm control）的 CLI 读视图。
//
// 三面同源：配置字段走模型（`interfaces[].storm_control`，REST 同源）；数据面实况走
// `network.StormProvider` 的实测读数（policer_dump / classify_table_info /
// policer_classify_dump / stats 计数）；本文件只做渲染，不另查一遍。

import (
	"context"
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// StormRuntime 风暴抑制的数据面实况读物（决策 #385）。nil = 未接入（块内如实说明，
// 不编造实测值）。
type StormRuntime interface {
	StormDataplane(ctx context.Context, ifname string) (network.StormDataplane, bool)
}

// setStorm 注入风暴抑制数据面读物（CLI detail 块用）。
func (x *cliExecutor) setStorm(s StormRuntime) { x.storm = s }

// stormConfigSummary 配置侧摘要：只列已配置的类（0/缺省＝该类未配置）。
func stormConfigSummary(sc *model.StormControl) string {
	if sc == nil {
		return ""
	}
	parts := make([]string, 0, 2)
	if sc.BroadcastKbps > 0 {
		parts = append(parts, fmt.Sprintf("广播 %d kbps", sc.BroadcastKbps))
	}
	if sc.MulticastKbps > 0 {
		parts = append(parts, fmt.Sprintf("组播 %d kbps", sc.MulticastKbps))
	}
	return strings.Join(parts, " / ")
}

// stormKindLabel 类别中文名（与语句关键字对应）。
func stormKindLabel(kind string) string {
	if kind == network.StormKindMulticast {
		return "组播"
	}
	return "广播"
}

// stormControlBlock 渲染接口的风暴抑制块（配置 + 数据面实测）。返回文本行（可多行，
// 已含换行）与结构化视图（配置对象与 REST 同形；数据面实测另置 storm_control_runtime）。
//
// 如实口径：数据面读数不可核对（未接入/接口不在数据面/查询失败）时写明原因；计数读不到
// 时给出原因串（不显示 0 冒充「没有超速」）。
func (x *cliExecutor) stormControlBlock(ifc model.InterfaceConfig) (string, map[string]any) {
	sc := ifc.StormControl
	structCfg := map[string]any{}
	if sc.BroadcastKbps > 0 {
		structCfg["broadcast_kbps"] = sc.BroadcastKbps
	}
	if sc.MulticastKbps > 0 {
		structCfg["multicast_kbps"] = sc.MulticastKbps
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Storm control: %s（入向按目的 MAC 分类限速；超速丢弃）\n", stormConfigSummary(sc))

	out := map[string]any{"storm_control": structCfg}
	if x.storm == nil {
		b.WriteString("Storm control 数据面: 未接入（无法核对实测值）\n")
		return b.String(), out
	}
	dp, _ := x.storm.StormDataplane(context.Background(), ifc.Name)
	if !dp.Available {
		fmt.Fprintf(&b, "Storm control 数据面: 不可核对（%s）\n", dp.Reason)
		out["storm_control_runtime"] = map[string]any{"available": false, "reason": dp.Reason}
		return b.String(), out
	}
	rt := map[string]any{"available": true, "attached": dp.Attached}
	if dp.Attached {
		rt["attached_l2_table"] = dp.AttachedL2Table
	}
	// 数据面实况：逐类（配置的类必须给读数或如实说明；未配置的类不列）。
	kindLines := make([]string, 0, 2)
	counterLines := make([]string, 0, 2)
	kindRT := map[string]any{}
	for _, kind := range []string{network.StormKindBroadcast, network.StormKindMulticast} {
		if (kind == network.StormKindBroadcast && sc.BroadcastKbps == 0) ||
			(kind == network.StormKindMulticast && sc.MulticastKbps == 0) {
			continue
		}
		kd := dp.Kinds[kind]
		label := stormKindLabel(kind)
		one := map[string]any{"policer_present": kd.PolicerPresent}
		switch {
		case !kd.PolicerPresent:
			kindLines = append(kindLines, label+" policer 未在数据面（未收敛）")
		default:
			one["cir_kbps"] = kd.CirKbps
			line := fmt.Sprintf("%s policer 在（cir %d kbps）", label, kd.CirKbps)
			if kd.Table != nil {
				one["table"] = kd.Table
				line += fmt.Sprintf("；分类表 #%d（掩码 %s，会话 %d）", kd.Table.Index, kd.Table.Mask, kd.Table.Sessions)
			} else {
				// 表不在实况链上（决策 #401）：不再笼统报「登记缺失」——登记可能在，只是该口 L2
				// 槽上无本类分类表（被其它对象占用，或该类表未收敛）。
				line += "；分类表未挂（接口 L2 槽上无本类分类表——可能被其它对象占用或未收敛）"
			}
			kindLines = append(kindLines, line)
		}
		if kd.Counters != nil {
			one["counters"] = kd.Counters
			counterLines = append(counterLines, fmt.Sprintf("%s conform %d / exceed %d / violate %d",
				label, kd.Counters.ConformPackets, kd.Counters.ExceedPackets, kd.Counters.ViolatePackets))
		} else {
			one["counters_reason"] = kd.CountersReason
			counterLines = append(counterLines, fmt.Sprintf("%s 不可读（%s）", label, kd.CountersReason))
		}
		kindRT[kind] = one
	}
	rt["kinds"] = kindRT
	out["storm_control_runtime"] = rt
	fmt.Fprintf(&b, "Storm control 数据面: %s\n", strings.Join(kindLines, "；"))
	fmt.Fprintf(&b, "Storm control 计数: %s\n", strings.Join(counterLines, "；"))
	return b.String(), out
}
