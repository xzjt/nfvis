package network

// govpp NAT44 客户端（M3-5 三）。

import (
	"fmt"

	"go.fd.io/govpp/api"
	"go.fd.io/govpp/binapi/interface_types"
	"go.fd.io/govpp/binapi/ip_types"
	"go.fd.io/govpp/binapi/nat44_ei"
)

// NatClientFunc 返回随当前连接获取 NAT44 客户端的工厂。
func (m *Manager) NatClientFunc() func() (NatClient, error) {
	return func() (NatClient, error) {
		ch, err := m.APIChannel()
		if err != nil {
			return nil, err
		}
		return &govppNatClient{ch: ch}, nil
	}
}

type govppNatClient struct{ ch api.Channel }

func (g *govppNatClient) Close() { g.ch.Close() }

func (g *govppNatClient) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	return (&govppL3Client{ch: g.ch}).SwInterfaceIndex(ifname)
}

// NATAddressRange 增删一个地址池地址段。vrfID 是地址池所属转发域，必须传**外部转发域**
// （与 outside 同一张表）：缺省 0 = 默认表，池会与 outside 的 VRF 不一致——包进了 NAT
// 却分配不出端口（round84 实测：out of ports、会话恒为 0、show nat44 ei addresses 显示
// tenant VRF: 0），全程无报错。删除方向必须传登记时所用的同一个 VRF，否则删不掉。
func (g *govppNatClient) NATAddressRange(add bool, first, last string, vrfID uint32) error {
	f, err := ip_types.ParseIP4Address(first)
	if err != nil {
		return fmt.Errorf("解析起始地址 %q: %w", first, err)
	}
	l, err := ip_types.ParseIP4Address(last)
	if err != nil {
		return fmt.Errorf("解析结束地址 %q: %w", last, err)
	}
	reply := &nat44_ei.Nat44EiAddDelAddressRangeReply{}
	if err := g.ch.SendRequest(&nat44_ei.Nat44EiAddDelAddressRange{
		FirstIPAddress: f, LastIPAddress: l, VrfID: vrfID, IsAdd: add,
	}).ReceiveReply(reply); err != nil {
		// add 方向「已存在」即已达目标状态：重放（恢复收敛、连接重建后的全量下发）必然重复
		// 下发同一地址池。此前只容忍 del 方向，于是重放收到 -81 后**整个 ApplyNAT 中止**，
		// 表现为一条 WARN + 未收敛项，而配置与 show nat 看似完全正常（round84 R84-20）。
		if add && vppErrIs(err, vppValueExist) {
			return nil
		}
		// 移除方向：对象本就不在（-6）/ 形式已存在（-81）/ 插件已关（-169）都属「已是目标状态」，
		// 按成功处理，避免一次无害的重复删除把整批 apply 打回滚（round84 缺陷 B）。
		if !add && natRemovalBenign(err) {
			return nil
		}
		return err
	}
	if reply.Retval != 0 {
		if add && reply.Retval == vppValueExist {
			return nil
		}
		if !add && natRemovalBenignCode(reply.Retval) {
			return nil
		}
		return fmt.Errorf("nat44_ei_add_del_address_range(%s-%s,vrf=%d,add=%v) retval=%d", first, last, vrfID, add, reply.Retval)
	}
	return nil
}

func (g *govppNatClient) NATFeature(swIfIndex uint32, inside, add bool) error {
	flags := nat44_ei.NAT44_EI_IF_OUTSIDE
	if inside {
		flags = nat44_ei.NAT44_EI_IF_INSIDE
	}
	reply := &nat44_ei.Nat44EiInterfaceAddDelFeatureReply{}
	if err := g.ch.SendRequest(&nat44_ei.Nat44EiInterfaceAddDelFeature{
		IsAdd: add, Flags: flags, SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
	}).ReceiveReply(reply); err != nil {
		// 移除方向容忍「已是目标状态」：接口被重建/特性已被别处摘除时 VPP 报 -6，
		// 插件已关时报 -169；二者都不改变数据面状态（round84 实测 -6 曾把整批 apply 打回滚）。
		if !add && natRemovalBenign(err) {
			return nil
		}
		return err
	}
	if reply.Retval != 0 {
		if !add && natRemovalBenignCode(reply.Retval) {
			return nil
		}
		return fmt.Errorf("nat44_ei_interface_add_del_feature(if=%d,inside=%v,add=%v) retval=%d", swIfIndex, inside, add, reply.Retval)
	}
	return nil
}

