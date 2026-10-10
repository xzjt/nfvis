package netkernel

// 决策 #443：内核数据面下恢复收敛告警的按来源廉价复核（收口 R7-1）。
//
// 由来（round7 真机实证）：走数据面切换固定动作（切内核时两口仍在 vfio-pci 手里）时，内核
// 恢复重放对 `interfaces/<n>` 失败 → 落 RECOVERY_IFACE_MISSING；两口交还内核、事实愈合后
// 告警**跨约 21 个 15s 巡检周期仍不消解**（文案与事实相反），直到下一次全量重放才清。
// 根因：内核侧 ReconcileRecoveryAlarms 曾是空操作（api_surface.go，按「VPP 侧登记型对账、
// 内核无对应族」处理），而内核恢复重放**确实会落这一族告警**（recovery.go 按同码族落）——
// #333/#367 的「告警随事实消解」纪律只落到了 VPP 侧。本文件按与 VPP 侧**同一份核心**
// （network.ReconcileRecoveryAlarmsCore）补齐。
//
// 来源词表＝内核恢复重放（recovery.go 的 collect 落点）：
//   - 二段 family/name：acls/、qos/、bonds/、virtual-switches/、interfaces/、
//     port-mirroring/、vrfs/、vxlan/（键名与 recovery.go 的 source 前缀逐字一致）；
//   - 三段 container-functions/<owner>/interfaces/<iface>：声明集由 containerVethSpecsOf
//     派生（与恢复重放**同一派生口径**：容器/接口声明已删 ⇒ 消解）；
//   - virtual-switches/<sw>/<leaf> 叶判定走共享核心（决策 #367 口径）；
//   - family 级（forwarding/lldp/dns-proxy/nat/virtual-switches）与不可解析者保守保留。
//
// 纪律与 VPP 侧同（#333/#367）：只复核（只调 Resolve）、不 Raise、不触发重放；接口存在性
// 查询失败 ⇒ 保守保留并上抛（问不出来 ≠ 已复原）。本方法**不读配置发动机**（p.config()）：
// 与 apply/巡检路径同一条硬约束（apply 路径读发动机即重入自死锁，见 provider.go 的
// configSnapshot 注释），事实一律来自调用点给的 cfg 快照与内核实况。

import (
	"context"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

func (p *Provider) ReconcileRecoveryAlarms(ctx context.Context, cfg model.Config) []error {
	if p == nil || p.alarms == nil {
		return nil
	}
	// 容器 vNIC 声明集：owner/iface 键与恢复重放的记源拼装逐字同口径。
	declared := map[string]bool{}
	for _, spec := range containerVethSpecsOf(cfg) {
		declared[spec.owner+"/"+spec.iface] = true
	}
	return network.ReconcileRecoveryAlarmsCore(ctx, p.alarms, network.RecoveryReconcileSpec{
		Families: map[string]map[string]bool{
			"acls":             network.RecoveryDeclSet(cfg.Acls, func(a model.Acl) string { return a.Name }),
			"qos":              network.RecoveryDeclSet(cfg.QosPolicies, func(q model.QosPolicy) string { return q.Name }),
			"bonds":            network.RecoveryDeclSet(cfg.Bonds, func(b model.Bond) string { return b.Name }),
			"virtual-switches": network.RecoveryDeclSet(cfg.VirtualSwitches, func(v model.VirtualSwitch) string { return v.Name }),
			"interfaces":       network.RecoveryDeclSet(cfg.Interfaces, func(i model.InterfaceConfig) string { return i.Name }),
			"port-mirroring":   network.RecoveryDeclSet(cfg.PortMirroring, func(pm model.PortMirroring) string { return pm.Name }),
			"vrfs":             network.RecoveryDeclSet(cfg.Vrfs, func(v model.Vrf) string { return v.Name }),
			"vxlan":            network.RecoveryDeclSet(cfg.VxlanTunnels, func(v model.VxlanTunnel) string { return v.Name }),
		},
		Switches: cfg.VirtualSwitches,
		SubSource: func(a orchestrator.AlarmRef) bool {
			if !strings.HasPrefix(a.Source, "container-functions/") {
				return false // 其余三段来源保守保留（不认识的形状不猜测）
			}
			owner, iface, ok := strings.Cut(strings.TrimPrefix(a.Source, "container-functions/"), "/interfaces/")
			if !ok || owner == "" || iface == "" || strings.Contains(iface, "/") {
				return false // 不可解析，保守保留
			}
			return !declared[owner+"/"+iface]
		},
		IfaceAppeared: func(ctx context.Context, name string) (bool, error) {
			// 内核实况（`ip -d -j link show`）：口在接口清单里即「已出现」。
			states, err := p.rt().InterfaceStates(ctx)
			if err != nil {
				return false, err // 问不出来 ⇒ 调用方保守保留并上抛
			}
			_, ok := states[name]
			return ok, nil
		},
	})
}
