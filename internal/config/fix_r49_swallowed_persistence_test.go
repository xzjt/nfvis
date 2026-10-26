package config

// round49 收尾（主会话裁定）：把 engine.go 里剩下三处「吞掉持久化错误」的点一并收口。
//
//	① `sweepLocked` 的 `_ = e.store.ReleaseLock(...)`（engine.go ~1006）：释放失败照样记
//	   `config.lock-timeout/success` 并发「candidate 已释放」事件 ⇒ **假成功**——锁行仍在、
//	   他会话仍被 409 挡住，而操作者看到的是「已释放」。
//	② `appendAudit` 丢掉 `Store.AppendAudit` 的错误 ⇒ 审计（本轮 #1 之后操作者唯一的持久
//	   事实源）写不进去时**完全无痕**。
//	③ `Edit` 失败分支的 `_ = e.store.ReleaseLock(h, sess.ID)`（engine.go ~368）⇒ 刚拿到又
//	   立刻要放弃的锁释放失败时无声无息。
//
// 注入手法统一用 SQLite 触发器（确定性、不依赖关库）：
//   - 锁释放失败：`candidate_lock` 上的 `BEFORE DELETE … RAISE(ABORT,'r49 nodel')`；
//   - 审计写失败：`audit_log` 上的 `BEFORE INSERT … RAISE(ABORT,'r49 noaudit')`。

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// injectLockDeleteFailure 让「释放会话锁」这一步确定性失败。
func injectLockDeleteFailure(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(
		`CREATE TRIGGER r49_nodel BEFORE DELETE ON candidate_lock BEGIN SELECT RAISE(ABORT, 'r49 nodel'); END`,
	); err != nil {
		t.Fatalf("建锁释放失败触发器: %v", err)
	}
}

// injectAuditInsertFailure 让「写审计」这一步确定性失败。
func injectAuditInsertFailure(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(
		`CREATE TRIGGER r49_noaudit BEFORE INSERT ON audit_log BEGIN SELECT RAISE(ABORT, 'r49 noaudit'); END`,
	); err != nil {
		t.Fatalf("建审计失败触发器: %v", err)
	}
}

// ---------- ① 空闲锁释放失败不得记成功 ----------

