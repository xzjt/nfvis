package network

// 决策 #385：接口入向风暴抑制（storm control v1，FR-NET-019）。
//
// VPP 26.06 没有 storm 命名的 API，用既有等价原语组合（能力前提的真机 spike 见
// docs/evidence/v2-round166-storm-control-spike.txt）：
//   - 每类一个 1R2C policer（kbps；conform=transmit / exceed=drop），名 nfvis-storm-<if>-<kind>；
//   - 每类一张 L2 classify 表（skip_n_vectors=0 / match_n_vectors=1 / 16 字节掩码）+ 一条
//     session（match = 目的 MAC 的掩码写法，命中结果即该类的 policer index）；
//   - 表的挂载走 `policer_classify_set_interface` 的 **L2 槽**（入向、按目的 MAC 分类）。
//
// 两处结构上的如实说明（实现期按可用 API 定形；真机核对点见交付说明）：
//
//  1. **接口的 L2 policer-classify 槽只有一张表**：`policer_classify_set_interface` 的
//     l2_table_index 是单值，`classify_table_by_interface` 也只回一个 l2_table_id——
//     两类各一张表不可能都直接挂上去。两类并存时挂**广播表**，并用它的 `NextTableIndex`
//     指向组播表（VPP classify 的**表链**：本表未命中时在下一张表里继续查）：广播帧在广播
//     表精确命中；其余组播帧未命中广播表、经链进组播表（I/G 位）命中；单播两表都不命中、
//     放行。若该表链语义在本底座不成立，最坏结果是**组播一类不生效**（广播不受影响、单播
//     不会被误限）——不会出现错误方向的限速。
//
//  2. **两类的掩码不同，必须两张表**：一张 classify 表只有一个掩码，而广播是目的 MAC
//     精确匹配（ff:ff:ff:ff:ff:ff）、组播是目的 MAC 首字节的 I/G 位（0x01 掩码「匹配位」
//     写法）；同一张表无法同时表达两类。另：I/G 位口径下**广播帧也算组播**，故只配
//     multicast 时广播也受该速率限制（手册写明；两类同配时广播走自己的精确表）。
//
// 判据/登记口径沿用本仓库既定纪律：
//   - 变更先撤旧后建新（旧 policer/表/session 全删，不留残渣，同 #380/#383 的教训）；
//   - 登记（进程内）语义 = **最后一个成功下发的状态**（决策 #363）：任何一步失败即返回，
//     登记停在最后成功态，重试不会因「登记说已下发」而静默跳过；
//   - 失效（VPP 重启/重连）走 reset：登记清空后由恢复收敛按声明全量重放（幂等）。

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/xzjt/nfvis/internal/model"
)

// 风暴抑制类别（与模型/语句树的关键字同名）。
const (
	StormKindBroadcast = "broadcast"
	StormKindMulticast = "multicast"
)

// stormKinds 固定处理顺序（输出与调用序确定：广播在前、组播在后）。
var stormKinds = []string{StormKindBroadcast, StormKindMulticast}

// stormVectorLen 分类向量长度：16 字节（1 个向量），目的 MAC 在前 6 字节。
const stormVectorLen = 16

// stormCbBytes policer 的令牌桶容量（字节）：按 8 秒 CIR 估算（CIR kbps → 字节/秒 = kbps*125）。
//
// 为什么必须有确定值：真机实测 `Cb: 0` 被 VPP 拒绝（`Invalid value (-7)`，round166 踩坑
// 入册），故取一个非 0 的包级常量；8 秒的突发窗口与常见硬件风暴抑制的量级相称。**不作为
// 用户配置项暴露**（多一个没人会调的旋钮，且它不改变「超速丢弃」的语义）。
func stormCbBytes(kbps uint32) uint64 { return uint64(kbps) * 125 * 8 }

// stormMask 该类的 classify 掩码（16 字节；「匹配位」写法）。
//
//   - broadcast：前 6 字节全 0xff = 目的 MAC 精确匹配；
//   - multicast：仅首字节 0x01 = 目的 MAC 的 I/G 位（最低位；组播/Broadcast 帧为 1）。
//
// 其余字节为 0（不参与匹配）。由 stormVectorRoundTrip 单测钉住形状。
func stormMask(kind string) []byte {
	m := make([]byte, stormVectorLen)
	if kind == StormKindMulticast {
		m[0] = 0x01
		return m
	}
	for i := 0; i < 6; i++ {
		m[i] = 0xff
	}
	return m
}

