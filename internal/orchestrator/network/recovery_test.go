package network

// M3-8：恢复收敛单测（FR-OPS-010/011）。

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// recoveryFixture 组装带全部子编排器的 L2Network 与假客户端（同包测试复用各 fake）。
type recoveryFixture struct {
	net    *L2Network
	l2     *fakeL2
	l3     *fakeL3
	acl    *fakeAcl
	nat    *fakeNat
	svc    *fakeSvc
	bond   *fakeBond
	lldp   *fakeLldp
	alarms *AlarmStore
}

func newRecoveryFixture() *recoveryFixture {
	l2, l3 := newFakeL2(), newFakeL3()
	acl, nat, svc, bond := newFakeAcl(), newFakeNat(), newFakeSvc(), newFakeBond()
	lldp := &fakeLldp{}
	net := NewL2Network(orchestrator.NewNoopNetwork(), NewL2Provider(l2))
	net.SetL3(NewL3Provider(l3))
	net.SetServices(NewServicesProvider(svc))
	net.SetACL(NewAclProvider(acl))
	net.SetNAT(NewNatProvider(nat))
	net.SetBond(NewBondProvider(bond))
	net.SetLldp(NewLldpProvider(lldp))
	alarms := NewAlarmStore()
	net.SetAlarms(alarms)
	return &recoveryFixture{net: net, l2: l2, l3: l3, acl: acl, nat: nat, svc: svc, bond: bond, lldp: lldp, alarms: alarms}
}

func l2Switch(name string, ports ...string) model.VirtualSwitch {
	vs := model.VirtualSwitch{Name: name, Type: "l2"}
	for i, p := range ports {
		vs.Ports = append(vs.Ports, model.VSwitchPort{Seq: i + 1, Interface: p})
	}
	return vs
}

// nfvisd 重启后（登记表为空）VPP 侧 BD 已被删除 → 收敛补建并按配置挂接成员。
func TestEnsureConsistentRecreatesDeletedBD(t *testing.T) {
	f := newRecoveryFixture()
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{l2Switch("vs-a", "ens192", "ens224")}}

	errs := f.net.EnsureConsistent(context.Background(), cfg)
	if len(errs) != 0 {
		t.Fatalf("应收敛成功，实际: %v", errs)
	}
	if !f.l2.bds[BDID("vs-a")] {
		t.Fatal("BD 应被补建")
	}
	if len(f.l2.bridge) != 2 {
		t.Fatalf("两个端口都应挂接，实际 %v", f.l2.bridge)
	}
	if got := len(f.alarms.List(AlarmActive)); got != 0 {
		t.Fatalf("成功收敛不应产生告警，实际 %d", got)
	}

	// 再次收敛：仍应幂等（不重复建 BD、不摘除成员）
	if errs := f.net.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("重复收敛应成功: %v", errs)
	}
	if !f.l2.bds[BDID("vs-a")] || len(f.l2.bridge) != 2 {
		t.Fatal("重复收敛破坏了既有状态")
	}
}

