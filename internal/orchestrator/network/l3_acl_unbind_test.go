package network

// 决策 #361：L3 接口 ACL 绑定的撤销路径（UnbindL3IfaceACL / DeleteL3Interface / DeleteVRF 增补）。
//
// 缺陷形态（round142 真机）：`delete … l3-interface <if> acl-in <acl>`（只清绑定）或整条删
// l3-interface 提交成功、读视图干净，而 VPP 侧 `show acl-plugin interface` 仍有 input acl(s)
// （deny 继续拦）、删 ACL 后伴随 macip 残留、口留原表且地址不摘。根因是 BindIndex 的
// 「空绑定＝解绑」分支没有生产调用者，且没有任何路径回收单条 l3-interface。

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"go.fd.io/govpp/api"

	"github.com/xzjt/nfvis/internal/model"
)

// scriptedL3 在 fakeL3 之上记录调用序并按需注入指定步骤的失败（调用序与 -2 容错断言用）。
// 嵌入保证其余方法语义与既有 fakeL3 完全一致。
type scriptedL3 struct {
	*fakeL3
	log         *[]string
	addrDelErr  error // 清地址时返回的错误（-2 容错用例）
	setTableErr error // 置表时返回的错误（-2 容错用例）
}

func (s *scriptedL3) SwInterfaceAddDelAddress(swIfIndex uint32, prefix string, add, delAll bool) error {
	if delAll {
		*s.log = append(*s.log, "addr-clear:"+strconv.Itoa(int(swIfIndex)))
		if s.addrDelErr != nil {
			return s.addrDelErr
		}
	}
	return s.fakeL3.SwInterfaceAddDelAddress(swIfIndex, prefix, add, delAll)
}

func (s *scriptedL3) SwInterfaceSetTable(swIfIndex uint32, isIP6 bool, tableID uint32) error {
	if s.setTableErr != nil {
		return s.setTableErr
	}
	if tableID == 0 { // 只记「移回默认表」，ApplyVRF 的置入目标表不入日志
		proto := "v4"
		if isIP6 {
			proto = "v6"
		}
		*s.log = append(*s.log, "set-default:"+proto+":"+strconv.Itoa(int(swIfIndex)))
	}
	return s.fakeL3.SwInterfaceSetTable(swIfIndex, isIP6, tableID)
}

// notingAcl 在 fakeAcl 之上记录绑定调用（跨两类 client 的调用序断言用）。
type notingAcl struct {
	*fakeAcl
	log *[]string
}

func (n *notingAcl) ACLInterfaceSet(swIfIndex, inAcl, outAcl uint32, inSet, outSet bool) error {
	*n.log = append(*n.log, "acl-set:"+strconv.Itoa(int(swIfIndex)))
	return n.fakeAcl.ACLInterfaceSet(swIfIndex, inAcl, outAcl, inSet, outSet)
}

func (n *notingAcl) MacipACLInterfaceAddDel(swIfIndex, aclIndex uint32, isAdd bool) error {
	verb := "unbind"
	if isAdd {
		verb = "bind"
	}
	*n.log = append(*n.log, "macip-"+verb+":"+strconv.Itoa(int(swIfIndex)))
	return n.fakeAcl.MacipACLInterfaceAddDel(swIfIndex, aclIndex, isAdd)
}