// stormMatch 该类的 session 匹配向量（16 字节）。
//
//   - broadcast：前 6 字节全 0xff（ff:ff:ff:ff:ff:ff）；
//   - multicast：首字节 0x01（与掩码配合 = 「I/G 位为 1」）。
func stormMatch(kind string) []byte {
	return stormMask(kind) // 两类「掩码位 = 匹配值」同形；分两个函数是为了让语义各自成文
}

// stormPolicerName 该类在该接口上的 policer 名（命名空间独立于 QoS 策略：前缀不同）。
func stormPolicerName(ifname, kind string) string {
	return "nfvis-storm-" + ifname + "-" + kind
}

// StormPolicer 数据面 policer 的**实测**视图（读视图用；取自 policer_dump——该 dump 的
// 条目不含索引，索引只在创建应答里给出，故这里只有名字与 CIR）。
type StormPolicer struct {
	Name    string
	CirKbps uint32
}

// StormTableInfo 分类表的**实测**视图（读视图用；取自 classify_table_info）。
type StormTableInfo struct {
	Index          uint32 `json:"index"`
	Mask           string `json:"mask"` // 十六进制
	Sessions       uint32 `json:"sessions"`
	MatchNVectors  uint32 `json:"match_n_vectors,omitempty"`
	NextTableIndex uint32 `json:"next_table_index,omitempty"`
	MissNextIndex  uint32 `json:"miss_next_index,omitempty"`
}

// StormCounters 单类 policer 的实测计数（stats segment；读不到由调用方如实说明，不猜）。
type StormCounters struct {
	ConformPackets uint64 `json:"conform_packets"`
	ConformBytes   uint64 `json:"conform_bytes,omitempty"`
	ExceedPackets  uint64 `json:"exceed_packets"`
	ViolatePackets uint64 `json:"violate_packets"`
	ViolateBytes   uint64 `json:"violate_bytes,omitempty"`
}

// StormKindDataplane 单类的数据面实况（全部为实测值；取不到就留空/给原因）。
type StormKindDataplane struct {
	// PolicerPresent 数据面是否存在该类的 policer（按名在 policer_dump 里找）。
	PolicerPresent bool
	PolicerIndex   uint32
	CirKbps        uint32 // 实测 CIR（kbps；与配置值不一致即数据面未收敛）
	Table          *StormTableInfo
	Counters       *StormCounters
	// CountersReason 计数不可读时的原因（如实说明；可读时为空）。
	CountersReason string
}

// StormDataplane 接口 storm control 的数据面实况（读视图）。
type StormDataplane struct {
	// Available 能否核对数据面（接口可解析 + 底座可查）；false 时 Reason 说明原因。
	Available bool
	Reason    string
	// AttachedL2Table 接口当前挂着的 L2 policer-classify 表索引（实测；Attached=false 未挂）。
	AttachedL2Table uint32
	Attached        bool
	Kinds           map[string]StormKindDataplane
}

// StormClient storm control 的 VPP 能力集（薄适配；真机实现见 storm_govpp.go）。
type StormClient interface {
	SwInterfaceIndex(ifname string) (uint32, bool, error)
	PolicerAddDel(name string, cirKbps uint32, cb uint64, add bool) (uint32, error)
	// ClassifyAddTable 建 L2 分类表（16 字节掩码），返回表索引。
	// nextTableIndex = ^uint32(0) 表示无表链；否则本表未命中时继续在该表里查。
	ClassifyAddTable(mask []byte, nextTableIndex uint32) (uint32, error)
	// ClassifyDelTable 删表（delChain=true 连同表链上的后续表一并删——两类并存时广播表链着
	// 组播表，按实况清理只有链根可循）；表不存在按「已达成」处理（幂等）。
	ClassifyDelTable(tableIndex uint32, delChain bool) error
	// ClassifyAddSession 加一条 session：match 为 16 字节匹配向量，命中结果 = policerIndex。
	ClassifyAddSession(tableIndex uint32, match []byte, policerIndex uint32) error
	// ClassifyDelSession 删 session；不存在按「已达成」处理（幂等）。
	ClassifyDelSession(tableIndex uint32, match []byte) error
	// PolicerClassifySetInterface 把（或摘掉）该接口 L2 槽的分类表；不存在按「已达成」。
	PolicerClassifySetInterface(swIfIndex, l2TableIndex uint32, add bool) error
	// PolicerDump 列出数据面 policer（读视图核对用）。
	PolicerDump() ([]StormPolicer, error)
	// AttachedL2Table 读该接口当前挂的 L2 分类表（实测；ok=false = 未挂）。
	AttachedL2Table(swIfIndex uint32) (uint32, bool, error)
	// ClassifyTableInfo 读一张分类表的实测属性（掩码/会话数/表链）。
	ClassifyTableInfo(tableIndex uint32) (StormTableInfo, bool, error)
	Close()
}

