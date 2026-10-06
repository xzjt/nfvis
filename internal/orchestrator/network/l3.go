package network

// M3-4：L3 编排（FR-NET-013/014）：VRF（IP table）、L3 接口地址（v4/v6）、静态路由、
// L2 交换机的 BVI 三层网关。运行态 FIB 经 ip_route_dump 提供（/vrfs/{name}/routes）。
//
// 映射（附录 B）：L3 虚拟交换机（type=l3）→ 同名 Vrf 条目 → VPP IP table；
// BVI 网关：在 BD 上创建 BVI 接口、挂入 BD（port type BVI）、配地址并置于 VRF table。

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// RouteEntry 一条 FIB 路由（运行态）。
//
// NextHop：**全部**下一跳以逗号串呈现（ECMP；与配置/`display set` 同形，单跳逐字不变；决策 #393）。
// Distance：v1 不下发也不回读（VPP ip_route_add_del 无该参数），恒 0；读视图据此如实标注
// 「未下发」而非打印假值（决策 #393；下发能力不在本条）。
type RouteEntry struct {
	Prefix   string `json:"prefix"`
	NextHop  string `json:"next_hop"`
	Distance int    `json:"distance,omitempty"`
}

// L3Client VPP L3 binary API 的最小能力集（govpp 适配/单测假实现）。
type L3Client interface {
	SwInterfaceIndex(ifname string) (uint32, bool, error)
	// SwInterfaceTable 查接口**运行态**当前所属表（sw_interface_get_table）。
	// ok=false 表示客户端答不出来（测试假实现），调用方据此退回「直接置表」。
	SwInterfaceTable(swIfIndex uint32, isIP6 bool) (uint32, bool, error)
	CreateSubif(req CreateSubifReq) (uint32, error)
	IPTableAddDel(tableID uint32, isIPv6, add bool, name string) error
	// IPTableExists 读回该协议下 tableID 是否存在（ip_table_dump）。删表校验的唯一判据：
	// ip_table_add_del(del) 在表仍被接口占用时**返回 0 却不删**，返回码不足以判断是否真删掉了。
	IPTableExists(tableID uint32, isIP6 bool) (bool, error)
	// IPTables 列出 VPP 里现有的 IP 表（v4/v6 合并去重，升序）：供「配置未声明的表」
	// 对账（残留发现，决策 #192）。查询失败必须上抛，不得按空集处理。
	IPTables() ([]uint32, error)
	SwInterfaceSetTable(swIfIndex uint32, isIPv6 bool, tableID uint32) error
	SwInterfaceAddDelAddress(swIfIndex uint32, prefix string, add, delAll bool) error
	IPRouteAddDel(tableID uint32, prefix, nextHop string, add bool) error
	Routes(tableID uint32, isIP6 bool) ([]RouteEntry, error)
	BviCreate() (uint32, error)
	BviOfBD(bdID uint32) (uint32, bool, error)
	SetState(swIfIndex uint32, up bool) error
	BviDelete(swIfIndex uint32) error
	BviSetBD(swIfIndex, bdID uint32) error
	Close()
}

// TableID 由 VRF 名确定性派生 IP table ID（0 为默认表，故最小取 1）。
func TableID(name string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	id := h.Sum32() & 0xFFFFFF
	if id == 0 {
		id = 1
	}
	return id
}

// GatewayVRFName L2 交换机 BVI 网关缺省专属 VRF 名。
func GatewayVRFName(vsName string) string { return "vr-" + vsName }

// vnfAttach 一条 VNF vNIC 的置表登记（表 + sw_if_index）。
//
// vNIC 置入 L3 交换机后与 L3 接口同属该 VRF 的转发域，必须一并登记：NAT inside 解析走
// AttachedIfaces，不登记则 vNIC 拿不到 nat44-ei-in2out 特性——其流量不被转换，而配置、
// show nat 全都正常（round84 实测：guest 100% Destination Host Unreachable）。
type vnfAttach struct {
	tableID uint32
	idx     uint32
}

// L3Provider 实现 VRF/L3 接口/静态路由与 BVI 网关编排。
type L3Provider struct {
	client func() (L3Client, error)

	mu         sync.Mutex
	ifaces     map[uint32][]uint32  // tableID → 配置过的 sw_if_index（删除时清地址）
	subifs     map[uint32][]uint32  // tableID → 自建子接口（删除时一并移除）
	vnfs       map[string]vnfAttach // VNF vNIC 接口名 → 置表登记（NAT inside 解析、删除时摘除）
	bvis       map[string]uint32    // 交换机名 → BVI sw_if_index
	ownTable   map[string]bool      // 交换机名 → BVI 使用专属表（删除时删表）
	ifaceTable map[string]uint32    // 接口名 → 所属 VRF tableID（NAT outside 转发域解析，决策 #52）
	// ifaceIdx 接口名（配置名）→ 最近一次解析/创建的 sw_if_index。三处用它：登记重建按名取
	// 权威索引（vlan 子接口由 create_subif 返回，按名重查可能查到父口）、ForgetVnfIface/
	// DeleteVRF 的摘除口径与 ifaces/ifaceTable 对齐。
	ifaceIdx map[string]uint32
	// idxSwitch sw_if_index → 作为该口「转发域」的交换机名（决策 #345）：BVI（L2）、L3 接口、
	// 置入该 L3 交换机表的 VNF vNIC 三者都登记。DNS 代理按上行 desc.sw_if_index 反查所属交换机、
	// 取按域上游（未配则回落全局）；索引在 VPP 重启后会变，故随重置/重放一并更新。
	idxSwitch map[uint32]string
	// pendingDeletes 删除未收敛的 L3 交换机（tableID → VRF 名）：删表读回报「表仍存在」，
	// 该表只能在数据面重启后消失。留档供后续复核（恢复收敛/周期巡检）：表真没了 → 清登记并消警；
	// 配置又把它声明回来（合法存在）同样清登记。见 RetryPendingDeletes。
	pendingDeletes map[uint32]string
	acl            *AclProvider // 可选：L3 接口/BVI 的 acl-in 绑定
}

// SetACL 注入 ACL 编排（L3 接口与 BVI 网关的 acl-in 绑定）。
func (p *L3Provider) SetACL(a *AclProvider) { p.acl = a }

// reset 清空进程内登记表（恢复收敛前调用，避免陈旧 sw_if_index/表归属把重放带偏）。
//
// pendingDeletes **不**随 reset 清空：待清理项是「数据面已确实存在、配置里已没有」的残留，
// 与按配置重放无关；清掉它就再也没人复核该表的去留（正是 round86 R86-9「残渣事后不可见」）。
func (p *L3Provider) reset() {
	p.mu.Lock()
	p.ifaces = map[uint32][]uint32{}
	p.subifs = map[uint32][]uint32{}
	p.vnfs = map[string]vnfAttach{}
	p.bvis = map[string]uint32{}
	p.ownTable = map[string]bool{}
	p.ifaceTable = map[string]uint32{}
	p.ifaceIdx = map[string]uint32{}
	p.idxSwitch = map[uint32]string{}
	p.mu.Unlock()
}

// NewL3Provider 以固定客户端构造（测试）。
func NewL3Provider(c L3Client) *L3Provider {
	return &L3Provider{client: func() (L3Client, error) { return c, nil },
		ifaces: map[uint32][]uint32{}, subifs: map[uint32][]uint32{}, vnfs: map[string]vnfAttach{},
		bvis: map[string]uint32{}, ownTable: map[string]bool{}, ifaceTable: map[string]uint32{},
		ifaceIdx: map[string]uint32{}, idxSwitch: map[uint32]string{}, pendingDeletes: map[uint32]string{}}
}

