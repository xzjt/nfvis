package api

// O-3（报告 docs/REPORT-v3.0.29.md §7）：无「强制释放编辑锁」命令。
//
// 现场：脏锁只能等 10 分钟（`DefaultLockIdleTTL`）或 `systemctl restart nfvis`；
// `show configuration sessions` 也不给照做路径。
//
// 修法：`request system configuration unlock [force]`
//   - 不带 force：只释放**本会话**的锁（等价 discard + 释放）；本会话没锁时如实说。
//   - 带 force：释放**全部**锁（S 级 + 审计），文案点名「持锁会话的未提交 candidate 会被丢弃」。
// 同时 `show configuration sessions` 末尾追加可照做的命令。
//
// 本文件是这两条行为的守护。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/config"
)

// 命令树里存在该形态（含可选 force），且 class 为 S。
func TestConfigUnlockInCommandTree(t *testing.T) {
	x, _ := newCLIKit(t)
	// 用 `?` 候选验证「树里有这个节点」——与操作者看到的一致（不靠内存里找结构体）。
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system configuration ?")
	if !strings.Contains(res.Output, "unlock") {
		t.Fatalf("`request system configuration ?` 应列出 unlock：%s", res.Output)
	}
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system configuration unlock ?")
	if !strings.Contains(res.Output, "force") {
		t.Fatalf("`request system configuration unlock ?` 应列出 force：%s", res.Output)
	}
}

// 无锁时如实说「无需释放」（不谎报「已释放」）。
func TestConfigUnlockNoLock(t *testing.T) {
	x, _ := newCLIKit(t)
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system configuration unlock").Output
	if !strings.Contains(out, "无持锁会话") {
		t.Fatalf("无锁时应如实说明：%q", out)
	}
	if strings.Contains(out, "已释放") {
		t.Fatalf("无锁时不应声称已释放：%q", out)
	}
	// force 在无锁时同样如实
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system configuration unlock force").Output
	if !strings.Contains(out, "无持锁会话") {
		t.Fatalf("force 无锁时应如实说明：%q", out)
	}
}

// 不带 force：释放本会话的锁，另一个用户/会话的锁不受影响。
func TestConfigUnlockReleasesOwnOnly(t *testing.T) {
	x, _ := newCLIKit(t)

	// userA（ssh）取得锁并留下未提交变更（脏锁）
	if err := x.engine.Edit(sessOfUser("alice", "ssh")); err != nil {
		t.Fatalf("alice Edit: %v", err)
	}
	if out := x.Execute("alice", aaa.ClassSuperUser, "ssh", "set system hostname alice-node").Output; strings.Contains(out, "%%") {
		t.Fatalf("alice set: %s", out)
	}
	// 另一个会话（bob）看不到锁
	if out := x.Execute("bob", aaa.ClassSuperUser, "ssh", "request system configuration unlock").Output; !strings.Contains(out, "未持有编辑锁") {
		t.Fatalf("bob 无锁时应如实说未持有：%q", out)
	}
	// alice 的锁仍在（bob 的 unlock 不该动它）
	if views, _ := x.engine.Sessions(); len(views) != 1 || views[0].User != "alice" {
		t.Fatalf("bob 的 unlock 不应释放 alice 的锁：%+v", views)
	}

	// alice 自己 unlock ⇒ 释放；新会话可立即取锁
	out := x.Execute("alice", aaa.ClassSuperUser, "ssh", "request system configuration unlock").Output
	if !strings.Contains(out, "已释放") {
		t.Fatalf("alice unlock 应释放自己的锁：%q", out)
	}
	if views, _ := x.engine.Sessions(); len(views) != 0 {
		t.Fatalf("释放后应无持锁会话：%+v", views)
	}
	if err := x.engine.Edit(sessOfUser("alice", "ssh")); err != nil {
		t.Fatalf("释放后应能重新取锁: %v", err)
	}
}

