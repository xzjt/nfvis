package config

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
)

// ---------- 测试基础设施 ----------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// timerSink 手动触发 AfterFunc（模拟 confirmed 定时器）。
type timerSink struct {
	mu      sync.Mutex
	pending []*pendingTimer
}

type pendingTimer struct {
	fn      func()
	stopped bool
}

func newTimerSink() *timerSink { return &timerSink{} }

func (ts *timerSink) after(_ time.Duration, fn func()) (stop func()) {
	pt := &pendingTimer{fn: fn}
	ts.mu.Lock()
	ts.pending = append(ts.pending, pt)
	ts.mu.Unlock()
	return func() {
		ts.mu.Lock()
		pt.stopped = true
		ts.mu.Unlock()
	}
}

// FireAll 触发所有未 stop 的挂起定时器。
func (ts *timerSink) FireAll() {
	ts.mu.Lock()
	fns := make([]*pendingTimer, len(ts.pending))
	copy(fns, ts.pending)
	ts.pending = nil
	ts.mu.Unlock()
	for _, pt := range fns {
		if !pt.stopped {
			pt.fn()
		}
	}
}

type mockApplier struct {
	mu    sync.Mutex
	fail  bool
	calls int
}

func (m *mockApplier) Apply(ctx context.Context, old, new model.Config) error {
	m.mu.Lock()
	m.calls++
	fail := m.fail
	m.mu.Unlock()
	if fail {
		return fmt.Errorf("模拟底座下发失败")
	}
	return nil
}

func baseCommitted() model.Config {
	t := true
	return model.Config{
		System: &model.SystemConfig{
			Hostname:   "nfvis-node1",
			Management: &model.MgmtConfig{Address: "192.168.1.10/24", Gateway: "192.168.1.1"},
		},
		Interfaces: []model.InterfaceConfig{{Name: "ens2f0", Enabled: &t}},
		VirtualSwitches: []model.VirtualSwitch{{
			Name: "vs-app", Type: "l2", VlanAccess: 100,
			Ports: []model.VSwitchPort{{Seq: 1, Interface: "ens2f0"}},
		}},
	}
}

type engineKit struct {
	engine  *Engine
	store   *Store
	clock   *fakeClock
	timers  *timerSink
	applier *mockApplier
	events  *[]Event
}

