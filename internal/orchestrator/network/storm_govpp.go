package network

// 决策 #385：storm control 的 govpp 薄适配（policer + L2 classify + 挂接口）。
//
// 依赖集中在 *_govpp.go（由真机集成测试覆盖，不入本地覆盖率门槛），业务与向量构造在
// storm.go（纯函数，可跨平台单测）。
//
// ⚠️ 底座能力前提（真机探针实测）：本底座的 `classify_table_by_interface` 与
// `policer_classify_dump` **都读不到 policer-classify 绑定**（阴性不可判，详见
// `AttachedL2Table` 的注释）——绑定事实的四态判定（含按形状/表链自认领）与删除安全化在
// storm.go（决策 #421）。
//
// 读数（dump / 单请求查询）一律经 recvMultiBound / recvReplyBound 套应答时限（决策 #422，
// 说明与红-绿用例见 govpp_read_bound.go）；**写路径有意不套时限**，语义保持原样。

import (
	"encoding/hex"
	"fmt"

	"go.fd.io/govpp/api"
	"go.fd.io/govpp/binapi/classify"
	ifapi "go.fd.io/govpp/binapi/interface"
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
		if !add && stormAbsentCode(err) { // 删方向：本就不在 ⇒ ErrStormAbsent（Provider 按已达成）
			return 0, ErrStormAbsent
		}
		return 0, err
	}
	if reply.Retval != 0 {
		if !add && stormAbsentCode(api.RetvalToVPPApiError(reply.Retval)) {
			return 0, ErrStormAbsent
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

// stormAbsentCode 删除方向的 VPP 错误码 → 是否表示「对象本就不在」（由 Provider 决定是否按
// 「已达成」继续）：-6 No such entry / -65 No such table（真机实测：解绑未挂在接口上的表）/
// -91 Classify table not found（**分类插件自己的码**；真机实测：带链删顺带删掉链上那张后，
// 再对那张做属性读取/删前形状复核就收到 -91）/ -81 VALUE_EXIST（历史实现即以此判 del 幂等）。
func stormAbsentCode(err error) bool {
	return vppErrIs(err, vppNoSuchEntry, vppNoSuchTable, vppClassifyTableNotFound, vppValueExist)
}

// ClassifyDelTable 删表；delChain=true 连同表链上的后续表一并删（两类并存时广播表链着
// 组播表，按数据面实况清理只有链根可循）；表不存在返回 ErrStormAbsent。
func (g *govppStormClient) ClassifyDelTable(tableIndex uint32, delChain bool) error {
	reply := &classify.ClassifyAddDelTableReply{}
	err := g.ch.SendRequest(&classify.ClassifyAddDelTable{
		IsAdd:      false,
		DelChain:   delChain,
		TableIndex: tableIndex,
	}).ReceiveReply(reply)
	if err != nil {
		if stormAbsentCode(err) {
			return ErrStormAbsent
		}
		return err
	}
	if reply.Retval != 0 {
		if stormAbsentCode(api.RetvalToVPPApiError(reply.Retval)) {
			return ErrStormAbsent
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
		if stormAbsentCode(err) {
			return ErrStormAbsent
		}
		return err
	}
	if reply.Retval != 0 {
		if stormAbsentCode(api.RetvalToVPPApiError(reply.Retval)) {
			return ErrStormAbsent
		}
		return fmt.Errorf("classify_add_del_session(del,table=%d) retval=%d", tableIndex, reply.Retval)
	}
	return nil
}

// PolicerClassifySetInterface 挂上/摘掉该接口 L2 槽的分类表（入向）。
// 摘除时 l2_table_index 传槽上那张表的索引：实况阳性用实况索引，实况不可回读时用**登记索引**
// （见 `AttachedL2Table` 的能力说明）——真机实测按陈旧登记解绑会被 VPP 以 `No such table (-65)`
// 拒（归一到 ErrStormAbsent，按本仓库口径＝「已是目标状态」）。摘除的返回是「解绑已达成」的
// 确认依据：删表前必须拿到确认（无错误或 ErrStormAbsent），否则 Provider 一律不删表。
// **挂上方向不容错**：槽已被别的表占用时静默吞错会留下「登记说新、实况是旧」的错位。
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
		if !add && stormAbsentCode(err) {
			return ErrStormAbsent
		}
		return err
	}
	if reply.Retval != 0 {
		if !add && stormAbsentCode(api.RetvalToVPPApiError(reply.Retval)) {
			return ErrStormAbsent
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
		stop, err := recvMultiBound(g.ch, req, d) // 有界读数（决策 #422）
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

// AttachedL2Table 读接口 L2 槽**当前实际挂着**的表：用 `classify_table_by_interface`
// （请求/应答）。
//
// ⚠️ **能力前提（真机探针实测，2026-10-08，勿再把它当权威事实源）**：本底座（VPP 26.06）上
// policer-classify 绑定**两个 API 都读不到**——
//   - 本方法（`classify_table_by_interface`）对 policer-classify 绑定**恒回
//     l2_table_id=0xFFFFFFFF（NONE）**，对所有接口都如此（含 vppctl 里确认有绑定的 ens224）；
//   - `policer_classify_dump`（L2/IP4/IP6 三档）**恒返回 0 条目**；
//     而 macip（端口安全）的绑定读得到——即绑定本身在数据面是生效的，只是回读通道缺失。
//     探针原始输出与崩溃现场见 docs/evidence/v3-round3-release-clean-install-walkthrough.txt §2。
//
// 因此：**ok=false 只表示「没有阳性结果」，不等于「未挂」**（阴性 = 不可判）。绑定事实由
// Provider 按四态判定（storm.go 的 bindFact：阳性权威 + 登记/自认领兜底），**绝不以本方法的阴性
// 单方面作删除依据**——历史缺陷正是把阴性当未挂，孤儿清扫删掉槽上正在生效的表，接口槽悬空
// 指向已释放的表，首包在 policer 插件里空指针（VPP 崩溃循环）。若未来底座恢复可回读，
// 只需采信阳性（阴性仍不可判）。
//
// 无绑定（retval 非 0，或 l2_table_id = ~0 的「空槽」约定）返回 ok=false；查询被拒与「本就没挂」
// 在返回值上不可区分——判断一律走 Provider 的事实判定（bindFact/adoptDeclared），本方法只提供原始读数。
func (g *govppStormClient) AttachedL2Table(swIfIndex uint32) (uint32, bool, error) {
	req := g.ch.SendRequest(&classify.ClassifyTableByInterface{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
	})
	reply := &classify.ClassifyTableByInterfaceReply{}
	// 单请求读数同样有界（决策 #422）。
	if err := recvReplyBound(g.ch, req, reply); err != nil {
		if stormAbsentCode(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if reply.Retval != 0 || reply.L2TableID == ^uint32(0) {
		return 0, false, nil
	}
	return reply.L2TableID, true, nil
}

// ClassifyTableIDs 列出全部 classify 表索引（孤儿清扫用）。
func (g *govppStormClient) ClassifyTableIDs() ([]uint32, error) {
	req := g.ch.SendRequest(&classify.ClassifyTableIds{})
	reply := &classify.ClassifyTableIdsReply{}
	// 单请求读数同样有界（决策 #422）。
	if err := recvReplyBound(g.ch, req, reply); err != nil {
		return nil, err
	}
	if reply.Retval != 0 {
		return nil, fmt.Errorf("classify_table_ids retval=%d", reply.Retval)
	}
	return reply.Ids, nil
}

// AllInterfaceIndexes 列出全部接口索引（孤儿清扫的保护集计算用）。
func (g *govppStormClient) AllInterfaceIndexes() ([]uint32, error) {
	req := g.ch.SendMultiRequest(&ifapi.SwInterfaceDump{})
	var out []uint32
	for {
		d := &ifapi.SwInterfaceDetails{}
		stop, err := recvMultiBound(g.ch, req, d) // 有界读数（决策 #422）
		if err != nil {
			return nil, err
		}
		if stop {
			break
		}
		out = append(out, uint32(d.SwIfIndex))
	}
	return out, nil
}

// ClassifyTableInfo 读一张分类表的实测属性（掩码/会话数/表链；表不存在返回 ok=false）。
func (g *govppStormClient) ClassifyTableInfo(tableIndex uint32) (StormTableInfo, bool, error) {
	req := g.ch.SendRequest(&classify.ClassifyTableInfo{TableID: tableIndex})
	reply := &classify.ClassifyTableInfoReply{}
	// 单请求读数同样有界（决策 #422）。
	if err := recvReplyBound(g.ch, req, reply); err != nil {
		if stormAbsentCode(err) {
			return StormTableInfo{}, false, nil
		}
		return StormTableInfo{}, false, err
	}
	if reply.Retval != 0 {
		if stormAbsentCode(api.RetvalToVPPApiError(reply.Retval)) {
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
