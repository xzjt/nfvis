package api

// FR-SEC-007 / 决策 #25：口令哈希不得回显于任何 show/API 输出。
//
// 背景（V1 收尾复核发现的安全缺陷）：commit 的审计详情是完整 diff，而
// GET /audit-logs 只需 ClassReadOnly，导致 operator/read-only 账号可取得
// 他人 PBKDF2 哈希原文；配置视图（candidate/rollback 回显）同样含哈希。
// 本测试为「全路径脱敏」守护：任何新增的泄露路径都会在此失败。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"context"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
)

const leakedHashMarker = "pbkdf2$"

// seedUserWithPassword 经 API 建一个带口令的用户（走真实 commit + 审计链路）。
func seedUserWithPassword(t *testing.T, ts *httptest.Server, token, name string) {
	t.Helper()
	body := map[string]any{"name": name, "password": "Secret@98765", "class": "operator"}
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/login-users", token,
		body, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("建用户 %s: %d %s", name, status, data)
	}
}

// 全路径守护：审计日志、配置视图、diff、CLI show 均不得出现口令哈希。
func TestSecretsNeverEchoed(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	seedUserWithPassword(t, ts, token, "opssec")

	// 1) GET /audit-logs（缺陷原发路径；ClassReadOnly 即可读）
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/audit-logs?limit=50", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("audit-logs: %d %s", status, data)
	}
	if strings.Contains(string(data), leakedHashMarker) {
		t.Fatalf("审计日志泄露口令哈希:\n%s", data)
	}
	if !strings.Contains(string(data), "password-hash") {
		t.Fatalf("审计仍应记录「口令变更」这一事实（值脱敏）:\n%s", data)
	}

	// 2) GET /configuration/candidate（含 login 段时不得回显哈希）
	if status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		candidateWithUser(), nil); status != http.StatusOK {
		t.Fatalf("PUT candidate: %d", status)
	}
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/candidate", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET candidate: %d %s", status, data)
	}
	if strings.Contains(string(data), leakedHashMarker) || strings.Contains(string(data), "password_hash") {
		t.Fatalf("candidate 视图泄露口令哈希:\n%s", data)
	}

	// 3) GET /configuration/diff（JunOS 风格文本）
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/diff", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("diff: %d %s", status, data)
	}
	if strings.Contains(string(data), leakedHashMarker) {
		t.Fatalf("diff 文本泄露口令哈希:\n%s", data)
	}

	// 4) GET /system/login-users（列表端点）
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/login-users", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("login-users: %d %s", status, data)
	}
	if strings.Contains(string(data), leakedHashMarker) {
		t.Fatalf("login-users 泄露口令哈希:\n%s", data)
	}

	// 5) CLI show configuration / show log audit 直连路径：经事务引擎写入真实哈希
	x, eng := newCLIKit(t)
	seedCLIUserWithHash(t, x, eng)
	for _, line := range []string{"show configuration", "show log audit last 20"} {
		out := x.Execute("admin", aaa.ClassSuperUser, "ssh", line).Output
		if strings.Contains(out, leakedHashMarker) {
			t.Fatalf("CLI %q 泄露口令哈希:\n%s", line, out)
		}
	}
}

// candidateWithUser 构造含 login 用户（带哈希）的候选配置，用于验证视图脱敏。
func candidateWithUser() map[string]any {
	return map[string]any{
		"system": map[string]any{
			"hostname": "sec-node",
			"login": map[string]any{
				"users": []map[string]any{
					{"name": "opssec", "class": "operator",
						"password_hash": "pbkdf2$sha256$600000$VIEWSALT$VIEWHASH"},
				},
			},
		},
	}
}

// seedCLIUserWithHash 经事务引擎写入一个带真实哈希的用户，触发 CLI 侧审计与 show 路径
// （等价于 aaa 服务写入；不依赖口令交互）。
func seedCLIUserWithHash(t *testing.T, x *cliExecutor, eng *config.Engine) {
	t.Helper()
	sess := config.Session{User: "admin", Source: "ssh"}
	if err := eng.Edit(sess); err != nil {
		t.Fatalf("edit: %v", err)
	}
	cfg, _, err := eng.Candidate()
	if err != nil {
		t.Fatalf("candidate: %v", err)
	}
	if cfg.System == nil {
		cfg.System = &model.SystemConfig{}
	}
	if cfg.System.Login == nil {
		cfg.System.Login = &model.SystemLogin{}
	}
	cfg.System.Login.Users = append(cfg.System.Login.Users, model.LoginUserConfig{
		Name: "cliuser", Class: "operator",
		PasswordHash: "pbkdf2$sha256$600000$CLISALT$CLIHASH",
	})
	if err := eng.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("update candidate: %v", err)
	}
	if _, err := eng.Commit(context.Background(), sess, config.CommitOpts{Message: "seed"}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := eng.Release(sess); err != nil {
		t.Fatalf("release: %v", err)
	}
	_ = x
}

// 脱敏助手本身的直接单测：敏感键整个移除，非敏感键保留。
func TestRedactConfigView(t *testing.T) {
	b, _ := json.Marshal(candidateWithUser())
	var tree any
	_ = json.Unmarshal(b, &tree)
	redactTree(tree)
	out, _ := json.Marshal(tree)
	s := string(out)
	if strings.Contains(s, "pbkdf2") || strings.Contains(s, "password_hash") {
		t.Fatalf("敏感字段未移除: %s", s)
	}
	for _, want := range []string{"opssec", "operator", "sec-node"} {
		if !strings.Contains(s, want) {
			t.Fatalf("非敏感字段 %q 不应被移除: %s", want, s)
		}
	}
}

// FR-API-007（决策 #70）：/audit-logs 的 offset 必须真正生效
// （契约早已声明 limit/offset 而实现静默忽略，属契约漂移）。
func TestAuditLogsOffsetEffective(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	// 产生两条审计（两次 commit）
	seedUserWithPassword(t, ts, token, "pg1")
	seedUserWithPassword(t, ts, token, "pg2")

	get := func(q string) []map[string]any {
		t.Helper()
		status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/audit-logs"+q, token, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("audit-logs%s: %d %s", q, status, data)
		}
		var rows []map[string]any
		if err := json.Unmarshal(data, &rows); err != nil {
			t.Fatalf("解析: %v", err)
		}
		return rows
	}
	page0 := get("?limit=1&offset=0")
	page1 := get("?limit=1&offset=1")
	if len(page0) != 1 || len(page1) != 1 {
		t.Fatalf("分页大小错误: %d %d", len(page0), len(page1))
	}
	if page0[0]["timestamp"] == page1[0]["timestamp"] {
		t.Fatalf("offset 未生效：两页返回同一条（%v）", page0[0])
	}
}
