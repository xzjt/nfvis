package api

// 决策 #317：会话与身份键分离，根治 R79-1。
//
// 由来（R79-1，round79 发布校验 H 段最小复现）：候选与编辑锁按 `user@api`（**身份键**）
// 生效，而 `sessionFromIdentity` 把所有 REST 流量都映射成 `{用户, "api"}`、CLI 的
// `Teardown()` 又恒调 `Logout()` ⇒ 同一用户跑任意一条 CLI 命令（哪怕只是 `show version`）
// 会把该用户在 api 侧未提交的候选与编辑锁一起丢掉。现场：token1 `PUT /configuration/candidate`
// （脏）→ `sessions=[{holder:"admin@api"}]`；token2 `POST /logout` **204** → sessions 空、
// token1 读 candidate **CONFLICT**，而 token1 本人仍认证。
//
// 本文件是那一段现场的最小复现自动化版：会话 A 建候选 → 会话 B（同用户、不同 token）
// 跑一条命令 / 登出 → 断言 A 的候选原封不动、A 仍持锁；反向 A 登出只清自己。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// loginAdminTwice 同一用户两次登录——得到两个**不同 token**（= 两个不同会话）。
func loginAdminTwice(t *testing.T, ts *httptest.Server) (string, string) {
	t.Helper()
	_, a := login(t, ts, "admin", "s3cret-Passw0rd!")
	_, b := login(t, ts, "admin", "s3cret-Passw0rd!")
	if a.Token == "" || b.Token == "" || a.Token == b.Token {
		t.Fatalf("两次登录应得到两个不同 token: a=%q b=%q", a.Token, b.Token)
	}
	if a.TokenID == "" || b.TokenID == "" || a.TokenID == b.TokenID {
		t.Fatalf("两次登录应得到两个不同 token_id: a=%q b=%q", a.TokenID, b.TokenID)
	}
	return a.Token, b.Token
}

// sessionsOf 读会话表（GET /system/configuration/sessions 原样列表）。
func sessionsOf(t *testing.T, ts *httptest.Server, token string) []map[string]any {
	t.Helper()
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/configuration/sessions", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("sessions: %d %s", status, body)
	}
	var out []map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析 sessions: %v (%s)", err, body)
	}
	return out
}

