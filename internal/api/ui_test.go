package api

// 决策 #115：Web 控制面增量 1（只读总览）的内嵌静态资源托管。
//
// 这里钉住四件事：
//  ① 三个资源都能取到、Content-Type 正确、且**不带 Authorization 也能取**
//     （静态资源无鉴权是有意的：登录页必须先能加载）；
//  ② 页面本身**不含敏感信息**——没有 token、没有口令字段的 value、没有内联脚本
//     （内联脚本会被 CSP 挡掉，也会让"无外部依赖"的承诺失真）；
//  ③ 未知路径 404 走统一错误体，不做目录列表、不接受路径穿越；
//  ④ 重定向：`/ui` → `/ui/`（显式注册，不依赖 ServeMux 对子树根的隐式补斜杠）。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUIAssetsServedWithoutAuth(t *testing.T) {
	ts := newTestServerOpts(t, Options{})
	for path, wantCT := range map[string]string{
		APIPrefix + "/ui/":           "text/html; charset=utf-8",
		APIPrefix + "/ui/index.html": "text/html; charset=utf-8",
		APIPrefix + "/ui/app.js":     "text/javascript; charset=utf-8",
		APIPrefix + "/ui/style.css":  "text/css; charset=utf-8",
	} {
		resp, err := http.Get(ts.URL + path) // 故意不带 Authorization
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: 状态 %d", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != wantCT {
			t.Errorf("%s: Content-Type = %q，期望 %q", path, ct, wantCT)
		}
		if len(body) == 0 {
			t.Errorf("%s: 空响应", path)
		}
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", path, got)
		}
		if got := resp.Header.Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'self'") {
			t.Errorf("%s: CSP 未收紧到 self：%q", path, got)
		}
	}
}

// 页面不得把 token/口令这类东西内联进静态资源，也不得含内联脚本
// （内联脚本与 CSP 的 default-src 'self' 冲突，且会让"页面只用同源外部资源"失真）。
func TestUIAssetsHaveNoSecretsOrInlineScript(t *testing.T) {
	ts := newTestServerOpts(t, Options{})
	get := func(p string) string {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	html := get(APIPrefix + "/ui/index.html")
	for _, bad := range []string{"<script>", "onclick=", "Bearer ", "password_hash"} {
		if strings.Contains(html, bad) {
			t.Errorf("index.html 不应含 %q", bad)
		}
	}
	if !strings.Contains(html, `src="app.js"`) || !strings.Contains(html, `href="style.css"`) {
		t.Error("index.html 应通过同源外部资源引入脚本与样式")
	}
	// 前端只能经同源 REST 取数：不得写死外部主机（否则同源托管与"无外部依赖"都不成立）。
	for _, asset := range []string{APIPrefix + "/ui/app.js", APIPrefix + "/ui/style.css"} {
		body := get(asset)
		if strings.Contains(body, "http://") || strings.Contains(body, "https://") {
			t.Errorf("%s 不应含绝对外部地址", asset)
		}
	}
}

func TestUIAssetsUnknownPathAndTraversal(t *testing.T) {
	ts := newTestServerOpts(t, Options{})
	// 白名单在 handler 这一层：**直接**用构造好的 URL.Path 调它。
	// 之所以不经过 HTTP 客户端——客户端会先把 `..` 规范化掉，测出来的只是客户端行为。
	// 非白名单路径一律 404 + 统一错误体（不做目录列表、不落到内嵌 FS 之外）。
	h := ts.Config.Handler
	for _, p := range []string{
		APIPrefix + "/ui/nope.js",
		APIPrefix + "/ui/sub/app.js",
		APIPrefix + "/ui/openapi.json",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "http://nfvis.local/", nil)
		req.URL.Path = p
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: 状态 %d，期望 404（白名单应拦住）", p, rec.Code)
			continue
		}
		var e struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Code != "NOT_FOUND" {
			t.Errorf("%s: 期望统一错误体 NOT_FOUND，得到 %s", p, rec.Body.String())
		}
	}

	// `..` 类路径**到不了 handler**：ServeMux 先把含 `.`/`..` 的非规范路径 307 到规范形式
	// （stdlib 行为，本仓不改）。这里断言的是**结果**：不返回 200，也不吐出任何文件内容。
	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, p := range []string{APIPrefix + "/ui/../openapi.json", APIPrefix + "/ui/../../etc/passwd"} {
		resp, err := cli.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s: 不应返回 200（得到 %s）", p, string(body))
		}
		if strings.Contains(string(body), "root:") {
			t.Fatalf("穿越请求读到了系统文件：%s", string(body))
		}
	}
}

func TestUIRedirectsToTrailingSlash(t *testing.T) {
	ts := newTestServerOpts(t, Options{})
	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cli.Get(ts.URL + APIPrefix + "/ui")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("状态 %d，期望 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != APIPrefix+"/ui/" {
		t.Fatalf("Location = %q，期望 %q", loc, APIPrefix+"/ui/")
	}
}

// 契约守护的另一半：UI 路由必须在契约里声明（routes_contract_test.go 判的是
// 「注册的路由 ⊆ 契约」，这里再正面确认这两条确实被契约收下，免得将来只删契约不删路由）。
func TestUIContractDeclaresUI(t *testing.T) {
	for _, p := range []string{"/ui", "/ui/"} {
		if !contractRoutes(t)["GET "+p] {
			t.Errorf("契约未声明 GET %s", p)
		}
	}
}
