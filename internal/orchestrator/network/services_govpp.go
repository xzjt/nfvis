package network

// govpp SPAN/QoS/接口客户端（M3-5）。

import (
	"fmt"

	"go.fd.io/govpp/api"
	ifapi "go.fd.io/govpp/binapi/interface"
	"go.fd.io/govpp/binapi/interface_types"
	"go.fd.io/govpp/binapi/policer"
	"go.fd.io/govpp/binapi/policer_types"
	"go.fd.io/govpp/binapi/span"
)

// SvcClientFunc 返回随当前连接获取 SPAN/QoS 客户端的工厂。
func (m *Manager) SvcClientFunc() func() (SvcClient, error) {
	return func() (SvcClient, error) {
		ch, err := m.APIChannel()
		if err != nil {
			return nil, err
		}
		return &govppSvcClient{ch: ch}, nil
	}
}

type govppSvcClient struct{ ch api.Channel }

func (g *govppSvcClient) Close() { g.ch.Close() }

func (g *govppSvcClient) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	return (&govppL3Client{ch: g.ch}).SwInterfaceIndex(ifname)
}

func (g *govppSvcClient) SetMTU(swIfIndex, mtu uint32) error {
	reply := &ifapi.SwInterfaceSetMtuReply{}
	if err := g.ch.SendRequest(&ifapi.SwInterfaceSetMtu{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
		Mtu:       []uint32{mtu, 0, 0, 0},
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("sw_interface_set_mtu(if=%d,mtu=%d) retval=%d", swIfIndex, mtu, reply.Retval)
	}
	return nil
}

func (g *govppSvcClient) SpanSet(from, to uint32, state string, isL2 bool) error {
	st := span.SPAN_STATE_API_RX_TX
	switch state {
	case "rx":
		st = span.SPAN_STATE_API_RX
	case "tx":
		st = span.SPAN_STATE_API_TX
	case "disabled":
		st = span.SPAN_STATE_API_DISABLED
	}
	reply := &span.SwInterfaceSpanEnableDisableReply{}
	if err := g.ch.SendRequest(&span.SwInterfaceSpanEnableDisable{
		SwIfIndexFrom: interface_types.InterfaceIndex(from),
		SwIfIndexTo:   interface_types.InterfaceIndex(to),
		State:         st,
		IsL2:          isL2,
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("sw_interface_span_enable_disable(from=%d,to=%d) retval=%d", from, to, reply.Retval)
	}
	return nil
}

func (g *govppSvcClient) SpanDisable(from, to uint32) error {
	return g.SpanSet(from, to, "disabled", false)
}

func (g *govppSvcClient) PolicerAddDel(name string, cirKbps uint32, cb uint64, add bool) (uint32, error) {
	reply := &policer.PolicerAddDelReply{}
	if err := g.ch.SendRequest(&policer.PolicerAddDel{
		IsAdd:         add,
		Name:          name,
		Cir:           cirKbps,
		Cb:            cb,
		RateType:      policer_types.SSE2_QOS_RATE_API_KBPS,
		RoundType:     policer_types.SSE2_QOS_ROUND_API_TO_UP,
		Type:          policer_types.SSE2_QOS_POLICER_TYPE_API_1R2C,
		ConformAction: policer_types.Sse2QosAction{Type: policer_types.SSE2_QOS_ACTION_API_TRANSMIT},
		ExceedAction:  policer_types.Sse2QosAction{Type: policer_types.SSE2_QOS_ACTION_API_DROP},
	}).ReceiveReply(reply); err != nil {
		if vppErrIs(err, vppValueExist) { // 幂等：已存在（add）/已不存在（del）
			return 0, nil
		}
		return 0, err
	}
	if reply.Retval != 0 {
		if vppErrIs(api.RetvalToVPPApiError(reply.Retval), vppValueExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("policer_add_del(%s,add=%v) retval=%d", name, add, reply.Retval)
	}
	return reply.PolicerIndex, nil
}

func (g *govppSvcClient) PolicerInput(swIfIndex uint32, name string, apply bool) error {
	reply := &policer.PolicerInputReply{}
	if err := g.ch.SendRequest(&policer.PolicerInput{
		Name:      name,
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
		Apply:     apply,
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("policer_input(if=%d,name=%s,apply=%v) retval=%d", swIfIndex, name, apply, reply.Retval)
	}
	return nil
}
