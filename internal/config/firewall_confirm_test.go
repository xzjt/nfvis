package config

// 决策 #388：管理面主机防火墙变更的自锁保护——非 console 会话的防火墙变更（规则/默认策略，
// 含首次配置与删除）必须 commit confirmed；console 会话豁免；不涉及防火墙的提交不受影响。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// fwEngine 基线已带管理口**网卡名**（用 console 会话先提交一次声明——console 豁免，
// 与真机「管理口早已配好」的现场一致），使防火墙守卫成为唯一变量。
func fwEngine(t *testing.T) *Engine {
	t.Helper()
	eng, _, _ := newEngineWithExternals(t, nil, nil)
	con := Session{User: "admin", Source: "console"}
	if err := eng.Edit(con); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := eng.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	cfg.System.Management.Interface = "ens160"
	if err := eng.UpdateCandidate(con, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Commit(context.Background(), con, CommitOpts{}); err != nil {
		t.Fatalf("预置管理口网卡名: %v", err)
	}
	return eng
}

// setFirewall 把防火墙段写进候选（其余字段不动）。
func setFirewall(t *testing.T, eng *Engine, sess Session, fw *model.FirewallConfig) {
	t.Helper()
	if err := eng.Edit(sess); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := eng.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	cfg.System.Firewall = fw
	if err := eng.UpdateCandidate(sess, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestFirewallChangeRequiresConfirmed(t *testing.T) {
	fw := &model.FirewallConfig{Rules: []model.FirewallRule{{Seq: 100, Action: "accept", Source: "192.0.2.0/24", Protocol: "tcp", Port: 9999}}}
	for _, src := range []string{"ssh", "api"} {
		eng := fwEngine(t)
		sess := Session{User: "admin", Source: src}
		setFirewall(t, eng, sess, fw)
		_, err := eng.Commit(context.Background(), sess, CommitOpts{})
		if !errors.Is(err, ErrFirewallConfirmRequired) {
			t.Fatalf("source=%s 防火墙变更应要求 commit confirmed，得到 %v", src, err)
		}
		if !strings.Contains(err.Error(), "commit confirmed") {
			t.Fatalf("拒绝文案须含照做路径（commit confirmed）: %v", err)
		}
		// confirmed 放行并返回待确认截止时间
		res, err := eng.Commit(context.Background(), sess, CommitOpts{ConfirmedMinutes: 5})
		if err != nil {
			t.Fatalf("source=%s commit confirmed 应放行: %v", src, err)
		}
		if res.ConfirmedUntil == nil {
			t.Fatalf("source=%s 应返回待确认截止时间", src)
		}
		if err := eng.ConfirmCommit(sess); err != nil {
			t.Fatalf("确认应成功: %v", err)
		}
		cfg, err := eng.Committed()
		if err != nil || cfg.FirewallOf() == nil || len(cfg.FirewallOf().Rules) != 1 {
			t.Fatalf("防火墙配置应已生效: %+v %v", cfg.FirewallOf(), err)
		}
	}
}

// 本地串口豁免：console 不依赖管理网连通性（与 FR-CFG-012 同判据）。
func TestFirewallConsoleExempt(t *testing.T) {
	eng := fwEngine(t)
	con := Session{User: "admin", Source: "console"}
	setFirewall(t, eng, con, &model.FirewallConfig{DefaultPolicy: "drop"})
	if _, err := eng.Commit(context.Background(), con, CommitOpts{}); err != nil {
		t.Fatalf("本地串口不应被要求 confirmed: %v", err)
	}
}

// 删除防火墙（配置里曾有、新配置没有）同样算变更，需要 confirmed。
func TestFirewallRemovalRequiresConfirmed(t *testing.T) {
	eng := fwEngine(t)
	con := Session{User: "admin", Source: "console"}
	setFirewall(t, eng, con, &model.FirewallConfig{Rules: []model.FirewallRule{{Seq: 1, Action: "drop", Protocol: "udp"}}})
	if _, err := eng.Commit(context.Background(), con, CommitOpts{}); err != nil {
		t.Fatal(err)
	}
	sess := Session{User: "admin", Source: "ssh"}
	setFirewall(t, eng, sess, nil) // 删除整段
	if _, err := eng.Commit(context.Background(), sess, CommitOpts{}); !errors.Is(err, ErrFirewallConfirmRequired) {
		t.Fatalf("删除防火墙段应要求 commit confirmed，得到 %v", err)
	}
}

// 不涉及防火墙的提交不得被误要求 confirmed（否则等于把自锁保护扩大到全量提交）。
func TestUnrelatedCommitNotGatedByFirewall(t *testing.T) {
	eng := fwEngine(t)
	con := Session{User: "admin", Source: "console"}
	setFirewall(t, eng, con, &model.FirewallConfig{Rules: []model.FirewallRule{{Seq: 1, Action: "drop", Protocol: "udp"}}})
	if _, err := eng.Commit(context.Background(), con, CommitOpts{}); err != nil {
		t.Fatal(err)
	}
	sess := Session{User: "admin", Source: "ssh"}
	if err := eng.Edit(sess); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := eng.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	cfg.System.IdleTimeoutMinutes = 35 // 只动无关字段，防火墙原样
	if err := eng.UpdateCandidate(sess, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Commit(context.Background(), sess, CommitOpts{}); err != nil {
		t.Fatalf("防火墙未变时不应要求 confirmed: %v", err)
	}
}
