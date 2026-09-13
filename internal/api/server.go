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
	"github.com/xzjt/nfvis/internal/state"
)

// API 版本与产品版本（FR-API-007；组件版本经 GET /system/version 汇报）。
const (
	APIPrefix  = "/api/v1"
	VersionStr = "1.0.0-dev"
)

// Options server 可选项。
type Options struct {
	Addr      string // 监听地址（默认 :443）
	TLSCert   string // TLS 证书路径（FR-API-001，HTTPS；与 TLSKey 成对）
	TLSKey    string // TLS 私钥路径；二者为空 = 明文 HTTP（仅限开发/测试）
	Log       *slog.Logger
	VPP       VppController      // VPP 数据面控制（M3-2；nil = /vpp/* 返回 503）
	L2        L2Runtime          // L2 运行态查询（M3-3；nil = mac-table 503）
	L3        L3Runtime          // L3 运行态查询（M3-4；nil = routes 503）
	LLDP      LldpRuntime        // LLDP 邻居（M3-6；nil = 503）
	State     *state.State       // 运行态聚合（M3-7；nil = 省略运行态字段）
	SRIOV     SRIOVSetter        // SR-IOV VF 数量（M3-7；nil = 503）
	NAT       NatSessionsRuntime // NAT 会话（M3-7；nil = 503）
	Alarms    AlarmRuntime       // 告警列表（M3-8；nil = 503）
	Diag      DiagRuntime        // CLI 诊断命令（M3-9；nil = 命令报不可用）
	VM        VMRuntime          // VM 生命周期（M4-3；nil = 生命周期动作 503、状态省略）
	VMConsole VMConsoleRuntime   // VM 串口 console（M4-5；nil = console 端点 503）
}

// Server NFViS REST server。
type Server struct {
	aaa         *aaa.Service
	engine      *config.Engine
	cliExec     *cliExecutor
	vpp         VppController
	l2          L2Runtime
	l3          L3Runtime
	lldp        LldpRuntime
	state       *state.State
	sriov       SRIOVSetter
	natSessions NatSessionsRuntime
	alarms      AlarmRuntime
	vm          VMRuntime
	vmConsole   VMConsoleRuntime
	consoleTix  *consoleTickets
	log         *slog.Logger
	mux         *http.ServeMux
	http        *http.Server
	tlsCert     string
	tlsKey      string
}

// Handler 返回根 HTTP handler（测试与嵌套装配使用）。
func (s *Server) Handler() http.Handler { return s.logRequests(s.mux) }