func TestSweepLockReleaseFailureNotReportedAsSuccess(t *testing.T) {
	k := newEngineKit(t)
	sess := Session{User: "admin", Source: "ssh"}
	k.edit(t, "admin", "ssh")
	cfg := baseCommitted()
	cfg.System.Hostname = "touched"
	if err := k.engine.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	injectLockDeleteFailure(t, k.store)

	// 推过脏锁阈值（10 分钟）→ 巡检尝试释放，失败
	k.clock.Advance(11 * time.Minute)
	if _, err := k.engine.Sessions(); err != nil { // 任意见里入口即触发巡检
		t.Fatalf("Sessions: %v", err)
	}

	// 锁行仍在（释放没成功）
	li, err := k.store.GetLock()
	if err != nil || li == nil {
		t.Fatalf("释放失败时锁行应仍在: %+v err=%v", li, err)
	}
	// 不得发「空闲超时已释放」的成功事件
	for _, ev := range *k.events {
		if ev.Type == EventLockTimeout {
			t.Fatalf("释放失败不得发「空闲超时已释放」事件: %+v", ev)
		}
	}
	// 必须有 failure 审计，且点名后果与出路
	audit, err := k.store.ListAudit(20, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	failed := firstLockTimeoutAudit(audit, "failure")
	if failed == nil {
		t.Fatalf("释放失败必须留一条 config.lock-timeout/failure 审计: %+v", audit)
	}
	if !strings.Contains(failed.Detail, "r49 nodel") {
		t.Fatalf("失败审计要带底层错误: %q", failed.Detail)
	}
	if !strings.Contains(failed.Detail, "unlock") {
		t.Fatalf("失败审计要给照做路径（unlock）: %q", failed.Detail)
	}
	if firstLockTimeoutAudit(audit, "success") != nil {
		t.Fatalf("释放失败不得留下成功审计: %+v", audit)
	}

	// 每轮巡检不得重复刷：再走几个入口，失败审计仍只有一条、失败事件仍只有一个
	for i := 0; i < 2; i++ {
		if _, err := k.engine.Committed(); err != nil {
			t.Fatalf("Committed: %v", err)
		}
		if _, _, err := k.engine.Candidate(); err != nil {
			t.Fatalf("Candidate: %v", err)
		}
	}
	audit, _ = k.store.ListAudit(50, 0)
	if n := countLockTimeoutAudit(audit, "failure"); n != 1 {
		t.Fatalf("同一会话连续释放失败只应记一条 failure 审计（防刷屏），实际 %d 条", n)
	}
	nEv := 0
	for _, ev := range *k.events {
		if ev.Type == EventLockReleaseFailed {
			nEv++
		}
	}
	if nEv != 1 {
		t.Fatalf("同一会话连续释放失败只应发一个失败事件，实际 %d 个", nEv)
	}

	// 恢复后（拆触发器）巡检能真正释放，且**内存与库一致**：锁行消失、编辑态也没了。
	// 注意用带巡检的入口（Sessions/Candidate；`Committed()` 是纯读、有意不巡检）。
	if _, err := k.store.db.Exec(`DROP TRIGGER r49_nodel`); err != nil {
		t.Fatalf("拆触发器: %v", err)
	}
	if _, err := k.engine.Sessions(); err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if li, _ := k.store.GetLock(); li != nil {
		t.Fatalf("恢复后巡检应真正释放锁: %+v", li)
	}
	if _, _, err := k.engine.Candidate(); !errors.Is(err, ErrNotEditing) {
		t.Fatalf("恢复后内存态也应无编辑会话（内存与库一致）: %v", err)
	}
}

// firstLockTimeoutAudit 取第一条指定结果的 config.lock-timeout 审计（无则 nil）。
func firstLockTimeoutAudit(audit []AuditEntry, result string) *AuditEntry {
	for i := range audit {
		if audit[i].Action == "config.lock-timeout" && audit[i].Result == result {
			return &audit[i]
		}
	}
	return nil
}

func countLockTimeoutAudit(audit []AuditEntry, result string) int {
	n := 0
	for _, a := range audit {
		if a.Action == "config.lock-timeout" && a.Result == result {
			n++
		}
	}
	return n
}

// TestSweepLockReleaseFailureKeepsMemoryConsistent：释放失败时**内存态不清**（候选不被丢弃），
// 使「内存认为已释放、实际仍被占」这一错配不可能出现。
func TestSweepLockReleaseFailureKeepsMemoryConsistent(t *testing.T) {
	k := newEngineKit(t)
	sess := Session{User: "admin", Source: "ssh"}
	k.edit(t, "admin", "ssh")
	cfg := baseCommitted()
	cfg.System.Hostname = "touched"
	if err := k.engine.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	injectLockDeleteFailure(t, k.store)

	k.clock.Advance(11 * time.Minute)
	if _, _, err := k.engine.Candidate(); err != nil {
		t.Fatalf("释放失败时本会话应仍持有候选（内存与库一致）: %v", err)
	}
}

// ---------- ② 审计写失败必须有出口 ----------

func TestAuditWriteFailureReachesHook(t *testing.T) {
	store := openTestStore(t)
	clock := newFakeClock()
	if _, err := store.AppendRevision(mustJSON(baseCommitted()), clock.Now(), "初始基线", "admin"); err != nil {
		t.Fatalf("预置基线: %v", err)
	}
	type report struct {
		entry AuditEntry
		err   error
	}
	var got []report
	e, err := NewEngine(store, &mockApplier{}, Options{
		Now: clock.Now,
		OnAuditError: func(en AuditEntry, err error) {
			got = append(got, report{en, err})
		},
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	injectAuditInsertFailure(t, store)

	e.Audit("admin", "system.round49.probe", "审计写入失败探针", "success")
	if len(got) != 1 {
		t.Fatalf("审计写失败应上报一次钩子，实际 %d 次", len(got))
	}
	if got[0].entry.Action != "system.round49.probe" || got[0].entry.User != "admin" {
		t.Fatalf("钩子应收到那条写入失败的 entry: %+v", got[0].entry)
	}
	if got[0].err == nil || !strings.Contains(got[0].err.Error(), "r49 noaudit") {
		t.Fatalf("钩子应收到底层错误: %v", got[0].err)
	}

	// 同一根因的连续失败不再重复上报（防刷屏）
	e.Audit("admin", "system.round49.probe", "再来一条", "success")
	e.Audit("admin", "system.round49.probe", "第三条", "success")
	if len(got) != 1 {
		t.Fatalf("同一根因的连续审计失败不应刷屏，实际 %d 次", len(got))
	}

	// 审计恢复写成功 → 去重状态复位；再坏时重新上报（结果变化即重新可见）
	if _, err := store.db.Exec(`DROP TRIGGER r49_noaudit`); err != nil {
		t.Fatalf("拆触发器: %v", err)
	}
	e.Audit("admin", "system.round49.probe", "恢复后正常写入", "success")
	injectAuditInsertFailure(t, store)
	e.Audit("admin", "system.round49.probe", "再次失败", "success")
	if len(got) != 2 {
		t.Fatalf("恢复后再次失败应重新上报，实际 %d 次", len(got))
	}
}

func TestAuditWriteFailureNilHookDoesNotPanic(t *testing.T) {
	store := openTestStore(t)
	clock := newFakeClock()
	if _, err := store.AppendRevision(mustJSON(baseCommitted()), clock.Now(), "初始基线", "admin"); err != nil {
		t.Fatalf("预置基线: %v", err)
	}
	e, err := NewEngine(store, &mockApplier{}, Options{Now: clock.Now}) // 未接线 OnAuditError
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	injectAuditInsertFailure(t, store)
	e.Audit("admin", "system.round49.probe", "无钩子时的审计失败", "success") // 不得 panic
}

// ---------- ③ Edit 清理失败不得静默 ----------

func TestEditCleanupFailureIsAudited(t *testing.T) {
	store := openTestStore(t)
	clock := newFakeClock()
	if _, err := store.AppendRevision(mustJSON(baseCommitted()), clock.Now(), "初始基线", "admin"); err != nil {
		t.Fatalf("预置基线: %v", err)
	}
	e, err := NewEngine(store, &mockApplier{}, Options{Now: clock.Now})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	// 让「取锁成功后读 committed」失败：最新修订放一段非法 JSON
	if _, err := store.AppendRevision([]byte(`{oops`), clock.Now(), "损坏的修订", "admin"); err != nil {
		t.Fatalf("写入损坏修订: %v", err)
	}
	injectLockDeleteFailure(t, store)

	if err := e.Edit(Session{User: "admin", Source: "ssh"}); err == nil {
		t.Fatalf("committed 解析失败时 Edit 必须报错")
	}
	// 清理也失败（触发器挡着）⇒ 锁行残留，但**必须留痕**
	if li, _ := store.GetLock(); li == nil {
		t.Fatalf("触发器挡着删除时锁行应仍在（本用例前提）")
	}
	audit, err := store.ListAudit(20, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	found := false
	for _, a := range audit {
		if a.Result == "failure" && strings.Contains(a.Detail, "r49 nodel") && strings.Contains(a.Detail, "锁") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Edit 清理锁失败应留一条 failure 审计（点名锁清理失败与后果）: %+v", audit)
	}
	// 内存从未认为本会话持锁 ⇒ 不得留下编辑态（内存与库的错配只是「库里有把无主的锁」）
	if _, _, err := e.Candidate(); !errors.Is(err, ErrNotEditing) {
		t.Fatalf("Edit 失败后不应留下编辑态: %v", err)
	}
}
