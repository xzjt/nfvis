package api

// 决策 #318：干净锁不排他 + 路径级释放（修 R98-1，#317 引入的回归）。
//
// round98 真机：`cli-fulltest` 从 198/0/11 退化为 195/3/11、语义 12/0/1 → 10/1/1，失败签名
// 统一为「candidate 会话锁被占用: 由 admin@ssh 持有」，现场 `sessions` 里躺着一把
// `{holder:"admin@ssh", dirty:false}` 的**干净锁**无人释放。根因是 CLI 一次调用留下的锁
// （`commit and-quit` 之后；且登出按 `user@api` 身份键清不掉 `user@ssh` 的锁）挡住了下一次调用。
//
// 本文件按**真机踩到的场景**逐条断言：连续多次 CLI configure/commit（新 token）不再被上一把
// 干净锁挡住；脚本步骤多轮执行全成功；脏候选仍严格排他；接管路径；登出跨接入源释放。

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// loginSuperUserNamed 引导一个**额外的 super-user**（跨用户排他性用例需要），并登录取 token。
func loginSuperUserNamed(t *testing.T, ts *httptest.Server, adminToken, name string) string {
	t.Helper()
	status, _, body := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/login-users", adminToken,
		map[string]any{"name": name, "kind": "user", "password": "Extra-SuperUser-Passw0rd!", "class": "super-user"}, nil)
	if status != http.StatusCreated {
		t.Fatalf("建 super-user %s：%d %s", name, status, body)
	}
	st, resp := login(t, ts, name, "Extra-SuperUser-Passw0rd!")
	if st != http.StatusOK || resp.Token == "" {
		t.Fatalf("登录 %s：%d %+v", name, st, resp)
	}
	return resp.Token
}

// noLockSeen 用给定 token 读会话表，断言当前无持锁会话。
func noLockSeen(t *testing.T, ts *httptest.Server, token, what string) {
	t.Helper()
	if h := lockHolder(t, ts, token); h != "" {
		t.Fatalf("%s：不该还有持锁会话，实得 holder=%q", what, h)
	}
}

// TestCLICleanLockTakeoverNewInvocation 干净锁被同一用户的**新 CLI 调用**（新 token）接管。
//
// 现场最小复现：会话 A `configure` 后未 exit（干净锁留着）→ 新 token 的 B `configure` 必须成功
// （修复前：`%% candidate 会话锁被占用: 由 admin@ssh 持有`）。
func TestCLICleanLockTakeoverNewInvocation(t *testing.T) {
	ts := newTestServer(t)
	tokenA, tokenB := loginAdminTwice(t, ts)

	cliRun(t, ts, tokenA, "ssh", "configure") // A 取锁、不 exit（干净锁残留）
	if h := lockHolder(t, ts, tokenA); h != "admin@ssh" {
		t.Fatalf("A 应持锁: %q", h)
	}

	res := cliLine(t, ts, tokenB, "ssh", "configure") // B 是同一用户的新会话
	if strings.Contains(res.Output, "%%") || res.Mode != "config" {
		t.Fatalf("干净锁应被同用户新会话接管，B configure 实得：%q mode=%q", res.Output, res.Mode)
	}
	views := sessionsOf(t, ts, tokenB)
	if len(views) != 1 {
		t.Fatalf("接管后应恰一条持锁会话：%+v", views)
	}
	if got, _ := views[0]["holder"].(string); got != "admin@ssh" {
		t.Errorf("接管后 holder 应仍是展示用 admin@ssh，实得 %q", got)
	}
	// A 被接管后再写入应得到**明确**错误（不是静默变成别人）。
	if out := cliLine(t, ts, tokenA, "ssh", "set system hostname stale-a").Output; !strings.Contains(out, "%%") ||
		!strings.Contains(out, "编辑权") {
		t.Fatalf("被接管后 A 写入应明确报错，实得：%q", out)
	}
	cliRun(t, ts, tokenB, "ssh", "exit")
	noLockSeen(t, ts, tokenB, "B exit 后")
}

// TestCLICommitAndQuitReleasesLock `commit and-quit` 提交成功后**必定释放**本会话锁
// （R98-1 实测那把残留干净锁的来路），且脚本多轮（新 token）全部成功。
func TestCLICommitAndQuitReleasesLock(t *testing.T) {
	ts := newTestServer(t)
	for round := 1; round <= 3; round++ {
		token := loginAdmin(t, ts) // 每轮都是一次新的 CLI 调用（新 token）
		cliRun(t, ts, token, "ssh", "configure")
		cliRun(t, ts, token, "ssh", fmt.Sprintf("set system hostname and-quit-round%d", round))
		res := cliLine(t, ts, token, "ssh", "commit and-quit")
		if strings.Contains(res.Output, "%%") {
			t.Fatalf("第 %d 轮 commit and-quit 失败：%s", round, res.Output)
		}
		if res.Mode != "oper" {
			t.Errorf("第 %d 轮 and-quit 后应回到操作模式：mode=%q", round, res.Mode)
		}
		noLockSeen(t, ts, token, "and-quit 之后")
	}
}

