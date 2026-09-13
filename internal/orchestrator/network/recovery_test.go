package network

// M3-8：恢复收敛单测（FR-OPS-010/011）。

import (
	"context"
	"errors"
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
