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
)

// RouteEntry 一条 FIB 路由（运行态）。
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
	acl      *AclProvider // 可选：L3 接口/BVI 的 acl-in 绑定
}

// SetACL 注入 ACL 编排（L3 接口与 BVI 网关的 acl-in 绑定）。
func (p *L3Provider) SetACL(a *AclProvider) { p.acl = a }

// reset 清空进程内登记表（恢复收敛前调用，避免陈旧 sw_if_index/表归属把重放带偏）。
func (p *L3Provider) reset() {
	p.mu.Lock()
	p.ifaces = map[uint32][]uint32{}
	p.subifs = map[uint32][]uint32{}
	p.vnfs = map[string]vnfAttach{}
	p.bvis = map[string]uint32{}
	p.ownTable = map[string]bool{}
	p.ifaceTable = map[string]uint32{}
	p.ifaceIdx = map[string]uint32{}
	p.mu.Unlock()
}

// NewL3Provider 以固定客户端构造（测试）。
func NewL3Provider(c L3Client) *L3Provider {
	return &L3Provider{client: func() (L3Client, error) { return c, nil },
		ifaces: map[uint32][]uint32{}, subifs: map[uint32][]uint32{}, vnfs: map[string]vnfAttach{},
		bvis: map[string]uint32{}, ownTable: map[string]bool{}, ifaceTable: map[string]uint32{},
		ifaceIdx: map[string]uint32{}}
}

// NewL3ProviderFunc 以客户端工厂构造（连接可重连）。
func NewL3ProviderFunc(f func() (L3Client, error)) *L3Provider {
	return &L3Provider{client: f, ifaces: map[uint32][]uint32{}, subifs: map[uint32][]uint32{},
		vnfs: map[string]vnfAttach{}, bvis: map[string]uint32{}, ownTable: map[string]bool{},
		ifaceTable: map[string]uint32{}, ifaceIdx: map[string]uint32{}}
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
	p.ifaces[tableID], p.subifs[tableID] = idxs, subs
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
		p.ifaceTable[li.Interface] = tableID
		p.ifaceIdx[li.Interface] = idx
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
// 为什么不能让 ApplyVRF/DeleteVRF 代劳：ApplyVRF 只下发声明里的路由（只加不撤），
// 而 DeleteVRF 依赖删表——VPP 会保住仍被接口占用的表（`show ip table` 里该项带
// locks:[interface:…]），路由随之长期留在 FIB 里而配置、show 输出全绿（真机实测）。
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

// TableOfIface 返回接口所属 VRF 的 tableID（未归属任何 VRF 时 ok=false）。
// 供 NAT44 outside 转发域解析（决策 #52）。
func (p *L3Provider) TableOfIface(ifname string) (uint32, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.ifaceTable[ifname]
	return t, ok
}

// DeleteVRF 删除 VRF：清接口地址、删 v4/v6 table（路由随表删除）。
func (p *L3Provider) DeleteVRF(ctx context.Context, name string) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	tableID := TableID(name)
	p.mu.Lock()
	idxs, subs := p.ifaces[tableID], p.subifs[tableID]
	delete(p.ifaces, tableID)
	delete(p.subifs, tableID)
	for k, t := range p.ifaceTable {
		if t == tableID {
			delete(p.ifaceTable, k)
			delete(p.ifaceIdx, k)
		}
	}
	// 置入该表的 vNIC 登记一并摘除（表已删，登记不再有意义；重新声明时 ApplyVRF/SetVnfTable 重建）。
	for k, a := range p.vnfs {
		if a.tableID == tableID {
			delete(p.vnfs, k)
		}
	}
	p.mu.Unlock()
	for _, idx := range idxs {
		if err := c.SwInterfaceAddDelAddress(idx, "", false, true); err != nil {
			return fmt.Errorf("清理接口 %d 地址: %w", idx, err)
		}
	}
	for _, sub := range subs {
		if err := c.SwInterfaceAddDelAddress(sub, "", false, true); err != nil {
			return fmt.Errorf("清理子接口 %d 地址: %w", sub, err)
		}
	}
	for _, ip6 := range []bool{false, true} {
		if err := c.IPTableAddDel(tableID, ip6, false, name); err != nil {
			return fmt.Errorf("删 IP table %d: %w", tableID, err)
		}
	}
	return nil
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
	p.ifaceTable[ifname] = tableID
	p.ifaceIdx[ifname] = idx
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
