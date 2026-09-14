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
	CreateSubif(req CreateSubifReq) (uint32, error)
	IPTableAddDel(tableID uint32, isIP6, add bool, name string) error
	SwInterfaceSetTable(swIfIndex uint32, isIP6 bool, tableID uint32) error
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

// L3Provider 实现 VRF/L3 接口/静态路由与 BVI 网关编排。
type L3Provider struct {
	client func() (L3Client, error)

	mu         sync.Mutex
	ifaces     map[uint32][]uint32 // tableID → 配置过的 sw_if_index（删除时清地址）
	subifs     map[uint32][]uint32 // tableID → 自建子接口（删除时一并移除）
	bvis       map[string]uint32   // 交换机名 → BVI sw_if_index
	ownTable   map[string]bool     // 交换机名 → BVI 使用专属表（删除时删表）
	ifaceTable map[string]uint32   // 接口名 → 所属 VRF tableID（NAT outside 转发域解析，决策 #52）
	acl        *AclProvider        // 可选：L3 接口/BVI 的 acl-in 绑定
}

// SetACL 注入 ACL 编排（L3 接口与 BVI 网关的 acl-in 绑定）。
func (p *L3Provider) SetACL(a *AclProvider) { p.acl = a }

// reset 清空进程内登记表（恢复收敛前调用，避免陈旧 sw_if_index/表归属把重放带偏）。
func (p *L3Provider) reset() {
	p.mu.Lock()
	p.ifaces = map[uint32][]uint32{}
	p.subifs = map[uint32][]uint32{}
	p.bvis = map[string]uint32{}
	p.ownTable = map[string]bool{}
	p.ifaceTable = map[string]uint32{}
	p.mu.Unlock()
}

// NewL3Provider 以固定客户端构造（测试）。
func NewL3Provider(c L3Client) *L3Provider {
	return &L3Provider{client: func() (L3Client, error) { return c, nil },
		ifaces: map[uint32][]uint32{}, subifs: map[uint32][]uint32{},
		bvis: map[string]uint32{}, ownTable: map[string]bool{}, ifaceTable: map[string]uint32{}}
}

// NewL3ProviderFunc 以客户端工厂构造（连接可重连）。
func NewL3ProviderFunc(f func() (L3Client, error)) *L3Provider {
	return &L3Provider{client: f, ifaces: map[uint32][]uint32{}, subifs: map[uint32][]uint32{},
		bvis: map[string]uint32{}, ownTable: map[string]bool{}, ifaceTable: map[string]uint32{}}
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
	}

	// 先清旧路由再全量下发（PUT 语义：全量替换静态路由）
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
	for _, li := range vrf.L3Interfaces {
		p.ifaceTable[li.Interface] = tableID
	}
	p.mu.Unlock()
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

// AttachedIfaces 返回 VRF 已配置的 sw_if_index（供 NAT inside 解析）。
func (p *L3Provider) AttachedIfaces(vrfName string) []uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec := p.ifaces[TableID(vrfName)]
	out := make([]uint32, len(rec))
	copy(out, rec)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// SetVnfTable 把 VNF vNIC 接口置入 L3 交换机（VRF）对应表（FR-NET-020 的 L3 场景；
// vNIC 无 IP，仅入表以便经 VRF 转发）。接口按确定性名解析（由 ApplyVnfInterface 先建）。
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
	if err := c.SwInterfaceSetTable(idx, false, tableID); err != nil {
		return fmt.Errorf("VNF 接口 %s 置入 VRF %s 的 IPv4 表: %w", ifname, vrfName, err)
	}
	if err := c.SwInterfaceSetTable(idx, true, tableID); err != nil {
		return fmt.Errorf("VNF 接口 %s 置入 VRF %s 的 IPv6 表: %w", ifname, vrfName, err)
	}
	return nil
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
			return 0, 0, fmt.Errorf("%w: %s（是否未由 DPDK 接管？）", ErrIfaceUnavailable, base)
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
		return 0, 0, fmt.Errorf("%w: %s（是否未由 DPDK 接管？）", ErrIfaceUnavailable, base)
	}
	return idx, 0, nil
}

// IsIPv6Prefix 判断地址是否为 IPv6（供测试与调用方使用）。
func IsIPv6Prefix(prefix string) bool { return strings.Contains(prefix, ":") }

// ErrL3Unavailable 未连接 VPP 时 L3 客户端不可用。
var ErrL3Unavailable = errors.New("VPP 未连接，L3 客户端不可用")
