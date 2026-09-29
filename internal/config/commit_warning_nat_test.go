package config

// round87（决策 #192）：删除「被 NAT 用过」的 L3 交换机时，提交输出必须给出提示。
//
// 真机背景：NAT44 用过的表在 VPP 里带 nat44-ei-hi 引用，只有数据面重启才释放 ⇒ 删表在本次
// 提交里不会真正生效。提交按「延后收敛」口径成功（决策 #192），但**提交成功不等于表已经从
// 数据面消失**——这句话必须出现在提交输出里，否则就是假成功（残渣另进告警）。

import (
	"context"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// natRefConfig 旧配置：vs-lan（NAT inside，池落在它的表）+ vs-wan（出接口 ens2f1 所属的 L3 交换机）。
// 自带两块物理口：本用例只关心 NAT 与 VRF 的引用关系，不掺交换机端口的角色互斥。
func natRefConfig() model.Config {
	on := true
	return model.Config{
		System: &model.SystemConfig{
			Hostname: "nfvis-node1",
			Login: &model.SystemLogin{Users: []model.LoginUserConfig{
				{Name: "admin", Class: model.ClassSuperUser, PasswordHash: fixtureUserHash}}},
		},
		Interfaces: []model.InterfaceConfig{{Name: "ens2f0", Enabled: &on}, {Name: "ens2f1", Enabled: &on}},
		VirtualSwitches: []model.VirtualSwitch{
			{Name: "vs-lan", Type: "l3"}, {Name: "vs-wan", Type: "l3"}},
		Vrfs: []model.Vrf{
			{Name: "vs-lan", L3Interfaces: []model.L3Interface{{Interface: "ens2f0", Addresses: []string{"10.10.0.1/24"}}}},
			{Name: "vs-wan", L3Interfaces: []model.L3Interface{{Interface: "ens2f1", Addresses: []string{"192.168.155.20/24"}}}},
		},
		Nat: &model.NatConfig{
			SourcePools: []model.NatSourcePool{{Name: "pool-1", AddressRange: "192.168.155.30 to 192.168.155.35"}},
			Rules: []model.NatRule{{Seq: 10, MatchSource: "10.10.0.0/24", VirtualSwitch: "vs-lan",
				Action: model.NatAction{SourcePool: "pool-1", Interface: "ens2f1"}}},
		},
	}
}

// 删 vs-wan（NAT outside 所属交换机）必须提示：表要等 request vpp restart 才从数据面移除。
func TestCommitWarnsOnDeletedNatReferencedVRF(t *testing.T) {
	k := newEngineKit(t)
	sess := Session{User: "admin", Source: "console"}
	// 前置：把带 NAT 的配置提交成 committed
	k.edit(t, "admin", "console")
	if err := k.engine.UpdateCandidate(sess, natRefConfig()); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	if _, err := k.engine.Commit(context.Background(), sess, CommitOpts{}); err != nil {
		t.Fatalf("前置提交: %v", err)
	}

	// 同一提交里改 NAT 出接口 + 删旧 L3 交换机（真机 R86-10 的形态）
	k.edit(t, "admin", "console")
	cfg := natRefConfig()
	cfg.Vrfs = []model.Vrf{cfg.Vrfs[0]} // 删掉 vs-wan
	cfg.VirtualSwitches = []model.VirtualSwitch{{Name: "vs-lan", Type: "l3"}}
	cfg.Nat.Rules = []model.NatRule{{Seq: 10, MatchSource: "10.10.0.0/24", VirtualSwitch: "vs-lan",
		Action: model.NatAction{SourcePool: "pool-1", Interface: "ens2f0"}}}
	if err := k.engine.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate2: %v", err)
	}
	res, err := k.engine.Commit(context.Background(), sess, CommitOpts{})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !warningsContain(res.Warnings, "vs-wan") || !warningsContain(res.Warnings, "request vpp restart") {
		t.Fatalf("删 NAT 用过的交换机应提示「表要等 request vpp restart 才消失」: %+v", res.Warnings)
	}
	if warningsContain(res.Warnings, "vs-lan") {
		t.Fatalf("未删除的交换机不应出现在提示里: %+v", res.Warnings)
	}
}

// 删一台**与 NAT 无关**的 L3 交换机不得产生该提示（不制造无谓告警文本）。
func TestCommitNoWarningOnDeletedPlainVRF(t *testing.T) {
	k := newEngineKit(t)
	sess := Session{User: "admin", Source: "console"}
	k.edit(t, "admin", "console")
	cfg := baseCommitted()
	cfg.VirtualSwitches = append(cfg.VirtualSwitches, model.VirtualSwitch{Name: "vs-plain", Type: "l3"})
	cfg.Vrfs = []model.Vrf{{Name: "vs-plain"}}
	if err := k.engine.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	if _, err := k.engine.Commit(context.Background(), sess, CommitOpts{}); err != nil {
		t.Fatalf("前置提交: %v", err)
	}
	k.edit(t, "admin", "console")
	cfg2 := baseCommitted()
	if err := k.engine.UpdateCandidate(sess, cfg2); err != nil {
		t.Fatalf("UpdateCandidate2: %v", err)
	}
	res, err := k.engine.Commit(context.Background(), sess, CommitOpts{})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if warningsContain(res.Warnings, "vs-plain") {
		t.Fatalf("与 NAT 无关的交换机删除不应提示表锁: %+v", res.Warnings)
	}
}