// NewL3ProviderFunc 以客户端工厂构造（连接可重连）。
func NewL3ProviderFunc(f func() (L3Client, error)) *L3Provider {
	return &L3Provider{client: f, ifaces: map[uint32][]uint32{}, subifs: map[uint32][]uint32{},
		vnfs: map[string]vnfAttach{}, bvis: map[string]uint32{}, ownTable: map[string]bool{},
		ifaceTable: map[string]uint32{}, ifaceIdx: map[string]uint32{}, idxSwitch: map[uint32]string{},
		pendingDeletes: map[uint32]string{}}
}

// ApplyVRF 把 VRF（L3 交换机）收敛到 VPP：建 v4/v6 table、配置 L3 接口地址、
// 下发静态路由（含默认路由 0.0.0.0/0）。
func (p *L3Provider) ApplyVRF(ctx context.Context, vrf model.Vrf) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	tableID := TableID(vrf.Name)
	if err := p.ensureTables(c, tableID, vrf.Name); err != nil {
		return err
	}

	var idxs, subs []uint32
	idxOf := make(map[string]uint32, len(vrf.L3Interfaces))
	for _, li := range vrf.L3Interfaces {
		idx, sub, err := p.resolveL3Iface(c, li)
		if err != nil {
			return err
		}
		// 先清旧地址再置表：VPP 拒绝把仍带地址的接口移到其它 VRF（-114）
		if err := c.SwInterfaceAddDelAddress(idx, "", false, true); err != nil {
			return fmt.Errorf("清理接口 %s 旧地址: %w", li.Interface, err)
		}
		if err := c.SwInterfaceSetTable(idx, false, tableID); err != nil {
			return fmt.Errorf("接口 %s 置入 VRF %s: %w", li.Interface, vrf.Name, err)
		}
		if err := c.SwInterfaceSetTable(idx, true, tableID); err != nil {
			return fmt.Errorf("接口 %s 置入 VRF %s(IPv6): %w", li.Interface, vrf.Name, err)
		}
		for _, addr := range li.Addresses {
			if err := c.SwInterfaceAddDelAddress(idx, addr, true, false); err != nil {
				return fmt.Errorf("接口 %s 配地址 %s: %w", li.Interface, addr, err)
			}
		}
		idxs = append(idxs, idx)
		if sub != 0 {
			subs = append(subs, sub)
		}
		idxOf[li.Interface] = idx
	}

	// 静态路由：**只下发声明里的**（撤销由提交编排按旧/新声明求差集另行下发，
	// 见 orchestrator/apply.go 的 del-route 计划操作与 DeleteRoute）。
	for _, r := range vrf.Routes {
		if err := c.IPRouteAddDel(tableID, r.Prefix, r.NextHop, true); err != nil {
			return fmt.Errorf("下发路由 %s via %s: %w", r.Prefix, r.NextHop, err)
		}
	}

	if p.acl != nil {
		c2, err := p.acl.client()
		if err != nil {
			return err
		}
		defer c2.Close()
		for i, li := range vrf.L3Interfaces {
			if li.AclIn == "" {
				continue
			}
			if err := p.acl.BindIndex(c2, idxs[i], li.AclIn, ""); err != nil {
				return err
			}
		}
	}

	p.mu.Lock()
	// 移入本表的接口必须从**其它**表的登记里摘掉：登记是「转发域成员」的唯一来源，
	// 接口换表（改归属到另一台交换机）后旧表仍留着它，会让旧表被删时的解绑把它踢回默认表
	// ——新配置刚把它置入新表，转眼又被旧表的清理撤销（数据面与配置不一致，且全程无报错）。
	p.reassignLocked(tableID, idxs)
	p.ifaces[tableID], p.subifs[tableID] = idxs, subs
	// 决策 #345：登记「该口属于哪台交换机」（DNS 代理按上行 sw_if_index 反查按域上游）。
	for _, idx := range idxs {
		p.idxSwitch[idx] = vrf.Name
	}
	for name, idx := range idxOf {
		p.ifaceTable[name] = tableID
		p.ifaceIdx[name] = idx
	}
	// 已置入本表的 vNIC 同属该转发域：一并计入 ifaces，使 ifaces 自身即「完整转发域」
	// （NAT inside 的另一来源是 vnfs；两处一致才不会出现「登记在、集合里没有」的错觉）。
	// 只并集写入、不替换，故本方法重放不会丢掉 vNIC（附录 A #35：只补齐不摘除）。
	for _, a := range p.vnfs {
		if a.tableID == tableID && !containsIdx(p.ifaces[tableID], a.idx) {
			p.ifaces[tableID] = append(p.ifaces[tableID], a.idx)
		}
	}
	p.mu.Unlock()
	return nil
}

// containsIdx 判断索引是否已在集合里（登记表并集写入用）。
func containsIdx(idxs []uint32, idx uint32) bool {
	for _, v := range idxs {
		if v == idx {
			return true
		}
	}
	return false
}

// reassignLocked 把 idxs 从**除 keep 之外**所有表的成员登记里摘掉（调用方须持 p.mu）。
//
// 一个接口在同一时刻只能属于一张表（VPP 的 sw_interface_set_table 就是这么做的），
// 而登记是删除路径（DeleteVRF 清地址/解绑）与 NAT inside 解析的唯一依据：不摘旧登记，
// 接口换表之后旧表被删时会被"顺手"解绑回默认表——新配置的归属被无声撤销。
func (p *L3Provider) reassignLocked(keep uint32, idxs []uint32) {
	moved := make(map[uint32]bool, len(idxs))
	for _, idx := range idxs {
		moved[idx] = true
	}
	prune := func(m map[uint32][]uint32) {
		for t, list := range m {
			if t == keep || len(list) == 0 {
				continue
			}
			out := list[:0]
			for _, idx := range list {
				if !moved[idx] {
					out = append(out, idx)
				}
			}
			if len(out) == 0 {
				delete(m, t)
				continue
			}
			m[t] = out
		}
	}
	prune(p.ifaces)
	prune(p.subifs)
}

// AttachFailure 一条「按配置重建登记」的失败项：Source 与恢复收敛其它步骤同风格
// （如 vrfs/<交换机>/<接口>），Err 说明该接口为何进不了转发域。调用方须使其可见
// （进未收敛清单/告警），不得静默丢弃——登记缺项会让 NAT 下发答非所问。
type AttachFailure struct {
	Source string
	Err    error
}