// New 构造 server 并注册路由。
func New(e *config.Engine, a *aaa.Service, opts Options) *Server {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	s := &Server{aaa: a, engine: e, cliExec: newCLIExecutor(e, a), vpp: opts.VPP, l2: opts.L2, l3: opts.L3, lldp: opts.LLDP, state: opts.State, sriov: opts.SRIOV, natSessions: opts.NAT, alarms: opts.Alarms, vm: opts.VM, vmConsole: opts.VMConsole, consoleTix: newConsoleTickets(), log: log}
	s.cliExec.setRuntime(opts.Diag, opts.State)
	s.cliExec.setNetRuntime(opts.L2, opts.L3, opts.LLDP, opts.NAT, opts.Alarms)
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

	// CLI 执行通道（cli_bridge）：逐命令权限在执行器内按 schema 节点判定
	mux.Handle("POST "+APIPrefix+"/cli/execute", s.auth(s.handleCLIExecute, schema.ClassReadOnly, "cli"))
	mux.Handle("GET "+APIPrefix+"/cli/candidates", s.auth(s.handleCLICandidates, schema.ClassReadOnly, "cli"))
	mux.Handle("GET "+APIPrefix+"/audit-logs", s.auth(s.handleAuditLogs, schema.ClassReadOnly, "show log audit"))

	// W5：资源池 / 本地用户 / 系统状态（GET=R；写=S，§4）
	mux.Handle("GET "+APIPrefix+"/resource-pools", s.auth(s.handleGetResourcePools, schema.ClassReadOnly, "show resource-pools"))
	mux.Handle("PUT "+APIPrefix+"/resource-pools", cfgAPI(s.handlePutResourcePools))
	mux.Handle("GET "+APIPrefix+"/system/login-users", cfgAPI(s.handleGetLoginUsers))
	mux.Handle("POST "+APIPrefix+"/system/login-users", cfgAPI(s.handlePostLoginUser))
	mux.Handle("PUT "+APIPrefix+"/system/login-users/{name}", cfgAPI(s.handlePutLoginUser))
	mux.Handle("DELETE "+APIPrefix+"/system/login-users/{name}", cfgAPI(s.handleDeleteLoginUser))
	// {name}:change-password 含冒号后缀，ServeMux 通配符不支持——以 {tail...} 捕获后分发
	mux.Handle("POST "+APIPrefix+"/system/login-users/{tail...}", s.auth(s.dispatchLoginUsersPost, schema.ClassReadOnly, "request system password change"))
	mux.Handle("GET "+APIPrefix+"/system/status", s.auth(s.handleGetSystemStatus, schema.ClassReadOnly, "show system uptime"))
	// M3-2：VPP 数据面状态与重启（FR-SYS-007/009）
	mux.Handle("GET "+APIPrefix+"/vpp/status", s.auth(s.handleGetVppStatus, schema.ClassReadOnly, "show vpp"))
	mux.Handle("GET "+APIPrefix+"/vpp/config", s.auth(s.handleGetVppConfig, schema.ClassReadOnly, "show vpp"))
	mux.Handle("POST "+APIPrefix+"/vpp/restart", s.auth(s.handlePostVppRestart, schema.ClassSuperUser, "request vpp restart"))

	// W6：网络配置层第二组（GET=R；写=S）
	mux.Handle("GET "+APIPrefix+"/acls", s.auth(s.handleGetAcls, schema.ClassReadOnly, "show acls"))
	mux.Handle("GET "+APIPrefix+"/acls/{name}", s.auth(s.handleGetAcl, schema.ClassReadOnly, "show acls"))
	mux.Handle("POST "+APIPrefix+"/acls", cfgAPI(s.handlePostAcl))
	mux.Handle("DELETE "+APIPrefix+"/acls/{name}", cfgAPI(s.handleDeleteAcl))
	mux.Handle("GET "+APIPrefix+"/nat", s.auth(s.handleGetNat, schema.ClassReadOnly, "show nat"))
	mux.Handle("GET "+APIPrefix+"/nat/sessions", s.auth(s.handleGetNatSessions, schema.ClassReadOnly, "show nat sessions"))
	mux.Handle("PUT "+APIPrefix+"/nat", cfgAPI(s.handlePutNat))
	mux.Handle("GET "+APIPrefix+"/qos/policies", s.auth(s.handleGetQosPolicies, schema.ClassReadOnly, "show qos policies"))
	mux.Handle("POST "+APIPrefix+"/qos/policies", cfgAPI(s.handlePostQosPolicy))
	mux.Handle("DELETE "+APIPrefix+"/qos/policies/{name}", cfgAPI(s.handleDeleteQosPolicy))
	mux.Handle("GET "+APIPrefix+"/port-mirroring", s.auth(s.handleGetPMs, schema.ClassReadOnly, "show port-mirroring"))
	mux.Handle("POST "+APIPrefix+"/port-mirroring", cfgAPI(s.handlePostPM))
	mux.Handle("DELETE "+APIPrefix+"/port-mirroring/{name}", cfgAPI(s.handleDeletePM))
	mux.Handle("GET "+APIPrefix+"/bonds", s.auth(s.handleGetBonds, schema.ClassReadOnly, "show bonds"))
	mux.Handle("GET "+APIPrefix+"/bonds/{name}", s.auth(s.handleGetBond, schema.ClassReadOnly, "show bonds"))
	mux.Handle("POST "+APIPrefix+"/bonds", cfgAPI(s.handlePostBond))
	mux.Handle("DELETE "+APIPrefix+"/bonds/{name}", cfgAPI(s.handleDeleteBond))
	mux.Handle("GET "+APIPrefix+"/protocols/lldp", s.auth(s.handleGetLldp, schema.ClassReadOnly, "show lldp"))
	mux.Handle("PUT "+APIPrefix+"/protocols/lldp", cfgAPI(s.handlePutLldp))
	mux.Handle("GET "+APIPrefix+"/protocols/lldp/neighbors", s.auth(s.handleGetLldpNeighbors, schema.ClassReadOnly, "show lldp"))

	// M4-3：VM VNF 配置与生命周期（FR-CMP-010~013）
	mux.Handle("GET "+APIPrefix+"/virtual-machine-functions", s.auth(s.handleListVMs, schema.ClassReadOnly, "show virtual-machine-functions"))
	mux.Handle("GET "+APIPrefix+"/virtual-machine-functions/{name}", s.auth(s.handleGetVM, schema.ClassReadOnly, "show virtual-machine-functions"))
	mux.Handle("POST "+APIPrefix+"/virtual-machine-functions", cfgAPI(s.handlePostVM))
	mux.Handle("PUT "+APIPrefix+"/virtual-machine-functions/{name}", cfgAPI(s.handlePutVM))
	mux.Handle("DELETE "+APIPrefix+"/virtual-machine-functions/{name}", cfgAPI(s.handleDeleteVM))
	// {name}:start|stop|restart 含冒号后缀，ServeMux 通配符不支持——{tail...} 捕获后分发
	mux.Handle("POST "+APIPrefix+"/virtual-machine-functions/{tail...}", s.auth(s.dispatchVMPost, schema.ClassSuperUser, "request virtual-machine-functions"))
	// M4-5：串口 console（凭证端点需 Bearer；ws 端点以一次性 ticket 鉴权）
	mux.Handle("POST "+APIPrefix+"/virtual-machine-functions/{name}/console", s.auth(s.handleConsoleTicket, schema.ClassSuperUser, "request virtual-machine-functions console"))
	mux.Handle("GET "+APIPrefix+"/virtual-machine-functions/{name}/console/ws", http.HandlerFunc(s.handleConsoleWS))

	// M3-8：告警列表（恢复收敛的不可收敛项落点，FR-OPS-010）
	mux.Handle("GET "+APIPrefix+"/alarms", s.auth(s.handleGetAlarms, schema.ClassReadOnly, "show alarms"))

	// 资源 handlers 第一组（GET = show 等级 R；写 = configure 等级 S；FR-API-003 映射）
	mux.Handle("GET "+APIPrefix+"/system", s.auth(s.handleGetSystem, schema.ClassReadOnly, "show system"))
	mux.Handle("PUT "+APIPrefix+"/system", cfgAPI(s.handlePutSystem))
	mux.Handle("GET "+APIPrefix+"/interfaces", s.auth(s.handleGetInterfaces, schema.ClassReadOnly, "show interfaces"))
	mux.Handle("GET "+APIPrefix+"/interfaces/{name}", s.auth(s.handleGetInterface, schema.ClassReadOnly, "show interfaces"))
	mux.Handle("PUT "+APIPrefix+"/interfaces/{name}", cfgAPI(s.handlePutInterface))
	mux.Handle("PUT "+APIPrefix+"/interfaces/{name}/sriov", s.auth(s.handlePutSRIOV, schema.ClassSuperUser, "set interfaces sriov"))
	mux.Handle("GET "+APIPrefix+"/virtual-switches", s.auth(s.handleGetVSwitches, schema.ClassReadOnly, "show virtual-switches"))
	mux.Handle("GET "+APIPrefix+"/virtual-switches/{name}", s.auth(s.handleGetVSwitch, schema.ClassReadOnly, "show virtual-switches"))
	mux.Handle("GET "+APIPrefix+"/virtual-switches/{name}/ports", s.auth(s.handleGetVSwitchPorts, schema.ClassReadOnly, "show virtual-switches"))
	mux.Handle("GET "+APIPrefix+"/virtual-switches/{name}/mac-table", s.auth(s.handleGetMacTable, schema.ClassReadOnly, "show virtual-switches"))
	mux.Handle("POST "+APIPrefix+"/virtual-switches", cfgAPI(s.handlePostVSwitch))
	mux.Handle("DELETE "+APIPrefix+"/virtual-switches/{name}", cfgAPI(s.handleDeleteVSwitch))
	mux.Handle("PUT "+APIPrefix+"/virtual-switches/{name}/ports", cfgAPI(s.handlePutVSwitchPorts))
	mux.Handle("GET "+APIPrefix+"/vrfs", s.auth(s.handleGetVrfs, schema.ClassReadOnly, "show vrfs"))
	mux.Handle("GET "+APIPrefix+"/vrfs/{name}", s.auth(s.handleGetVrf, schema.ClassReadOnly, "show vrfs"))
	mux.Handle("POST "+APIPrefix+"/vrfs", cfgAPI(s.handlePostVrf))
	mux.Handle("DELETE "+APIPrefix+"/vrfs/{name}", cfgAPI(s.handleDeleteVrf))
	mux.Handle("GET "+APIPrefix+"/vrfs/{name}/routes", s.auth(s.handleGetVrfRoutes, schema.ClassReadOnly, "show vrfs"))
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
