package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/xzjt/nfvis/internal/aaa"
)

// ---------- 统一错误格式（FR-API-005：Error{code, message, detail[]}） ----------

// ErrorDetail 单条错误明细（如校验错误逐条列出）。
type ErrorDetail struct {
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

// ErrorResponse OpenAPI Error schema。
type ErrorResponse struct {
	Code    string        `json:"code"`
	Message string        `json:"message"`
	Detail  []ErrorDetail `json:"detail,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, message string, detail []ErrorDetail) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorResponse{Code: code, Message: message, Detail: detail})
}

// ---------- 认证端点（FR-API-001） ----------

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token     string    `json:"token"`
	TokenID   string    `json:"token_id"`
	User      loginUser `json:"user"`
	ExpiresIn int       `json:"expires_in"` // 秒
}

// loginUser 对应契约的 LoginUser：`user` 是**对象**（name/class）。
// 曾经实现回的是扁平字符串 + 顶层 class——照契约（FR-API-002：Web 控制面据此开发）
// 写的前端把 body.user 当对象用，顶栏于是显示 "undefined（undefined）"（round39 可视验收发现）。
type loginUser struct {
	Name  string `json:"name"`
	Class string `json:"class"`
}

// handleLogin POST /api/v1/login：用户名口令换 Bearer Token。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "需要 username 与 password", nil)
		return
	}
	tok, err := s.aaa.Login(req.Username, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, aaa.ErrLocked):
			writeError(w, http.StatusLocked, "ACCOUNT_LOCKED", err.Error(), nil) // 423
		case errors.Is(err, aaa.ErrNoUsers):
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error(), nil)
		default:
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "用户名或口令错误", nil)
		}
		return
	}
	// FR-SEC-007：token 不落日志；响应仅此一次携带
	writeJSON(w, http.StatusOK, loginResponse{
		Token:     tok.Token,
		TokenID:   tok.Token[:8],
		User:      loginUser{Name: tok.User, Class: tok.Class},
		ExpiresIn: int(time.Until(tok.ExpiresAt).Seconds()),
	})
}

// handleLogout POST /api/v1/logout：吊销当前 token（FR-API-001 可吊销）。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if tok := bearerToken(r); tok != "" {
		s.aaa.Logout(tok)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleVersion GET /api/v1/system/version（OpenAPI VersionInfo；Ubuntu/VPP
// 等组件版本由 state 模块接入底座后补齐，FR-SYS-007）。
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"nfvis":  VersionStr,
		"ubuntu": "", "vpp": "", "dpdk": "", "libvirt": "", "qemu": "", "docker": "",
	})
}

// ---------- 通用 ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