// StormCountersReader policer 计数读物（stats segment）。真机实现见 stats_linux.go；
// 非 Linux / 工具不可用时按「读不到」如实说明（不猜）。
type StormCountersReader interface {
	StormCounters(ctx context.Context, policerIndex uint32, policerName string) (StormCounters, bool, string)
}

// stormKindRT 单类在该接口上的登记 = **最后一个成功下发**的状态（决策 #363 口径）。
type stormKindRT struct {
	policerName string
	policerIdx  uint32
	tableIdx    uint32
	kbps        uint32
}

// stormIfaceRT 接口级登记。
type stormIfaceRT struct {
	kinds map[string]*stormKindRT
	// attachTable/attached：当前挂在该接口 L2 槽的表（attached=true 才有意义——
	// 表索引 0 是合法值，不能用 0 当「未挂」）。
	attachTable uint32
	attached    bool
}

// StormProvider 接口入向风暴抑制编排（决策 #385）。
type StormProvider struct {
	client func() (StormClient, error)
	// counters policer 计数读数（可空：空则读视图如实报「未接入」）。
	counters StormCountersReader

	mu sync.Mutex
	rt map[string]*stormIfaceRT
}

// NewStormProvider 以固定客户端构造（测试）。
func NewStormProvider(c StormClient) *StormProvider {
	return &StormProvider{client: func() (StormClient, error) { return c, nil }, rt: map[string]*stormIfaceRT{}}
}

// NewStormProviderFunc 以客户端工厂构造（连接可重连）。
func NewStormProviderFunc(f func() (StormClient, error)) *StormProvider {
	return &StormProvider{client: f, rt: map[string]*stormIfaceRT{}}
}

// SetCountersReader 注入 policer 计数读数（stats segment；可空）。
func (p *StormProvider) SetCountersReader(r StormCountersReader) { p.counters = r }

// reset 清空进程内登记（恢复收敛/重连前调用：VPP 重启后表与 policer 全失，
// 声明未变也要按实况重建——登记清空即可让 ApplyInterface 全量重放）。
func (p *StormProvider) reset() {
	p.mu.Lock()
	p.rt = map[string]*stormIfaceRT{}
	p.mu.Unlock()
}

// stormDesired 从配置声明算出期望状态（kind → kbps；0 = 该类未配置）。
func stormDesired(sc *model.StormControl) map[string]uint32 {
	want := map[string]uint32{}
	if sc == nil {
		return want
	}
	if sc.BroadcastKbps > 0 {
		want[StormKindBroadcast] = uint32(sc.BroadcastKbps)
	}
	if sc.MulticastKbps > 0 {
		want[StormKindMulticast] = uint32(sc.MulticastKbps)
	}
	return want
}

// applyDone 判断期望是否与登记一致（一致 = 幂等重跑，零 VPP 调用）。
func (p *StormProvider) applyDone(ifname string, want map[string]uint32) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	rt := p.rt[ifname]
	if rt == nil {
		return len(want) == 0
	}
	if len(rt.kinds) != len(want) {
		return false
	}
	for k, v := range want {
		e := rt.kinds[k]
		if e == nil || e.kbps != v {
			return false
		}
	}
	// 有配置就必须挂着表（挂载是下发的一部分；登记里 attached 才是真实态）。
	if len(want) > 0 && !rt.attached {
		return false
	}
	return true
}

