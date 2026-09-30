package config

// 决策 #318：干净锁不排他 + 路径级释放（修 R98-1，#317 引入的回归）。
//
// 由来（R98-1，round98 真机）：#317 把配置会话按 token 拆开后，前一次 CLI 调用留下的
// **干净锁**（dirty=false）不再被下一次调用接管（新 token = 新会话键），而释放路径也没在这些
// 调用路径上生效 —— 真机 fulltest 从 198/0/11 退化为 195/3/11，失败签名统一为
// 「candidate 会话锁被占用: 由 admin@ssh 持有」。
//
// 本文件覆盖：① 干净锁被同一用户的新会话接管（含 fulltest 的连续多次 configure/commit 现场）；
// ② 脏锁仍严格排他（R79-1 不回归）；③ 跨用户不可接管；④ 接管后原会话得到明确错误；
// ⑤ 会话级释放不区分接入源（登出清得掉 CLI 留下的锁）；⑥ 干净锁更快回收。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestCleanLockTakeoverAcrossInvocations 连续多次「configure → 写 → commit」的**新会话**
// 不得因上一次留下的干净锁而失败（真机 fulltest 的三条失败现场）。
//
// 每一步都是新会话（新 token），且**不**显式释放（模拟 commit/一次调用结束后锁仍在）。
func TestCleanLockTakeoverAcrossInvocations(t *testing.T) {
	k := newEngineKit(t)
	for i, id := range []string{"tok-1", "tok-2", "tok-3"} {
		sess := Session{User: "admin", Source: "ssh", ID: id}
		if err := k.engine.Edit(sess); err != nil {
			t.Fatalf("第 %d 次 configure（会话 %s）应能取锁（上一把是干净锁），实得 %v", i+1, id, err)
		}
		cfg := baseCommitted()
		cfg.System.Hostname = "round-" + id
		if err := k.engine.UpdateCandidate(sess, cfg); err != nil {
			t.Fatalf("第 %d 次写候选: %v", i+1, err)
		}
		res, err := k.engine.Commit(context.Background(), sess, CommitOpts{})
		if err != nil {
			t.Fatalf("第 %d 次 commit: %v", i+1, err)
		}
		if res.Revision == 0 {
			t.Fatalf("第 %d 次 commit 应产生修订", i+1)
		}
		// commit **不释放**锁：下一次 configure 面对的是一把干净锁（这正是 R98-1 的现场）。
		if li, _ := k.store.GetLock(); li == nil || li.SessionID != id {
			t.Fatalf("第 %d 次 commit 后锁应仍由 %s 持有（干净），实得 %+v", i+1, id, li)
		}
	}
	// 收尾：最后一把锁被接管/释放后应无持锁会话。
	last := Session{User: "admin", Source: "ssh", ID: "tok-final"}
	if err := k.engine.Edit(last); err != nil {
		t.Fatalf("末次接管: %v", err)
	}
	if err := k.engine.Discard(last); err != nil {
		t.Fatalf("末次丢弃: %v", err)
	}
	if views, _ := k.engine.Sessions(); len(views) != 0 {
		t.Fatalf("全部结束后应无持锁会话: %+v", views)
	}
}

// TestDirtyLockStaysExclusiveForSameUser 脏候选严格排他：同一用户的新会话**不得**接管，
// 且原会话的候选原封不动（R79-1 的保护语义）。
func TestDirtyLockStaysExclusiveForSameUser(t *testing.T) {
	k := newEngineKit(t)
	a := Session{User: "admin", Source: "ssh", ID: "tok-a"}
	b := Session{User: "admin", Source: "ssh", ID: "tok-b"}

	if err := k.engine.Edit(a); err != nil {
		t.Fatalf("A 取锁: %v", err)
	}
	cfg := baseCommitted()
	cfg.System.Hostname = "dirty-by-a"
	if err := k.engine.UpdateCandidate(a, cfg); err != nil {
		t.Fatalf("A 写候选: %v", err)
	}
	if err := k.engine.Edit(b); !errors.Is(err, ErrLocked) {
		t.Fatalf("A 持脏候选时 B configure 应被拒（ErrLocked），实得 %v", err)
	}
	got, dirty, err := k.engine.Candidate()
	if err != nil || !dirty || hostnameOf(t, got) != "dirty-by-a" {
		t.Fatalf("A 的脏候选被动了: hostname=%q dirty=%v err=%v", hostnameOf(t, got), dirty, err)
	}
	// 同一用户但**另一**会话也不得冒认为持有者。
	if err := k.engine.UpdateCandidate(b, baseCommitted()); !errors.Is(err, ErrLockLost) {
		t.Fatalf("B 未持锁编辑应 ErrLockLost，实得 %v", err)
	}
}