// 决策 #170：恢复收敛同样要把 VNF 侧声明的 vNIC 挂进 BD——否则 nfvisd 重启/VPP 重启后，
// vhost-user 口回不到 bridge-domain 里，guest 静默失去 L2 连通。
func TestEnsureConsistentAttachesVnfNicMember(t *testing.T) {
	f := newRecoveryFixture()
	vh := orchestrator.VnfIfaceName("vm-a", "eth0")
	f.l2.ifaces[vh] = 77 // vNIC 接口由 ApplyVnfInterface 重放后存在
	f.l2.names[77] = SwIfInfo{Name: vh}
	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{l2Switch("vs-vnf", "ens192")},
		VirtualMachineFunctions: []model.VMFunction{{Name: "vm-a", Image: "img",
			Interfaces: []model.VnfInterface{{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-vnf"}}}},
	}

	if errs := f.net.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("应收敛成功: %v", errs)
	}
	if f.l2.bridge[77] != BDID("vs-vnf") {
		t.Fatalf("VNF 声明的 vhost 口应为 BD 成员: %v", f.l2.bridge)
	}
	if got := len(f.alarms.List(AlarmActive)); got != 0 {
		t.Fatalf("成功收敛不应产生告警，实际 %d", got)
	}
}

// 声明的交换机不存在时进未收敛清单（warning 告警），不静默跳过。
func TestEnsureConsistentReportsUnresolvableVnicSwitch(t *testing.T) {
	f := newRecoveryFixture()
	cfg := model.Config{VirtualMachineFunctions: []model.VMFunction{{Name: "vm-a", Image: "img",
		Interfaces: []model.VnfInterface{{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-ghost"}}}}}

	errs := f.net.EnsureConsistent(context.Background(), cfg)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "vs-ghost") {
		t.Fatalf("应恰有 1 条无法归位的声明: %v", errs)
	}
	active := f.alarms.List(AlarmActive)
	if len(active) != 1 || active[0].Code != AlarmUnconverged {
		t.Fatalf("应产生未收敛告警: %+v", active)
	}
}

// 收敛时拆除声明集之外的 bond（apply 撤销缺口：数据面残留 BondEthernetX 与成员关系），
// 声明仍在的 bond 不受影响。
func TestEnsureConsistentPrunesUndeclaredBond(t *testing.T) {
	f := newRecoveryFixture()
	f.bond.addBond(102, "bond9", 2) // 数据面残留（配置已不再声明）
	cfg := model.Config{Bonds: []model.Bond{{Name: "bond0", Members: []string{"ens192"}}}}

	if errs := f.net.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("应收敛成功，实际: %v", errs)
	}
	if len(f.bond.deleted) != 1 || f.bond.deleted[0] != 102 {
		t.Fatalf("只应拆除未声明的 bond9: %v", f.bond.deleted)
	}
	if len(f.bond.detach) != 1 || f.bond.detach[0] != 2 {
		t.Fatalf("应摘除 bond9 的成员: %v", f.bond.detach)
	}
	// 声明仍在的 bond0 已按其配置建立（未被 prune 触碰）
	if len(f.bond.created) != 1 || f.bond.names[101] != "bond0" {
		t.Fatalf("声明中的 bond 应正常收敛: created=%v names=%v", f.bond.created, f.bond.names)
	}
	if got := len(f.alarms.List(AlarmActive)); got != 0 {
		t.Fatalf("成功拆除不应产生告警，实际 %d", got)
	}
}

// 配置引用的物理口被移除：不可收敛项进告警（error 级），其余对象继续收敛。
func TestEnsureConsistentMissingIfaceRaisesAlarm(t *testing.T) {
	f := newRecoveryFixture()
	delete(f.l2.ifaces, "ens224")
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{
		l2Switch("vs-a", "ens192"),
		l2Switch("vs-b", "ens224"),
	}}

	errs := f.net.EnsureConsistent(context.Background(), cfg)
	if len(errs) != 1 {
		t.Fatalf("应恰有 1 个未收敛项，实际 %d: %v", len(errs), errs)
	}
	if !errors.Is(errs[0], ErrIfaceUnavailable) {
		t.Fatalf("应标记为接口缺失: %v", errs[0])
	}
	if !strings.Contains(errs[0].Error(), "virtual-switches/vs-b") {
		t.Fatalf("未收敛项应指向 vs-b: %v", errs[0])
	}
	// 失败不阻塞其它对象
	if !f.l2.bds[BDID("vs-a")] || f.l2.bridge[1] != BDID("vs-a") {
		t.Fatal("vs-a 应在 vs-b 失败前完成收敛")
	}

	active := f.alarms.List(AlarmActive)
	if len(active) != 1 || active[0].Code != AlarmIfaceMissing || active[0].Severity != SeverityError {
		t.Fatalf("应产生接口缺失 error 告警: %+v", active)
	}
	if active[0].Source != "virtual-switches/vs-b" {
		t.Fatalf("告警 source 不符: %+v", active[0])
	}
}

// 修复后再次收敛：告警自动 resolved。
func TestEnsureConsistentResolvesAlarmAfterFix(t *testing.T) {
	f := newRecoveryFixture()
	delete(f.l2.ifaces, "ens224")
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{l2Switch("vs-b", "ens224")}}

	if errs := f.net.EnsureConsistent(context.Background(), cfg); len(errs) != 1 {
		t.Fatalf("首次应收敛失败: %v", errs)
	}
	f.l2.ifaces["ens224"] = 2 // 端口恢复
	if errs := f.net.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("修复后应收敛成功: %v", errs)
	}
	if got := len(f.alarms.List(AlarmActive)); got != 0 {
		t.Fatalf("告警应已 resolved，实际活动 %d", got)
	}
	if got := len(f.alarms.List(AlarmResolved)); got != 1 {
		t.Fatalf("应保留 1 条 resolved 记录，实际 %d", got)
	}
}

// VPP 重启后 bond 已丢失，但进程内登记的 sw_if_index 陈旧：收敛应重建而非复用。
func TestEnsureConsistentRebuildsStaleBond(t *testing.T) {
	f := newRecoveryFixture()
	cfg := model.Config{Bonds: []model.Bond{{Name: "bond0", Members: []string{"ens192", "ens224"}}}}

	if errs := f.net.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("首次收敛应成功: %v", errs)
	}
	created := len(f.bond.created)
	if created != 1 {
		t.Fatalf("应创建 1 个 bond，实际 %d", created)
	}

	// 第二次收敛：登记表被清空，按接口名发现既有 bond → 删除重建
	if errs := f.net.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("二次收敛应成功: %v", errs)
	}
	if len(f.bond.created) != created+1 {
		t.Fatalf("应重建 bond（created=%v，deleted=%v）", f.bond.created, f.bond.deleted)
	}
	if len(f.bond.deleted) == 0 {
		t.Fatal("应先删除 VPP 侧既有 bond 再重建")
	}
}

