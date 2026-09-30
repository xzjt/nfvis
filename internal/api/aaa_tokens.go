package api

// 决策 #301：活动会话 / API Token（列出 + 逐 token 吊销）。
//
// 与 CLI 同一实现：`GET /system/api-tokens` ≡ `show system api tokens`，
// `POST /system/api-tokens/{id}:revoke` ≡ `request system api token revoke <token-id>`。
// 权限与范围判定（super-user 全量 / 其他 class 仅自己、404 不泄露存在性）单源落在
// aaa.Service.ListTokens / RevokeToken——CLI 执行器不经 HTTP handler 直接调服务，
// 这里不再复刻第二份判定，只保留最低 class 的纵深防御（注册处 schema.ClassReadOnly）。

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/xzjt/nfvis/internal/aaa"
)

// apiTokenJSON GET /system/api-tokens 的响应条目（契约 ApiToken）。
type apiTokenJSON struct {
	TokenID   string `json:"token_id"`
	User      string `json:"user"`
	Class     string `json:"class"`
	IssuedAt  string `json:"issued_at"`
	ExpiresAt string `json:"expires_at"`
	Current   bool   `json:"current"`
}

type apiTokenListJSON struct {
	Tokens []apiTokenJSON `json:"tokens"`
}

// handleListAPITokens GET /api/v1/system/api-tokens：活动会话清单。
// super-user 见全部用户的会话；其他 class 只见自己的（范围判定在 aaa 实现）。
func (s *Server) handleListAPITokens(w http.ResponseWriter, r *http.Request) {
	ident, _ := Identity(r)
	views := s.aaa.ListTokens(ident.User, ident.Class)
	out := make([]apiTokenJSON, 0, len(views))
	for _, v := range views {
		out = append(out, apiTokenJSON{
			TokenID:   v.ID,
			User:      v.User,
			Class:     v.Class,
			IssuedAt:  v.IssuedAt.Format(time.RFC3339),
			ExpiresAt: v.ExpiresAt.Format(time.RFC3339),
			Current:   v.Current(ident.ID),
		})
	}
	writeJSON(w, http.StatusOK, apiTokenListJSON{Tokens: out})
}

// dispatchAPITokensPost POST /api/v1/system/api-tokens/{tail...}：
// {id}:revoke 含冒号后缀，ServeMux 通配符不支持——{tail...} 捕获后分发。
func (s *Server) dispatchAPITokensPost(w http.ResponseWriter, r *http.Request) {
	tail := r.PathValue("tail")
	id := strings.TrimSuffix(tail, ":revoke")
	if id == "" || id == tail {
		// 形态只能是 `<id>:revoke`；其余（无动作后缀/未知动作）按不存在处理。
		writeError(w, http.StatusNotFound, "NOT_FOUND", "会话不存在或无权操作该会话", nil)
		return
	}
	s.handleRevokeAPIToken(w, r, id)
}

// handleRevokeAPIToken POST /api/v1/system/api-tokens/{id}:revoke：吊销指定会话。
// 吊销成功 204；「不存在或无权」一律 404 同一文案（不泄露存在性）。
func (s *Server) handleRevokeAPIToken(w http.ResponseWriter, r *http.Request, id string) {
	ident, _ := Identity(r)
	if err := s.aaa.RevokeToken(ident.User, ident.Class, id); err != nil {
		if errors.Is(err, aaa.ErrTokenNotFoundOrForbidden) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error(), nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	// 安全相关动作入审计（与 CLI 执行器同一动作名，两侧同源可配对）
	s.engine.Audit(ident.User, "system.api-token.revoke", "吊销会话 "+id, "success")
	w.WriteHeader(http.StatusNoContent)
}
