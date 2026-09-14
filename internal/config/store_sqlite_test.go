package config

import (
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "nfvis.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStoreRevisions(t *testing.T) {
	s := openTestStore(t)

	rev, data, err := s.LatestRevision()
	if err != nil || rev != 0 || data != nil {
		t.Fatalf("空库应返回 rev=0 data=nil，实际 rev=%d err=%v", rev, err)
	}

	now := time.Now()
	r1, err := s.AppendRevision([]byte(`{"system":{"hostname":"a"}}`), now, "init")
	if err != nil || r1 != 1 {
		t.Fatalf("AppendRevision: rev=%d err=%v", r1, err)
	}
	r2, err := s.AppendRevision([]byte(`{"system":{"hostname":"b"}}`), now, "second")
	if err != nil || r2 != 2 {
		t.Fatalf("AppendRevision: rev=%d err=%v", r2, err)
	}

	rev, data, err = s.LatestRevision()
	if err != nil || rev != 2 || string(data) != `{"system":{"hostname":"b"}}` {
		t.Fatalf("LatestRevision: rev=%d data=%s err=%v", rev, data, err)
	}

	got, err := s.LoadRevision(1)
	if err != nil || string(got) != `{"system":{"hostname":"a"}}` {
		t.Fatalf("LoadRevision(1): %s err=%v", got, err)
	}
	if _, err := s.LoadRevision(99); !errors.Is(err, ErrNoRevision) {
		t.Fatalf("LoadRevision(99) 应返回 ErrNoRevision，实际 %v", err)
	}
}

func TestStorePruneRevisions(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	for i := 0; i < 60; i++ {
		if _, err := s.AppendRevision([]byte(`{}`), now, ""); err != nil {
			t.Fatalf("AppendRevision #%d: %v", i, err)
		}
	}
	if err := s.PruneRevisions(51); err != nil {
		t.Fatalf("PruneRevisions: %v", err)
	}
	// 保留 51 份（当前 + 50 份历史，FR-CFG-005）：rev 10..60 存活，1..9 被清理
	if _, err := s.LoadRevision(9); !errors.Is(err, ErrNoRevision) {
		t.Fatalf("rev 9 应被清理，实际可加载")
	}
	if _, err := s.LoadRevision(10); err != nil {
		t.Fatalf("rev 10 应保留: %v", err)
	}
	rev, _, _ := s.LatestRevision()
	if rev != 60 {
		t.Fatalf("最新修订应仍为 60，实际 %d", rev)
	}
}

func TestStoreLock(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()

	if err := s.AcquireLock("admin@ssh", now); err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if err := s.AcquireLock("netop@ssh", now); !errors.Is(err, ErrLocked) {
		t.Fatalf("重复加锁应返回 ErrLocked，实际 %v", err)
	}

	li, err := s.GetLock()
	if err != nil || li == nil || li.Holder != "admin@ssh" {
		t.Fatalf("GetLock: %+v err=%v", li, err)
	}

	if err := s.RefreshLock("admin@ssh", now.Add(time.Minute)); err != nil {
		t.Fatalf("RefreshLock: %v", err)
	}
	li, _ = s.GetLock()
	if !li.LastActivity.Equal(now.Add(time.Minute)) {
		t.Fatalf("RefreshLock 未更新活动时间: %+v", li)
	}

	if err := s.ReleaseLock("netop@ssh"); err == nil {
		t.Fatalf("非持有者释放锁应报错")
	}
	if err := s.ReleaseLock("admin@ssh"); err != nil {
		t.Fatalf("ReleaseLock: %v", err)
	}
	if li, _ := s.GetLock(); li != nil {
		t.Fatalf("释放后锁应为空，实际 %+v", li)
	}
}

