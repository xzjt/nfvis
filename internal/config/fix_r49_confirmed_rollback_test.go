package config

// round49 缺陷 #1（高危）：commit confirmed 超时回滚**只回滚 SQLite 与宿主侧，不反向下发数据面**。
//
// 现场：`set system firewall …` 一类变更以 `commit confirmed 2` 提交 → 忘确认 → 定时器到点走
// `doConfirmedRollback` —— 它只 `LoadRevision` + `AppendRevision` + `ClearConfirmed` + `onCommit`，
// **从不调用 applier.Apply**；而恢复收敛（L2/L3）明确「只补齐不摘除」。结果是
// committed 回到旧版、数据面（VPP/nft/tc）仍是新拓扑：配置与事实不一致，且**没有任何告警**。
//
// 修复口径：超时回滚与提交一样「反向下发」——在锁内取当前 committed 与基线快照，调
// `applier.Apply(ctx, current, base)`（applier 语义是 Apply(old, new)）；**成功才落库**
// （回滚修订 + 清 confirmed 同事务），失败则数据库**不动**（保持 committed 与数据面一致）、
// 清 confirmed（避免定时器/巡检无限重试）并如实地报一条失败。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
)

// recApplier 记录每次 Apply 的入参与**收到的上下文是否已取消**，并支持在「下发进行中」
// 这一刻就地观测存储（用于断言「先下发、后落库」的顺序）。
type recApplier struct {
	mu    sync.Mutex
	calls []applierCall
	fail  error
	// observe 每次 Apply 开始时的观测钩子。
	observe func()
	// onCall 每次 Apply 开始时的钩子（就地取消调用方上下文等场景用）。
	onCall func()
}

type applierCall struct {
	old    model.Config
	new    model.Config
	ctxErr error
}

func (a *recApplier) Apply(ctx context.Context, old, new model.Config) error {
	a.mu.Lock()
	fail, observe, onCall := a.fail, a.observe, a.onCall
	a.mu.Unlock()
	if onCall != nil {
		onCall()
	}
	if observe != nil {
		observe()
	}
	// 上下文状态在回调**之后**读取：模拟「下发进行中调用方断开」时要断言的是
	// 引擎交给 applier 的上下文在整段下发期间都没有失效（客户端断开不得中断服务端处置）。
	a.mu.Lock()
	a.calls = append(a.calls, applierCall{old: old, new: new, ctxErr: ctx.Err()})
	a.mu.Unlock()
	return fail
}

func (a *recApplier) snapshot() []applierCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]applierCall, len(a.calls))
	copy(out, a.calls)
	return out
}

func (a *recApplier) setFail(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fail = err
}

// rollbackKit 一台已进入 confirmed 保护期的引擎：基线 rev1（hostname nfvis-node1）→
// confirmed 提交 rev2（hostname risky，10 分钟窗口）。
type rollbackKit struct {
	engine  *Engine
	store   *Store
	clock   *fakeClock
	timers  *timerSink
	applier *recApplier
	events  *[]Event
	commits *[]string // "rev:user"
}