// RegisterL3Interfaces 按配置登记一台 L3 交换机（Vrf）的三层接口，供恢复收敛在 VRF 落地之后、
// **ApplyNAT 之前**重建 L3 侧登记（幂等；只补齐不摘除）。
//
// 为什么必须由配置重建：恢复收敛开头会失效全部进程内登记，而 NAT 的 inside/outside 解析只读
// L3 侧这三张表——登记缺项时 NAT 会认为「该口不该有 inside/outside 特性」而下发删除，
// 登记整体为空时更会把插件当作「没有 NAT 配置」关掉（真机实测：`systemctl restart vpp`
// 加 `systemctl restart nfvis` 后 `show nat44 ei interfaces`/`addresses` 全空，而配置完好、
// 日志无未收敛项）。登记因此不能依赖「某次增量调用是否成功」。
//
// 写入的三处登记：
//   - ifaceIdx[接口名] → sw_if_index（登记重建的输入，也供 ForgetVnfIface/DeleteVRF 摘除）；
//   - ifaces[tableID] → 该 VRF 转发域成员索引（NAT inside 经 AttachedIfaces 解析）；
//   - ifaceTable[接口名] → 接口所属表（NAT outside 转发域经 TableOfIface 解析，决策 #52）。
//
// 索引来源按可信度排序：先按名问运行态（无歧义时这就是唯一真源），运行态答不出再回退到
// 本进程 ApplyVRF 刚解析/创建出的索引（vlan 子接口的权威值由 create_subif 返回，
// 且 VPP 侧子接口名可能与推导不同）。两处都拿不到 = 该口尚未进入数据面，按未收敛上报
// （ErrIfaceUnavailable，恢复收敛据此转 error 级告警），不静默丢。
func (p *L3Provider) RegisterL3Interfaces(ctx context.Context, vrf model.Vrf) []AttachFailure {
	c, err := p.client()
	if err != nil {
		return []AttachFailure{{Source: "vrfs/" + vrf.Name, Err: err}}
	}
	defer c.Close()
	tableID := TableID(vrf.Name)
	var fails []AttachFailure
	for _, li := range vrf.L3Interfaces {
		source := "vrfs/" + vrf.Name + "/" + li.Interface
		idx, ok, err := p.resolveRegistered(c, li)
		if err != nil {
			fails = append(fails, AttachFailure{Source: source,
				Err: fmt.Errorf("解析接口 %s 的 sw_if_index: %w", li.Interface, err)})
			continue
		}
		if !ok {
			fails = append(fails, AttachFailure{Source: source,
				Err: fmt.Errorf("%w: %s"+ifaceMissingHint, ErrIfaceUnavailable, li.Interface)})
			continue
		}
		p.mu.Lock()
		p.reassignLocked(tableID, []uint32{idx})
		p.ifaceTable[li.Interface] = tableID
		p.ifaceIdx[li.Interface] = idx
		p.idxSwitch[idx] = vrf.Name // 决策 #345：L3 接口是该交换机的转发域
		if !containsIdx(p.ifaces[tableID], idx) {
			p.ifaces[tableID] = append(p.ifaces[tableID], idx)
		}
		p.mu.Unlock()
	}
	return fails
}

// resolveRegistered 求一个 L3 接口当前的 sw_if_index：**先按名问运行态**（名字无歧义时运行态
// 就是唯一真源，也避免用到陈旧索引），运行态答不出（如 vlan 子接口在 VPP 侧的名字与推导不同、
// 或查询失败）再回退到本进程 ApplyVRF 刚解析/创建出的索引——create_subif 的返回值即权威值。
func (p *L3Provider) resolveRegistered(c L3Client, li model.L3Interface) (uint32, bool, error) {
	// 按名查：vlan 子接口在 VPP 侧的名字由 VPP 生成（<父口>.<sub_id>），配置里的写法可能是
	// 子接口名、也可能是父口名 + vlan。**绝不退回父口名**——把父口登记成该 VRF 的三层接口
	// （NAT inside 因而指错接口）比查不到更糟：查不到会上报未收敛，指错口则全程无报错。
	name := li.Interface
	if li.Vlan > 0 {
		name = vlanSubifName(li)
	}
	idx, ok, err := c.SwInterfaceIndex(name)
	if ok {
		return idx, true, nil
	}
	p.mu.Lock()
	idx, ok = p.ifaceIdx[li.Interface]
	p.mu.Unlock()
	if ok {
		return idx, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	return 0, false, nil
}

// vlanSubifName vlan 子接口在 VPP 中的确定性名：create_subif 不带 name 时由 VPP 按
// <父口>.<sub_id> 命名；父口名取配置名去掉已带的 .<vlan> 后缀，两种写法归一。
func vlanSubifName(li model.L3Interface) string {
	base := li.Interface
	if i := strings.Index(base, "."); i > 0 {
		base = base[:i]
	}
	return fmt.Sprintf("%s.%d", base, li.Vlan)
}

// ApplyRoute 下发一条静态路由（幂等）：供提交编排在撤销路由删除失败时补偿，
// 也供将来按单条路由的增量下发使用。
func (p *L3Provider) ApplyRoute(ctx context.Context, vrfName string, r model.Route) error {
	return p.routeAddDel(ctx, vrfName, r, true)
}

// DeleteRoute 撤销一条静态路由（**声明里已不再有的路由必须从 FIB 撤走**）。
//
// 为什么不能让 ApplyVRF/DeleteVRF 代劳：ApplyVRF 只下发声明里的路由（只加不撤），而
// DeleteVRF 只在整台 L3 交换机被删时才调用——「同名 VRF 仍在声明里、只去掉一条路由」
// 走不到它，缺口下路由会长期留在 FIB 里，而配置与 show 输出全绿（真机实测）。
// 表不存在时本条为「已达成」：路由本就无处可留。
func (p *L3Provider) DeleteRoute(ctx context.Context, vrfName string, r model.Route) error {
	return p.routeAddDel(ctx, vrfName, r, false)
}

// routeAddDel 按 VRF 名派生表后下发/撤销一条路由。不在此处建表：建表是 ApplyVRF 的
// 职责（撤销路径上表可能已被删，那时路由已不存在，属已达成而非失败）。
func (p *L3Provider) routeAddDel(ctx context.Context, vrfName string, r model.Route, add bool) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	tableID := TableID(vrfName)
	if err := c.IPRouteAddDel(tableID, r.Prefix, r.NextHop, add); err != nil {
		verb := "撤销"
		if add {
			verb = "下发"
		}
		return fmt.Errorf("%s路由 %s via %s（VRF %s）: %w", verb, r.Prefix, r.NextHop, vrfName, err)
	}
	return nil
}

// UnbindL3IfaceACL 撤销一个 L3 接口的 acl-in 绑定与伴随 macip 白名单（决策 #361）。
//
// 为什么必须有这条单接口撤销：`AclProvider.BindIndex` 的「空绑定＝解绑」分支此前**没有任何
// 生产调用者**——ApplyVRF 只对仍声明 AclIn 的接口下发绑定、对「新声明里不再有绑定」不复核，
// 于是 `delete … l3-interface <if> acl-in <acl>`（只清绑定）提交成功、读视图干净，而 VPP 的
// `show acl-plugin interface` 里绑定仍在、deny 规则继续拦（round142 真机实测）。
//
// 索引解析与恢复收敛同源（resolveRegistered）：先按名问运行态、再回退进程内登记。
// **查不到且登记也没有**＝接口不在数据面，绑定随接口消失，属已达成（返回 nil）；
// 查询本身失败照实上抛（不许把「问不出来」当成「已达成」）。
func (p *L3Provider) UnbindL3IfaceACL(ctx context.Context, vrfName string, li model.L3Interface) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	idx, ok, err := p.resolveRegistered(c, li)
	if err != nil {
		return fmt.Errorf("解析 VRF %s 的接口 %s（ACL 解绑）: %w", vrfName, li.Interface, err)
	}
	if !ok {
		return nil
	}
	return p.unbindTableACLs([]uint32{idx})
}

