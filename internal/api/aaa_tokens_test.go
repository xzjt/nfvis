package api

// 决策 #301：活动会话 / API Token 两个端点的 handler 测试。
//
// 覆盖：登录响应的 token_id 是稳定 ID 且与清单一致；清单范围（super-user 全部 /
// 其他 class 仅自己的）；吊销权限边界与「不存在/无权」同一 404 文案（不泄露存在性）；
// 吊销后该 token 的下一个请求即 401。范围判定的权威在 aaa.Service（有自己的单测），
// 这里核「端点把同一份事实正确地发出去」。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// apiTokenEntry GET /system/api-tokens 响应元素（与契约 ApiToken 对齐的解码形态）。
type apiTokenEntry struct {
	TokenID   string `json:"token_id"`
	User      string `json:"user"`
	Class     string `json:"class"`
	IssuedAt  string `json:"issued_at"`
	ExpiresAt string `json:"expires_at"`
	Current   bool   `json:"current"`
}

func listAPITokens(t *testing.T, ts *httptest.Server, token string) (int, []apiTokenEntry) {
	t.Helper()
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/api-tokens", token, nil, nil)
	var out struct {
		Tokens []apiTokenEntry `json:"tokens"`
	}
	if status == http.StatusOK {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("响应不是契约形状: %v %s", err, body)
		}
	}
	return status, out.Tokens
}

func revokeAPIToken(t *testing.T, ts *httptest.Server, token, id string) (int, []byte) {
	t.Helper()
	status, _, body := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/api-tokens/"+id+":revoke", token, nil, nil)
	return status, body
}

func TestAPITokenLoginCarriesStableID(t *testing.T) {
	ts := newTestServer(t)
	status, resp := login(t, ts, "admin", "s3cret-Passw0rd!")
	if status != http.StatusOK || resp.Token == "" {
		t.Fatalf("登录: %d %s", status, resp.Token)
	}
	if resp.TokenID == "" || strings.ContainsAny(resp.TokenID, "/+") {
		t.Fatalf("token_id 应为稳定 UUID 形态: %q", resp.TokenID)
	}
	if resp.TokenID == resp.Token[:8] {
		t.Fatalf("token_id 不应是 token 本体的前缀（旧口径）: %q", resp.TokenID)
	}
	// 与清单一致（admin 为 super-user，能看见自己）
	code, tokens := listAPITokens(t, ts, resp.Token)
	if code != http.StatusOK {
		t.Fatalf("GET /system/api-tokens: %d", code)
	}
	found := false
	for _, e := range tokens {
		if e.TokenID == resp.TokenID {
			found = true
			if !e.Current {
				t.Fatalf("本人查询时该会话应标 current=true: %+v", e)
			}
			if e.User != "admin" || e.Class != "super-user" || e.IssuedAt == "" || e.ExpiresAt == "" {
				t.Fatalf("清单条目字段不符: %+v", e)
			}
		}
	}
	if !found {
		t.Fatalf("清单应含刚登录会话的 token_id %q: %+v", resp.TokenID, tokens)
	}
}

func TestAPITokensListScope(t *testing.T) {
	ts := newTestServer(t)
	adminTok := loginAdmin(t, ts)
	viewerTok := loginViewer(t, ts)

	// viewer（read-only）：只见自己的会话，看不见 admin 的
	code, vt := listAPITokens(t, ts, viewerTok)
	if code != http.StatusOK {
		t.Fatalf("viewer 清单: %d", code)
	}
	if len(vt) != 1 {
		t.Fatalf("viewer 应只见自己 1 个会话，实得 %d: %+v", len(vt), vt)
	}
	if vt[0].User != "viewer" {
		t.Fatalf("viewer 清单混入了他人会话: %+v", vt)
	}
	// admin（super-user）：两个会话都可见
	_, at := listAPITokens(t, ts, adminTok)
	users := map[string]bool{}
	for _, e := range at {
		users[e.User] = true
	}
	if !users["admin"] || !users["viewer"] {
		t.Fatalf("super-user 应见全部用户会话: %+v", at)
	}
}