func newRollbackKit(t *testing.T, ap *recApplier) *rollbackKit {
	t.Helper()
	store := openTestStore(t)
	clock := newFakeClock()
	timers := newTimerSink()
	if _, err := store.AppendRevision(mustJSON(baseCommitted()), clock.Now(), "初始基线", "admin"); err != nil {
		t.Fatalf("预置基线: %v", err)
	}
	var events []Event
	var commits []string
	e, err := NewEngine(store, ap, Options{
		Now:       clock.Now,
		AfterFunc: timers.after,
		OnEvent:   func(ev Event) { events = append(events, ev) },
		OnCommitted: func(rev int, user string) {
			commits = append(commits, fmt.Sprintf("%d:%s", rev, user))
		},
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	k := &rollbackKit{engine: e, store: store, clock: clock, timers: timers, applier: ap, events: &events, commits: &commits}

	sess := Session{User: "admin", Source: "ssh"}
	if err := e.Edit(sess); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	cfg := baseCommitted()
	cfg.System.Hostname = "risky"
	if err := e.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	if _, err := e.Commit(context.Background(), sess, CommitOpts{ConfirmedMinutes: 10}); err != nil {
		t.Fatalf("Commit confirmed: %v", err)
	}
	if rev, _, _ := store.LatestRevision(); rev != 2 {
		t.Fatalf("confirmed 提交应落 rev 2，实际 %d", rev)
	}
	return k
}

// fireTimeout 推到 confirmed 截止并触发定时器（与生产同一条路径）。
func (k *rollbackKit) fireTimeout() {
	k.clock.Advance(10 * time.Minute)
	k.timers.FireAll()
}

// TestConfirmedTimeoutRollbackReversesDataPlane：超时回滚必须像提交一样反向下发数据面，
// 且顺序是**先下发、后落库**（下发失败就不能落库）。
func TestConfirmedTimeoutRollbackReversesDataPlane(t *testing.T) {
	ap := &recApplier{}
	k := newRollbackKit(t, ap)

	// 在**回滚下发那一刻**观测存储：数据库必须还停在「risky」那一版、confirmed 仍在途
	// ——即「先下发、后落库」的顺序（下发失败就不许落库）。
	var duringApplyRev = -1
	var duringApplyConfirmed bool
	ap.observe = func() {
		rev, _, err := k.store.LatestRevision()
		if err != nil {
			t.Errorf("观测 LatestRevision: %v", err)
		}
		duringApplyRev = rev
		cf, err := k.store.GetConfirmed()
		if err != nil {
			t.Errorf("观测 GetConfirmed: %v", err)
		}
		duringApplyConfirmed = cf != nil
	}

	k.fireTimeout()

	calls := ap.snapshot()
	if len(calls) != 2 {
		t.Fatalf("应发生两次下发（提交 1 次 + 超时回滚 1 次），实际 %d 次", len(calls))
	}
	rb := calls[1]
	if hostnameOf(t, rb.old) != "risky" {
		t.Fatalf("回滚下发应把**当前生效**配置作为 old 传下去，实际 %q", hostnameOf(t, rb.old))
	}
	if hostnameOf(t, rb.new) != "nfvis-node1" {
		t.Fatalf("回滚下发应把**基线**配置作为 new 传下去，实际 %q", hostnameOf(t, rb.new))
	}
	if rb.ctxErr != nil {
		t.Fatalf("回滚下发的上下文不应已取消: %v", rb.ctxErr)
	}
	// 顺序：下发时数据库尚未回滚（committed 仍是 risky、confirmed 仍在途）
	if duringApplyRev != 2 {
		t.Fatalf("下发时必须尚未落库（rev 应仍为 2），实际 rev=%d", duringApplyRev)
	}
	if !duringApplyConfirmed {
		t.Fatalf("下发时 confirmed 应仍在途（落库清除必须在成功之后）")
	}

	// 成功之后：落库、清 confirmed、同步 candidate、通知宿主侧
	if got, _ := k.engine.Committed(); hostnameOf(t, got) != "nfvis-node1" {
		t.Fatalf("回滚后 committed 应为基线，实际 %s", hostnameOf(t, got))
	}
	if rev, _, _ := k.store.LatestRevision(); rev != 3 {
		t.Fatalf("回滚应以新修订落库（rev 3），实际 %d", rev)
	}
	if cf, _ := k.store.GetConfirmed(); cf != nil {
		t.Fatalf("回滚后 confirmed 应已清除: %+v", cf)
	}
	if cand, _, err := k.engine.Candidate(); err != nil || hostnameOf(t, cand) != "nfvis-node1" {
		t.Fatalf("candidate 应同步回滚配置: %v %s", err, hostnameOf(t, cand))
	}
	cs := *k.commits
	if len(cs) != 2 || cs[len(cs)-1] != "3:system" {
		t.Fatalf("回滚必须以 system 身份通知宿主侧重收敛，实际 %v", cs)
	}
	audit, _ := k.store.ListAudit(10, 0)
	if len(audit) == 0 || audit[0].Action != "config.rollback-auto" || audit[0].Result != "success" {
		t.Fatalf("回滚成功应入一条 config.rollback-auto/success 审计: %+v", audit)
	}
	if len(*k.events) == 0 || (*k.events)[len(*k.events)-1].Type != EventConfirmedTimeout {
		t.Fatalf("应产生 confirmed 超时事件: %+v", *k.events)
	}
}

// TestConfirmedTimeoutRollbackFailureKeepsCommitted：反向下发失败时**不回滚数据库**
// （保持 committed 与数据面一致），但仍要清 confirmed（避免无限重试）并如实报失败。
func TestConfirmedTimeoutRollbackFailureKeepsCommitted(t *testing.T) {
	ap := &recApplier{}
	k := newRollbackKit(t, ap)
	ap.setFail(errors.New("模拟数据面反向下发失败"))

	k.fireTimeout()

	if len(ap.snapshot()) != 2 {
		t.Fatalf("应仍然尝试了一次反向下发，实际 %d 次", len(ap.snapshot()))
	}
	got, err := k.engine.Committed()
	if err != nil {
		t.Fatal(err)
	}
	if hostnameOf(t, got) != "risky" {
		t.Fatalf("下发失败时数据库不得回滚（保持与数据面一致），实际 %s", hostnameOf(t, got))
	}
	if rev, _, _ := k.store.LatestRevision(); rev != 2 {
		t.Fatalf("下发失败时不应追加修订，实际 rev=%d", rev)
	}
	if cf, _ := k.store.GetConfirmed(); cf != nil {
		t.Fatalf("下发失败也必须清 confirmed（否则每个巡检周期都会重试）: %+v", cf)
	}
	// 不得谎报已回滚：宿主侧通知只能有一次（提交那次的），事件必须是失败事件
	if cs := *k.commits; len(cs) != 1 {
		t.Fatalf("回滚未生效时不得通知宿主侧「已提交变更」: %v", cs)
	}
	last := (*k.events)[len(*k.events)-1]
	if last.Type != EventConfirmedRollbackFailed {
		t.Fatalf("应上报回滚失败事件 %s，实际 %+v", EventConfirmedRollbackFailed, *k.events)
	}
	if !strings.Contains(last.Message, "失败") {
		t.Fatalf("失败事件文案要说清结果: %q", last.Message)
	}
	audit, _ := k.store.ListAudit(10, 0)
	if len(audit) == 0 || audit[0].Action != "config.rollback-auto" || audit[0].Result != "failure" {
		t.Fatalf("回滚失败应入一条 config.rollback-auto/failure 审计: %+v", audit)
	}
	if !strings.Contains(audit[0].Detail, "未回滚") {
		t.Fatalf("失败审计要说清「配置未回滚」: %q", audit[0].Detail)
	}
	// 再次巡检不得重试（confirmed 已清）
	k.timers.FireAll()
	if len(ap.snapshot()) != 2 {
		t.Fatalf("confirmed 已清后不得重试下发，实际 %d 次", len(ap.snapshot()))
	}
}

// TestConfirmedTimeoutRollbackMissingBaselineReports：基线快照缺失时不得静默——
// 清 confirmed、报事件，且**不下发**（没有基线可下发）。
func TestConfirmedTimeoutRollbackMissingBaselineReports(t *testing.T) {
	ap := &recApplier{}
	k := newRollbackKit(t, ap)
	// 把在途 confirmed 的基线指向一个不存在的修订（模拟快照被清理/损坏）。
	cf, err := k.store.GetConfirmed()
	if err != nil || cf == nil {
		t.Fatalf("在途 confirmed 应在场: %+v %v", cf, err)
	}
	if err := k.store.SetConfirmed(999, cf.Deadline, cf.Holder); err != nil {
		t.Fatalf("SetConfirmed: %v", err)
	}

	k.fireTimeout()

	if len(ap.snapshot()) != 1 {
		t.Fatalf("无基线可下发时不得调 Apply，实际 %d 次", len(ap.snapshot()))
	}
	if cf, _ := k.store.GetConfirmed(); cf != nil {
		t.Fatalf("基线缺失时也应清 confirmed: %+v", cf)
	}
	if rev, _, _ := k.store.LatestRevision(); rev != 2 {
		t.Fatalf("基线缺失时不得追加修订，实际 rev=%d", rev)
	}
	last := (*k.events)[len(*k.events)-1]
	if last.Type != EventConfirmedRollbackFailed {
		t.Fatalf("回滚未能执行应上报失败事件 %s，实际 %+v", EventConfirmedRollbackFailed, *k.events)
	}
	if !strings.Contains(last.Message, "缺失") {
		t.Fatalf("基线缺失要如实报出: %q", last.Message)
	}
	audit, _ := k.store.ListAudit(10, 0)
	if len(audit) == 0 || audit[0].Action != "config.rollback-auto" || audit[0].Result != "failure" {
		t.Fatalf("回滚未能执行应入一条 config.rollback-auto/failure 审计: %+v", audit)
	}
}
