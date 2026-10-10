package network

// 决策 #443：恢复收敛告警族的**数据面中立**按来源廉价复核核心。
//
// 由来：这一族（recovery 作用域的 RECOVERY_UNCONVERGED / RECOVERY_IFACE_MISSING）的按来源
// 复核最早只落到 VPP 侧（决策 #333/#367，见 recovery_reconcile.go），内核数据面侧的
// ReconcileRecoveryAlarms 曾是空操作——而内核恢复重放**确实会落这一族告警**
// （netkernel/recovery.go 按同码族落），于是内核数据面下告警不随事实消解（v3 R7-1 真机实证：
// 交还端口后跨约 21 个 15s 巡检周期仍不消解）。本文件把判定逻辑抽成两数据面共用的核心：
// 各侧入口只负责提供「声明集 + 来源词表 + 廉价存在性查询」三类事实。
//
// 处置（决策 #333 明确**不做周期性全量重放**——重，且会把暂时性错误刷成告警抖动），
// 15s 巡检只做按来源的廉价复核：
//  1. 来源对象已不在 committed 配置 → 消解（失败前提已消失；数据面残渣另由残渣对账负责）；
//  2. RECOVERY_IFACE_MISSING 且来源 interfaces/<name> 且配置仍声明 → 用该数据面的
//     **廉价存在性查询**看该口是否已出现在数据面，已出现即消解（告警文案「接口不存在」
//     已与事实不符）；查询失败保守保留并上抛（问不出来 ≠ 已复原）；
//  3. 其余全部保守保留——尤其 **RECOVERY_UNCONVERGED 保留**：15s 路径证实不了
//     「现在能 apply 成功」（那要真重放一次才知道），不猜。权威的全量重放仍只在 nfvisd
//     启动/数据面（重）连时进行。
//
// 纪律（与 #333/#367 同）：**只复核（只调 Resolve）、绝不 Raise、绝不触发重放/Apply***；
// family 级（无对象名）、不可解析与不认识的来源一律保守保留。各侧来源清单与其恢复重放的
// 落点一一对应，映射不上的族不猜测。

import (
	"context"
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// RecoveryReconcileSpec 一次按来源复核所需的各数据面事实（决策 #443）。
type RecoveryReconcileSpec struct {
	// Families 二段来源（family/name）的声明集：family → 名字集合；未列出的 family 保守保留。
	Families map[string]map[string]bool
	// Switches virtual-switches/<n>/<leaf> 子来源的叶判定（leaf ∈ learn-limit|dhcp-relay|dhcp-server；
	// 交换机已删或叶已不在声明 ⇒ 消解）——两数据面共享这一份实现。
	Switches []model.VirtualSwitch
	// SubSource 其余三段及以上来源的复核（各数据面词表不同）：返回 true = 判定为已消解（Resolve）；
	// 不认识的形状返回 false（保守保留）。
	SubSource func(a orchestrator.AlarmRef) bool
	// IfaceAppeared interfaces/<name> + IFACE_MISSING + 配置仍声明时的廉价存在性查询。
	// err != nil ⇒ 保守保留并把错误上抛（问不出来 ≠ 已复原）。
	IfaceAppeared func(ctx context.Context, name string) (bool, error)
}

// RecoveryDeclSet 把对象切片按名字收成声明集（两侧共用；与 VPP 侧原 keyByName 同语义）。
func RecoveryDeclSet[T any](items []T, key func(T) string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[key(it)] = true
	}
	return m
}