// 恢复收敛逐对象收集失败：一个 ACL 失败不影响其它对象。
func TestEnsureConsistentCollectsPerObjectFailures(t *testing.T) {
	f := newRecoveryFixture()
	f.nat.err = errors.New("nat 下发失败")
	cfg := model.Config{
		Acls:            []model.Acl{{Name: "a1"}},
		Nat:             &model.NatConfig{SourcePools: []model.NatSourcePool{{Name: "p1", AddressRange: "10.0.0.1 to 10.0.0.9"}}},
		VirtualSwitches: []model.VirtualSwitch{l2Switch("vs-a", "ens192")},
	}
	errs := f.net.EnsureConsistent(context.Background(), cfg)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "nat") {
		t.Fatalf("应仅 NAT 失败: %v", errs)
	}
	if !f.l2.bds[BDID("vs-a")] {
		t.Fatal("NAT 失败不应阻塞虚拟交换机收敛")
	}
	active := f.alarms.List(AlarmActive)
	if len(active) != 1 || active[0].Code != AlarmUnconverged || active[0].Severity != SeverityWarning {
		t.Fatalf("应产生一般未收敛 warning 告警: %+v", active)
	}
}

// fakeAclLookup 模拟 VPP 侧已有同名 ACL（acl_dump 按 tag 反查）。
type fakeAclLookup struct {
	*fakeAcl
	existing map[string]uint32
}

func (f *fakeAclLookup) ACLIndexByTag(tag string) (uint32, bool, error) {
	idx, ok := f.existing[tag]
	return idx, ok, nil
}

// 恢复收敛遇到 VPP 侧已存在同名 ACL：走 replace（复用索引）而非新建重复项。
func TestApplyACLReusesExistingTag(t *testing.T) {
	c := &fakeAclLookup{fakeAcl: newFakeAcl(), existing: map[string]uint32{"a1": 7}}
	p := NewAclProvider(c)
	if err := p.ApplyACL(context.Background(), model.Acl{Name: "a1"}); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	if len(c.added) != 0 {
		t.Fatalf("不应新建 ACL: %v", c.added)
	}
	if len(c.replaced) != 1 || c.replaced[0] != 7 {
		t.Fatalf("应 replace 已有索引 7: %v", c.replaced)
	}
}

