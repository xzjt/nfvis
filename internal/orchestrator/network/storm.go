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
// 三处结构上的如实说明（实现期按可用 API 定形；真机已验证的部分见下）：
//
//  1. **接口的 L2 policer-classify 槽只有一张表**：`policer_classify_set_interface` 的
//     l2_table_index 是单值，`classify_table_by_interface` 也只回一个 l2_table_id——
//     两类各一张表不可能都直接挂上去。两类并存时挂**广播表**，并用它的 `NextTableIndex`
//     指向组播表（VPP classify 的**表链**：本表未命中时在下一张表里继续查）：广播帧在广播
//     表精确命中；其余组播帧未命中广播表、经链进组播表（I/G 位）命中；单播两表都不命中、
//     放行。【真机已验证：两类独立限速 + 表链成立】
//
//  2. **两类的掩码不同，必须两张表**：一张 classify 表只有一个掩码，而广播是目的 MAC
//     精确匹配（ff:ff:ff:ff:ff:ff）、组播是目的 MAC 首字节的 I/G 位（0x01 掩码「匹配位」
//     写法）；同一张表无法同时表达两类。另：I/G 位口径下**广播帧也算组播**，故只配
//     multicast 时广播也受该速率限制（手册写明；两类同配时广播走自己的精确表）。
//
//  3. **绑定事实是四态的，实况阴性 ≠「未挂」**（决策 #421）：
//     - **实况回读**（`classify_table_by_interface` 给出阳性表索引）——权威事实；
//     - **按登记**（实况阴性，但本进程登记说在位、且登记的那张表仍在数据面、形状仍是本类）；
//     - **自认领**（实况阴性、且本进程登记为空——如 nfvisd 重启而 VPP 存活——时按
//     「policer 按名在场 + 掩码形状 + 表链自洽 + 会话数 ≥1」把已存在的对象认回登记，
//     见 adoptDeclared；认领是**推断**，读视图如实标注依据）；
//     - **未挂**（以上都不成立）。
//
//  4. **孤儿清扫只识别不删（绑定不可回读的底座）**：分类表没有名字/标记，保护集在绑定不可
//     回读时**不可证**；此时任何删除都可能删到正在生效的表（接口槽悬空 ⇒ 首包空指针崩溃）。
//     故清扫改为：保护集（实况阳性绑定 ∪ 登记/认领表及其链）之外的候选**只识别不删**，
//     如实进读视图与巡检未收敛项；只有实况回读确实可用（取到阳性绑定）才允许删。
//     分类表随 VPP 重启自然消失——孤儿是瞬态，删的价值远小于误删代价。
//
//     为什么必须有中间态：本底座（VPP 26.06）**两个 API 都读不到 policer-classify 绑定**——
//     `classify_table_by_interface` 对 policer-classify 绑定恒回 l2_table_id=NONE（0xFFFFFFFF）、
//     `policer_classify_dump`（L2/IP4/IP6 三档）恒 0 条目（自建 govpp 探针实测，原始输出见
//     docs/evidence/v3-round3-release-clean-install-walkthrough.txt §2；macip 绑定则读得到），
//     而 vppctl 里绑定明明在场（`POLICER_CLAS` + `show classify table interface <if>`）。
//     曾把实况当「绑定的唯一事实源」：阴性被当成「没挂」⇒ 孤儿清扫把**接口 L2 槽正挂着、
//     正在生效**的分类表删掉 ⇒ 槽悬空指向已释放的表 ⇒ 首包在 policer 插件里空指针
//     （VPP 崩溃循环，真机 restart counter 实测到 23）。故：
//       - **绝不以 API 阴性单方面作删除依据**；
//       - 删表前一律先解绑并按事实确认（阳性走实况索引、登记态走登记索引，见 teardown）；
//       - 孤儿清扫在「任一接口登记在位而未取到阳性绑定」时**整轮放弃**（见 sweepOrphanTables）；
//       - 巡检的在场判据把「登记在位且表存在」算在场（见 declaredPresent）——只据阴性反复
//         拆建正是崩溃循环的放大器；
//       - 若未来底座恢复可回读，只需采信阳性（阴性=不可判）这一条不变。
//
//  5. **删登记态的表前必须按实测掩码核对形状（决策 #429）**：登记里的表索引是「最后一次成功
//     下发」的坐标，而分类表没有名字/标记、索引可能被 VPP 复用给外来对象（别的插件/手工建的
//     分类表）——只凭登记就删可能删到别人的表。故删除**登记态**的表之前一律读 `ClassifyTableInfo`
//     的实测掩码并与**本类**风暴掩码核对：一致才允许删；不一致即不删并如实报「登记指向的表形状
//     不符（疑似索引被复用），已跳过删除」进未收敛项；读不到（表已不存在）同样不删（删除方向的
//     「本就不在」＝已是目标状态，见 stormAbsentOK，故不报错、只推进登记）。带链删的**链目标**
//     同样要核对（stormChainDeleteAllowed）——链目标形状不符即改为只删本表，不顺着链删外来对象。
//     计数读取同理**一律按 policer 名匹配**（policer 索引在 `policer_dump` 里不可回读、且可能被
//     复用，按索引猜会错配到别的 policer 的计数；名字在数据面查不到即如实报不可读）。
//
// 判据/登记口径沿用本仓库既定纪律：
//   - 变更先撤旧后建新（旧 policer/表/session 全删，不留残渣，同 #380/#383 的教训）；
//   - 登记（进程内）语义 = **最后一个成功下发的状态**（决策 #363）：任何一步失败即返回，
//     登记停在最后成功态，重试不会因「登记说已下发」而静默跳过；
//   - 失效（VPP 重启/重连）走 reset：登记清空后由恢复收敛按声明全量重放（幂等）。

import (
	"context"
	"encoding/hex"
	"errors"
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
	// Table 实况回读到的本类分类表（接口 L2 槽挂着的表及其表链里按掩码认出；权威）。
	Table *StormTableInfo
	// TableByRegistration 按登记的该类分类表：实况阴性（本底座该绑定不可回读）但登记在位、
	// 且登记的那张表仍在数据面、形状仍是本类时给出。读视图据此如实呈现「按登记」，
	// 与「实况回读」「未挂」区分开（绝不用它冒充实况回读）。
	TableByRegistration *StormTableInfo
	// TableAdopted 自认领的该类分类表：登记为空后按「policer 按名在场 + 掩码形状 + 表链
	// 自洽 + 会话数 ≥1」从数据面认回来的表（同样是推断，读视图给依据，不冒充实况回读）。
	TableAdopted *StormTableInfo
	Counters     *StormCounters
	// CountersReason 计数不可读时的原因（如实说明；可读时为空）。
	CountersReason string
}

// 读视图的绑定事实取值（StormDataplane.Binding）：与 StormBinding* 四态一一对应，
// 消费方（CLI/REST 渲染）据此分述，不再把「阴性」当「未挂」。
const (
	StormBindingLive     = "live"     // 实况回读（API 阳性，权威）
	StormBindingDeclared = "declared" // 按登记（API 阴性但登记在位且登记的表仍在）
	StormBindingAdopted  = "adopted"  // 自认领（登记为空后按形状/表链把已存在的对象认回；推断）
	StormBindingNone     = "none"     // 未挂（以上都不成立）
)

// stormBindFact 接口 L2 槽上 policer-classify 绑定事实的**四态**（决策 #421①；收口后 +自认领）。
type stormBindFact int

const (
	// stormBindNone 未挂：实况无阳性绑定，且登记也不能证明在位
	//（登记不在位，或登记的表已不在数据面，或形状已不是本类——可能索引被复用）。
	stormBindNone stormBindFact = iota
	// stormBindLive 实况回读：classify_table_by_interface 给出阳性绑定（权威事实）。
	stormBindLive
	// stormBindDeclared 按登记：实况阴性（本底座读不到该绑定）但登记在位，且登记的那张表
	// 仍以本类形状存在——是「在位」的合理证据，也是解绑时唯一可用的索引来源。
	stormBindDeclared
	// stormBindAdopted 自认领：实况阴性、登记为空（进程重启而 VPP 存活）时，按
	// 「policer 按名在场 + 掩码形状 + 表链自洽 + 会话数 ≥1」从数据面认回来的在位状态。
	// 归属是**推断**：解绑必须拿到明确应答才允许删表（见 teardown）。
	stormBindAdopted
)

