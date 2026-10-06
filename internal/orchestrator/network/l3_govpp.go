package network

// govpp L3 客户端（M3-4）：唯一使用 ip/interface/l2 BVI binapi 的地方。

import (
	"fmt"
	"sort"
	"strings"

	"go.fd.io/govpp/api"
	"go.fd.io/govpp/binapi/fib_types"
	ifapi "go.fd.io/govpp/binapi/interface"
	"go.fd.io/govpp/binapi/interface_types"
	"go.fd.io/govpp/binapi/ip"
	"go.fd.io/govpp/binapi/ip_types"
	"go.fd.io/govpp/binapi/l2"
)

// L3ClientFunc 返回随当前连接获取 L3 客户端的工厂。
func (m *Manager) L3ClientFunc() func() (L3Client, error) {
	return func() (L3Client, error) {
		ch, err := m.APIChannel()
		if err != nil {
			return nil, err
		}
		return &govppL3Client{ch: ch}, nil
	}
}

type govppL3Client struct{ ch api.Channel }

func (g *govppL3Client) Close() { g.ch.Close() }

func (g *govppL3Client) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	reqCtx := g.ch.SendMultiRequest(&ifapi.SwInterfaceDump{})
	for {
		d := &ifapi.SwInterfaceDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return 0, false, err
		}
		if stop {
			return 0, false, nil
		}
		if d.InterfaceName == ifname || d.Tag == ifname {
			return uint32(d.SwIfIndex), true, nil
		}
	}
}

func (g *govppL3Client) CreateSubif(req CreateSubifReq) (uint32, error) {
	return (&govppL2Client{ch: g.ch}).CreateSubif(req)
}

func (g *govppL3Client) IPTableAddDel(tableID uint32, isIP6, add bool, name string) error {
	reply := &ip.IPTableAddDelReply{}
	err := g.ch.SendRequest(&ip.IPTableAddDel{
		IsAdd: add,
		Table: ip.IPTable{TableID: tableID, IsIP6: isIP6, Name: name},
	}).ReceiveReply(reply)
	if err != nil {
		if add && vppErrIs(err, vppTableExist) {
			return nil
		}
		return err
	}
	if reply.Retval != 0 {
		if add && reply.Retval == vppTableExist {
			return nil
		}
		return fmt.Errorf("ip_table_add_del(table=%d,ip6=%v,add=%v) retval=%d", tableID, isIP6, add, reply.Retval)
	}
	return nil
}

// IPTableExists 读回该协议下 tableID 是否存在（全量 ip_table_dump 后按 table_id + 协议匹配）。
//
// 删表校验专用：VPP 的 ip_table_add_del(del) 在表仍被接口占用时返回 0 却不删，
// 只信返回码（或只信 CLI/配置侧回读）就会「报成功却没做到」。同一个 table_id 在 v4/v6
// 各有一张表，故必须连协议一起匹配。
func (g *govppL3Client) IPTableExists(tableID uint32, isIP6 bool) (bool, error) {
	reqCtx := g.ch.SendMultiRequest(&ip.IPTableDump{})
	for {
		d := &ip.IPTableDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return false, err
		}
		if stop {
			return false, nil
		}
		if d.Table.TableID == tableID && d.Table.IsIP6 == isIP6 {
			return true, nil
		}
	}
}

// SwInterfaceTable 查接口运行态当前所属表（供 SetVnfTable 判断是否需要下发置表：
// VPP 只允许无地址的接口换表，带了地址的口重复下发会报 -114）。
func (g *govppL3Client) SwInterfaceTable(swIfIndex uint32, isIP6 bool) (uint32, bool, error) {
	reply := &ifapi.SwInterfaceGetTableReply{}
	err := g.ch.SendRequest(&ifapi.SwInterfaceGetTable{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex), IsIPv6: isIP6,
	}).ReceiveReply(reply)
	if err != nil {
		return 0, false, err
	}
	if reply.Retval != 0 {
		return 0, false, fmt.Errorf("sw_interface_get_table(if=%d,ip6=%v) retval=%d", swIfIndex, isIP6, reply.Retval)
	}
	return reply.VrfID, true, nil
}

func (g *govppL3Client) SwInterfaceSetTable(swIfIndex uint32, isIP6 bool, tableID uint32) error {
	reply := &ifapi.SwInterfaceSetTableReply{}
	err := g.ch.SendRequest(&ifapi.SwInterfaceSetTable{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex), IsIPv6: isIP6, VrfID: tableID,
	}).ReceiveReply(reply)
	if err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("sw_interface_set_table(if=%d,vrf=%d) retval=%d", swIfIndex, tableID, reply.Retval)
	}
	return nil
}

