package api

// 决策 #151：一次性事务（取锁 → 写候选 → 立即提交）提交后交还全局编辑锁。
//
// 由来：`Engine.Commit` **不释放**锁（只有 Release/Discard/空闲超时释放），而 REST 的
// login-users 一族与带 `X-NFVIS-Auto-Commit: true` 的直提写都是「提交即结束」的事务
// ⇒ 锁留到登出（决策 #119）或 `lockIdleTTL`，其它会话随后的配置写被 409
// 「candidate 会话锁被占用: 由 X 持有」挡住（round72 首次查出、round76 校验三次复现）。
//
// 每个被修的入口断言两件事：① 本会话不再持锁；② **别的会话**（CLI/ssh 来源）随后能
// 立刻取锁——后者才是本条的目的（跨会话不再被挡）。反向用例钉住"有意持锁"的三类：
// 纯候选写入、提交校验失败（候选脏）、显式收尾路径（CLI configure/commit、
// REST POST /configuration/commit）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// ---------- 断言基础设施 ----------

// lockHolder 读持锁会话表（GET /system/configuration/sessions）的首条 holder，无则 ""。
func lockHolder(t *testing.T, ts *httptest.Server, token string) string {
	t.Helper()
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/configuration/sessions", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("sessions: %d %s", status, body)
	}
	var sessions []map[string]any
	if err := json.Unmarshal(body, &sessions); err != nil {
		t.Fatalf("解析 sessions: %v", err)
	}
	if len(sessions) == 0 {
		return ""
	}
	h, _ := sessions[0]["holder"].(string)
	return h
}

// cliLine 经 CLI 桥执行一行命令（`source` 决定引擎会话来源：api 与 ssh 是两个会话）。
func cliLine(t *testing.T, ts *httptest.Server, token, source, line string) cliExecuteResponse {
	t.Helper()
	status, body := postJSON(t, ts.URL+APIPrefix+"/cli/execute", map[string]any{"line": line, "source": source},
		map[string]string{"Authorization": "Bearer " + token})
	if status != http.StatusOK {
		t.Fatalf("cli/execute %q: %d %s", line, status, body)
	}
	var res cliExecuteResponse
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("解析 cli 响应: %v (%s)", err, body)
	}
	return res
}

// cliRun 同上，命令失败（输出含 %% 判据）即 fatal。
func cliRun(t *testing.T, ts *httptest.Server, token, source, line string) string {
	t.Helper()
	res := cliLine(t, ts, token, source, line)
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("命令 %q 失败: %s", line, res.Output)
	}
	return res.Output
}

// assertOneShotEnded 断言一次性事务收尾到位：① 本会话的锁已交还；② 别的会话立刻能取锁。
// 取锁探针用 CLI/ssh 来源（与 REST 的 api 来源是两个会话，正是 round76 被挡的那一侧）。
func assertOneShotEnded(t *testing.T, ts *httptest.Server, token, what string) {
	t.Helper()
	if h := lockHolder(t, ts, token); h != "" {
		t.Errorf("%s：一次性事务结束后不该还持锁，会话表仍见 holder=%q", what, h)
	}
	out := cliLine(t, ts, token, "ssh", "configure")
	if strings.Contains(out.Output, "%%") {
		t.Errorf("%s：其它会话（CLI/ssh）应能立刻取锁，实得：%s", what, out.Output)
		return
	}
	cliRun(t, ts, token, "ssh", "exit") // 干净候选直接释放（归还探针占用的锁）
	if h := lockHolder(t, ts, token); h != "" {
		t.Errorf("%s：探针会话应已退出，会话表仍见 holder=%q", what, h)
	}
}

// assertLockKept 断言锁**仍在**（反向用例）：会话表里是本会话，且别的会话取不到锁。
// 探针用 CLI/console 来源——与 ssh、api 都是不同会话，且不会与正在配置模式里的 ssh 会话混在一起。
func assertLockKept(t *testing.T, ts *httptest.Server, token, want, what string) {
	t.Helper()
	if h := lockHolder(t, ts, token); h != want {
		t.Errorf("%s：锁应仍由 %s 持有，实得 %q", what, want, h)
	}
	out := cliLine(t, ts, token, "console", "configure")
	if !strings.Contains(out.Output, "锁被占用") {
		t.Errorf("%s：别的会话此时不该能取锁，实得：%s", what, out.Output)
	}
}