// StormDataplane 接口 storm control 的数据面实况（读视图）。
type StormDataplane struct {
	// Available 能否核对数据面（接口可解析 + 底座可查）；false 时 Reason 说明原因。
	Available bool
	Reason    string
	// Binding 接口 L2 槽绑定事实的**四态**取值（StormBindingLive / StormBindingDeclared /
	// StormBindingAdopted / StormBindingNone；内核数据面没有这层结构，保持空串——消费方
	// 不发射该字段）。
	Binding string
	// AttachedL2Table 实况回读到的绑定表索引（Binding=live 时有效；Attached 同）。
	AttachedL2Table uint32
	Attached        bool
	// DeclaredTable 按登记的绑定表索引（Binding=declared 时有效；实况索引不可回读）。
	DeclaredTable uint32
	// AdoptedTable 自认领的绑定表索引（Binding=adopted 时有效；归属为按形状/表链的推断）。
	AdoptedTable uint32
	// OrphanCandidates 数据面存在、形状属本产品却不被任何绑定/登记/认领覆盖的分类表索引
	//（只识别不删；来自最近一次巡检/清扫的只读识别，见 scanStormOrphans）。
	OrphanCandidates []uint32
	Kinds            map[string]StormKindDataplane
}

// ErrStormAbsent 目标对象在数据面不存在（解绑/删除方向的「已是目标状态」）。
//
// 适配器把 VPP 的「本就不在」类错误码（-6 No such entry / -65 No such table / -81 VALUE_EXIST）
// 归一为本哨兵，**由 Provider 决定容错**（而不是适配器私自吞掉）：这样「`No such table (-65)`
// 按已达成处理」这条策略在单测里可复现（真机实测：解绑一张未挂在该接口上的表会得到 -65）。
var ErrStormAbsent = errors.New("数据面对象不存在")

// stormAbsentOK 判断删除方向收到的错误是否表示「已是目标状态」。
func stormAbsentOK(err error) bool { return errors.Is(err, ErrStormAbsent) }

// StormClient storm control 的 VPP 能力集（薄适配；真机实现见 storm_govpp.go）。
//
// 删除方向的「本就不在」以 ErrStormAbsent 返回（由 Provider 决定是否按已达成继续）。
type StormClient interface {
	SwInterfaceIndex(ifname string) (uint32, bool, error)
	PolicerAddDel(name string, cirKbps uint32, cb uint64, add bool) (uint32, error)
	// ClassifyAddTable 建 L2 分类表（16 字节掩码），返回表索引。
	// nextTableIndex = ^uint32(0) 表示无表链；否则本表未命中时继续在该表里查。
	ClassifyAddTable(mask []byte, nextTableIndex uint32) (uint32, error)
	// ClassifyDelTable 删表（delChain=true 连同表链上的后续表一并删——两类并存时广播表链着
	// 组播表，按实况清理只有链根可循）；表不存在返回 ErrStormAbsent。
	ClassifyDelTable(tableIndex uint32, delChain bool) error
	// ClassifyAddSession 加一条 session：match 为 16 字节匹配向量，命中结果 = policerIndex。
	ClassifyAddSession(tableIndex uint32, match []byte, policerIndex uint32) error
	// ClassifyDelSession 删 session；不存在返回 ErrStormAbsent。
	ClassifyDelSession(tableIndex uint32, match []byte) error
	// PolicerClassifySetInterface 挂上（add=true）或摘掉该接口 L2 槽的分类表。
	// 挂上方向**不容错**（槽已被别的表占用时 VPP 不给覆盖，静默吞掉会留下「登记说新、实况是旧」
	// 的错位——真机实测即由此产生重复表）；摘除方向「表不存在/未挂」返回 ErrStormAbsent。
	PolicerClassifySetInterface(swIfIndex, l2TableIndex uint32, add bool) error
	// PolicerDump 列出数据面 policer（读视图 + 按名清理用）。
	PolicerDump() ([]StormPolicer, error)
	// AttachedL2Table 读该接口 L2 槽**当前实际挂着**的表（`classify_table_by_interface`）。
	// ok=false = **没有阳性结果**——注意这**不等于「未挂」**：本底座（VPP 26.06）对
	// policer-classify 绑定恒回 NONE，阴性只能当「不可判」（见 stormBindFact 与 storm_govpp.go
	// 的能力说明）。绑定事实的完整判定见 Provider 的 bindFact（阳性权威 + 登记兜底）。
	AttachedL2Table(swIfIndex uint32) (uint32, bool, error)
	// ClassifyTableInfo 读一张分类表的实测属性（掩码/会话数/表链）。
	ClassifyTableInfo(tableIndex uint32) (StormTableInfo, bool, error)
	// ClassifyTableIDs 列出全部 classify 表索引（孤儿分类表清扫用）。
	ClassifyTableIDs() ([]uint32, error)
	// AllInterfaceIndexes 列出全部接口索引（孤儿清扫的保护集计算用：仍被挂着的表不动）。
	AllInterfaceIndexes() ([]uint32, error)
	Close()
}

// StormCountersReader policer 计数读物（stats segment）。真机实现见 stats_linux.go；
// 非 Linux / 工具不可用时按「读不到」如实说明（不猜）。
type StormCountersReader interface {
	// StormCounters 按 policer **名**读该 policer 的计数（决策 #429②）：policer 索引在
	// `policer_dump` 里不可回读（自认领态只能以哨兵占位）、且可能被底座复用——按索引匹配会
	// 错配到别的 policer 的计数，故一律按名。名字在 stats segment 里查不到即 ok=false + 原因
	//（调用方如实呈现，不返回零值当真值）。
	StormCounters(ctx context.Context, policerName string) (StormCounters, bool, string)
}

// stormPolicerIdxUnknown 登记里「policer 索引不可回读」的哨兵（决策 #429②）。
//
// 为什么会有哨兵：VPP 的 `policer_dump` 条目（`PolicerDetails`）**只有名字与配置字段、没有
// 索引**（真机探针实测），索引只在 add 的应答里给出——自认领来的登记没有这一步，故以哨兵占位；
// 计数的读取据此**一律按 policer 名**匹配（索引既不可读、又可能被底座复用，按索引猜会错配）。
const stormPolicerIdxUnknown = ^uint32(0)

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
	// adopted=true 表示这份登记是**自认领**来的（按形状/表链推断，不是本进程下发过的）：
	// 读视图据此与「实况回读」「按登记」分列，teardown 据此要求解绑拿到明确应答才删表。
	adopted bool
}

// stormIfaceReg 接口登记的快照（锁内拷贝；VPP 调用一律在锁外做）。
// 「按登记」这一态与解绑用的登记索引都以它为准——调用方必须在推进登记**之前**取快照。
type stormIfaceReg struct {
	attached    bool
	attachTable uint32
	adopted     bool
	kinds       map[string]*stormKindRT
}

// regSnapshot 取某接口登记的快照（无登记时为零值）。
func (p *StormProvider) regSnapshot(ifname string) stormIfaceReg {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out stormIfaceReg
	if rt := p.rt[ifname]; rt != nil {
		out.attached, out.attachTable, out.adopted = rt.attached, rt.attachTable, rt.adopted
		out.kinds = make(map[string]*stormKindRT, len(rt.kinds))
		for k, e := range rt.kinds {
			out.kinds[k] = e
		}
	}
	return out
}

// regSnapshotAll 取全部接口登记的快照（孤儿清扫的保护集计算用）。
func (p *StormProvider) regSnapshotAll() map[string]stormIfaceReg {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]stormIfaceReg, len(p.rt))
	for ifname, rt := range p.rt {
		reg := stormIfaceReg{attached: rt.attached, attachTable: rt.attachTable, adopted: rt.adopted,
			kinds: make(map[string]*stormKindRT, len(rt.kinds))}
		for k, e := range rt.kinds {
			reg.kinds[k] = e
		}
		out[ifname] = reg
	}
	return out
}

