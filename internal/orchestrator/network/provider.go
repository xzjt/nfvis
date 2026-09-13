package network

// M3-3/M3-4：把 L2/L3 编排接入 orchestrator.NetworkProvider（其余沿用基础实现）。

import (
	"context"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// L2Network 在基础 NetworkProvider 上覆盖 L2 虚拟交换机与 L3/VRF 编排。
type L2Network struct {
	orchestrator.NetworkProvider // 其余方法（ACL/NAT/SPAN/QoS）沿用基础实现
	l2                           *L2Provider
	l3                           *L3Provider
	svc                          *ServicesProvider
	acl                          *AclProvider
	nat                          *NatProvider
	bond                         *BondProvider
	lldp                         *LldpProvider
	alarms                       *AlarmStore // 恢复收敛失败项落点（M3-8，可空）
}

// NewL2Network 以基础 Provider 与 L2 编排器构造装饰器。
func NewL2Network(base orchestrator.NetworkProvider, l2 *L2Provider) *L2Network {
	if base == nil {
		base = orchestrator.NewNoopNetwork()
	}
	return &L2Network{NetworkProvider: base, l2: l2}
}

// SetL3 追加 L3/VRF 编排（BVI 网关随 L2 交换机一并处理）。
func (n *L2Network) SetL3(l3 *L3Provider) { n.l3 = l3 }

// SetServices 追加 SPAN/QoS/接口编排（M3-5）。
func (n *L2Network) SetServices(svc *ServicesProvider) { n.svc = svc }

// SetACL 追加 ACL 编排（M3-5 二），并把索引查询注入 L2/L3 以绑定端口/接口。
func (n *L2Network) SetACL(a *AclProvider) {
	n.acl = a
	if n.l2 != nil {
		n.l2.SetACL(a)
	}
	if n.l3 != nil {
		n.l3.SetACL(a)
	}
}

// SetNAT 追加 NAT44 编排（M3-5 三），并把 L2 挂接表注入为 inside 接口来源。
func (n *L2Network) SetNAT(p *NatProvider) {
	n.nat = p
	if p != nil && n.l3 != nil {
		p.SetInsideResolver(n.l3.AttachedIfaces) // NAT 仅作用于 L3 交换机
	}
}

// SetBond 追加 bond 编排（M3-6）。
func (n *L2Network) SetBond(p *BondProvider) { n.bond = p }

// SetLldp 追加 LLDP 编排（M3-6）。
func (n *L2Network) SetLldp(p *LldpProvider) { n.lldp = p }

func (n *L2Network) ApplyBond(ctx context.Context, bond model.Bond) error {
	if n.bond == nil {
		return nil
	}
	return n.bond.ApplyBond(ctx, bond)
}

func (n *L2Network) DeleteBond(ctx context.Context, name string) error {
	if n.bond == nil {
		return nil
	}
	return n.bond.DeleteBond(ctx, name)
}

func (n *L2Network) ApplyLLDP(ctx context.Context, lldpCfg *model.LldpConfig) error {
	if n.lldp == nil {
		return nil
	}
	return n.lldp.ApplyLLDP(ctx, lldpCfg)
}

// LldpNeighbors 供 /protocols/lldp/neighbors 运行态（M3-6）。
func (n *L2Network) LldpNeighbors(ctx context.Context) ([]LldpNeighbor, error) {
	if n.lldp == nil {
		return nil, nil
	}
	return n.lldp.Neighbors(ctx)
}

func (n *L2Network) ApplyNAT(ctx context.Context, nat model.NatConfig) error {
	if n.nat == nil {
		return nil
	}
	return n.nat.ApplyNAT(ctx, nat)
}

// NATSessions 供 /nat 运行态展示（M3-7）。
func (n *L2Network) NATSessions(ctx context.Context) ([]NATSession, error) {
	if n.nat == nil {
		return nil, nil
	}
	return n.nat.Sessions(ctx)
}

func (n *L2Network) ApplyACL(ctx context.Context, acl model.Acl) error {
	if n.acl == nil {
		return nil
	}
	return n.acl.ApplyACL(ctx, acl)
}

func (n *L2Network) DeleteACL(ctx context.Context, name string) error {
	if n.acl == nil {
		return nil
	}
	return n.acl.DeleteACL(ctx, name)
}

func (n *L2Network) ApplyInterface(ctx context.Context, iface model.InterfaceConfig) error {
	if n.svc == nil {
		return n.NetworkProvider.ApplyInterface(ctx, iface)
	}
	return n.svc.ApplyInterface(ctx, iface)
}

func (n *L2Network) ApplySpan(ctx context.Context, pm model.PortMirroring) error {
	if n.svc == nil {
		return nil
	}
	return n.svc.ApplySpan(ctx, pm)
}

func (n *L2Network) DeleteSpan(ctx context.Context, name string) error {
	if n.svc == nil {
		return nil
	}
	return n.svc.DeleteSpan(ctx, name)
}

func (n *L2Network) ApplyQos(ctx context.Context, q model.QosPolicy) error {
	if n.svc == nil {
		return nil
	}
	return n.svc.ApplyQos(ctx, q)
}

func (n *L2Network) DeleteQos(ctx context.Context, name string) error {
	if n.svc == nil {
		return nil
	}
	return n.svc.DeleteQos(ctx, name)
}

func (n *L2Network) ApplyBridgeDomain(ctx context.Context, vs model.VirtualSwitch) error {
	if err := n.l2.ApplyBridgeDomain(ctx, vs); err != nil {
		return err
	}
	if n.l3 != nil {
		return n.l3.ApplyGateway(ctx, vs) // 有 Gateway 才动作（FR-NET-014）
	}
	return nil
}

func (n *L2Network) DeleteBridgeDomain(ctx context.Context, name string) error {
	if n.l3 != nil {
		if err := n.l3.DeleteGateway(ctx, name); err != nil {
			return err
		}
	}
	return n.l2.DeleteBridgeDomain(ctx, name)
}

func (n *L2Network) ApplyVRF(ctx context.Context, vrf model.Vrf) error {
	if n.l3 == nil {
		return nil
	}
	return n.l3.ApplyVRF(ctx, vrf)
}

func (n *L2Network) DeleteVRF(ctx context.Context, name string) error {
	if n.l3 == nil {
		return nil
	}
	return n.l3.DeleteVRF(ctx, name)
}

// MACTable 供 /virtual-switches/{name}/mac-table 运行态查询（M3-3）。
func (n *L2Network) MACTable(ctx context.Context, name string) ([]MACTableEntry, error) {
	return n.l2.MACTable(ctx, name)
}

// Routes 供 /vrfs/{name}/routes 运行态查询（M3-4）。
func (n *L2Network) Routes(ctx context.Context, name string) ([]RouteEntry, error) {
	if n.l3 == nil {
		return nil, ErrL3Unavailable
	}
	return n.l3.Routes(ctx, name)
}
