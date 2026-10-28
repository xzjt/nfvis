package api

// round51 评审 L6：`request system configuration unlock force` 把「不持锁的占位行」算成
// 已释放的锁。
//
// `Engine.Sessions()` 的返回里有两类行：真的持锁会话行，以及**在途 commit confirmed 的
// 占位行**（决策 #377/E13：confirmed 的 Holder 在列表里没有同名持锁行时如实追加一行——
// 该行 `SessionID == ""`、锁早已不在）。force 分支只跳过 `SessionID == "" && Holder == ""`
// 的行，占位行（Holder 非空）因此照样进循环；构造出的 `config.Session{ID: ""}` 调 Discard
// 得到 `ErrNotEditing`，而它在字符串上与「未持有」命中 ⇒ 被计进 `released`，输出
// 「已强制释放 N 个编辑锁……未提交 candidate 已被丢弃」——**什么都没释放**，还顺手把
// 「占位行」当成真正的锁（复核分支同样按 `Holder != ""` 判定，于是同屏出现
// 「已强制释放 1 个编辑锁」＋「释放后仍有持锁会话」这一对自相矛盾的话）。
//
// 判据：锁**确实被释放**才算已释放；占位行如实归入「无锁可释放」，也不再进复核的
// 「仍持锁」名单。真实锁的措辞与退出码/审计口径不动。

import (
	"strings"
	"testing"
)

// TestFixR51UnlockForceLocklessConfirmedRow：只剩在途 confirmed 占位行时，force 必须如实
// 报「无锁可释放」，不得谎报已释放。
func TestFixR51UnlockForceLocklessConfirmedRow(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set system hostname r51-node",
		"commit confirmed 5",
		"exit") // 退出配置模式＝释放锁；confirmed 仍在途

	views, err := engine.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Holder == "" || views[0].SessionID != "" || views[0].ConfirmedUntil == nil {
		t.Fatalf("前置：应只剩在途 commit confirmed 的占位行（无锁）: %+v", views)
	}

	out := x.Execute("admin", aaaClassSU, "ssh", "request system configuration unlock force").Output
	if strings.Contains(out, "已强制释放") || strings.Contains(out, "未提交 candidate 已被丢弃") {
		t.Fatalf("占位行不持有编辑锁，不得谎报已释放（含代价文案）：%q", out)
	}
	if !strings.Contains(out, "无锁可释放") {
		t.Fatalf("应如实报「无锁可释放」：%q", out)
	}
	if strings.Contains(out, "仍有持锁会话") {
		t.Fatalf("占位行不是锁，复核不得把它算成持锁会话：%q", out)
	}
	// 现场未被误改：confirmed 仍在途（占位行不是锁，force 不该动它）
	after, err := engine.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].ConfirmedUntil == nil {
		t.Fatalf("force 不应动在途 confirmed：%+v", after)
	}
}

// TestFixR51UnlockForceRealLockStillReports：正控——真有锁时措辞与行为逐字不变（含代价提示）。
func TestFixR51UnlockForceRealLockStillReports(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "alice", aaaClassSU, "ssh", "configure", "set system hostname alice-node")
	views, err := engine.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].SessionID == "" && views[0].Holder == "" {
		t.Fatalf("前置：应有 1 个持锁会话：%+v", views)
	}

	out := x.Execute("bob", aaaClassSU, "ssh", "request system configuration unlock force").Output
	if !strings.Contains(out, "已强制释放 1 个编辑锁") {
		t.Fatalf("真实锁的措辞应逐字不变：%q", out)
	}
	if !strings.Contains(out, "未提交 candidate 已被丢弃") {
		t.Fatalf("代价提示须保留：%q", out)
	}
	if strings.Contains(out, "无锁可释放") {
		t.Fatalf("真实锁被释放时不应报「无锁可释放」：%q", out)
	}
	if views, _ = engine.Sessions(); len(views) != 0 {
		t.Fatalf("force 后应无持锁会话：%+v", views)
	}
}
