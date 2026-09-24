package api

// 决策 #152 端到端：整文档提交不能把本机提交成「无人可登录」（自锁），且**被拒之后
// 本机仍能用 admin 登录**——判据不看"报错好不好看"，而看账号与登录路径是否真的还在
// （独立事实源：口令走完整认证路径，账号读 committed 而非候选）。
//
// 这条缺口是 round77 复验捅出来的：`DELETE /system/login-users/{name}` 有「不能删最后一个
// super-user」守卫，而 `PUT /configuration/candidate`（整文档替换）+ commit 一次就能清空账号表。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestCommitWithoutSuperUserRejectedAndLoginSurvives(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	// 写空配置候选（整文档替换）→ 提交 → 400
	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		map[string]any{}, nil)
	if status != http.StatusOK {
		t.Fatalf("写空候选应 200: %d %s", status, body)
	}
	status, _, body = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/configuration/commit", token,
		map[string]any{"message": "清空账号"}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("清空账号表的提交应 400: %d %s", status, body)
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(body, &errResp); err != nil || errResp.Code != "VALIDATION_FAILED" {
		t.Fatalf("错误码应为 VALIDATION_FAILED: %v %s", err, body)
	}
	if !strings.Contains(string(body), "super-user") {
		t.Fatalf("报错应点明要留一个 super-user: %s", body)
	}

	// ① 账号还在（读 committed）：GET /system/login-users 仍列出 admin
	status, _, body = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/login-users", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"admin"`) {
		t.Fatalf("被拒后 committed 里应仍有 admin: %d %s", status, body)
	}
	// ② 本机仍能用 admin 登录（独立事实源：走完整认证路径，不看接口自述）
	status, resp := login(t, ts, "admin", "s3cret-Passw0rd!")
	if status != http.StatusOK || resp.Token == "" {
		t.Fatalf("被拒后 admin 应仍能登录（否则等于自锁）: %d %+v", status, resp)
	}

	// ③ 候选保留（操作者改完可以直接重提）：候选仍是空配置且标记 dirty
	status, _, body = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/candidate", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"dirty":true`) {
		t.Fatalf("被拒后候选应保留（编辑态、dirty）: %d %s", status, body)
	}

	// ④ 补回 super-user 后同一候选能提交（不是"卡死"，是"改完再提"）
	status, _, body = cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		map[string]any{"system": map[string]any{
			"hostname": "rescued",
			"login":    map[string]any{"users": []map[string]any{superUserDoc()}},
		}}, nil)
	if status != http.StatusOK {
		t.Fatalf("补回 super-user 应 200: %d %s", status, body)
	}
	status, _, body = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/configuration/commit", token,
		map[string]any{}, nil)
	if status != http.StatusOK {
		t.Fatalf("补回 super-user 后提交应 200: %d %s", status, body)
	}
}
