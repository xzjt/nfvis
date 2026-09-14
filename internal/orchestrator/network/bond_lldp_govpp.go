package network

// govpp bond 与 LLDP 客户端（M3-6）。

import (
	"fmt"
	"strings"

	"go.fd.io/govpp/api"
	"go.fd.io/govpp/binapi/bond"
	ifapi "go.fd.io/govpp/binapi/interface"
	"go.fd.io/govpp/binapi/interface_types"
	"go.fd.io/govpp/binapi/lldp"
)

// BondClientFunc 返回随当前连接获取 bond 客户端的工厂。
func (m *Manager) BondClientFunc() func() (BondClient, error) {
	return func() (BondClient, error) {
		ch, err := m.APIChannel()
		if err != nil {
			return nil, err
		}
		return &govppBondClient{ch: ch}, nil
	}
}

type govppBondClient struct{ ch api.Channel }

func (g *govppBondClient) Close() { g.ch.Close() }

func (g *govppBondClient) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	return (&govppL3Client{ch: g.ch}).SwInterfaceIndex(ifname)
}

func (g *govppBondClient) BondCreate(lacp bool) (uint32, error) {
	mode := bond.BOND_API_MODE_XOR // 静态聚合：哈希分发（附录 A #33）
	if lacp {
		mode = bond.BOND_API_MODE_LACP
	}
	reply := &bond.BondCreateReply{}
	if err := g.ch.SendRequest(&bond.BondCreate{ID: ^uint32(0), Mode: mode}).ReceiveReply(reply); err != nil {
		return 0, err
	}
	if reply.Retval != 0 {
		return 0, fmt.Errorf("bond_create retval=%d", reply.Retval)
	}
	return uint32(reply.SwIfIndex), nil
}

func (g *govppBondClient) SetInterfaceName(swIfIndex uint32, name string) error {
	reply := &ifapi.SwInterfaceSetInterfaceNameReply{}
	if err := g.ch.SendRequest(&ifapi.SwInterfaceSetInterfaceName{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex), Name: name,
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("sw_interface_set_interface_name(if=%d,name=%s) retval=%d", swIfIndex, name, reply.Retval)
	}
	return nil
}

func (g *govppBondClient) BondAddMember(bondSwIfIndex, memberSwIfIndex uint32, passive bool) error {
	reply := &bond.BondAddMemberReply{}
	if err := g.ch.SendRequest(&bond.BondAddMember{
		SwIfIndex:     interface_types.InterfaceIndex(memberSwIfIndex),
		BondSwIfIndex: interface_types.InterfaceIndex(bondSwIfIndex),
		IsPassive:     passive,
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("bond_add_member(bond=%d,member=%d) retval=%d", bondSwIfIndex, memberSwIfIndex, reply.Retval)
	}
	return nil
}

// BondDetachMember 按成员 sw_if_index 摘除（VPP 由成员反查所属 bond）。
func (g *govppBondClient) BondDetachMember(memberSwIfIndex uint32) error {
	reply := &bond.BondDetachMemberReply{}
	if err := g.ch.SendRequest(&bond.BondDetachMember{
		SwIfIndex: interface_types.InterfaceIndex(memberSwIfIndex),
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("bond_detach_member(member=%d) retval=%d", memberSwIfIndex, reply.Retval)
	}
	return nil
}

func (g *govppBondClient) BondDelete(bondSwIfIndex uint32) error {
	reply := &bond.BondDeleteReply{}
	if err := g.ch.SendRequest(&bond.BondDelete{
		SwIfIndex: interface_types.InterfaceIndex(bondSwIfIndex),
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("bond_delete(if=%d) retval=%d", bondSwIfIndex, reply.Retval)
	}
	return nil
}

func (g *govppBondClient) SetState(swIfIndex uint32, up bool) error {
	return setIfaceState(g.ch, swIfIndex, up)
}

func (g *govppBondClient) SetMTU(swIfIndex, mtu uint32) error {
	return (&govppSvcClient{ch: g.ch}).SetMTU(swIfIndex, mtu)
}

// LldpClientFunc 返回随当前连接获取 LLDP 客户端的工厂。
func (m *Manager) LldpClientFunc() func() (LldpClient, error) {
	return func() (LldpClient, error) {
		ch, err := m.APIChannel()
		if err != nil {
			return nil, err
		}
		return &govppLldpClient{ch: ch}, nil
	}
}

type govppLldpClient struct{ ch api.Channel }

func (g *govppLldpClient) Close() { g.ch.Close() }

func (g *govppLldpClient) LldpConfig(txInterval, txHold uint32, systemName string) error {
	if txHold == 0 {
		txHold = 4
	}
	reply := &lldp.LldpConfigReply{}
	if err := g.ch.SendRequest(&lldp.LldpConfig{
		TxInterval: txInterval, TxHold: txHold, SystemName: systemName,
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("lldp_config retval=%d", reply.Retval)
	}
	return nil
}

func (g *govppLldpClient) LldpSetInterface(ifname string, enable bool) error {
	idx, ok, err := (&govppL3Client{ch: g.ch}).SwInterfaceIndex(ifname)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("LLDP 接口 %s 不存在于 VPP（是否未由 DPDK 接管？）", ifname)
	}
	reply := &lldp.SwInterfaceSetLldpReply{}
	if err := g.ch.SendRequest(&lldp.SwInterfaceSetLldp{
		SwIfIndex: interface_types.InterfaceIndex(idx),
		MgmtOid:   make([]byte, 128),
		PortDesc:  ifname,
		Enable:    enable,
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("sw_interface_set_lldp(if=%s,enable=%v) retval=%d", ifname, enable, reply.Retval)
	}
	return nil
}

func (g *govppLldpClient) LldpNeighbors() ([]LldpNeighbor, error) {
	names, err := (&govppL2Client{ch: g.ch}).SwInterfaceNames()
	if err != nil {
		return nil, err
	}
	// lldp_dump 是 cursor 型 RequestMessage（非 multipart）；govpp 的 MultiRequest
	// 在「无邻居」时会把终止回复当作意外消息报错，此时按空表处理。
	reqCtx := g.ch.SendMultiRequest(&lldp.LldpDump{})
	var out []LldpNeighbor
	for {
		d := &lldp.LldpDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			if strings.Contains(err.Error(), "lldp_dump_reply") {
				return out, nil
			}
			return nil, err
		}
		if stop {
			break
		}
		info := names[uint32(d.SwIfIndex)]
		out = append(out, LldpNeighbor{
			Interface: info.Name,
			// 按 subtype 解码：MAC 型标识是二进制，直接转字符串会输出乱码（决策 #70）
			ChassisID: lldpIDBySubtype(uint32(d.ChassisIDSubtype), uint32(lldp.CHASSIS_ID_SUBTYPE_MAC_ADDR), d.ChassisID[:d.ChassisIDLen]),
			PortID:    lldpIDBySubtype(uint32(d.PortIDSubtype), uint32(lldp.PORT_ID_SUBTYPE_MAC_ADDR), d.PortID[:d.PortIDLen]),
			TTL:       int(d.TTL),
			LastHeard: d.LastHeard,
		})
	}
	return out, nil
}