// candidateHostname 读本会话候选里的 hostname 与 dirty 标记。
func candidateHostname(t *testing.T, ts *httptest.Server, token string) (string, bool) {
	t.Helper()
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/candidate", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET candidate: %d %s", status, body)
	}
	var got struct {
		Candidate struct {
			System struct {
				Hostname string `json:"hostname"`
			} `json:"system"`
		} `json:"candidate"`
		Dirty bool `json:"dirty"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("解析 candidate: %v (%s)", err, body)
	}
	return got.Candidate.System.Hostname, got.Dirty
}

// putCandidate 写本会话候选（未提交）。
func putCandidate(t *testing.T, ts *httptest.Server, token, hostname string) {
	t.Helper()
	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		map[string]any{"system": map[string]any{"hostname": hostname,
			"login": map[string]any{"users": []map[string]any{superUserDoc()}}}}, nil)
	if status != http.StatusOK {
		t.Fatalf("PUT candidate: %d %s", status, body)
	}
}

func logoutToken(t *testing.T, ts *httptest.Server, token string) {
	t.Helper()
	status, _ := postJSON(t, ts.URL+APIPrefix+"/logout", nil, map[string]string{"Authorization": "Bearer " + token})
	if status != http.StatusNoContent {
		t.Fatalf("logout 应 204: %d", status)
	}
}

// TestR79_1OtherSessionLogoutKeepsCandidate：R79-1 核心——**别的会话登出不得丢弃本会话的候选**。
//
// 现场三连：A 建脏候选 → B 跑一条 CLI 命令（`show version`）→ B 登出；
// 断言 A 的候选逐字段原封不动、A 仍持锁（同一 user@api 的两个 token 是两个会话）。
func TestR79_1OtherSessionLogoutKeepsCandidate(t *testing.T) {
	ts := newTestServer(t)
	tokenA, tokenB := loginAdminTwice(t, ts)

	putCandidate(t, ts, tokenA, "r79-hold")
	if got, dirty := candidateHostname(t, ts, tokenA); got != "r79-hold" || !dirty {
		t.Fatalf("A 建候选后应读到 r79-hold 且 dirty: got=%q dirty=%v", got, dirty)
	}
	if h := lockHolder(t, ts, tokenA); h != "admin@api" {
		t.Fatalf("A 应持锁 admin@api，实得 %q", h)
	}

	// B 跑一条一次性 CLI 命令（哪怕只是 show version）。CLI 收尾会 Logout。
	cliRun(t, ts, tokenB, "api", "show version")
	// B 再显式登出（= 控制台/脚本侧的一次性事务收尾）。
	logoutToken(t, ts, tokenB)

	// A 的候选必须原封不动、A 仍持锁。
	if got, dirty := candidateHostname(t, ts, tokenA); got != "r79-hold" || !dirty {
		t.Errorf("R79-1：别的会话登出后 A 的候选被动了：hostname=%q dirty=%v（应仍为 r79-hold/true）", got, dirty)
	}
	if h := lockHolder(t, ts, tokenA); h != "admin@api" {
		t.Errorf("R79-1：别的会话登出后 A 的编辑锁被释放：holder=%q", h)
	}
	// A 仍能正常提交（证明候选与锁都还在）。
	status, _, body := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/configuration/commit", tokenA, map[string]any{}, nil)
	if status != http.StatusOK {
		t.Fatalf("A 提交应成功（候选与锁都还在）：%d %s", status, body)
	}
}

// TestR79_1OwnLogoutDiscardsOnlyOwn：反向——A 自己登出只清自己的候选与锁，B 不受影响。
func TestR79_1OwnLogoutDiscardsOnlyOwn(t *testing.T) {
	ts := newTestServer(t)
	tokenA, tokenB := loginAdminTwice(t, ts)

	// B 先建候选（B 是持锁方）。
	putCandidate(t, ts, tokenB, "r79-owner")
	if h := lockHolder(t, ts, tokenB); h != "admin@api" {
		t.Fatalf("B 应持锁 admin@api，实得 %q", h)
	}

	// A 登出（A 没持锁）：不得动 B 的候选或锁。
	logoutToken(t, ts, tokenA)
	if got, dirty := candidateHostname(t, ts, tokenB); got != "r79-owner" || !dirty {
		t.Errorf("A 登出后 B 的候选被动了：hostname=%q dirty=%v（应仍为 r79-owner/true）", got, dirty)
	}
	if h := lockHolder(t, ts, tokenB); h != "admin@api" {
		t.Errorf("A 登出后 B 的锁被释放：holder=%q", h)
	}

	// B 自己登出：候选与锁随之清掉（决策 #119 语义只作用于本会话）。
	logoutToken(t, ts, tokenB)
	// 换一个新 token 查会话表（B 的 token 已吊销）：锁不该还在。
	tokenC, _ := loginAdminTwice(t, ts)
	if views := sessionsOf(t, ts, tokenC); len(views) != 0 {
		t.Fatalf("B 自己登出后候选锁应释放，仍见 %+v", views)
	}
}

// TestR79_1SessionsListedIndependently：会话表如实列出在编辑会话的标识与所属用户，不合并。
func TestR79_1SessionsListedIndependently(t *testing.T) {
	ts := newTestServer(t)
	_, respA := login(t, ts, "admin", "s3cret-Passw0rd!")
	_, respB := login(t, ts, "admin", "s3cret-Passw0rd!")

	putCandidate(t, ts, respA.Token, "r79-list")
	views := sessionsOf(t, ts, respA.Token)
	if len(views) != 1 {
		t.Fatalf("应恰有 1 条在编辑会话，实得 %d：%+v", len(views), views)
	}
	v := views[0]
	if h, _ := v["holder"].(string); h != "admin@api" {
		t.Errorf("holder 应为展示用 admin@api，实得 %q", h)
	}
	if u, _ := v["user"].(string); u != "admin" {
		t.Errorf("会话应带所属用户 admin，实得 %q", u)
	}
	if id, _ := v["session_id"].(string); id != respA.TokenID {
		t.Errorf("会话应带持锁 token 的稳定 ID %q，实得 %q", respA.TokenID, id)
	}
	// B 的会话标识不得出现在持锁会话里（不合并、不张冠李戴）。
	if strings.Contains(string(mustJSON(t, views)), respB.TokenID) {
		t.Errorf("持锁会话里不应出现 B 的会话标识 %q：%+v", respB.TokenID, views)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestR79_1ShowSessionsListsIdentity：`show configuration sessions` 如实列出会话标识与所属用户
// （与 REST 同源），并保留等价写法。
func TestR79_1ShowSessionsListsIdentity(t *testing.T) {
	ts := newTestServer(t)
	_, respA := login(t, ts, "admin", "s3cret-Passw0rd!")
	putCandidate(t, ts, respA.Token, "r79-show")

	for _, form := range []string{"show system configuration sessions", "show configuration sessions"} {
		out := cliRun(t, ts, respA.Token, "ssh", form)
		for _, want := range []string{"Holder", "Session", "User", "admin@api", respA.TokenID, "admin"} {
			if !strings.Contains(out, want) {
				t.Errorf("%q 输出应含 %q，实得：\n%s", form, want, out)
			}
		}
	}
}

// TestR79_1RevokeSelfEqualsLogout：吊销**当前**会话等价于登出（丢弃本会话候选、释放锁）。
func TestR79_1RevokeSelfEqualsLogout(t *testing.T) {
	ts := newTestServer(t)
	_, respA := login(t, ts, "admin", "s3cret-Passw0rd!")
	putCandidate(t, ts, respA.Token, "revoke-self")
	idA := respA.TokenID

	if status, body := revokeAPIToken(t, ts, respA.Token, idA); status != http.StatusNoContent {
		t.Fatalf("吊销当前会话应 204：%d %s", status, body)
	}
	// 换一个新会话查：候选锁应已释放。
	_, respC := login(t, ts, "admin", "s3cret-Passw0rd!")
	if views := sessionsOf(t, ts, respC.Token); len(views) != 0 {
		t.Fatalf("吊销当前会话后候选锁应释放，仍见 %+v", views)
	}
}

// TestR79_1RevokeOtherKeepsCandidateUntilSweep：吊销**他人**会话不主动释放其锁
// （口径：由既有空闲巡检回收）——这是本决策明确择定的路径，别让实现悄悄改了。
func TestR79_1RevokeOtherKeepsCandidateUntilSweep(t *testing.T) {
	ts := newTestServer(t)
	_, respA := login(t, ts, "admin", "s3cret-Passw0rd!") // super-user，执行吊销
	_, respB := login(t, ts, "admin", "s3cret-Passw0rd!")
	putCandidate(t, ts, respB.Token, "revoke-other")
	idB := respB.TokenID

	if status, body := revokeAPIToken(t, ts, respA.Token, idB); status != http.StatusNoContent {
		t.Fatalf("super-user 吊销他人会话应 204：%d %s", status, body)
	}
	views := sessionsOf(t, ts, respA.Token)
	if len(views) != 1 {
		t.Fatalf("吊销他人会话不应主动释放其编辑锁（由空闲巡检回收），实得 %+v", views)
	}
	if id, _ := views[0]["session_id"].(string); id != idB {
		t.Fatalf("持锁会话仍是 B 的会话 %q，实得 %q", idB, id)
	}
}

// TestR79_1NonSuperSeesOnlyOwnSessions：非 super-user 只看到自己的会话（#301 同口径）；
// super-user 看全部。配置锁只有 super-user 会话能取得，故非 super 实际看不到别人的编辑锁。
// 走 CLI 路径（该 show 命令是 R 级；REST 同端点本就要求 super-user，非 super 直接 403）。
func TestR79_1NonSuperSeesOnlyOwnSessions(t *testing.T) {
	ts := newTestServer(t)
	tokenA := loginAdmin(t, ts)
	putCandidate(t, ts, tokenA, "scope")

	viewer := loginViewer(t, ts)
	if out := cliRun(t, ts, viewer, "ssh", "show system configuration sessions"); !strings.Contains(out, "（无持锁会话）") {
		t.Errorf("非 super-user 不应看到他人的持锁会话，实得：\n%s", out)
	}
	if out := cliRun(t, ts, tokenA, "ssh", "show system configuration sessions"); !strings.Contains(out, "admin@api") {
		t.Errorf("super-user 应看到持锁会话，实得：\n%s", out)
	}
}

// TestR79_1ConcurrentEditSecondRejected 同一用户两个会话同时编辑：持**脏**候选者排他。
//
// 决策 #318 起，干净锁可被同一用户的新会话接管（见 clean_lock_test.go），故这里让 A 先写入
// 一条未提交变更（dirty=true）再断言 B 被拒——这才是锁必须保护的东西。
func TestR79_1ConcurrentEditSecondRejected(t *testing.T) {
	ts := newTestServer(t)
	tokenA, tokenB := loginAdminTwice(t, ts)

	cliRun(t, ts, tokenA, "ssh", "configure")                    // A 取锁
	cliRun(t, ts, tokenA, "ssh", "set system hostname r79-hold") // A 持脏候选
	if res := cliLine(t, ts, tokenB, "ssh", "configure"); !strings.Contains(res.Output, "锁被占用") {
		t.Fatalf("B（同用户另一会话）此时 configure 应被拒，实得：%s", res.Output)
	}
	// A 释放后，B 才能取锁。
	cliRun(t, ts, tokenA, "ssh", "discard")
	cliRun(t, ts, tokenA, "ssh", "exit")
	if res := cliLine(t, ts, tokenB, "ssh", "configure"); strings.Contains(res.Output, "%%") {
		t.Fatalf("A 释放后 B 应能取锁，实得：%s", res.Output)
	}
	cliRun(t, ts, tokenB, "ssh", "exit")
}
