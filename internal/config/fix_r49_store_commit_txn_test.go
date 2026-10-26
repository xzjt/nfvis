package config

// round49 缺陷 #3（中危）：提交成功后落库**不是单个事务**。
//
// 现场：`Engine.Commit` 依次独立调用 `AppendRevision` → `PruneRevisions` → `SetConfirmed`，
// 而配置库全文没有 `BeginTx` —— 任一步失败（磁盘写满、进程被杀、SQL 报错）都会留下
// 「数据面已生效、修订已写、调用方却收到失败、**confirmed 保护未建立**」的中间态：
// 数据面在跑新版、配置面以为这次提交失败了，而且危险变更的超时保护根本不存在。
//
// 失败注入手法：在 `confirmed_pending` 上建一个 `BEFORE INSERT` 触发器 `RAISE(ABORT, …)`，
// 让「写 confirmed」这一步**确定性地**失败（不依赖关库/只读文件系统这类不稳定手段），
// 再断言新修订与修剪都没有落进库里（即整个事务被回滚）。

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedRevisions 预置 n 份修订，返回一个基准时间。
func seedRevisions(t *testing.T, s *Store, n int) time.Time {
	t.Helper()
	base := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		if _, err := s.AppendRevision([]byte(`{"note":"seed"}`), base, "", "admin"); err != nil {
			t.Fatalf("预置修订 #%d: %v", i, err)
		}
	}
	return base
}

func TestStoreAppendCommitAtomicOnConfirmFailure(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "nfvis.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	base := seedRevisions(t, s, 55)

	// 让「写 confirmed」这一步失败
	if _, err := s.db.Exec(
		`CREATE TRIGGER r49_boom BEFORE INSERT ON confirmed_pending BEGIN SELECT RAISE(ABORT, 'r49 boom'); END`,
	); err != nil {
		t.Fatalf("建失败注入触发器: %v", err)
	}
	_, err = s.AppendCommit([]byte(`{"system":{"hostname":"risky"}}`), base, "commit confirmed", "admin",
		&ConfirmedInfo{Deadline: base.Add(5 * time.Minute), Holder: "admin@ssh"}, storeKeepRevisions)
	if err == nil {
		t.Fatalf("写 confirmed 失败时 AppendCommit 必须报错（不得谎报成功）")
	}
	if !strings.Contains(err.Error(), "r49 boom") {
		t.Fatalf("错误应带上底层原因: %v", err)
	}

	// 中间态不存在：修订没被写入、修剪没发生
	rev, data, err := s.LatestRevision()
	if err != nil || rev != 55 || strings.Contains(string(data), "risky") {
		t.Fatalf("事务回滚后不得残留新修订: rev=%d data=%s err=%v", rev, data, err)
	}
	if _, err := s.LoadRevision(56); !errors.Is(err, ErrNoRevision) {
		t.Fatalf("rev 56 不应存在: %v", err)
	}
	if _, err := s.LoadRevision(5); err != nil {
		t.Fatalf("修剪与写入同事务，回滚后 rev 5 应仍在: %v", err)
	}
	if cf, _ := s.GetConfirmed(); cf != nil {
		t.Fatalf("confirmed 不应被建立: %+v", cf)
	}

	// 拆掉注入后同一调用成功：修订 + 修剪 + confirmed 一起提交
	if _, err := s.db.Exec(`DROP TRIGGER r49_boom`); err != nil {
		t.Fatalf("移除触发器: %v", err)
	}
	rev, err = s.AppendCommit([]byte(`{"system":{"hostname":"risky"}}`), base, "commit confirmed", "admin",
		&ConfirmedInfo{Deadline: base.Add(5 * time.Minute), Holder: "admin@ssh"}, storeKeepRevisions)
	if err != nil {
		t.Fatalf("AppendCommit: %v", err)
	}
	if rev != 56 {
		t.Fatalf("新修订号应为 56，实际 %d", rev)
	}
	cf, err := s.GetConfirmed()
	if err != nil || cf == nil {
		t.Fatalf("confirmed 应已建立: %+v err=%v", cf, err)
	}
	if cf.BaseRev != 55 || cf.Holder != "admin@ssh" {
		t.Fatalf("confirmed 基线应为本修订的前一版（55）、持有者为提交会话: %+v", cf)
	}
	if _, err := s.LoadRevision(5); !errors.Is(err, ErrNoRevision) {
		t.Fatalf("修剪应随同事务生效：rev 5 应被清理")
	}
	if _, err := s.LoadRevision(6); err != nil {
		t.Fatalf("rev 6 应在保留窗口内: %v", err)
	}
	if got, err := s.LoadRevision(56); err != nil || !strings.Contains(string(got), "risky") {
		t.Fatalf("rev 56 应为本次提交的配置: %s err=%v", got, err)
	}
}

