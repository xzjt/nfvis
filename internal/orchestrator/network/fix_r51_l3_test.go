package network

// round51 评审缺陷收口（N1/N2/N4/N6）的守护用例。
//
// 每条用例的注释写明**修复前的可观测症状**（真机形态），断言按该症状写——
// 不是按实现细节写：假实现已按「真机实际返回码」建模（fakeL3.CreateSubif 撞 -56、
// fakeL3.DeleteSubif 对非子接口回 -2、表读回等），故用例在修复前必须是红的。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// hideSubifLookupOnce 只在**第一次**按名查该子接口时答「不存在」，之后照常委派。
// 用来逼出 resolveL3Iface 的 -56 幂等兜底分支：第一次查不到 ⇒ 去建 ⇒ VPP 回 -56
// （已存在）⇒ 再按名读回成功。真机成因是查询与建接口之间的时序/客户端应答差异。
type hideSubifLookupOnce struct {
	*fakeL3
	name  string
	hidde int
}

func (h *hideSubifLookupOnce) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	if ifname == h.name && h.hidde == 0 {
		h.hidde++
		return 0, false, nil
	}
	return h.fakeL3.SwInterfaceIndex(ifname)
}

// ---------- N1：L3 的 vlan 子接口创建必须幂等 ----------

// 修复前症状：`ApplyVRF` 无条件 `create_subif`，而真机对已存在的子接口回 `-56`
// （Value already exists，与 L2 侧 round88 同一返回码）⇒ 任何一次触碰该 VRF 的提交
// （改路由/加地址/再加一个 l3-interface）整次回滚；nfvisd 重启后的恢复重放（recovery.go
// 对每台 VRF 都调 ApplyVRF）必然失败，该 VRF 永久未收敛。
func TestFixR51N1VlanSubifIdempotentOnReplay(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	ctx := context.Background()
	vrf := model.Vrf{Name: "vs-vlan", L3Interfaces: []model.L3Interface{
		{Interface: "ens192", Vlan: 100, Addresses: []string{"10.1.1.1/24"}}},
		Routes: []model.Route{{Prefix: "0.0.0.0/0", NextHop: "10.1.1.254"}}}

	if err := p.ApplyVRF(ctx, vrf); err != nil {
		t.Fatalf("首次 ApplyVRF: %v", err)
	}
	sub, ok := f.ifaces["ens192.100"]
	if !ok {
		t.Fatalf("前置：子接口应按 VPP 命名规则 <父口>.<vlan> 建出: %v", f.ifaces)
	}

	// 重放（真机 = 恢复收敛/任何一次触碰本 VRF 的提交）
	if err := p.ApplyVRF(ctx, vrf); err != nil {
		t.Fatalf("重复收敛必须幂等（真机 create_subif 对已存在的子接口回 -56）: %v", err)
	}
	if f.ifaces["ens192.100"] != sub {
		t.Fatalf("必须复用同一个子接口（另建一个会把地址/绑定留在旧口上）: %v", f.ifaces)
	}
	tid := TableID("vs-vlan")
	if f.v4table[sub] != tid || f.v6table[sub] != tid {
		t.Fatalf("子接口应仍在本 VRF 表内: v4=%d v6=%d", f.v4table[sub], f.v6table[sub])
	}
	if len(f.addrs[sub]) != 1 || f.addrs[sub][0] != "10.1.1.1/24" {
		t.Fatalf("地址应被重放配回: %v", f.addrs[sub])
	}
	if len(f.routes[tid]) != 1 {
		t.Fatalf("路由应被重放: %v", f.routes[tid])
	}
}

