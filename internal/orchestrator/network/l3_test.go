package network

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.fd.io/govpp/api"
	ifapi "go.fd.io/govpp/binapi/interface"

	"github.com/xzjt/nfvis/internal/model"
)

// ---------- M3-4：L3/VRF/BVI（FR-NET-013/014）单测（假 L3Client） ----------

type fakeL3 struct {
	ifaces  map[string]uint32
	nextSub uint32
	nextBVI uint32

	tables      map[uint32]bool
	v4table     map[uint32]uint32 // swIfIndex → v4 table
	v6table     map[uint32]uint32 // swIfIndex → v6 table（镜像 VPP：两协议各一张）
	setTables   int               // SwInterfaceSetTable 调用次数（置表幂等断言用）
	setTableIdx []uint32          // 每次置表的目标接口（区分是谁下发的）
	// tableQueryUnavailable 模拟答不出 sw_interface_get_table 的客户端：
	// SetVnfTable 应退回「直接置表」，不得因此报错。
	tableQueryUnavailable bool
	// tableQueryErr 运行态查询失败：按「不知道」处理并退回直接置表（查询是优化不是前提），
	// 但置表也失败时错误里要同时带上它。
	tableQueryErr error
	setTableErr   error // 仅置表失败
	// vnfIndexFailOnce 首次解析该接口名时答「不存在」，之后正常：模拟 vNIC 接入路径
	// 那一轮登记失败（此后接口才可见），恢复收敛的登记重建必须随后把它补回来。
	vnfIndexFailOnce map[string]bool
	// ifaceFailN 前 N 次解析该接口名答「不存在」，之后正常：模拟 VPP 刚重启时接口尚未
	// 枚举出来——重放那一刻解析不到（ApplyVRF 整条 VRF 因此失败、登记为空），稍后才可见。
	// 登记重建必须不依赖重放那次是否成功。
	ifaceFailN map[string]int
	addrs      map[uint32][]string
	routes     map[uint32][]RouteEntry
	routes6    map[uint32][]RouteEntry // v6 路由（与 v4 分表，镜像 VPP 语义）
	bviBD      map[uint32]uint32
	bviGone    []uint32
	state      map[uint32]bool // 接口管理员状态（SetState 记录）
	bviCreated int             // BviCreate 调用次数（恢复幂等断言用）
	cleared    []uint32        // 被清地址的接口（删除全部地址）
	err        error
}

func newFakeL3() *fakeL3 {
	return &fakeL3{
		ifaces: map[string]uint32{"ens192": 1, "ens224": 2}, nextSub: 100, nextBVI: 900,
		tables: map[uint32]bool{}, v4table: map[uint32]uint32{}, v6table: map[uint32]uint32{},
		vnfIndexFailOnce: map[string]bool{}, ifaceFailN: map[string]int{},
		addrs: map[uint32][]string{}, routes: map[uint32][]RouteEntry{},
		routes6: map[uint32][]RouteEntry{}, bviBD: map[uint32]uint32{},
		state: map[uint32]bool{}, cleared: nil,
	}
}

func (f *fakeL3) Close() {}

// BviOfBD 返回 BD 上既有 BVI（模拟 nfvisd 重启后 VPP 侧对象仍在）。
func (f *fakeL3) BviOfBD(bdID uint32) (uint32, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	for idx, bd := range f.bviBD {
		if bd == bdID {
			return idx, true, nil
		}
	}
	return 0, false, nil
}

func (f *fakeL3) SetState(swIfIndex uint32, up bool) error {
	if f.err != nil {
		return f.err
	}
	f.state[swIfIndex] = up
	return nil
}

func (f *fakeL3) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	if f.vnfIndexFailOnce[ifname] {
		delete(f.vnfIndexFailOnce, ifname)
		return 0, false, nil
	}
	if n := f.ifaceFailN[ifname]; n > 0 {
		f.ifaceFailN[ifname] = n - 1
		return 0, false, nil
	}
	idx, ok := f.ifaces[ifname]
	return idx, ok, nil
}

func (f *fakeL3) CreateSubif(req CreateSubifReq) (uint32, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.nextSub++
	f.ifaces[""] = f.nextSub
	return f.nextSub, nil
}

