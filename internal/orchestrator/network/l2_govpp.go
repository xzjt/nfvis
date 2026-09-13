package network

// govpp L2 客户端（M3-3）：唯一使用 l2/interface binapi 的地方，供 L2Provider 调用。

import (
	"fmt"
	"net"

	"go.fd.io/govpp/api"
	ifapi "go.fd.io/govpp/binapi/interface"
	"go.fd.io/govpp/binapi/interface_types"
	"go.fd.io/govpp/binapi/l2"
)

// APIChannel 打开一条 binary API channel（未连接时返回 ErrL2Unavailable）。
func (m *Manager) APIChannel() (api.Channel, error) {
	m.mu.Lock()
	s := m.session
	m.mu.Unlock()
	if s == nil {
		return nil, ErrL2Unavailable
	}
	cs, ok := s.(interface{ APIChannel() (api.Channel, error) })
	if !ok {
		return nil, ErrL2Unavailable
	}
	return cs.APIChannel()
}

func (s *govppSession) APIChannel() (api.Channel, error) { return s.conn.NewAPIChannel() }

// L2ClientFunc 返回随当前连接获取 L2 客户端的工厂（断线重连后自动用新 channel）。
func (m *Manager) L2ClientFunc() func() (L2Client, error) {
	return func() (L2Client, error) {
		ch, err := m.APIChannel()
		if err != nil {
			return nil, err
		}
		return &govppL2Client{ch: ch}, nil
	}
}

type govppL2Client struct{ ch api.Channel }

func (g *govppL2Client) Close() { g.ch.Close() }

func (g *govppL2Client) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	req := &ifapi.SwInterfaceDump{}
	reqCtx := g.ch.SendMultiRequest(req)
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

func (g *govppL2Client) SwInterfaceNames() (map[uint32]SwIfInfo, error) {
	reqCtx := g.ch.SendMultiRequest(&ifapi.SwInterfaceDump{})
	names := map[uint32]SwIfInfo{}
	for {
		d := &ifapi.SwInterfaceDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return nil, err
		}
		if stop {
			return names, nil
		}
		names[uint32(d.SwIfIndex)] = SwIfInfo{Name: d.InterfaceName, OuterVlanID: d.SubOuterVlanID}
	}
}

func (g *govppL2Client) BridgeDomainAddDel(bdID uint32, add, learn bool, tag string) error {
	reply := &l2.BridgeDomainAddDelReply{}
	err := g.ch.SendRequest(&l2.BridgeDomainAddDel{
		BdID: bdID, Flood: true, UuFlood: true, Forward: true, Learn: learn,
		BdTag: tag, IsAdd: add,
	}).ReceiveReply(reply)
	if err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("bridge_domain_add_del retval=%d", reply.Retval)
	}
	return nil
}

func (g *govppL2Client) SwInterfaceSetL2Bridge(swIfIndex, bdID uint32, portType L2PortType, shg uint8, enable bool) error {
	reply := &l2.SwInterfaceSetL2BridgeReply{}
	err := g.ch.SendRequest(&l2.SwInterfaceSetL2Bridge{
		RxSwIfIndex: interface_types.InterfaceIndex(swIfIndex),
		BdID:        bdID,
		PortType:    l2.L2PortType(portType),
		Shg:         shg,
		Enable:      enable,
	}).ReceiveReply(reply)
	if err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("sw_interface_set_l2_bridge(if=%d,bd=%d) retval=%d", swIfIndex, bdID, reply.Retval)
	}
	return nil
}

func (g *govppL2Client) SwInterfaceSetL2Xconnect(swIfIndex, bdID uint32, enable bool) error {
	reply := &l2.SwInterfaceSetL2XconnectReply{}
	err := g.ch.SendRequest(&l2.SwInterfaceSetL2Xconnect{
		RxSwIfIndex: interface_types.InterfaceIndex(swIfIndex),
		TxSwIfIndex: interface_types.InterfaceIndex(bdID), // 复用参数：cross-connect 对端口
		Enable:      enable,
	}).ReceiveReply(reply)
	if err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("sw_interface_set_l2_xconnect(if=%d,tx=%d) retval=%d", swIfIndex, bdID, reply.Retval)
	}
	return nil
}

func (g *govppL2Client) CreateSubif(req CreateSubifReq) (uint32, error) {
	var flags interface_types.SubIfFlags
	if req.OneTag {
		flags |= interface_types.SUB_IF_API_FLAG_ONE_TAG
	}
	if req.ExactMatch {
		flags |= interface_types.SUB_IF_API_FLAG_EXACT_MATCH
	}
	if req.DefaultSub {
		flags |= interface_types.SUB_IF_API_FLAG_DEFAULT
	}
	reply := &ifapi.CreateSubifReply{}
	err := g.ch.SendRequest(&ifapi.CreateSubif{
		SwIfIndex:   interface_types.InterfaceIndex(req.ParentSwIfIndex),
		SubID:       req.SubID,
		SubIfFlags:  flags,
		OuterVlanID: req.OuterVlanID,
	}).ReceiveReply(reply)
	if err != nil {
		return 0, err
	}
	if reply.Retval != 0 {
		return 0, fmt.Errorf("create_subif(parent=%d,sub=%d,vlan=%d) retval=%d",
			req.ParentSwIfIndex, req.SubID, req.OuterVlanID, reply.Retval)
	}
	return uint32(reply.SwIfIndex), nil
}

func (g *govppL2Client) L2InterfaceVlanTagRewrite(req VlanTagRewriteReq) error {
	reply := &l2.L2InterfaceVlanTagRewriteReply{}
	err := g.ch.SendRequest(&l2.L2InterfaceVlanTagRewrite{
		SwIfIndex: interface_types.InterfaceIndex(req.SwIfIndex),
		VtrOp:     req.Op,
		PushDot1q: req.PushDot1Q,
		Tag1:      req.Tag1,
	}).ReceiveReply(reply)
	if err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("l2_interface_vlan_tag_rewrite(if=%d) retval=%d", req.SwIfIndex, reply.Retval)
	}
	return nil
}

func (g *govppL2Client) MACTable(bdID uint32) ([]MACEntry, error) {
	reqCtx := g.ch.SendMultiRequest(&l2.L2FibTableDump{BdID: bdID})
	var out []MACEntry
	for {
		d := &l2.L2FibTableDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return nil, err
		}
		if stop {
			break
		}
		out = append(out, MACEntry{
			MAC:       net.HardwareAddr(d.Mac[:]).String(),
			SwIfIndex: uint32(d.SwIfIndex),
			Static:    d.StaticMac,
			BVI:       d.BviMac,
		})
	}
	return out, nil
}
