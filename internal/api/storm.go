package api

// 决策 #385：接口入向风暴抑制（storm control）的 CLI 读视图。
//
// 三面同源：配置字段走模型（`interfaces[].storm_control`，REST 同源）；数据面实况走
// `network.StormProvider` 的实测读数（policer_dump / classify_table_info /
// policer_classify_dump / stats 计数）；本文件只做渲染，不另查一遍。
//
// 绑定事实按**四态**渲染（决策 #421④）：实况回读（权威）/按登记（本底座 policer-classify
// 绑定不可回读：`classify_table_by_interface` 恒回 NONE、`policer_classify_dump` 恒 0 条目，
// 而 vppctl 里绑定在场——阴性不可判，不能当「未挂」）/自认领（进程登记丢失后按 policer 按名
// 在场 + 表链匹配从数据面认回，归属是推断）/未挂。四态在文本与结构化输出里都如实分列、
// 各自给依据，绝不把「按登记」「自认领」冒充成实况回读。

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
// 时给出原因串（不显示 0 冒充「没有超速」）；绑定事实按四态分列（实况回读 / 按登记 / 自认领 /
// 未挂，见文件头），结构化输出同源给 binding / binding_basis / declared_l2_table /
// adopted_l2_table / table_declared / table_adopted / table_source，另有只识别不删的
// 分类表候选（orphan_candidates）与 policer 候选（orphan_policers）。
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
	// VPP 的"接口 L2 槽上挂哪张分类表"是 VPP 侧概念：内核数据面（tc 过滤器）没有这层结构，
	// 不发射这个字段，避免读视图出现一个恒 0 的假读数（R2-15②）。
	kernel := x.dpMode() == model.DataPlaneKernel
	if dp.Attached && !kernel {
		rt["attached_l2_table"] = dp.AttachedL2Table
	}
	// 绑定事实四态（决策 #421④）：live = 实况回读（权威）；declared = 按登记（本底座读不到
	// 该绑定）；adopted = 自认领（登记丢失后按 policer 按名在场 + 表链匹配从数据面认回，
	// 归属是推断）；none = 未挂。内核数据面没有这层结构（Binding 为空串）——不发射该字段。
	if !kernel && dp.Binding != "" {
		rt["binding"] = dp.Binding
		switch dp.Binding {
		case network.StormBindingLive:
			rt["binding_basis"] = "实况回读"
		case network.StormBindingDeclared:
			rt["binding_basis"] = "按登记（本数据面版本读不到该绑定）"
			rt["declared_l2_table"] = dp.DeclaredTable
		case network.StormBindingAdopted:
			rt["binding_basis"] = "自认领（policer 按名在场 + 表链匹配）"
			rt["adopted_l2_table"] = dp.AdoptedTable
		}
	}
	// 分类表候选（只识别不删，决策 #421 收口②）：形状属本产品却不被任何绑定/登记/自认领覆盖
	// 的表——本数据面版本不可回读绑定、保护集不可证，故如实列出而不删（随数据面重启自然消失）。
	if !kernel && len(dp.OrphanCandidates) > 0 {
		rt["orphan_candidates"] = dp.OrphanCandidates
	}
	// policer 候选（只识别不删，决策 #433）：数据面存在 `nfvis-storm-` 前缀、却不被任何声明/
	// 登记/自认领覆盖的 policer——与分类表候选同族（保护集不可证），如实列出而不删。
	if !kernel && len(dp.OrphanPolicers) > 0 {
		rt["orphan_policers"] = dp.OrphanPolicers
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
			switch {
			case kd.Table != nil:
				one["table"] = kd.Table
				one["table_source"] = "实况回读"
				line += fmt.Sprintf("；分类表 #%d（实况回读；掩码 %s，会话 %d）", kd.Table.Index, kd.Table.Mask, kd.Table.Sessions)
			case kd.TableByRegistration != nil:
				// 按登记（决策 #421④）：本底座读不到该绑定，但登记在位且登记的表仍在数据面
				//——如实标注来源与依据，不与实况回读混同。
				one["table_declared"] = kd.TableByRegistration
				one["table_source"] = "按登记"
				line += fmt.Sprintf("；分类表 #%d 按登记在位（本数据面版本读不到该绑定；掩码 %s，会话 %d）",
					kd.TableByRegistration.Index, kd.TableByRegistration.Mask, kd.TableByRegistration.Sessions)
			case kd.TableAdopted != nil:
				// 自认领（决策 #421 收口③）：登记丢失后按「policer 按名在场 + 表链匹配」从数据面
				// 认回的表——归属是推断，读视图把依据一并给出来，不与实况回读混同。
				one["table_adopted"] = kd.TableAdopted
				one["table_source"] = "自认领（policer 按名在场 + 表链匹配）"
				line += fmt.Sprintf("；分类表 #%d 自认领在位（依据 policer 按名在场 + 表链匹配；掩码 %s，会话 %d）",
					kd.TableAdopted.Index, kd.TableAdopted.Mask, kd.TableAdopted.Sessions)
			case kernel:
				// 内核数据面没有 VPP 的"分类表 / 接口 L2 槽"（限速就是 tc 过滤器本身）：
				// 不套用 VPP 话术，否则会把"正在限速"描述成"分类表未挂（未收敛）"。
				line += "；限速落在内核 tc 入向过滤器上"
			case dp.Binding == network.StormBindingLive || dp.Attached:
				// 实况回读是权威：读到了阳性绑定但没有本类表（槽被其它对象占用，或该类未收敛）。
				line += "；分类表未挂（接口 L2 槽上无本类分类表——可能被其它对象占用或未收敛）"
			default:
				// 实况阴性（本底座读不到该绑定）且登记也不能证明在位：只能如实说「两样都没有」，
				// 不能断言「槽上无本类表」（那正是把阴性当结论）。
				line += "；分类表未挂（本数据面版本读不到该绑定，且无有效下发登记）"
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
	if !kernel && len(dp.OrphanCandidates) > 0 {
		fmt.Fprintf(&b, "Storm control 分类表候选: %s（未被绑定或登记覆盖；本数据面版本不可回读绑定，只识别不删）\n",
			stormIDsLine(dp.OrphanCandidates))
	}
	if !kernel && len(dp.OrphanPolicers) > 0 {
		fmt.Fprintf(&b, "Storm control policer 候选: %s（未被声明或登记覆盖；本数据面版本不可回读绑定，只识别不删）\n",
			stormNamesLine(dp.OrphanPolicers))
	}
	return b.String(), out
}

// stormIDsLine 分类表索引列表渲染成「#3、#4」（候选行用）。
func stormIDsLine(ids []uint32) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("#%d", id))
	}
	return strings.Join(parts, "、")
}

// stormNamesLine policer 名列表渲染成「name、name」（候选行用；仿 stormIDsLine）。
func stormNamesLine(names []string) string {
	return strings.Join(names, "、")
}