func (f *fakeL3) IPTableAddDel(tableID uint32, isIP6, add bool, name string) error {
	if f.err != nil {
		return f.err
	}
	if add {
		f.tables[tableID] = true
	} else {
		delete(f.tables, tableID)
	}
	return nil
}

// SwInterfaceTable 运行态查询（sw_interface_get_table）：v4/v6 分开跟踪，镜像 VPP 语义。
// tableQueryUnavailable 时返回 ok=false（答不出），调用方应退回直接置表。
func (f *fakeL3) SwInterfaceTable(swIfIndex uint32, isIP6 bool) (uint32, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	if f.tableQueryErr != nil {
		return 0, false, f.tableQueryErr
	}
	if f.tableQueryUnavailable {
		return 0, false, nil
	}
	if isIP6 {
		return f.v6table[swIfIndex], true, nil
	}
	return f.v4table[swIfIndex], true, nil
}

func (f *fakeL3) SwInterfaceSetTable(swIfIndex uint32, isIP6 bool, tableID uint32) error {
	if f.err != nil {
		return f.err
	}
	if f.setTableErr != nil {
		return f.setTableErr
	}
	f.setTables++
	f.setTableIdx = append(f.setTableIdx, swIfIndex)
	if isIP6 {
		f.v6table[swIfIndex] = tableID
		return nil
	}
	f.v4table[swIfIndex] = tableID
	return nil
}

func (f *fakeL3) SwInterfaceAddDelAddress(swIfIndex uint32, prefix string, add, delAll bool) error {
	if f.err != nil {
		return f.err
	}
	if delAll {
		delete(f.addrs, swIfIndex)
		f.cleared = append(f.cleared, swIfIndex)
		return nil
	}
	if add {
		f.addrs[swIfIndex] = append(f.addrs[swIfIndex], prefix)
	}
	return nil
}

func (f *fakeL3) IPRouteAddDel(tableID uint32, prefix, nextHop string, add bool) error {
	if f.err != nil {
		return f.err
	}
	if add {
		entry := RouteEntry{Prefix: prefix, NextHop: nextHop}
		if isV6Prefix(prefix) {
			f.routes6[tableID] = append(f.routes6[tableID], entry)
		} else {
			f.routes[tableID] = append(f.routes[tableID], entry)
		}
	}
	return nil
}

// Routes 按协议返回（镜像 VPP：ip_route_dump 不指定 IsIP6 时只返回 v4）。
func (f *fakeL3) Routes(tableID uint32, isIP6 bool) ([]RouteEntry, error) {
	if f.err != nil {
		return nil, f.err
	}
	if isIP6 {
		return f.routes6[tableID], nil
	}
	return f.routes[tableID], nil
}

// isV6Prefix 判断前缀是否为 IPv6（含 ':' 即可）。
func isV6Prefix(prefix string) bool {
	for _, c := range prefix {
		if c == ':' {
			return true
		}
	}
	return false
}

func (f *fakeL3) BviCreate() (uint32, error) {
	f.bviCreated++
	if f.err != nil {
		return 0, f.err
	}
	f.nextBVI++
	return f.nextBVI, nil
}

func (f *fakeL3) BviDelete(swIfIndex uint32) error {
	if f.err != nil {
		return f.err
	}
	f.bviGone = append(f.bviGone, swIfIndex)
	return nil
}

func (f *fakeL3) BviSetBD(swIfIndex, bdID uint32) error {
	if f.err != nil {
		return f.err
	}
	f.bviBD[swIfIndex] = bdID
	return nil
}