// -56 的**兜底**分支：按名查不到（时序/客户端答不出）时去建，撞 -56 后必须再按名读回复用，
// 而不是把 -56 当成硬错把整条 VRF 判失败。
func TestFixR51N1VlanSubifToleratesExistsError(t *testing.T) {
	base := newFakeL3()
	// 前置：VPP 里已有该子接口（上一次重放建的），索引 201。
	base.nextSub = 200
	base.ifaces["ens192.100"] = 201
	base.subifIdx[201] = true
	l3c := &hideSubifLookupOnce{fakeL3: base, name: "ens192.100"}
	p := NewL3Provider(l3c)
	ctx := context.Background()

	vrf := model.Vrf{Name: "vs-vlan", L3Interfaces: []model.L3Interface{
		{Interface: "ens192", Vlan: 100, Addresses: []string{"10.1.1.1/24"}}}}
	if err := p.ApplyVRF(ctx, vrf); err != nil {
		t.Fatalf("create_subif 撞 -56（已存在）后必须按名读回复用，不得让整条 VRF 失败: %v", err)
	}
	if l3c.hidde == 0 {
		t.Fatal("前置失效：本次没有走到「按名查不到」的路径，-56 兜底未被覆盖")
	}
	if base.v4table[201] != TableID("vs-vlan") {
		t.Fatalf("复用的应是既有子接口 201: %v", base.v4table)
	}
	if got := p.AttachedIfaces("vs-vlan"); len(got) != 1 || got[0] != 201 {
		t.Fatalf("复用路径同样要登记转发域成员: %v", got)
	}
}

// 修复前症状之二：解析失败发生在逐接口的「清地址 → 置表」**之间**——前面几个接口已经被
// 剥掉地址、换过表，坏接口才报错 ⇒ 整次提交回滚而数据面停在半路。现在先把声明里每个接口
// 都解析出来再动数据面：解析不了就分毫不动。
func TestFixR51N1ResolveAllBeforeMutating(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	ctx := context.Background()
	err := p.ApplyVRF(ctx, model.Vrf{Name: "vs-mix", L3Interfaces: []model.L3Interface{
		{Interface: "ens192", Addresses: []string{"10.0.0.1/24"}}, // 存在
		{Interface: "ens999", Addresses: []string{"10.0.0.2/24"}}, // 不在数据面
	}})
	if err == nil || !errors.Is(err, ErrIfaceUnavailable) {
		t.Fatalf("含解析不了的接口应如实失败: %v", err)
	}
	if len(f.cleared) != 0 || f.setTables != 0 {
		t.Fatalf("解析阶段失败前不得改动数据面（旧实现已把前一个口剥空）: cleared=%v setTables=%d",
			f.cleared, f.setTables)
	}
	if got := p.AttachedIfaces("vs-mix"); len(got) != 0 {
		t.Fatalf("失败的收敛不得留下半套登记: %v", got)
	}
}

// ---------- N2：L3 的 vlan 子接口必须有回收路径 ----------

// 修复前症状：`delete virtual-switches <n> l3-interface <if> vlan <v>` 只清地址/解绑/移回
// 默认表，**从不 DeleteSubif** ⇒ `<父口>.<vlan>` 长期留在数据面（`show interfaces physical`
// 标「未声明」），并占着同一个名字使「删掉再加同一 vlan 接口」必然撞 -56。
func TestFixR51N2DeleteL3InterfaceReclaimsSubif(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	ctx := context.Background()
	li := model.L3Interface{Interface: "ens192", Vlan: 100, Addresses: []string{"10.1.1.1/24"}}
	if err := p.ApplyVRF(ctx, model.Vrf{Name: "vs-vlan", L3Interfaces: []model.L3Interface{li}}); err != nil {
		t.Fatalf("前置 ApplyVRF: %v", err)
	}
	sub, ok := f.ifaces["ens192.100"]
	if !ok {
		t.Fatalf("前置：子接口应已建出: %v", f.ifaces)
	}
	if err := p.DeleteL3Interface(ctx, "vs-vlan", li); err != nil {
		t.Fatalf("DeleteL3Interface: %v", err)
	}
	if !fixed51Contains(f.deletedSubifs, sub) {
		t.Fatalf("自建子接口必须回收（否则数据面长期挂「未声明」的 <父口>.<vlan>）: deleted=%v", f.deletedSubifs)
	}
	if _, ok := f.ifaces["ens192.100"]; ok {
		t.Fatalf("子接口应已从数据面消失: %v", f.ifaces)
	}
	// 回收后同名可重建（此前必然撞 -56）——「删掉再加同一 vlan 接口」必须能走通
	if err := p.ApplyVRF(ctx, model.Vrf{Name: "vs-vlan", L3Interfaces: []model.L3Interface{li}}); err != nil {
		t.Fatalf("回收后重新加同一 vlan 接口不应撞 -56: %v", err)
	}
}