// 带 force：释放**全部**锁，且新会话能立刻取锁（报告要求的判据）。
func TestConfigUnlockForceReleasesAll(t *testing.T) {
	x, _ := newCLIKit(t)

	// alice 持脏锁
	if err := x.engine.Edit(sessOfUser("alice", "ssh")); err != nil {
		t.Fatalf("alice Edit: %v", err)
	}
	if out := x.Execute("alice", aaa.ClassSuperUser, "ssh", "set system hostname alice-node").Output; strings.Contains(out, "%%") {
		t.Fatalf("alice set: %s", out)
	}
	if views, _ := x.engine.Sessions(); len(views) != 1 {
		t.Fatalf("前置：应有 1 个持锁会话：%+v", views)
	}

	// bob（super-user）force 释放
	out := x.Execute("bob", aaa.ClassSuperUser, "ssh", "request system configuration unlock force").Output
	if strings.Contains(out, "%%") {
		t.Fatalf("force 应成功：%s", out)
	}
	if !strings.Contains(out, "已强制释放") {
		t.Fatalf("应报告已强制释放：%q", out)
	}
	// 代价必须写明（这是本命令最容易被误用的地方）
	if !strings.Contains(out, "未提交 candidate 已被丢弃") && !strings.Contains(out, "未提交的 candidate 已丢弃") {
		t.Fatalf("force 文案必须点名代价（丢弃未提交 candidate）：%q", out)
	}
	if views, _ := x.engine.Sessions(); len(views) != 0 {
		t.Fatalf("force 后应无持锁会话：%+v", views)
	}
	// 判据：新会话能立刻取锁（报文要求的「force 释放后新会话能取锁」）
	if err := x.engine.Edit(sessOfUser("bob", "ssh")); err != nil {
		t.Fatalf("force 释放后新会话应能取锁: %v", err)
	}
}

// force 必须是 S 级（operator 不得释放别人的编辑锁）。
func TestConfigUnlockForceRequiresSuperUser(t *testing.T) {
	x, _ := newCLIKit(t)
	if err := x.engine.Edit(sessOfUser("alice", "ssh")); err != nil {
		t.Fatalf("alice Edit: %v", err)
	}
	out := x.Execute("op1", aaa.ClassOperator, "ssh", "request system configuration unlock force").Output
	if !strings.Contains(out, "无权限") {
		t.Fatalf("operator 不应能强制释放：%q", out)
	}
	// 锁仍在
	if views, _ := x.engine.Sessions(); len(views) != 1 {
		t.Fatalf("被拒的 force 不应释放锁：%+v", views)
	}
}

// `show configuration sessions` 必须给出照做路径（报告原文：此前不给）。
func TestShowConfigSessionsGivesUnlockHint(t *testing.T) {
	x, _ := newCLIKit(t)
	if err := x.engine.Edit(sessOfUser("alice", "ssh")); err != nil {
		t.Fatalf("alice Edit: %v", err)
	}
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration sessions").Output
	if !strings.Contains(out, "request system configuration unlock") {
		t.Fatalf("有持锁会话时应给出照做命令：%q", out)
	}
	if !strings.Contains(out, "force") {
		t.Fatalf("super-user 应看到 force 形态：%q", out)
	}
	views2 := []config.SessionView{{Holder: "alice@ssh", User: "alice"}}

	// 非 super-user 的读视图按决策 #317 只列自己的会话（configure 本身是 S 级，operator
	// 拿不到锁，因此实际操作中看不到任何持锁行）。渲染函数自身的分支直接单测：
	hint := configUnlockHint(views2, "op1", aaa.ClassOperator)
	if strings.Contains(hint, "unlock force") {
		t.Fatalf("operator 不应被指示使用 force（无权限）：%q", hint)
	}
	if !strings.Contains(hint, "无权强制释放") {
		t.Fatalf("operator 侧应说明 force 需 super-user：%q", hint)
	}

	// 无持锁会话：不追加提示（空态保持逐字不变——既有守护依赖）
	x2, _ := newCLIKit(t)
	empty := x2.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration sessions").Output
	if strings.Contains(empty, "提示：") {
		t.Fatalf("空态不应追加提示：%q", empty)
	}
	if !strings.Contains(empty, "无持锁会话") {
		t.Fatalf("空态应保持原文案：%q", empty)
	}
}

// 语法守护：未知参数给可照做的语法，而不是落到 requestSystem 的通用分支。
func TestConfigUnlockSyntaxErrors(t *testing.T) {
	x, _ := newCLIKit(t)
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system configuration unlock bogus").Output
	if !strings.Contains(out, "语法") || !strings.Contains(out, "force") {
		t.Fatalf("未知参数应给语法提示（含 force）：%q", out)
	}
}

// sessOfUser 构造某用户在某接入源下的会话（无稳定 token ID——测试里不需要）。
func sessOfUser(user, source string) config.Session {
	return config.Session{User: user, Source: source}
}
