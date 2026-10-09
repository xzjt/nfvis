package network

// govpp DHCP 服务器客户端（决策 #359）：唯一使用 tapv2 / l2 / interface binapi 的地方。
//
// 能力集（round140 spike 实测）：
//   - tapv2.TapCreateV3 / TapDeleteV2 / SwInterfaceTapV2Dump（按 HostIfName 找存量；
//     **dump 不回 tag**，tag 只写不读，不作查找键）；
//   - l2.SwInterfaceSetL2Bridge（把 tap 加入交换机 bridge-domain）；
//   - interface.SwInterfaceSetFlags（显式置 up——VPP 侧 tap 默认 down）。
//
// 注意：binapi 的 `default=` 不会在发送时自动填充（l2_govpp.go 的同款教训），
// 显式给全部默认值（ID=^uint32(0) 让 VPP 分配、随机 MAC、1 队列、256 环）。

import (
	"fmt"

	"github.com/xzjt/nfvis/internal/model"
	"go.fd.io/govpp/api"
	ifapi "go.fd.io/govpp/binapi/interface"
	"go.fd.io/govpp/binapi/interface_types"
	"go.fd.io/govpp/binapi/l2"
	"go.fd.io/govpp/binapi/tapv2"
)

// TapInfo 一个 VPP tap 的存量信息（恢复查找用 HostIfName）。
type TapInfo struct {
	SwIfIndex  uint32
	HostIfName string
	HostMAC    string
}

// DHCPServerClient DHCP 服务器传输面所需的最小能力集（govpp 适配 / 内核侧实现 / 单测假实现）。
//
// 接口面向**每交换机一次收敛**的调用形态：实现由 `client(sw)` 工厂按该交换机的声明构造
// （内核侧实现用它派生 bridge 名/服务 VLAN 等事实，见 network/dhcpserver.go 的字段说明），
// 方法本身仍只收发 sw_if_index/bdID。
type DHCPServerClient interface {
	// TapCreate 创建一个 tap，返回 VPP 侧 sw_if_index（内核侧名 hostIfName、tag 仅写不改）。
	TapCreate(hostIfName, tag string) (uint32, error)
	// TapDelete 删除 tap（已不存在由调用方按 isMissingIfaceErr 处理）。
	TapDelete(swIfIndex uint32) error
	// TapDump 全部 tap 存量（按 HostIfName 找产品自持的内置 tap）。
	TapDump() ([]TapInfo, error)
	// SetL2Bridge 把接口加入/移出 bridge-domain（幂等）。
	SetL2Bridge(swIfIndex, bdID uint32, enable bool) error
	// SetState 置接口管理员 up/down（幂等）。
	SetState(swIfIndex uint32, up bool) error
	Close()
}

// DHCPServerClientFunc 返回随当前连接获取 DHCP 服务器客户端的工厂（断线重连自动换 channel）。
// 入参 sw 是本次收敛的交换机声明：VPP 侧只需要 bdID/sw_if_index（BD 天然与所有端口同域，
// 没有内核 bridge 那类「服务 VLAN」问题），故收下但**不使用**——行为与改造前逐字一致。
func (m *Manager) DHCPServerClientFunc() func(sw model.VirtualSwitch) (DHCPServerClient, error) {
	return func(_ model.VirtualSwitch) (DHCPServerClient, error) {
		ch, err := m.APIChannel()
		if err != nil {
			return nil, err
		}
		return &govppDHCPServerClient{ch: ch}, nil
	}
}

type govppDHCPServerClient struct{ ch api.Channel }

func (g *govppDHCPServerClient) Close() { g.ch.Close() }

func (g *govppDHCPServerClient) TapCreate(hostIfName, tag string) (uint32, error) {
	reply := &tapv2.TapCreateV3Reply{}
	// 默认值显式给全（default= 不自动填充）：随机 MAC（内核侧由 VPP 生成）、1 收 1 发、
	// 256 环、ID=^uint32(0) 由 VPP 分配（与 `vppctl create tap host-if-name X` 等价）。
	if err := g.ch.SendRequest(&tapv2.TapCreateV3{
		ID:            ^uint32(0),
		UseRandomMac:  true,
		NumRxQueues:   1,
		NumTxQueues:   1,
		TxRingSz:      256,
		RxRingSz:      256,
		HostIfNameSet: true,
		HostIfName:    hostIfName,
		Tag:           tag,
	}).ReceiveReply(reply); err != nil {
		return 0, err
	}
	if reply.Retval != 0 {
		return 0, fmt.Errorf("tap_create_v3(host-if-name=%s) retval=%d", hostIfName, reply.Retval)
	}
	return uint32(reply.SwIfIndex), nil
}

func (g *govppDHCPServerClient) TapDelete(swIfIndex uint32) error {
	reply := &tapv2.TapDeleteV2Reply{}
	if err := g.ch.SendRequest(&tapv2.TapDeleteV2{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("tap_delete_v2(sw_if_index=%d) retval=%d", swIfIndex, reply.Retval)
	}
	return nil
}

func (g *govppDHCPServerClient) TapDump() ([]TapInfo, error) {
	// 无过滤：default 不自动填充，须显式给 0xFFFFFFFF（否则按 sw_if_index 0 过滤而返回空）。
	reqCtx := g.ch.SendMultiRequest(&tapv2.SwInterfaceTapV2Dump{
		SwIfIndex: interface_types.InterfaceIndex(0xFFFFFFFF),
	})
	out := make([]TapInfo, 0, 4)
	for {
		d := &tapv2.SwInterfaceTapV2Details{}
		stop, err := recvMultiBound(g.ch, reqCtx, d) // 有界读数（决策 #422）
		if err != nil {
			return nil, err
		}
		if stop {
			return out, nil
		}
		out = append(out, TapInfo{SwIfIndex: d.SwIfIndex, HostIfName: d.HostIfName, HostMAC: d.HostMacAddr.String()})
	}
}

func (g *govppDHCPServerClient) SetL2Bridge(swIfIndex, bdID uint32, enable bool) error {
	reply := &l2.SwInterfaceSetL2BridgeReply{}
	if err := g.ch.SendRequest(&l2.SwInterfaceSetL2Bridge{
		RxSwIfIndex: interface_types.InterfaceIndex(swIfIndex),
		BdID:        bdID,
		PortType:    l2.L2PortType(L2PortNormal), // 与 l2_govpp.go 同一端口类型常量（普通成员口）
		Enable:      enable,
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("sw_interface_set_l2_bridge(if=%d,bd=%d,enable=%v) retval=%d", swIfIndex, bdID, enable, reply.Retval)
	}
	return nil
}

func (g *govppDHCPServerClient) SetState(swIfIndex uint32, up bool) error {
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
