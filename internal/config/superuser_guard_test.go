package config

// 决策 #152：整文档提交的「至少留一个 super-user」兜底（交接 §2.3 登记、round77 复验捅出）。
//
// `DELETE /system/login-users/{name}` 早有「不能删最后一个 super-user」守卫，但**整文档提交**
// （PUT /configuration/candidate + commit、CLI `load override`、`request system configuration
// restore`）没有等价兜底：一次提交就能把本机提交成「无人可登录」，只能带外恢复。守卫因此落在
// `Engine.Commit` 里（**不**放进 `Options.Validate` 那条可注入的链——自定义链会把它整条漏掉），
// 失败语义与既有校验一致：返回 ValidationError、候选保留、编辑锁保留。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// newSuperUserKit 基线只含一个 super-user、**不含管理口配置**：本文件的断言要只针对自锁
// 兜底，不能让「管理口变更需 commit confirmed」（FR-CFG-012）那条守卫抢先命中
// ——空配置相对带管理口的基线正是"管理口变更"。
func newSuperUserKit(t *testing.T) *engineKit {
	t.Helper()
	store := openTestStore(t)
	clock := newFakeClock()
	timers := newTimerSink()
	applier := &mockApplier{}
	base := model.Config{System: &model.SystemConfig{
		Hostname: "nfvis-node1",
		Login: &model.SystemLogin{Users: []model.LoginUserConfig{
			{Name: "admin", Class: model.ClassSuperUser, PasswordHash: fixtureUserHash},
		}},
	}}
	if _, err := store.AppendRevision(mustJSON(base), clock.Now(), "初始基线", "admin"); err != nil {
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

// ①空配置提交被拒：报错可读、候选保留、锁保留、committed 不动。
func TestCommitRejectsEmptyConfigWithoutSuperUser(t *testing.T) {
	k := newSuperUserKit(t)
	sess := Session{User: "admin", Source: "ssh"}
	k.edit(t, "admin", "ssh")

	if err := k.engine.UpdateCandidate(sess, model.Config{}); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	_, err := k.engine.Commit(context.Background(), sess, CommitOpts{})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("空配置提交应返回 ValidationError，实得 %v", err)
	}
	if len(ve.Errors) != 1 || ve.Errors[0].Path != "system.login.users" {
		t.Fatalf("应恰好一条 system.login.users 错误，实得 %+v", ve.Errors)
	}
	msg := ve.Errors[0].Message
	for _, want := range []string{"super-user", "无人可登录", "zeroize"} {
		if !strings.Contains(msg, want) {
			t.Errorf("报错应可照着办（缺 %q）: %q", want, msg)
		}
	}
	if strings.Contains(msg, "FR-") {
		t.Errorf("给操作者看的文本不得含需求编号: %q", msg)
	}

	// 候选保留（操作者改完可以直接重提）、锁保留
	cand, dirty, err := k.engine.Candidate()
	if err != nil || !dirty || cand.System != nil {
		t.Fatalf("被拒后候选应原样保留且仍脏: %v dirty=%v %+v", err, dirty, cand.System)
	}
	if err := k.engine.Edit(Session{User: "other", Source: "ssh"}); !errors.Is(err, ErrLocked) {
		t.Fatalf("被拒后编辑锁应保留（他人 Edit 应 ErrLocked），实得 %v", err)
	}
	// committed 不动：账号还在，rev 不增
	got, err := k.engine.Committed()
	if err != nil || got.System == nil || got.System.Login == nil || len(got.System.Login.Users) != 1 {
		t.Fatalf("被拒后 committed 不应变化: %v %+v", err, got.System)
	}
	if rev, _, _ := k.store.LatestRevision(); rev != 1 {
		t.Fatalf("被拒不应产生新修订，实得 rev=%d", rev)
	}

	// 补回 super-user 后同一候选即可提交（不是"卡死"，是"改完再提"）
	fix := model.Config{System: &model.SystemConfig{Login: &model.SystemLogin{
		Users: []model.LoginUserConfig{{Name: "admin", Class: model.ClassSuperUser, PasswordHash: fixtureUserHash}},
	}}}
	if err := k.engine.UpdateCandidate(sess, fix); err != nil {
		t.Fatalf("UpdateCandidate(修好): %v", err)
	}
	if _, err := k.engine.Commit(context.Background(), sess, CommitOpts{}); err != nil {
		t.Fatalf("补回 super-user 后应能提交: %v", err)
	}
}

// ②只有 operator / read-only 用户的配置被拒；③class 为空的账号按 read-only 算，同样被拒。
func TestCommitRejectsNonSuperUserOnlyConfig(t *testing.T) {
	for _, tc := range []struct {
		name  string
		users []model.LoginUserConfig
	}{
		{"只有 operator", []model.LoginUserConfig{{Name: "netop", Class: "operator", PasswordHash: fixtureUserHash}}},
		{"只有 read-only", []model.LoginUserConfig{{Name: "viewer", Class: "read-only", PasswordHash: fixtureUserHash}}},
		{"class 为空（缺省只读）", []model.LoginUserConfig{{Name: "viewer", PasswordHash: fixtureUserHash}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newEngineKit(t)
			sess := Session{User: "admin", Source: "ssh"}
			k.edit(t, "admin", "ssh")

			cfg := baseCommitted()
			cfg.System.Login = &model.SystemLogin{Users: tc.users}
			if err := k.engine.UpdateCandidate(sess, cfg); err != nil {
				t.Fatalf("UpdateCandidate: %v", err)
			}
			_, err := k.engine.Commit(context.Background(), sess, CommitOpts{})
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("没有 super-user 的配置应被拒，实得 %v", err)
			}
			if len(ve.Errors) != 1 || !strings.Contains(ve.Errors[0].Message, "super-user") {
				t.Fatalf("错误应点明缺 super-user，实得 %+v", ve.Errors)
			}
		})
	}
}

