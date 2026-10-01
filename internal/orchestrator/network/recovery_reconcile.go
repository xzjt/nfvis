package network

// 决策 #333（修 R111-1）：恢复收敛告警族（recovery 作用域的 RECOVERY_UNCONVERGED /
// RECOVERY_IFACE_MISSING）的按来源廉价复核。
//
// 由来（round111b 真机实测，2.0.0~dev21）：这一族告警只在 runRecovery（EnsureConsistent
// 末尾对 recovery 作用域的 Sync 全量覆盖）里重估，而那条路只挂在 vppMgr.OnConnect——
// 对象事实变化（配置声明已删、接口已出现在 VPP）之后告警不消解，与决策 #188/#321
// 的「告警随对象消失/状态恢复自动消解」口径不符。
//
// 处置（决策 #333 明确**不做周期性全量重放**——重，且会把暂时性错误刷成告警抖动），
// 15s 巡检只做按来源的廉价复核：
//  1. 来源对象已不在 committed 配置 → 消解（失败前提已消失；数据面残渣另由残渣对账负责）；
//  2. RECOVERY_IFACE_MISSING 且来源 interfaces/<name> 且配置仍声明 → 用既有
//     SwInterfaceIndex 存在性查询（dump 级代价）看该口是否已出现在 VPP，已出现即消解
//     （告警文案「接口不存在」已与事实不符）；查询失败保守保留并上抛；
//  3. 其余全部保守保留——尤其 **RECOVERY_UNCONVERGED 保留**：15s 路径证实不了
//     「现在能 apply 成功」（那要真重放一次才知道），不猜。权威的全量重放仍只在下次
//     VPP 重连（现状不变）。
//
// 本方法绝不 Raise 新告警、绝不触发重放/Apply*；来源清单与 EnsureConsistent 的
// record(...) 落点一一对应（recovery.go）。

import (
	"context"
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// ReconcileRecoveryAlarms 对 recovery 作用域的活动告警逐条做按来源的廉价复核（决策 #333，
// 供 15s 巡检与 ReconcileResidue 同块调用；EnsureConsistent 末尾的 Sync 全量覆盖不受影响）。
// 返回存在性查询错误清单（调用方记日志即可）；查询失败的告警保守保留（问不出来 ≠ 已复原）。
func (n *L2Network) ReconcileRecoveryAlarms(ctx context.Context, cfg model.Config) []error {
	if n == nil || n.alarms == nil {
		return nil
	}
	// 各来源族的「配置声明名」集合（整轮不变，一次算好）。
	acls := keyByName(cfg.Acls, func(a model.Acl) string { return a.Name })
	bonds := keyByName(cfg.Bonds, func(b model.Bond) string { return b.Name })
	switches := keyByName(cfg.VirtualSwitches, func(v model.VirtualSwitch) string { return v.Name })
	vrfs := keyByName(cfg.Vrfs, func(v model.Vrf) string { return v.Name })
	pms := keyByName(cfg.PortMirroring, func(p model.PortMirroring) string { return p.Name })
	qos := keyByName(cfg.QosPolicies, func(q model.QosPolicy) string { return q.Name })
	ifaces := keyByName(cfg.Interfaces, func(i model.InterfaceConfig) string { return i.Name })
	// vnf-ports/<vm>/<nic>：来源有两段，声明集由 VnfPortsOf 派生（与恢复收敛重放同一口径）。
	ports := orchestrator.VnfPortsOf(cfg, n.vhostDir, n.memifDir)

	var errs []error
	for _, a := range n.alarms.ActiveOf(recoveryScope) {
		// 残渣码（*LEFTOVER）有独立对账路径（residue.go），提交期码不属本族，均不在此处理。
		if a.Code != AlarmUnconverged && a.Code != AlarmIfaceMissing {
			continue
		}
		switch {
		case strings.HasPrefix(a.Source, "vnf-ports/"):
			vm, nic, ok := strings.Cut(strings.TrimPrefix(a.Source, "vnf-ports/"), "/")
			if !ok || vm == "" || nic == "" || strings.Contains(nic, "/") {
				continue // 不可解析，保守保留
			}
			if !vnfPortDeclared(ports, vm, nic) {
				n.alarms.Resolve(recoveryScope, a.Code, a.Source)
			}
		case strings.HasPrefix(a.Source, "qos/policies/"):
			// qos 族是两段家族名（qos/policies/<name>），先于通用解析处理。
			if name := strings.TrimPrefix(a.Source, "qos/policies/"); name != "" && !qos[name] {
				n.alarms.Resolve(recoveryScope, a.Code, a.Source)
			}
		default:
			family, name, ok := strings.Cut(a.Source, "/")
			if !ok || family == "" || name == "" || strings.Contains(name, "/") {
				continue // family 级（ip-tables / nat / residue-scan / 裸 bonds 等）与不可解析者，保守保留
			}
			var set map[string]bool
			switch family {
			case "acls":
				set = acls
			case "bonds":
				set = bonds
			case "virtual-switches":
				set = switches
			case "vrfs":
				set = vrfs
			case "port-mirroring":
				set = pms
			case "interfaces":
				set = ifaces
			default:
				continue // protocols/lldp 等未映射族，保守保留（不猜测）
			}
			if !set[name] {
				n.alarms.Resolve(recoveryScope, a.Code, a.Source)
				continue
			}
			// 仍声明：只有「接口缺失」这一种告警能被一次 dump 级查询廉价证实已恢复；
			// UNCONVERGED 无法证实「现在能 apply 成功」，一律保留（见文件头）。
			if a.Code == AlarmIfaceMissing && family == "interfaces" && n.svc != nil {
				exists, err := n.svc.InterfaceExists(name)
				if err != nil {
					errs = append(errs, fmt.Errorf("复核接口 %s 是否已在 VPP: %w", name, err))
					continue // 问不出来 ≠ 已复原，保守保留
				}
				if exists {
					n.alarms.Resolve(recoveryScope, a.Code, a.Source)
				}
			}
		}
	}
	return errs
}

// vnfPortDeclared 该 (vm, nic) 是否仍在配置声明的 vNIC 接入清单里。
func vnfPortDeclared(ports []orchestrator.VnfPort, vm, nic string) bool {
	for _, p := range ports {
		if p.VM == vm && p.Interface == nic {
			return true
		}
	}
	return false
}

// keyByName 把对象切片按名字收成集合（本文件的声明集口径专用）。
func keyByName[T any](items []T, key func(T) string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[key(it)] = true
	}
	return m
}