// bindFact 判定接口 L2 槽上 policer-classify 绑定的**四态事实**（决策 #421①）：
//
//	① 实况回读（`classify_table_by_interface` 阳性）——权威，直接用；
//	② 实况阴性时**不得**据此断言「未挂」：本底座（VPP 26.06）该绑定两个 API 都读不到
//	   （阳性恒缺、dump 恒空，见 storm_govpp.go 的能力说明），阴性 = 不可判。此时按登记判断：
//	   登记在位、登记的表仍在数据面且形状仍是本类 ⇒ 「按登记」；
//	③ 两者都不成立 ⇒ 未挂。
//
// 返回的 table 是「槽上挂着哪张表」的最佳已知索引：阳性时是实况索引，按登记时是登记索引
// （teardown 的先解绑后删表就按它走）。
func (p *StormProvider) bindFact(c StormClient, ifname string, idx uint32, reg stormIfaceReg) (stormBindFact, uint32, error) {
	t, ok, err := c.AttachedL2Table(idx)
	if err != nil {
		return stormBindNone, 0, fmt.Errorf("读取接口 %s 的 L2 分类槽: %w", ifname, err)
	}
	if ok {
		return stormBindLive, t, nil
	}
	if reg.attached {
		ti, ok, err := c.ClassifyTableInfo(reg.attachTable)
		if err != nil {
			return stormBindNone, 0, fmt.Errorf("读取接口 %s 登记的分类表 %d 属性: %w", ifname, reg.attachTable, err)
		}
		// 表仍在且形状仍是本类才算「按登记/自认领在位」：分类表没有名字/标记，形状是唯一的
		// 身份线索；索引可能被 VPP 复用（真机实测 sw_if_index 即被复用），形状不符时不认。
		if ok && stormMaskKnown(ti.Mask) {
			if reg.adopted {
				return stormBindAdopted, reg.attachTable, nil
			}
			return stormBindDeclared, reg.attachTable, nil
		}
	}
	return stormBindNone, 0, nil
}

// stormAdopt 自认领的判定结果（决策 #421 收口）。
type stormAdopt int

const (
	// stormAdoptNotNeeded 不适用：登记非空（本进程知道自己的对象），或实况回读给得出阳性绑定。
	stormAdoptNotNeeded stormAdopt = iota
	// stormAdoptNoObjects 数据面没有本接口命名的 policer：没有可认的对象（按声明正常建）。
	stormAdoptNoObjects
	// stormAdoptOK 认领成功（登记已写入）。
	stormAdoptOK
	// stormAdoptPartial 存在本接口命名的 policer，但对象形态/表链不自洽、或无法判定归属：
	// **放弃认领**（不删不建，按未收敛如实报出）。
	stormAdoptPartial
)

// adoptDeclared 自认领（决策 #421 收口）：绑定不可回读 + 本进程登记为空（如 nfvisd 重启而
// VPP 存活、槽上仍挂着我们的表）时，用**全部可读的 API**（`policer_dump` /
// `classify_table_ids` / `classify_table_info`）把数据面已存在的本产品对象认回登记——
// 认回来就不拆不建（这正是「进程重启后重放把在用的表删掉 / attach 撞『槽已挂表』」的根治）。
//
// 判据（全部满足才认领；任一不满足即放弃，见 stormAdoptPartial）：
//   - 本接口各类（命名 `nfvis-storm-<if>-<kind>`）的 policer **按名在场**——分类表没有名字/
//     标记，policer 名是把数据面对象归到具体接口的**唯一身份线索**；
//   - 按声明形态找到**一组自洽的分类表**：两类并存 = 一张广播表（掩码全 f）的
//     `NextTableIndex` 指向一张组播表（掩码 0100…）、组播表不再有后续链；单类 = 该类的表
//     且不挂在任何表链上（不是别的表的目标）；
//   - 表上会话数 ≥1（空表不算在位）；
//   - **候选唯一**：数据面存在多组形态相符的表时无法判定归属（多接口同时重整等）——
//     认错会导致后续按错误索引解绑/删表 ⇒ 宁可放弃（宁可不认领，绝不误删）。
//
// 认领写入的登记带 adopted 标记：读视图据此显示「自认领（依据 policer 按名在场 + 表链匹配）」，
// teardown 对认领来的绑定要求解绑拿到**明确应答**才允许删表——归属是推断，「本就不在」的
// 应答恰恰说明推断错了（那张表是别人的），此时绝不删。
func (p *StormProvider) adoptDeclared(c StormClient, ifname string, idx uint32, reg stormIfaceReg) (stormAdopt, string, error) {
	if reg.attached || len(reg.kinds) > 0 {
		return stormAdoptNotNeeded, "", nil
	}
	// 前提：绑定不可回读。回读得出阳性就走既有「实况回读」权威路径，无须认领。
	if _, ok, err := c.AttachedL2Table(idx); err != nil {
		return stormAdoptNotNeeded, "", fmt.Errorf("读取接口 %s 的 L2 分类槽: %w", ifname, err)
	} else if ok {
		return stormAdoptNotNeeded, "", nil
	}
	pols, err := c.PolicerDump()
	if err != nil {
		return stormAdoptNotNeeded, "", fmt.Errorf("读取数据面 policer 清单: %w", err)
	}
	byName := make(map[string]StormPolicer, len(pols))
	for _, pl := range pols {
		byName[pl.Name] = pl
	}
	found := map[string]StormPolicer{}
	for _, kind := range stormKinds {
		if pl, ok := byName[stormPolicerName(ifname, kind)]; ok {
			found[kind] = pl
		}
	}
	if len(found) == 0 {
		return stormAdoptNoObjects, "", nil // 数据面没有本接口的痕迹：没有可认的对象
	}
	ids, err := c.ClassifyTableIDs()
	if err != nil {
		return stormAdoptPartial, "", fmt.Errorf("读取数据面分类表清单: %w", err)
	}
	var known []StormTableInfo
	for _, id := range ids {
		ti, ok, err := c.ClassifyTableInfo(id)
		if err != nil {
			return stormAdoptPartial, "", fmt.Errorf("读取分类表 %d 属性: %w", id, err)
		}
		if ok && stormMaskKnown(ti.Mask) {
			known = append(known, ti)
		}
	}
	if len(known) == 0 {
		return stormAdoptPartial, "数据面有本接口命名的 policer，但没有本产品形状的分类表", nil
	}
	// 被别的表用 NextTableIndex 指向的表＝在链上（不是链首；单类认领时排除）
	chained := map[uint32]bool{}
	for _, ti := range known {
		if ti.NextTableIndex != ^uint32(0) {
			chained[ti.NextTableIndex] = true
		}
	}
	type chainCand struct {
		head  uint32
		kinds map[string]uint32
	}
	var cands []chainCand
	if len(found) == 2 {
		for _, b := range known {
			if b.Mask != hexMask(stormMask(StormKindBroadcast)) || b.NextTableIndex == ^uint32(0) || b.Sessions < 1 {
				continue
			}
			for _, m := range known {
				if m.Index != b.NextTableIndex || m.Mask != hexMask(stormMask(StormKindMulticast)) ||
					m.NextTableIndex != ^uint32(0) || m.Sessions < 1 {
					continue
				}
				cands = append(cands, chainCand{head: b.Index, kinds: map[string]uint32{
					StormKindBroadcast: b.Index, StormKindMulticast: m.Index}})
			}
		}
	} else {
		kind := StormKindBroadcast
		if _, ok := found[StormKindMulticast]; ok {
			kind = StormKindMulticast
		}
		for _, ti := range known {
			if ti.Mask != hexMask(stormMask(kind)) || ti.NextTableIndex != ^uint32(0) ||
				ti.Sessions < 1 || chained[ti.Index] {
				continue
			}
			cands = append(cands, chainCand{head: ti.Index, kinds: map[string]uint32{kind: ti.Index}})
		}
	}
	switch {
	case len(cands) == 0:
		return stormAdoptPartial, "数据面有本接口命名的 policer，但没有形态自洽（掩码形状、表链、会话数）的分类表", nil
	case len(cands) > 1:
		return stormAdoptPartial, fmt.Sprintf("数据面存在 %d 组形态相符的分类表，无法判定哪一组属于本接口（多接口同时重整时请逐个接口处理，或重启数据面后重建）", len(cands)), nil
	}
	kinds := make(map[string]*stormKindRT, len(cands[0].kinds))
	for kind, tbl := range cands[0].kinds {
		kinds[kind] = &stormKindRT{
			policerName: stormPolicerName(ifname, kind),
			// policer 索引不可读（dump 条目不含索引；真机实测）：以哨兵占位，计数按名匹配
			//（决策 #429②；StormCountersReader 一律按名）。
			policerIdx: stormPolicerIdxUnknown,
			tableIdx:   tbl,
			kbps:       found[kind].CirKbps,
		}
	}
	if !p.setAdopted(ifname, cands[0].head, kinds) {
		return stormAdoptNotNeeded, "", nil // 并发里已有别的登记：以那一份为准
	}
	return stormAdoptOK, "", nil
}