// DeleteL3Interface 回收一个已从声明里删除的 L3 接口（决策 #361），顺序固定：
//  1. 解析索引（与恢复收敛同源；查不到且无登记＝接口不在数据面，属已达成，只清登记后返回）；
//  2. 清地址（delAll，-2 容错——接口可能已随其属主 VM/vNIC 删除而消失）；
//  3. 解绑 IP ACL 与伴随 macip（BindIndex 的空绑定语义，登记门控幂等）；
//  4. 移回默认表（v4/v6，-2 容错）——该口不再属于本 VRF 的转发域；
//     **例外**：该口仍由本 VRF 的 VNF vNIC 声明留表时保持表归属（见 vnicHoldsTable）；
//  5. 摘登记（只摘由本条 l3-interface 贡献的项，见 forgetL3Iface）。
//
// 为什么必须显式回收：声明里删掉 l3-interface 后它就不再进入 ApplyVRF 的处理范围，而
// DeleteVRF 只在整台交换机被删时才跑——此前没有任何路径把口从原表摘出来、清地址、解 ACL，
// 于是口留在原表（继续占着表、旧表删不掉）、地址与 deny 绑定原样生效（round142 §1-1b 实测）。
func (p *L3Provider) DeleteL3Interface(ctx context.Context, vrfName string, li model.L3Interface) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	idx, ok, err := p.resolveRegistered(c, li)
	if err != nil {
		return fmt.Errorf("解析 VRF %s 的接口 %s（回收）: %w", vrfName, li.Interface, err)
	}
	if !ok {
		// 接口与登记都不在：可回收的数据面对象不存在（地址/绑定随接口一起消失），按已达成。
		p.forgetL3Iface(vrfName, li, 0)
		return nil
	}
	if err := c.SwInterfaceAddDelAddress(idx, "", false, true); err != nil && !isMissingIfaceErr(err) {
		return fmt.Errorf("清理接口 %s 地址: %w", li.Interface, err)
	}
	if err := p.unbindTableACLs([]uint32{idx}); err != nil {
		return err
	}
	// 4) 移回默认表（v4/v6，-2 容错）——该口不再属于本 VRF 的转发域。
	// 例外（共存边界，决策 #361）：同一个口同时是本 VRF 的 **VNF vNIC**（vNIC 作 L3 接口，
	// #172 的受支持形态，用户手册 §8.9）时**保持表归属**，只撤地址与绑定——vNIC 侧声明仍在，
	// 把它移回默认表会让 guest 的转发域在下次重放前静默丢失（登记侧由 forgetL3Iface 的
	// 同类守卫保持，两处口径一致）。
	if !p.vnicHoldsTable(vrfName, li.Interface) {
		for _, ip6 := range []bool{false, true} {
			if err := c.SwInterfaceSetTable(idx, ip6, 0); err != nil && !isMissingIfaceErr(err) {
				return fmt.Errorf("把接口 %s 移回默认表（%s）: %w", li.Interface, ipVerName(ip6), err)
			}
		}
	}
	p.forgetL3Iface(vrfName, li, idx)
	return nil
}

// vnicHoldsTable 该接口是否仍由本 VRF 的 VNF vNIC 声明留在该表里（决策 #361 的共存边界判据）：
// 是则删 l3-interface 叶子只撤地址/绑定，不动表归属。
func (p *L3Provider) vnicHoldsTable(vrfName, ifname string) bool {
	tableID := TableID(vrfName)
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.vnfs[ifname]
	return ok && a.tableID == tableID
}

// forgetL3Iface 摘除一条 l3-interface 的进程内登记（决策 #361 的单接口回收路径；调用方无须持锁）。
//
// 只摘**由这条声明贡献**的项，不误伤别的来源（决策 #361 的边界口径）：
//   - ifaces/subifs[tableID]：该 sw_if_index 在本表下的成员登记（DeleteVRF 清地址/解绑的来源）；
//   - ifaceTable/ifaceIdx：指向本 VRF 的该接口名（NAT outside 转发域解析的来源）；
//   - idxSwitch：仅当登记指向本交换机——同一个 sw_if_index 可能是别的对象的转发域（预留判据）。
//
// 例外：该接口名同时是本表的 **VNF vNIC**（VNF 侧声明仍在，其转发域身份由 vNIC 侧承担）时
// 整套登记保持——这些登记同时由 SetVnfTable 写入、由 ForgetVnfIface 维护，此处摘掉会让
// DeleteVRF 漏清该口的地址/归属、NAT 解析答不出。idx==0（接口与登记都不在）时只清按名登记。
//
// 按名登记（ifaceTable/ifaceIdx）另须**核对索引**（决策 #393）：同 VRF 内 vlan 变更
// （旧 100→新 200）时新旧声明同名（li.Interface 都是父口名）而索引不同，按名无条件删会把
// **新**子接口的登记一并摘掉，致 TableOfIface 落空、NAT outside 解析缺项（nat.go 落回表 0）。
// 只摘「登记索引 == 本次摘除索引」的项。
func (p *L3Provider) forgetL3Iface(vrfName string, li model.L3Interface, idx uint32) {
	tableID := TableID(vrfName)
	p.mu.Lock()
	defer p.mu.Unlock()
	if a, ok := p.vnfs[li.Interface]; ok && a.tableID == tableID {
		return // 本表的 vNIC 仍持有这些登记
	}
	dropIdx := func(m map[uint32][]uint32) {
		list := m[tableID]
		if len(list) == 0 {
			return
		}
		out := list[:0]
		for _, v := range list {
			if v != idx {
				out = append(out, v)
			}
		}
		if len(out) == 0 {
			delete(m, tableID)
			return
		}
		m[tableID] = out
	}
	if idx != 0 {
		dropIdx(p.ifaces)
		dropIdx(p.subifs)
	}
	if t, ok := p.ifaceTable[li.Interface]; ok && t == tableID {
		// 只摘「本条声明贡献的项」：登记索引须与本次摘除的索引一致（决策 #393）。
		// idx==0（接口与登记都不在）时无索引可核，仍按名清（此时不存在同名的新登记）。
		if idx == 0 || p.ifaceIdx[li.Interface] == idx {
			delete(p.ifaceTable, li.Interface)
			delete(p.ifaceIdx, li.Interface)
		}
	}
	if idx != 0 {
		if name, ok := p.idxSwitch[idx]; ok && name == vrfName {
			delete(p.idxSwitch, idx)
		}
	}
}

// unbindTableACLs 批量解绑一组 sw_if_index 上的 IP ACL 与伴随 macip（决策 #361 的共用路径：
// 单接口回收与删整台交换机）。去重、幂等（BindIndex 的「空绑定」只在有登记时才动 VPP）；
// p.acl 未注入（无 VPP 路径/单测）时空操作——与 ApplyVRF 绑定侧的 `p.acl == nil` 跳过对称。
func (p *L3Provider) unbindTableACLs(idxs []uint32) error {
	if p.acl == nil || len(idxs) == 0 {
		return nil
	}
	ac, err := p.acl.client()
	if err != nil {
		return err
	}
	defer ac.Close()
	seen := make(map[uint32]bool, len(idxs))
	for _, idx := range idxs {
		if seen[idx] {
			continue
		}
		seen[idx] = true
		if err := p.acl.BindIndex(ac, idx, "", ""); err != nil {
			return err
		}
	}
	return nil
}

// TableOfIface 返回接口所属 VRF 的 tableID（未归属任何 VRF 时 ok=false）。
// 供 NAT44 outside 转发域解析（决策 #52）。
func (p *L3Provider) TableOfIface(ifname string) (uint32, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.ifaceTable[ifname]
	return t, ok
}

// ForwardDomainOf 返回该 sw_if_index 作为「转发域」所属的交换机名（决策 #345）。
//
// L2 交换机的转发域是 BVI；L3 交换机是其 l3-interface（含 vlan 子接口）与置入其表的 VNF vNIC。
// 供数据面 DNS 代理按上行 desc.sw_if_index 反查交换机、取按域上游（未配则回落全局）。
// 未登记（不属于任何已收敛交换机的转发域）时 ok=false——调用方据此按「无按域上游」处理。
func (p *L3Provider) ForwardDomainOf(idx uint32) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	name, ok := p.idxSwitch[idx]
	return name, ok
}

