package network

// govpp vhost-user 客户端（M4-4）。薄 binary-API 适配层：文件名 _govpp.go →
// 覆盖率排除，由 nfvis-vm 集成测试覆盖。

import (
	"fmt"
	"net"

	"go.fd.io/govpp/api"
	"go.fd.io/govpp/binapi/ethernet_types"
	ifapi "go.fd.io/govpp/binapi/interface"
	"go.fd.io/govpp/binapi/interface_types"
	vhostapi "go.fd.io/govpp/binapi/vhost_user"
)

// VhostUserClientFunc 返回随当前连接获取 vhost-user 客户端的工厂。
func (m *Manager) VhostUserClientFunc() func() (VhostUserClient, error) {
	return func() (VhostUserClient, error) {
		ch, err := m.APIChannel()
		if err != nil {
			return nil, err
		}
		return &govppVhostUserClient{ch: ch}, nil
	}
}

type govppVhostUserClient struct{ ch api.Channel }

func (g *govppVhostUserClient) Close() { g.ch.Close() }

func (g *govppVhostUserClient) CreateVhostUser(sockFilename string, isServer bool, tag string) (uint32, error) {
	reply := &vhostapi.CreateVhostUserIfV2Reply{}
	if err := g.ch.SendRequest(&vhostapi.CreateVhostUserIfV2{
		IsServer:     isServer,
		SockFilename: sockFilename,
		Tag:          tag,
	}).ReceiveReply(reply); err != nil {
		return 0, err
	}
	if reply.Retval != 0 {
		return 0, fmt.Errorf("create_vhost_user_if_v2(sock=%s,server=%v) retval=%d", sockFilename, isServer, reply.Retval)
	}
	return uint32(reply.SwIfIndex), nil
}

func (g *govppVhostUserClient) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	return (&govppL3Client{ch: g.ch}).SwInterfaceIndex(ifname)
}

// VhostUserSocket 由 sw_interface_vhost_user_dump 取指定接口的 socket 路径
// （multi-request 流须读完，避免剩余 reply 落在已关闭 channel 上）。
func (g *govppVhostUserClient) VhostUserSocket(swIfIndex uint32) (string, bool, error) {
	reqCtx := g.ch.SendMultiRequest(&vhostapi.SwInterfaceVhostUserDump{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
	})
	found := ""
	exists := false
	for {
		details := &vhostapi.SwInterfaceVhostUserDetails{}
		stop, err := reqCtx.ReceiveReply(details)
		if err != nil {
			return "", false, err
		}
		if stop {
			break
		}
		if uint32(details.SwIfIndex) == swIfIndex {
			found, exists = details.SockFilename, true
		}
	}
	return found, exists, nil
}

func (g *govppVhostUserClient) SetInterfaceName(swIfIndex uint32, name string) error {
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

func (g *govppVhostUserClient) SetInterfaceMAC(swIfIndex uint32, mac string) error {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return fmt.Errorf("MAC %q 非法: %w", mac, err)
	}
	var addr ethernet_types.MacAddress
	copy(addr[:], hw)
	reply := &ifapi.SwInterfaceSetMacAddressReply{}
	if err := g.ch.SendRequest(&ifapi.SwInterfaceSetMacAddress{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex), MacAddress: addr,
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("sw_interface_set_mac_address(if=%d) retval=%d", swIfIndex, reply.Retval)
	}
	return nil
}

func (g *govppVhostUserClient) SetState(swIfIndex uint32, up bool) error {
	flags := interface_types.IF_STATUS_API_FLAG_ADMIN_UP
	return g.ch.SendRequest(&ifapi.SwInterfaceSetFlags{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex), Flags: flags,
	}).ReceiveReply(&ifapi.SwInterfaceSetFlagsReply{})
}

// InterfaceStatus 由 sw_interface_dump 取 flags（ADMIN_UP/LINK_UP 位）。
// 注意：multi-request 流必须读完（即使已找到目标），否则剩余 reply 落在已关闭的
// channel 上，govpp 报 "reply to an already closed binary API request"。
func (g *govppVhostUserClient) InterfaceStatus(swIfIndex uint32) (bool, bool, bool, error) {
	reqCtx := g.ch.SendMultiRequest(&ifapi.SwInterfaceDump{})
	var (
		adminUp, linkUp, exists bool
	)
	for {
		details := &ifapi.SwInterfaceDetails{}
		stop, err := reqCtx.ReceiveReply(details)
		if err != nil {
			return false, false, false, err
		}
		if stop {
			break
		}
		if uint32(details.SwIfIndex) == swIfIndex {
			adminUp = details.Flags&interface_types.IF_STATUS_API_FLAG_ADMIN_UP != 0
			linkUp = details.Flags&interface_types.IF_STATUS_API_FLAG_LINK_UP != 0
			exists = true
		}
	}
	return adminUp, linkUp, exists, nil
}

func (g *govppVhostUserClient) DeleteVhostUser(swIfIndex uint32) error {
	reply := &vhostapi.DeleteVhostUserIfReply{}
	if err := g.ch.SendRequest(&vhostapi.DeleteVhostUserIf{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("delete_vhost_user_if(if=%d) retval=%d", swIfIndex, reply.Retval)
	}
	return nil
}
