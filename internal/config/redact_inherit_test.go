package config

// R44-1（round44 可视验收发现）：脱敏视图回写不得抹掉口令哈希。
//
// 背景：配置视图（GET /configuration、candidate 读取）按 FR-SEC-007 / 决策 #25 **移除**
// 口令哈希；客户端据此整文档回写（Web 控制台的配置页正是这么做的）时，入参里没有哈希——
// 若原样落进 candidate，提交就会让该账号失去口令。引擎对"入参缺失的敏感叶子"从 committed
// 同名用户继承（客户端没收到过的东西，不该被它删掉）。

import (
	"context"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

func TestUpdateCandidateInheritsRedactedPasswordHash(t *testing.T) {
	store := openTestStore(t)
	clock := newFakeClock()
	base := baseCommitted()
	base.System.Login = &model.SystemLogin{Users: []model.LoginUserConfig{
		{Name: "admin", Class: "super-user", PasswordHash: "pbkdf2$sha256$600000$c2FsdA$aGFzaA"},
	}}
	if _, err := store.AppendRevision(mustJSON(base), clock.Now(), "基线"); err != nil {
		t.Fatalf("预置基线: %v", err)
	}
	e, err := NewEngine(store, &mockApplier{}, Options{Now: clock.Now})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	sess := Session{User: "admin", Source: "api"}
	if err := e.Edit(sess); err != nil {
		t.Fatalf("Edit: %v", err)
	}

	// 模拟"脱敏视图回写"：整文档里用户**没有** password_hash（视图把它移除了）。
	in := deepCopyConfig(base)
	in.System.Hostname = "changed-by-web"
	in.System.Login.Users[0].PasswordHash = ""
	if err := e.UpdateCandidate(sess, in); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}

	cand, _, err := e.Candidate()
	if err != nil {
		t.Fatalf("Candidate: %v", err)
	}
	if got := cand.System.Login.Users[0].PasswordHash; got == "" {
		t.Fatal("candidate 里口令哈希被抹掉了：脱敏视图回写会毁掉该账号（R44-1 未修）")
	}
	if cand.System.Hostname != "changed-by-web" {
		t.Fatalf("普通字段应照常更新，得到 %q", cand.System.Hostname)
	}

	// 显式给出新哈希时不得被旧值覆盖（改口令路径）。
	in2 := deepCopyConfig(base)
	in2.System.Login.Users[0].PasswordHash = "pbkdf2$sha256$600000$bmV3$bmV3"
	if err := e.UpdateCandidate(sess, in2); err != nil {
		t.Fatalf("UpdateCandidate(新哈希): %v", err)
	}
	cand, _, err = e.Candidate()
	if err != nil {
		t.Fatalf("Candidate: %v", err)
	}
	if got := cand.System.Login.Users[0].PasswordHash; got != "pbkdf2$sha256$600000$bmV3$bmV3" {
		t.Fatalf("显式新哈希应生效，得到 %q", got)
	}
	if _, err := e.Commit(context.Background(), sess, CommitOpts{}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}