// ④正常含 super-user 的提交照常通过（含"super-user + 其它账号"混排）。
func TestCommitWithSuperUserStillPasses(t *testing.T) {
	k := newEngineKit(t)
	sess := Session{User: "admin", Source: "ssh"}
	k.edit(t, "admin", "ssh")

	cfg := baseCommitted()
	cfg.System.Hostname = "renamed"
	cfg.System.Login.Users = append(cfg.System.Login.Users,
		model.LoginUserConfig{Name: "netop", Class: "operator", PasswordHash: fixtureUserHash})
	if err := k.engine.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	res, err := k.engine.Commit(context.Background(), sess, CommitOpts{})
	if err != nil {
		t.Fatalf("含 super-user 的提交应通过: %v", err)
	}
	if res.Revision != 2 {
		t.Fatalf("应产生 rev 2，实得 %d", res.Revision)
	}
}

// ⑤恢复出厂（zeroize）例外：带 AllowNoSuperUser 才能把配置提交成空账号表。
func TestCommitAllowNoSuperUserExemptsZeroize(t *testing.T) {
	k := newSuperUserKit(t)
	sess := Session{User: "admin", Source: SourceZeroize}
	k.edit(t, "admin", SourceZeroize)

	if err := k.engine.UpdateCandidate(sess, model.Config{}); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	// 不带豁免 → 拒（这正是恢复出厂必须显式声明豁免的原因）
	if _, err := k.engine.Commit(context.Background(), sess, CommitOpts{}); err == nil {
		t.Fatal("不带豁免的空配置提交应被拒")
	}
	res, err := k.engine.Commit(context.Background(), sess, CommitOpts{AllowNoSuperUser: true, Message: "zeroize"})
	if err != nil {
		t.Fatalf("带豁免的空配置提交应通过: %v", err)
	}
	if res.Revision != 2 {
		t.Fatalf("应产生 rev 2，实得 %d", res.Revision)
	}
	got, err := k.engine.Committed()
	if err != nil || got.System != nil {
		t.Fatalf("恢复出厂后 committed 应为空配置: %v %+v", err, got.System)
	}
}

// 守卫不在可注入的校验链里：自定义 Validate 把既有校验整条换掉时，兜底仍然生效
// （否则装配方一换链子，自锁防护就静默消失）。
func TestSuperUserGuardSurvivesCustomValidateChain(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.AppendRevision(mustJSON(baseCommitted()), newFakeClock().Now(), "基线", "admin"); err != nil {
		t.Fatalf("预置基线: %v", err)
	}
	e, err := NewEngine(store, &mockApplier{}, Options{
		Now: newFakeClock().Now,
		// 自定义链：什么都不校验（模拟"装配方换了自己的校验器"）
		Validate: func(model.Config) []model.ValidateError { return nil },
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	sess := Session{User: "admin", Source: "ssh"}
	if err := e.Edit(sess); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if err := e.UpdateCandidate(sess, model.Config{}); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	if _, err := e.Commit(context.Background(), sess, CommitOpts{}); err == nil {
		t.Fatal("自定义校验链把校验换掉了，但自锁兜底仍须生效（决策 #152：守卫不走 Validate 链）")
	}
}