// ApplyInterface 把接口的风暴抑制收敛到声明：先撤旧后建新；声明为空 = 全部撤除。
// 接口不在数据面时按 ErrIfaceUnavailable 返回（由提交编排决定「延后收敛」与否）。
func (p *StormProvider) ApplyInterface(ctx context.Context, iface model.InterfaceConfig) error {
	want := stormDesired(iface.StormControl)
	if p.applyDone(iface.Name, want) {
		return nil // 幂等：状态已一致，零 VPP 调用
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	idx, ok, err := c.SwInterfaceIndex(iface.Name)
	if err != nil {
		return fmt.Errorf("解析接口 %s: %w", iface.Name, err)
	}
	if !ok {
		return fmt.Errorf("%w: %s"+ifaceMissingHint, ErrIfaceUnavailable, iface.Name)
	}
	if err := p.teardown(c, iface.Name, idx); err != nil {
		return err
	}
	if len(want) == 0 {
		return nil
	}
	return p.build(c, iface.Name, idx, want)
}

// teardown 撤掉该接口已下发的风暴抑制对象（幂等路径：不存在即已达成）。
//
// 两路来源（都必要，覆盖两种失去同步的形态）：
//   - **登记非空**（正常变更路径）：按登记里的表索引/名精确删除——先摘绑定（detach），
//     再逐类删 session → 删表 → 删 policer（契约口径）。每步成功才推进登记。
//   - **登记为空**（典型是 nfvisd 重启而 VPP 未重启：进程内登记已丢、数据面对象还在）：
//     按数据面实况清理——接口 L2 槽上挂着的表（`policer_classify_dump`；先核对掩码形状
//     与本产品两类一致才认作自己的，形状不符＝槽被别的表占用，如实报错且不动它）摘绑定后
//     带 DelChain 删表（两类并存时广播表链着组播表）；再按 `nfvis-storm-<if>-<kind>`
//     名字把 policer_dump 里存在的 policer 删掉。不这么做，重放会撞「policer 已存在
//     （拿不到索引、拿不到表引用）」并留下孤儿分类表。
func (p *StormProvider) teardown(c StormClient, ifname string, idx uint32) error {
	p.mu.Lock()
	var (
		attached    bool
		attachTable uint32
		kindsRT     map[string]*stormKindRT
	)
	if rt := p.rt[ifname]; rt != nil {
		attached, attachTable = rt.attached, rt.attachTable
		kindsRT = make(map[string]*stormKindRT, len(rt.kinds))
		for k, e := range rt.kinds {
			kindsRT[k] = e
		}
	}
	p.mu.Unlock()

	if len(kindsRT) > 0 {
		if attached {
			if err := c.PolicerClassifySetInterface(idx, attachTable, false); err != nil {
				return fmt.Errorf("解绑接口 %s 的风暴抑制分类表 %d: %w", ifname, attachTable, err)
			}
			p.setAttach(ifname, 0, false)
		}
		for _, kind := range stormKinds {
			e := kindsRT[kind]
			if e == nil {
				continue
			}
			if err := c.ClassifyDelSession(e.tableIdx, stormMatch(kind)); err != nil {
				return fmt.Errorf("删除接口 %s 的 %s 风暴抑制会话: %w", ifname, kind, err)
			}
			if err := c.ClassifyDelTable(e.tableIdx, false); err != nil {
				return fmt.Errorf("删除接口 %s 的 %s 风暴抑制分类表: %w", ifname, kind, err)
			}
			if _, err := c.PolicerAddDel(e.policerName, 0, 0, false); err != nil {
				return fmt.Errorf("删除接口 %s 的 %s 风暴抑制 policer: %w", ifname, kind, err)
			}
			p.clearKind(ifname, kind)
		}
		return nil
	}

	// 登记为空：按数据面实况清。
	t, hasAttached, err := c.AttachedL2Table(idx)
	if err != nil {
		return fmt.Errorf("读取接口 %s 的 L2 分类槽: %w", ifname, err)
	}
	if hasAttached {
		ti, known, err := c.ClassifyTableInfo(t)
		if err != nil {
			return fmt.Errorf("读取接口 %s 的分类表 %d 属性: %w", ifname, t, err)
		}
		if !known || !stormMaskKnown(ti.Mask) {
			return fmt.Errorf("接口 %s 的 L2 分类槽被非本产品对象占用（表 %d，掩码 %s）：风暴抑制无法收敛，"+
				"请先清除该占用（例如 VPP 侧的其它分类表绑定）", ifname, t, ti.Mask)
		}
		if err := c.PolicerClassifySetInterface(idx, t, false); err != nil {
			return fmt.Errorf("解绑接口 %s 的遗留风暴抑制分类表 %d: %w", ifname, t, err)
		}
		if err := c.ClassifyDelTable(t, true); err != nil {
			return fmt.Errorf("删除接口 %s 的遗留风暴抑制分类表 %d: %w", ifname, t, err)
		}
	}
	pols, err := c.PolicerDump()
	if err != nil {
		return fmt.Errorf("读取数据面 policer 清单: %w", err)
	}
	present := map[string]bool{}
	for _, pl := range pols {
		present[pl.Name] = true
	}
	for _, kind := range stormKinds {
		name := stormPolicerName(ifname, kind)
		if !present[name] {
			continue
		}
		if _, err := c.PolicerAddDel(name, 0, 0, false); err != nil {
			return fmt.Errorf("删除接口 %s 的遗留 %s 风暴抑制 policer: %w", ifname, kind, err)
		}
	}
	return nil
}

// build 下发期望状态：**组播表先建**（两类并存时广播表要用它作表链目标），随后两类各自
// policer → 表 → session，最后把接口 L2 槽挂到该挂的表上（详见文件头说明 1）。
// 任一步失败即回滚本次已建对象（不留孤儿表/policer）并返回原错误（附清理结果说明）。
func (p *StormProvider) build(c StormClient, ifname string, idx uint32, want map[string]uint32) error {
	order := make([]string, 0, len(want))
	for _, kind := range stormKinds {
		if want[kind] > 0 {
			order = append(order, kind)
		}
	}
	// 表链目标先建：两类并存时组播表在前（广播表的 NextTableIndex 要用它的索引）。
	if len(order) == 2 {
		order = []string{StormKindMulticast, StormKindBroadcast}
	}
	type built struct {
		tableIdx uint32
		polName  string
	}
	var done []built
	rollback := func() string {
		var msgs []string
		for _, b := range done {
			if err := c.ClassifyDelTable(b.tableIdx, false); err != nil {
				msgs = append(msgs, fmt.Sprintf("删表 %d: %v", b.tableIdx, err))
			}
			if _, err := c.PolicerAddDel(b.polName, 0, 0, false); err != nil {
				msgs = append(msgs, fmt.Sprintf("删 policer %s: %v", b.polName, err))
			}
		}
		if len(msgs) > 0 {
			return "；本次已建对象的回滚未全部成功（" + strings.Join(msgs, "、") + "）"
		}
		return ""
	}
	tableOf := map[string]uint32{}
	for _, kind := range order {
		kbps := want[kind]
		name := stormPolicerName(ifname, kind)
		pidx, err := c.PolicerAddDel(name, kbps, stormCbBytes(kbps), true)
		if err != nil {
			return fmt.Errorf("创建接口 %s 的 %s 风暴抑制 policer: %w%s", ifname, kind, err, rollback())
		}
		// 表链：广播表在两类并存时指向组播表（未命中继续在组播表里查，见文件头说明 1）。
		next := ^uint32(0)
		if kind == StormKindBroadcast {
			if m, ok := tableOf[StormKindMulticast]; ok {
				next = m
			}
		}
		tidx, err := c.ClassifyAddTable(stormMask(kind), next)
		if err != nil {
			return fmt.Errorf("创建接口 %s 的 %s 风暴抑制分类表: %w%s", ifname, kind, err, rollback())
		}
		done = append(done, built{tableIdx: tidx, polName: name})
		if err := c.ClassifyAddSession(tidx, stormMatch(kind), pidx); err != nil {
			return fmt.Errorf("创建接口 %s 的 %s 风暴抑制会话: %w%s", ifname, kind, err, rollback())
		}
		tableOf[kind] = tidx
		p.setKind(ifname, kind, &stormKindRT{policerName: name, policerIdx: pidx, tableIdx: tidx, kbps: kbps})
	}
	// 挂接口：两类并存挂广播表（表链已指向组播表）；只有组播时挂组播表。
	attach, ok := tableOf[StormKindBroadcast]
	if !ok {
		attach = tableOf[StormKindMulticast]
	}
	if err := c.PolicerClassifySetInterface(idx, attach, true); err != nil {
		for _, kind := range order {
			p.clearKind(ifname, kind)
		}
		return fmt.Errorf("接口 %s 挂风暴抑制分类表 %d: %w%s", ifname, attach, err, rollback())
	}
	p.setAttach(ifname, attach, true)
	return nil
}

// stormMaskKnown 该掩码是否本产品两类风暴抑制表之一的形状（按实况清理时的身份核对：
// 分类表没有名字/标记，形状是唯一可核对的身份线索）。
func stormMaskKnown(mask string) bool {
	return mask == hexMask(stormMask(StormKindBroadcast)) || mask == hexMask(stormMask(StormKindMulticast))
}

func hexMask(m []byte) string { return hex.EncodeToString(m) }

// 登记读写（锁内小步；VPP 调用一律在锁外）。

func (p *StormProvider) setKind(ifname, kind string, e *stormKindRT) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rt := p.rt[ifname]
	if rt == nil {
		rt = &stormIfaceRT{kinds: map[string]*stormKindRT{}}
		p.rt[ifname] = rt
	}
	rt.kinds[kind] = e
}