// candidateDirty 读本会话候选的 dirty 标记。
func candidateDirty(t *testing.T, ts *httptest.Server, token string) bool {
	t.Helper()
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/candidate", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET candidate: %d %s", status, body)
	}
	var got struct {
		Dirty bool `json:"dirty"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("解析 candidate: %v", err)
	}
	return got.Dirty
}

// ---------- 正面：一次性事务逐入口交还锁 ----------

// login-users 一族（mutateLoginUsers：建/改/删用户、改 class、重置口令、自助改密）。
func TestOneShotLockReleasedLoginUsersFamily(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	steps := []struct {
		what, method, path string
		body               any
		status             int
	}{
		{"建用户", http.MethodPost, "/system/login-users",
			map[string]any{"name": "ops77", "kind": "user", "password": "OneShot-Passw0rd!1", "class": "operator"}, http.StatusCreated},
		{"改 class 与重置口令", http.MethodPut, "/system/login-users/ops77",
			map[string]any{"class": "read-only", "password": "OneShot-Passw0rd!2"}, http.StatusOK},
		{"自助改密", http.MethodPost, "/system/login-users/admin:change-password",
			map[string]any{"old_password": "s3cret-Passw0rd!", "new_password": "OneShot-Passw0rd!3"}, http.StatusOK},
		{"删用户", http.MethodDelete, "/system/login-users/ops77", nil, http.StatusOK},
	}
	for _, s := range steps {
		status, _, body := cfgRequest(t, s.method, ts.URL+APIPrefix+s.path, token, s.body, nil)
		if status != s.status {
			t.Fatalf("%s：状态 %d %s", s.what, status, body)
		}
		assertOneShotEnded(t, ts, token, s.what)
	}
}

// 带 X-NFVIS-Auto-Commit 的直提写：PUT /configuration/candidate 与资源端点
// （mutateCandidate 的直提分支），含"写候选之前就失败"的 409 分支。
func TestOneShotLockReleasedAutoCommitWrites(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	auto := map[string]string{"X-NFVIS-Auto-Commit": "true"}

	// ① PUT /configuration/candidate 单请求直提（决策 #22）
	status, hdr, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, sampleCandidate(), auto)
	if status != http.StatusOK || hdr.Get("X-NFVIS-Committed") != "true" {
		t.Fatalf("直提应 200 且 X-NFVIS-Committed: true：%d %s", status, body)
	}
	assertOneShotEnded(t, ts, token, "PUT candidate 直提")

	// ② 资源写端点直提（POST /vrfs）
	status, _, body = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/vrfs", token, model.Vrf{Name: "vs77"}, auto)
	if status != http.StatusCreated {
		t.Fatalf("建 VRF：%d %s", status, body)
	}
	assertOneShotEnded(t, ts, token, "POST /vrfs 直提")

	// ③ 写候选之前就失败（重名 409）也要交还：候选没动过，留着锁只会挡住别人
	status, _, body = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/vrfs", token, model.Vrf{Name: "vs77"}, auto)
	if status != http.StatusConflict {
		t.Fatalf("重名应 409：%d %s", status, body)
	}
	assertOneShotEnded(t, ts, token, "直提在写候选前失败（409）")
}

// CLI 操作模式下的一次性动作（commitMutate：request interfaces enable|disable 等）。
func TestOneShotLockReleasedCLIOperAction(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// 先声明接口并提交（配置模式内 commit 有意不释放锁，exit 释放）
	cliRun(t, ts, token, "ssh", "configure")
	cliRun(t, ts, token, "ssh", "set interfaces ens2f0 description to-TOR")
	cliRun(t, ts, token, "ssh", "commit")
	cliRun(t, ts, token, "ssh", "exit")
	if h := lockHolder(t, ts, token); h != "" {
		t.Fatalf("exit 后应无持锁会话，实得 %q", h)
	}

	// 操作模式下的一次性动作：提交后交还锁（其它会话不再被挡）
	cliRun(t, ts, token, "ssh", "request interfaces ens2f0 enable")
	assertOneShotEnded(t, ts, token, "CLI request interfaces enable")
}

// ---------- 反向：有意持锁的三类必须原样保留 ----------

// ① 纯候选写入（不带 Auto-Commit 头）：操作者正在编辑，锁必须留着。
func TestCandidateWriteWithoutAutoCommitKeepsLock(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	status, hdr, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, sampleCandidate(), nil)
	if status != http.StatusOK || hdr.Get("X-NFVIS-Committed") != "false" {
		t.Fatalf("纯候选写入应 200 且未提交：%d %s", status, body)
	}
	assertLockKept(t, ts, token, "admin@api", "纯候选写入之后")
	status, _, _ = cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/configuration/candidate", token, nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("discard 应 204：%d", status)
	}
}

// ② 提交因校验失败（候选保留）：操作者要接着改，锁必须留着且候选仍脏。
func TestFailedCommitKeepsLockAndDirtyCandidate(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	bad := sampleCandidate()
	bad.VirtualSwitches = []model.VirtualSwitch{{Name: "vs1", Type: "l2", VlanAccess: 4095}} // vlan_access 越界
	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, bad,
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusBadRequest {
		t.Fatalf("校验失败应 400：%d %s", status, body)
	}
	assertLockKept(t, ts, token, "admin@api", "提交校验失败之后")
	if !candidateDirty(t, ts, token) {
		t.Errorf("提交校验失败后候选应保留且为脏（操作者的改动不能被替它丢掉）")
	}
	status, _, _ = cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/configuration/candidate", token, nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("discard 应 204：%d", status)
	}
}

// ③ 显式收尾路径：CLI 的 configure/commit 与 REST 的 POST /configuration/commit 都不释放
// （客户端自己按决策 #120 的口径收尾：CLI 用 exit/discard，控制台用 DELETE candidate）。
func TestExplicitCommitPathsKeepLock(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// CLI：configure → set → commit 之后仍在配置模式、仍持锁
	cliRun(t, ts, token, "ssh", "configure")
	cliRun(t, ts, token, "ssh", "set system hostname cli-keep")
	if res := cliLine(t, ts, token, "ssh", "commit"); strings.Contains(res.Output, "%%") {
		t.Fatalf("commit 失败：%s", res.Output)
	}
	if res := cliLine(t, ts, token, "ssh", "show"); res.Mode != "config" {
		t.Errorf("commit 后应仍在配置模式：mode=%q", res.Mode)
	}
	assertLockKept(t, ts, token, "admin@ssh", "CLI configure/commit 之后")
	cliRun(t, ts, token, "ssh", "exit")

	// REST：PUT candidate（编辑态）+ POST /configuration/commit 之后锁仍在，直到客户端收尾
	if status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token, sampleCandidate(), nil); status != http.StatusOK {
		t.Fatalf("PUT candidate：%d %s", status, body)
	}
	if status, _, body := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/configuration/commit", token, map[string]any{}, nil); status != http.StatusOK {
		t.Fatalf("commit：%d %s", status, body)
	}
	assertLockKept(t, ts, token, "admin@api", "REST POST /configuration/commit 之后")
	status, _, _ := cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/configuration/candidate", token, nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("discard 应 204：%d", status)
	}
}

// ③b 配置模式内发起一次性动作（`run request …`）：操作者还要接着编辑 ⇒ 不释放。
func TestCLIConfigModeRunRequestKeepsLock(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	cliRun(t, ts, token, "ssh", "configure")
	cliRun(t, ts, token, "ssh", "set interfaces ens2f0 description to-TOR")
	cliRun(t, ts, token, "ssh", "commit")

	res := cliLine(t, ts, token, "ssh", "run request interfaces ens2f0 enable")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("run request 失败：%s", res.Output)
	}
	if res.Mode != "config" {
		t.Errorf("run 之后应仍在配置模式：mode=%q", res.Mode)
	}
	assertLockKept(t, ts, token, "admin@ssh", "配置模式内 run request 之后")

	cliRun(t, ts, token, "ssh", "discard")
	if h := lockHolder(t, ts, token); h != "" {
		t.Errorf("discard 后应无持锁会话，实得 %q", h)
	}
}
