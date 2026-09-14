package api

// FR-API-002（决策 #69）：随产品发布的 OpenAPI 规范运行时副本。
//
// 内容为 docs/NFViS-openapi.yaml 的规范转换结果，编译期嵌入二进制
// （契约真源仍为 YAML；一致性由 `make docscheck` → check_openapi_json_sync.sh 守护）。
// 无鉴权（与 /metrics 同）：该文档亦随 deb 安装于 /usr/share/doc/nfvis/，不含敏感信息。

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.json
var openAPISpec []byte

// handleOpenAPISpec GET /api/v1/openapi.json：返回本规范（Web 控制面据此开发）。
func (s *Server) handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openAPISpec)
}
