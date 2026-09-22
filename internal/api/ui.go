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
	"strings"
)

//go:embed ui
var uiAssets embed.FS

// uiFiles 静态资源白名单：路径 → Content-Type。
// 只发这张表里的文件——既不做目录列表（内嵌 FS 里本就没有目录语义），也不接受任意路径，
// 路径穿越（`../`）因此在查表这一步就被拒了，无需额外过滤。
var uiFiles = map[string]string{
	"index.html": "text/html; charset=utf-8",
	"app.js":     "text/javascript; charset=utf-8",
	"style.css":  "text/css; charset=utf-8",
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
	ct, ok := uiFiles[name]
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "静态资源不存在", nil)
		return
	}
	data, err := fs.ReadFile(uiAssets, "ui/"+name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "读取内嵌资源失败", nil)
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