// 删整台 L3 交换机同样回收自建子接口；子接口已不存在（带外删过/父口已下线）时按 -2 容忍，
// 不得因为一个已消失的子接口阻断删表。
func TestFixR51N2DeleteVrfReclaimsSubif(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	ctx := context.Background()
	if err := p.ApplyVRF(ctx, model.Vrf{Name: "vs-vlan", L3Interfaces: []model.L3Interface{
		{Interface: "ens192", Vlan: 100}}}); err != nil {
		t.Fatalf("前置 ApplyVRF: %v", err)
	}
	sub := f.ifaces["ens192.100"]
	if err := p.DeleteVRF(ctx, "vs-vlan"); err != nil {
		t.Fatalf("DeleteVRF: %v", err)
	}
	if !fixed51Contains(f.deletedSubifs, sub) {
		t.Fatalf("删整台交换机也要回收自建子接口: deleted=%v", f.deletedSubifs)
	}
	if f.tables[TableID("vs-vlan")] {
		t.Fatal("表应已删除")
	}

	// -2 容错：子接口在数据面已不存在
	f2 := newFakeL3()
	p2 := NewL3Provider(f2)
	if err := p2.ApplyVRF(ctx, model.Vrf{Name: "vs-vlan2", L3Interfaces: []model.L3Interface{
		{Interface: "ens192", Vlan: 100}}}); err != nil {
		t.Fatalf("前置 ApplyVRF: %v", err)
	}
	delete(f2.subifIdx, f2.ifaces["ens192.100"]) // 模拟「子接口已不在数据面」
	if err := p2.DeleteVRF(ctx, "vs-vlan2"); err != nil {
		t.Fatalf("子接口已不存在时按已达成（-2 容错），不得阻断删表: %v", err)
	}
}

// 非 vlan 的 l3-interface（物理口）**不得**被回收成子接口：判据只在声明形态为 vlan 子接口时成立。
func TestFixR51N2PhysicalIfaceNotTouched(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	ctx := context.Background()
	li := model.L3Interface{Interface: "ens192", Addresses: []string{"10.0.0.1/24"}}
	if err := p.ApplyVRF(ctx, model.Vrf{Name: "vs-phys", L3Interfaces: []model.L3Interface{li}}); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	if err := p.DeleteL3Interface(ctx, "vs-phys", li); err != nil {
		t.Fatalf("DeleteL3Interface: %v", err)
	}
	if len(f.deletedSubifs) != 0 {
		t.Fatalf("物理口不是子接口，不得删: %v", f.deletedSubifs)
	}
	if f.ifaces["ens192"] != 1 {
		t.Fatalf("物理口应仍在数据面: %v", f.ifaces)
	}
}

// ---------- N6：带点写法的 l3-interface（`ens224.100`）等价于「父口 + vlan」 ----------