// round84 缺陷 B：恢复收敛开头会失效全部进程内登记，而那时 VRF 条目尚未重放——
// vNIC 的 L3 置表登记不能因为「这一轮接入时失败」就永远缺失。登记缺了 vNIC 的直接后果是
// 下一次 ApplyNAT 认为该口不该有 inside 特性而去删（VPP 报 `No such entry (-6)`），
// 并把整批 apply 打回滚。这里守护「按配置声明在 VRF 落地之后重建登记」这一路：
// 接入那一轮登记失败（接口当时还不可见）时，收敛也必须把该口放进 AttachedIfaces，
// 使随后的 NAT 下发把它纳入 inside。
func TestEnsureConsistentRebuildsVnfTableRegistration(t *testing.T) {
	f := newRecoveryFixture()
	f.net.SetVhostUser(vhostProvider(newFakeVhost())) // 接入编排在位
	vh := orchestrator.VnfIfaceName("vm-a", "eth0")
	f.l3.ifaces[vh] = 5
	f.l3.vnfIndexFailOnce[vh] = true // 接入那一轮：接口尚未可见 → 登记失败

	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{
			{Name: "vs-nat", Type: "l3"},
			l2Switch("vs-wan", "ens224"),
		},
		Vrfs: []model.Vrf{
			{Name: "vs-nat", L3Interfaces: []model.L3Interface{
				{Interface: "ens192", Addresses: []string{"192.168.200.1/24"}}}},
			{Name: "vs-wan", L3Interfaces: []model.L3Interface{
				{Interface: "ens224", Addresses: []string{"192.168.155.61/24"}}}},
		},
		VirtualMachineFunctions: []model.VMFunction{{Name: "vm-a", Image: "img",
			Interfaces: []model.VnfInterface{{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-nat"}}}},
		Nat: &model.NatConfig{Rules: []model.NatRule{{Seq: 10, MatchSource: "192.168.200.0/24",
			VirtualSwitch: "vs-nat", Action: model.NatAction{Interface: "ens224"}}}},
	}

	// 接入路径那一轮失败只记未收敛项（1 条），不得因此丢掉登记
	errs := f.net.EnsureConsistent(context.Background(), cfg)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "vh-vm-a-eth0") {
		t.Fatalf("应恰有 1 条接入失败（登记由重建补齐）: %v", errs)
	}
	if got := f.l3.v4table[5]; got != TableID("vs-nat") {
		t.Fatalf("vNIC 应被置入 vs-nat 表（实际 %d）", got)
	}
	inside := false
	for _, idx := range f.net.l3.AttachedIfaces("vs-nat") {
		if idx == 5 {
			inside = true
		}
	}
	if !inside {
		t.Fatalf("vNIC 必须重新登记进 inside 解析来源: %v", f.net.l3.AttachedIfaces("vs-nat"))
	}
	joined := strings.Join(f.nat.feats, ",")
	if !strings.Contains(joined, "add:5:inside") {
		t.Fatalf("NAT 下发必须把重建的 vNIC 当 inside: %v", f.nat.feats)
	}

	// 再次收敛：接口可见、登记已在、VPP 侧也已在目标表 → 应收敛干净，且不再对 vNIC 置表
	// （幂等；对带地址的口重复置表会被 VPP 以 -114 拒绝）
	before := len(f.l3.setTableIdx)
	if errs := f.net.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("重复收敛应成功: %v", errs)
	}
	for _, idx := range f.l3.setTableIdx[before:] {
		if idx == 5 {
			t.Fatalf("重复收敛不得再对 vNIC 下发置表: %v", f.l3.setTableIdx[before:])
		}
	}
}

// round84 R84-21 收尾（回归）：`systemctl restart vpp` 加（或）`systemctl restart nfvis` 之后，
// NAT 必须仍然在位——真机实测曾是「NAT 被整体关掉」：`show nat44 ei interfaces` 与
// `show nat44 ei addresses` 全空，而配置里规则/交换机/地址都在、日志无未收敛项。
//
// 机制：恢复收敛开头失效了全部进程内登记（VPP 重启后索引会变，必须失效），而 NAT 的
// inside/outside 解析**只读** L3 侧三张登记表——登记不按配置重建，desiredFeatures 就拿不到
// inside 成员与 outside 转发域。本用例守护「失效 → 一次完整收敛 ⇒ 登记全部回来、NAT 重新下发」：
//   - AttachedIfaces(vs-nat) 答得出 L3 接口 ens192(1) 与 vNIC vh-vm-a-eth0(5)；
//   - TableOfIface(ens224) 答得出 vs-wan 的表（outside 转发域，决策 #52）；
//   - NAT 重新下发 inside/outside 特性与地址池（池落 outside VRF），全程不得出现 del/disable。
func TestEnsureConsistentRebuildsL3RegistrationsForNAT(t *testing.T) {
	f := newRecoveryFixture()
	f.net.SetVhostUser(vhostProvider(newFakeVhost()))
	ctx := context.Background()
	vh := orchestrator.VnfIfaceName("vm-a", "eth0")
	f.l3.ifaces[vh] = 5 // 接入重放后该 vNIC 在 VPP 中存在

	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{{Name: "vs-nat", Type: "l3"}, {Name: "vs-wan", Type: "l3"}},
		Vrfs: []model.Vrf{
			{Name: "vs-nat", L3Interfaces: []model.L3Interface{
				{Interface: "ens192", Addresses: []string{"192.168.200.1/24"}}}},
			{Name: "vs-wan", L3Interfaces: []model.L3Interface{
				{Interface: "ens224", Addresses: []string{"192.168.155.61/24"}}}},
		},
		VirtualMachineFunctions: []model.VMFunction{{Name: "vm-a", Image: "img",
			Interfaces: []model.VnfInterface{{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-nat"}}}},
		Nat: &model.NatConfig{
			SourcePools: []model.NatSourcePool{{Name: "pool-a", AddressRange: "192.168.155.62"}},
			Rules: []model.NatRule{{Seq: 10, MatchSource: "192.168.200.0/24", VirtualSwitch: "vs-nat",
				Action: model.NatAction{SourcePool: "pool-a", Interface: "ens224"}}},
		},
	}
	// 前置：配置已在跑（首个收敛即正常运行态）
	if errs := f.net.EnsureConsistent(ctx, cfg); len(errs) != 0 {
		t.Fatalf("前置收敛应成功: %v", errs)
	}
	natTable, wanTable := TableID("vs-nat"), TableID("vs-wan")
	assertNATOps := func(t *testing.T, feats []string, ranges []string, enables []string) {
		t.Helper()
		joined := strings.Join(feats, ",")
		for _, want := range []string{"add:1:inside", "add:5:inside", "add:2:outside"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("NAT 特性缺少 %s：%v", want, feats)
			}
		}
		want := "add:192.168.155.62-192.168.155.62@" + strconv.FormatUint(uint64(wanTable), 10)
		if !containsStr(ranges, want) {
			t.Fatalf("地址池必须落 outside 转发域（%s）：%v", want, ranges)
		}
		if len(enables) != 1 || enables[0] != "enable:"+strconv.FormatUint(uint64(natTable), 10)+
			"/"+strconv.FormatUint(uint64(wanTable), 10) {
			t.Fatalf("插件应按 inside/outside 转发域启用一次: %v", enables)
		}
		for _, ops := range [][]string{feats, ranges} {
			for _, op := range ops {
				if strings.HasPrefix(op, "del:") {
					t.Fatalf("重放只补齐不摘除，不得出现 del: %v", op)
				}
			}
		}
		for _, op := range enables {
			if strings.HasPrefix(op, "disable") {
				t.Fatalf("有 NAT 配置时不得关闭插件: %v", enables)
			}
		}
	}
	f.nat.ranges, f.nat.feats, f.nat.static, f.nat.enables = nil, nil, nil, nil

	// VPP 连接（重）建立：登记全部失效，随后的一次收敛必须把登记按配置重建并重放 NAT
	f.net.InvalidateRuntimeState()
	if got := f.net.l3.AttachedIfaces("vs-nat"); len(got) != 0 {
		t.Fatalf("失效后派生查询应为空（这是 NAT 必须重建登记的由来）: %v", got)
	}
	if errs := f.net.EnsureConsistent(ctx, cfg); len(errs) != 0 {
		t.Fatalf("重启后的收敛应成功: %v", errs)
	}
	if got := f.net.l3.AttachedIfaces("vs-nat"); len(got) != 2 || got[0] != 1 || got[1] != 5 {
		t.Fatalf("L3 接口与 vNIC 都必须重新登记进 inside 集合: %v", got)
	}
	if tbl, ok := f.net.l3.TableOfIface("ens224"); !ok || tbl != wanTable {
		t.Fatalf("outside 出接口应解析回 vs-wan 的表: %d %v", tbl, ok)
	}
	if tbl, ok := f.net.l3.TableOfIface(vh); !ok || tbl != natTable {
		t.Fatalf("vNIC 应登记所属 VRF（NAT outside 解析同源）: %d %v", tbl, ok)
	}
	assertNATOps(t, f.nat.feats, f.nat.ranges, f.nat.enables)

	// 再次重启收敛：仍然只补齐（不 del、不 disable），登记不变
	f.nat.ranges, f.nat.feats, f.nat.static, f.nat.enables = nil, nil, nil, nil
	f.net.InvalidateRuntimeState()
	if errs := f.net.EnsureConsistent(ctx, cfg); len(errs) != 0 {
		t.Fatalf("重复收敛应成功: %v", errs)
	}
	if got := f.net.l3.AttachedIfaces("vs-nat"); len(got) != 2 || got[0] != 1 || got[1] != 5 {
		t.Fatalf("重复收敛后登记应保持: %v", got)
	}
	assertNATOps(t, f.nat.feats, f.nat.ranges, f.nat.enables)
}

// 同上，但规则不带 source-pool：外部地址走「出接口自身地址」（nat44_ei_add_del_interface_addr）
// 这条路在登记重建后同样必须重新下发（两条路径都要覆盖，round84 收尾）。
func TestEnsureConsistentRebuildsL3RegistrationsForNATIfaceAddr(t *testing.T) {
	f := newRecoveryFixture()
	f.net.SetVhostUser(vhostProvider(newFakeVhost()))
	ctx := context.Background()
	f.l3.ifaces[orchestrator.VnfIfaceName("vm-a", "eth0")] = 5
	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{{Name: "vs-nat", Type: "l3"}, {Name: "vs-wan", Type: "l3"}},
		Vrfs: []model.Vrf{
			{Name: "vs-nat", L3Interfaces: []model.L3Interface{
				{Interface: "ens192", Addresses: []string{"192.168.200.1/24"}}}},
			{Name: "vs-wan", L3Interfaces: []model.L3Interface{
				{Interface: "ens224", Addresses: []string{"192.168.155.61/24"}}}},
		},
		VirtualMachineFunctions: []model.VMFunction{{Name: "vm-a", Image: "img",
			Interfaces: []model.VnfInterface{{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-nat"}}}},
		Nat: &model.NatConfig{Rules: []model.NatRule{{Seq: 10, MatchSource: "192.168.200.0/24",
			VirtualSwitch: "vs-nat", Action: model.NatAction{Interface: "ens224"}}}},
	}

	f.net.InvalidateRuntimeState()
	if errs := f.net.EnsureConsistent(ctx, cfg); len(errs) != 0 {
		t.Fatalf("重启后的收敛应成功: %v", errs)
	}
	joined := strings.Join(f.nat.feats, ",")
	for _, want := range []string{"add:1:inside", "add:5:inside", "add:2:outside", "add:2:ifaddr"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("不带池的规则应以出接口地址作外部地址（缺 %s）: %v", want, f.nat.feats)
		}
	}
	if len(f.nat.enables) != 1 || !strings.HasPrefix(f.nat.enables[0], "enable:") {
		t.Fatalf("插件应保持启用: %v", f.nat.enables)
	}
	if got := f.net.l3.AttachedIfaces("vs-nat"); len(got) != 2 {
		t.Fatalf("inside 集合应含 L3 接口与 vNIC: %v", got)
	}
}

// 回归（round84 收尾，可判别）：VPP 重启后，L3 交换机三层接口所在的口在**重放那一刻**
// 还没枚举出来（guest 的口 = vNIC，round84 证据 §14 的拓扑里它就是该交换机的 l3-interface），
// 于是接入重放与 ApplyVRF 两条增量路径都没能把登记落地——若登记只依赖增量调用，
// `AttachedIfaces` 就永远是空的，NAT 的 inside 随之整批消失（`show nat44 ei interfaces` 无 `in`）。
// 本用例要求：这两次失败如实上报（不静默），同时登记必须由配置重建补回、NAT 的 inside 仍在。
func TestEnsureConsistentRebuildsL3RegistrationMissedByReplay(t *testing.T) {
	f := newRecoveryFixture()
	f.net.SetVhostUser(vhostProvider(newFakeVhost()))
	ctx := context.Background()
	vh := orchestrator.VnfIfaceName("vm-a", "eth0")
	f.l3.ifaces[vh] = 5
	// 该口在重放那一刻不可见（接入重放 + ApplyVRF 两次解析都落空），随后才可见
	f.l3.ifaceFailN[vh] = 2

	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{{Name: "vs-nat", Type: "l3"}, {Name: "vs-wan", Type: "l3"}},
		Vrfs: []model.Vrf{
			{Name: "vs-nat", L3Interfaces: []model.L3Interface{
				{Interface: vh, Addresses: []string{"192.168.200.1/24"}}}},
			{Name: "vs-wan", L3Interfaces: []model.L3Interface{
				{Interface: "ens224", Addresses: []string{"192.168.155.61/24"}}}},
		},
		VirtualMachineFunctions: []model.VMFunction{{Name: "vm-a", Image: "img",
			Interfaces: []model.VnfInterface{{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-nat"}}}},
		Nat: &model.NatConfig{
			SourcePools: []model.NatSourcePool{{Name: "pool-a", AddressRange: "192.168.155.62"}},
			Rules: []model.NatRule{{Seq: 10, MatchSource: "192.168.200.0/24", VirtualSwitch: "vs-nat",
				Action: model.NatAction{SourcePool: "pool-a", Interface: "ens224"}}},
		},
	}

	f.net.InvalidateRuntimeState()
	errs := f.net.EnsureConsistent(ctx, cfg)
	// 两次增量路径的失败必须可见（未收敛项），不得因为「重建会补」就吞掉
	if len(errs) != 2 {
		t.Fatalf("接入重放与 VRF 重放各应报 1 条未收敛项，实际 %d: %v", len(errs), errs)
	}
	for _, err := range errs {
		if !strings.Contains(err.Error(), vh) {
			t.Fatalf("未收敛项应指出是哪个口: %v", err)
		}
	}
	// 登记由配置重建补回：inside 集合含该 vNIC，outside 转发域解析正常
	if got := f.net.l3.AttachedIfaces("vs-nat"); len(got) != 1 || got[0] != 5 {
		t.Fatalf("重放漏掉的 L3 接口必须由登记重建补回: %v", got)
	}
	if tbl, ok := f.net.l3.TableOfIface("ens224"); !ok || tbl != TableID("vs-wan") {
		t.Fatalf("outside 出接口应解析回 vs-wan 的表: %d %v", tbl, ok)
	}
	// NAT 必须把它当 inside 下发，且插件保持启用（而不是被当成「没有 inside」）
	joined := strings.Join(f.nat.feats, ",")
	if !strings.Contains(joined, "add:5:inside") || !strings.Contains(joined, "add:2:outside") {
		t.Fatalf("NAT 必须下发 inside(vNIC) 与 outside: %v", f.nat.feats)
	}
	if len(f.nat.enables) != 1 || !strings.HasPrefix(f.nat.enables[0], "enable:") {
		t.Fatalf("插件应保持启用: %v", f.nat.enables)
	}
}

// containsStr 报告集合里是否有该元素（测试断言用）。
func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// round84 R84-21：VPP 连接（重）建立时进程内登记必须失效，使随后的收敛全量重放。
// 带外 `systemctl restart vpp` 清空 VPP 侧配置后，残留登记会让 ApplyNAT 认为「已下发」
// 而跳过重放——show nat44 空、NAT 静默失效，必须重启 nfvisd 才恢复。
// 同时守护「只补齐不摘除」（附录 A #35）：失效 + 重放只发 add，不得出现 del。
func TestInvalidateRuntimeStateForcesFullNATReplay(t *testing.T) {
	f := newRecoveryFixture()
	ctx := context.Background()
	vrf := model.Vrf{Name: "vs-nat", L3Interfaces: []model.L3Interface{
		{Interface: "ens192", Addresses: []string{"192.168.200.1/24"}}}}
	natCfg := model.NatConfig{
		SourcePools: []model.NatSourcePool{{Name: "pool-a", AddressRange: "192.168.155.62"}},
		Rules: []model.NatRule{{Seq: 10, MatchSource: "192.168.200.0/24", VirtualSwitch: "vs-nat",
			Action: model.NatAction{SourcePool: "pool-a", Interface: "ens224"}}},
	}
	if err := f.net.ApplyVRF(ctx, vrf); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	if err := f.net.ApplyNAT(ctx, natCfg); err != nil {
		t.Fatalf("ApplyNAT: %v", err)
	}
	if len(f.nat.ranges) != 1 || len(f.nat.enables) != 1 {
		t.Fatalf("首次下发应配置地址池并启用插件: ranges=%v enables=%v", f.nat.ranges, f.nat.enables)
	}

	// 登记仍在：同一配置的重放不产生任何请求（正是 R84-21 里「跳过重放」的机制）
	f.nat.ranges, f.nat.feats, f.nat.static, f.nat.enables = nil, nil, nil, nil
	if err := f.net.ApplyNAT(ctx, natCfg); err != nil {
		t.Fatalf("重复 ApplyNAT: %v", err)
	}
	if n := len(f.nat.ranges) + len(f.nat.feats) + len(f.nat.static) + len(f.nat.enables); n != 0 {
		t.Fatalf("登记仍在时不应重复下发，实际 %d 次操作", n)
	}

	// 连接（重）建立的挂点：失效 → 随后的收敛全量重放（顺序与恢复收敛一致：先 VRF 后 NAT）
	f.net.InvalidateRuntimeState()
	f.nat.ranges, f.nat.feats, f.nat.static, f.nat.enables = nil, nil, nil, nil
	if err := f.net.ApplyVRF(ctx, vrf); err != nil {
		t.Fatalf("失效后 ApplyVRF: %v", err)
	}
	if err := f.net.ApplyNAT(ctx, natCfg); err != nil {
		t.Fatalf("失效后 ApplyNAT: %v", err)
	}
	if len(f.nat.ranges) != 1 || !strings.HasPrefix(f.nat.ranges[0], "add:") {
		t.Fatalf("失效后必须重新下发地址池: %v", f.nat.ranges)
	}
	if len(f.nat.enables) != 1 || !strings.HasPrefix(f.nat.enables[0], "enable:") {
		t.Fatalf("失效后必须重新启用插件: %v", f.nat.enables)
	}
	joined := strings.Join(f.nat.feats, ",")
	if !strings.Contains(joined, "add:1:inside") || !strings.Contains(joined, "add:2:outside") {
		t.Fatalf("失效后必须重建 inside/outside 特性: %v", f.nat.feats)
	}
	for _, op := range append(append([]string{}, f.nat.ranges...), f.nat.feats...) {
		if strings.HasPrefix(op, "del:") {
			t.Fatalf("失效只清进程内登记，重放只补齐不摘除: %v", op)
		}
	}
}
