package network

// govpp L3 客户端（M3-4）：唯一使用 ip/interface/l2 BVI binapi 的地方。

import (
	"fmt"

	"go.fd.io/govpp/api"
	"go.fd.io/govpp/binapi/fib_types"
	ifapi "go.fd.io/govpp/binapi/interface"
	"go.fd.io/govpp/binapi/interface_types"
	"go.fd.io/govpp/binapi/ip"
	"go.fd.io/govpp/binapi/ip_types"
	"go.fd.io/govpp/binapi/l2"
)

// L3ClientFunc 返回随当前连接获取 L3 客户端的工厂。
func (m *Manager) L3ClientFunc() func() (L3Client, error) {
	return func() (L3Client, error) {
		ch, err := m.APIChannel()
		if err != nil {
			return nil, err
		}
		return &govppL3Client{ch: ch}, nil
	}
}

type govppL3Client struct{ ch api.Channel }

func (g *govppL3Client) Close() { g.ch.Close() }

func (g *govppL3Client) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	reqCtx := g.ch.SendMultiRequest(&ifapi.SwInterfaceDump{})
	for {
		d := &ifapi.SwInterfaceDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return 0, false, err
		}
		if stop {
			return 0, false, nil
		}
		if d.InterfaceName == ifname || d.Tag == ifname {
			return uint32(d.SwIfIndex), true, nil
		}
	}
}

func (g *govppL3Client) CreateSubif(req CreateSubifReq) (uint32, error) {
	return (&govppL2Client{ch: g.ch}).CreateSubif(req)
}

func (g *govppL3Client) IPTableAddDel(tableID uint32, isIP6, add bool, name string) error {
	reply := &ip.IPTableAddDelReply{}
	err := g.ch.SendRequest(&ip.IPTableAddDel{
		IsAdd: add,
		Table: ip.IPTable{TableID: tableID, IsIP6: isIP6, Name: name},
	}).ReceiveReply(reply)
	if err != nil {
		if add && vppErrIs(err, vppTableExist) {
			return nil
		}
		return err
	}
	if reply.Retval != 0 {
		if add && reply.Retval == vppTableExist {
			return nil
		}
		return fmt.Errorf("ip_table_add_del(table=%d,ip6=%v,add=%v) retval=%d", tableID, isIP6, add, reply.Retval)
	}
	return nil
}

func (g *govppL3Client) SwInterfaceSetTable(swIfIndex uint32, isIP6 bool, tableID uint32) error {
	reply := &ifapi.SwInterfaceSetTableReply{}
	err := g.ch.SendRequest(&ifapi.SwInterfaceSetTable{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex), IsIPv6: isIP6, VrfID: tableID,
	}).ReceiveReply(reply)
	if err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("sw_interface_set_table(if=%d,vrf=%d) retval=%d", swIfIndex, tableID, reply.Retval)
	}
	return nil
}

func (g *govppL3Client) SwInterfaceAddDelAddress(swIfIndex uint32, prefix string, add, delAll bool) error {
	req := &ifapi.SwInterfaceAddDelAddress{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
		IsAdd:     add,
		DelAll:    delAll,
	}
	if !delAll {
		p, err := ip_types.ParseAddressWithPrefix(prefix)
		if err != nil {
			return fmt.Errorf("解析地址 %q: %w", prefix, err)
		}
		req.Prefix = p
	}
	reply := &ifapi.SwInterfaceAddDelAddressReply{}
	if err := g.ch.SendRequest(req).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("sw_interface_add_del_address(if=%d,%s) retval=%d", swIfIndex, prefix, reply.Retval)
	}
	return nil
}

func (g *govppL3Client) IPRouteAddDel(tableID uint32, prefix, nextHop string, add bool) error {
	p, err := ip_types.ParsePrefix(prefix)
	if err != nil {
		return fmt.Errorf("解析前缀 %q: %w", prefix, err)
	}
	route := ip.IPRoute{TableID: tableID, Prefix: p}
	if nextHop != "" {
		addr, err := ip_types.ParseAddress(nextHop)
		if err != nil {
			return fmt.Errorf("解析下一跳 %q: %w", nextHop, err)
		}
		var nh ip_types.AddressUnion
		proto := fib_types.FIB_API_PATH_NH_PROTO_IP4
		switch addr.Af {
		case ip_types.ADDRESS_IP6:
			nh = ip_types.AddressUnionIP6(addr.Un.GetIP6())
			proto = fib_types.FIB_API_PATH_NH_PROTO_IP6
		default:
			nh = ip_types.AddressUnionIP4(addr.Un.GetIP4())
		}
		route.NPaths = 1
		route.Paths = []fib_types.FibPath{{
			SwIfIndex: ^uint32(0), // ~0 = 经下一跳递归解析
			Proto:     proto,
			Nh:        fib_types.FibPathNh{Address: nh},
		}}
	}
	reply := &ip.IPRouteAddDelReply{}
	if err := g.ch.SendRequest(&ip.IPRouteAddDel{IsAdd: add, Route: route}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("ip_route_add_del(%s via %s, table=%d) retval=%d", prefix, nextHop, tableID, reply.Retval)
	}
	return nil
}

