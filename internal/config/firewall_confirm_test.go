package config

// 决策 #388：管理面主机防火墙变更的自锁保护——非 console 会话的防火墙变更（规则/默认策略，
// 含首次配置与删除）必须 commit confirmed；console 会话豁免；不涉及防火墙的提交不受影响。

import (
	"context"
	"errors"
	"fmt"
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

// TestConfirmedTimeoutRollbackNotifiesOnCommit（决策 #388 真机验证 round169 抓到的缺口）：
// confirmed 超时自动回滚改变了 committed 配置，却只落库、不走 OnCommitted ⇒ 宿主侧重收敛
// （防火墙/TLS/syslog/日志保留）不触发——防火墙场景的真机实锤是「配置已回滚、nft 表仍是
// policy drop」，管理面被锁死到重启。回滚必须与 Commit 走同一「已提交变更」通知。
func TestConfirmedTimeoutRollbackNotifiesOnCommit(t *testing.T) {
	var calls []string // "rev:user"
	store := openTestStore(t)
	clock := newFakeClock()
	if _, err := store.AppendRevision(mustJSON(baseCommitted()), clock.Now(), "初始基线", "admin"); err != nil {
		t.Fatalf("预置基线: %v", err)
	}
	e, err := NewEngine(store, &mockApplier{}, Options{
		Now:       clock.Now,
		AfterFunc: newTimerSink().after,
		OnCommitted: func(rev int, user string) {
			calls = append(calls, fmt.Sprintf("%d:%s", rev, user))
		},
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	// console 预置管理口（豁免路径，与 fwEngine 同口径）。
	con := Session{User: "admin", Source: "console"}
	if err := e.Edit(con); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := e.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	cfg.System.Management.Interface = "ens160"
	if err := e.UpdateCandidate(con, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Commit(context.Background(), con, CommitOpts{}); err != nil {
		t.Fatalf("预置管理口: %v", err)
	}
	// ssh 会话的防火墙变更，confirmed 提交（不确认 → 走超时回滚）。
	sess := Session{User: "admin", Source: "ssh"}
	setFirewall(t, e, sess, &model.FirewallConfig{Rules: []model.FirewallRule{
		{Seq: 100, Action: "accept", Source: "192.0.2.0/24", Protocol: "tcp", Port: 9999},
	}})
	if _, err := e.Commit(context.Background(), sess, CommitOpts{ConfirmedMinutes: 2}); err != nil {
		t.Fatalf("confirmed 提交: %v", err)
	}
	cf, err := store.GetConfirmed()
	if err != nil || cf == nil {
		t.Fatalf("在途 confirmed 应在场: %v", err)
	}
	// 与定时器同一路径的超时回滚（fakeClock 未到 deadline，直接调 doConfirmedRollback）。
	e.doConfirmedRollback(cf)

	// 断言：预置(1) + 防火墙提交(2) + 超时回滚(3) —— 第三次必须来自回滚（user=system）。
	if len(calls) != 3 {
		t.Fatalf("OnCommitted 应触发 3 次（含回滚一次），实际 %d 次：%v", len(calls), calls)
	}
	if !strings.HasSuffix(calls[2], ":system") {
		t.Fatalf("回滚的 OnCommitted 应以 system 身份通知，实际 %q", calls[2])
	}
	got, err := e.Committed()
	if err != nil {
		t.Fatal(err)
	}
	if got.System.Firewall != nil {
		t.Fatalf("回滚后 committed 不应再含防火墙段")
	}
}

// 决策 #392：确认守卫豁免内部维护/救援动作——**防火墙守卫矩阵**。
//
// 三类合成来源（恢复出厂 SourceZeroize / 恢复配置 SourceRestore / 重置数据分区
// SourceFormatData）各自有操作级双重确认（破坏性闸门），且 Restore 本身就是「防火墙自锁」的
// 救援路径，故在防火墙变更下**放行**（不要求 commit confirmed）；用户会话（ssh / api）
// 判据不变（仍拒），本地串口（console）豁免不变（决策 #388 原有口径）。
func TestFirewallConfirmGuardExemptsInternalMaintenanceSources(t *testing.T) {
	cases := []struct {
		source string
		reject bool
	}{
		{"ssh", true},
		{"api", true},
		{"console", false},
		{SourceZeroize, false},
		{SourceRestore, false},
		{SourceFormatData, false},
	}
	fw := &model.FirewallConfig{Rules: []model.FirewallRule{
		{Seq: 100, Action: "accept", Source: "192.0.2.0/24", Protocol: "tcp", Port: 9999},
	}}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			eng := fwEngine(t)
			sess := Session{User: "admin", Source: tc.source}
			setFirewall(t, eng, sess, fw)
			_, err := eng.Commit(context.Background(), sess, CommitOpts{})
			if tc.reject {
				if !errors.Is(err, ErrFirewallConfirmRequired) {
					t.Fatalf("source=%s 防火墙变更应要求 commit confirmed，得到 %v", tc.source, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("source=%s 应被豁免（不要求 confirmed），得到 %v", tc.source, err)
			}
		})
	}
}
