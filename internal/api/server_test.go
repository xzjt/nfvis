package api

import (
	"bytes"
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
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// ---------- 测试基础设施 ----------

func testAAA(t *testing.T) *aaa.Service {
	t.Helper()
	hash, err := aaa.HashPassword("s3cret-Passw0rd!")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	cfg := model.Config{
		System: &model.SystemConfig{
			Login: &model.SystemLogin{
				Users: []model.LoginUserConfig{
					{Name: "admin", PasswordHash: hash, Class: aaa.ClassSuperUser},
					{Name: "viewer", PasswordHash: hash, Class: aaa.ClassReadOnly},
				},
			},
		},
	}
	return aaa.NewService(fakeSource{cfg}, nil)
}

type fakeSource struct{ cfg model.Config }

func (f fakeSource) Committed() (model.Config, error) { return f.cfg, nil }

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	store, err := config.OpenStore(filepath.Join(t.TempDir(), "nfvis.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	engine, err := config.NewEngine(store, orchestrator.NewNoopApplier(), config.Options{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(engine.Close)
	srv := New(engine, testAAA(t), Options{Addr: ":0", Log: slog.New(slog.DiscardHandler)})
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