func (g *govppL3Client) SwInterfaceAddDelAddress(swIfIndex uint32, prefix string, add, delAll bool) error {
	req := &ifapi.SwInterfaceAddDelAddress{
		SwIfIndex: interface_types.InterfaceIndex(swIfIndex),
		IsAdd:     add,
		DelAll:    delAll,
	}
	if !delAll {
		p, err := ip_types.ParseAddressWithPrefix(prefix)
		if err != nil {
			return fmt.Errorf("解析地址 %q: %w", prefix, err)
		}
		req.Prefix = p
	}
	reply := &ifapi.SwInterfaceAddDelAddressReply{}
	if err := g.ch.SendRequest(req).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("sw_interface_add_del_address(if=%d,%s) retval=%d", swIfIndex, prefix, reply.Retval)
	}
	return nil
}

func (g *govppL3Client) IPRouteAddDel(tableID uint32, prefix, nextHop string, add bool) error {
	p, err := ip_types.ParsePrefix(prefix)
	if err != nil {
		return fmt.Errorf("解析前缀 %q: %w", prefix, err)
	}
	route := ip.IPRoute{TableID: tableID, Prefix: p}
	if nextHop != "" {
		paths, err := nextHopPaths(tableID, nextHop)
		if err != nil {
			return err
		}
		route.NPaths = uint8(len(paths))
		route.Paths = paths
	}
	reply := &ip.IPRouteAddDelReply{}
	if err := g.ch.SendRequest(&ip.IPRouteAddDel{IsAdd: add, Route: route}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("ip_route_add_del(%s via %s, table=%d) retval=%d", prefix, nextHop, tableID, reply.Retval)
	}
	return nil
}

// recursiveNextHopPaths 构造「经下一跳递归解析」的 FIB path（下一跳为纯地址，不含出接口）。
//
// TableID 必填：递归下一跳在**哪张表**里解析由 path 的 table_id 决定，缺省 0 = 默认表。
// 不填时 VRF 内路由会去默认表找下一跳（真机 round88：FIB 显示 `via X in fib:0`，
// 默认表里没有该邻居 → 整条路由恒 dpo-drop、不转发，而 IPRouteDump 回读一切正常，
// 是典型「命令成功但答非所问」）。
func recursiveNextHopPaths(tableID uint32, nextHop string) ([]fib_types.FibPath, error) {
	addr, err := ip_types.ParseAddress(nextHop)
	if err != nil {
		return nil, fmt.Errorf("解析下一跳 %q: %w", nextHop, err)
	}
	proto := fib_types.FIB_API_PATH_NH_PROTO_IP4
	var nh ip_types.AddressUnion
	switch addr.Af {
	case ip_types.ADDRESS_IP6:
		nh = ip_types.AddressUnionIP6(addr.Un.GetIP6())
		proto = fib_types.FIB_API_PATH_NH_PROTO_IP6
	default:
		nh = ip_types.AddressUnionIP4(addr.Un.GetIP4())
	}
	return []fib_types.FibPath{{
		SwIfIndex: ^uint32(0), // ~0 = 经下一跳递归解析
		TableID:   tableID,    // 在路由所属表内解析下一跳（勿省，见上）
		Proto:     proto,
		Nh:        fib_types.FibPathNh{Address: nh},
	}}, nil
}