func TestL3ApplyVRF(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	vrf := model.Vrf{Name: "vs-l3",
		L3Interfaces: []model.L3Interface{{Interface: "ens192", Addresses: []string{"10.0.0.1/24", "2001:db8::1/64"}}},
		Routes:       []model.Route{{Prefix: "0.0.0.0/0", NextHop: "10.0.0.254"}, {Prefix: "192.168.5.0/24", NextHop: "10.0.0.2"}}}
	if err := p.ApplyVRF(context.Background(), vrf); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	tid := TableID("vs-l3")
	if !f.tables[tid] {
		t.Fatalf("应建 IP table %d", tid)
	}
	if f.v4table[1] != tid {
		t.Fatalf("ens192 应置入 VRF 表: %v", f.v4table)
	}
	if len(f.addrs[1]) != 2 {
		t.Fatalf("应配 2 个地址: %v", f.addrs[1])
	}
	if len(f.routes[tid]) != 2 || f.routes[tid][0].Prefix != "0.0.0.0/0" {
		t.Fatalf("静态路由未下发: %+v", f.routes[tid])
	}

	rows, err := p.Routes(context.Background(), "vs-l3")
	if err != nil || len(rows) != 2 {
		t.Fatalf("Routes 运行态: %v %+v", err, rows)
	}
}

func TestL3VlanSubInterface(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	vrf := model.Vrf{Name: "vs-vlan", L3Interfaces: []model.L3Interface{
		{Interface: "ens192", Vlan: 100, Addresses: []string{"10.1.1.1/24"}}}}
	if err := p.ApplyVRF(context.Background(), vrf); err != nil {
		t.Fatalf("ApplyVRF(vlan): %v", err)
	}
	if f.v4table[101] != TableID("vs-vlan") {
		t.Fatalf("vlan 子接口应置入 VRF 表: %v", f.v4table)
	}

	// 删除应清子接口地址
	if err := p.DeleteVRF(context.Background(), "vs-vlan"); err != nil {
		t.Fatalf("DeleteVRF: %v", err)
	}
	if f.tables[TableID("vs-vlan")] {
		t.Fatalf("应删除 table")
	}
	if len(f.addrs[101]) != 0 {
		t.Fatalf("应清理子接口地址: %v", f.addrs[101])
	}
}

func TestL3Gateway(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	vs := model.VirtualSwitch{Name: "vs-app", Type: "l2",
		Gateway: &model.VSGateway{Addresses: []string{"192.168.100.1/24"}}}
	if err := p.ApplyGateway(context.Background(), vs); err != nil {
		t.Fatalf("ApplyGateway: %v", err)
	}
	if len(f.bviBD) != 1 {
		t.Fatalf("应创建 BVI 并挂入 BD: %v", f.bviBD)
	}
	for bvi, bd := range f.bviBD {
		if bd != BDID("vs-app") {
			t.Fatalf("BVI 应挂入 BD %d: %v", BDID("vs-app"), f.bviBD)
		}
		if len(f.addrs[bvi]) != 1 {
			t.Fatalf("BVI 应配地址: %v", f.addrs[bvi])
		}
		if !f.state[bvi] {
			t.Fatal("BVI 必须显式置为 up（VPP 默认 down，否则网关不可达）")
		}
		clearedBefore := false
		for _, idx := range f.cleared {
			if idx == bvi {
				clearedBefore = true
			}
		}
		if !clearedBefore {
			t.Fatal("置 VRF 前必须清理 BVI 旧地址（否则 VPP 报 -114）")
		}
		if f.v4table[bvi] != TableID(GatewayVRFName("vs-app")) {
			t.Fatalf("BVI 应置入专属 VRF: %v", f.v4table)
		}
	}
	gwTable := TableID(GatewayVRFName("vs-app"))
	if err := p.DeleteGateway(context.Background(), "vs-app"); err != nil {
		t.Fatalf("DeleteGateway: %v", err)
	}
	if len(f.bviGone) != 1 {
		t.Fatalf("应删除 BVI: %v", f.bviGone)
	}
	if f.tables[gwTable] {
		t.Fatalf("专属 VRF 表应删除")
	}
}