// StormProvider 接口入向风暴抑制编排（决策 #385）。
type StormProvider struct {
	client func() (StormClient, error)
	// counters policer 计数读数（可空：空则读视图如实报「未接入」）。
	counters StormCountersReader

	mu sync.Mutex
	rt map[string]*stormIfaceRT
	// orphans 最近一次孤儿扫描识别出的候选（形状属本产品、却不被任何绑定/登记/认领覆盖的
	// 分类表索引）。**只识别不删**：进读视图如实呈现（见 scanStormOrphans）。
	orphans []uint32
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
//
// 重放路径**先尝试自认领**（adoptDeclared）：VPP 若没重启（只是 nfvisd 重启/重连），
// 数据面对象仍在，认回来即零删零建；VPP 真重启过则没有可认的对象，按声明正常重建。
func (p *StormProvider) reset() {
	p.mu.Lock()
	p.rt = map[string]*stormIfaceRT{}
	p.orphans = nil // 候选是数据面实况的读视图：连接失效后旧读数一并作废
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
	return p.rebuild(ctx, iface)
}

// rebuild 按声明强制重放（跳过 applyDone 的幂等早退）：先自认领，再按事实撤旧、按声明建新。
//
// 重放**不清登记**：登记是事实判定的「按登记」半边，teardown 要用它把解绑走在登记索引上
// 并拿到确认；先清登记就只能盲删（真机崩溃循环的成因）。
func (p *StormProvider) rebuild(ctx context.Context, iface model.InterfaceConfig) error {
	want := stormDesired(iface.StormControl)
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
	// 自认领（决策 #421 收口）：登记为空（进程重启而 VPP 存活）且绑定不可回读时，先把数据面
	// 已存在的对象按形状/表链认回登记——认回来且与声明一致就零改动返回（不拆不建）。
	if reg := p.regSnapshot(iface.Name); !reg.attached && len(reg.kinds) == 0 {
		res, reason, err := p.adoptDeclared(c, iface.Name, idx, reg)
		if err != nil {
			return err
		}
		switch res {
		case stormAdoptOK:
			if p.applyDone(iface.Name, want) {
				return nil
			}
		case stormAdoptPartial:
			return fmt.Errorf("接口 %s 的风暴抑制无法认领：%s（为避免误删数据面仍在引用的对象，"+
				"本次不删不建；可稍后重试，或 request vpp restart 清空数据面后重建）", iface.Name, reason)
		}
	}
	if err := p.teardown(c, iface.Name, idx); err != nil {
		return err
	}
	if len(want) == 0 {
		return nil
	}
	return p.build(c, iface.Name, idx, want)
}

// Reconcile 15s 巡检的在场复核（决策 #385 ④ 的兑现 / #394① / #421③）：对配置里声明了
// storm-control 的接口核对声明是否在场，缺项即**按登记重放**（不清登记——见 rebuild）。
//
// 沿用「实况优先、登记兜底、阴性不可判」：在场判据见 declaredPresent——实况阴性**不再**单独
// 判为「不在场」（本底座读不到该绑定；只据阴性就重放＝反复拆建，是崩溃循环的放大器）。
// **只补不猜**：声明为空/未声明的接口一律不动（撤除走提交编排）；接口不在数据面时不报错
// （由接口层的 RECOVERY_IFACE_MISSING 告警负责——风暴抑制无法在无接口时收敛，此处不制造重复告警）。
func (p *StormProvider) Reconcile(ctx context.Context, cfg model.Config) []error {
	var errs []error
	for _, iface := range cfg.Interfaces {
		want := stormDesired(iface.StormControl)
		if len(want) == 0 {
			continue
		}
		present, inDataplane, err := p.declaredPresent(iface.Name, want)
		if err != nil {
			errs = append(errs, fmt.Errorf("interfaces/%s/storm-control: %w", iface.Name, err))
			continue
		}
		if !inDataplane || present {
			continue
		}
		// 数据面缺项：按声明重放（登记保留给 teardown 做解绑确认；登记为空时 rebuild 先自认领）。
		if err := p.rebuild(ctx, iface); err != nil {
			errs = append(errs, fmt.Errorf("interfaces/%s/storm-control: %w", iface.Name, err))
		}
	}
	// 孤儿候选的只读识别（决策 #421 收口②）：**只识别不删**——绑定不可回读时保护集不可证，
	// 任何删除都可能删到在用的表（接口槽悬空 ⇒ 首包崩溃）；候选如实回报（巡检未收敛项），
	// 读视图同源呈现。只在「配置里有风暴抑制 或 本进程还持有登记」时扫描（没用过的机器零开销）。
	if p.stormInUse(cfg) {
		if c, err := p.client(); err == nil {
			sc := p.scanStormOrphans(c)
			c.Close()
			if sc.OK {
				p.setOrphanCandidates(sc.Candidates)
				if len(sc.Candidates) > 0 {
					errs = append(errs, fmt.Errorf("数据面存在未被绑定或登记覆盖的本产品形状分类表（只识别不删）: %s",
						stormIDsText(sc.Candidates)))
				}
			}
		}
	}
	return errs
}

// stormInUse 配置里是否有风暴抑制声明，或本进程是否还持有风暴登记（决定是否做全局孤儿扫描）。
func (p *StormProvider) stormInUse(cfg model.Config) bool {
	if len(p.regSnapshotAll()) > 0 {
		return true
	}
	for _, iface := range cfg.Interfaces {
		if len(stormDesired(iface.StormControl)) > 0 {
			return true
		}
	}
	return false
}

// stormIDsText 表索引列表渲染成「#1、#2」（读视图与未收敛项文案共用）。
func stormIDsText(ids []uint32) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("#%d", id))
	}
	return strings.Join(parts, "、")
}

