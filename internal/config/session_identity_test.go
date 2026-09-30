package config

// 决策 #317：会话与身份键分离（根治 R79-1）。
//
// 引擎级覆盖：同一用户在**同一接入源**下的两个会话（不同稳定 ID）互不干扰；
// 无稳定 ID 的旧式调用仍按身份键归并；进程重启（候选随内存消失）时遗留锁被释放，
// 新会话不再被旧会话 ID 挡在门外。

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestEngineSessionIdentitySeparatesSameUserSessions 同一身份键、不同会话 ID = 两把锁。
func TestEngineSessionIdentitySeparatesSameUserSessions(t *testing.T) {
	k := newEngineKit(t)
	a := Session{User: "admin", Source: "api", ID: "tok-a"}
	b := Session{User: "admin", Source: "api", ID: "tok-b"}

	if err := k.engine.Edit(a); err != nil {
		t.Fatalf("会话 A 取锁: %v", err)
	}
	// 同一身份键的会话 B 不得据此认为自己是持有者。
	if err := k.engine.Edit(b); !errors.Is(err, ErrLocked) {
		t.Fatalf("会话 B（同身份键）取锁应 ErrLocked，实得 %v", err)
	}
	if err := k.engine.UpdateCandidate(b, baseCommitted()); !errors.Is(err, ErrNotEditing) {
		t.Fatalf("会话 B 未持锁编辑应 ErrNotEditing，实得 %v", err)
	}

	cfg := baseCommitted()
	cfg.System.Hostname = "held-by-a"
	if err := k.engine.UpdateCandidate(a, cfg); err != nil {
		t.Fatalf("会话 A 写候选: %v", err)
	}
	views, err := k.engine.Sessions()
	if err != nil || len(views) != 1 {
		t.Fatalf("Sessions: %+v err=%v", views, err)
	}
	if v := views[0]; v.Holder != "admin@api" || v.SessionID != "tok-a" || v.User != "admin" || !v.Dirty {
		t.Fatalf("持锁会话视图应含标识/用户/脏标记: %+v", v)
	}

	// B 不能释放 / 丢弃 A 的锁（#317 的核心：登出只作用本会话）。
	if err := k.engine.Discard(b); !errors.Is(err, ErrNotEditing) {
		t.Fatalf("会话 B 不得丢弃会话 A 的候选，实得 %v", err)
	}
	if v, _, err := k.engine.Candidate(); err != nil || v.System.Hostname != "held-by-a" {
		t.Fatalf("会话 A 的候选被 B 动了: hostname=%q err=%v", hostnameOf(t, v), err)
	}

	// A 释放后 B 才可进入。
	if err := k.engine.Discard(a); err != nil {
		t.Fatalf("会话 A 丢弃: %v", err)
	}
	if err := k.engine.Edit(b); err != nil {
		t.Fatalf("A 释放后 B 应能取锁: %v", err)
	}
	if err := k.engine.Release(b); err != nil {
		t.Fatalf("会话 B 释放: %v", err)
	}
	if views, _ := k.engine.Sessions(); len(views) != 0 {
		t.Fatalf("全部释放后应无持锁会话: %+v", views)
	}
}

// TestEngineLegacySessionMergesByIdentity 无稳定 ID 的旧式调用仍按身份键归并（保守语义）。
func TestEngineLegacySessionMergesByIdentity(t *testing.T) {
	k := newEngineKit(t)
	legacy := Session{User: "system", Source: "console"} // ID 为空

	if err := k.engine.Edit(legacy); err != nil {
		t.Fatalf("旧式会话取锁: %v", err)
	}
	// 无 ID 的同一身份键重复 Edit 幂等。
	if err := k.engine.Edit(legacy); err != nil {
		t.Fatalf("旧式会话重复 Edit 应幂等: %v", err)
	}
	// 有稳定 ID 的同名会话是**另一**会话 → 被拒。
	if err := k.engine.Edit(Session{User: "system", Source: "console", ID: "x"}); !errors.Is(err, ErrLocked) {
		t.Fatalf("有 ID 的同身份键会话应被拒，实得 %v", err)
	}
	if err := k.engine.Release(legacy); err != nil {
		t.Fatalf("旧式会话释放: %v", err)
	}
}

// TestEngineRestartReleasesStaleLock nfvisd 重启后遗留锁被释放（候选随进程消失）。
func TestEngineRestartReleasesStaleLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nfvis.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if _, err := store.AppendRevision(mustJSON(baseCommitted()), newFakeClock().Now(), "基线", "admin"); err != nil {
		t.Fatalf("预置基线: %v", err)
	}

	e1, err := NewEngine(store, &mockApplier{}, Options{Now: newFakeClock().Now})
	if err != nil {
		t.Fatalf("NewEngine#1: %v", err)
	}
	if err := e1.Edit(Session{User: "admin", Source: "api", ID: "old-token"}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	e1.Close()
	if err := store.Close(); err != nil {
		t.Fatalf("Close store: %v", err)
	}

	// 重开（= nfvisd 重启）：遗留锁不应把新会话（新 token ID）挡在门外。
	store2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("重开 store: %v", err)
	}
	defer store2.Close()
	if _, err := NewEngine(store2, &mockApplier{}, Options{Now: newFakeClock().Now}); err != nil {
		t.Fatalf("NewEngine#2: %v", err)
	}
	if li, _ := store2.GetLock(); li != nil {
		t.Fatalf("重启装配后应已释放遗留锁，实得 %+v", li)
	}
}