func TestL3Errors(t *testing.T) {
	f := newFakeL3()
	f.err = errors.New("boom")
	p := NewL3Provider(f)
	if err := p.ApplyVRF(context.Background(), model.Vrf{Name: "v"}); err == nil {
		t.Fatalf("建表错误应上抛")
	}
	if err := p.DeleteVRF(context.Background(), "v"); err == nil {
		t.Fatalf("删表错误应上抛")
	}
	if _, err := p.Routes(context.Background(), "v"); err == nil {
		t.Fatalf("路由查询错误应上抛")
	}
	if err := p.ApplyGateway(context.Background(), model.VirtualSwitch{Name: "x", Type: "l2",
		Gateway: &model.VSGateway{}}); err == nil {
		t.Fatalf("网关建表错误应上抛")
	}
	// 无 Gateway 时 ApplyGateway 为空操作
	f2 := newFakeL3()
	if err := NewL3Provider(f2).ApplyGateway(context.Background(), model.VirtualSwitch{Name: "y", Type: "l2"}); err != nil {
		t.Fatalf("无网关应跳过: %v", err)
	}
	// 不存在的接口
	if err := NewL3Provider(newFakeL3()).ApplyVRF(context.Background(),
		model.Vrf{Name: "v", L3Interfaces: []model.L3Interface{{Interface: "ens999"}}}); err == nil {
		t.Fatalf("接口缺失应报错")
	}
}

func TestTableIDAndDecorator(t *testing.T) {
	if TableID("a") != TableID("a") || TableID("a") == TableID("b") || TableID("") == 0 {
		t.Fatalf("TableID 应确定且非 0")
	}
	if !IsIPv6Prefix("2001:db8::1/64") || IsIPv6Prefix("10.0.0.1/24") {
		t.Fatalf("IsIPv6Prefix 判断错误")
	}

	// 装饰器：ApplyVRF 委派 L3；带 Gateway 的 L2 交换机先建 BD 再建 BVI
	l2f := newFakeL2()
	l3f := newFakeL3()
	n := NewL2Network(nil, NewL2Provider(l2f))
	n.SetL3(NewL3Provider(l3f))
	if err := n.ApplyVRF(context.Background(), model.Vrf{Name: "vs-l3",
		L3Interfaces: []model.L3Interface{{Interface: "ens192"}}}); err != nil {
		t.Fatalf("装饰器 ApplyVRF: %v", err)
	}
	vs := l2vs("vs-gw", model.VSwitchPort{Seq: 0, Interface: "ens192"})
	vs.Gateway = &model.VSGateway{Addresses: []string{"10.9.9.1/24"}}
	if err := n.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("装饰器 ApplyBridgeDomain(网关): %v", err)
	}
	if len(l3f.bviBD) != 1 {
		t.Fatalf("装饰器应建 BVI: %v", l3f.bviBD)
	}
	if err := n.DeleteBridgeDomain(context.Background(), "vs-gw"); err != nil {
		t.Fatalf("装饰器 DeleteBridgeDomain: %v", err)
	}
	if len(l3f.bviGone) != 1 {
		t.Fatalf("装饰器应删 BVI: %v", l3f.bviGone)
	}
	rows, err := n.Routes(context.Background(), "vs-l3")
	if err != nil {
		t.Fatalf("装饰器 Routes: %v", err)
	}
	if rows == nil {
		rows = []RouteEntry{}
	}
}

// TestL3GatewayReuseExistingBVI nfvisd 重启后（内存映射丢失、VPP 侧 BVI 仍在）
// 重放配置必须复用已有 BVI，不得重建挂 BD（否则 VPP 报 -152，恢复收敛失效）。
func TestL3GatewayReuseExistingBVI(t *testing.T) {
	f := newFakeL3()
	// 模拟 VPP 侧既有 BVI 挂在目标 BD 上（重启后 VPP 状态仍在）
	bd := BDID("vs-app")
	f.bviBD[900] = bd

	p := NewL3Provider(f)
	vs := model.VirtualSwitch{Name: "vs-app", Type: "l2",
		Gateway: &model.VSGateway{Addresses: []string{"10.10.0.1/24"}}}
	if err := p.ApplyGateway(context.Background(), vs); err != nil {
		t.Fatalf("重放 ApplyGateway: %v", err)
	}
	if f.bviCreated != 0 {
		t.Fatalf("应复用既有 BVI，实际调用了 BviCreate %d 次", f.bviCreated)
	}
	if !f.state[900] {
		t.Fatal("复用的 BVI 仍须置为 up")
	}
	if len(f.addrs[900]) != 1 || f.addrs[900][0] != "10.10.0.1/24" {
		t.Fatalf("复用的 BVI 须配地址: %v", f.addrs[900])
	}
}

