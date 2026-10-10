package network

// 决策 #333（修 R111-1）：恢复收敛告警族（recovery 作用域的 RECOVERY_UNCONVERGED /
// RECOVERY_IFACE_MISSING）的按来源廉价复核——VPP 侧入口。
//
// 由来（round111b 真机实测，2.0.0~dev21）：这一族告警只在 runRecovery（EnsureConsistent
// 末尾对 recovery 作用域的 Sync 全量覆盖）里重估，而那条路只挂在 vppMgr.OnConnect——
// 对象事实变化（配置声明已删、接口已出现在 VPP）之后告警不消解，与决策 #188/#321
// 的「告警随对象消失/状态恢复自动消解」口径不符。
//
// 决策 #367（收口 R142-7）：#333 的复核漏了三类**三段**子来源——
// `virtual-switches/<n>/learn-limit|dhcp-relay|dhcp-server`（recovery.go 的独立记源）。
//
// 决策 #443：判定逻辑已抽到数据面中立的 recovery_reconcile_core.go（内核侧复用同一份），
// 本文件只负责构建 **VPP 侧**的声明集与三段来源词表，VPP 行为逐字不变（既有单测守护）：
//   - 六族二段来源：acls / bonds / virtual-switches / vrfs / port-mirroring / interfaces；
//   - 三段词表：vnf-ports/<vm>/<nic>（声明集由 VnfPortsOf 派生，与恢复收敛重放同一口径）、
//     qos/policies/<name>（两段家族名，先于通用解析处理）；
//   - 接口存在性：既有 ServicesProvider.InterfaceExists（dump 级代价）。

import (
	"context"
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
	acls := RecoveryDeclSet(cfg.Acls, func(a model.Acl) string { return a.Name })
	bonds := RecoveryDeclSet(cfg.Bonds, func(b model.Bond) string { return b.Name })
	switches := RecoveryDeclSet(cfg.VirtualSwitches, func(v model.VirtualSwitch) string { return v.Name })
	vrfs := RecoveryDeclSet(cfg.Vrfs, func(v model.Vrf) string { return v.Name })
	pms := RecoveryDeclSet(cfg.PortMirroring, func(p model.PortMirroring) string { return p.Name })
	qos := RecoveryDeclSet(cfg.QosPolicies, func(q model.QosPolicy) string { return q.Name })
	ifaces := RecoveryDeclSet(cfg.Interfaces, func(i model.InterfaceConfig) string { return i.Name })
	// vnf-ports/<vm>/<nic>：来源有两段，声明集由 VnfPortsOf 派生（与恢复收敛重放同一口径）。
	ports := orchestrator.VnfPortsOf(cfg, n.vhostDir, n.memifDir)

	return ReconcileRecoveryAlarmsCore(ctx, n.alarms, RecoveryReconcileSpec{
		Families: map[string]map[string]bool{
			"acls":             acls,
			"bonds":            bonds,
			"virtual-switches": switches,
			"vrfs":             vrfs,
			"port-mirroring":   pms,
			"interfaces":       ifaces,
		},
		Switches: cfg.VirtualSwitches,
		SubSource: func(a orchestrator.AlarmRef) bool {
			// vnf-ports/<vm>/<nic>：该 vNIC 不再声明 ⇒ 前提消失 ⇒ 消解。
			if strings.HasPrefix(a.Source, "vnf-ports/") {
				vm, nic, ok := strings.Cut(strings.TrimPrefix(a.Source, "vnf-ports/"), "/")
				if !ok || vm == "" || nic == "" || strings.Contains(nic, "/") {
					return false // 不可解析，保守保留
				}
				return !vnfPortDeclared(ports, vm, nic)
			}
			// qos 族是两段家族名（qos/policies/<name>），先于通用解析处理。
			if strings.HasPrefix(a.Source, "qos/policies/") {
				name := strings.TrimPrefix(a.Source, "qos/policies/")
				return name != "" && !qos[name]
			}
			return false // 其余多段来源保守保留（不认识的形状不猜测）
		},
		IfaceAppeared: func(_ context.Context, name string) (bool, error) {
			if n.svc == nil {
				return false, nil // 无 services 面：问不出来 ⇒ 保守保留
			}
			return n.svc.InterfaceExists(name)
		},
	})
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
