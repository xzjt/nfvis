package api

// 增量 2 的 API 侧收口（决策 #119）：
//   ① `GET /configuration`——committed 全量读取（round42 覆盖核查缺口 #1）；
//   ② `GET /configuration/candidate` 契约按实现形状声明（{candidate, dirty}）；
//   ③ 已知缺陷 #19——`POST /logout` 释放 candidate 锁（与 CLI Teardown 对齐）。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// committed 读取：返回整份配置与版本号；契约声明的两个字段都必须真的发得出来。
func TestGetConfigurationReturnsCommitted(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// 先提交一次，使 committed 有内容与版本号可核（接口写入经 candidate + 自动提交）。
	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/interfaces/ens2f0", token,
		map[string]any{"name": "ens2f0", "description": "to-TOR"}, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("写入接口配置: %d %s", status, body)
	}

	status, _, body = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /configuration: %d %s", status, body)
	}
	var got struct {
		Configuration map[string]any `json:"configuration"`
		Revision      int            `json:"revision"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if got.Configuration == nil {
		t.Fatalf("configuration 缺席：%s", body)
	}
	if got.Revision <= 0 {
		t.Fatalf("revision 应为正数：%s", body)
	}
	ifaces, _ := got.Configuration["interfaces"].([]any)
	if len(ifaces) == 0 {
		t.Fatalf("committed 配置里应有刚写入的接口：%s", body)
	}
	// 与 CLI `show configuration` 同源：都取 committed。
	first, _ := ifaces[0].(map[string]any)
	if first["name"] != "ens2f0" {
		t.Fatalf("接口名不符：%v", first)
	}
}

// 未认证不得读整配置（与其它数据端点一致）。
func TestGetConfigurationRequiresAuth(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + APIPrefix + "/configuration")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401，得到 %d", resp.StatusCode)
	}
}

// candidate 读取：契约按实现形状声明 {candidate, dirty}——两个字段都必须存在。
func TestGetCandidateShapeMatchesContract(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// 未进入配置模式时读取应报错（不得谎报空 candidate）。
	status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/candidate", token, nil, nil)
	if status == http.StatusOK {
		t.Fatalf("未持锁时读 candidate 不应 200")
	}

	// 进入配置模式（PUT candidate）后读取：candidate + dirty 都在。
	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		map[string]any{"system": map[string]any{"hostname": "cand-node"}}, nil)
	if status != http.StatusOK {
		t.Fatalf("PUT candidate: %d %s", status, body)
	}
	status, _, body = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/candidate", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET candidate: %d %s", status, body)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("解析: %v", err)
	}
	cand, ok := got["candidate"].(map[string]any)
	if !ok {
		t.Fatalf("candidate 应为对象：%s", body)
	}
	if _, ok := got["dirty"]; !ok {
		t.Fatalf("dirty 缺席（契约 CandidateView 已声明）：%s", body)
	}
	sys, _ := cand["system"].(map[string]any)
	if sys["hostname"] != "cand-node" {
		t.Fatalf("candidate 内容不符：%v", cand)
	}
}

// R44-1 端到端（round44 可视验收发现的缺陷）：控制台的配置页流程是
// GET /configuration（脱敏）→ 改字段 → PUT candidate（整文档回写）→ 提交。
// 脱敏视图里没有口令哈希，回写若把它抹掉，提交后该账号就登不进来——
// 本用例把这条链路整跑一遍，最后**用原口令重新登录**作为判据。
func TestRedactedViewRoundTripKeepsPassword(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// 1) 读脱敏视图（与浏览器同源同路径）
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /configuration: %d %s", status, body)
	}
	var view struct {
		Configuration map[string]any `json:"configuration"`
		Revision      int            `json:"revision"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if raw := string(body); strings.Contains(raw, "password_hash") {
		t.Fatalf("配置视图不应含口令哈希（决策 #25）：%s", raw)
	}

	// 2) 改一个普通字段后整文档回写（模拟页面：以视图为底稿）
	if sys, ok := view.Configuration["system"].(map[string]any); ok {
		sys["hostname"] = "roundtrip-node"
	} else {
		view.Configuration["system"] = map[string]any{"hostname": "roundtrip-node"}
	}
	status, _, body = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		view.Configuration, nil)
	if status != http.StatusOK {
		t.Fatalf("PUT candidate: %d %s", status, body)
	}

	// 3) 差异里不该出现口令相关变更（脱敏回写不得被当成"改了口令"）
	status, _, body = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/diff", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("diff: %d %s", status, body)
	}
	if strings.Contains(string(body), "password") {
		t.Fatalf("差异里不应有口令变更（脱敏回写把哈希抹了？）：%s", body)
	}

	// 4) 提交，并确认原口令仍能登录（哈希未被抹掉）
	status, _, body = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/configuration/commit", token,
		map[string]any{}, nil)
	if status != http.StatusOK {
		t.Fatalf("commit: %d %s", status, body)
	}
	status, resp := login(t, ts, "admin", "s3cret-Passw0rd!")
	if status != http.StatusOK || resp.Token == "" {
		t.Fatalf("提交后原口令应仍可登录（哈希被抹掉会锁死账号）：%d", status)
	}
}