// declaredPresent 核对声明的 storm 抑制是否在场（决策 #394①、#421③）：
// 绑定在场（实况回读阳性，或实况阴性但**登记在位且登记的表仍在数据面**（含自认领））
// 且各声明类的 policer 按名存在 ⇒ present=true。
//
// **实况阴性不是「不在场」**：本底座 policer-classify 绑定不可回读（阳性和 dump 两条路都
// 读不到），阴性 = 不可判；此时按登记核对——登记在位、表还在，就视为在场，不得重建
// （只据阴性反复重放正是真机崩溃循环的放大器）。登记也空（进程重启）时先**自认领**
// （adoptDeclared：policer 按名在场 + 掩码形状 + 表链自洽），认不回来即按未收敛如实报出
// （不删不建）。按登记/自认领态还要逐类核对登记表的形状，表被带外删除时如实判缺项、由重放补齐。
// inDataplane=false = 接口不在数据面（无从核对，调用方跳过）。
func (p *StormProvider) declaredPresent(ifname string, want map[string]uint32) (present, inDataplane bool, err error) {
	c, err := p.client()
	if err != nil {
		return false, false, err
	}
	defer c.Close()
	idx, ok, err := c.SwInterfaceIndex(ifname)
	if err != nil {
		return false, false, fmt.Errorf("解析接口 %s: %w", ifname, err)
	}
	if !ok {
		return false, false, nil
	}
	reg := p.regSnapshot(ifname)
	fact, _, err := p.bindFact(c, ifname, idx, reg)
	if err != nil {
		return false, true, err
	}
	if fact == stormBindNone {
		// 登记为空 + 绑定不可回读：先尝试自认领（决策 #421 收口①）——认回来即判在场、不重放
		//（重放要撤旧建新，而在用的表删不得，正是「槽已挂表」卡死与崩溃循环的来源）。
		res, reason, aerr := p.adoptDeclared(c, ifname, idx, reg)
		if aerr != nil {
			return false, true, aerr
		}
		switch res {
		case stormAdoptOK:
			fact, reg = stormBindAdopted, p.regSnapshot(ifname)
		case stormAdoptPartial:
			return false, true, fmt.Errorf("接口 %s 的风暴抑制无法认领：%s（不删不建；可稍后重试，或 request vpp restart 清空数据面后重建）",
				ifname, reason)
		default:
			return false, true, nil
		}
	}
	if fact == stormBindDeclared || fact == stormBindAdopted {
		// 按登记/自认领态的在场判据：各声明类的登记表都还在（形状仍是本类）——任何一类缺了都算缺项。
		for kind := range want {
			e := reg.kinds[kind]
			if e == nil {
				return false, true, nil
			}
			ti, ok, err := c.ClassifyTableInfo(e.tableIdx)
			if err != nil {
				return false, true, fmt.Errorf("读取接口 %s 的 %s 分类表 %d 属性: %w", ifname, kind, e.tableIdx, err)
			}
			if !ok || ti.Mask != hexMask(stormMask(kind)) {
				return false, true, nil
			}
		}
	}
	pols, err := c.PolicerDump()
	if err != nil {
		return false, true, fmt.Errorf("读取数据面 policer 清单: %w", err)
	}
	names := map[string]bool{}
	for _, pl := range pols {
		names[pl.Name] = true
	}
	for kind := range want {
		if !names[stormPolicerName(ifname, kind)] {
			return false, true, nil
		}
	}
	return true, true, nil
}

// teardown 撤掉该接口已下发的风暴抑制对象，并清理本产品的孤儿分类表（幂等：不存在即已达成）。
//
// 判据与顺序（真机实测修正 + 决策 #421③ + #429①）：
//  1. **绑定事实先按四态判定**（`bindFact`）：实况回读阳性为权威；阴性不等于未挂——本底座
//     该绑定不可回读，此时按登记（登记在位且登记的表仍在、形状仍是本类）。
//  2. **先解绑、后删表；删表前必须确认解绑已达成**：
//     - 阳性绑定走**实况索引**：解绑后**复核**一次实况——仍读到阳性绑定即解绑未达成，
//     一张表都不删并如实报错（真机上「表被删而接口 L2 槽仍指向它」正是首包空指针的崩溃形态）；
//     - 登记态走**登记索引**（槽上挂着哪张表只有登记知道）：解绑拿到确认（VPP 无错误返回，
//     或按本仓库口径「本就不在」＝已是目标状态）才开始删表；其它错误＝无法确认 ⇒ 不删；
//     - **自认领态**（归属是按形状/表链推断的）要求更严：只有明确应答（无错误）才算解绑
//     达成——应答「本就不在」说明推断错了（那张表是别人的），此时同样不删。
//     槽被别人的表占用（掩码形状不符本产品两类）时如实报错且**不动它**。
//  3. **登记里的逐类表**：与实况不同（陈旧登记、attach 未生效等）也要清，但**每删一张表前
//     一律先按该表的索引解绑并确认**（解绑失败即中止，不删）。
//  4. **删登记态的表（②③④）之前一律用实测掩码核对形状**（决策 #429①，见
//     stormCheckRegisteredTable）：与本类风暴掩码一致才允许删——登记索引可能被 VPP 复用给
//     外来对象；不符即不删并如实报未收敛，读不到（表已不存在）不删但按已达成推进。
//     带链删还要核对**链目标**（见 stormChainDeleteAllowed）：链目标形状不符即改为只删本表，
//     绝不顺着链删掉不是我们的对象。
//  5. **按名清 policer**（`policer_dump` 里存在的逐个删）：登记丢失（nfvisd 重启而 VPP 未重启）
//     时这是唯一途径（policer 按名幂等）。
//  6. **孤儿分类表清扫**（`sweepOrphanTables`）：分类表没有名字/标记，身份只能按**掩码形状**核对；
//     清扫范围与放弃条件见该函数（本底座登记在位而实况不可回读时整轮放弃）。
func (p *StormProvider) teardown(c StormClient, ifname string, idx uint32) error {
	// 登记快照在**最前**读（下面第 ①② 步会推进登记，之后读就看不到「陈旧绑定」了——
	// 而陈旧绑定正是需要尝试解绑并容忍 `No such table` 的那一幕）。
	reg := p.regSnapshot(ifname)
	fact, bound, err := p.bindFact(c, ifname, idx, reg)
	if err != nil {
		return err
	}

	// ① 实况回读阳性（权威）：按实况索引解绑 → 复核解绑已达成 → 再删表（含表链）
	switch fact {
	case stormBindLive:
		if err := p.unbindLiveForDelete(c, ifname, idx, bound); err != nil {
			return err
		}
		kind := stormRegisteredKind(reg, bound)
		delChain, err := stormChainDeleteAllowed(c, bound, kind)
		if err != nil {
			return err
		}
		if err := c.ClassifyDelTable(bound, delChain); err != nil && !stormAbsentOK(err) {
			return fmt.Errorf("删除接口 %s 的分类表 %d%s: %w", ifname, bound, stormChainText(delChain), err)
		}
		p.setAttach(ifname, 0, false)
	// ② 按登记/自认领（实况不可回读，登记是「槽上挂着哪张表」的唯一线索）：解绑走登记索引，
	// 同样先解绑、确认后才删表。**自认领的归属是推断**：只有拿到明确应答（无错误）才算解绑
	// 达成——应答「本就不在」说明这张表并不挂在本接口槽上（推断错了，多半是别的接口的表），
	// 此时绝不删（宁留孤儿不误删）。
	case stormBindDeclared, stormBindAdopted:
		switch uerr := c.PolicerClassifySetInterface(idx, bound, false); {
		case uerr == nil:
		case stormAbsentOK(uerr) && fact == stormBindDeclared:
			// 本仓库既有口径：「本就不在」＝已是目标状态（陈旧登记/带外删除后的自愈路径）
		case stormAbsentOK(uerr):
			return fmt.Errorf("接口 %s 的自认领分类表 %d 不挂在本接口的 L2 槽上：自认领的归属是按形状与表链"+
				"推断的，为避免误删其它对象，本次不删表（可 request vpp restart 清空数据面后重建）", ifname, bound)
		default:
			return fmt.Errorf("解绑接口 %s 的 L2 分类表 %d 未获确认（%v）：本数据面版本读不到该绑定，"+
				"无法确认槽已摘除；为避免删除数据面仍在引用的表，本次不删表（可稍后重试，或 request vpp restart 清空数据面后重试）", ifname, bound, uerr)
		}
		// 删表许可的另一半（决策 #429①）：实测形状必须仍是本类——登记索引可能被 VPP 复用给
		// 外来对象（此时绝不动它）。
		kind := stormRegisteredKind(reg, bound)
		v, ti, err := stormCheckRegisteredTable(c, bound, kind)
		if err != nil {
			return err
		}
		if v == stormShapeReused {
			return stormShapeReusedErr(ifname, kind, bound, ti.Mask)
		}
		if v == stormShapeOwn {
			delChain, err := stormChainDeleteAllowed(c, bound, kind)
			if err != nil {
				return err
			}
			if err := c.ClassifyDelTable(bound, delChain); err != nil && !stormAbsentOK(err) {
				return fmt.Errorf("删除接口 %s 的分类表 %d%s: %w", ifname, bound, stormChainText(delChain), err)
			}
		}
		p.setAttach(ifname, 0, false)
	}

	// ③ 陈旧登记（实况阳性且与登记不一致：上一次 attach 未生效等）——解绑登记索引，幂等。
	// 删表许可来自①的实况权威：槽上挂的是实况那张，登记那张不在槽上。
	if fact == stormBindLive && reg.attached && reg.attachTable != bound {
		if err := c.PolicerClassifySetInterface(idx, reg.attachTable, false); err != nil && !stormAbsentOK(err) {
			return fmt.Errorf("解绑接口 %s 的登记分类表 %d: %w", ifname, reg.attachTable, err)
		}
	}

	// ④ 登记里的逐类表：删前一律先按该表的索引解绑并确认（拿不到确认即中止，不删；
	// ①② 已解绑过的那张不再重复调用），再按实测掩码核对形状（决策 #429①）后才允许删。
	for _, kind := range stormKinds {
		e := reg.kinds[kind]
		if e == nil {
			continue
		}
		if fact == stormBindNone || e.tableIdx != bound {
			if err := c.PolicerClassifySetInterface(idx, e.tableIdx, false); err != nil && !stormAbsentOK(err) {
				return fmt.Errorf("解绑接口 %s 的 %s 风暴抑制分类表 %d 未获确认（%v）：无法确认槽已摘除，"+
					"为避免删除数据面仍在引用的表，本次不删表（可稍后重试，或 request vpp restart 清空数据面后重试）", ifname, kind, e.tableIdx, err)
			}
		}
		v, ti, err := stormCheckRegisteredTable(c, e.tableIdx, kind)
		if err != nil {
			return err
		}
		switch v {
		case stormShapeReused:
			return stormShapeReusedErr(ifname, kind, e.tableIdx, ti.Mask)
		case stormShapeGone:
			// 该索引上已无本产品表（带外删除等）：删除方向的「本就不在」＝已是目标状态
			//（stormAbsentOK 口径），故不报错；**不删**（也没有可删的对象），登记随之作废并继续。
			p.clearKind(ifname, kind)
			continue
		}
		if err := c.ClassifyDelSession(e.tableIdx, stormMatch(kind)); err != nil && !stormAbsentOK(err) {
			return fmt.Errorf("删除接口 %s 的 %s 风暴抑制会话: %w", ifname, kind, err)
		}
		if err := c.ClassifyDelTable(e.tableIdx, false); err != nil && !stormAbsentOK(err) {
			return fmt.Errorf("删除接口 %s 的 %s 风暴抑制分类表: %w", ifname, kind, err)
		}
		p.clearKind(ifname, kind)
	}
	// ⑤ 按名清 policer（登记丢失时的唯一途径；按名幂等）
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
		if _, err := c.PolicerAddDel(name, 0, 0, false); err != nil && !stormAbsentOK(err) {
			return fmt.Errorf("删除接口 %s 的 %s 风暴抑制 policer: %w", ifname, kind, err)
		}
	}
	// ⑥ 孤儿表清扫（含历史重复表）
	return p.sweepOrphanTables(c)
}

