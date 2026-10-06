package network

// 决策 #385：storm control 的 govpp 薄适配（policer + L2 classify + 挂接口）。
//
// 依赖集中在 *_govpp.go（由真机集成测试覆盖，不入本地覆盖率门槛），业务与向量构造在
// storm.go（纯函数，可跨平台单测）。

import (
	"encoding/hex"
	"fmt"

	"go.fd.io/govpp/api"
	"go.fd.io/govpp/binapi/classify"
	"go.fd.io/govpp/binapi/interface_types"
	"go.fd.io/govpp/binapi/policer"
	"go.fd.io/govpp/binapi/policer_types"
)

// StormClientFunc 返回随当前连接获取 storm control 客户端的工厂。
func (m *Manager) StormClientFunc() func() (StormClient, error) {
	return func() (StormClient, error) {
		ch, err := m.APIChannel()
		if err != nil {
			return nil, err
		}
		return &govppStormClient{ch: ch}, nil
	}
}

type govppStormClient struct{ ch api.Channel }

func (g *govppStormClient) Close() { g.ch.Close() }

func (g *govppStormClient) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	return (&govppL3Client{ch: g.ch}).SwInterfaceIndex(ifname)
}

// PolicerAddDel 1R2C policer（kbps），conform=transmit / exceed=drop。
// **增方向不吞 VALUE_EXIST**（与 QoS 的容忍不同）：风暴抑制把 policer 索引写进分类会话，
// 已存在时 VPP 不给索引——硬吞会写出错误的分类结果（指向索引 0/他对象）。清理路径（先按
// 数据面实况撤旧）已保证新增时不该存在同名 policer；真撞上就让调用方看到错误，不静默错配。
// 删方向仍容忍「本就不存在」（幂等）。Cb 必须非 0（真机实测 Cb=0 报 Invalid value (-7)）。
func (g *govppStormClient) PolicerAddDel(name string, cirKbps uint32, cb uint64, add bool) (uint32, error) {
	reply := &policer.PolicerAddDelReply{}
	err := g.ch.SendRequest(&policer.PolicerAddDel{
		IsAdd:         add,
		Name:          name,
		Cir:           cirKbps,
		Cb:            cb,
		RateType:      policer_types.SSE2_QOS_RATE_API_KBPS,
		RoundType:     policer_types.SSE2_QOS_ROUND_API_TO_UP,
		Type:          policer_types.SSE2_QOS_POLICER_TYPE_API_1R2C,
		ConformAction: policer_types.Sse2QosAction{Type: policer_types.SSE2_QOS_ACTION_API_TRANSMIT},
		ExceedAction:  policer_types.Sse2QosAction{Type: policer_types.SSE2_QOS_ACTION_API_DROP},
	}).ReceiveReply(reply)
	if err != nil {
		if !add && vppErrIs(err, vppNoSuchEntry, vppValueExist) { // 删方向：本就不在＝已达成
			return 0, nil
		}
		return 0, err
	}
	if reply.Retval != 0 {
		if !add && vppErrIs(api.RetvalToVPPApiError(reply.Retval), vppNoSuchEntry, vppValueExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("policer_add_del(%s,add=%v) retval=%d", name, add, reply.Retval)
	}
	return reply.PolicerIndex, nil
}

// ClassifyAddTable 建 L2 分类表：16 字节掩码、1 个匹配向量、不跳过向量。
// 表索引/其余字段由 VPP 分配（TableIndex=^0）；nextTableIndex=^0 表示无表链。
func (g *govppStormClient) ClassifyAddTable(mask []byte, nextTableIndex uint32) (uint32, error) {
	reply := &classify.ClassifyAddDelTableReply{}
	if err := g.ch.SendRequest(&classify.ClassifyAddDelTable{
		IsAdd:          true,
		TableIndex:     ^uint32(0),
		Nbuckets:       2,
		MemorySize:     2 << 20,
		SkipNVectors:   0,
		MatchNVectors:  1,
		NextTableIndex: nextTableIndex,
		MissNextIndex:  ^uint32(0),
		Mask:           mask,
	}).ReceiveReply(reply); err != nil {
		return 0, err
	}
	if reply.Retval != 0 {
		return 0, fmt.Errorf("classify_add_del_table(add) retval=%d", reply.Retval)
	}
	return reply.NewTableIndex, nil
}

// ClassifyDelTable 删表；delChain=true 连同表链上的后续表一并删（两类并存时广播表链着
// 组播表，按数据面实况清理只有链根可循）；表不存在按「已达成」处理（幂等）。
func (g *govppStormClient) ClassifyDelTable(tableIndex uint32, delChain bool) error {
	reply := &classify.ClassifyAddDelTableReply{}
	err := g.ch.SendRequest(&classify.ClassifyAddDelTable{
		IsAdd:      false,
		DelChain:   delChain,
		TableIndex: tableIndex,
	}).ReceiveReply(reply)
	if err != nil {
		if vppErrIs(err, vppNoSuchEntry, vppValueExist) {
			return nil
		}
		return err
	}
	if reply.Retval != 0 {
		if vppErrIs(api.RetvalToVPPApiError(reply.Retval), vppNoSuchEntry, vppValueExist) {
			return nil
		}
		return fmt.Errorf("classify_add_del_table(del,%d) retval=%d", tableIndex, reply.Retval)
	}
	return nil
}

// ClassifyAddSession 命中结果同时写入 hit_next_index 与 opaque_index（同一个 policer index）：
// classify 的结果字段是 hit_next_index，而 spike 的写法把 policer index 放在 opaque_index——
// 两处同写覆盖两种取法（policer 插件只用其一，冗余字段不影响语义）。advance=0。
func (g *govppStormClient) ClassifyAddSession(tableIndex uint32, match []byte, policerIndex uint32) error {
	reply := &classify.ClassifyAddDelSessionReply{}
	if err := g.ch.SendRequest(&classify.ClassifyAddDelSession{
		IsAdd:        true,
		TableIndex:   tableIndex,
		HitNextIndex: policerIndex,
		OpaqueIndex:  policerIndex,
		Match:        match,
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("classify_add_del_session(add,table=%d) retval=%d", tableIndex, reply.Retval)
	}
	return nil
}

func (g *govppStormClient) ClassifyDelSession(tableIndex uint32, match []byte) error {
	reply := &classify.ClassifyAddDelSessionReply{}
	err := g.ch.SendRequest(&classify.ClassifyAddDelSession{
		IsAdd:      false,
		TableIndex: tableIndex,
		Match:      match,
	}).ReceiveReply(reply)
	if err != nil {
		if vppErrIs(err, vppNoSuchEntry, vppValueExist) {
			return nil
		}
		return err
	}
	if reply.Retval != 0 {
		if vppErrIs(api.RetvalToVPPApiError(reply.Retval), vppNoSuchEntry, vppValueExist) {
			return nil
		}
		return fmt.Errorf("classify_add_del_session(del,table=%d) retval=%d", tableIndex, reply.Retval)
	}
	return nil
}

// PolicerClassifySetInterface 挂/摘该接口 L2 槽的分类表（入向）。
// 摘除时 l2_table_index 传登记里的表索引（VPP 按该值核对要摘的是哪张）。
func (g *govppStormClient) PolicerClassifySetInterface(swIfIndex, l2TableIndex uint32, add bool) error {
	reply := &classify.PolicerClassifySetInterfaceReply{}
	err := g.ch.SendRequest(&classify.PolicerClassifySetInterface{
		SwIfIndex:     interface_types.InterfaceIndex(swIfIndex),
		IP4TableIndex: ^uint32(0),
		IP6TableIndex: ^uint32(0),
		L2TableIndex:  l2TableIndex,
		IsAdd:         add,
	}).ReceiveReply(reply)
	if err != nil {
		if vppErrIs(err, vppNoSuchEntry, vppValueExist) {
			return nil
		}
		return err
	}
	if reply.Retval != 0 {
		if vppErrIs(api.RetvalToVPPApiError(reply.Retval), vppNoSuchEntry, vppValueExist) {
			return nil
		}
		return fmt.Errorf("policer_classify_set_interface(if=%d,table=%d,add=%v) retval=%d",
			swIfIndex, l2TableIndex, add, reply.Retval)
	}
	return nil
}

// PolicerDump 列出数据面 policer（读视图按名核对我们建的 nfvis-storm-<if>-<kind>；
// policer_dump 的条目不含索引，索引只在 add 的应答里给出）。
func (g *govppStormClient) PolicerDump() ([]StormPolicer, error) {
	req := g.ch.SendMultiRequest(&policer.PolicerDump{})
	var out []StormPolicer
	for {
		d := &policer.PolicerDetails{}
		stop, err := req.ReceiveReply(d)
		if err != nil {
			return nil, err
		}
		if stop {
			break
		}
		out = append(out, StormPolicer{Name: d.Name, CirKbps: d.Cir})
	}
	return out, nil
}

// AttachedL2Table 读接口当前挂的 L2 policer-classify 表（逐表 dump 后按 sw_if_index 过滤；
// 未挂返回 ok=false）。
func (g *govppStormClient) AttachedL2Table(swIfIndex uint32) (uint32, bool, error) {
	req := g.ch.SendMultiRequest(&classify.PolicerClassifyDump{
		Type:      classify.POLICER_CLASSIFY_API_TABLE_L2,
		SwIfIndex: ^interface_types.InterfaceIndex(0),
	})
	for {
		d := &classify.PolicerClassifyDetails{}
		stop, err := req.ReceiveReply(d)
		if err != nil {
			return 0, false, err
		}
		if stop {
			break
		}
		if uint32(d.SwIfIndex) == swIfIndex {
			return d.TableIndex, true, nil
		}
	}
	return 0, false, nil
}

// ClassifyTableInfo 读一张分类表的实测属性（掩码/会话数/表链；表不存在返回 ok=false）。
func (g *govppStormClient) ClassifyTableInfo(tableIndex uint32) (StormTableInfo, bool, error) {
	reply := &classify.ClassifyTableInfoReply{}
	if err := g.ch.SendRequest(&classify.ClassifyTableInfo{TableID: tableIndex}).ReceiveReply(reply); err != nil {
		if vppErrIs(err, vppNoSuchEntry) {
			return StormTableInfo{}, false, nil
		}
		return StormTableInfo{}, false, err
	}
	if reply.Retval != 0 {
		if vppErrIs(api.RetvalToVPPApiError(reply.Retval), vppNoSuchEntry) {
			return StormTableInfo{}, false, nil
		}
		return StormTableInfo{}, false, fmt.Errorf("classify_table_info(%d) retval=%d", tableIndex, reply.Retval)
	}
	return StormTableInfo{
		Index:          reply.TableID,
		Mask:           hex.EncodeToString(reply.Mask),
		Sessions:       reply.ActiveSessions,
		MatchNVectors:  reply.MatchNVectors,
		NextTableIndex: reply.NextTableIndex,
		MissNextIndex:  reply.MissNextIndex,
	}, true, nil
}