// FR-NET-013（A-3，决策 #69）：VRF 运行态 FIB 必须同时包含 IPv4 与 IPv6 路由。
// 守护点：VPP 的 ip_route_dump 不显式传 IsIP6 时只返回 v4——曾经的缺陷是
// `show routes` / GET /vrfs/{n}/routes 看不到 v6 静态路由（v6 路由实际已下发）。
func TestL3RoutesIncludeIPv6(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	vrf := model.Vrf{Name: "vs-v6",
		L3Interfaces: []model.L3Interface{{Interface: "ens192", Addresses: []string{"2001:db8:155::1/64"}}},
		Routes: []model.Route{
			{Prefix: "0.0.0.0/0", NextHop: "10.0.0.254"},
			{Prefix: "2001:db8:aaaa::/64", NextHop: "2001:db8:155::2"},
			{Prefix: "::/0", NextHop: "2001:db8:155::2"},
		}}
	if err := p.ApplyVRF(context.Background(), vrf); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	rows, err := p.Routes(context.Background(), "vs-v6")
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Prefix] = r.NextHop
	}
	for _, want := range []string{"0.0.0.0/0", "2001:db8:aaaa::/64", "::/0"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("运行态 FIB 缺少 %s（实际 %v）", want, got)
		}
	}
	if got["2001:db8:aaaa::/64"] != "2001:db8:155::2" {
		t.Fatalf("v6 下一跳错误: %v", got)
	}
	// 不得因合并 v4/v6 而产生重复项
	if len(rows) != 3 {
		t.Fatalf("期望 3 条路由，实际 %d: %+v", len(rows), rows)
	}
}

// round84 缺陷修复：置入 L3 交换机的 vNIC 必须出现在 AttachedIfaces（NAT inside 解析来源），
// 否则它拿不到 nat44-ei-in2out 特性——配置与 show nat 全都正常而 guest 100% 不通。
// 同批修掉 R84-22 的顺序问题：置表前须幂等建表，否则「vNIC 接入先于同名 Vrf 条目」时报 -3。
func TestL3SetVnfTableRegistersAttachedIface(t *testing.T) {
	f := newFakeL3()
	f.ifaces["vh-vm-a-eth0"] = 5
	p := NewL3Provider(f)
	ctx := context.Background()
	if err := p.SetVnfTable(ctx, "vs-nat", "vh-vm-a-eth0"); err != nil {
		t.Fatalf("SetVnfTable: %v", err)
	}
	if !f.tables[TableID("vs-nat")] {
		t.Fatal("置表前必须幂等建表（否则先重放 vNIC 接入时报 -3 No such FIB / VRF）")
	}
	if f.v4table[5] != TableID("vs-nat") {
		t.Fatalf("vNIC 应置入 vs-nat 表: %v", f.v4table)
	}
	if got := p.AttachedIfaces("vs-nat"); len(got) != 1 || got[0] != 5 {
		t.Fatalf("vNIC 必须进 AttachedIfaces（NAT inside 来源）: %v", got)
	}
	// 另一张表不受影响
	if got := p.AttachedIfaces("vs-other"); len(got) != 0 {
		t.Fatalf("其它 VRF 不得含该 vNIC: %v", got)
	}

	// 幂等：同一 (表, 索引) 重复置表不再打 VPP（vNIC 可能就是该 VRF 的 l3-interface，已带地址，
	// 而 VPP 只允许无地址的接口换表）
	setTables := f.setTables
	if err := p.SetVnfTable(ctx, "vs-nat", "vh-vm-a-eth0"); err != nil {
		t.Fatalf("重复置表应幂等: %v", err)
	}
	if f.setTables != setTables {
		t.Fatalf("重复置表不应再下发: %d → %d", setTables, f.setTables)
	}

	// vNIC 同时是该 VRF 的 l3-interface（配了地址）时不重复登记
	vrf := model.Vrf{Name: "vs-nat", L3Interfaces: []model.L3Interface{
		{Interface: "vh-vm-a-eth0", Addresses: []string{"192.168.200.1/24"}}}}
	if err := p.ApplyVRF(ctx, vrf); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	if got := p.AttachedIfaces("vs-nat"); len(got) != 1 || got[0] != 5 {
		t.Fatalf("同索引不得重复登记: %v", got)
	}

	// 摘除：vNIC 从数据面移除后不得留在转发域里（残留索引会让下次 ApplyNAT 失败）
	p.ForgetVnfIface("vh-vm-a-eth0")
	if got := p.AttachedIfaces("vs-nat"); len(got) != 0 {
		t.Fatalf("摘除后不得再出现在 AttachedIfaces: %v", got)
	}
	// 摘除幂等（未登记的接口名同样安全）
	p.ForgetVnfIface("vh-vm-a-eth0")
	p.ForgetVnfIface("mf-ct-a-eth0")
}