// TestEngineCommitNoMiddleStateOnStoreFailure：端到端——引擎提交时若「写 confirmed」失败，
// 不得留下「修订已写、confirmed 未建立」的中间态（数据面此时已生效，中间态会让配置面
// 以为提交失败、而危险变更的超时保护根本不存在）。失败注入仍在 confirmed_pending 上。
func TestEngineCommitNoMiddleStateOnStoreFailure(t *testing.T) {
	k := newEngineKit(t)
	sess := Session{User: "admin", Source: "ssh"}
	k.edit(t, "admin", "ssh")

	cfg := baseCommitted()
	cfg.System.Hostname = "risky"
	if err := k.engine.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	if _, err := k.store.db.Exec(
		`CREATE TRIGGER r49_boom2 BEFORE INSERT ON confirmed_pending BEGIN SELECT RAISE(ABORT, 'r49 boom'); END`,
	); err != nil {
		t.Fatalf("建失败注入触发器: %v", err)
	}

	if _, err := k.engine.Commit(context.Background(), sess, CommitOpts{ConfirmedMinutes: 10}); err == nil {
		t.Fatalf("落库失败时 commit 必须报错（不得谎报成功）")
	}
	rev, data, _ := k.store.LatestRevision()
	if rev != 1 || strings.Contains(string(data), "risky") {
		t.Fatalf("落库失败不得留下中间态（修订已写、confirmed 未建立）: rev=%d data=%s", rev, data)
	}
	if cf, _ := k.store.GetConfirmed(); cf != nil {
		t.Fatalf("confirmed 不得被建立: %+v", cf)
	}
	// 失败要留得下痕迹（数据面此时已下发，读审计的人必须看得出这次动作做了一半）
	audit, _ := k.store.ListAudit(10, 0)
	if len(audit) == 0 || audit[0].Result != "failure" || !strings.Contains(audit[0].Detail, "落库失败") {
		t.Fatalf("落库失败应入一条 failure 审计并点名原因: %+v", audit)
	}
	// candidate 保留（操作者改完可重提），锁仍在
	if _, dirty, err := k.engine.Candidate(); err != nil || !dirty {
		t.Fatalf("失败后 candidate 应保留且仍为脏: %v dirty=%v", err, dirty)
	}
}

// TestStoreAppendCommitNilConfirmClears：confirm 为 nil 时同一事务内清除在途 confirmed
// （无待确认的提交与自动回滚走这条路径；清一个不存在的行是幂等空操作）。
func TestStoreAppendCommitNilConfirmClears(t *testing.T) {
	s := openTestStore(t)
	base := seedRevisions(t, s, 1)
	if err := s.SetConfirmed(1, base.Add(time.Minute), "admin@ssh"); err != nil {
		t.Fatalf("SetConfirmed: %v", err)
	}

	rev, err := s.AppendCommit([]byte(`{"system":{"hostname":"base"}}`), base, "回滚", "system", nil, storeKeepRevisions)
	if err != nil {
		t.Fatalf("AppendCommit: %v", err)
	}
	if rev != 2 {
		t.Fatalf("应追加为 rev 2，实际 %d", rev)
	}
	if cf, _ := s.GetConfirmed(); cf != nil {
		t.Fatalf("confirm=nil 应在同一事务内清掉在途 confirmed: %+v", cf)
	}
	// 无在途 confirmed 时同样不报错（幂等）
	if _, err := s.AppendCommit([]byte(`{}`), base, "", "admin", nil, storeKeepRevisions); err != nil {
		t.Fatalf("无在途 confirmed 时清除应为空操作: %v", err)
	}
}
