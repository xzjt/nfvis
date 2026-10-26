package config

// round49 收尾：`clearConfirmedLocked`（确认/取消在途 confirmed 的路径）此前 `_ = e.store.ClearConfirmed()`
// —— 与 #3 同一类「持久化步骤失败被吞掉」。现场：确认（显式 `confirm`、新 commit 的隐式确认、
// rollback 取消等待）成功后**清除失败**时，调用方拿到的是成功，而在途行还在库里、定时器还挂着
// ⇒ 一次**已被确认**的变更会在到期后被自动回滚（配置突然变了回去，且无人知道为什么）。
//
// 修法：该路径的清除失败**上抛给调用方**（文案说明「确认未能持久化，超时回滚仍可能在到期后触发」），
// 并把那条审计记成 failure（不得谎报成功）；定时器与库里的在途行保持一致（都不动），
// 由调用方把风险报给操作者。
//
// 失败注入：`confirmed_pending` 上的 `BEFORE DELETE` 触发器（`RAISE(ABORT, …)`），
// 与 fix_r49_store_commit_txn_test.go 里用过的 INSERT 触发器同一手法（确定性，不依赖关库）。

import (
	"context"
	"strings"
	"testing"
	"time"
)

// injectConfirmedClearFailure 让「清除在途 confirmed」这一步确定性失败。
func injectConfirmedClearFailure(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(
		`CREATE TRIGGER r49_nodelete BEFORE DELETE ON confirmed_pending BEGIN SELECT RAISE(ABORT, 'r49 nodelete'); END`,
	); err != nil {
		t.Fatalf("建失败注入触发器: %v", err)
	}
}

// confirmedClearFailureKit 一台已在 confirmed 保护期的引擎（baseline rev1 → 提交 rev2，10 分钟窗口）。
func confirmedClearFailureKit(t *testing.T) *engineKit {
	t.Helper()
	k := newEngineKit(t)
	sess := Session{User: "admin", Source: "ssh"}
	k.edit(t, "admin", "ssh")
	cfg := baseCommitted()
	cfg.System.Hostname = "risky"
	if err := k.engine.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	if _, err := k.engine.Commit(context.Background(), sess, CommitOpts{ConfirmedMinutes: 10}); err != nil {
		t.Fatalf("Commit confirmed: %v", err)
	}
	return k
}

// TestConfirmCommitReportsUnpersistedClear：显式确认时清除失败 ⇒ 报错（说清风险）、
// 不谎报成功、在途保护仍在（到期确实会回滚——这正是必须报出来的原因）。
func TestConfirmCommitReportsUnpersistedClear(t *testing.T) {
	k := confirmedClearFailureKit(t)
	injectConfirmedClearFailure(t, k.store)

	err := k.engine.ConfirmCommit(Session{User: "admin", Source: "ssh"})
	if err == nil {
		t.Fatalf("清除在途 confirmed 失败时确认必须报错（不得谎报成功）")
	}
	if !strings.Contains(err.Error(), "确认未能持久化") || !strings.Contains(err.Error(), "超时回滚") {
		t.Fatalf("错误文案要说清「确认未能持久化、回滚仍可能触发」: %v", err)
	}
	if cf, _ := k.store.GetConfirmed(); cf == nil {
		t.Fatalf("清除失败时在途保护应如实仍在库里: %+v", cf)
	}
	audit, _ := k.store.ListAudit(10, 0)
	if len(audit) == 0 || audit[0].Action != "config.confirm" || audit[0].Result != "failure" {
		t.Fatalf("清除失败应入一条 config.confirm/failure 审计（不得记成功）: %+v", audit)
	}

	// 风险是真实的：确认没能落下去 ⇒ 到期仍会按在途保护触发反向回滚（这正是必须报给操作者、
	// 而不是静默成功的理由）。注意本用例里裁剪触发器仍挡着清除，故回滚只能走到「已下发」——
	// 这恰好也说明「在途行还在库里」这件事本身就是要报出来的风险。
	k.clock.Advance(10 * time.Minute)
	k.timers.FireAll()
	if n := k.applier.calls; n != 2 {
		t.Fatalf("确认未能持久化 ⇒ 到期应仍触发一次反向回滚下发（共 2 次），实际 %d 次", n)
	}
}

// TestCommitImplicitConfirmClearFailureAbortsBeforeApply：新 commit 的隐式确认清除失败 ⇒
// 提交在**下发之前**中止（数据面与库都不动、candidate 保留），且如实报出风险。
func TestCommitImplicitConfirmClearFailureAbortsBeforeApply(t *testing.T) {
	k := confirmedClearFailureKit(t)
	sess := Session{User: "admin", Source: "ssh"}
	cfg := baseCommitted()
	cfg.System.Hostname = "stable"
	if err := k.engine.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	injectConfirmedClearFailure(t, k.store)

	_, err := k.engine.Commit(context.Background(), sess, CommitOpts{})
	if err == nil {
		t.Fatalf("隐式确认清除失败时提交必须报错（不得谎报成功）")
	}
	if n := k.applier.calls; n != 1 {
		t.Fatalf("应只有第一次 confirmed 提交下发过一次，实际 %d 次（中止必须发生在下发之前）", n)
	}
	if !strings.Contains(err.Error(), "确认未能持久化") {
		t.Fatalf("错误文案要说清原因: %v", err)
	}
	if rev, _, _ := k.store.LatestRevision(); rev != 2 {
		t.Fatalf("中止的提交不得落库，实际 rev=%d", rev)
	}
	if got, _ := k.engine.Committed(); hostnameOf(t, got) != "risky" {
		t.Fatalf("中止的提交不得改变 committed，实际 %s", hostnameOf(t, got))
	}
	if cand, dirty, cerr := k.engine.Candidate(); cerr != nil || !dirty || hostnameOf(t, cand) != "stable" {
		t.Fatalf("candidate 应原样保留（可重提）: %v dirty=%v %s", cerr, dirty, hostnameOf(t, cand))
	}
	if cf, _ := k.store.GetConfirmed(); cf == nil {
		t.Fatalf("在途保护应如实仍在库里: %+v", cf)
	}
}
