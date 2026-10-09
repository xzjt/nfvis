package netkernel

import (
	"context"
	"fmt"

	"github.com/xzjt/nfvis/internal/model"
)

// ApplyBridgeDomain 收敛一台 L2 虚拟交换机为内核 bridge。
//
// 映射：交换机名 → bridge 接口名；成员口 `ip link set master`；access/trunk/native 由
// `bridge vlan` 表达（仅在声明了 VLAN 时打开 vlan_filtering，避免无谓改变默认转发）。
// 网关（BVI 等价物）地址直接落在 bridge 上，bridge 再按网关 VRF 入表。
//
// type=l3 的交换机不建 bridge（其 VRF 设备由 ApplyVRF 收敛），如实返回成功。
func (p *Provider) ApplyBridgeDomain(ctx context.Context, vs model.VirtualSwitch) error {
	if vs.Type == "l3" {
		return nil
	}
	br := LinkName(vs.Name)
	if err := p.ipIdem(ctx, "link", "add", "name", br, "type", "bridge"); err != nil {
		return err
	}
	if err := p.ipReq(ctx, "link", "set", "dev", br, "type", "bridge", "vlan_filtering", vlanFilteringValue(vs)); err != nil {
		return err
	}
	if vs.Description != "" {
		if err := p.ipReq(ctx, "link", "set", "dev", br, "alias", vs.Description); err != nil {
			return err
		}
	}

	// 成员口：声明集合为准，同时释放已不在声明里的旧成员（ApplyBridgeDomain 是全量语义）。
	declared := map[string]bool{}
	for _, port := range vs.Ports {
		name, ok := memberLinkName(port)
		if !ok {
			// vNIC 成员由 libvirt 的 bridge 接入自行 enslave（见 ApplyVnfInterface）；
			// 容器 vNIC 在内核数据面无对应物，提交期已拒绝。
			continue
		}
		declared[name] = true
		if err := p.ipReq(ctx, "link", "set", "dev", name, "master", br); err != nil {
			return err
		}
		if err := p.ensureLinkUp(ctx, name); err != nil {
			return err
		}
		if err := p.applyPortVlans(ctx, name, vs, port); err != nil {
			return err
		}
	}
	for _, cur := range p.bridgeMembers(ctx, br) {
		if declared[cur] {
			continue
		}
		// 只摘**本产品 enslave 的成员**：libvirt 自建的 VM tap（vnetN，`info_kind=tun`）与
		// ApplyVxlan 自管的隧道口（vxlan）不由本方法 enslave，一律不碰——旧实现按内核实况
		// 全量摘除，任何一次交换机重放（服务重启/升级/给同台交换机加端口）都会把运行中 VM 的
		// tap 从 bridge 摘掉：VM 仍在跑、产品零报错，宿主到 guest 的 L2 静默断流（R2-1，P0）。
		// 物理口/子接口没有 info_kind（不是虚拟设备），属正常可释放成员。
		row, ok := p.linkDetail(ctx, cur)
		if !ok {
			// 读不到设备详情：不猜、不摘（保守）——摘错的代价是运行中 VM 的 L2 静默断流，
			// 留下的陈旧成员至少是读视图可见的（且该口转作其它角色时归属会被自然改走）。
			continue
		}
		if row.LinkInfo != nil && (row.LinkInfo.InfoKind == "tun" || row.LinkInfo.InfoKind == "vxlan") {
			continue
		}
		if err := p.ipBest(ctx, "link", "set", "dev", cur, "nomaster"); err != nil {
			return err
		}
	}

	if vs.Gateway != nil && len(vs.Gateway.Addresses) > 0 {
		if err := p.applyGateway(ctx, vs, br); err != nil {
			return err
		}
	}
	return p.ensureLinkUp(ctx, br)
}

// vlanFilteringValue 是否需要打开 bridge 的 VLAN 过滤。
func vlanFilteringValue(vs model.VirtualSwitch) string {
	if vs.VlanAccess > 0 {
		return "1"
	}
	for _, port := range vs.Ports {
		if port.NativeVlan > 0 || len(port.TrunkVlans) > 0 {
			return "1"
		}
	}
	return "0"
}

// memberLinkName 端口声明对应的内核成员口名；ok=false 表示该端口不由本方法 enslave。
func memberLinkName(port model.VSwitchPort) (string, bool) {
	switch {
	case port.Interface != "":
		return LinkName(port.Interface), true
	case port.Vnf != "":
		return "", false // libvirt 自建 tap 并挂 bridge
	default:
		return "", false
	}
}

