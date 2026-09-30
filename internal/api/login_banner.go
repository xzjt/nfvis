package api

// 登录横幅（决策 #303）：一处配置（system.login.banner）、三个面——
//   GET    /login-banner          未认证只读（登录页/CLI 登录前展示用）
//   PUT    /system/login-banner   设置（Web 管理面；一次性事务，等价 CLI set system login banner）
//   DELETE /system/login-banner   清除（等价 CLI delete system login banner）
//
// 未认证端点的边界：**只返回横幅文本本身**，不返回主机名/版本/用户清单等任何其他信息——
// 登录前不给探查面。未设置横幅时 banner 字段省略（不回空串、不编造），客户端据此不渲染横幅块。
// 本端点与 POST /login 同级注册在鉴权包装之外（见 server.go），无限流豁免：登录页每次展示
// 都会取一次，随既有中间件即可。

import (
	"net/http"

	"github.com/xzjt/nfvis/internal/model"
)

// loginBannerOf 返回配置文档里的登录横幅（未设置返回空串）。
func loginBannerOf(cfg model.Config) string {
	if cfg.System != nil && cfg.System.Login != nil {
		return cfg.System.Login.Banner
	}
	return ""
}

// handleLoginBanner GET /api/v1/login-banner：登录横幅（未认证可达）。
func (s *Server) handleLoginBanner(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	out := map[string]any{}
	if b := loginBannerOf(cfg); b != "" {
		out["banner"] = b
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePutLoginBanner PUT /api/v1/system/login-banner：设置登录横幅。
// 取值校验（单行、512 字节上限）由 model.Validate 在提交时执行，错误文案带上限值。
func (s *Server) handlePutLoginBanner(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Banner string `json:"banner"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if in.Banner == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED",
			"横幅文本为空：设置请给出文本，清除请用 DELETE /system/login-banner（CLI 为 delete system login banner）", nil)
		return
	}
	s.mutateLoginUsers(w, r, http.StatusOK, "login banner 变更", func(l *model.SystemLogin) error {
		l.Banner = in.Banner
		return nil
	})
}

// handleDeleteLoginBanner DELETE /api/v1/system/login-banner：清除登录横幅。
func (s *Server) handleDeleteLoginBanner(w http.ResponseWriter, r *http.Request) {
	s.mutateLoginUsers(w, r, http.StatusOK, "login banner 变更", func(l *model.SystemLogin) error {
		l.Banner = ""
		return nil
	})
}