// unbindLiveForDelete 阳性绑定的解绑：按**实况索引**解绑，再复核解绑已达成，最后核对形状。
// 任一步不成立即如实报错（调用方一张表都不删）。
func (p *StormProvider) unbindLiveForDelete(c StormClient, ifname string, idx, bound uint32) error {
	ti, known, err := c.ClassifyTableInfo(bound)
	if err != nil {
		return fmt.Errorf("读取接口 %s 的分类表 %d 属性: %w", ifname, bound, err)
	}
	if known && !stormMaskKnown(ti.Mask) {
		return fmt.Errorf("接口 %s 的 L2 分类槽被非本产品对象占用（表 %d，掩码 %s）：风暴抑制无法收敛，"+
			"请先清除该占用（例如 VPP 侧的其它分类表绑定）", ifname, bound, ti.Mask)
	}
	if err := c.PolicerClassifySetInterface(idx, bound, false); err != nil && !stormAbsentOK(err) {
		return fmt.Errorf("解绑接口 %s 的 L2 分类表 %d 未获确认（%v）：为避免删除数据面仍在引用的表，本次不删表（可稍后重试，或 request vpp restart 清空数据面后重试）",
			ifname, bound, err)
	}
	// 复核：解绑后仍读到阳性绑定 ⇒ 解绑未达成（槽还指着表），此时绝不删表。
	if t2, ok2, err := c.AttachedL2Table(idx); err != nil {
		return fmt.Errorf("复核接口 %s 的 L2 分类槽: %w", ifname, err)
	} else if ok2 {
		return fmt.Errorf("接口 %s 的 L2 分类表解绑未达成（复核仍读到绑定表 %d）：为避免删除数据面仍在引用的表，本次不删表",
			ifname, t2)
	}
	return nil
}

// stormShapeVerdict 删除**登记态**表之前「形状复核」的结论（决策 #429①）。
type stormShapeVerdict int

const (
	// stormShapeOwn 实测掩码与本类风暴掩码一致：是「最后一次成功下发」的那张表，允许删。
	stormShapeOwn stormShapeVerdict = iota
	// stormShapeGone 属性读不到（该索引上已无表）：**不删**。删除方向的「本就不在」＝已是
	// 目标状态（既有 stormAbsentOK 口径，同族错误码 -6/-65/-81），故不报错、只推进登记。
	stormShapeGone
	// stormShapeReused 实测掩码与本类不符（疑似登记索引被 VPP 复用给外来表）：**不删**，
	// 如实报未收敛（宁停在未收敛，也不动不是我们的对象）。
	stormShapeReused
)

// stormCheckRegisteredTable 删除**登记态**表之前的形状复核（决策 #429①）。
//
// 为什么必须复核：登记里的表索引是「最后一次成功下发」的坐标，而分类表没有名字/标记、索引
// 可能被 VPP 复用给外来对象（别的插件/手工建的分类表）——只凭登记就删可能删到别人的表。
//
// 复核口径：读 `ClassifyTableInfo` 的**实测掩码**并与本类风暴掩码核对（kind 为空 = 登记里
// 认不出该表归属哪一类，此时接受本产品两类掩码之一——挂表按构造必属两类之一，这与实况阳性
// 路径同一身份判据）。读属性**报错**＝无法核对 ⇒ 返回错误（调用方不删、如实报；出错时随错误
// 给出的是最保守的取值 = 不删）；表不存在 ⇒ stormShapeGone（不删，按已达成处理）；形状不符
// ⇒ stormShapeReused（不删，如实报未收敛）。
func stormCheckRegisteredTable(c StormClient, tableIdx uint32, kind string) (stormShapeVerdict, StormTableInfo, error) {
	ti, ok, err := c.ClassifyTableInfo(tableIdx)
	if err != nil {
		return stormShapeGone, ti, fmt.Errorf("读取分类表 %d 属性（删前形状复核）: %w", tableIdx, err)
	}
	if !ok {
		return stormShapeGone, ti, nil
	}
	if kind == "" {
		if !stormMaskKnown(ti.Mask) {
			return stormShapeReused, ti, nil
		}
		return stormShapeOwn, ti, nil
	}
	if ti.Mask != hexMask(stormMask(kind)) {
		return stormShapeReused, ti, nil
	}
	return stormShapeOwn, ti, nil
}

// stormRegisteredKind 登记里持有该表索引的类（找不到返回空串——该索引不被任何已登记类持有，
// 例如登记里的挂表坐标与逐类表都已不同步；此时形状核对退到「本产品两类掩码之一」）。
func stormRegisteredKind(reg stormIfaceReg, tableIdx uint32) string {
	for _, kind := range stormKinds {
		if e := reg.kinds[kind]; e != nil && e.tableIdx == tableIdx {
			return kind
		}
	}
	return ""
}

