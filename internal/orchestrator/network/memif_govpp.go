package network

// govpp memif 客户端（M4-7）。薄适配层：文件名 _govpp.go → 覆盖率排除。

import (
	"fmt"
	"net"

	"go.fd.io/govpp/api"
	"go.fd.io/govpp/binapi/ethernet_types"
	ifapi "go.fd.io/govpp/binapi/interface"
	"go.fd.io/govpp/binapi/interface_types"
	"go.fd.io/govpp/binapi/memif"
)

// MemifClientFunc 返回随当前连接获取 memif 客户端的工厂。
func (m *Manager) MemifClientFunc() func() (MemifClient, error) {
	return func() (MemifClient, error) {
		ch, err := m.APIChannel()
		if err != nil {
			return nil, err
		}
		return &govppMemifClient{ch: ch}, nil
	}
}

type govppMemifClient struct{ ch api.Channel }

func (g *govppMemifClient) Close() { g.ch.Close() }

func (g *govppMemifClient) AddSocketFilename(socketID uint32, path string, add bool) error {
	reply := &memif.MemifSocketFilenameAddDelReply{}
	if err := g.ch.SendRequest(&memif.MemifSocketFilenameAddDel{
		IsAdd: add, SocketID: socketID, SocketFilename: path,
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("memif_socket_filename_add_del(id=%d,path=%s,add=%v) retval=%d", socketID, path, add, reply.Retval)
	}
	return nil
}

// CreateMemif 以 MASTER/ETHERNET 建 memif 接口（VPP 创建 socket，容器侧为 slave）。
func (g *govppMemifClient) CreateMemif(id, socketID uint32, mac string) (uint32, error) {
	var hw ethernet_types.MacAddress
	if mac != "" {
		parsed, err := net.ParseMAC(mac)
		if err != nil {
			return 0, fmt.Errorf("MAC %q 非法: %w", mac, err)
		}
		copy(hw[:], parsed)
	}
	reply := &memif.MemifCreateReply{}
	if err := g.ch.SendRequest(&memif.MemifCreate{
		Role:     memif.MEMIF_ROLE_API_MASTER,
		Mode:     memif.MEMIF_MODE_API_ETHERNET,
		RxQueues: 1, TxQueues: 1,
		ID: id, SocketID: socketID,
		HwAddr: hw,
	}).ReceiveReply(reply); err != nil {
		return 0, err
	}
	if reply.Retval != 0 {
		return 0, fmt.Errorf("memif_create(id=%d,socket_id=%d) retval=%d", id, socketID, reply.Retval)
	}
	return uint32(reply.SwIfIndex), nil
}

func (g *govppMemifClient) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	return (&govppL3Client{ch: g.ch}).SwInterfaceIndex(ifname)
}

func (g *govppMemifClient) SetInterfaceName(swIfIndex uint32, name string) error {
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

func (g *govppMemifClient) SetInterfaceMAC(swIfIndex uint32, mac string) error {
	parsed, err := net.ParseMAC(mac)
	if err != nil {
		return fmt.Errorf("MAC %q 非法: %w", mac, err)
	}
	var addr ethernet_types.MacAddress
	copy(addr[:], parsed)
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

func (g *govppMemifClient) SetState(swIfIndex uint32, up bool) error {
	return g.ch.SendRequest(&ifapi.SwInterfaceSetFlags{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
		Flags:     interface_types.IF_STATUS_API_FLAG_ADMIN_UP,
	}).ReceiveReply(&ifapi.SwInterfaceSetFlagsReply{})
}

func (g *govppMemifClient) DeleteMemif(swIfIndex uint32) error {
	reply := &memif.MemifDeleteReply{}
	if err := g.ch.SendRequest(&memif.MemifDelete{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("memif_delete(if=%d) retval=%d", swIfIndex, reply.Retval)
	}
	return nil
}