// 修复前症状：tree 允许 `l3-interface ens224.100`（校验层剥掉点后按父口名接受），而
// resolveL3Iface 只在 `li.Vlan > 0` 时建子接口、否则按名直查 ⇒ 带点写法既没有 Vlan 也没有
// 同名接口，报 ErrIfaceUnavailable 并把原因归到 DPDK 绑定上（**指错方向**）。
func TestFixR51N6DottedIfaceNameCreatesSubif(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	ctx := context.Background()
	vrf := model.Vrf{Name: "vs-dot", L3Interfaces: []model.L3Interface{
		{Interface: "ens224.100", Addresses: []string{"10.9.9.1/24"}}}}
	if err := p.ApplyVRF(ctx, vrf); err != nil {
		t.Fatalf("带点写法应等价于「父口 + vlan 子接口」: %v", err)
	}
	sub, ok := f.ifaces["ens224.100"]
	if !ok || !f.subifIdx[sub] {
		t.Fatalf("应建出 vlan 子接口（父口 ens224）: %v", f.ifaces)
	}
	if f.v4table[sub] != TableID("vs-dot") || f.v6table[sub] != TableID("vs-dot") {
		t.Fatalf("子接口应置入该 VRF 表: %v %v", f.v4table, f.v6table)
	}
	if len(f.addrs[sub]) != 1 {
		t.Fatalf("地址应配在子接口上: %v", f.addrs)
	}
	// 幂等（与 N1 同一路径）
	if err := p.ApplyVRF(ctx, vrf); err != nil {
		t.Fatalf("重复收敛必须幂等: %v", err)
	}
	if f.ifaces["ens224.100"] != sub {
		t.Fatalf("应复用同一子接口: %v", f.ifaces)
	}
	// 回收路径同样按形态识别（带点写法也要能删掉）
	if err := p.DeleteL3Interface(ctx, "vs-dot", vrf.L3Interfaces[0]); err != nil {
		t.Fatalf("DeleteL3Interface: %v", err)
	}
	if !fixed51Contains(f.deletedSubifs, sub) {
		t.Fatalf("带点写法的子接口也要回收: %v", f.deletedSubifs)
	}

	// 重复写法（名字已带 .<vlan> 又写了 vlan <v>）同样归一：解析与回收两处必须算出同一个名字，
	// 否则建出来的口永远查不回来（登记重建/回收走 vlanSubifName 的同一份口径）。
	f2 := newFakeL3()
	p2 := NewL3Provider(f2)
	dup := model.Vrf{Name: "vs-dup", L3Interfaces: []model.L3Interface{
		{Interface: "ens192.100", Vlan: 100, Addresses: []string{"10.8.8.1/24"}}}}
	if err := p2.ApplyVRF(ctx, dup); err != nil {
		t.Fatalf("重复写法应归一（父口 ens192 + vlan 100）: %v", err)
	}
	if err := p2.ApplyVRF(ctx, dup); err != nil {
		t.Fatalf("重复写法同样要幂等: %v", err)
	}
	dupSub, ok := f2.ifaces["ens192.100"]
	if !ok || f2.v4table[dupSub] != TableID("vs-dup") {
		t.Fatalf("归一后的子接口应可查回并在表内: %v %v", f2.ifaces, f2.v4table)
	}
	if err := p2.DeleteL3Interface(ctx, "vs-dup", dup.L3Interfaces[0]); err != nil {
		t.Fatalf("重复写法的回收: %v", err)
	}
	if !fixed51Contains(f2.deletedSubifs, dupSub) {
		t.Fatalf("重复写法的子接口也要回收: %v", f2.deletedSubifs)
	}
}

// 物理口/bond/vNIC 的普通写法一字不变（不得被误当成子接口去建）。点后不是合法 VID 的名字
// （如 `ens2f0.abc`）也退回按名直查：拿不到就是 ErrIfaceUnavailable，不得凭空建接口。
func TestFixR51N6PlainNamesUnaffected(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	ctx := context.Background()
	if err := p.ApplyVRF(ctx, model.Vrf{Name: "vs-plain", L3Interfaces: []model.L3Interface{
		{Interface: "ens192", Addresses: []string{"10.0.0.1/24"}}}}); err != nil {
		t.Fatalf("物理口写法: %v", err)
	}
	if len(f.subifIdx) != 0 {
		t.Fatalf("物理口不得建子接口: %v", f.subifIdx)
	}
	if f.v4table[1] != TableID("vs-plain") {
		t.Fatalf("物理口应置入 VRF 表: %v", f.v4table)
	}
	err := p.ApplyVRF(ctx, model.Vrf{Name: "vs-odd", L3Interfaces: []model.L3Interface{
		{Interface: "ens2f0.abc"}}})
	if err == nil || !errors.Is(err, ErrIfaceUnavailable) {
		t.Fatalf("点后不是合法 VID 的名字应如实报「接口不在数据面」: %v", err)
	}
	if len(f.subifIdx) != 0 {
		t.Fatalf("不得为非法名字建子接口: %v", f.subifIdx)
	}
}