// SwInterfaceTable 走 govpp（sw_interface_get_table）：请求带 sw_if_index 与协议，
// 应答的 vrf_id 是运行态事实——SetVnfTable 据此避免对带地址的口重复下发置表（VPP 报 -114）。
func TestL3GovppSwInterfaceTable(t *testing.T) {
	ch := &fakeAPIChannel{fill: func(msg api.Message) {
		if r, ok := msg.(*ifapi.SwInterfaceGetTableReply); ok {
			r.VrfID = 4242
		}
	}}
	tbl, known, err := (&govppL3Client{ch: ch}).SwInterfaceTable(5, true)
	if err != nil || !known || tbl != 4242 {
		t.Fatalf("运行态查表应为 (4242,true,nil)，实际 (%d,%v,%v)", tbl, known, err)
	}
	if len(ch.sent) != 1 {
		t.Fatalf("应下发 1 个请求: %d", len(ch.sent))
	}
	req, ok := ch.sent[0].(*ifapi.SwInterfaceGetTable)
	if !ok {
		t.Fatalf("请求类型不符: %T", ch.sent[0])
	}
	if uint32(req.SwIfIndex) != 5 || !req.IsIPv6 {
		t.Fatalf("请求字段不符: if=%d ip6=%v", req.SwIfIndex, req.IsIPv6)
	}
	// 非零 retval 必须上抛（不得把「答不出来」当成「在默认表」）
	bad := &fakeAPIChannel{fill: func(msg api.Message) {
		msg.(*ifapi.SwInterfaceGetTableReply).Retval = -5
	}}
	if _, _, err := (&govppL3Client{ch: bad}).SwInterfaceTable(5, false); err == nil {
		t.Fatal("非零 retval 应上抛")
	}
}

// 删除 VRF 时其 vNIC 登记一并清除（表已不存在，登记不再有意义）。
func TestL3DeleteVrfClearsVnfAttach(t *testing.T) {
	f := newFakeL3()
	f.ifaces["vh-vm-a-eth0"] = 5
	p := NewL3Provider(f)
	ctx := context.Background()
	if err := p.SetVnfTable(ctx, "vs-nat", "vh-vm-a-eth0"); err != nil {
		t.Fatalf("SetVnfTable: %v", err)
	}
	if err := p.DeleteVRF(ctx, "vs-nat"); err != nil {
		t.Fatalf("DeleteVRF: %v", err)
	}
	if got := p.AttachedIfaces("vs-nat"); len(got) != 0 {
		t.Fatalf("删 VRF 后不得残留 vNIC 登记: %v", got)
	}
}