// nextHopPaths 按逗号分隔的多下一跳（ECMP，决策 #381）构造 FIB 路径列表：
// 每个下一跳一条递归路径、Weight=1 等权。单值即 1 条（与既有行为一致）。
//
// 各路径的解析语义完全交给 recursiveNextHopPaths（表内递归、SwIfIndex=~0、TableID 必填）。
// 元素级合法性（非空/有效 IP/去重/同族/上限）由提交期 model.Validate 保证，此处只负责构造；
// 解析失败（含空元素）时错误文案含**完整** nextHop 串，便于定位整条语句。
func nextHopPaths(tableID uint32, nextHop string) ([]fib_types.FibPath, error) {
	hops := strings.Split(nextHop, ",")
	paths := make([]fib_types.FibPath, 0, len(hops))
	for _, nh := range hops {
		ps, err := recursiveNextHopPaths(tableID, nh)
		if err != nil {
			return nil, fmt.Errorf("下一跳 %q（整串 %q）: %w", nh, nextHop, err)
		}
		for _, p := range ps {
			p.Weight = 1 // 等权 ECMP；单路径时权重无影响
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// fibNhString 从 FIB path 解析下一跳文本（IPv4/IPv6）。
func fibNhString(p fib_types.FibPath) string {
	switch p.Proto {
	case fib_types.FIB_API_PATH_NH_PROTO_IP4:
		return p.Nh.Address.GetIP4().String()
	case fib_types.FIB_API_PATH_NH_PROTO_IP6:
		return p.Nh.Address.GetIP6().String()
	}
	return ""
}

func (g *govppL3Client) Routes(tableID uint32, isIP6 bool) ([]RouteEntry, error) {
	// IsIP6 必须显式传递：否则 VPP 只 dump IPv4 路由，v6 静态路由不可见
	reqCtx := g.ch.SendMultiRequest(&ip.IPRouteDump{Table: ip.IPTable{TableID: tableID, IsIP6: isIP6}})
	var out []RouteEntry
	for {
		d := &ip.IPRouteDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return nil, err
		}
		if stop {
			break
		}
		e := RouteEntry{Prefix: d.Route.Prefix.String()}
		if len(d.Route.Paths) > 0 {
			// 决策 #393：如实呈现**全部**下一跳。此前只取 Paths[0]，ECMP 路由在运行态
			// 读视图里只显示首跳（真机 round171 §1.4：FIB `buckets:2` 而读视图单跳），
			// 用户看不出等价多路径已生效。多跳以逗号串呈现，与配置/`display set` 同形；
			// 单跳时逐字不变。非 IP 路径（如 dpo-drop）无下一跳文本，跳过。
			nhs := make([]string, 0, len(d.Route.Paths))
			for _, p := range d.Route.Paths {
				if s := fibNhString(p); s != "" {
					nhs = append(nhs, s)
				}
			}
			e.NextHop = strings.Join(nhs, ",")
		}
		out = append(out, e)
	}
	return out, nil
}

// SetState 置接口管理员状态（BVI 与数据口一致：默认 down，必须显式 up）。
func (g *govppL3Client) SetState(swIfIndex uint32, up bool) error {
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

func (g *govppL3Client) BviCreate() (uint32, error) {
	reply := &l2.BviCreateReply{}
	if err := g.ch.SendRequest(&l2.BviCreate{UserInstance: ^uint32(0)}).ReceiveReply(reply); err != nil {
		return 0, err
	}
	if reply.Retval != 0 {
		return 0, fmt.Errorf("bvi_create retval=%d", reply.Retval)
	}
	return uint32(reply.SwIfIndex), nil
}

// BviOfBD 返回 bridge domain 上既有 BVI 的 sw_if_index（无则 found=false）。
// 全量 dump 后按 BdID 匹配：带 BdID 过滤的 dump 在 VPP 26.06 上会返回空。
func (g *govppL3Client) BviOfBD(bdID uint32) (uint32, bool, error) {
	reqCtx := g.ch.SendMultiRequest(&l2.BridgeDomainDump{
		BdID:      bdID,
		SwIfIndex: interface_types.InterfaceIndex(0xFFFFFFFF),
	})
	for {
		d := &l2.BridgeDomainDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return 0, false, err
		}
		if stop {
			return 0, false, nil
		}
		if d.BdID == bdID && d.BviSwIfIndex != 0 &&
			d.BviSwIfIndex != interface_types.InterfaceIndex(0xFFFFFFFF) {
			return uint32(d.BviSwIfIndex), true, nil
		}
	}
}

func (g *govppL3Client) BviDelete(swIfIndex uint32) error {
	reply := &l2.BviDeleteReply{}
	if err := g.ch.SendRequest(&l2.BviDelete{SwIfIndex: interface_types.InterfaceIndex(swIfIndex)}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("bvi_delete(%d) retval=%d", swIfIndex, reply.Retval)
	}
	return nil
}

// BviSetBD 把 BVI 挂入 bridge domain（port type BVI，FR-NET-014）。
func (g *govppL3Client) BviSetBD(swIfIndex, bdID uint32) error {
	reply := &l2.SwInterfaceSetL2BridgeReply{}
	if err := g.ch.SendRequest(&l2.SwInterfaceSetL2Bridge{
		RxSwIfIndex: interface_types.InterfaceIndex(swIfIndex),
		BdID:        bdID,
		PortType:    l2.L2_API_PORT_TYPE_BVI,
		Enable:      true,
	}).ReceiveReply(reply); err != nil {
		return err
	}
	if reply.Retval != 0 {
		return fmt.Errorf("sw_interface_set_l2_bridge(BVI %d, bd=%d) retval=%d", swIfIndex, bdID, reply.Retval)
	}
	return nil
}

// IPTables 列出 VPP 里现有的 IP 表（v4/v6 合并去重，升序）。
//
// 与 IPTableExists 同一次 dump 的能力，只是这里要全集：删表延后/补偿残渣留下的空表只有靠
// 「配置声明集 ⇄ VPP 实况」对账才看得见（决策 #192，见 L3Provider.LeftoverTables）。
// 默认表 0 也在返回里——声明集同样含它，不会被当成残留。
func (g *govppL3Client) IPTables() ([]uint32, error) {
	reqCtx := g.ch.SendMultiRequest(&ip.IPTableDump{})
	seen := map[uint32]bool{}
	for {
		d := &ip.IPTableDetails{}
		stop, err := reqCtx.ReceiveReply(d)
		if err != nil {
			return nil, fmt.Errorf("列出 IP 表: %w", err)
		}
		if stop {
			break
		}
		seen[d.Table.TableID] = true
	}
	out := make([]uint32, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
