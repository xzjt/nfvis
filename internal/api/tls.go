package api

// M5-8：TLS 证书管理（FR-SYS-011）。
//
// GET  /system/tls              当前证书信息（subject/issuer/有效期/指纹）
// PUT  /system/tls              安装外部证书（PEM 文本，校验配对后立即生效）
// POST /system/tls:regenerate   重签自签证书（立即生效）

import (
	"context"
	"net/http"
	"os"

	"github.com/xzjt/nfvis/internal/system"
)

// TlsRuntime 证书能力（*system.TLSManager 经适配注入；nil = 503）。
//
// 接口里**没有**「带 SAN 参数的重签」：自签证书的 SAN 由实现按本机监听地址统一推导
// （`system.ServerCertSANs`）。这是有意的——历史上 REST 与 CLI 各自传 ips（CLI 传 nil），
// 重签出的证书缺回环 IP SAN，而 nfvis-cli 缺省连 https://127.0.0.1 且做完整主机名校验
// （决策 #78），于是「重签」当场自毁管理路径（发现 #10）。把 ips 移出接口后，
// 调用方**没有机会**漏传或传错（同决策 #75：约束要落在两侧共同依赖处）。
type TlsRuntime interface {
	Info() (system.TlsInfo, bool)
	Install(certPEM, keyPEM string) (system.TlsInfo, error)
	RegenerateSelfSigned(hostname string) (system.TlsInfo, error)
	RegenerateSSHHostKeys(ctx context.Context) error
}

func (s *Server) requireTLS(w http.ResponseWriter) bool {
	if s.tlsMgr == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "证书管理未接入", nil)
		return false
	}
	return true
}

// handleGetTLS GET /api/v1/system/tls
func (s *Server) handleGetTLS(w http.ResponseWriter, r *http.Request) {
	if !s.requireTLS(w) {
		return
	}
	info, ok := s.tlsMgr.Info()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handlePutTLS PUT /api/v1/system/tls（安装外部证书 PEM）
func (s *Server) handlePutTLS(w http.ResponseWriter, r *http.Request) {
	if !s.requireTLS(w) {
		return
	}
	var in struct {
		Certificate string `json:"certificate"`
		Key         string `json:"key"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if in.Certificate == "" || in.Key == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "certificate 与 key 必填（PEM 文本）", nil)
		return
	}
	info, err := s.tlsMgr.Install(in.Certificate, in.Key)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	user := "api"
	if info2, ok := Identity(r); ok {
		user = info2.User
	}
	s.engine.Audit(user, "system.tls.install", "安装外部证书（指纹 "+info.Fingerprint+"）", "success")
	writeJSON(w, http.StatusOK, info)
}

// handlePostTLSRegenerate POST /api/v1/system/tls:regenerate
func (s *Server) handlePostTLSRegenerate(w http.ResponseWriter, r *http.Request) {
	if !s.requireTLS(w) {
		return
	}
	host, _ := os.Hostname()
	info, err := s.tlsMgr.RegenerateSelfSigned(host)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error(), nil)
		return
	}
	user := "api"
	if id, ok := Identity(r); ok {
		user = id.User
	}
	s.engine.Audit(user, "system.tls.regenerate", "重签自签证书（指纹 "+info.Fingerprint+"）", "success")
	writeJSON(w, http.StatusOK, info)
}