// applyPortVlans 按声明收敛成员口的 VLAN 条目（先按实况清、再按声明加，保证删除也能收敛）。
//
// VLAN 表由 `bridge` 命令维护（**不是** `ip vlan`——后者不存在）；`bridge vlan add/del`
// 在真机上本身幂等/容错（重复 add、删不存在的 vid 都返回 0）。
func (p *Provider) applyPortVlans(ctx context.Context, port string, vs model.VirtualSwitch, spec model.VSwitchPort) error {
	want := map[int]vlanSpec{}
	if spec.NativeVlan > 0 {
		want[spec.NativeVlan] = vlanSpec{pvid: true, untagged: true}
	}
	for _, vid := range spec.TrunkVlans {
		if _, ok := want[vid]; !ok {
			want[vid] = vlanSpec{}
		}
	}
	if vs.VlanAccess > 0 {
		want[vs.VlanAccess] = vlanSpec{pvid: true, untagged: true}
	}
	for _, vid := range p.bridgePortVlans(ctx, port) {
		if _, ok := want[vid]; ok {
			continue
		}
		// 本交换机没声明任何 VLAN 时保留默认 VID 1（VLAN 过滤本就关着，动它没有意义）。
		if vid == 1 && len(want) == 0 {
			continue
		}
		if err := p.bridgeIdem(ctx, "vlan", "del", "dev", port, "vid", fmt.Sprint(vid)); err != nil {
			return err
		}
	}
	for vid, s := range want {
		args := []string{"vlan", "add", "dev", port, "vid", fmt.Sprint(vid)}
		if s.pvid {
			args = append(args, "pvid")
		}
		if s.untagged {
			args = append(args, "untagged")
		}
		if err := p.bridgeIdem(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

type vlanSpec struct{ pvid, untagged bool }

// applyGateway 下发 L2 交换机的三层网关：地址落在 bridge 上，bridge 入网关 VRF。
func (p *Provider) applyGateway(ctx context.Context, vs model.VirtualSwitch, br string) error {
	vrfName := vs.Gateway.Vrf
	if vrfName == "" {
		vrfName = GatewayVRFName(vs.Name)
	} else {
		vrfName = LinkName(vrfName)
	}
	if err := p.ensureVRF(ctx, vrfName); err != nil {
		return err
	}
	if err := p.ipReq(ctx, "link", "set", "dev", br, "master", vrfName); err != nil {
		return err
	}
	for _, cidr := range vs.Gateway.Addresses {
		if err := p.ipIdem(ctx, "addr", "replace", cidr, "dev", br); err != nil {
			return err
		}
	}
	return nil
}

// DeleteBridgeDomain 删除内核 bridge（成员口由内核自动释放），并回收**它自己创建的**网关 VRF。
//
// 网关 VRF 由 ApplyBridgeDomain 按需创建（无显式 vrf 声明时用派生的 `vr-<交换机名>`）——
// 删交换机时必须一并回收，否则内核里长期留一张空 VRF（真机走查实测的残留：
// 删了 vs-lan，`vr-vs-lan` 还在）。显式声明的 `gateway vrf <名>` 不在此删除：
// 那是操作者自己的对象，生命周期不由本交换机决定。
func (p *Provider) DeleteBridgeDomain(ctx context.Context, name string) error {
	// 决策 #438：先停该交换机的 DHCP 服务器——内置 tap 是 bridge 成员口、单播接收 socket 绑在
	// bridge 的 BVI 地址上（reference 先解、被引用者后删，与 #437 relay/#196/#342 的删除倒序一致）。
	// 未启用/从未收敛时幂等空操作。
	srv, hub := p.dhcpComponents()
	if err := hub.Stop(name); err != nil {
		return err
	}
	if err := srv.Sync(ctx, model.VirtualSwitch{Name: name}); err != nil {
		return err
	}
	// 决策 #437：先停该交换机的 DHCP 中继实例（它引用 bridge 的收发路径与 BVI 地址）——
	// 先解引用、后删被引用，与 #196/#342 的删除倒序一致。无实例/未声明时幂等空操作。
	if err := p.relayMgr().Stop(name); err != nil {
		return err
	}
	if err := p.ipBest(ctx, "link", "del", LinkName(name)); err != nil {
		return err
	}
	return p.ipBest(ctx, "link", "del", GatewayVRFName(name))
}