// ---------- N4：vNIC 从 L3 交换机上摘下来时必须解除表归属 ----------

// 修复前症状：`ApplyVnfInterface` 只在 `port.VRF != ""` 时置表，空 VRF 那一支什么都不做 ⇒
// `set virtual-machine-functions <vm> interfaces <nic> virtual-switch <L2 交换机>`（或清掉
// 该叶子）提交成功、配置与读视图全绿，而口仍留在旧 L3 交换机的表（v4+v6）与登记里——
// NAT inside（AttachedIfaces）照旧把它当转发域成员下发特性。
func TestFixR51N4ReleaseVnfIface(t *testing.T) {
	f := newFakeL3()
	f.ifaces["vh-vm-a-eth0"] = 5
	p := NewL3Provider(f)
	ctx := context.Background()
	const name = "vh-vm-a-eth0"
	if err := p.SetVnfTable(ctx, "vs-nat", name); err != nil {
		t.Fatalf("前置 SetVnfTable: %v", err)
	}
	tid := TableID("vs-nat")
	if f.v4table[5] != tid || f.v6table[5] != tid {
		t.Fatalf("前置：口应在该表里: %v %v", f.v4table, f.v6table)
	}

	if err := p.ReleaseVnfIface(ctx, name); err != nil {
		t.Fatalf("ReleaseVnfIface: %v", err)
	}
	if f.v4table[5] != 0 || f.v6table[5] != 0 {
		t.Fatalf("必须移回默认表（v4/v6 都要）: %v %v", f.v4table, f.v6table)
	}
	if got := p.AttachedIfaces("vs-nat"); len(got) != 0 {
		t.Fatalf("转发域登记必须摘掉（NAT inside 的来源）: %v", got)
	}
	if _, ok := p.TableOfIface(name); ok {
		t.Fatal("按名归属登记（NAT outside 解析来源）必须摘掉")
	}
	// 幂等
	if err := p.ReleaseVnfIface(ctx, name); err != nil {
		t.Fatalf("重复解除应幂等: %v", err)
	}
	// 查询失败不许当「已达成」（把「问不出来」当成功是本仓反复收口的假绿形态）
	f2 := newFakeL3()
	f2.ifaces["vh-vm-b-eth0"] = 6
	f2.tableQueryErr = errors.New("dump 超时")
	if err := NewL3Provider(f2).ReleaseVnfIface(ctx, "vh-vm-b-eth0"); err == nil ||
		!strings.Contains(err.Error(), "dump 超时") {
		t.Fatalf("运行态查询失败必须上抛: %v", err)
	}
	// 接口不在数据面且无登记：归属随接口消失，按已达成
	if err := NewL3Provider(newFakeL3()).ReleaseVnfIface(ctx, "vh-ghost-eth0"); err != nil {
		t.Fatalf("接口不在数据面应按已达成: %v", err)
	}
}

