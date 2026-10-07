package netkernel

import (
	"context"
	"fmt"

	"github.com/xzjt/nfvis/internal/model"
)

// DefaultVxlanDstPort VXLAN 缺省目的端口（与内核 vxlan 驱动缺省一致）。
const DefaultVxlanDstPort = 4789

// ApplyVxlan 收敛一条 VXLAN 隧道为内核 vxlan 设备。
//
// prev 非 nil 且元组与本次不同时，先按旧元组删设备再按新元组建（内核 vxlan 的 local/remote/vni
// 不可原地改）。VirtualSwitch 非空时把隧道口挂进该交换机对应的 bridge。
func (p *Provider) ApplyVxlan(ctx context.Context, t model.VxlanTunnel, prev *model.VxlanTunnel) error {
	if prev != nil && !sameVxlanTuple(*prev, t) {
		if err := p.DeleteVxlan(ctx, *prev); err != nil {
			return err
		}
	}
	dev := LinkName(t.Name)
	port := t.DstPort
	if port == 0 {
		port = DefaultVxlanDstPort
	}
	if err := p.ipIdem(ctx, "link", "add", dev, "type", "vxlan", "id", fmt.Sprint(t.Vni),
		"local", t.Local, "remote", t.Remote, "dstport", fmt.Sprint(port)); err != nil {
		return err
	}
	if err := p.ipReq(ctx, "link", "set", "dev", dev, "up"); err != nil {
		return err
	}
	if t.VirtualSwitch != "" {
		if err := p.ipReq(ctx, "link", "set", "dev", dev, "master", LinkName(t.VirtualSwitch)); err != nil {
			return err
		}
	}
	return nil
}

// DeleteVxlan 撤销一条 VXLAN 隧道（先出 bridge 再删设备）。
func (p *Provider) DeleteVxlan(ctx context.Context, t model.VxlanTunnel) error {
	dev := LinkName(t.Name)
	if err := p.ipBest(ctx, "link", "set", "dev", dev, "nomaster"); err != nil {
		return err
	}
	return p.ipBest(ctx, "link", "del", dev)
}

// sameVxlanTuple 两条隧道的下垫/标识元组是否一致（一致则无需重建）。
func sameVxlanTuple(a, b model.VxlanTunnel) bool {
	return a.Vni == b.Vni && a.Local == b.Local && a.Remote == b.Remote &&
		effectiveDstPort(a.DstPort) == effectiveDstPort(b.DstPort) && a.VirtualSwitch == b.VirtualSwitch
}

func effectiveDstPort(p int) int {
	if p == 0 {
		return DefaultVxlanDstPort
	}
	return p
}