func TestAPITokenRevokeScopeAndEffect(t *testing.T) {
	ts := newTestServer(t)
	adminTok := loginAdmin(t, ts)
	viewerTok := loginViewer(t, ts)

	// viewer 吊销 admin 的会话：404「不存在或无权」（与不存在的 id 同一文案，不泄露存在性）
	_, at := listAPITokens(t, ts, adminTok)
	var adminID string
	for _, e := range at {
		if e.User == "admin" {
			adminID = e.TokenID
		}
	}
	if adminID == "" {
		t.Fatalf("取不到 admin 的 token_id: %+v", at)
	}
	code, body := revokeAPIToken(t, ts, viewerTok, adminID)
	if code != http.StatusNotFound || !strings.Contains(string(body), "会话不存在或无权操作") {
		t.Fatalf("viewer 吊销他人会话应 404 同一文案: %d %s", code, body)
	}
	code, body = revokeAPIToken(t, ts, viewerTok, "no-such-id")
	if code != http.StatusNotFound || !strings.Contains(string(body), "会话不存在或无权操作") {
		t.Fatalf("吊销不存在的 id 应同一 404 文案: %d %s", code, body)
	}

	// viewer 吊销自己的：204，随后该 token 立即 401
	code, _ = revokeAPIToken(t, ts, viewerTok, mustOwnTokenID(t, ts, viewerTok))
	if code != http.StatusNoContent {
		t.Fatalf("吊销自己的会话应 204: %d", code)
	}
	status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/status", viewerTok, nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("吊销后下一请求应 401: %d", status)
	}

	// admin（super-user）吊销他人（重新登录 viewer）：204 并立即失效
	viewerTok2 := loginViewer(t, ts)
	viewerID2 := mustOwnTokenID(t, ts, viewerTok2)
	code, _ = revokeAPIToken(t, ts, adminTok, viewerID2)
	if code != http.StatusNoContent {
		t.Fatalf("super-user 吊销他人会话应 204: %d", code)
	}
	status, _, _ = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/status", viewerTok2, nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("被 super 吊销后下一请求应 401: %d", status)
	}
	// admin 自己不受影响
	status, _, _ = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/status", adminTok, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("admin 自身会话应仍有效: %d", status)
	}
	// 重复吊销同一 id：同一 404
	code, body = revokeAPIToken(t, ts, adminTok, viewerID2)
	if code != http.StatusNotFound || !strings.Contains(string(body), "会话不存在或无权操作") {
		t.Fatalf("重复吊销应同一 404 文案: %d %s", code, body)
	}
}

// mustOwnTokenID 取 token 自己的 token_id（viewer 只能看见自己，清单即自己的会话）。
func mustOwnTokenID(t *testing.T, ts *httptest.Server, token string) string {
	t.Helper()
	code, tokens := listAPITokens(t, ts, token)
	if code != http.StatusOK || len(tokens) != 1 {
		t.Fatalf("清单应恰含自己的 1 个会话: %d %+v", code, tokens)
	}
	return tokens[0].TokenID
}

// TestAPITokenRevokeMalformedForm 非法动作形态（无 :revoke 后缀 / 未知动作）按不存在处理。
func TestAPITokenRevokeMalformedForm(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	for _, path := range []string{
		ts.URL + APIPrefix + "/system/api-tokens/whatever",  // 缺 :revoke
		ts.URL + APIPrefix + "/system/api-tokens/x:restart", // 未知动作
		ts.URL + APIPrefix + "/system/api-tokens/:revoke",   // 空 id
	} {
		status, _, body := cfgRequest(t, http.MethodPost, path, token, nil, nil)
		if status != http.StatusNotFound {
			t.Fatalf("%s 应 404: %d %s", path, status, body)
		}
	}
}

// ---------- CLI 执行器侧（同一实现，不经 HTTP handler） ----------

// newCLITokenKit 构造带 AAA 引用的执行器套件：返回（执行器、AAA 服务、引擎）。
// 与 newCLIKit 同一装配，只是把 aaa.Service 一并交回——拿到可登录的会话。
func newCLITokenKit(t *testing.T) (*cliExecutor, *aaa.Service, *config.Engine) {
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
	authz := aaa.NewService(engine, nil)
	if _, _, err := aaa.EnsureBootstrapAdmin(engine, authz, "TestPassw0rd!"); err != nil {
		t.Fatalf("引导 admin: %v", err)
	}
	return newCLIExecutor(engine, authz), authz, engine
}