// 覆盖**两个方向**（进 L3 交换机 / 出 L3 交换机）：ApplyVnfInterface 必须对称。
func TestFixR51N4ApplyVnfInterfaceBothDirections(t *testing.T) {
	l3f := newFakeL3()
	n := NewL2Network(nil, NewL2Provider(newFakeL2()))
	n.SetL3(NewL3Provider(l3f))
	n.SetVhostUser(vhostProvider(newFakeVhost()))
	ctx := context.Background()
	name := orchestrator.VnfIfaceName("vm-a", "nic0")
	l3f.ifaces[name] = 7 // vhost-user 口由接入编排建出（假实现分属两个 registry）
	port := orchestrator.VnfPort{VM: "vm-a", Interface: "nic0", Type: "vhost-user",
		Socket: "/run/nfvis/vhost/vm-a-nic0.sock", VRF: "vs-l3"}

	if err := n.ApplyVnfInterface(ctx, port); err != nil {
		t.Fatalf("接入 L3 交换机: %v", err)
	}
	tid := TableID("vs-l3")
	if l3f.v4table[7] != tid || l3f.v6table[7] != tid {
		t.Fatalf("入向：vNIC 应置入该 L3 交换机的表: %v %v", l3f.v4table, l3f.v6table)
	}
	if got := n.l3.AttachedIfaces("vs-l3"); len(got) != 1 || got[0] != 7 {
		t.Fatalf("入向：应进该 VRF 的转发域（NAT inside）: %v", got)
	}

	// 改挂 L2 交换机（VNF 侧声明的 virtual-switch 换了 ⇒ port.VRF 为空）
	port.VRF = ""
	if err := n.ApplyVnfInterface(ctx, port); err != nil {
		t.Fatalf("改挂 L2 交换机: %v", err)
	}
	if l3f.v4table[7] != 0 || l3f.v6table[7] != 0 {
		t.Fatalf("出向：必须移回默认表（否则它继续当旧 VRF 的转发域成员）: %v %v", l3f.v4table, l3f.v6table)
	}
	if got := n.l3.AttachedIfaces("vs-l3"); len(got) != 0 {
		t.Fatalf("出向：转发域登记必须摘掉: %v", got)
	}

	// 第三态：同一条声明改成 sriov-vf（VF 直通不经 VPP，故不置表）；空 VRF 一侧仍要解除
	// 旧归属——否则改类型后那个口仍留在旧 L3 交换机的表里（同族残留）。
	port.VRF = "vs-l3"
	if err := n.ApplyVnfInterface(ctx, port); err != nil {
		t.Fatalf("置回 L3 交换机: %v", err)
	}
	if l3f.v4table[7] != tid {
		t.Fatalf("前置：应重新置入该表: %v", l3f.v4table)
	}
	passthrough := port
	passthrough.Type = "sriov-vf"
	passthrough.VRF = ""
	if err := n.ApplyVnfInterface(ctx, passthrough); err != nil {
		t.Fatalf("改类型为 sriov-vf: %v", err)
	}
	if l3f.v4table[7] != 0 || l3f.v6table[7] != 0 {
		t.Fatalf("sriov-vf（空 VRF）应解除旧表归属: %v %v", l3f.v4table, l3f.v6table)
	}
	if got := n.l3.AttachedIfaces("vs-l3"); len(got) != 0 {
		t.Fatalf("sriov-vf 一侧同样不得留在转发域里: %v", got)
	}
}

// ---------- 小工具 ----------

// fixed51Contains 切片包含判定（用例内联，避免与既有测试的同名工具冲突）。
func fixed51Contains(xs []uint32, want uint32) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// round51 真机首验追加（决策 #525）：L3 vlan 子接口**必须**带 ExactMatch 创建。
//
// 由来（nfvis-vm 上的实测，不是推断）：VPP 26.06 对非 exact-match 的子接口直接拒绝配地址——
// `vppctl set interface ip address ens224.100 …` 回
// `sub-interface without exact-match doesn't support IP addressing`，产品侧表现为
// `VPPApiError: Unsupported (-126)`。于是「建得出子接口、配不上地址」⇒ 整次提交回滚
// （补偿还会去删那张刚建的表）。L3 接口的全部意义就是配地址，这个标志因此是**要件**。
// L2 的 trunk 端口早就带着它（l2.go desiredMembers 的 CreateSubifReq{… ExactMatch: true}），
// L3 侧是漏的。假件已按真机语义记录该标志，本用例钉住「L3 建的每个子接口都带它」。
func TestFixR51N8L3SubifRequiresExactMatch(t *testing.T) {
	c := newFakeL3()
	p := NewL3Provider(c)
	vrf := model.Vrf{Name: "vs-vlan", L3Interfaces: []model.L3Interface{
		{Interface: "ens224", Vlan: 100, Addresses: []string{"10.99.0.1/24"}},
	}}
	if err := p.ApplyVRF(context.Background(), vrf); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	if got, ok := c.subifExact[100]; !ok {
		t.Fatal("未建出 sub_id=100 的子接口")
	} else if !got {
		t.Fatal("L3 vlan 子接口未带 ExactMatch——真机上它永远配不上 IP 地址（Unsupported -126）")
	}
	// 地址必须真的落到子接口上（正控：证明这条路径端到端可用，而不只是标志对了）
	sub, ok := c.ifaces["ens224.100"]
	if !ok {
		t.Fatal("VPP 侧没有 ens224.100")
	}
	if len(c.addrs[sub]) == 0 {
		t.Fatalf("子接口上没有地址: %+v", c.addrs)
	}
}