// round84 缺陷 C：vNIC 同时是该 VRF 的 l3-interface（已带地址）时，登记失效后重建
// **不得**再下发置表——VPP 只允许无地址的接口换表，对带地址的口下发会报 -114，
// 重建就会失败，登记再也补不回来（缺陷 B 的根因形态之一）。
// 判据是运行态（sw_interface_get_table）：已在目标表则直接登记。
func TestL3SetVnfTableSkipsSetWhenAlreadyInTable(t *testing.T) {
	f := newFakeL3()
	f.ifaces["vh-vm-a-eth0"] = 5
	tableID := TableID("vs-nat")
	// VPP 侧该口已在 vs-nat 表里（v4/v6 都有），且带地址（ApplyVRF 配的 guest 网关）
	f.v4table[5], f.v6table[5] = tableID, tableID
	f.addrs[5] = []string{"192.168.200.1/24"}

	p := NewL3Provider(f)
	if err := p.SetVnfTable(context.Background(), "vs-nat", "vh-vm-a-eth0"); err != nil {
		t.Fatalf("SetVnfTable: %v", err)
	}
	if f.setTables != 0 {
		t.Fatalf("已在目标表的带地址口不得重复置表（VPP 会报 -114）: %d 次", f.setTables)
	}
	if got := p.AttachedIfaces("vs-nat"); len(got) != 1 || got[0] != 5 {
		t.Fatalf("本周转域登记必须重建: %v", got)
	}

	// 客户端答不出运行态查询时退回直接置表（不得因此报错、更不得漏登记）
	f2 := newFakeL3()
	f2.ifaces["vh-vm-a-eth0"] = 5
	f2.tableQueryUnavailable = true
	p2 := NewL3Provider(f2)
	if err := p2.SetVnfTable(context.Background(), "vs-nat", "vh-vm-a-eth0"); err != nil {
		t.Fatalf("答不出运行态时应退回直接置表: %v", err)
	}
	if f2.setTables != 2 || f2.v4table[5] != tableID || f2.v6table[5] != tableID {
		t.Fatalf("应下发 v4/v6 两次置表: %d %v %v", f2.setTables, f2.v4table, f2.v6table)
	}
	if got := p2.AttachedIfaces("vs-nat"); len(got) != 1 || got[0] != 5 {
		t.Fatalf("退回路径同样必须登记: %v", got)
	}

	// 查询出错不影响收敛：按「不知道」处理，退回直接置表并照常登记
	f3 := newFakeL3()
	f3.ifaces["vh-vm-a-eth0"] = 5
	f3.tableQueryErr = errors.New("timeout")
	p3 := NewL3Provider(f3)
	if err := p3.SetVnfTable(context.Background(), "vs-nat", "vh-vm-a-eth0"); err != nil {
		t.Fatalf("运行态查询失败应退回直接置表: %v", err)
	}
	if f3.setTables != 2 || f3.v4table[5] != tableID {
		t.Fatalf("退回路径应下发 v4/v6 两次置表: %d %v", f3.setTables, f3.v4table)
	}
	if got := p3.AttachedIfaces("vs-nat"); len(got) != 1 || got[0] != 5 {
		t.Fatalf("退回路径同样必须登记: %v", got)
	}
	// 查询失败 + 置表失败：错误里两条信息都要有（查询是优化，但失败要可诊断）
	f4 := newFakeL3()
	f4.ifaces["vh-vm-a-eth0"] = 5
	f4.tableQueryErr, f4.setTableErr = errors.New("timeout"), errors.New("boom")
	err := NewL3Provider(f4).SetVnfTable(context.Background(), "vs-nat", "vh-vm-a-eth0")
	if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("查询失败与置表失败都应出现在错误里: %v", err)
	}
}