// fibNhString 从 FIB path 解析下一跳文本（IPv4/IPv6）。
func fibNhString(p fib_types.FibPath) string {
	switch p.Proto {
	case fib_types.FIB_API_PATH_NH_PROTO_IP4:
		return p.Nh.Address.GetIP4().String()
	case fib_types.FIB_API_PATH_NH_PROTO_IP6:
		return p.Nh.Address.GetIP6().String()
	}
	return ""
}

func (g *govppL3Client) Routes(tableID uint32) ([]RouteEntry, error) {
	reqCtx := g.ch.SendMultiRequest(&ip.IPRouteDump{Table: ip.IPTable{TableID: tableID}})
	var out []RouteEntry
	for {
		d := &ip.IPRouteDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return nil, err
		}
		if stop {
			break
		}
		e := RouteEntry{Prefix: d.Route.Prefix.String()}
		if len(d.Route.Paths) > 0 {
			e.NextHop = fibNhString(d.Route.Paths[0])
		}
		out = append(out, e)
	}
	return out, nil
}

// SetState 置接口管理员状态（BVI 与数据口一致：默认 down，必须显式 up）。
func (g *govppL3Client) SetState(swIfIndex uint32, up bool) error {
	var flags interface_types.IfStatusFlags // 0 = down；仅 ADMIN_UP 位表示 up
	if up {
		flags = interface_types.IF_STATUS_API_FLAG_ADMIN_UP
	}
	reply := &ifapi.SwInterfaceSetFlagsReply{}
	if err := g.ch.SendRequest(&ifapi.SwInterfaceSetFlags{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
		Flags:     flags,
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("sw_interface_set_flags(if=%d,up=%v) retval=%d", swIfIndex, up, reply.Retval)
	}
	return nil
}

func (g *govppL3Client) BviCreate() (uint32, error) {
	reply := &l2.BviCreateReply{}
	if err := g.ch.SendRequest(&l2.BviCreate{UserInstance: ^uint32(0)}).ReceiveReply(reply); err != nil {
		return 0, err
	}
	if reply.Retval != 0 {
		return 0, fmt.Errorf("bvi_create retval=%d", reply.Retval)
	}
	return uint32(reply.SwIfIndex), nil
}

// BviOfBD 返回 bridge domain 上既有 BVI 的 sw_if_index（无则 found=false）。
// 全量 dump 后按 BdID 匹配：带 BdID 过滤的 dump 在 VPP 26.06 上会返回空。
func (g *govppL3Client) BviOfBD(bdID uint32) (uint32, bool, error) {
	reqCtx := g.ch.SendMultiRequest(&l2.BridgeDomainDump{
		BdID:      bdID,
		SwIfIndex: interface_types.InterfaceIndex(0xFFFFFFFF),
	})
	for {
		d := &l2.BridgeDomainDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return 0, false, err
		}
		if stop {
			return 0, false, nil
		}
		if d.BdID == bdID && d.BviSwIfIndex != 0 &&
			d.BviSwIfIndex != interface_types.InterfaceIndex(0xFFFFFFFF) {
			return uint32(d.BviSwIfIndex), true, nil
		}
	}
}

func (g *govppL3Client) BviDelete(swIfIndex uint32) error {
	reply := &l2.BviDeleteReply{}
	if err := g.ch.SendRequest(&l2.BviDelete{SwIfIndex: interface_types.InterfaceIndex(swIfIndex)}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("bvi_delete(%d) retval=%d", swIfIndex, reply.Retval)
	}
	return nil
}

// BviSetBD 把 BVI 挂入 bridge domain（port type BVI，FR-NET-014）。
func (g *govppL3Client) BviSetBD(swIfIndex, bdID uint32) error {
	reply := &l2.SwInterfaceSetL2BridgeReply{}
	if err := g.ch.SendRequest(&l2.SwInterfaceSetL2Bridge{
		RxSwIfIndex: interface_types.InterfaceIndex(swIfIndex),
		BdID:        bdID,
		PortType:    l2.L2_API_PORT_TYPE_BVI,
		Enable:      true,
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("sw_interface_set_l2_bridge(BVI %d, bd=%d) retval=%d", swIfIndex, bdID, reply.Retval)
	}
	return nil
}