func (g *govppNatClient) NATEnable(enable bool, insideVRF, outsideVRF uint32) error {
	reply := &nat44_ei.Nat44EiPluginEnableDisableReply{}
	if err := g.ch.SendRequest(&nat44_ei.Nat44EiPluginEnableDisable{
		Enable: enable, InsideVrf: insideVRF, OutsideVrf: outsideVRF,
	}).ReceiveReply(reply); err != nil {
		if vppErrIs(err, vppFeatureAlreadyEnabled, vppFeatureAlreadyDisabled) {
			return nil
		}
		return err
	}
	if reply.Retval != 0 {
		// 已是目标状态同样按成功处理（非零 retval 也可能落在 Reply 上，两条路径都要判）。
		if reply.Retval == vppFeatureAlreadyEnabled || reply.Retval == vppFeatureAlreadyDisabled {
			return nil
		}
		return fmt.Errorf("nat44_ei_plugin_enable_disable(enable=%v,inside-vrf=%d,outside-vrf=%d) retval=%d",
			enable, insideVRF, outsideVRF, reply.Retval)
	}
	return nil
}

func (g *govppNatClient) NATInterfaceAddr(add bool, swIfIndex uint32) error {
	reply := &nat44_ei.Nat44EiAddDelInterfaceAddrReply{}
	if err := g.ch.SendRequest(&nat44_ei.Nat44EiAddDelInterfaceAddr{
		IsAdd: add, SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
	}).ReceiveReply(reply); err != nil {
		// VPP 对已在自动地址列表里的接口返回 -81（Value already exists）：目标状态已达成，
		// 按成功处理（同 NATAddressRange；只容忍 del 会让重放中止，R84-20）。
		if add && vppErrIs(err, vppValueExist) {
			return nil
		}
		if !add && natRemovalBenign(err) {
			return nil
		}
		return err
	}
	if reply.Retval != 0 {
		if add && reply.Retval == vppValueExist {
			return nil
		}
		if !add && natRemovalBenignCode(reply.Retval) {
			return nil
		}
		return fmt.Errorf("nat44_ei_add_del_interface_addr(if=%d,add=%v) retval=%d", swIfIndex, add, reply.Retval)
	}
	return nil
}

func (g *govppNatClient) NATStatic(add bool, inside, outside string) error {
	in, err := ip_types.ParseIP4Address(inside)
	if err != nil {
		return fmt.Errorf("解析内网地址 %q: %w", inside, err)
	}
	out, err := ip_types.ParseIP4Address(outside)
	if err != nil {
		return fmt.Errorf("解析外网地址 %q: %w", outside, err)
	}
	reply := &nat44_ei.Nat44EiAddDelStaticMappingReply{}
	if err := g.ch.SendRequest(&nat44_ei.Nat44EiAddDelStaticMapping{
		IsAdd:          add,
		Flags:          nat44_ei.NAT44_EI_STATIC_MAPPING,
		LocalIPAddress: in, ExternalIPAddress: out,
		Protocol:     ^uint8(0), // ~0 = 任意协议
		ExternalPort: ^uint16(0),
		LocalPort:    ^uint16(0),
	}).ReceiveReply(reply); err != nil {
		// 同 NATAddressRange：add 方向「已存在」= 该静态映射已在位，按成功处理（重放安全）；
		// 移除方向的本就不在/插件已关同样按已达成处理。
		if add && vppErrIs(err, vppValueExist) {
			return nil
		}
		if !add && natRemovalBenign(err) {
			return nil
		}
		return err
	}
	if reply.Retval != 0 {
		if add && reply.Retval == vppValueExist {
			return nil
		}
		if !add && natRemovalBenignCode(reply.Retval) {
			return nil
		}
		return fmt.Errorf("nat44_ei_add_del_static_mapping(%s→%s,add=%v) retval=%d", inside, outside, add, reply.Retval)
	}
	return nil
}

func (g *govppNatClient) NATSessions() ([]NATSession, error) {
	reqCtx := g.ch.SendMultiRequest(&nat44_ei.Nat44EiUserSessionDump{IPAddress: ip_types.IP4Address{}})
	var out []NATSession
	for {
		d := &nat44_ei.Nat44EiUserSessionDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return nil, err
		}
		if stop {
			break
		}
		out = append(out, NATSession{
			InsideIP:    d.InsideIPAddress.String(),
			InsidePort:  int(d.InsidePort),
			OutsideIP:   d.OutsideIPAddress.String(),
			OutsidePort: int(d.OutsidePort),
			Protocol:    int(d.Protocol),
			Bytes:       d.TotalBytes,
			Packets:     d.TotalPkts,
		})
	}
	return out, nil
}
