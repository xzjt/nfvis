package network

// M3-3：把 L2 编排接入 orchestrator.NetworkProvider（其余方法沿用基础实现）。

import (
	"context"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// L2Network 在基础 NetworkProvider 上覆盖 L2 虚拟交换机编排。
type L2Network struct {
	orchestrator.NetworkProvider // 其余方法（ACL/NAT/SPAN/QoS/VRF）沿用基础实现
	l2                           *L2Provider
}

// NewL2Network 以基础 Provider 与 L2 编排器构造装饰器。
func NewL2Network(base orchestrator.NetworkProvider, l2 *L2Provider) *L2Network {
	if base == nil {
		base = orchestrator.NewNoopNetwork()
	}
	return &L2Network{NetworkProvider: base, l2: l2}
}

func (n *L2Network) ApplyBridgeDomain(ctx context.Context, vs model.VirtualSwitch) error {
	return n.l2.ApplyBridgeDomain(ctx, vs)
}

func (n *L2Network) DeleteBridgeDomain(ctx context.Context, name string) error {
	return n.l2.DeleteBridgeDomain(ctx, name)
}

// MACTable 供 /virtual-switches/{name}/mac-table 运行态查询（M3-3/M3-7）。
func (n *L2Network) MACTable(ctx context.Context, name string) ([]MACTableEntry, error) {
	return n.l2.MACTable(ctx, name)
}