func newEngineKit(t *testing.T) *engineKit {
	t.Helper()
	store := openTestStore(t)
	clock := newFakeClock()
	timers := newTimerSink()
	applier := &mockApplier{}
	// 预置基线 committed 配置为 rev 1
	if _, err := store.AppendRevision(mustJSON(baseCommitted()), clock.Now(), "初始基线"); err != nil {
		t.Fatalf("预置基线: %v", err)
	}
	var events []Event
	e, err := NewEngine(store, applier, Options{
		Now:       clock.Now,
		AfterFunc: timers.after,
		OnEvent:   func(ev Event) { events = append(events, ev) },
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return &engineKit{engine: e, store: store, clock: clock, timers: timers, applier: applier, events: &events}
}

func (k *engineKit) edit(t *testing.T, user, source string) {
	t.Helper()
	if err := k.engine.Edit(Session{User: user, Source: source}); err != nil {
		t.Fatalf("Edit(%s@%s): %v", user, source, err)
	}
}

func hostnameOf(t *testing.T, cfg model.Config) string {
	t.Helper()
	if cfg.System == nil {
		return ""
	}
	return cfg.System.Hostname
}

// ---------- 引擎测试 ----------

func TestEngineInitEmptyConfig(t *testing.T) {
	store := openTestStore(t)
	e, err := NewEngine(store, &mockApplier{}, Options{Now: newFakeClock().Now})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	committed, err := e.Committed()
	if err != nil {
		t.Fatalf("Committed: %v", err)
	}
	if committed.System != nil {
		t.Fatalf("空库初始化应为空配置")
	}
	rev, _, _ := store.LatestRevision()
	if rev != 1 {
		t.Fatalf("初始化应写入 rev 1，实际 %d", rev)
	}
}

func TestEngineLifecycle(t *testing.T) {
	k := newEngineKit(t)
	k.edit(t, "admin", "ssh")

	cfg := baseCommitted()
	cfg.System.Hostname = "changed"
	if err := k.engine.UpdateCandidate(Session{User: "admin", Source: "ssh"}, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}

	cand, dirty, err := k.engine.Candidate()
	if err != nil || !dirty || hostnameOf(t, cand) != "changed" {
		t.Fatalf("Candidate: %v dirty=%v host=%s", err, dirty, hostnameOf(t, cand))
	}

	res, err := k.engine.Commit(context.Background(), Session{User: "admin", Source: "ssh"}, CommitOpts{Message: "改主机名"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if res.Revision != 2 {
		t.Fatalf("commit 后 revision 应为 2，实际 %d", res.Revision)
	}
	if res.ConfirmedUntil != nil || len(res.Warnings) != 0 {
		t.Fatalf("普通 commit 不应有 confirmed/warnings: %+v", res)
	}
	if got, _ := k.engine.Committed(); hostnameOf(t, got) != "changed" {
		t.Fatalf("committed 未更新: %s", hostnameOf(t, got))
	}
	if _, dirty, _ := k.engine.Candidate(); dirty {
		t.Fatalf("commit 后 dirty 应为 false")
	}

	// 无变更的 commit 不产生新修订
	res2, err := k.engine.Commit(context.Background(), Session{User: "admin", Source: "ssh"}, CommitOpts{})
	if err != nil || res2.Revision != 2 {
		t.Fatalf("无变更 commit: rev=%d err=%v", res2.Revision, err)
	}
	rev, _, _ := k.store.LatestRevision()
	if rev != 2 {
		t.Fatalf("无变更 commit 不应追加修订，实际 %d", rev)
	}

	// 审计（FR-CFG-010）
	audit, _ := k.store.ListAudit(10, 0)
	if len(audit) != 1 || audit[0].User != "admin" || audit[0].Action != "config.commit" ||
		audit[0].Result != "success" || !strings.Contains(audit[0].Detail, "hostname") {
		t.Fatalf("审计记录不符: %+v", audit)
	}
}

func TestEngineLockConflict(t *testing.T) {
	k := newEngineKit(t)
	k.edit(t, "admin", "ssh")

	err := k.engine.Edit(Session{User: "netop", Source: "ssh"})
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("第二个会话 configure 应返回 ErrLocked，实际 %v", err)
	}
	if err := k.engine.UpdateCandidate(Session{User: "netop", Source: "ssh"}, baseCommitted()); !errors.Is(err, ErrNotEditing) {
		t.Fatalf("未持锁会话编辑应返回 ErrNotEditing，实际 %v", err)
	}

	// 其他会话仍可只读 committed（FR-CFG-009）
	if _, err := k.engine.Committed(); err != nil {
		t.Fatalf("只读 committed 失败: %v", err)
	}

	// 持有者作出变更后，会话列表应展示 dirty 状态
	cfg := baseCommitted()
	cfg.System.Hostname = "in-edit"
	if err := k.engine.UpdateCandidate(Session{User: "admin", Source: "ssh"}, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}

	views, err := k.engine.Sessions()
	if err != nil || len(views) != 1 || views[0].Holder != "admin@ssh" || !views[0].Dirty {
		t.Fatalf("Sessions: %+v err=%v", views, err)
	}

	// 持有者重复 configure 幂等
	if err := k.engine.Edit(Session{User: "admin", Source: "ssh"}); err != nil {
		t.Fatalf("持有者重复 Edit: %v", err)
	}

	// discard 释放锁（骨架 §3.4：discard → 无锁）
	if err := k.engine.Discard(Session{User: "admin", Source: "ssh"}); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	views, _ = k.engine.Sessions()
	if len(views) != 0 {
		t.Fatalf("discard 后应无持锁会话: %+v", views)
	}
	if err := k.engine.UpdateCandidate(Session{User: "admin", Source: "ssh"}, baseCommitted()); !errors.Is(err, ErrNotEditing) {
		t.Fatalf("discard 后应需重新 Edit: %v", err)
	}
}

func TestEngineValidationAggregatesErrors(t *testing.T) {
	k := newEngineKit(t)
	k.edit(t, "admin", "ssh")

	cfg := baseCommitted()
	bad := false
	cfg.VirtualSwitches[0].VlanAccess = 4095 // vlan 越界
	cfg.VirtualMachineFunctions = []model.VMFunction{{
		Name: "fw-vm", Image: "", // 缺 image
		VCPU:       model.VMCpu{Count: 4},
		Memory:     model.VMMemory{SizeMB: 8192, Backing: "swap"}, // 枚举非法
		Interfaces: []model.VnfInterface{{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-app", MAC: "52:54:00:aa:00:01", Sriov: &model.SriovBind{}}},
	}}
	cfg.Interfaces[0].Enabled = &bad
	if err := k.engine.UpdateCandidate(Session{User: "admin", Source: "ssh"}, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}

	_, err := k.engine.Commit(context.Background(), Session{User: "admin", Source: "ssh"}, CommitOpts{})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("commit 应返回 ValidationError，实际 %v", err)
	}
	if len(ve.Errors) < 3 {
		t.Fatalf("FR-CFG-002 应逐条列出全部错误，实际 %d: %v", len(ve.Errors), ve.Errors)
	}
	// candidate 保留（FR-CFG-002）
	if _, dirty, _ := k.engine.Candidate(); !dirty {
		t.Fatalf("校验失败后 candidate 应保留")
	}
	// revision 不前进
	rev, _, _ := k.store.LatestRevision()
	if rev != 1 {
		t.Fatalf("校验失败不应追加修订，实际 %d", rev)
	}

	// commit check 复用同一校验路径
	errs, err := k.engine.CommitCheck(Session{User: "admin", Source: "ssh"})
	if err != nil || len(errs) < 3 {
		t.Fatalf("CommitCheck: %v %v", errs, err)
	}
}

func TestEngineApplyFailure(t *testing.T) {
	k := newEngineKit(t)
	k.applier.fail = true
	k.edit(t, "admin", "ssh")

	cfg := baseCommitted()
	cfg.System.Hostname = "changed"
	_ = k.engine.UpdateCandidate(Session{User: "admin", Source: "ssh"}, cfg)

	_, err := k.engine.Commit(context.Background(), Session{User: "admin", Source: "ssh"}, CommitOpts{})
	if err == nil {
		t.Fatalf("底座失败时 commit 应报错")
	}
	rev, _, _ := k.store.LatestRevision()
	if rev != 1 {
		t.Fatalf("底座失败不应落库，实际 rev=%d", rev)
	}
	audit, _ := k.store.ListAudit(10, 0)
	if len(audit) != 1 || audit[0].Result != "failure" {
		t.Fatalf("失败 commit 应入审计: %+v", audit)
	}
}

func TestEngineCommitConfirmedTimeout(t *testing.T) {
	k := newEngineKit(t)
	k.edit(t, "admin", "ssh")

	cfg := baseCommitted()
	cfg.System.Hostname = "risky"
	_ = k.engine.UpdateCandidate(Session{User: "admin", Source: "ssh"}, cfg)

	res, err := k.engine.Commit(context.Background(), Session{User: "admin", Source: "ssh"}, CommitOpts{ConfirmedMinutes: 10})
	if err != nil {
		t.Fatalf("Commit confirmed: %v", err)
	}
	if res.ConfirmedUntil == nil || !res.ConfirmedUntil.Equal(k.clock.Now().Add(10*time.Minute)) {
		t.Fatalf("ConfirmedUntil 不符: %+v", res.ConfirmedUntil)
	}
	if got, _ := k.engine.Committed(); hostnameOf(t, got) != "risky" {
		t.Fatalf("confirmed commit 立即生效")
	}

	// 超时（FR-CFG-003）：自动回滚到变更前配置并产生告警事件
	k.clock.Advance(10 * time.Minute)
	k.timers.FireAll()

	if got, _ := k.engine.Committed(); hostnameOf(t, got) != "nfvis-node1" {
		t.Fatalf("超时后应回滚到变更前配置，实际 %s", hostnameOf(t, got))
	}
	rev, data, _ := k.store.LatestRevision()
	if rev != 3 || strings.Contains(string(data), "risky") {
		t.Fatalf("自动回滚应以新修订落库: rev=%d data=%s", rev, data)
	}
	if len(*k.events) != 1 || (*k.events)[0].Type != EventConfirmedTimeout {
		t.Fatalf("应产生 confirmed 超时告警事件: %+v", *k.events)
	}
	audit, _ := k.store.ListAudit(10, 0)
	if audit[0].Action != "config.rollback-auto" {
		t.Fatalf("自动回滚应入审计: %+v", audit[0])
	}
	// candidate 会话若仍持有，重置为回滚后配置
	cand, _, err := k.engine.Candidate()
	if err != nil || hostnameOf(t, cand) != "nfvis-node1" {
		t.Fatalf("candidate 应同步回滚配置: %v %s", err, hostnameOf(t, cand))
	}
}

func TestEngineCommitConfirmedConfirm(t *testing.T) {
	k := newEngineKit(t)
	k.edit(t, "admin", "ssh")

	cfg := baseCommitted()
	cfg.System.Hostname = "risky"
	_ = k.engine.UpdateCandidate(Session{User: "admin", Source: "ssh"}, cfg)
	if _, err := k.engine.Commit(context.Background(), Session{User: "admin", Source: "ssh"}, CommitOpts{ConfirmedMinutes: 10}); err != nil {
		t.Fatalf("Commit confirmed: %v", err)
	}

	// FR-CFG-004：确认期内 commit（此处 ConfirmCommit）确认
	if err := k.engine.ConfirmCommit(Session{User: "admin", Source: "ssh"}); err != nil {
		t.Fatalf("ConfirmCommit: %v", err)
	}
	k.timers.FireAll()
	if got, _ := k.engine.Committed(); hostnameOf(t, got) != "risky" {
		t.Fatalf("确认后不应回滚")
	}
	views, _ := k.engine.Sessions()
	if views[0].ConfirmedUntil != nil {
		t.Fatalf("确认后 confirmed 状态应清除: %+v", views[0])
	}
}

func TestEngineNewCommitConfirmsPending(t *testing.T) {
	k := newEngineKit(t)
	k.edit(t, "admin", "ssh")
	cfg := baseCommitted()
	cfg.System.Hostname = "risky"
	_ = k.engine.UpdateCandidate(Session{User: "admin", Source: "ssh"}, cfg)
	if _, err := k.engine.Commit(context.Background(), Session{User: "admin", Source: "ssh"}, CommitOpts{ConfirmedMinutes: 10}); err != nil {
		t.Fatalf("Commit confirmed: %v", err)
	}

	cfg2 := cfg
	cfg2.System.Hostname = "stable"
	_ = k.engine.UpdateCandidate(Session{User: "admin", Source: "ssh"}, cfg2)
	if _, err := k.engine.Commit(context.Background(), Session{User: "admin", Source: "ssh"}, CommitOpts{}); err != nil {
		t.Fatalf("第二次 commit（隐式确认）: %v", err)
	}
	k.timers.FireAll()
	if got, _ := k.engine.Committed(); hostnameOf(t, got) != "stable" {
		t.Fatalf("新 commit 应隐式确认旧 pending: %s", hostnameOf(t, got))
	}
}

func TestEngineConfirmedPersistenceAcrossRestart(t *testing.T) {
	store := openTestStore(t)
	clock := newFakeClock()
	timers := newTimerSink()
	applier := &mockApplier{}
	if _, err := store.AppendRevision(mustJSON(baseCommitted()), clock.Now(), "初始基线"); err != nil {
		t.Fatalf("预置基线: %v", err)
	}

	e1, err := NewEngine(store, applier, Options{Now: clock.Now, AfterFunc: timers.after})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	sess := Session{User: "admin", Source: "ssh"}
	if err := e1.Edit(sess); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	cfg := baseCommitted()
	cfg.System.Hostname = "risky"
	_ = e1.UpdateCandidate(sess, cfg)
	if _, err := e1.Commit(context.Background(), sess, CommitOpts{ConfirmedMinutes: 10}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	e1.Close() // nfvisd 重启（计时器在守护进程侧，FR 骨架 §3.4）

	// 重启后 deadline 未到：重新武装定时器
	e2, err := NewEngine(store, applier, Options{Now: clock.Now, AfterFunc: timers.after})
	if err != nil {
		t.Fatalf("重启装配: %v", err)
	}
	timers.FireAll()
	if got, _ := e2.Committed(); hostnameOf(t, got) != "risky" {
		t.Fatalf("deadline 未到不应回滚")
	}

	// 重启后 deadline 已过：装配时立即自动回滚
	clock.Advance(11 * time.Minute)
	e3, err := NewEngine(store, applier, Options{Now: clock.Now, AfterFunc: timers.after})
	if err != nil {
		t.Fatalf("重启装配2: %v", err)
	}
	if got, _ := e3.Committed(); hostnameOf(t, got) != "nfvis-node1" {
		t.Fatalf("deadline 已过应装配时回滚，实际 %s", hostnameOf(t, got))
	}
}

func TestEngineRollbackAndCompare(t *testing.T) {
	k := newEngineKit(t)
	sess := Session{User: "admin", Source: "ssh"}
	k.edit(t, "admin", "ssh")

	cfg := baseCommitted()
	cfg.System.Hostname = "v2"
	_ = k.engine.UpdateCandidate(sess, cfg)
	if _, err := k.engine.Commit(context.Background(), sess, CommitOpts{}); err != nil {
		t.Fatalf("commit v2: %v", err)
	}
	cfg.System.Hostname = "v3"
	_ = k.engine.UpdateCandidate(sess, cfg)
	if _, err := k.engine.Commit(context.Background(), sess, CommitOpts{}); err != nil {
		t.Fatalf("commit v3: %v", err)
	}

	// FR-CFG-005：rollback n 取历史快照为 candidate，需再 commit 生效
	if err := k.engine.Rollback(sess, 1); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	cand, dirty, _ := k.engine.Candidate()
	if !dirty || hostnameOf(t, cand) != "v2" {
		t.Fatalf("rollback 1 应取 v2 为 candidate: %s dirty=%v", hostnameOf(t, cand), dirty)
	}
	if got, _ := k.engine.Committed(); hostnameOf(t, got) != "v3" {
		t.Fatalf("rollback 不应立即改变 committed")
	}

	diff, err := k.engine.CompareCandidate()
	if err != nil || !strings.Contains(diff, "v2") || !strings.Contains(diff, "v3") {
		t.Fatalf("CompareCandidate 应显示 candidate⇄committed 差异: %s err=%v", diff, err)
	}
	if _, err := k.engine.Commit(context.Background(), sess, CommitOpts{}); err != nil {
		t.Fatalf("rollback 后 commit: %v", err)
	}
	if got, _ := k.engine.Committed(); hostnameOf(t, got) != "v2" {
		t.Fatalf("rollback 后 commit 应生效")
	}

	// FR-CFG-006：show configuration | compare rollback n
	cfg.System.Hostname = "v4"
	_ = k.engine.UpdateCandidate(sess, cfg)
	_, _ = k.engine.Commit(context.Background(), sess, CommitOpts{})
	rdiff, err := k.engine.Compare(2)
	if err != nil || !strings.Contains(rdiff, "v3") || !strings.Contains(rdiff, "v4") {
		t.Fatalf("Compare(2) 应显示 committed(v4) ⇄ 第 2 个历史快照(v3): %s err=%v", rdiff, err)
	}
	if err := k.engine.Rollback(sess, 99); !errors.Is(err, ErrNoRevision) {
		t.Fatalf("rollback 越界应返回 ErrNoRevision: %v", err)
	}
}

func TestEngineMgmtPortSelfLock(t *testing.T) {
	k := newEngineKit(t)
	sess := Session{User: "admin", Source: "ssh"}
	k.edit(t, "admin", "ssh")

	cfg := baseCommitted()
	cfg.System.Management.Address = "192.168.2.10/24"
	_ = k.engine.UpdateCandidate(sess, cfg)

	// FR-CFG-012：SSH 会话变更管理口必须 commit confirmed
	_, err := k.engine.Commit(context.Background(), sess, CommitOpts{})
	if !errors.Is(err, ErrConfirmRequired) {
		t.Fatalf("SSH 会话管理口变更应强制 confirmed，实际 %v", err)
	}
	if _, err := k.engine.Commit(context.Background(), sess, CommitOpts{ConfirmedMinutes: 5}); err != nil {
		t.Fatalf("confirmed 方式应放行: %v", err)
	}
	if err := k.engine.Release(sess); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// 非 SSH 会话放行但给警告
	k.edit(t, "op", "console")
	cfg2 := baseCommitted()
	cfg2.System.Management.Address = "192.168.3.10/24"
	_ = k.engine.UpdateCandidate(Session{User: "op", Source: "console"}, cfg2)
	res, err := k.engine.Commit(context.Background(), Session{User: "op", Source: "console"}, CommitOpts{})
	if err != nil {
		t.Fatalf("console 会话应放行: %v", err)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "管理口") {
			found = true
		}
	}
	if !found {
		t.Fatalf("管理口变更应有警告: %+v", res.Warnings)
	}
}

func TestEngineCommitWarnings(t *testing.T) {
	k := newEngineKit(t)
	sess := Session{User: "admin", Source: "console"}
	k.edit(t, "admin", "console")

	cfg := baseCommitted()
	cfg.Vpp = &model.VppConfig{CPU: &model.VppCPU{MainCore: 0}}
	_ = k.engine.UpdateCandidate(sess, cfg)
	res, err := k.engine.Commit(context.Background(), sess, CommitOpts{})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !warningsContain(res.Warnings, "request vpp restart") {
		t.Fatalf("vpp 变更应有重启提示（FR-SYS-009）: %+v", res.Warnings)
	}

	k.edit(t, "admin", "console")
	cfg2 := baseCommitted()
	cfg2.ResourcePools = &model.ResourcePool{Hugepages: []model.HPool{{PageSize: "1G", Count: 64}}}
	_ = k.engine.UpdateCandidate(sess, cfg2)
	res2, err := k.engine.Commit(context.Background(), sess, CommitOpts{})
	if err != nil {
		t.Fatalf("Commit2: %v", err)
	}
	if !warningsContain(res2.Warnings, "reboot") {
		t.Fatalf("resource-pools 变更应有 reboot 提示（FR-SYS-002）: %+v", res2.Warnings)
	}
}

func warningsContain(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestEngineLockIdleTimeout(t *testing.T) {
	k := newEngineKit(t)
	k.edit(t, "admin", "ssh")

	// 空闲超过 TTL 自动释放锁（骨架 §3.4）
	k.clock.Advance(11 * time.Minute)
	views, _ := k.engine.Sessions()
	if len(views) != 0 {
		t.Fatalf("空闲超时后应释放锁: %+v", views)
	}
	if err := k.engine.UpdateCandidate(Session{User: "admin", Source: "ssh"}, baseCommitted()); !errors.Is(err, ErrNotEditing) {
		t.Fatalf("锁释放后编辑应报 ErrNotEditing: %v", err)
	}
	if len(*k.events) != 1 || (*k.events)[0].Type != EventLockTimeout {
		t.Fatalf("锁超时释放应产生事件: %+v", *k.events)
	}
}

func TestEngineMergeCandidate(t *testing.T) {
	k := newEngineKit(t)
	sess := Session{User: "admin", Source: "ssh"}
	k.edit(t, "admin", "ssh")

	// FR-CFG-008 load merge：增量合并，不清掉既有内容
	patch := model.Config{
		System: &model.SystemConfig{Hostname: "merged"},
		VirtualMachineFunctions: []model.VMFunction{{
			Name: "fw-vm", Image: "ubuntu22-vm",
			VCPU:   model.VMCpu{Count: 4},
			Memory: model.VMMemory{SizeMB: 8192},
		}},
	}
	if err := k.engine.MergeCandidate(sess, patch); err != nil {
		t.Fatalf("MergeCandidate: %v", err)
	}
	cand, dirty, _ := k.engine.Candidate()
	if !dirty || hostnameOf(t, cand) != "merged" {
		t.Fatalf("merge 后 candidate 不符")
	}
	if len(cand.VirtualMachineFunctions) != 1 || cand.VirtualMachineFunctions[0].Image != "ubuntu22-vm" {
		t.Fatalf("merge 应新增 VM 且保留字段: %+v", cand.VirtualMachineFunctions)
	}
	// 既有 interfaces 不应被 merge 清掉
	if len(cand.VirtualSwitches) != 1 {
		t.Fatalf("merge 不应影响既有配置")
	}
}

// ---------- FR-CFG-011⑤⑩：依赖外部状态的 commit 校验 ----------

type mockImages map[string]ImageInfo

func (m mockImages) Names() []string {
	var out []string
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (m mockImages) Lookup(name string) (ImageInfo, bool) {
	i, ok := m[name]
	return i, ok
}

type mockTopo map[string]int

func (m mockTopo) InterfaceNUMA(name string) (int, bool) {
	v, ok := m[name]
	return v, ok
}

// newEngineWithExternals 带镜像仓库与 NUMA 拓扑注入的引擎（预置基线 rev1）。
func newEngineWithExternals(t *testing.T, images ImageResolver, topo TopologyReader) (*Engine, *Store, *fakeClock) {
	t.Helper()
	store := openTestStore(t)
	clock := newFakeClock()
	if _, err := store.AppendRevision(mustJSON(baseCommitted()), clock.Now(), "初始基线"); err != nil {
		t.Fatalf("预置基线: %v", err)
	}
	e, err := NewEngine(store, &mockApplier{}, Options{
		Now:            clock.Now,
		AfterFunc:      newTimerSink().after,
		ImageResolver:  images,
		TopologyReader: topo,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e, store, clock
}

// poolsFor 无 VPP 的资源池（给 VM 分配用）。
func poolsFor() *model.ResourcePool {
	return &model.ResourcePool{
		Hugepages: []model.HPool{{PageSize: "1G", Count: 32}},
		CPU:       &model.CPUSetup{IsolatedCores: []int{4, 5, 6, 7}},
	}
}

func TestEngineImageTypeRule(t *testing.T) {
	images := mockImages{
		"ubuntu22-vm": {Name: "ubuntu22-vm", Type: "vm-image"},
		"alpine-ct":   {Name: "alpine-ct", Type: "container-image"},
	}
	e, _, _ := newEngineWithExternals(t, images, nil)
	sess := Session{User: "admin", Source: "ssh"}
	if err := e.Edit(sess); err != nil {
		t.Fatalf("Edit: %v", err)
	}

	// ⑤：VM 引用 container-image 类型镜像 → 类型不匹配
	cfg := baseCommitted()
	cfg.ResourcePools = poolsFor()
	cfg.VirtualMachineFunctions = []model.VMFunction{{
		Name: "fw-vm", Image: "alpine-ct",
		VCPU:   model.VMCpu{Count: 2},
		Memory: model.VMMemory{SizeMB: 8192, HugepageSize: "1G"},
	}}
	if err := e.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	_, err := e.Commit(context.Background(), sess, CommitOpts{})
	var ve *ValidationError
	if !errors.As(err, &ve) || !strings.Contains(fmt.Sprint(ve.Errors), "vm-image") {
		t.Fatalf("应报镜像类型不匹配（FR-CFG-011⑤）: %v", err)
	}

	// 未知镜像 → 不存在
	cfg.VirtualMachineFunctions[0].Image = "ghost"
	_ = e.UpdateCandidate(sess, cfg)
	_, err = e.Commit(context.Background(), sess, CommitOpts{})
	if !errors.As(err, &ve) || !strings.Contains(fmt.Sprint(ve.Errors), "不存在") {
		t.Fatalf("未知镜像应报错: %v", err)
	}

	// 正确类型 → 通过（连账本一起校验）
	cfg.VirtualMachineFunctions[0].Image = "ubuntu22-vm"
	_ = e.UpdateCandidate(sess, cfg)
	if _, err := e.Commit(context.Background(), sess, CommitOpts{}); err != nil {
		t.Fatalf("vm-image 应放行: %v", err)
	}
}

func TestEngineNumaWarning(t *testing.T) {
	topo := mockTopo{"ens2f0": 1}
	e, _, _ := newEngineWithExternals(t, nil, topo)
	sess := Session{User: "admin", Source: "console"}
	if err := e.Edit(sess); err != nil {
		t.Fatalf("Edit: %v", err)
	}

	// ⑩：vNIC 落点物理口 NUMA 1，VM 内存 NUMA 0 → 性能警告
	cfg := baseCommitted()
	cfg.ResourcePools = poolsFor()
	cfg.VirtualMachineFunctions = []model.VMFunction{{
		Name: "fw-vm", Image: "ubuntu22-vm",
		VCPU:       model.VMCpu{Count: 2},
		Memory:     model.VMMemory{SizeMB: 8192, HugepageSize: "1G", NumaNode: intPtr(0)},
		Interfaces: []model.VnfInterface{{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-app"}},
	}}
	if err := e.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	res, err := e.Commit(context.Background(), sess, CommitOpts{})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !warningsContain(res.Warnings, "跨 NUMA") {
		t.Fatalf("跨 NUMA 应给性能警告: %+v", res.Warnings)
	}

	// 内存 NUMA 调整为 1（与物理口一致）→ 不再警告
	cfg.VirtualMachineFunctions[0].Memory.NumaNode = intPtr(1)
	_ = e.UpdateCandidate(sess, cfg)
	res2, err := e.Commit(context.Background(), sess, CommitOpts{})
	if err != nil {
		t.Fatalf("Commit2: %v", err)
	}
	if warningsContain(res2.Warnings, "FR-CFG-011⑩") {
		t.Fatalf("NUMA 一致后不应警告: %+v", res2.Warnings)
	}
}

func intPtr(n int) *int { return &n }

// TestImageMissingMessageIsActionable（附录 A #98）：「镜像不存在」的报错要能照着排查——
// 列出仓库现有镜像；容器镜像再点出「可用名是 tar 内嵌 tag，与上传目录项名不一致时两个名字都不可用」
// 这个已知陷阱（真机踩过：用目录名 → Docker API 404；用 tag → 校验拒「不存在」）。
func TestImageMissingMessageIsActionable(t *testing.T) {
	images := mockImages{
		"alpine-ct": {Name: "alpine-ct", Type: "container-image"},
		"ubuntu-vm": {Name: "ubuntu-vm", Type: "vm-image"},
	}
	e, _, _ := newEngineWithExternals(t, images, nil)
	sess := Session{User: "admin", Source: "ssh"}
	if err := e.Edit(sess); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	cfg := baseCommitted()
	cfg.ResourcePools = poolsFor()
	cfg.ContainerFunctions = []model.ContainerFunction{{
		Name: "ct1", Image: "alpine:3.20", VCPU: 1, MemoryMB: 128,
	}}
	if err := e.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}

	_, err := e.Commit(context.Background(), sess, CommitOpts{})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("应报镜像不存在: %v", err)
	}
	msg := fmt.Sprint(ve.Errors)
	if !strings.Contains(msg, "alpine-ct") || !strings.Contains(msg, "ubuntu-vm") {
		t.Errorf("应列出仓库现有镜像: %v", msg)
	}
	if !strings.Contains(msg, "tar 内嵌 tag") {
		t.Errorf("容器镜像缺失应点出入口名/tag 口径: %v", msg)
	}

	// VM 镜像缺失：列出可选项，但不套用容器的那句口径
	cfg.ContainerFunctions = nil
	cfg.VirtualMachineFunctions = []model.VMFunction{{
		Name: "vm1", Image: "ghost", VCPU: model.VMCpu{Count: 1}, Memory: model.VMMemory{SizeMB: 1024},
	}}
	_ = e.UpdateCandidate(sess, cfg)
	_ = e.UpdateCandidate(sess, cfg)
	_, err = e.Commit(context.Background(), sess, CommitOpts{})
	if !errors.As(err, &ve) {
		t.Fatalf("应报镜像不存在: %v", err)
	}
	msg = fmt.Sprint(ve.Errors)
	if !strings.Contains(msg, "ubuntu-vm") {
		t.Errorf("应列出仓库现有镜像: %v", msg)
	}
	if strings.Contains(msg, "tar 内嵌 tag") {
		t.Errorf("VM 镜像不应套用容器口径: %v", msg)
	}
}
