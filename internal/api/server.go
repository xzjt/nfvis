// Package api 实现 NFViS REST server（FR-API-001/005/007）。
//
// M2 首块：路由骨架、Bearer Token 中间件、统一错误格式（Error{code,message,
// detail[]}）、认证端点；配置事务/资源 handlers 随 M2 后续任务按 OpenAPI
// 契约逐组落地。版本化路径 /api/v1/。
package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/schema"
)

// API 版本与产品版本（FR-API-007；组件版本经 GET /system/version 汇报）。
const (
	APIPrefix  = "/api/v1"
	VersionStr = "1.0.0-dev"
)

// Options server 可选项。
type Options struct {
	Addr    string // 监听地址（默认 :443）
	TLSCert string // TLS 证书路径（FR-API-001，HTTPS；与 TLSKey 成对）
	TLSKey  string // TLS 私钥路径；二者为空 = 明文 HTTP（仅限开发/测试）
	Log     *slog.Logger
}

// Server NFViS REST server。
type Server struct {
	aaa     *aaa.Service
	engine  *config.Engine
	log     *slog.Logger
	mux     *http.ServeMux
	http    *http.Server
	tlsCert string
	tlsKey  string
}

// Handler 返回根 HTTP handler（测试与嵌套装配使用）。
func (s *Server) Handler() http.Handler { return s.logRequests(s.mux) }

// New 构造 server 并注册路由。
func New(e *config.Engine, a *aaa.Service, opts Options) *Server {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	s := &Server{aaa: a, engine: e, log: log}
	mux := http.NewServeMux()

	// 认证（免 token，FR-API-001）
	mux.HandleFunc("POST "+APIPrefix+"/login", s.handleLogin)
	// 认证后端点：required class + 命令树路径（自定义 class ACL 判定用）
	mux.Handle("POST "+APIPrefix+"/logout", s.auth(s.handleLogout, schema.ClassReadOnly, "logout"))
	mux.Handle("GET "+APIPrefix+"/system/version", s.auth(s.handleVersion, schema.ClassReadOnly, "show version"))

	// 配置事务（/configuration/*，configure 为 S 级权限，命令树 §4）
	cfgAPI := func(h http.HandlerFunc) http.Handler {
		return s.auth(h, schema.ClassSuperUser, "configure")
	}
	mux.Handle("GET "+APIPrefix+"/configuration/candidate", cfgAPI(s.handleGetCandidate))
	mux.Handle("PUT "+APIPrefix+"/configuration/candidate", cfgAPI(s.handlePutCandidate))
	mux.Handle("DELETE "+APIPrefix+"/configuration/candidate", cfgAPI(s.handleDeleteCandidate))
	mux.Handle("POST "+APIPrefix+"/configuration/commit", cfgAPI(s.handleCommit))
	mux.Handle("POST "+APIPrefix+"/configuration/commit:confirm", cfgAPI(s.handleCommitConfirm))
	mux.Handle("GET "+APIPrefix+"/configuration/diff", cfgAPI(s.handleDiff))
	mux.Handle("POST "+APIPrefix+"/configuration/rollback/{n}", cfgAPI(s.handleRollback))
	mux.Handle("GET "+APIPrefix+"/system/configuration/sessions", cfgAPI(s.handleSessions))

	// 资源 handlers 第一组（GET = show 等级 R；写 = configure 等级 S；FR-API-003 映射）
	mux.Handle("GET "+APIPrefix+"/system", s.auth(s.handleGetSystem, schema.ClassReadOnly, "show system"))
	mux.Handle("PUT "+APIPrefix+"/system", cfgAPI(s.handlePutSystem))
	mux.Handle("GET "+APIPrefix+"/interfaces", s.auth(s.handleGetInterfaces, schema.ClassReadOnly, "show interfaces"))
	mux.Handle("GET "+APIPrefix+"/interfaces/{name}", s.auth(s.handleGetInterface, schema.ClassReadOnly, "show interfaces"))
	mux.Handle("PUT "+APIPrefix+"/interfaces/{name}", cfgAPI(s.handlePutInterface))
	mux.Handle("GET "+APIPrefix+"/virtual-switches", s.auth(s.handleGetVSwitches, schema.ClassReadOnly, "show virtual-switches"))
	mux.Handle("GET "+APIPrefix+"/virtual-switches/{name}", s.auth(s.handleGetVSwitch, schema.ClassReadOnly, "show virtual-switches"))
	mux.Handle("GET "+APIPrefix+"/virtual-switches/{name}/ports", s.auth(s.handleGetVSwitchPorts, schema.ClassReadOnly, "show virtual-switches"))
	mux.Handle("POST "+APIPrefix+"/virtual-switches", cfgAPI(s.handlePostVSwitch))
	mux.Handle("DELETE "+APIPrefix+"/virtual-switches/{name}", cfgAPI(s.handleDeleteVSwitch))
	mux.Handle("PUT "+APIPrefix+"/virtual-switches/{name}/ports", cfgAPI(s.handlePutVSwitchPorts))
	mux.Handle("GET "+APIPrefix+"/vrfs", s.auth(s.handleGetVrfs, schema.ClassReadOnly, "show vrfs"))
	mux.Handle("GET "+APIPrefix+"/vrfs/{name}", s.auth(s.handleGetVrf, schema.ClassReadOnly, "show vrfs"))
	mux.Handle("POST "+APIPrefix+"/vrfs", cfgAPI(s.handlePostVrf))
	mux.Handle("DELETE "+APIPrefix+"/vrfs/{name}", cfgAPI(s.handleDeleteVrf))
	mux.Handle("PUT "+APIPrefix+"/vrfs/{name}/routes", cfgAPI(s.handlePutVrfRoutes))

	addr := opts.Addr
	if addr == "" {
		addr = ":443"
	}
	s.mux = mux
	s.http = &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.tlsCert, s.tlsKey = opts.TLSCert, opts.TLSKey
	return s
}