func TestCLIShowSystemAPITokens(t *testing.T) {
	x, authz, _ := newCLITokenKit(t)
	tok, err := authz.Login("admin", "TestPassw0rd!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// 经 ExecuteAs（与真实 CLI 通道同形）：当前会话要被标记出来
	res := x.ExecuteAs("admin", aaa.ClassSuperUser, "ssh", tok.ID, "show system api tokens")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("show 不应失败: %q", res.Output)
	}
	for _, want := range []string{"Token-ID", "admin", "super-user", "当前会话"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("输出缺 %q: %q", want, res.Output)
		}
	}
	if !strings.Contains(res.Output, tok.ID) {
		t.Errorf("输出应含稳定 ID %q: %q", tok.ID, res.Output)
	}

	// 无稳定 ID 的调用形态（内部便利路径）：标记列如实为 -，不猜
	res2 := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system api tokens")
	if strings.Contains(res2.Output, "当前会话") {
		t.Errorf("无稳定 ID 时不得标当前会话: %q", res2.Output)
	}

	// 非法子形态必须报错，不得静默回落
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system api").Output; !strings.Contains(out, "%%") {
		t.Errorf("show system api（缺 tokens）应报错: %q", out)
	}
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system api tokens extra").Output; !strings.Contains(out, "%%") {
		t.Errorf("多余 token 应报错: %q", out)
	}

	// operator 只见自己的：这里 admin 没有别的会话可看，重点是看得到自己且不报权限错
	tokOp, err := authz.Login("admin", "TestPassw0rd!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	_ = tokOp
	if out := x.ExecuteAs("admin", aaa.ClassSuperUser, "ssh", tok.ID, "show system api tokens").Output; strings.Contains(out, "%%") {
		t.Errorf("super-user 清单不应失败: %q", out)
	}
}

func TestCLIRevokeTokenScopeAndAudit(t *testing.T) {
	x, authz, engine := newCLITokenKit(t)
	tok, err := authz.Login("admin", "TestPassw0rd!")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// 吊销不存在的 id：%% + 明确文案，且留失败审计
	res := x.ExecuteAs("admin", aaa.ClassSuperUser, "ssh", tok.ID, "request system api token revoke no-such-id")
	out := res.Output
	if !strings.Contains(out, "%%") || !strings.Contains(out, "会话不存在或无权操作") {
		t.Fatalf("吊销不存在应 %% 文案: %q", out)
	}
	trail, err := engine.AuditTrail(10, 0)
	if err != nil {
		t.Fatalf("AuditTrail: %v", err)
	}
	if len(trail) == 0 || trail[0].Action != "system.api-token.revoke" || trail[0].Result != "failure" {
		t.Fatalf("失败吊销应留审计: %+v", trail)
	}

	// 吊销自己的会话：成功文案 + 成功审计 + token 立即失效
	out = x.ExecuteAs("admin", aaa.ClassSuperUser, "ssh", tok.ID, "request system api token revoke "+tok.ID).Output
	if strings.Contains(out, "%%") || !strings.Contains(out, "已吊销当前会话") {
		t.Fatalf("吊销自己应成功并说明: %q", out)
	}
	if _, err := authz.VerifyToken(tok.Token); err == nil {
		t.Fatalf("吊销后 token 应失效")
	}
	trail, err = engine.AuditTrail(10, 0)
	if err != nil {
		t.Fatalf("AuditTrail: %v", err)
	}
	if trail[0].Action != "system.api-token.revoke" || trail[0].Result != "success" {
		t.Fatalf("成功吊销应留审计: %+v", trail[0])
	}

	// 缺 token-id 参数：语法提示
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system api token revoke").Output; !strings.Contains(out, "%%") || !strings.Contains(out, "token-id") {
		t.Fatalf("缺参数应给语法提示: %q", out)
	}
}
