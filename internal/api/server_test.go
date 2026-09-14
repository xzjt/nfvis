package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/events"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/system"
)

// ---------- 测试基础设施 ----------

// newTestServer 真实引擎 + 引擎背书的 AAA（admin/viewer 预置进 committed 配置）。
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newTestServerOpts(t, Options{})
}

// newTestServerOpts 同上，可注入 Options（如 VPP 控制器）。
func newTestServerOpts(t *testing.T, opts Options) *httptest.Server {
	t.Helper()
	store, err := config.OpenStore(filepath.Join(t.TempDir(), "nfvis.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	engineOpts := config.Options{}
	if opts.Events != nil { // M5-1：测试装配 config-committed 事件
		engineOpts.OnCommitted = func(revision int, user string) {
			opts.Events.Publish(events.TypeConfigCommitted, map[string]any{"revision": revision, "user": user})
		}
	}
	engine, err := config.NewEngine(store, orchestrator.NewNoopApplier(), engineOpts)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(engine.Close)
	authz := aaa.NewService(engine, nil)
	if _, _, err := aaa.EnsureBootstrapAdmin(engine, authz, "s3cret-Passw0rd!"); err != nil {
		t.Fatalf("引导 admin: %v", err)
	}
	// 预置 viewer（read-only）
	hash, _ := aaa.HashPassword("s3cret-Passw0rd!")
	sess := config.Session{User: "system", Source: "console"}
	if err := engine.Edit(sess); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	cfg, _ := engine.Committed()
	if cfg.System == nil {
		cfg.System = &model.SystemConfig{}
	}
	if cfg.System.Login == nil {
		cfg.System.Login = &model.SystemLogin{}
	}
	cfg.System.Login.Users = append(cfg.System.Login.Users,
		model.LoginUserConfig{Name: "viewer", PasswordHash: hash, Class: aaa.ClassReadOnly})
	if err := engine.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	if _, err := engine.Commit(context.Background(), sess, config.CommitOpts{Message: "预置 viewer"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	_ = engine.Release(sess)

	opts.Addr = ":0"
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	// M5-6：默认装配备份/恢复管理器（绑定同一引擎，测试可直接打端点）
	if opts.SysOps == nil {
		opts.SysOps = system.NewManager(system.Config{Dir: filepath.Join(t.TempDir(), "backup")}, engine, nil, "test")
	}
	srv := New(engine, authz, opts)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func postJSON(t *testing.T, url string, body any, header map[string]string) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func getWithToken(t *testing.T, url, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func login(t *testing.T, ts *httptest.Server, user, password string) (int, loginResponse) {
	t.Helper()
	status, data := postJSON(t, ts.URL+APIPrefix+"/login", loginRequest{Username: user, Password: password}, nil)
	var resp loginResponse
	_ = json.Unmarshal(data, &resp)
	return status, resp
}

// ---------- 认证流（FR-API-001） ----------

func TestLoginFlow(t *testing.T) {
	ts := newTestServer(t)

	// 成功登录 → token
	status, resp := login(t, ts, "admin", "s3cret-Passw0rd!")
	if status != http.StatusOK || resp.Token == "" {
		t.Fatalf("登录应成功: status=%d resp=%+v", status, resp)
	}
	if resp.User != "admin" || resp.Class != aaa.ClassSuperUser || resp.ExpiresIn <= 0 {
		t.Fatalf("登录响应不符: %+v", resp)
	}

	// 携带 token 访问受保护端点
	status, data := getWithToken(t, ts.URL+APIPrefix+"/system/version", resp.Token)
	if status != http.StatusOK {
		t.Fatalf("version 应可访问: %d %s", status, data)
	}
	var ver map[string]string
	_ = json.Unmarshal(data, &ver)
	if ver["nfvis"] != VersionStr {
		t.Fatalf("版本信息不符: %v", ver)
	}

	// 登出后 token 失效
	status, _ = postJSON(t, ts.URL+APIPrefix+"/logout", nil, map[string]string{"Authorization": "Bearer " + resp.Token})
	if status != http.StatusNoContent {
		t.Fatalf("logout 应 204: %d", status)
	}
	status, _ = getWithToken(t, ts.URL+APIPrefix+"/system/version", resp.Token)
	if status != http.StatusUnauthorized {
		t.Fatalf("吊销后应 401: %d", status)
	}
}

func TestLoginFailuresHTTP(t *testing.T) {
	ts := newTestServer(t)

	// 口令错误 → 401 + 统一错误格式（FR-API-005）
	status, data := postJSON(t, ts.URL+APIPrefix+"/login", loginRequest{Username: "admin", Password: "wrong"}, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("应 401: %d", status)
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(data, &errResp); err != nil || errResp.Code != "UNAUTHORIZED" {
		t.Fatalf("统一错误格式不符: %s", data)
	}

	// 缺参数 → 400
	status, _ = postJSON(t, ts.URL+APIPrefix+"/login", map[string]string{"username": "x"}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("缺 password 应 400: %d", status)
	}

	// 无 token 访问受保护端点 → 401
	status, _ = getWithToken(t, ts.URL+APIPrefix+"/system/version", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401: %d", status)
	}

	// 错误 token → 401
	status, _ = getWithToken(t, ts.URL+APIPrefix+"/system/version", "bogus-token")
	if status != http.StatusUnauthorized {
		t.Fatalf("伪造 token 应 401: %d", status)
	}
}

func TestLockoutHTTP423(t *testing.T) {
	ts := newTestServer(t)
	for i := 0; i < 5; i++ {
		login(t, ts, "admin", "wrong")
	}
	status, data := postJSON(t, ts.URL+APIPrefix+"/login", loginRequest{Username: "admin", Password: "s3cret-Passw0rd!"}, nil)
	if status != http.StatusLocked {
		t.Fatalf("连续失败后应 423: %d %s", status, data)
	}
	if !strings.Contains(string(data), "ACCOUNT_LOCKED") {
		t.Fatalf("错误码应为 ACCOUNT_LOCKED: %s", data)
	}
}

func TestClassAuthorization(t *testing.T) {
	ts := newTestServer(t)
	// viewer（read-only）可访问 show 类端点
	_, vresp := login(t, ts, "viewer", "s3cret-Passw0rd!")
	status, _ := getWithToken(t, ts.URL+APIPrefix+"/system/version", vresp.Token)
	if status != http.StatusOK {
		t.Fatalf("read-only 应可访问 show 端点: %d", status)
	}
	// logout 对所有已登录用户开放
	status, _ = postJSON(t, ts.URL+APIPrefix+"/logout", nil, map[string]string{"Authorization": "Bearer " + vresp.Token})
	if status != http.StatusNoContent {
		t.Fatalf("logout 应 204: %d", status)
	}
}