// TestCrossUserCleanLockStillExclusive 干净锁也**只**限同一用户接管；跨用户仍被拒。
func TestCrossUserCleanLockStillExclusive(t *testing.T) {
	k := newEngineKit(t)
	if err := k.engine.Edit(Session{User: "admin", Source: "ssh", ID: "tok-a"}); err != nil {
		t.Fatalf("admin 取锁: %v", err)
	}
	err := k.engine.Edit(Session{User: "netop", Source: "ssh", ID: "tok-b"})
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("跨用户不得接管干净锁，应 ErrLocked，实得 %v", err)
	}
	if li, _ := k.store.GetLock(); li == nil || userOfHolder(li.Holder) != "admin" {
		t.Fatalf("跨用户被拒后锁应仍是 admin 的，实得 %+v", li)
	}
}

// TestTakeoverGivesOriginalSessionExplicitError 接管后原会话再操作得到**明确**错误
// （ErrLockLost），而不是静默变成别人。
func TestTakeoverGivesOriginalSessionExplicitError(t *testing.T) {
	k := newEngineKit(t)
	a := Session{User: "admin", Source: "ssh", ID: "tok-a"}
	b := Session{User: "admin", Source: "ssh", ID: "tok-b"}

	if err := k.engine.Edit(a); err != nil {
		t.Fatalf("A 取锁: %v", err)
	}
	if err := k.engine.Edit(b); err != nil {
		t.Fatalf("B 应能接管干净锁: %v", err)
	}
	// 会话表如实反映新的持锁会话（可见性不因接管而改变，只是换了个会话）。
	views, err := k.engine.Sessions()
	if err != nil || len(views) != 1 {
		t.Fatalf("Sessions: %+v err=%v", views, err)
	}
	if views[0].SessionID != "tok-b" || views[0].Holder != "admin@ssh" || views[0].Dirty {
		t.Fatalf("接管后会话表应显示 B、干净: %+v", views[0])
	}
	// 原会话 A 重新 configure（写语句内部走 Edit）/ 写候选 / 释放 / 提交都必须得到
	// ErrLockLost（明确「你已不是持有者」），不得静默把现场又接管回自己。
	cfg := baseCommitted()
	cfg.System.Hostname = "a-after-takeover"
	if err := k.engine.Edit(a); !errors.Is(err, ErrLockLost) {
		t.Fatalf("被接管后 A 重新 configure 应 ErrLockLost，实得 %v", err)
	}
	if err := k.engine.UpdateCandidate(a, cfg); !errors.Is(err, ErrLockLost) {
		t.Fatalf("被接管后 A 写候选应 ErrLockLost，实得 %v", err)
	}
	if err := k.engine.Release(a); !errors.Is(err, ErrLockLost) {
		t.Fatalf("被接管后 A 释放应 ErrLockLost，实得 %v", err)
	}
	if _, err := k.engine.Commit(context.Background(), a, CommitOpts{}); !errors.Is(err, ErrLockLost) {
		t.Fatalf("被接管后 A 提交应 ErrLockLost，实得 %v", err)
	}
	// B 的候选未被 A 的失败调用影响；B 可正常提交。
	if err := k.engine.Release(b); err != nil {
		t.Fatalf("接管者 B 释放: %v", err)
	}
	// 接管者释放后，原会话 A 又可重新进入（「被接管」记录随即失效）。
	if err := k.engine.Edit(a); err != nil {
		t.Fatalf("接管者释放后 A 应能重新进入: %v", err)
	}
	if err := k.engine.Release(a); err != nil {
		t.Fatalf("A 释放: %v", err)
	}
}

// TestTakeoverRecordedInAudit 接管留痕（不报错，但审计里看得出「谁从谁手里接管了干净锁」）。
func TestTakeoverRecordedInAudit(t *testing.T) {
	k := newEngineKit(t)
	if err := k.engine.Edit(Session{User: "admin", Source: "ssh", ID: "tok-a"}); err != nil {
		t.Fatalf("A 取锁: %v", err)
	}
	if err := k.engine.Edit(Session{User: "admin", Source: "ssh", ID: "tok-b"}); err != nil {
		t.Fatalf("B 接管: %v", err)
	}
	audit, err := k.store.ListAudit(10, 0)
	if err != nil || len(audit) == 0 {
		t.Fatalf("应有一条接管审计: %+v err=%v", audit, err)
	}
	if a := audit[0]; a.Action != "config.lock-takeover" || a.Result != "success" ||
		!strings.Contains(a.Detail, "admin@ssh") || !strings.Contains(a.Detail, "接管") {
		t.Fatalf("接管审计不符: %+v", a)
	}
}