// stormShapeReusedErr 登记索引指向的表形状不符时的如实报错（措辞单一事实源：决策 #429①，
// ②与④两条删除路径共用）。
func stormShapeReusedErr(ifname, kind string, tableIdx uint32, mask string) error {
	label, want := "登记的分类表", "本产品两类风暴掩码之一"
	if kind != "" {
		label, want = kind+" 风暴抑制分类表", "本类风暴掩码 "+hexMask(stormMask(kind))
	}
	if mask == "" { // 底座没回掩码（空掩码表）：如实说是「空掩码」，不留空白
		mask = "（空）"
	}
	return fmt.Errorf("接口 %s 的 %s %d：登记指向的表形状不符（疑似索引被复用），已跳过删除"+
		"（实测掩码 %s，非%s；为避免删除数据面仍在引用的外来对象，本次不删该表；"+
		"可 request vpp restart 清空数据面后重建）", ifname, label, tableIdx, mask, want)
}

// stormChainDeleteAllowed 链删除前的**链目标**核对（决策 #429①：删表前按实测掩码核对形状，
// 这条判据同样适用于「顺带删掉的那张」）。
//
// 为什么必须核对：两类并存时广播表用 `NextTableIndex` 链着组播表，删除只有链根可循（见
// ClassifyDelTable 的说明）——带链删会**顺着链删掉链目标**，而链目标同样是登记态表、其索引
// 同样可能被 VPP 复用给外来对象。故链目标形状不符（或不属本产品、或读不到）时**不带链删**
// （宁可留下一个孤立的链目标，也绝不动不是我们的对象）；链目标若是登记里的另一类表，逐类
// 核对（teardown ④）会照常把「登记指向的表形状不符……已跳过删除」如实报出。
//
// 返回 delChain：无链目标（unset/自指）或链目标已不在 ⇒ true（带链标志无对象可波及，等价于
// 普通删表）；链目标存在且形状与本类（kind 为空 = 本产品两类之一）一致 ⇒ true，否则 false。
func stormChainDeleteAllowed(c StormClient, tableIdx uint32, kind string) (bool, error) {
	ti, ok, err := c.ClassifyTableInfo(tableIdx)
	if err != nil {
		return false, fmt.Errorf("读取分类表 %d 属性（删前链核对）: %w", tableIdx, err)
	}
	if !ok {
		return true, nil // 表已不在：删除调用按「本就不在」收场，标志无对象可波及
	}
	if ti.NextTableIndex == ^uint32(0) || ti.NextTableIndex == tableIdx {
		return true, nil // 无链（unset/自指）
	}
	nt, ok, err := c.ClassifyTableInfo(ti.NextTableIndex)
	if err != nil {
		return false, fmt.Errorf("读取链上分类表 %d 属性（删前链核对）: %w", ti.NextTableIndex, err)
	}
	if !ok {
		return false, nil // 链目标已不在：没有可删的链对象（不带链标志，也就不会因陈旧链报错）
	}
	if kind == StormKindBroadcast {
		// 本产品的链只有一种形状：广播表 ⇒ 组播表（见文件头说明 1）。
		return nt.Mask == hexMask(stormMask(StormKindMulticast)), nil
	}
	if kind == StormKindMulticast {
		return false, nil // 本类不挂链：链目标若存在也不认得（不带链删）
	}
	return stormMaskKnown(nt.Mask), nil
}

// stormChainText 删除文案里的链标记（与实际发出的 delChain 标志同源，不虚报「含表链」）。
func stormChainText(delChain bool) string {
	if delChain {
		return "（含表链）"
	}
	return "（不含表链）"
}

// sweepOrphanTables 孤儿分类表清扫：**只识别，绑定可回读时才允许删**（决策 #421 收口②）。
//
// 为什么不再默认删：分类表没有名字/标记，身份只能按**掩码形状**核对；本底座
// （VPP 26.06）policer-classify 绑定不可回读（阴性 = 不可判，见 storm_govpp.go 的能力说明），
// 意味着保护集**不可证**——任何删除都可能删到接口 L2 槽上正在生效的表（槽悬空 ⇒ 首包在
// policer 插件里空指针崩溃）。而分类表随 VPP 重启自然消失，孤儿是瞬态：删的价值远小于
// 误删的代价。故：
//   - 只读识别候选（形状属本产品两类、且不被任何实况阳性绑定/登记/自认领覆盖的表），
//     如实进读视图（`StormProvider.Dataplane` 的 OrphanCandidates）与巡检未收敛项；
//   - **只有本轮扫描取到过阳性绑定**（说明该底座的绑定回读确实可用）、且保护集完整
//     （没有「登记在位却取不到阳性绑定」的接口）才执行删除；
//   - 表清单/接口清单/保护集任一不可得 ⇒ 放弃（不清不删，留待下次）。
func (p *StormProvider) sweepOrphanTables(c StormClient) error {
	sc := p.scanStormOrphans(c)
	if !sc.OK {
		return nil // 清单/保护集不可得：放弃清扫与识别（不误删、不误报）
	}
	p.setOrphanCandidates(sc.Candidates) // 只读识别的候选进读视图（只识别不删）
	if !sc.Deletable {
		return nil // 绑定不可回读（或保护集不可证）：绝不删（宁留孤儿不误删）
	}
	for _, id := range sc.Candidates {
		if err := c.ClassifyDelTable(id, true); err != nil && !stormAbsentOK(err) {
			return fmt.Errorf("清理遗留风暴抑制分类表 %d: %w", id, err)
		}
	}
	return nil
}

// stormOrphanScan 孤儿扫描的结果（只读识别；决策 #421 收口②）。
type stormOrphanScan struct {
	// OK 扫描完成（false = 表清单/接口清单/保护集不可得：调用方按「放弃」处理，既不删也不报）。
	OK bool
	// Deletable 允许删除：本轮扫描取到过**实况阳性绑定**（该底座的绑定回读可用）且保护集
	// 完整（没有「登记在位却取不到阳性绑定」的接口）。
	Deletable bool
	// Candidates 形状属本产品两类、且不被任何实况阳性绑定/登记/自认领覆盖的分类表索引。
	Candidates []uint32
}

// scanStormOrphans 只读识别孤儿候选与删除许可（决策 #421②）：
// 保护集 = 全部接口的实况阳性绑定（含表链）∪ 全部接口登记的表及其链（含自认领）；
// 任一接口「登记在位而未取到阳性绑定」⇒ 保护集不完整（Deletable=false，宁留孤儿不误删）。
// 只删掩码形状命中本产品两类的表（别人的分类表形状不符，不动）。
func (p *StormProvider) scanStormOrphans(c StormClient) stormOrphanScan {
	ids, err := c.ClassifyTableIDs()
	if err != nil {
		return stormOrphanScan{}
	}
	if len(ids) == 0 {
		return stormOrphanScan{OK: true} // 没有分类表：无可识别、无可删
	}
	ifaces, err := c.AllInterfaceIndexes()
	if err != nil {
		return stormOrphanScan{}
	}
	var sc stormOrphanScan
	sc.OK = true
	bound := map[uint32]bool{}
	positive := map[uint32]bool{}
	for _, i := range ifaces {
		t, ok, err := c.AttachedL2Table(i)
		if err != nil {
			return stormOrphanScan{} // 保护集不完整
		}
		if !ok {
			continue
		}
		sc.Deletable = true // 取到了阳性绑定 ⇒ 本底座的绑定回读可用
		positive[i] = true
		p.protectStormChain(c, bound, t)
	}
	for ifname, reg := range p.regSnapshotAll() {
		if !reg.attached && len(reg.kinds) == 0 {
			continue
		}
		idx, ok, err := c.SwInterfaceIndex(ifname)
		if err != nil {
			return stormOrphanScan{}
		}
		if !ok {
			continue // 接口已不在数据面：它的 L2 槽随之消失，没有「挂在它上面」的表
		}
		if !positive[idx] {
			// 登记在位却取不到阳性绑定：本底座阴性不可判，无法证明这些表不在槽上
			// ⇒ 保护集不完整 ⇒ 本轮不允许删除（宁留孤儿不误删）。
			sc.Deletable = false
		}
		if reg.attached {
			p.protectStormChain(c, bound, reg.attachTable)
		}
		for _, kind := range stormKinds {
			if e := reg.kinds[kind]; e != nil {
				p.protectStormChain(c, bound, e.tableIdx)
			}
		}
	}
	for _, id := range ids {
		if bound[id] {
			continue
		}
		ti, ok, err := c.ClassifyTableInfo(id)
		if err != nil {
			return stormOrphanScan{}
		}
		if !ok || !stormMaskKnown(ti.Mask) {
			continue
		}
		sc.Candidates = append(sc.Candidates, id)
	}
	return sc
}

