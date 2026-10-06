package network

// govpp VXLAN 客户端（决策 #383）：唯一使用 vxlan binapi 的地方。
//
// 消息选取（按本仓库锁定的 govpp binapi，即 VPP 25.10 生成的 vxlan 2.1.0 口径）：
//   - 建/撤隧道用 **vxlan_add_del_tunnel_v3**（当前版本；带 src_port/dst_port 与 is_l3，
//     产品只用 v2 的字段 + is_l3=false——与 vppctl `create vxlan tunnel` 同一条消息）；
//   - dump 用 **vxlan_tunnel_v2_dump/vxlan_tunnel_v2_details**（当前版本的 dump；
//     产品读 instance/src/dst/src_port/dst_port/vni/sw_if_index）。
//   v2/v1 的建撤消息在本 binapi 里已标 deprecated，不用（真机若报消息版本不符，改这里即可）。

import (
	"fmt"

	"go.fd.io/govpp/api"
	ifapi "go.fd.io/govpp/binapi/interface"
	"go.fd.io/govpp/binapi/interface_types"
	"go.fd.io/govpp/binapi/l2"
	"go.fd.io/govpp/binapi/vxlan"
)

// VxlanClientFunc 返回随当前连接获取 VXLAN 客户端的工厂（断线重连后自动用新 channel）。
func (m *Manager) VxlanClientFunc() func() (VxlanClient, error) {
	return func() (VxlanClient, error) {
		ch, err := m.APIChannel()
		if err != nil {
			return nil, err
		}
		return &govppVxlanClient{ch: ch}, nil
	}
}

type govppVxlanClient struct{ ch api.Channel }

func (g *govppVxlanClient) Close() { g.ch.Close() }

// TunnelAddDel 建/撤隧道。地址用 ip_types.ParseIP4Address 的 v4 形态（产品 v1 只做 IPv4
// 下垫层；非 v4 在模型校验层已被拒绝，这里再兜底报错，不静默发零值）。
func (g *govppVxlanClient) TunnelAddDel(isAdd bool, instance uint32, vni uint32, src, dst string, dstPort uint16) (uint32, error) {
	srcAddr, err := v4Address(src)
	if err != nil {
		return 0, fmt.Errorf("隧道本地地址: %w", err)
	}
	dstAddr, err := v4Address(dst)
	if err != nil {
		return 0, fmt.Errorf("隧道远端地址: %w", err)
	}
	reply := &vxlan.VxlanAddDelTunnelV3Reply{}
	if err := g.ch.SendRequest(&vxlan.VxlanAddDelTunnelV3{
		IsAdd:      isAdd,
		Instance:   instance,
		SrcAddress: srcAddr,
		DstAddress: dstAddr,
		SrcPort:    0, // 0 = 默认源端口（不参与匹配的注册端口）
		DstPort:    dstPort,
		Vni:        vni,
		IsL3:       false, // v1 只做 L2 成员
	}).ReceiveReply(reply); err != nil {
		return 0, err
	}
	if reply.Retval != 0 {
		return 0, fmt.Errorf("vxlan_add_del_tunnel_v3(add=%v,instance=%d,vni=%d,%s→%s,port=%d) retval=%d",
			isAdd, instance, vni, src, dst, dstPort, reply.Retval)
	}
	return uint32(reply.SwIfIndex), nil
}

// TunnelDump 列出 VPP 里实际的 VXLAN 隧道（multi-request 读到 stop 为止；
// 与同包其它 dump 的读法一致）。
func (g *govppVxlanClient) TunnelDump() ([]VxlanTunnelInfo, error) {
	reqCtx := g.ch.SendMultiRequest(&vxlan.VxlanTunnelV2Dump{})
	out := make([]VxlanTunnelInfo, 0, 4)
	for {
		d := &vxlan.VxlanTunnelV2Details{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return nil, err
		}
		if stop {
			return out, nil
		}
		out = append(out, VxlanTunnelInfo{
			Instance:  d.Instance,
			Vni:       d.Vni,
			Src:       d.SrcAddress.String(),
			Dst:       d.DstAddress.String(),
			SrcPort:   d.SrcPort,
			DstPort:   d.DstPort,
			SwIfIndex: uint32(d.SwIfIndex),
		})
	}
}

// SetInterfaceUp 置接口 up（VPP 新建隧道默认 down，不置 up 则报文发不出去）。
func (g *govppVxlanClient) SetInterfaceUp(swIfIndex uint32) error {
	reply := &ifapi.SwInterfaceSetFlagsReply{}
	if err := g.ch.SendRequest(&ifapi.SwInterfaceSetFlags{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
		Flags:     interface_types.IF_STATUS_API_FLAG_ADMIN_UP,
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("sw_interface_set_flags(if=%d,up=true) retval=%d", swIfIndex, reply.Retval)
	}
	return nil
}

// SetL2Bridge 把隧道口加入/移出 bridge-domain（普通成员口，shg=0；与 L2 编排同一条消息）。
func (g *govppVxlanClient) SetL2Bridge(swIfIndex, bdID uint32, enable bool) error {
	reply := &l2.SwInterfaceSetL2BridgeReply{}
	if err := g.ch.SendRequest(&l2.SwInterfaceSetL2Bridge{
		RxSwIfIndex: interface_types.InterfaceIndex(swIfIndex),
		BdID:        bdID,
		PortType:    l2.L2PortType(L2PortNormal),
		Shg:         0,
		Enable:      enable,
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("sw_interface_set_l2_bridge(if=%d,bd=%d,enable=%v) retval=%d", swIfIndex, bdID, enable, reply.Retval)
	}
	return nil
}