// 没有口令的用户不得提交（R44-1 兜底：新用户无从继承哈希，必须显式设口令）。
func TestCommitRejectsUserWithoutPassword(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		map[string]any{"system": map[string]any{"login": map[string]any{
			"users": []map[string]any{{"name": "nopass", "class": "super-user"}},
		}}}, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status == http.StatusOK {
		t.Fatalf("无口令用户不应提交成功：%s", body)
	}
	if !strings.Contains(string(body), "没有口令") {
		t.Fatalf("报错应点明缺口令：%s", body)
	}
}

// 管理口变更经 REST 也须 commit confirmed（FR-CFG-012，决策 #121 扩围）——
// 这正是控制台配置页提交管理口改动时会走的路径：被拒 → 以 confirmed 提交 → 确认。
func TestManagementChangeRequiresConfirmedViaREST(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		map[string]any{"system": map[string]any{"management": map[string]any{"address": "192.0.2.10/24"}}}, nil)
	if status != http.StatusOK {
		t.Fatalf("PUT candidate: %d %s", status, body)
	}
	// 普通提交应被拒，且错误码可辨
	status, _, body = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/configuration/commit", token,
		map[string]any{}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("管理口变更的普通提交应 400，得到 %d %s", status, body)
	}
	if !strings.Contains(string(body), "CONFIRM_REQUIRED") {
		t.Fatalf("应返回 CONFIRM_REQUIRED：%s", body)
	}
	// 以 commit confirmed 提交 → 200 + 待确认截止时间
	status, _, body = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/configuration/commit", token,
		map[string]any{"confirmed_minutes": 10}, nil)
	if status != http.StatusOK {
		t.Fatalf("commit confirmed: %d %s", status, body)
	}
	if !strings.Contains(string(body), "confirmed_until") {
		t.Fatalf("应返回 confirmed_until：%s", body)
	}
	// 确认 → 生效
	status, _, body = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/configuration/commit:confirm", token, nil, nil)
	if status != http.StatusOK && status != http.StatusNoContent {
		t.Fatalf("confirm: %d %s", status, body)
	}
}

func TestLogoutReleasesCandidateLock(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// API 会话取得 candidate 锁。
	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		map[string]any{"system": map[string]any{"hostname": "api-node"}}, nil)
	if status != http.StatusOK {
		t.Fatalf("PUT candidate: %d %s", status, body)
	}
	holderOf := func() string {
		t.Helper()
		st, _, b := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/configuration/sessions", token, nil, nil)
		if st != http.StatusOK {
			t.Fatalf("sessions: %d %s", st, b)
		}
		var sessions []map[string]any
		if err := json.Unmarshal(b, &sessions); err != nil {
			t.Fatalf("解析 sessions: %v", err)
		}
		if len(sessions) == 0 {
			return ""
		}
		h, _ := sessions[0]["holder"].(string)
		return h
	}
	if h := holderOf(); h != "admin@api" {
		t.Fatalf("持锁者应为 admin@api，得到 %q", h)
	}

	// 登出：锁随之释放（决策 #119 / 缺陷 #19）。
	status, _ = postJSON(t, ts.URL+APIPrefix+"/logout", nil, map[string]string{"Authorization": "Bearer " + token})
	if status != http.StatusNoContent {
		t.Fatalf("logout 应 204: %d", status)
	}
	// 换一个新 token 查会话表（旧 token 已吊销）：锁不该还在。
	token2 := loginAdmin(t, ts)
	status, _, body = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/configuration/sessions", token2, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("sessions: %d %s", status, body)
	}
	var sessions []map[string]any
	if err := json.Unmarshal(body, &sessions); err != nil {
		t.Fatalf("解析 sessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("登出后 candidate 锁应已释放，仍见 %v", sessions)
	}
}
