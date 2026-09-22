package config

// 发现 #12(a)：管理口自锁保护（FR-CFG-012）此前在「基线还没有管理口配置」时直接放行，
// 于是**首次声明**管理口（含地址/网关这类可能切断当前 SSH 会话的改动）不需要 commit confirmed。
// 本文件锁住修复后的语义，并守住「不涉及管理口的提交不应被误要求 confirmed」。

import (
	"context"
	"errors"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// mgmtSet 在候选里设置管理口配置（保持其它字段不动）。
func mgmtSet(t *testing.T, eng *Engine, sess Session, mgmt *model.MgmtConfig) {
	t.Helper()
	cfg, _, err := eng.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.System == nil {
		cfg.System = &model.SystemConfig{}
	}
	cfg.System.Management = mgmt
	if err := eng.UpdateCandidate(sess, cfg); err != nil {
		t.Fatal(err)
	}
}

// newEmptyEngine 基线**不含**任何管理口配置（模拟"本机还没配过管理口"）。
// 注意：共享的 newEngineWithExternals 预置的基线里已带 management，用它测不出这个缺口。
func newEmptyEngine(t *testing.T) *Engine {
	t.Helper()
	store := openTestStore(t)
	eng, err := NewEngine(store, &mockApplier{}, Options{Now: newFakeClock().Now})
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err := eng.Committed(); err != nil || cfg.System != nil {
		t.Fatalf("空库基线应无 system 段: %+v %v", cfg.System, err)
	}
	return eng
}

// 首次声明管理口（基线**没有** management 字段）也必须 commit confirmed：
// 地址/网关正是自锁保护要防的那类改动（决策 #71：改管理网卡可能切断当前 SSH 会话）。
func TestFirstManagementDeclarationRequiresConfirmed(t *testing.T) {
	eng := newEmptyEngine(t)
	sess := Session{User: "admin", Source: "ssh"}
	if err := eng.Edit(sess); err != nil {
		t.Fatal(err)
	}
	mgmtSet(t, eng, sess, &model.MgmtConfig{Address: "192.168.1.10/24"})

	if _, err := eng.Commit(context.Background(), sess, CommitOpts{}); !errors.Is(err, ErrConfirmRequired) {
		t.Fatalf("首次声明管理口应要求 commit confirmed: %v", err)
	}
	// 用 confirmed 提交应当成功，随后可确认
	if _, err := eng.Commit(context.Background(), sess, CommitOpts{ConfirmedMinutes: 5}); err != nil {
		t.Fatalf("commit confirmed 应成功: %v", err)
	}
	if err := eng.ConfirmCommit(sess); err != nil {
		t.Fatalf("确认应成功: %v", err)
	}
	cfg, err := eng.Committed()
	if err != nil || cfg.System == nil || cfg.System.Management == nil ||
		cfg.System.Management.Address != "192.168.1.10/24" {
		t.Fatalf("管理口地址应已生效: %+v %v", cfg.System, err)
	}
}

// 首次声明管理**网卡名**同样算管理口变更（决策 #71 明确该字段受同一保护）。
func TestFirstManagementIfaceNameRequiresConfirmed(t *testing.T) {
	eng := newEmptyEngine(t)
	sess := Session{User: "admin", Source: "ssh"}
	if err := eng.Edit(sess); err != nil {
		t.Fatal(err)
	}
	mgmtSet(t, eng, sess, &model.MgmtConfig{Interface: "ens160"})
	if _, err := eng.Commit(context.Background(), sess, CommitOpts{}); !errors.Is(err, ErrConfirmRequired) {
		t.Fatalf("首次声明管理网卡名应要求 commit confirmed: %v", err)
	}
}

// 不涉及管理口的提交不得被误要求 confirmed（否则等于把自锁保护扩大到全量提交）。
func TestUnrelatedCommitDoesNotRequireConfirmed(t *testing.T) {
	eng := newEmptyEngine(t)
	sess := Session{User: "admin", Source: "ssh"}
	if err := eng.Edit(sess); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := eng.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.System == nil {
		cfg.System = &model.SystemConfig{}
	}
	cfg.System.IdleTimeoutMinutes = 25
	if err := eng.UpdateCandidate(sess, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Commit(context.Background(), sess, CommitOpts{}); err != nil {
		t.Fatalf("与管理口无关的提交不应要求 confirmed: %v", err)
	}
}

// 基线的管理口保持不变时，改别的字段同样不要求 confirmed。
func TestBaselineMgmtUnchangedDoesNotRequireConfirmed(t *testing.T) {
	eng, _, _ := newEngineWithExternals(t, nil, nil) // 基线自带 management
	sess := Session{User: "admin", Source: "ssh"}
	if err := eng.Edit(sess); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := eng.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	cfg.System.IdleTimeoutMinutes = 30 // 只动无关字段，management 原样
	if err := eng.UpdateCandidate(sess, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Commit(context.Background(), sess, CommitOpts{}); err != nil {
		t.Fatalf("管理口未变时不应要求 confirmed: %v", err)
	}
}

// 经网络接入的会话都受约束（FR-CFG-012，决策 #121 扩围）：ssh 与 api（REST/Web 控制台）
// 都依赖管理网连通性，改管理口可能切断自己的管理路径；本地串口不依赖管理网，保持豁免。
func TestNetworkSessionsGatedConsoleExempt(t *testing.T) {
	for _, src := range []string{"ssh", "api"} {
		eng := newEmptyEngine(t)
		sess := Session{User: "admin", Source: src}
		if err := eng.Edit(sess); err != nil {
			t.Fatal(err)
		}
		mgmtSet(t, eng, sess, &model.MgmtConfig{Interface: "ens160"})
		if _, err := eng.Commit(context.Background(), sess, CommitOpts{}); !errors.Is(err, ErrConfirmRequired) {
			t.Fatalf("source=%s 变更管理口应要求 commit confirmed，得到 %v", src, err)
		}
		// 带 confirmed_minutes 即放行（待确认状态）
		res, err := eng.Commit(context.Background(), sess, CommitOpts{ConfirmedMinutes: 10})
		if err != nil {
			t.Fatalf("source=%s commit confirmed 应放行: %v", src, err)
		}
		if res.ConfirmedUntil == nil {
			t.Fatalf("source=%s 应返回待确认截止时间", src)
		}
	}

	// 本地串口豁免：不依赖管理网连通性。
	eng := newEmptyEngine(t)
	con := Session{User: "admin", Source: "console"}
	if err := eng.Edit(con); err != nil {
		t.Fatal(err)
	}
	mgmtSet(t, eng, con, &model.MgmtConfig{Interface: "ens160"})
	if _, err := eng.Commit(context.Background(), con, CommitOpts{}); err != nil {
		t.Fatalf("本地串口不应被要求 confirmed: %v", err)
	}
}
