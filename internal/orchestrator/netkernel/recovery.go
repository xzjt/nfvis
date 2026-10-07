package netkernel

import (
	"context"
	"fmt"

	"github.com/xzjt/nfvis/internal/model"
)

// EnsureConsistent 恢复收敛：把 committed 配置按序重放到内核。
//
// 与 VPP 实现的差别（如实说明）：内核是唯一事实源，本实现**不做「对比数据面实况再补差」**，
// 而是把声明式下发整体重放一遍——所有下发方法本身幂等（`ip link add` 容忍已存在、
// `ip addr add`/`ip route replace`/`nft add rule` 幂等），重放的结果与内核实况一致；
// nfvisd 重启后读视图直接查内核，也不需要靠重放重建进程内登记。
//
// 返回不可收敛项（逐条带对象路径），由调用方转告警，不阻塞启动。
func (p *Provider) EnsureConsistent(ctx context.Context, cfg model.Config) []error {
	p.SetConfig(cfg)
	var errs []error
	collect := func(path string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
		}
	}
	// 绑定族对象先于接口/三层接口重放：接口声明里只写策略名，解析需要策略本体
	// （与提交编排 plan 的段序一致：ACL → 镜像 → QoS → 接口）。
	for _, acl := range cfg.Acls {
		collect("acls/"+acl.Name, p.ApplyACL(ctx, acl))
	}
	for _, pm := range cfg.PortMirroring {
		collect("port-mirroring/"+pm.Name, p.ApplySpan(ctx, pm))
	}
	for _, q := range cfg.QosPolicies {
		collect("qos/"+q.Name, p.ApplyQos(ctx, q))
	}
	for _, iface := range cfg.Interfaces {
		collect("interfaces/"+iface.Name, p.ApplyInterface(ctx, iface))
	}
	for _, bond := range cfg.Bonds {
		collect("bonds/"+bond.Name, p.ApplyBond(ctx, bond))
	}
	for _, vs := range cfg.VirtualSwitches {
		collect("virtual-switches/"+vs.Name, p.ApplyBridgeDomain(ctx, vs))
	}
	for _, vrf := range cfg.Vrfs {
		collect("vrfs/"+vrf.Name, p.ApplyVRF(ctx, vrf))
	}
	for _, vx := range cfg.VxlanTunnels {
		collect("vxlan/"+vx.Name, p.ApplyVxlan(ctx, vx, nil))
	}
	if cfg.Nat != nil {
		collect("nat", p.ApplyNAT(ctx, *cfg.Nat))
	}
	return errs
}