// round51 真机复现追加（决策 #525）：**历史遗留的非 exact-match 子接口必须删掉重建，不能按名复用**。
//
// 现场（nfvis-vm，从 v3.0.33 发布件升级到 r51 后）：VPP 里已有一个 `ens224.100`，是早先版本
// 建的**没带 exact-match** 的子接口。产品按确定性名查到它就复用了 —— 于是
// `set interface ip address` 报 `sub-interface without exact-match doesn't support IP addressing`
// （govpp `Unsupported (-126)`），整次提交回滚；而"删一次交换机再配"也没用（删的是新声明，
// 老壳仍在）。也就是说：只把创建改成带标志**不够**，复用路径同样要验。
func TestFixR51N9StaleNonExactMatchSubifRebuilt(t *testing.T) {
	c := newFakeL3()
	// 造现场：VPP 里已有 ens224.100（idx 100），且是**非** exact-match 的坏壳
	c.ifaces["ens224.100"] = 100
	c.subifIdx[100] = true
	c.subifNonExact[100] = true

	p := NewL3Provider(c)
	vrf := model.Vrf{Name: "vs-vlan", L3Interfaces: []model.L3Interface{
		{Interface: "ens224", Vlan: 100, Addresses: []string{"10.99.0.1/24"}},
	}}
	if err := p.ApplyVRF(context.Background(), vrf); err != nil {
		t.Fatalf("ApplyVRF 应删掉坏壳并重建，实际报错: %v", err)
	}
	// ① 坏壳被删了
	found := false
	for _, d := range c.deletedSubifs {
		if d == 100 {
			found = true
		}
	}
	if !found {
		t.Fatalf("历史遗留的非 exact-match 子接口应被删除重建（实得 deletedSubifs=%v）", c.deletedSubifs)
	}
	// ② 新的子接口带 exact-match 且拿到了地址
	sub, ok := c.ifaces["ens224.100"]
	if !ok {
		t.Fatal("重建后应有 ens224.100")
	}
	if sub == 100 {
		t.Fatal("重建的子接口不该复用旧索引（旧壳已被删）")
	}
	if !c.subifExact[100] && !c.subifExact[uint32(sub)] {
		// 记录的 key 是 sub_id；sub_id=100 对应这次创建
		t.Fatalf("重建时未带 ExactMatch: %+v", c.subifExact)
	}
	if len(c.addrs[sub]) == 0 {
		t.Fatalf("重建后的子接口上没有地址: %+v", c.addrs)
	}
}

// 负控：**已经是** exact-match 的既有子接口照旧复用（不得每次重放都删了重建）。
func TestFixR51N9HealthySubifStillReused(t *testing.T) {
	c := newFakeL3()
	c.ifaces["ens224.100"] = 100
	c.subifIdx[100] = true // exact-match（默认 true）

	p := NewL3Provider(c)
	vrf := model.Vrf{Name: "vs-vlan", L3Interfaces: []model.L3Interface{
		{Interface: "ens224", Vlan: 100, Addresses: []string{"10.99.0.1/24"}},
	}}
	if err := p.ApplyVRF(context.Background(), vrf); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	if len(c.deletedSubifs) != 0 {
		t.Fatalf("健康的 exact-match 子接口不该被删（重放是幂等的）: %v", c.deletedSubifs)
	}
	if len(c.addrs[100]) == 0 {
		t.Fatalf("复用路径仍须把地址配上: %+v", c.addrs)
	}
}