func (p *StormProvider) clearKind(ifname, kind string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rt := p.rt[ifname]
	if rt == nil {
		return
	}
	delete(rt.kinds, kind)
	if len(rt.kinds) == 0 && !rt.attached {
		delete(p.rt, ifname)
	}
}

func (p *StormProvider) setAttach(ifname string, table uint32, attached bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rt := p.rt[ifname]
	if rt == nil {
		rt = &stormIfaceRT{kinds: map[string]*stormKindRT{}}
		p.rt[ifname] = rt
	}
	rt.attachTable, rt.attached = table, attached
	if !attached && len(rt.kinds) == 0 {
		delete(p.rt, ifname)
	}
}

// Dataplane 读该接口风暴抑制的数据面实况（读视图用，全部为实测值）：
//   - policer 是否存在与实测 CIR（policer_dump，按名找）；
//   - 各类分类表的掩码/会话数/表链（classify_table_info，索引取自本 Provider 登记——
//     登记是「最后一次成功下发」，是产品知道自建表索引的唯一来源）；
//   - 接口 L2 槽当前挂的表（classify_table_by_interface）；
//   - 计数（stats segment；读不到给出原因，不猜）。
func (p *StormProvider) Dataplane(ctx context.Context, ifname string) (StormDataplane, error) {
	c, err := p.client()
	if err != nil {
		return StormDataplane{Reason: err.Error()}, nil
	}
	defer c.Close()
	idx, ok, err := c.SwInterfaceIndex(ifname)
	if err != nil {
		return StormDataplane{}, err
	}
	if !ok {
		return StormDataplane{Reason: "接口不在数据面（未由 DPDK 接管或名称不一致）"}, nil
	}
	pols, err := c.PolicerDump()
	if err != nil {
		return StormDataplane{Reason: "读取数据面 policer 失败：" + err.Error()}, nil
	}
	byName := make(map[string]StormPolicer, len(pols))
	for _, pl := range pols {
		byName[pl.Name] = pl
	}
	p.mu.Lock()
	rt := p.rt[ifname]
	var kindsRT map[string]*stormKindRT
	if rt != nil {
		kindsRT = make(map[string]*stormKindRT, len(rt.kinds))
		for k, e := range rt.kinds {
			kindsRT[k] = e
		}
	}
	p.mu.Unlock()

	out := StormDataplane{Available: true, Kinds: map[string]StormKindDataplane{}}
	if t, ok, err := c.AttachedL2Table(idx); err == nil && ok {
		out.AttachedL2Table, out.Attached = t, true
	}
	// 接口 L2 槽上那张表的实测属性（登记丢失时它仍能证明「哪类的表挂在口上」——按掩码认类）。
	var attachedInfo *StormTableInfo
	if out.Attached {
		if ti, ok, err := c.ClassifyTableInfo(out.AttachedL2Table); err == nil && ok {
			attachedInfo = &ti
		}
	}
	for _, kind := range stormKinds {
		kd := StormKindDataplane{}
		e := kindsRT[kind]
		name := stormPolicerName(ifname, kind)
		if pl, ok := byName[name]; ok {
			kd.PolicerPresent, kd.CirKbps = true, pl.CirKbps
		}
		switch {
		case e != nil:
			kd.PolicerIndex = e.policerIdx
			if ti, ok, err := c.ClassifyTableInfo(e.tableIdx); err == nil && ok {
				kd.Table = &ti
			}
		case attachedInfo != nil && attachedInfo.Mask == hexMask(stormMask(kind)):
			// 登记丢失（nfvisd 重启等）但表还挂在口上：按掩码形状认作该类的表（实测事实）
			kd.Table = attachedInfo
		}
		if e != nil {
			if p.counters == nil {
				kd.CountersReason = "未接入 stats 计数来源"
			} else if cnt, ok, reason := p.counters.StormCounters(ctx, e.policerIdx, e.policerName); ok {
				kd.Counters = &cnt
			} else {
				kd.CountersReason = reason
			}
		} else if kd.PolicerPresent {
			kd.CountersReason = "计数不可读（本进程未持有该接口的下发登记：刚重启或未重放）"
		}
		out.Kinds[kind] = kd
	}
	return out, nil
}