// ListenAndServe 阻塞提供服务；Shutdown 优雅退出。
func (s *Server) ListenAndServe() error {
	if s.tlsCert != "" && s.tlsKey != "" {
		s.log.Info("API 服务启动（HTTPS）", "addr", s.http.Addr)
		return s.http.ListenAndServeTLS(s.tlsCert, s.tlsKey)
	}
	s.log.Info("API 服务启动（HTTP，仅限开发/测试）", "addr", s.http.Addr)
	return s.http.ListenAndServe()
}

// Shutdown 优雅停机。
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

// ---------- 中间件 ----------

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.log.Debug("api", "method", r.Method, "path", r.URL.Path, "elapsed", time.Since(start))
	})
}

// auth Bearer Token 鉴权中间件（FR-API-001/FR-SEC-002/005）。
// required 为端点所需最低 class；cmdPath 为对应的 CLI 命令路径
// （自定义 class 的 allow/deny ACL 判定依据，FR-API-003：每个端点映射命令树）。
func (s *Server) auth(next http.HandlerFunc, required schema.Class, cmdPath ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r)
		if tok == "" {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "缺少 Bearer Token", nil)
			return
		}
		info, err := s.aaa.VerifyToken(tok)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "token 无效或已过期", nil)
			return
		}
		if !s.aaa.Authorize(info.Class, required, cmdPath...) {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "当前 class 无权执行该操作", nil)
			return
		}
		ctx := context.WithValue(r.Context(), identityKey{}, *info)
		next(w, r.WithContext(ctx))
	}
}

type identityKey struct{}

// Identity 从请求上下文取已认证身份。
func Identity(r *http.Request) (aaa.TokenInfo, bool) {
	v, ok := r.Context().Value(identityKey{}).(aaa.TokenInfo)
	return v, ok
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(h, "Bearer ")
}