// ErrVrfNotRemoved 删表后读回发现表仍在 VPP 里：按「未收敛」上报，调用方据此进未收敛清单/告警。
//
// 触发条件是 VPP 在表仍被占用时对 ip_table_add_del(del) **返回 0 却不真删**
// （`show ip table` 里该项继续带 locks:[…]，直到 VPP 重启才消失）。占用者有两类：
// 接口（本实现已在删表前解绑）与**数据面插件的 IP_TABLE_LOCK**（见 vrfDeleteStuckHint）。
//
// 与 orchestrator.ErrVrfNotRemoved **是同一个 error 值**（决策 #192）：提交编排要识别该状态
// 以决定「删表延后而非整体回滚」，而依赖方向不允许那里 import 本包；本包保留该名字，
// 使既有调用方（含测试与 API 层）的 errors.Is 判定不变。
var ErrVrfNotRemoved = orchestrator.ErrVrfNotRemoved

// vrfDeleteStuckHint 读回失败时的可照做提示（决策 #187，round87 按新口径改文案）。
//
// 真机实测（round86，可稳定复现）：**NAT44 一旦把某张表当作 inside/outside，就会在该表上留一个
// IP_TABLE_LOCK**（`vppctl show ip fib summary` 里可见 `locks:[nat44-ei-hi:1]`）。此后删 NAT 规则、
// 关插件（nat44_ei_plugin_enable_disable(false)）、乃至**手工 vppctl 再关一次**，该锁都**不释放**——
// 只有重启 VPP 才回到干净状态（重启按 committed 配置重建，代价是一次数据面中断）。
//
// 自决策 #192 起，提交编排把这一情形当作**非致命**：配置侧照常删除，残渣进告警，
// 数据面清理由重启后的恢复收敛完成（本包的 pendingDeletes 兜底）。本提示因此不再说
// 「重试删除」（表已在配置里删掉了，没有什么可重试的），只说清释放条件与清理时机。
const vrfDeleteStuckHint = "（表可能仍被数据面插件引用：NAT44 用过这张表后 VPP 不释放该引用，" +
	"执行 request vpp restart 后自动清理）"

// DeleteVRF 删除 VRF：清接口地址 → 解绑接口上的 IP ACL/伴随 macip → 解绑接口回默认表 →
// 删 v4/v6 table（路由随表删除）→ 读回核对。
//
// 解绑与删后读回都是 R84-29 补上的必需步骤：VPP 的 ip_table_add_del(del) 在表仍被接口
// 占用时**返回 0 但实际不删**（表项继续留在 `show ip table` 里带 locks:[interface:…]，
// 界面与配置侧全干净），只信返回码就是「报成功却没做到」——空表滞留还会与「残留声明重建
// 的表」混在一起误判。故：
//  1. 先把该表里的接口逐一改回默认表（v4/v6 都要），失败即中止且**保留**登记，重试可重来；
//  2. 删表后 dump 读回，确认表确实不存在，否则按未收敛报错，不静默成功。
//
// 登记清理（ifaces/subifs/ifaceTable/ifaceIdx/vnfs）落在解绑之后：解绑要按 ifaces 取索引清单，
// 提前清掉就再也不知道哪些口还绑着该表。
func (p *L3Provider) DeleteVRF(ctx context.Context, name string) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	tableID := TableID(name)
	p.mu.Lock()
	idxs, subs := p.ifaces[tableID], p.subifs[tableID]
	p.mu.Unlock()
	// 先清地址：VPP 拒绝把仍带地址的接口移到其它表（-114），与 ApplyVRF 同一前置。
	// 接口已被删（如「同一提交里既删 VNF 又删引用其 vNIC 的 L3 交换机」，见 R88-6）时
	// VPP 返回 Invalid sw_if_index(-2)：地址随接口一起消失，属**已达成**，不能因此
	// 打断整次删除（否则用户连一次清干净都做不到）。提交编排的删除顺序已按依赖倒序修正，
	// 这里是第二道保险。
	for _, idx := range idxs {
		if err := c.SwInterfaceAddDelAddress(idx, "", false, true); err != nil {
			if isMissingIfaceErr(err) {
				continue
			}
			return fmt.Errorf("清理接口 %d 地址: %w", idx, err)
		}
	}
	for _, sub := range subs {
		if err := c.SwInterfaceAddDelAddress(sub, "", false, true); err != nil {
			if isMissingIfaceErr(err) {
				continue
			}
			return fmt.Errorf("清理子接口 %d 地址: %w", sub, err)
		}
	}
	// 决策 #361：这些口上的 IP ACL 与伴随 macip 也要解绑——此前只清地址/表，绑定会留在
	// 已被移回默认表的接口上（`show acl-plugin interface` 里 deny 继续生效），直到 VPP 重启。
	// ACL 客户端与 L3 客户端是两类接口，此处各取一个（与 ApplyVRF 绑定侧同一写法）。
	if err := p.unbindTableACLs(append(append([]uint32{}, idxs...), subs...)); err != nil {
		return err
	}
	// 先解绑、再删表：接口还绑着该表时删表不生效（VPP 返回 0 却保住表）。子接口通常在
	// ifaces 里已有一份，去重由 unbindTableIfaces 负责。
	if err := p.unbindTableIfaces(c, append(append([]uint32{}, idxs...), subs...)); err != nil {
		return err
	}
	p.forgetTable(tableID)
	for _, ip6 := range []bool{false, true} {
		if err := c.IPTableAddDel(tableID, ip6, false, name); err != nil {
			return fmt.Errorf("删 IP table %d: %w", tableID, err)
		}
	}
	// 删后校验（返回 0 不等于删掉了）：读回确认，仍在则按未收敛上报。
	for _, ip6 := range []bool{false, true} {
		exists, err := c.IPTableExists(tableID, ip6)
		if err != nil {
			return fmt.Errorf("核对 IP table %d（%s）是否已删除: %w", tableID, ipVerName(ip6), err)
		}
		if exists {
			// 留档：该表只能等数据面重启才消失，后续复核（恢复收敛/周期巡检）负责清登记与消警。
			p.markPendingDelete(tableID, name)
			return fmt.Errorf("%w: IP table %d（%s）在删除后仍存在于 VPP，%s 的数据面未收敛%s",
				ErrVrfNotRemoved, tableID, ipVerName(ip6), name, vrfDeleteStuckHint)
		}
	}
	return nil
}

// markPendingDelete 登记一台「删表未收敛」的交换机（幂等）。
func (p *L3Provider) markPendingDelete(tableID uint32, name string) {
	p.mu.Lock()
	if p.pendingDeletes == nil {
		p.pendingDeletes = map[uint32]string{}
	}
	p.pendingDeletes[tableID] = name
	p.mu.Unlock()
}

