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

func (g *govppNatClient) NATAddressRange(add bool, first, last string) error {
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
		FirstIPAddress: f, LastIPAddress: l, IsAdd: add,
	}).ReceiveReply(reply); err != nil {
		if !add && vppErrIs(err, vppValueExist) {
			return nil
		}
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("nat44_ei_add_del_address_range(%s-%s,add=%v) retval=%d", first, last, add, reply.Retval)
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
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("nat44_ei_interface_add_del_feature(if=%d,inside=%v,add=%v) retval=%d", swIfIndex, inside, add, reply.Retval)
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
		if !add && vppErrIs(err, vppValueExist) {
			return nil
		}
		return err
	}
	if reply.Retval != 0 {
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
