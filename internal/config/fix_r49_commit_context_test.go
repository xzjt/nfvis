package config

// round49 缺陷 #2（高危）：REST 提交**继承请求 ctx** —— 客户端断开会让「下发」与「失败补偿」
// 一起失效。
//
// 现场：`internal/api/configuration.go` 把 `r.Context()` 直接交给 `Engine.Commit`（:479 / :502），
// 引擎又原样交给 `applier.Apply`；`netkernel` 的 Runner 用 `exec.CommandContext` —— 上下文一旦
// 取消，宿主命令**不执行**。于是「客户端断开」这个纯客户端行为，会让服务端的下发半途而废、
// 连补偿也一起失效：数据面停在中间态、而配置没提交（数据面与配置不一致）。
//
// 修复口径：`Engine.Commit` 为底座下发派生一个**与请求取消解耦**的上下文
// （`context.WithoutCancel` + 有界超时）；请求在开始前就已取消时仍尽早返回、不进入下发。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestCommitApplyContextSurvivesRequestCancel：下发进行中调用方断开（ctx 取消）——
// 引擎交给 applier 的上下文**不得**被取消（否则补偿链路里的宿主命令根本不执行）。
func TestCommitApplyContextSurvivesRequestCancel(t *testing.T) {
	ap := &recApplier{}
	store := openTestStore(t)
	clock := newFakeClock()
	if _, err := store.AppendRevision(mustJSON(baseCommitted()), clock.Now(), "初始基线", "admin"); err != nil {
		t.Fatalf("预置基线: %v", err)
	}
	e, err := NewEngine(store, ap, Options{Now: clock.Now, AfterFunc: newTimerSink().after})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	sess := Session{User: "admin", Source: "ssh"}
	if err := e.Edit(sess); err != nil {
		t.Fatal(err)
	}
	cfg := baseCommitted()
	cfg.System.Hostname = "risky"
	if err := e.UpdateCandidate(sess, cfg); err != nil {
		t.Fatal(err)
	}

	// 客户端在下发进行中断开：就在 applier 收到调用的那一刻取消请求上下文。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ap.onCall = cancel

	res, err := e.Commit(ctx, sess, CommitOpts{})
	if err != nil {
		t.Fatalf("客户端断开不得让服务端提交半途而废（下发与补偿必须跑完）: %v", err)
	}
	if err := ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("前提不成立：请求上下文应已在调用期间被取消，实际 %v", err)
	}
	calls := ap.snapshot()
	if len(calls) != 1 {
		t.Fatalf("应发生一次下发，实际 %d 次", len(calls))
	}
	if calls[0].ctxErr != nil {
		t.Fatalf("applier 收到的上下文已被解除取消：%v（补偿链路会因此失效）", calls[0].ctxErr)
	}
	// 提交照常落库
	if res.Revision != 2 {
		t.Fatalf("提交应照常落库（rev 2），实际 %d", res.Revision)
	}
	if got, _ := e.Committed(); hostnameOf(t, got) != "risky" {
		t.Fatalf("committed 应为新配置: %s", hostnameOf(t, got))
	}
}

// TestCommitPreCancelledRequestDoesNotApply：**未开始就取消**的提交不得实际下发
// （保留下发前取消的既有语义），且 candidate/锁原样保留供重试。
func TestCommitPreCancelledRequestDoesNotApply(t *testing.T) {
	k := newEngineKit(t)
	sess := Session{User: "admin", Source: "ssh"}
	k.edit(t, "admin", "ssh")
	cfg := baseCommitted()
	cfg.System.Hostname = "risky"
	if err := k.engine.UpdateCandidate(sess, cfg); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := k.engine.Commit(ctx, sess, CommitOpts{})
	if err == nil {
		t.Fatalf("请求已取消时提交必须尽早失败（不得谎报成功）")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误应可判定为「已取消」: %v", err)
	}
	if n := k.applier.calls; n != 0 {
		t.Fatalf("未开始就取消的提交不得下发到底座，实际 %d 次", n)
	}
	if rev, _, _ := k.store.LatestRevision(); rev != 1 {
		t.Fatalf("未下发不得落库，实际 rev=%d", rev)
	}
	if cf, _ := k.store.GetConfirmed(); cf != nil {
		t.Fatalf("未下发的提交不得建立 confirmed 保护: %+v", cf)
	}
	cand, dirty, cerr := k.engine.Candidate()
	if cerr != nil || !dirty || hostnameOf(t, cand) != "risky" {
		t.Fatalf("candidate 应原样保留（可重提）: %v dirty=%v %s", cerr, dirty, hostnameOf(t, cand))
	}
	audit, _ := k.store.ListAudit(10, 0)
	if len(audit) == 0 || audit[0].Result != "failure" || !strings.Contains(audit[0].Detail, "已取消") {
		t.Fatalf("取消的提交应入一条 failure 审计并点名原因: %+v", audit)
	}
}