// RetryPendingDeletes 复核「删表未收敛」的交换机，返回**已确认干净**（可消警）的交换机名。
//
// declared 报告某个 VRF 名是否仍被 committed 配置声明：声明回来了说明那张表是合法存在
// （例如操作者又把交换机加了回来），残渣顾虑随之消失，直接清除登记。
//
// 未声明的一般情形按**复核**处理：表已不在 VPP（数据面重启后的正常结果）→ 清除登记；
// 表仍在 → 再试一次删除（删不掉的仍是同一锁，保持登记、不重复告警文案）。
// 本方法不产生告警/日志噪声：真正的可见化由提交期告警（COMMIT_VRF_DELETE_DEFERRED）
// 与恢复收敛的未收敛清单承担。
func (p *L3Provider) RetryPendingDeletes(ctx context.Context, declared func(string) bool) []string {
	p.mu.Lock()
	pending := make(map[uint32]string, len(p.pendingDeletes))
	for t, n := range p.pendingDeletes {
		pending[t] = n
	}
	p.mu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	c, err := p.client()
	if err != nil {
		return nil
	}
	defer c.Close()
	var cleared []string
	for tableID, name := range pending {
		if declared != nil && declared(name) {
			p.clearPending(tableID)
			cleared = append(cleared, name)
			continue
		}
		// 表已不在 VPP（数据面重启后的正常结果）即视为干净；仍在则再试一次删除——
		// 删表前该表已由 DeleteVRF 解绑清空，这里只需「删 + 读回」。
		if !p.tableExists(c, tableID) {
			p.clearPending(tableID)
			cleared = append(cleared, name)
			continue
		}
		for _, ip6 := range []bool{false, true} {
			_ = c.IPTableAddDel(tableID, ip6, false, name)
		}
		if !p.tableExists(c, tableID) {
			p.clearPending(tableID)
			cleared = append(cleared, name)
		}
	}
	return cleared
}

// tableExists 读回 v4/v6 两张表是否还有一张存在；读不到按「存在」处理（不清登记、不消警）。
func (p *L3Provider) tableExists(c L3Client, tableID uint32) bool {
	for _, ip6 := range []bool{false, true} {
		exists, err := c.IPTableExists(tableID, ip6)
		if err != nil || exists {
			return true
		}
	}
	return false
}

// clearPending 摘除一台交换机的待清理登记。
func (p *L3Provider) clearPending(tableID uint32) {
	p.mu.Lock()
	delete(p.pendingDeletes, tableID)
	p.mu.Unlock()
}

// DeclaredTables 返回配置声明的 IP 表 ID → 名（默认表 0 亦在内，名为空）。
//
// 声明的表 = 默认表 0 ∪ 各 Vrf 条目（L3 交换机与显式网关 VRF）∪ 自建网关 VRF（vr-<交换机>）。
// 口径**唯一**：残留表对账（LeftoverTables）与恢复收敛的表预建（PrecreateTables）共用本函数，
// 避免「谁算声明表」出现第二份实现后各说各话。
func DeclaredTables(cfg model.Config) map[uint32]string {
	declared := map[uint32]string{0: ""}
	for _, v := range cfg.Vrfs {
		declared[TableID(v.Name)] = v.Name
	}
	for _, vs := range cfg.VirtualSwitches {
		if vs.Gateway == nil {
			continue
		}
		name := vs.Gateway.Vrf
		if name == "" {
			name = GatewayVRFName(vs.Name)
		}
		declared[TableID(name)] = name
	}
	return declared
}