// ReconcileRecoveryAlarmsCore 对 recovery 作用域中 RECOVERY_UNCONVERGED / RECOVERY_IFACE_MISSING
// 两码的活动告警逐条按来源复核（决策 #443；供两数据面的 ReconcileRecoveryAlarms 调用，
// 15s 巡检路径）。其余码（残渣码等）有独立对账路径，**绝不触碰**。
//
// 只复核（只调 Resolve）、绝不 Raise、绝不触发重放；返回存在性查询错误清单（调用方记日志），
// 查询失败的告警保守保留。
func ReconcileRecoveryAlarmsCore(ctx context.Context, store *AlarmStore, spec RecoveryReconcileSpec) []error {
	if store == nil {
		return nil
	}
	// 交换机名 → 声明（叶判定用；同名重复声明取先出现者，与两侧原实现逐字同口径）。
	byName := make(map[string]model.VirtualSwitch, len(spec.Switches))
	for i := range spec.Switches {
		vs := spec.Switches[i]
		if _, ok := byName[vs.Name]; !ok {
			byName[vs.Name] = vs
		}
	}

	var errs []error
	for _, a := range store.ActiveOf(recoveryScope) {
		// 残渣码（*LEFTOVER）有独立对账路径（residue.go），提交期码不属本族，均不在此处理。
		if a.Code != AlarmUnconverged && a.Code != AlarmIfaceMissing {
			continue
		}
		switch {
		case strings.HasPrefix(a.Source, "virtual-switches/") && strings.Count(a.Source, "/") >= 2:
			// 三段子来源（决策 #367，收口 R142-7）：virtual-switches/<sw>/<leaf>，
			// leaf ∈ {learn-limit, dhcp-relay, dhcp-server}（恢复重放的独立记源）。
			// 消解判据与恢复重放**同一口径**：重放根本不会再碰它（交换机已删，或子特性
			// 已不在声明）⇒ 告警前提已消失 ⇒ 消解。未知 leaf / 超过三段保守保留。
			sw, leaf, ok := strings.Cut(strings.TrimPrefix(a.Source, "virtual-switches/"), "/")
			if !ok || sw == "" || leaf == "" || strings.Contains(leaf, "/") {
				continue // 不可解析，保守保留
			}
			vs, declared := byName[sw]
			if !declared {
				store.Resolve(recoveryScope, a.Code, a.Source)
				continue
			}
			known, featureDeclared := false, false
			switch leaf {
			case "learn-limit":
				known, featureDeclared = true, vs.LearnLimit > 0
			case "dhcp-relay":
				known, featureDeclared = true, vs.DhcpRelayServer != ""
			case "dhcp-server":
				known, featureDeclared = true, vs.DHCPServerEnabled()
			}
			if known && !featureDeclared {
				store.Resolve(recoveryScope, a.Code, a.Source)
			}
		case strings.Count(a.Source, "/") >= 2:
			// 其余三段及以上来源：交各数据面词表（不认识的形状保守保留）。
			if spec.SubSource != nil && spec.SubSource(a) {
				store.Resolve(recoveryScope, a.Code, a.Source)
			}
		default:
			// 0 或 1 个斜杠：二段 family/name（或 family 级/不可解析）。
			family, name, ok := strings.Cut(a.Source, "/")
			if !ok || family == "" || name == "" {
				continue // family 级（forwarding / nat / lldp 等）与不可解析者，保守保留
			}
			set, mapped := spec.Families[family]
			if !mapped {
				continue // 未映射族，保守保留（不猜测）
			}
			if !set[name] {
				store.Resolve(recoveryScope, a.Code, a.Source)
				continue
			}
			// 仍声明：只有「接口缺失」这一种告警能被一次廉价存在性查询证实已恢复；
			// UNCONVERGED 无法证实「现在能 apply 成功」，一律保留（见文件头）。
			if a.Code == AlarmIfaceMissing && family == "interfaces" && spec.IfaceAppeared != nil {
				appeared, err := spec.IfaceAppeared(ctx, name)
				if err != nil {
					errs = append(errs, fmt.Errorf("复核接口 %s 是否已在数据面: %w", name, err))
					continue // 问不出来 ≠ 已复原，保守保留
				}
				if appeared {
					store.Resolve(recoveryScope, a.Code, a.Source)
				}
			}
		}
	}
	return errs
}
