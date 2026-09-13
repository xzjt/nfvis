package network

// M3-8：恢复收敛（FR-OPS-010/011）。
//
// 启动（首次连上 VPP）与 VPP 重启重连后，把 committed 配置逐对象重放到 VPP：
// 先清空各 Provider 的进程内登记表，使其按 VPP 实况重新判定对象是否存在并补齐
// （BD 经 bridge_domain_dump、ACL 经 acl_dump 按 tag、bond 经接口名查询），
// 再按事务 apply 的依赖顺序全量收敛。单个对象失败不阻塞其余对象，失败项转告警
// （GET /alarms）；配置引用的物理口被移除等不可收敛项标为 error 级别。
//
// 已知限制（附录 A #35）：govpp v0.13.0 的 sw_interface_details 无 bd_id，无法
// dump BD 成员/BVI 归属，故「nfvisd 进程内已登记」之外的成员关系（如跨进程删除
// 端口、已存在的 BVI）无法从 VPP 侧反查，只做补齐不做摘除；这类残留需删 BD 重建
// 或重启 VPP 消除。

import (
	"context"
	"errors"
	"fmt"

	"github.com/xzjt/nfvis/internal/model"
)

// ErrIfaceUnavailable 配置引用的接口在 VPP 中不存在（未由 DPDK 接管或被移除）。
// 属不可收敛项：恢复收敛据此转 error 级告警而非反复重试（FR-OPS-010）。
var ErrIfaceUnavailable = errors.New("接口在 VPP 中不存在")

// SetAlarms 注入告警表（恢复收敛的失败项落点）；未注入时仅返回错误列表。
func (n *L2Network) SetAlarms(a *AlarmStore) { n.alarms = a }

// EnsureConsistent 恢复收敛（FR-OPS-010/011）：把 committed 配置全量重放到 VPP。
// 返回未收敛项（调用方记日志即可，不据此拒绝服务）；不可收敛项同时进入告警表。
func (n *L2Network) EnsureConsistent(ctx context.Context, cfg model.Config) []error {
	if n == nil || n.l2 == nil {
		return nil
	}
	n.resetProviders()

	var (
		errs     []error
		failures []Alarm
	)
	record := func(source string, err error) {
		code, sev := AlarmUnconverged, SeverityWarning
		if errors.Is(err, ErrIfaceUnavailable) {
			code, sev = AlarmIfaceMissing, SeverityError
		}
		errs = append(errs, fmt.Errorf("%s: %w", source, err))
		failures = append(failures, Alarm{Severity: sev, Code: code, Message: err.Error(), Source: source})
	}

	// 顺序与事务 apply 计划一致：被引用对象先建（ACL/bond → BD/VRF → NAT → SPAN/QoS
	// → LLDP → 接口层），保证策略与地址在接口启用前就绪。
	for _, acl := range cfg.Acls {
		if err := n.ApplyACL(ctx, acl); err != nil {
			record("acls/"+acl.Name, err)
		}
	}
	for _, b := range cfg.Bonds {
		if err := n.ApplyBond(ctx, b); err != nil {
			record("bonds/"+b.Name, err)
		}
	}
	for _, vs := range cfg.VirtualSwitches {
		if vs.Type != "l2" { // L3 交换机经同名 Vrf 条目编排（附录 B）
			continue
		}
		if err := n.ApplyBridgeDomain(ctx, vs); err != nil {
			record("virtual-switches/"+vs.Name, err)
		}
	}
	for _, vrf := range cfg.Vrfs {
		if err := n.ApplyVRF(ctx, vrf); err != nil {
			record("vrfs/"+vrf.Name, err)
		}
	}
	if cfg.Nat != nil {
		if err := n.ApplyNAT(ctx, *cfg.Nat); err != nil {
			record("nat", err)
		}
	}
	for _, pm := range cfg.PortMirroring {
		if err := n.ApplySpan(ctx, pm); err != nil {
			record("port-mirroring/"+pm.Name, err)
		}
	}
	for _, q := range cfg.QosPolicies {
		if err := n.ApplyQos(ctx, q); err != nil {
			record("qos/policies/"+q.Name, err)
		}
	}
	if cfg.Protocols != nil {
		if err := n.ApplyLLDP(ctx, cfg.Protocols.LLDP); err != nil {
			record("protocols/lldp", err)
		}
	}
	for _, iface := range cfg.Interfaces {
		if err := n.ApplyInterface(ctx, iface); err != nil {
			record("interfaces/"+iface.Name, err)
		}
	}

	if n.alarms != nil {
		n.alarms.Sync(recoveryScope, failures)
	}
	return errs
}

// resetProviders 清空各 Provider 的进程内登记表，使本次收敛按 VPP 实况重新判定
// 对象存在性（避免跨进程/跨 VPP 重启后的陈旧登记表把重放带偏）。
func (n *L2Network) resetProviders() {
	if n.acl != nil {
		n.acl.reset()
	}
	if n.bond != nil {
		n.bond.reset()
	}
	if n.l2 != nil {
		n.l2.reset()
	}
	if n.l3 != nil {
		n.l3.reset()
	}
	if n.nat != nil {
		n.nat.reset()
	}
	if n.svc != nil {
		n.svc.reset()
	}
	if n.lldp != nil {
		n.lldp.reset()
	}
}