// PrecreateTables 预建配置声明的 IP 表（默认表 0 跳过），按表 ID 升序逐一幂等下发。
//
// 恢复收敛在重放 vNIC 接入/交换机**之前**调用：vNIC 若声明在某台 L3 交换机下（FR-NET-020），
// 置表要求该表已存在，否则 VPP 报 `No such FIB / VRF (-3)` 成为未收敛项、要再重放一次才成功
// （round84 登记 R84-22）。这里是「先建 VRF/L3 表与交换机，再置接口/成员」这条依赖顺序的
// 显式落点——复用各 Provider 既有的幂等建表（ensureTables），不新造排序器。
//
// 单个表建失败不阻塞其余表：逐条上报（调用方记未收敛项），与恢复收敛「单对象失败不阻塞其余」同口径。
func (p *L3Provider) PrecreateTables(ctx context.Context, cfg model.Config) []error {
	c, err := p.client()
	if err != nil {
		return []error{err}
	}
	defer c.Close()
	declared := DeclaredTables(cfg)
	ids := make([]uint32, 0, len(declared))
	for id := range declared {
		if id == 0 {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var errs []error
	for _, id := range ids {
		if err := p.ensureTables(c, id, declared[id]); err != nil {
			errs = append(errs, fmt.Errorf("预建 IP 表 %d(%s): %w", id, declared[id], err))
		}
	}
	return errs
}

// LeftoverTables 返回**配置未声明**却在 VPP 里存在的 IP 表（升序）。
//
// 用途：把「删表延后 / 补偿残渣」留下的空表变成一条可对账的事实——恢复收敛据此记未收敛项与
// 告警（scope=recovery，随 Sync 自动消解），因此**跨 nfvisd 重启仍然可见**：进程内登记会丢，
// 而「配置没声明这张表，它却在 VPP 里」是随时可复查的事实（round86 R86-9 的「事后不可见」
// 正是指这类残渣只能靠进程内记忆）。
//
// 声明的表由 DeclaredTables 给出（与表预建同一份口径）。查询失败上抛（把「问不出来」当
// 「没有残留」是假绿）。
func (p *L3Provider) LeftoverTables(cfg model.Config) ([]uint32, error) {
	c, err := p.client()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	have, err := c.IPTables()
	if err != nil {
		return nil, err
	}
	declared := DeclaredTables(cfg)
	var out []uint32
	for _, id := range have {
		if _, ok := declared[id]; !ok {
			out = append(out, id)
		}
	}
	return out, nil
}

// AllTables 返回 VPP 里当前的 IP 表 ID 集合（含默认表 0），供残渣对账判定「提交期告警对象
// 是否已复原」用（与 LeftoverTables 同一数据来源，避免第二次查询口径分叉）。
func (p *L3Provider) AllTables() (map[uint32]bool, error) {
	c, err := p.client()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	ids, err := c.IPTables()
	if err != nil {
		return nil, err
	}
	out := make(map[uint32]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// unbindTableIfaces 把接口逐一改回默认表（table 0，v4/v6 都要），用于删表前解绑。
// 幂等：已在默认表的口再置一次表是空操作；相同索引只下发一次（子接口可能在两处出现）。
func (p *L3Provider) unbindTableIfaces(c L3Client, idxs []uint32) error {
	seen := make(map[uint32]bool, len(idxs))
	for _, idx := range idxs {
		if seen[idx] {
			continue
		}
		seen[idx] = true
		for _, ip6 := range []bool{false, true} {
			if err := c.SwInterfaceSetTable(idx, ip6, 0); err != nil {
				// 接口已不存在：无需（也无法）置回默认表，视为已达成（R88-6）。
				if isMissingIfaceErr(err) {
					break
				}
				return fmt.Errorf("把接口 %d 移出该 VRF（改回默认表，%s）: %w", idx, ipVerName(ip6), err)
			}
		}
	}
	return nil
}

// forgetTable 摘除该表的进程内登记：ifaces/subifs（转发域成员）、ifaceTable/ifaceIdx（按名归属）、
// vnfs（置入该表的 vNIC）。五处缺一都会留下指向不存在表的死索引，NAT 解析与下一次删除都读它们。
// 重新声明后 ApplyVRF/SetVnfTable/RegisterL3Interfaces 会重建登记。
func (p *L3Provider) forgetTable(tableID uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 先摘 idxSwitch（决策 #345）：该表的口不再作为任何交换机的转发域。索引从删除前的
	// ifaces/subifs/vnfs 一并取出，避免漏摘（与 ifaceTable/vnfs 同一次遍历口径）。
	for _, idx := range p.ifaces[tableID] {
		delete(p.idxSwitch, idx)
	}
	for _, idx := range p.subifs[tableID] {
		delete(p.idxSwitch, idx)
	}
	delete(p.ifaces, tableID)
	delete(p.subifs, tableID)
	for k, t := range p.ifaceTable {
		if t == tableID {
			delete(p.ifaceTable, k)
			delete(p.ifaceIdx, k)
		}
	}
	for k, a := range p.vnfs {
		if a.tableID == tableID {
			delete(p.vnfs, k)
			delete(p.idxSwitch, a.idx) // 决策 #345：转发域归属随之消失
		}
	}
}

// AttachedIfaces 返回 VRF 转发域内已登记的 sw_if_index（供 NAT inside 解析）：
// L3 接口（ApplyVRF / RegisterL3Interfaces 登记）∪ 置入该表的 VNF vNIC（SetVnfTable 登记）。
//
// 两处来源必须合并：vNIC 此前只置表不登记，NAT inside 永远不含它（round84 实测
// `show nat44 ei interfaces` 只有物理口），guest 的流量不会被转换且全程无报错。
// vNIC 若本身就是该 VRF 的 l3-interface（带地址），两侧都会登记同一索引，此处去重。
//
// 登记可被 InvalidateRuntimeState 清空，故它**只在恢复收敛按配置重建之后**才是完整的
// （recovery.go 的登记重建步）；查询方不得把空集当成「配置里没有 inside」。
func (p *L3Provider) AttachedIfaces(vrfName string) []uint32 {
	tableID := TableID(vrfName)
	p.mu.Lock()
	rec := p.ifaces[tableID]
	seen := make(map[uint32]bool, len(rec))
	out := make([]uint32, 0, len(rec))
	for _, idx := range rec {
		if seen[idx] {
			continue
		}
		seen[idx] = true
		out = append(out, idx)
	}
	for _, a := range p.vnfs {
		if a.tableID != tableID || seen[a.idx] {
			continue
		}
		seen[a.idx] = true
		out = append(out, a.idx)
	}
	p.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// SetVnfTable 把 VNF vNIC 接口置入 L3 交换机（VRF）对应表（FR-NET-020 的 L3 场景；
// vNIC 无 IP，仅入表以便经 VRF 转发）。接口按确定性名解析（由 ApplyVnfInterface 先建）。
//
// 三处要点（均为 round84 实测缺陷的修复）：
//   - **建表先于置表**：事务计划与恢复收敛都先重放 vNIC 接入（vhost-user 接口须先建，
//     ApplyVRF 才能按名解析它），此时同名 Vrf 条目可能尚未下发 → 置表报 -3
//     （No such FIB / VRF）并成为未收敛项，要再重放一次才成功。这里幂等补建该表
//     （重复下发由 ip_table_add_del 的 -111 吸收），顺序问题不再存在。
//   - **置表前问运行态**：vNIC 现在可以是该 VRF 的 l3-interface（带地址，guest 的网关必须
//     落在 guest 自己的口上——VPP 不为同 VRF 的另一接口代答 ARP），而 VPP 只允许**无地址**
//     的接口换表（-114）。先查该口当前所属表，已在目标表就不再下发置表，因此「登记失效后
//     按运行态重建」不会被 -114 挡住。
//   - **置表后登记**：vNIC 与 L3 接口同属该 VRF 转发域，须进 AttachedIfaces（NAT inside
//     解析来源），否则其流量拿不到 nat44-ei-in2out 特性。
func (p *L3Provider) SetVnfTable(ctx context.Context, vrfName, ifname string) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	idx, ok, err := c.SwInterfaceIndex(ifname)
	if err != nil {
		return fmt.Errorf("解析 VNF 接口 %s: %w", ifname, err)
	}
	if !ok {
		return fmt.Errorf("%w: VNF 接口 %s 不存在", ErrIfaceUnavailable, ifname)
	}
	tableID := TableID(vrfName)
	// 已登记且索引未变 = 该口就在这张表里：不重复置表、也不重复查运行态。
	// 连接重建/恢复收敛会先失效登记（L2Network.InvalidateRuntimeState），故此处跳过
	// 不会掩盖「VPP 侧其实没置表」——那时登记是空的，会走下面的运行态核对。
	p.mu.Lock()
	old, registered := p.vnfs[ifname]
	p.mu.Unlock()
	if registered && old.tableID == tableID && old.idx == idx {
		return nil
	}
	if err := p.ensureTables(c, tableID, vrfName); err != nil {
		return fmt.Errorf("为 VNF 接口 %s 准备 VRF %s 的 IP 表: %w", ifname, vrfName, err)
	}
	// 逐协议核对运行态：只有「查得到**且**已在目标表」才跳过置表，避免对带地址的口
	// 重复下发 -114。查询失败按「不知道」处理、退回直接置表（与本包既有运行态查询
	// BviOfBD 同一口径：查询是优化不是前提），但置表也失败时把查询错误一并带上。
	for _, ip6 := range []bool{false, true} {
		cur, known, qerr := c.SwInterfaceTable(idx, ip6)
		if known && cur == tableID {
			continue
		}
		if err := c.SwInterfaceSetTable(idx, ip6, tableID); err != nil {
			if qerr != nil {
				return fmt.Errorf("VNF 接口 %s 置入 VRF %s 的 %s 表: %w（运行态查表也失败: %v）",
					ifname, vrfName, ipVerName(ip6), err, qerr)
			}
			return fmt.Errorf("VNF 接口 %s 置入 VRF %s 的 %s 表: %w", ifname, vrfName, ipVerName(ip6), err)
		}
	}
	p.mu.Lock()
	p.vnfs[ifname] = vnfAttach{tableID: tableID, idx: idx}
	// 同一登记使 TableOfIface（NAT outside 转发域解析）与 DeleteVRF 的清理也覆盖 vNIC。
	// ifaces 同样带上它：NAT inside 的解析来源之一是 vnfs，两处一致才不会出现
	// 「登记在、集合里没有」的错觉（DeleteVRF 按 ifaces 清地址时对 vNIC 是空操作）。
	// 换交换机（vNIC 改挂另一台 L3 交换机）时必须把旧表登记摘掉，见 reassignLocked。
	p.reassignLocked(tableID, []uint32{idx})
	p.ifaceTable[ifname] = tableID
	p.ifaceIdx[ifname] = idx
	p.idxSwitch[idx] = vrfName // 决策 #345：vNIC 是该 L3 交换机的转发域
	if !containsIdx(p.ifaces[tableID], idx) {
		p.ifaces[tableID] = append(p.ifaces[tableID], idx)
	}
	p.mu.Unlock()
	return nil
}

// ipVerName 协议名（错误文案用，与 VPP 侧 i/ip6 表述一致）。
func ipVerName(isIP6 bool) string {
	if isIP6 {
		return "IPv6"
	}
	return "IPv4"
}

// ForgetVnfIface 摘除 vNIC 的置表登记（vNIC 从数据面移除时调用；幂等），返回被摘除的
// sw_if_index（ok=false = 原本未登记），供调用方把 NAT 侧的同一索引登记一并摘掉。
// 只摘登记、不触碰 VPP：接口本身由 vhost-user/memif 编排删除，其 sw_if_index 随之失效，
// 残留登记会让 NAT inside 指向不存在的口（下一次 ApplyNAT 直接失败）。
func (p *L3Provider) ForgetVnfIface(ifname string) (uint32, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.vnfs[ifname]
	delete(p.vnfs, ifname)
	delete(p.ifaceTable, ifname)
	delete(p.ifaceIdx, ifname)
	if !ok {
		return 0, false
	}
	delete(p.idxSwitch, a.idx) // 决策 #345：该 vNIC 不再作为转发域
	// 该口若同时是 L3 接口（配了地址），ApplyVRF 也登记过它：一并摘掉，避免留下死索引。
	// 重新声明后 ApplyVRF/SetVnfTable 会再次登记（新 sw_if_index）。
	if rec := p.ifaces[a.tableID]; len(rec) > 0 {
		out := make([]uint32, 0, len(rec))
		for _, idx := range rec {
			if idx != a.idx {
				out = append(out, idx)
			}
		}
		p.ifaces[a.tableID] = out
	}
	return a.idx, true
}

// Routes 返回 VRF 的运行态 FIB（**IPv4 + IPv6 合并**）。
//
// 注：v6 路由须显式以 IsIP6 dump（VPP ip_route_dump 不区分协议就不返回 v6 条目），
// 否则 `show routes` / `GET /vrfs/{name}/routes` 看不到 FR-NET-013 的 v6 静态路由。
func (p *L3Provider) Routes(ctx context.Context, name string) ([]RouteEntry, error) {
	c, err := p.client()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	table := TableID(name)
	rows, err := c.Routes(table, false)
	if err != nil {
		return nil, err
	}
	v6, err := c.Routes(table, true)
	if err != nil {
		return nil, err
	}
	rows = append(rows, v6...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Prefix < rows[j].Prefix })
	return rows, nil
}

// ApplyGateway 配置 L2 交换机的 BVI 三层网关（FR-NET-014）。
func (p *L3Provider) ApplyGateway(ctx context.Context, vs model.VirtualSwitch) error {
	gw := vs.Gateway
	if gw == nil {
		return nil
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	vrfName := gw.Vrf
	own := vrfName == ""
	if own {
		vrfName = GatewayVRFName(vs.Name)
	}
	tableID := TableID(vrfName)
	if err := p.ensureTables(c, tableID, vrfName); err != nil {
		return err
	}

	p.mu.Lock()
	bvi, ok := p.bvis[vs.Name]
	p.mu.Unlock()
	reused := false
	if !ok {
		// 恢复场景：nfvisd 重启后内存映射丢失，而 VPP 侧 BD 上已有 BVI
		// （整机/进程重启后重放配置时命中），此时必须复用而非重建，
		// 否则 BviSetBD 报 -152 "Bridge domain already has a BVI interface"。
		if existing, found, derr := c.BviOfBD(BDID(vs.Name)); derr == nil && found {
			bvi, reused = existing, true
		} else {
			bvi, err = c.BviCreate()
			if err != nil {
				return fmt.Errorf("创建 BVI（%s）: %w", vs.Name, err)
			}
			if err := c.BviSetBD(bvi, BDID(vs.Name)); err != nil {
				return fmt.Errorf("BVI 挂入 bridge-domain %s: %w", vs.Name, err)
			}
		}
	}
	// 先清旧地址再置表：VPP 拒绝把仍带地址的接口移到其它 VRF（-114），
	// 与 L3 接口路径同一处理（BVI 换 VRF 时命中）
	if err := c.SwInterfaceAddDelAddress(bvi, "", false, true); err != nil {
		return fmt.Errorf("清理 BVI（%s）旧地址: %w", vs.Name, err)
	}
	if err := c.SwInterfaceSetTable(bvi, false, tableID); err != nil {
		return fmt.Errorf("BVI 置入 VRF %s: %w", vrfName, err)
	}
	if err := c.SwInterfaceSetTable(bvi, true, tableID); err != nil {
		return fmt.Errorf("BVI 置入 VRF %s(IPv6): %w", vrfName, err)
	}
	// VPP 接口（含 BVI）默认 admin-down：不置 up 则 BVI 恒为 down，网关不可达
	if err := c.SetState(bvi, true); err != nil {
		return fmt.Errorf("BVI（%s）置为 up: %w", vs.Name, err)
	}
	for _, addr := range gw.Addresses {
		if err := c.SwInterfaceAddDelAddress(bvi, addr, true, false); err != nil {
			return fmt.Errorf("BVI 配地址 %s: %w", addr, err)
		}
	}
	if p.acl != nil && (gw.AclIn != "" || gw.AclOut != "") {
		c2, err := p.acl.client()
		if err != nil {
			return err
		}
		defer c2.Close()
		if err := p.acl.BindIndex(c2, bvi, gw.AclIn, gw.AclOut); err != nil {
			return err
		}
	}
	_ = reused
	p.mu.Lock()
	p.bvis[vs.Name] = bvi
	p.ownTable[vs.Name] = own
	p.idxSwitch[bvi] = vs.Name // 决策 #345：BVI 是 L2 交换机的转发域（DNS 代理反查用）
	p.mu.Unlock()
	return nil
}

// DeleteGateway 拆除 BVI 与专属 VRF（非专属 VRF 的表保留）。
func (p *L3Provider) DeleteGateway(ctx context.Context, vsName string) error {
	p.mu.Lock()
	bvi, ok := p.bvis[vsName]
	own := p.ownTable[vsName]
	delete(p.bvis, vsName)
	delete(p.ownTable, vsName)
	delete(p.idxSwitch, bvi) // 决策 #345：BVI 转发域归属随之消失
	p.mu.Unlock()
	if !ok {
		return nil
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.SwInterfaceAddDelAddress(bvi, "", false, true); err != nil {
		return fmt.Errorf("清理 BVI 地址: %w", err)
	}
	if err := c.BviDelete(bvi); err != nil {
		return fmt.Errorf("删除 BVI: %w", err)
	}
	if own {
		tableID := TableID(GatewayVRFName(vsName))
		for _, ip6 := range []bool{false, true} {
			if err := c.IPTableAddDel(tableID, ip6, false, GatewayVRFName(vsName)); err != nil {
				return fmt.Errorf("删网关 VRF table %d: %w", tableID, err)
			}
		}
	}
	return nil
}

func (p *L3Provider) ensureTables(c L3Client, tableID uint32, name string) error {
	for _, ip6 := range []bool{false, true} {
		if err := c.IPTableAddDel(tableID, ip6, true, name); err != nil {
			return fmt.Errorf("建 IP table %d(%s): %w", tableID, name, err)
		}
	}
	return nil
}

// resolveL3Iface 解析 L3 接口：物理口/bond，或 vlan 子接口（li.Vlan>0 时创建）。
func (p *L3Provider) resolveL3Iface(c L3Client, li model.L3Interface) (idx, sub uint32, err error) {
	base := li.Interface
	if li.Vlan > 0 {
		parent, ok, ferr := c.SwInterfaceIndex(base)
		if ferr != nil {
			return 0, 0, fmt.Errorf("解析接口 %s: %w", base, ferr)
		}
		if !ok {
			return 0, 0, fmt.Errorf("%w: %s"+ifaceMissingHint, ErrIfaceUnavailable, base)
		}
		s, cerr := c.CreateSubif(CreateSubifReq{ParentSwIfIndex: parent, SubID: uint32(li.Vlan),
			OuterVlanID: uint16(li.Vlan), OneTag: true})
		if cerr != nil {
			return 0, 0, fmt.Errorf("创建 %s vlan %d 子接口: %w", base, li.Vlan, cerr)
		}
		return s, s, nil
	}
	idx, ok, err := c.SwInterfaceIndex(base)
	if err != nil {
		return 0, 0, fmt.Errorf("解析接口 %s: %w", base, err)
	}
	if !ok {
		return 0, 0, fmt.Errorf("%w: %s"+ifaceMissingHint, ErrIfaceUnavailable, base)
	}
	return idx, 0, nil
}

// IsIPv6Prefix 判断地址是否为 IPv6（供测试与调用方使用）。
func IsIPv6Prefix(prefix string) bool { return strings.Contains(prefix, ":") }

// ErrL3Unavailable 未连接 VPP 时 L3 客户端不可用。
var ErrL3Unavailable = errors.New("VPP 未连接，L3 客户端不可用")
