package network

import (
	"context"
	"errors"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// ---------- M3-4：L3/VRF/BVI（FR-NET-013/014）单测（假 L3Client） ----------

type fakeL3 struct {
	ifaces  map[string]uint32
	nextSub uint32
	nextBVI uint32

	tables     map[uint32]bool
	v4table    map[uint32]uint32 // swIfIndex → v4 table
	addrs      map[uint32][]string
	routes     map[uint32][]RouteEntry
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
		tables: map[uint32]bool{}, v4table: map[uint32]uint32{},
		addrs: map[uint32][]string{}, routes: map[uint32][]RouteEntry{}, bviBD: map[uint32]uint32{},
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

func (f *fakeL3) SwInterfaceSetTable(swIfIndex uint32, isIP6 bool, tableID uint32) error {
	if f.err != nil {
		return f.err
	}
	if !isIP6 {
		f.v4table[swIfIndex] = tableID
	}
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
		f.routes[tableID] = append(f.routes[tableID], RouteEntry{Prefix: prefix, NextHop: nextHop})
	}
	return nil
}

func (f *fakeL3) Routes(tableID uint32) ([]RouteEntry, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.routes[tableID], nil
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