// TestCLICommitAndQuitFailureKeepsEditing `commit and-quit` 提交**失败**时保持配置模式与锁
// （操作者要接着改），不得把人踢出配置模式又留下无人认领的锁。
func TestCLICommitAndQuitFailureKeepsEditing(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	cliRun(t, ts, token, "ssh", "configure")
	// vlan 越界 → commit 校验失败（候选保留）。
	cliRun(t, ts, token, "ssh", "set virtual-switches vs1 type l2")
	cliRun(t, ts, token, "ssh", "set virtual-switches vs1 vlan access 4095")
	res := cliLine(t, ts, token, "ssh", "commit and-quit")
	if !strings.Contains(res.Output, "校验失败") {
		t.Fatalf("越界提交应校验失败，实得：%s", res.Output)
	}
	if res.Mode != "config" {
		t.Errorf("提交失败后应仍在配置模式：mode=%q", res.Mode)
	}
	assertLockKept(t, ts, token, "admin@ssh", "commit and-quit 失败之后")
	cliRun(t, ts, token, "ssh", "discard")
	cliRun(t, ts, token, "ssh", "exit")
	noLockSeen(t, ts, token, "清理后")
}

// TestCLILogoutReleasesCLISessionLock 登出（REST 身份，holder 是 user@api）也要清掉
// **同一 token** 的 CLI 会话（holder user@ssh）留下的锁——这正是 #317 后释放路径的缺口。
func TestCLILogoutReleasesCLISessionLock(t *testing.T) {
	ts := newTestServer(t)
	_, respA := login(t, ts, "admin", "s3cret-Passw0rd!")
	tokenA := respA.Token

	cliRun(t, ts, tokenA, "ssh", "configure")
	cliRun(t, ts, tokenA, "ssh", "set system hostname logout-clean") // 脏，登出应连同候选一起丢
	if h := lockHolder(t, ts, tokenA); h != "admin@ssh" {
		t.Fatalf("登出前应持锁：%q", h)
	}
	logoutToken(t, ts, tokenA)

	// 换一个新 token 查：CLI 会话留下的锁应随本会话登出一起清掉。
	fresh := loginAdmin(t, ts)
	noLockSeen(t, ts, fresh, "登出（跨接入源）之后")
	// 新会话立刻能进入配置模式（R98-1 的症状消失）。
	if res := cliLine(t, ts, fresh, "ssh", "configure"); strings.Contains(res.Output, "%%") {
		t.Fatalf("登出后新会话应能取锁，实得：%s", res.Output)
	}
	cliRun(t, ts, fresh, "ssh", "exit")
}

// TestCLIDirtyLockStillExclusiveSameUser 脏候选时严格排他（R79-1 的保护语义不回归）：
// 同一用户的另一会话 configure 仍被拒，且原候选原封不动。
func TestCLIDirtyLockStillExclusiveSameUser(t *testing.T) {
	ts := newTestServer(t)
	tokenA, tokenB := loginAdminTwice(t, ts)

	cliRun(t, ts, tokenA, "ssh", "configure")
	cliRun(t, ts, tokenA, "ssh", "set system hostname dirty-protected")

	res := cliLine(t, ts, tokenB, "ssh", "configure")
	if !strings.Contains(res.Output, "锁被占用") {
		t.Fatalf("脏锁不得被同用户新会话接管，实得：%s", res.Output)
	}
	if got, dirty := candidateHostname(t, ts, tokenA); got != "dirty-protected" || !dirty {
		t.Fatalf("原会话的脏候选被动了：hostname=%q dirty=%v", got, dirty)
	}
	cliRun(t, ts, tokenA, "ssh", "discard")
	cliRun(t, ts, tokenA, "ssh", "exit")
}

// TestCLICleanLockNotTakeoverableCrossUser 跨用户即使锁是干净的也**不得**接管。
func TestCLICleanLockNotTakeoverableCrossUser(t *testing.T) {
	ts := newTestServer(t)
	admin := loginAdmin(t, ts)
	other := loginSuperUserNamed(t, ts, admin, "opsdirty")

	cliRun(t, ts, admin, "ssh", "configure") // admin 持干净锁
	res := cliLine(t, ts, other, "ssh", "configure")
	if !strings.Contains(res.Output, "锁被占用") {
		t.Fatalf("跨用户不得接管干净锁，实得：%s", res.Output)
	}
	if h := lockHolder(t, ts, admin); h != "admin@ssh" {
		t.Fatalf("被拒后锁应仍是 admin 的：%q", h)
	}
	cliRun(t, ts, admin, "ssh", "exit")
}
