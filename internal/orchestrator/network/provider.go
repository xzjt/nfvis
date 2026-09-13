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