// TestDiscardSessionReleasesAcrossSource 会话级释放**不区分接入源**：CLI（ssh/console）取得的
// 锁，登出请求（REST 身份）按 token 稳定 ID 也清得掉 —— 这是 R98-1 的释放缺口。
func TestDiscardSessionReleasesAcrossSource(t *testing.T) {
	k := newEngineKit(t)
	cli := Session{User: "admin", Source: "ssh", ID: "tok-cli"}
	cfg := baseCommitted()
	cfg.System.Hostname = "cli-dirty"

	if err := k.engine.Edit(cli); err != nil {
		t.Fatalf("CLI 取锁: %v", err)
	}
	if err := k.engine.UpdateCandidate(cli, cfg); err != nil {
		t.Fatalf("CLI 写候选: %v", err)
	}
	// 另一个 token 结束会话：不动本会话的锁。
	if err := k.engine.DiscardSession("admin", "tok-other"); !errors.Is(err, ErrNotEditing) {
		t.Fatalf("别的 token 结束会话不得动本会话锁，实得 %v", err)
	}
	// 别的用户结束同名 token：不动（用户也要匹配）。
	if err := k.engine.DiscardSession("netop", "tok-cli"); !errors.Is(err, ErrNotEditing) {
		t.Fatalf("跨用户 token 匹配不得释放，实得 %v", err)
	}
	if li, _ := k.store.GetLock(); li == nil {
		t.Fatalf("上述两次都不该释放锁")
	}
	// 本会话（token 一致，**接入源不同**）：释放并清候选。
	if err := k.engine.DiscardSession("admin", "tok-cli"); err != nil {
		t.Fatalf("本会话结束应清掉 CLI 留下的锁，实得 %v", err)
	}
	if li, _ := k.store.GetLock(); li != nil {
		t.Fatalf("本会话结束后不该还有锁: %+v", li)
	}
	if _, _, err := k.engine.Candidate(); !errors.Is(err, ErrNotEditing) {
		t.Fatalf("本会话结束后候选应清空，实得 %v", err)
	}
	// 空 ID（旧式调用）不当「wildcard」：不能借它释放别人的锁。
	if err := k.engine.Edit(Session{User: "admin", Source: "api", ID: "tok-api"}); err != nil {
		t.Fatalf("重新取锁: %v", err)
	}
	if err := k.engine.DiscardSession("admin", ""); !errors.Is(err, ErrNotEditing) {
		t.Fatalf("空会话标识不得释放任何锁，实得 %v", err)
	}
	if li, _ := k.store.GetLock(); li == nil || li.SessionID != "tok-api" {
		t.Fatalf("空标识调用后锁应仍在: %+v", li)
	}
}

// TestCleanLockRecycledFasterThanDirty 干净锁按更短阈值回收（复用既有巡检），
// 脏锁仍走 10 分钟的常规阈值（绝不替操作者丢掉未提交改动）。
func TestCleanLockRecycledFasterThanDirty(t *testing.T) {
	k := newEngineKit(t)

	// 干净锁：超过 CleanLockIdleTTL 即回收。
	clean := Session{User: "admin", Source: "ssh", ID: "tok-clean"}
	if err := k.engine.Edit(clean); err != nil {
		t.Fatalf("干净会话取锁: %v", err)
	}
	k.clock.Advance(DefaultCleanLockIdleTTL + time.Second)
	if views, _ := k.engine.Sessions(); len(views) != 0 {
		t.Fatalf("干净锁应更快回收，实得 %+v", views)
	}

	// 脏锁：超过 CleanLockIdleTTL 仍在；超过常规阈值才回收。
	dirty := Session{User: "admin", Source: "ssh", ID: "tok-dirty"}
	if err := k.engine.Edit(dirty); err != nil {
		t.Fatalf("脏会话取锁: %v", err)
	}
	cfg := baseCommitted()
	cfg.System.Hostname = "still-dirty"
	if err := k.engine.UpdateCandidate(dirty, cfg); err != nil {
		t.Fatalf("脏会话写候选: %v", err)
	}
	k.clock.Advance(DefaultCleanLockIdleTTL + time.Second)
	if views, _ := k.engine.Sessions(); len(views) != 1 || !views[0].Dirty {
		t.Fatalf("脏锁不应被干净阈值回收，实得 %+v", views)
	}
	k.clock.Advance(DefaultLockIdleTTL)
	if views, _ := k.engine.Sessions(); len(views) != 0 {
		t.Fatalf("脏锁超过常规阈值应回收，实得 %+v", views)
	}
}

// TestTakeoverDoesNotBreakOneShotHandback 接管与决策 #151 的一次性事务交还锁并存：
// 接管后新会话仍按 endOneShot 的判据（干净即释放）交还锁，别的会话立刻能用。
func TestTakeoverDoesNotBreakOneShotHandback(t *testing.T) {
	k := newEngineKit(t)
	if err := k.engine.Edit(Session{User: "admin", Source: "ssh", ID: "tok-old"}); err != nil {
		t.Fatalf("旧会话取锁: %v", err)
	}
	oneShot := Session{User: "admin", Source: "ssh", ID: "tok-new"}
	if err := k.engine.Edit(oneShot); err != nil {
		t.Fatalf("新会话接管: %v", err)
	}
	if _, dirty, err := k.engine.Candidate(); err != nil || dirty {
		t.Fatalf("接管后候选应干净: dirty=%v err=%v", dirty, err)
	}
	if err := k.engine.Release(oneShot); err != nil {
		t.Fatalf("一次性事务交还: %v", err)
	}
	if li, _ := k.store.GetLock(); li != nil {
		t.Fatalf("交还后应无锁: %+v", li)
	}
}