// 登记重建（恢复收敛用，round84 收尾）：按**配置**把 L3 交换机三层接口解析并登记进
// ifaces（NAT inside 来源）与 ifaceTable（NAT outside 转发域来源），不依赖某次增量调用是否成功。
func TestRegisterL3InterfacesRebuildsFromConfig(t *testing.T) {
	f := newFakeL3()
	f.ifaces["vh-vm-a-eth0"] = 5
	p := NewL3Provider(f)
	ctx := context.Background()
	tid := TableID("vs-nat")
	vrf := model.Vrf{Name: "vs-nat", L3Interfaces: []model.L3Interface{
		{Interface: "ens192", Addresses: []string{"192.168.200.1/24"}},
		{Interface: "vh-vm-a-eth0", Addresses: []string{"192.168.200.1/24"}},
	}}

	if fails := p.RegisterL3Interfaces(ctx, vrf); len(fails) != 0 {
		t.Fatalf("应无失败项: %+v", fails)
	}
	if got := p.AttachedIfaces("vs-nat"); len(got) != 2 || got[0] != 1 || got[1] != 5 {
		t.Fatalf("L3 接口与 vNIC 都要进 inside 集合: %v", got)
	}
	for _, name := range []string{"ens192", "vh-vm-a-eth0"} {
		if tbl, ok := p.TableOfIface(name); !ok || tbl != tid {
			t.Fatalf("接口 %s 应登记所属表 %d: %d %v", name, tid, tbl, ok)
		}
	}

	// 幂等：重复重建不产生重复索引、不报错（每次收敛都会跑一遍）
	if fails := p.RegisterL3Interfaces(ctx, vrf); len(fails) != 0 {
		t.Fatalf("重复重建应无失败项: %+v", fails)
	}
	if got := p.AttachedIfaces("vs-nat"); len(got) != 2 {
		t.Fatalf("重复重建不得产生重复索引: %v", got)
	}

	// 解析不到的口：按未收敛上报（不静默丢），且不得凭空登记任何接口
	fails := p.RegisterL3Interfaces(ctx, model.Vrf{Name: "vs-ghost",
		L3Interfaces: []model.L3Interface{{Interface: "ens999"}}})
	if len(fails) != 1 || !errors.Is(fails[0].Err, ErrIfaceUnavailable) {
		t.Fatalf("接口缺失应标记 ErrIfaceUnavailable: %+v", fails)
	}
	if fails[0].Source != "vrfs/vs-ghost/ens999" {
		t.Fatalf("失败项来源应可归位: %q", fails[0].Source)
	}
	if got := p.AttachedIfaces("vs-ghost"); len(got) != 0 {
		t.Fatalf("解析不到的口不得进 inside: %v", got)
	}
	if _, ok := p.TableOfIface("ens999"); ok {
		t.Fatal("解析不到的口不得登记所属表")
	}
}

// vlan 子接口按 VPP 侧确定性名（<父口>.<sub_id>）解析，**绝不退回父口名**：
// 把父口登记成该 VRF 的三层接口会让 NAT inside 指错接口，而且全程无报错——比查不到更糟。
func TestRegisterL3InterfacesVlanSubif(t *testing.T) {
	f := newFakeL3()
	f.ifaces["ens192.100"] = 101 // create_subif 建出的子接口（VPP 自动命名）
	p := NewL3Provider(f)
	ctx := context.Background()

	// 写法一：配置名即子接口名
	v1 := model.Vrf{Name: "vs-vlan", L3Interfaces: []model.L3Interface{{Interface: "ens192.100"}}}
	if fails := p.RegisterL3Interfaces(ctx, v1); len(fails) != 0 {
		t.Fatalf("子接口应按名解析成功: %+v", fails)
	}
	if got := p.AttachedIfaces("vs-vlan"); len(got) != 1 || got[0] != 101 {
		t.Fatalf("登记的应是子接口索引: %v", got)
	}
	// 写法二：父口名 + vlan（两种写法归一）
	v2 := model.Vrf{Name: "vs-vlan2", L3Interfaces: []model.L3Interface{{Interface: "ens192", Vlan: 100}}}
	if fails := p.RegisterL3Interfaces(ctx, v2); len(fails) != 0 {
		t.Fatalf("父口名 + vlan 应解析到子接口: %+v", fails)
	}
	if got := p.AttachedIfaces("vs-vlan2"); len(got) != 1 || got[0] != 101 {
		t.Fatalf("登记的应是子接口索引而非父口: %v", got)
	}

	// 子接口在运行态查不到：上报未收敛，父口（2）绝不能被登记
	v3 := model.Vrf{Name: "vs-vlan3", L3Interfaces: []model.L3Interface{{Interface: "ens224", Vlan: 100}}}
	fails := p.RegisterL3Interfaces(ctx, v3)
	if len(fails) != 1 || !errors.Is(fails[0].Err, ErrIfaceUnavailable) {
		t.Fatalf("子接口不存在应上报未收敛: %+v", fails)
	}
	if got := p.AttachedIfaces("vs-vlan3"); len(got) != 0 {
		t.Fatalf("不得退回父口登记: %v", got)
	}
}