func sameSeq(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// l3WithAcl 装配一个 L3 provider 与一个注入的 ACL provider（返回各自的 fake 以便断言）。
func l3WithAcl(l3c L3Client, aclc ACLClient) (*L3Provider, *AclProvider) {
	ap := NewAclProvider(aclc)
	p := NewL3Provider(l3c)
	p.SetACL(ap)
	return p, ap
}

// 单接口回收的完整调用序：清地址 → 解绑 IP ACL → 解绑伴随 macip → 移回默认表（v4/v6）→ 摘登记。
func TestDeleteL3InterfaceFullSequence(t *testing.T) {
	base := newFakeL3()
	var log []string
	af := newFakeAcl()
	p, aclP := l3WithAcl(&scriptedL3{fakeL3: base, log: &log}, &notingAcl{fakeAcl: af, log: &log})
	ctx := context.Background()
	li := model.L3Interface{Interface: "ens192", Addresses: []string{"10.0.0.1/24"}, AclIn: "web"}
	if err := aclP.ApplyACL(ctx, model.Acl{Name: "web"}); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	if err := p.ApplyVRF(ctx, model.Vrf{Name: "vs-l3", L3Interfaces: []model.L3Interface{li}}); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	// 前置：绑定与伴随 macip 都已下发，接口在该表里
	if len(af.setCalls) != 1 || af.setCalls[0] != [3]uint32{1, 100, 0} {
		t.Fatalf("前置绑定未下发: %v", af.setCalls)
	}
	if len(af.macipBind) != 1 || af.macipBind[0][2] != 1 {
		t.Fatalf("前置伴随 macip 未绑定: %v", af.macipBind)
	}
	if base.v4table[1] != TableID("vs-l3") || base.v6table[1] != TableID("vs-l3") {
		t.Fatalf("前置：接口应在该表里: %v %v", base.v4table, base.v6table)
	}
	log = nil // 只断言回收路径的调用序

	if err := p.DeleteL3Interface(ctx, "vs-l3", li); err != nil {
		t.Fatalf("DeleteL3Interface: %v", err)
	}
	want := []string{"addr-clear:1", "acl-set:1", "macip-unbind:1", "set-default:v4:1", "set-default:v6:1"}
	if !sameSeq(log, want) {
		t.Fatalf("调用序不符:\n got %v\nwant %v", log, want)
	}
	if got := af.setCalls[len(af.setCalls)-1]; got != [3]uint32{1, 0, 0} {
		t.Fatalf("IP ACL 绑定应被清空: %v", af.setCalls)
	}
	if got := af.macipBind[len(af.macipBind)-1]; got[0] != 1 || got[2] != 0 {
		t.Fatalf("伴随 macip 应被解绑: %v", af.macipBind)
	}
	if base.v4table[1] != 0 || base.v6table[1] != 0 {
		t.Fatalf("接口应移回默认表（v4/v6）: %v %v", base.v4table, base.v6table)
	}
	if len(base.addrs[1]) != 0 {
		t.Fatalf("接口地址应清掉: %v", base.addrs[1])
	}
	if got := p.AttachedIfaces("vs-l3"); len(got) != 0 {
		t.Fatalf("转发域登记应被摘除: %v", got)
	}
	if _, ok := p.TableOfIface("ens192"); ok {
		t.Fatal("所属表登记应被摘除")
	}
	if _, ok := p.ForwardDomainOf(1); ok {
		t.Fatal("转发域归属登记应被摘除")
	}
}

// 接口已不存在（-2）：清地址/解绑/置表全项容错，登记仍必须清干净（否则留下死索引）。
func TestDeleteL3InterfaceMissingIfaceTolerated(t *testing.T) {
	base := newFakeL3()
	var log []string
	af := newFakeAcl()
	l3c := &scriptedL3{fakeL3: base, log: &log}
	p, aclP := l3WithAcl(l3c, &notingAcl{fakeAcl: af, log: &log})
	ctx := context.Background()
	li := model.L3Interface{Interface: "ens192", Addresses: []string{"10.0.0.1/24"}, AclIn: "web"}
	if err := aclP.ApplyACL(ctx, model.Acl{Name: "web"}); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	if err := p.ApplyVRF(ctx, model.Vrf{Name: "vs-l3", L3Interfaces: []model.L3Interface{li}}); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	// 接口已从 VPP 消失（按名查不到），登记回退仍给出索引——随后每个数据面动作都撞 -2。
	delete(base.ifaces, "ens192")
	l3c.addrDelErr = api.VPPApiError(-2)
	l3c.setTableErr = api.VPPApiError(-2)
	af.setErr = api.VPPApiError(-2) // 解绑 IP ACL 也撞 -2（绑定随接口消失，属已达成）

	if err := p.DeleteL3Interface(ctx, "vs-l3", li); err != nil {
		t.Fatalf("接口已不存在时全项容错（-2 按已达成），不得报错: %v", err)
	}
	if got := p.AttachedIfaces("vs-l3"); len(got) != 0 {
		t.Fatalf("数据面全撞 -2 时登记仍要摘除: %v", got)
	}
	if _, ok := p.TableOfIface("ens192"); ok {
		t.Fatal("按名登记仍要摘除")
	}
	if _, ok := p.ForwardDomainOf(1); ok {
		t.Fatal("转发域归属登记仍要摘除")
	}
	aclP.mu.Lock()
	_, hadPair := aclP.bound[1]
	_, hadMacip := aclP.macipBound[1]
	aclP.mu.Unlock()
	if hadPair || hadMacip {
		t.Fatalf("-2 解绑后 ACL 侧登记应清空: bound=%v macip=%v", hadPair, hadMacip)
	}
}

// 名查不到且登记也没有 = 接口不在数据面：已达成、不产生任何数据面动作；
// 查询本身失败必须上抛（不许把「问不出来」当成「已达成」）。
func TestDeleteL3InterfaceAbsentAndQueryError(t *testing.T) {
	base := newFakeL3()
	p := NewL3Provider(base)
	ctx := context.Background()
	li := model.L3Interface{Interface: "ens999", Addresses: []string{"10.9.9.1/24"}, AclIn: "web"}
	if err := p.DeleteL3Interface(ctx, "vs-ghost", li); err != nil {
		t.Fatalf("接口与登记都不在应按已达成: %v", err)
	}
	if base.setTables != 0 || len(base.cleared) != 0 || len(base.setTableIdx) != 0 {
		t.Fatalf("不应产生数据面动作: setTables=%d cleared=%v", base.setTables, base.cleared)
	}

	be := newFakeL3()
	be.err = errors.New("boom")
	if err := NewL3Provider(be).DeleteL3Interface(ctx, "vs-l3", li); err == nil ||
		!strings.Contains(err.Error(), "boom") {
		t.Fatalf("解析失败应上抛: %v", err)
	}
	if err := NewL3Provider(be).UnbindL3IfaceACL(ctx, "vs-l3", li); err == nil ||
		!strings.Contains(err.Error(), "boom") {
		t.Fatalf("解绑路径的解析失败同样应上抛: %v", err)
	}
}

// vlan 子接口同样可回收：清子接口地址、移回默认表、摘 ifaces/subifs 两处登记。
func TestDeleteL3InterfaceVlanSubif(t *testing.T) {
	base := newFakeL3()
	var log []string
	l3c := &scriptedL3{fakeL3: base, log: &log}
	p, _ := l3WithAcl(l3c, newFakeAcl())
	ctx := context.Background()
	li := model.L3Interface{Interface: "ens192", Vlan: 100, Addresses: []string{"10.1.1.1/24"}}
	if err := p.ApplyVRF(ctx, model.Vrf{Name: "vs-vlan", L3Interfaces: []model.L3Interface{li}}); err != nil {
		t.Fatalf("ApplyVRF(vlan): %v", err)
	}
	if base.v4table[101] != TableID("vs-vlan") || len(base.addrs[101]) != 1 {
		t.Fatalf("前置：子接口应在表里并带地址: %v %v", base.v4table, base.addrs)
	}
	if err := p.DeleteL3Interface(ctx, "vs-vlan", li); err != nil {
		t.Fatalf("DeleteL3Interface(vlan): %v", err)
	}
	if base.v4table[101] != 0 || base.v6table[101] != 0 {
		t.Fatalf("子接口应移回默认表: %v %v", base.v4table, base.v6table)
	}
	if len(base.addrs[101]) != 0 {
		t.Fatalf("子接口地址应清掉: %v", base.addrs[101])
	}
	if got := p.AttachedIfaces("vs-vlan"); len(got) != 0 {
		t.Fatalf("子接口的登记应摘除（ifaces/subifs 两处）: %v", got)
	}
	if _, ok := p.TableOfIface("ens192"); ok {
		t.Fatal("按名登记应摘除")
	}
}

// vNIC 作 L3 接口（#172 受支持形态，用户手册 §8.9）：删 l3-interface 叶子只撤地址与绑定，
// **不得把口移回默认表**——vNIC 侧声明仍在，口要继续留在该 VRF 的转发域里；登记也保持
// （vnfs 由 vNIC 侧维护，误摘会让 guest 转发域与 NAT 解析同时失去依据）。
func TestDeleteL3InterfaceKeepsTableForVnicOwner(t *testing.T) {
	base := newFakeL3()
	base.ifaces["vh-vm-nic0"] = 300
	var log []string
	af := newFakeAcl()
	p, aclP := l3WithAcl(&scriptedL3{fakeL3: base, log: &log}, &notingAcl{fakeAcl: af, log: &log})
	ctx := context.Background()
	if err := aclP.ApplyACL(ctx, model.Acl{Name: "web"}); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	li := model.L3Interface{Interface: "vh-vm-nic0", Addresses: []string{"192.168.200.1/24"}, AclIn: "web"}
	if err := p.ApplyVRF(ctx, model.Vrf{Name: "vs-nat", L3Interfaces: []model.L3Interface{li}}); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	if err := p.SetVnfTable(ctx, "vs-nat", "vh-vm-nic0"); err != nil { // vNIC 侧声明（同表）
		t.Fatalf("SetVnfTable: %v", err)
	}
	log = nil

	if err := p.DeleteL3Interface(ctx, "vs-nat", li); err != nil {
		t.Fatalf("DeleteL3Interface: %v", err)
	}
	// 地址清、绑定撤，照常
	if len(base.addrs[300]) != 0 {
		t.Fatalf("地址应撤掉: %v", base.addrs[300])
	}
	if got := af.setCalls[len(af.setCalls)-1]; got != [3]uint32{300, 0, 0} {
		t.Fatalf("IP ACL 应解绑（登记仍在）: %v", af.setCalls)
	}
	if got := af.macipBind[len(af.macipBind)-1]; got[0] != 300 || got[2] != 0 {
		t.Fatalf("伴随 macip 应解绑: %v", af.macipBind)
	}
	// 表归属与登记保持（vNIC 声明仍在）
	if base.v4table[300] != TableID("vs-nat") || base.v6table[300] != TableID("vs-nat") {
		t.Fatalf("vNIC 仍声明在该表：不得移回默认表: %v %v", base.v4table, base.v6table)
	}
	for _, e := range log {
		if strings.HasPrefix(e, "set-default:") {
			t.Fatalf("不应产生移回默认表的调用: %v", log)
		}
	}
	if got := p.AttachedIfaces("vs-nat"); len(got) != 1 || got[0] != 300 {
		t.Fatalf("vNIC 转发域登记应保持: %v", got)
	}
	if !p.vnicHoldsTable("vs-nat", "vh-vm-nic0") {
		t.Fatal("vnfs 登记应保持（vNIC 声明是表归属的来源）")
	}
}

// UnbindL3IfaceACL：登记在 ⇒ 解绑 IP ACL 与伴随 macip；登记不在 ⇒ 空操作（不产生 VPP 调用）；
// 未注入 ACL 编排 / 接口不可解析 ⇒ 空操作返回 nil。
func TestUnbindL3IfaceACL(t *testing.T) {
	base := newFakeL3()
	af := newFakeAcl()
	p, aclP := l3WithAcl(base, af)
	ctx := context.Background()
	li := model.L3Interface{Interface: "ens192", Addresses: []string{"10.0.0.1/24"}, AclIn: "web"}
	if err := aclP.ApplyACL(ctx, model.Acl{Name: "web"}); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	if err := p.ApplyVRF(ctx, model.Vrf{Name: "vs-l3", L3Interfaces: []model.L3Interface{li}}); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	if err := p.UnbindL3IfaceACL(ctx, "vs-l3", li); err != nil {
		t.Fatalf("UnbindL3IfaceACL: %v", err)
	}
	if got := af.setCalls[len(af.setCalls)-1]; got != [3]uint32{1, 0, 0} {
		t.Fatalf("应解绑 IP ACL: %v", af.setCalls)
	}
	if got := af.macipBind[len(af.macipBind)-1]; got[0] != 1 || got[2] != 0 {
		t.Fatalf("应解绑伴随 macip: %v", af.macipBind)
	}

	// 登记不在（全新的 ACL 编排）：空操作，不产生任何 ACL VPP 调用
	af2 := newFakeAcl()
	p2, _ := l3WithAcl(base, af2)
	if err := p2.UnbindL3IfaceACL(ctx, "vs-l3", model.L3Interface{Interface: "ens192"}); err != nil {
		t.Fatalf("无登记解绑应为空操作: %v", err)
	}
	if len(af2.setCalls) != 0 || len(af2.macipBind) != 0 || len(af2.macipCreated) != 0 {
		t.Fatalf("无登记不得产生 ACL VPP 调用: %v %v", af2.setCalls, af2.macipBind)
	}

	// 接口名与登记都查不到：已达成
	if err := p2.UnbindL3IfaceACL(ctx, "vs-l3", model.L3Interface{Interface: "ens999"}); err != nil {
		t.Fatalf("接口不在数据面应已达成: %v", err)
	}

	// 未注入 ACL 编排：空操作（与 ApplyVRF 绑定侧对称）
	if err := NewL3Provider(base).UnbindL3IfaceACL(ctx, "vs-l3", li); err != nil {
		t.Fatalf("无 ACL 编排应为空操作: %v", err)
	}
}

// DeleteVRF 增补：删整台交换机时，其 L3 接口的 IP ACL 与伴随 macip 一并解绑
// （此前只清地址/表，绑定会残留在已回默认表的接口上，deny 直到 VPP 重启才消失）。
func TestDeleteVrfUnbindsL3IfaceACL(t *testing.T) {
	base := newFakeL3()
	af := newFakeAcl()
	p, aclP := l3WithAcl(base, af)
	ctx := context.Background()
	li := model.L3Interface{Interface: "ens192", Addresses: []string{"10.0.0.1/24"}, AclIn: "web"}
	if err := aclP.ApplyACL(ctx, model.Acl{Name: "web"}); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	if err := p.ApplyVRF(ctx, model.Vrf{Name: "vs-l3", L3Interfaces: []model.L3Interface{li}}); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	if len(af.setCalls) != 1 || len(af.macipBind) != 1 || af.macipBind[0][2] != 1 {
		t.Fatalf("前置：绑定与伴随 macip 应已下发: %v %v", af.setCalls, af.macipBind)
	}
	if err := p.DeleteVRF(ctx, "vs-l3"); err != nil {
		t.Fatalf("DeleteVRF: %v", err)
	}
	if got := af.setCalls[len(af.setCalls)-1]; got != [3]uint32{1, 0, 0} {
		t.Fatalf("删交换机必须解绑接口的 IP ACL: %v", af.setCalls)
	}
	if got := af.macipBind[len(af.macipBind)-1]; got[0] != 1 || got[2] != 0 {
		t.Fatalf("伴随 macip 应一并解绑: %v", af.macipBind)
	}
	if base.v4table[1] != 0 || base.v6table[1] != 0 {
		t.Fatalf("接口应回默认表: %v %v", base.v4table, base.v6table)
	}
	if base.tables[TableID("vs-l3")] {
		t.Fatal("表应被删除（ACL 解绑不得挡住删除路径）")
	}
	if got := p.AttachedIfaces("vs-l3"); len(got) != 0 {
		t.Fatalf("删 VRF 后登记不得残留: %v", got)
	}
}

// 决策 #361：L2Network 未注入 L3 编排时两个新方法为空操作（noop/无 VPP 路径），
// 与 ApplyVRF/DeleteVRF 的早退口径一致——接口与转发实现必须同步（编译期由接口方法集强制）。
func TestL2NetworkL3RevokeWithoutL3(t *testing.T) {
	n := NewL2Network(nil, nil)
	li := model.L3Interface{Interface: "ens192", AclIn: "web"}
	ctx := context.Background()
	if err := n.UnbindL3IfaceACL(ctx, "vs-l3", li); err != nil {
		t.Fatalf("未注入 L3 时解绑应为空操作: %v", err)
	}
	if err := n.DeleteL3Interface(ctx, "vs-l3", li); err != nil {
		t.Fatalf("未注入 L3 时回收应为空操作: %v", err)
	}
}
