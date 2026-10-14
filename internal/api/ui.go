package api

// 决策 #115：V2 Web 控制面增量 1（只读总览）。
//
// 免构建前端（原生 HTML/CSS/JS，无 npm/node）随二进制嵌入，**同源**托管于
// `GET /api/v1/ui/`。同源带来两个直接好处：不需要 CORS（浏览器不会把它当跨域请求）、
// 不需要额外的部署件（deb 里仍然只有 nfvisd 与 nfvis-cli）。
//
// 静态资源**无鉴权**（与 /metrics、/openapi.json 同例）：它们不含任何敏感信息，
// 且登录页必须先能加载才能去换 token；数据一律由页面经既有 REST 端点 +
// `Authorization: Bearer` 读取——本文件不碰任何业务数据，也不新增数据端点。
//
// 路由放在 `/api/v1` 下（而不是根路径）是**有意的**：`routes_contract_test.go` 按
// `mux.Handle("METHOD "+APIPrefix+"…")` 的形式解析路由并与契约比对，放在前缀下才落在这道
// 守护之内（放根路径会静默落在守护之外）。

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed ui
var uiAssets embed.FS

// uiFiles 静态资源白名单：路径 → Content-Type。
// 只发这张表里的文件——既不做目录列表（内嵌 FS 里本就没有目录语义），也不接受任意路径，
// 路径穿越（`../`）因此在查表这一步就被拒了，无需额外过滤。
var uiFiles = map[string]string{
	"index.html":  "text/html; charset=utf-8",
	"app.js":      "text/javascript; charset=utf-8",
	"vncview.js":  "text/javascript; charset=utf-8",
	"style.css":   "text/css; charset=utf-8",
	"router.js":   "text/javascript; charset=utf-8",
	"routes.json": "application/json; charset=utf-8",
}

// uiExtTypes novnc/ 子树（决策 #485：Web 内置 noVNC 查看器）按扩展名的 MIME 表。
// noVNC 是一组 ES 模块（core/*.js 相互相对 import、vendor/pako/*.js），逐个进白名单会
// 变成一份随上游版本漂移的清单——故 novnc/ 子树改为「前缀 + 扩展名查表」放行。
var uiExtTypes = map[string]string{
	".js":   "text/javascript; charset=utf-8",
	".mjs":  "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".json": "application/json; charset=utf-8",
	".wasm": "application/wasm",
	".png":  "image/png",
	".svg":  "image/svg+xml",
	".map":  "application/json; charset=utf-8",
	".txt":  "text/plain; charset=utf-8",
}

// uiContentType 解析静态资源的内容类型（命中即放行）。
// 白名单文件用固定表；novnc/ 子树按扩展名查表。其余一律「未命中」→ 404。
func uiContentType(name string) (string, bool) {
	if ct, ok := uiFiles[name]; ok {
		return ct, true
	}
	if !strings.HasPrefix(name, "novnc/") {
		return "", false
	}
	// 防路径穿越：内嵌 FS 只含 ui/ 下的文件，但仍显式拒绝 `..`/空段，避免把 ../ 交给 ReadFile。
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	ct, ok := uiExtTypes[strings.ToLower(path.Ext(name))]
	return ct, ok
}

// handleUIRedirect GET /api/v1/ui：跳到 `/ui/`。
// 显式注册（而不是靠 ServeMux 对子树根自动补斜杠）：行为要能被契约与测试固定下来。
func (s *Server) handleUIRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, APIPrefix+"/ui/", http.StatusFound)
}

// handleUIAssets GET /api/v1/ui/…：返回内嵌静态资源；空路径回落到 index.html。
func (s *Server) handleUIAssets(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, APIPrefix+"/ui/")
	if name == "" {
		name = "index.html"
	}
	ct, ok := uiContentType(name)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "静态资源不存在", nil)
		return
	}
	data, err := fs.ReadFile(uiAssets, "ui/"+name)
	if err != nil {
		// novnc/ 子树按扩展名放行，但并非每个扩展名下的路径都存在——不存在就当 404（而非 500）。
		writeError(w, http.StatusNotFound, "NOT_FOUND", "静态资源不存在", nil)
		return
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	// 前端随二进制发布：升级后必须立刻用上新脚本，故不缓存（禁掉启发式缓存即可，无需 no-store）。
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	// 页面只用同源资源（外部 app.js/style.css，无内联脚本与样式），故 CSP 可以收紧到 self。
	h.Set("Content-Security-Policy",
		"default-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