func TestStoreConfirmed(t *testing.T) {
	s := openTestStore(t)
	deadline := time.Now().Add(10 * time.Minute)

	if cf, _ := s.GetConfirmed(); cf != nil {
		t.Fatalf("初始应无 confirmed 待确认项")
	}
	if err := s.SetConfirmed(3, deadline, "admin@ssh"); err != nil {
		t.Fatalf("SetConfirmed: %v", err)
	}
	cf, err := s.GetConfirmed()
	if err != nil || cf.BaseRev != 3 || cf.Holder != "admin@ssh" || !cf.Deadline.Equal(deadline) {
		t.Fatalf("GetConfirmed: %+v err=%v", cf, err)
	}
	if err := s.ClearConfirmed(); err != nil {
		t.Fatalf("ClearConfirmed: %v", err)
	}
	if cf, _ := s.GetConfirmed(); cf != nil {
		t.Fatalf("清除后应为空")
	}
}

func TestStoreAudit(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	entries := []AuditEntry{
		{Time: now, User: "admin", Action: "config.commit", Detail: "diff-1", Result: "success"},
		{Time: now.Add(time.Second), User: "netop", Action: "config.commit", Detail: "diff-2", Result: "failure"},
		{Time: now.Add(2 * time.Second), User: "admin", Action: "config.rollback", Detail: "rollback 1", Result: "success"},
	}
	for _, e := range entries {
		if err := s.AppendAudit(e); err != nil {
			t.Fatalf("AppendAudit: %v", err)
		}
	}
	got, err := s.ListAudit(2, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(got) != 2 || got[0].Action != "config.rollback" || got[1].Action != "config.commit" {
		t.Fatalf("ListAudit 应按时间倒序返回 2 条: %+v", got)
	}
}

func TestStoreSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nfvis.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != CurrentSchemaVersion {
		t.Fatalf("新库版本应为 %d，实际 %d err=%v", CurrentSchemaVersion, v, err)
	}
	if _, err := s.AppendRevision([]byte(`{}`), time.Now(), ""); err != nil {
		t.Fatalf("AppendRevision: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 重新打开：幂等，数据保留
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	defer s2.Close()
	rev, _, err := s2.LatestRevision()
	if err != nil || rev != 1 {
		t.Fatalf("重开后数据应保留: rev=%d err=%v", rev, err)
	}

	// 过新的 schema 版本 → 拒绝打开（版本锁定，规格书 §2.3.4）
	s2.db.Exec(`PRAGMA user_version = 99`)
	if _, err := OpenStore(path); err == nil {
		t.Fatalf("过新版本应拒绝启动")
	}
}

// FR-API-007（决策 #70）：审计分页必须真正支持 offset——
// 契约早已声明 limit/offset，实现此前静默忽略 offset（返回同一页），属契约漂移。
func TestStoreListAuditOffset(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "nfvis.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	base := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if err := s.AppendAudit(AuditEntry{
			Time: base.Add(time.Duration(i) * time.Minute), User: "u",
			Action: "act", Detail: strconv.Itoa(i), Result: "success",
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	// 倒序：最新在前 → 第 0 页是 4,3，第 1 页（offset=2）是 2,1
	page0, err := s.ListAudit(2, 0)
	if err != nil {
		t.Fatalf("ListAudit(2,0): %v", err)
	}
	page1, err := s.ListAudit(2, 2)
	if err != nil {
		t.Fatalf("ListAudit(2,2): %v", err)
	}
	if len(page0) != 2 || len(page1) != 2 {
		t.Fatalf("页大小错误: %d %d", len(page0), len(page1))
	}
	if page0[0].Detail != "4" || page0[1].Detail != "3" {
		t.Fatalf("第 0 页内容错误: %v %v", page0[0].Detail, page0[1].Detail)
	}
	if page1[0].Detail != "2" || page1[1].Detail != "1" {
		t.Fatalf("offset 未生效（应跳过前 2 条）: %v %v", page1[0].Detail, page1[1].Detail)
	}
	// offset 超出范围 → 空页而非报错
	if last, err := s.ListAudit(2, 99); err != nil || len(last) != 0 {
		t.Fatalf("超范围 offset 应返回空页: %v %v", last, err)
	}
	// 负 offset 按 0 处理
	if neg, err := s.ListAudit(2, -5); err != nil || len(neg) != 2 || neg[0].Detail != "4" {
		t.Fatalf("负 offset 应按 0 处理: %+v %v", neg, err)
	}
}