// protectStormChain 把表 t 及其沿 `NextTableIndex` 的表链放进保护集（两类并存时另一张只在
// 链上；深度上限 4 防环）。
func (p *StormProvider) protectStormChain(c StormClient, bound map[uint32]bool, t uint32) {
	bound[t] = true
	for depth, next := 0, t; depth < 4; depth++ {
		ti, ok, err := c.ClassifyTableInfo(next)
		if err != nil || !ok || ti.NextTableIndex == ^uint32(0) || bound[ti.NextTableIndex] {
			break
		}
		bound[ti.NextTableIndex] = true
		next = ti.NextTableIndex
	}
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
			if err := c.ClassifyDelTable(b.tableIdx, false); err != nil && !stormAbsentOK(err) {
				msgs = append(msgs, fmt.Sprintf("删表 %d: %v", b.tableIdx, err))
			}
			if _, err := c.PolicerAddDel(b.polName, 0, 0, false); err != nil && !stormAbsentOK(err) {
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
	// attached=true 只由 build（真正下发成功）写入：这是「本进程下发过的」状态，
	// 自认领标记随之清掉（读视图据此区分「按登记」与「自认领」）。
	rt.adopted = false
	if !attached && len(rt.kinds) == 0 {
		delete(p.rt, ifname)
	}
}

// setAdopted 写入自认领的登记（**仅在登记仍为空时生效**：并发里已有别的登记时返回 false，
// 以那一份为准，不覆盖）。
func (p *StormProvider) setAdopted(ifname string, head uint32, kinds map[string]*stormKindRT) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rt := p.rt[ifname]; rt != nil && (rt.attached || len(rt.kinds) > 0) {
		return false
	}
	p.rt[ifname] = &stormIfaceRT{kinds: kinds, attachTable: head, attached: true, adopted: true}
	return true
}

// setOrphanCandidates 记录最近一次孤儿扫描识别的候选（读视图用；**只识别不删**）。
func (p *StormProvider) setOrphanCandidates(ids []uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(ids) == 0 {
		p.orphans = nil
		return
	}
	p.orphans = append([]uint32(nil), ids...)
}

// orphanCandidates 最近一次识别的孤儿候选（读视图用）。
func (p *StormProvider) orphanCandidates() []uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]uint32(nil), p.orphans...)
}

// Dataplane 读该接口风暴抑制的数据面实况（读视图用，全部为实测值）：
//   - policer 是否存在与实测 CIR（policer_dump，按名找）；
//   - **绑定事实四态**（决策 #421①，收口后 +自认领）：实况回读（权威）/按登记（本底座该绑定
//     不可回读）/自认领（登记为空后按 policer 按名在场 + 表链匹配从数据面认回，归属是推断）/
//     未挂——阴性绝不当「未挂」，按登记/自认领的表必须核对仍在且形状仍是本类；
//     （自认领由恢复重放与 15s 巡检写入登记，读视图如实呈现其结果；本方法是只读的。）
//   - 各类分类表的掩码/会话数/表链（classify_table_info；实况链按接口 L2 槽挂着的表展开，
//     索引取自本 Provider 登记——登记是「最后一次成功下发」，是产品知道自建表索引的唯一来源）；
//   - 计数（stats segment；读不到给出原因，不猜）；
//   - **孤儿候选**（只读识别的形状相符却不被绑定/登记覆盖的表，见 scanStormOrphans；
//     只识别不删，来自最近一次巡检/清扫的识别结果）。
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
	reg := p.regSnapshot(ifname)
	kindsRT := reg.kinds

	out := StormDataplane{Available: true, Binding: StormBindingNone, Kinds: map[string]StormKindDataplane{}}
	fact, bound, err := p.bindFact(c, ifname, idx, reg)
	if err != nil {
		return StormDataplane{Reason: "读取接口 L2 分类槽失败：" + err.Error()}, nil
	}
	switch fact {
	case stormBindLive:
		out.Binding, out.AttachedL2Table, out.Attached = StormBindingLive, bound, true
	case stormBindDeclared:
		out.Binding, out.DeclaredTable = StormBindingDeclared, bound
	case stormBindAdopted:
		out.Binding, out.AdoptedTable = StormBindingAdopted, bound
	}
	out.OrphanCandidates = p.orphanCandidates()
	// 实况表链：接口 L2 槽挂着的那张表 + 沿 next_table_index 链上的表（两类并存时另一类只在
	// 链上，见 build 的表链说明）。「实况在位」只能落在这个**实况**集合里——进程内登记只补
	// policer 索引/计数，不再充当表在位的证据（决策 #401：登记有、槽被别的对象占用时必须如实报
	// 未挂，否则限速静默不生效却显示「在位」）。逐张取属性；取不到即止，上限防环。
	var liveChain []StormTableInfo
	for cur, depth := out.AttachedL2Table, 0; out.Attached && depth < 4; depth++ {
		ti, ok, err := c.ClassifyTableInfo(cur)
		if err != nil || !ok {
			break
		}
		liveChain = append(liveChain, ti)
		if ti.NextTableIndex == ^uint32(0) || ti.NextTableIndex == cur {
			break
		}
		cur = ti.NextTableIndex
	}
	for _, kind := range stormKinds {
		kd := StormKindDataplane{}
		e := kindsRT[kind]
		name := stormPolicerName(ifname, kind)
		if pl, ok := byName[name]; ok {
			kd.PolicerPresent, kd.CirKbps = true, pl.CirKbps
		}
		if e != nil {
			kd.PolicerIndex = e.policerIdx
		}
		// 表「在位」按实况：实况链里掩码与本类风暴掩码一致的表才算（登记丢失时同样成立）。
		for i := range liveChain {
			if liveChain[i].Mask == hexMask(stormMask(kind)) {
				kd.Table = &liveChain[i]
				break
			}
		}
		// 按登记/自认领（决策 #421①④）：实况阴性（本底座读不到该绑定）但登记在位、登记的表
		// 仍在数据面且形状仍是本类 ⇒ 如实标为「按登记」或「自认领」（后者注明依据是推断），
		// 与「实况回读」「未挂」四态分列。
		if kd.Table == nil && (fact == stormBindDeclared || fact == stormBindAdopted) && e != nil {
			if ti, ok, err := c.ClassifyTableInfo(e.tableIdx); err == nil && ok && ti.Mask == hexMask(stormMask(kind)) {
				if fact == stormBindAdopted {
					kd.TableAdopted = &ti
				} else {
					kd.TableByRegistration = &ti
				}
			}
		}
		if e != nil {
			// 计数一律按 policer **名**读（决策 #429②）：索引在 policer_dump 里不可回读
			// （自认领态只有哨兵）且可能被底座复用——按索引猜会错配到别的 policer 的计数。
			// 名字在数据面查不到即如实报不可读（连读数来源都不问：没有对象可读）。
			switch {
			case !kd.PolicerPresent:
				kd.CountersReason = "计数不可读（policer 不在数据面）"
			case p.counters == nil:
				kd.CountersReason = "未接入 stats 计数来源"
			default:
				if cnt, ok, reason := p.counters.StormCounters(ctx, e.policerName); ok {
					kd.Counters = &cnt
				} else {
					kd.CountersReason = reason
				}
			}
		} else if kd.PolicerPresent {
			kd.CountersReason = "计数不可读（本进程未持有该接口的下发登记：刚重启或未重放）"
		}
		out.Kinds[kind] = kd
	}
	return out, nil
}
