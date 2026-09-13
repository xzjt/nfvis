package network

// govpp 诊断客户端（M3-9）：clear stats 与接口地址反查。唯一使用 interface/ip binapi
// 诊断消息的地方，供 Diagnostics 调用；ping 经 vppctl（附录 A #36）。

import (
	"fmt"
	"sort"

	"go.fd.io/govpp/api"
	ifapi "go.fd.io/govpp/binapi/interface"
	"go.fd.io/govpp/binapi/interface_types"
	"go.fd.io/govpp/binapi/ip"
)

// DiagClientFunc 返回随当前连接获取诊断客户端的工厂。
func (m *Manager) DiagClientFunc() func() (DiagClient, error) {
	return func() (DiagClient, error) {
		ch, err := m.APIChannel()
		if err != nil {
			return nil, err
		}
		return &govppDiagClient{ch: ch}, nil
	}
}

type govppDiagClient struct{ ch api.Channel }

func (g *govppDiagClient) Close() { g.ch.Close() }

func (g *govppDiagClient) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	return (&govppL3Client{ch: g.ch}).SwInterfaceIndex(ifname)
}

func (g *govppDiagClient) SwInterfaceNames() (map[uint32]string, error) {
	reqCtx := g.ch.SendMultiRequest(&ifapi.SwInterfaceDump{})
	names := map[uint32]string{}
	for {
		d := &ifapi.SwInterfaceDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return nil, err
		}
		if stop {
			return names, nil
		}
		names[uint32(d.SwIfIndex)] = d.InterfaceName
	}
}

func (g *govppDiagClient) InterfaceAddresses(isIPv6 bool) ([]IfaceAddr, error) {
	// VPP 26.06 的 ip_address_dump 不支持 sw_if_index=~0 全量（传 ~0/0 均返回空），
	// 故先枚举接口索引再逐口 dump（与 bridge_domain_dump 行为不同，真机实测）。
	names, err := g.SwInterfaceNames()
	if err != nil {
		return nil, err
	}
	idxs := make([]int, 0, len(names))
	for idx := range names {
		idxs = append(idxs, int(idx))
	}
	sort.Ints(idxs)

	var out []IfaceAddr
	for _, idx := range idxs {
		reqCtx := g.ch.SendMultiRequest(&ip.IPAddressDump{
			SwIfIndex: interface_types.InterfaceIndex(idx), IsIPv6: isIPv6,
		})
		for {
			d := &ip.IPAddressDetails{}
			stop, err := reqCtx.ReceiveReply(d)
			if err != nil {
				return nil, err
			}
			if stop {
				break
			}
			out = append(out, IfaceAddr{SwIfIndex: uint32(d.SwIfIndex), Prefix: d.Prefix.String()})
		}
	}
	return out, nil
}

func (g *govppDiagClient) ClearInterfaceStats(swIfIndex uint32) error {
	reply := &ifapi.SwInterfaceClearStatsReply{}
	err := g.ch.SendRequest(&ifapi.SwInterfaceClearStats{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
	}).ReceiveReply(reply)
	if err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("sw_interface_clear_stats(if=%d) retval=%d", swIfIndex, reply.Retval)
	}
	return nil
}
