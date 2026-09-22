// Package api 实现 NFViS REST server（FR-API-001/005/007）。
//
// M2 首块：路由骨架、Bearer Token 中间件、统一错误格式（Error{code,message,
// detail[]}）、认证端点；配置事务/资源 handlers 随 M2 后续任务按 OpenAPI
// 契约逐组落地。版本化路径 /api/v1/。
package api

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/netutil"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/events"
	"github.com/xzjt/nfvis/internal/schema"
	"github.com/xzjt/nfvis/internal/state"
	ksys "github.com/xzjt/nfvis/internal/system"
)

// API 版本与产品版本（FR-API-007；组件版本经 GET /system/version 汇报）。
const APIPrefix = "/api/v1"

// VersionStr 产品版本；可经 -ldflags "-X .../internal/api.VersionStr=x.y.z" 注入（deb 打包用）。
var VersionStr = "1.0.0-dev"

// Options server 可选项。
type Options struct {
	Addr        string // 监听地址（默认 :443）
	TLSCert     string // TLS 证书路径（FR-API-001，HTTPS；与 TLSKey 成对）
	TLSKey      string // TLS 私钥路径；二者为空 = 明文 HTTP（仅限开发/测试）
	Log         *slog.Logger
	VPP         VppController          // VPP 数据面控制（M3-2；nil = /vpp/* 返回 503）
	L2          L2Runtime              // L2 运行态查询（M3-3；nil = mac-table 503）
	L3          L3Runtime              // L3 运行态查询（M3-4；nil = routes 503）
	LLDP        LldpRuntime            // LLDP 邻居（M3-6；nil = 503）
	State       *state.State           // 运行态聚合（M3-7；nil = 省略运行态字段）
	SRIOV       SRIOVSetter            // SR-IOV VF 数量（M3-7；nil = 503）
	DPDK        DPDKSetter             // 网卡 DPDK 驱动接管（FR-NET-001，决策 #72；nil = 503）
	Kernel      ksys.KernelApplier     // 内核启动基线落地（FR-SYS-014；nil = 命令报未接入）
	NAT         NatSessionsRuntime     // NAT 会话（M3-7；nil = 503）
	Alarms      AlarmRuntime           // 告警列表（M3-8；nil = 503）
	Diag        DiagRuntime            // CLI 诊断命令（M3-9；nil = 命令报不可用）
	VM          VMRuntime              // VM 生命周期（M4-3；nil = 生命周期动作 503、状态省略）
	VMConsole   VMConsoleRuntime       // VM 串口 console（M4-5；nil = console 端点 503）
	VMSnapshots VMSnapshotRuntime      // VM 快照（M4-6；nil = 快照端点 503）
	Containers  ContainerRuntime       // 容器生命周期/日志（M4-7；nil = 503）
	Images      ImagesRuntime          // 镜像仓库（M4-8；nil = 503）
	Events      *events.Bus            // 事件总线（M5-1；nil = /events 503）
	SysOps      SystemOpsRuntime       // 备份/恢复/恢复出厂（M5-6；nil = 503）
	DiagOps     DiagOpsRuntime         // 诊断归档/core dump（M5-4；nil = 503）
	LogSource   func() ([]byte, error) // 系统日志来源（M5-9 show log system；nil = 报不可用）
	Capture     CaptureRuntime         // 数据面抓包（M5-3；nil = 503）
	Software    SoftwareRuntime        // 软件升级/电源/NTP（M5-7；nil = 503）
	Hardware    HardwareRuntime        // 硬件健康采集（M5-5；nil = 503）
	TLS         TlsRuntime             // 证书管理（M5-8；nil = 503）
	Ports       PortInventory          // 运行态端口清单（决策 #83；nil = 接口名无动态候选）
	VppState    VppStateRuntime        // VPP 运行态快照（决策 #84；nil = 相关 show 报未接入）
	Versions    VersionsRuntime        // 组件版本探测（R37-2 收口，决策 #118；nil = 只回 NFViS 版本）
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
	vppState    VppStateRuntime // VPP 运行态快照（决策 #84/#116：CLI show 与 REST 同源）
	versions    VersionsRuntime // 组件版本探测（R37-2 收口，决策 #118）
	sriov       SRIOVSetter
	dpdk        DPDKSetter
	natSessions NatSessionsRuntime
	alarms      AlarmRuntime
	vm          VMRuntime
	vmConsole   VMConsoleRuntime
	vmSnapshots VMSnapshotRuntime
	containers  ContainerRuntime
	images      ImagesRuntime
	ports       PortInventory // 运行态端口清单（决策 #83；候选与 show 同源）
	events      *events.Bus
	sysOps      SystemOpsRuntime
	diagOps     DiagOpsRuntime
	capture     CaptureRuntime
	software    SoftwareRuntime
	hardware    HardwareRuntime
	tlsMgr      TlsRuntime
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
	s := &Server{aaa: a, engine: e, cliExec: newCLIExecutor(e, a), vpp: opts.VPP, l2: opts.L2, l3: opts.L3, lldp: opts.LLDP, state: opts.State, vppState: opts.VppState, sriov: opts.SRIOV, dpdk: opts.DPDK, natSessions: opts.NAT, alarms: opts.Alarms, vm: opts.VM, vmConsole: opts.VMConsole, vmSnapshots: opts.VMSnapshots, containers: opts.Containers, images: opts.Images, ports: opts.Ports, events: opts.Events, sysOps: opts.SysOps, diagOps: opts.DiagOps, capture: opts.Capture, software: opts.Software, hardware: opts.Hardware, tlsMgr: opts.TLS, consoleTix: newConsoleTickets(), versions: opts.Versions, log: log}
	s.cliExec.setRuntime(opts.Diag, opts.State)
	s.cliExec.setPorts(opts.Ports)       // 决策 #83：show 的空态与 Tab 候选同源
	s.cliExec.setVppCtl(opts.VPP)        // 发现 #11：show vpp 的版本/连接/待重启
	s.cliExec.setVppState(opts.VppState) // 决策 #84：show 的运行态事实来源
	s.cliExec.setNetRuntime(opts.L2, opts.L3, opts.LLDP, opts.NAT, opts.Alarms)
	s.cliExec.setComputeRuntime(opts.VM, opts.VMConsole, opts.VMSnapshots, opts.Containers, opts.Images)
	s.cliExec.setEventBus(opts.Events) // M5-1：CLI 直连动作也发布 vnf-state-changed
	s.cliExec.setSystemOps(opts.SysOps)
	s.cliExec.setDiagOps(opts.DiagOps)
	s.cliExec.setLogSource(opts.LogSource)
	s.cliExec.setVersions(opts.Versions) // R37-2 收口（决策 #118）：show version 七组件汇总
	s.cliExec.setCapture(opts.Capture)
	s.cliExec.setSoftware(opts.Software)
	s.cliExec.setHardware(opts.Hardware)
	s.cliExec.setSRIOV(opts.SRIOV)
	s.cliExec.setDPDK(opts.DPDK)
	s.cliExec.setKernel(opts.Kernel)
	s.cliExec.setTLS(opts.TLS)
	s.cliExec.setVPPRestart(func(ctx context.Context) error {
		if s.vpp == nil {
			return fmt.Errorf("VPP 控制未接入")
		}
		cfg, err := e.Committed()
		if err != nil {
			return err
		}
		return s.vpp.Restart(ctx, cfg.Vpp)
	})
	// M4-12：CLI `request … console` 复用 console 端点同一 ticket 表（ws 桥接与审计同源）
	s.cliExec.issueConsole = func(vm, user string) (string, int, error) {
		tok, ttl, err := s.consoleTix.issue(vm, user)
		if err != nil {
			return "", 0, err
		}
		return fmt.Sprintf("%s/virtual-machine-functions/%s/console/ws?ticket=%s", APIPrefix, vm, tok), ttl, nil
	}
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
	mux.Handle("GET "+APIPrefix+"/configuration", s.auth(s.handleGetConfiguration, schema.ClassReadOnly, "show configuration"))
	mux.Handle("GET "+APIPrefix+"/configuration/candidate", cfgAPI(s.handleGetCandidate))
	mux.Handle("PUT "+APIPrefix+"/configuration/candidate", cfgAPI(s.handlePutCandidate))
	mux.Handle("DELETE "+APIPrefix+"/configuration/candidate", cfgAPI(s.handleDeleteCandidate))
	mux.Handle("POST "+APIPrefix+"/configuration/commit", cfgAPI(s.handleCommit))
	mux.Handle("POST "+APIPrefix+"/configuration/commit:confirm", cfgAPI(s.handleCommitConfirm))
	mux.Handle("GET "+APIPrefix+"/configuration/diff", cfgAPI(s.handleDiff))
	mux.Handle("POST "+APIPrefix+"/configuration/check", cfgAPI(s.handleCheck))
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
	// M4-6：快照（列表/创建/回滚/删除）
	mux.Handle("GET "+APIPrefix+"/virtual-machine-functions/{name}/snapshots", s.auth(s.handleListSnapshots, schema.ClassReadOnly, "show virtual-machine-functions"))
	mux.Handle("POST "+APIPrefix+"/virtual-machine-functions/{name}/snapshots", s.auth(s.handleCreateSnapshot, schema.ClassSuperUser, "request virtual-machine-functions snapshot create"))
	mux.Handle("POST "+APIPrefix+"/virtual-machine-functions/{name}/snapshots/{tail...}", s.auth(s.dispatchSnapshotPost, schema.ClassSuperUser, "request virtual-machine-functions snapshot"))
	mux.Handle("DELETE "+APIPrefix+"/virtual-machine-functions/{name}/snapshots/{snapshot}", s.auth(s.handleDeleteSnapshot, schema.ClassSuperUser, "request virtual-machine-functions snapshot delete"))

	// M4-7：容器 VNF（FR-CMP-020~022）
	mux.Handle("GET "+APIPrefix+"/container-functions", s.auth(s.handleListContainers, schema.ClassReadOnly, "show container-functions"))
	mux.Handle("GET "+APIPrefix+"/container-functions/{name}", s.auth(s.handleGetContainer, schema.ClassReadOnly, "show container-functions"))
	mux.Handle("POST "+APIPrefix+"/container-functions", cfgAPI(s.handlePostContainer))
	mux.Handle("DELETE "+APIPrefix+"/container-functions/{name}", cfgAPI(s.handleDeleteContainer))
	mux.Handle("GET "+APIPrefix+"/container-functions/{name}/logs", s.auth(s.handleContainerLogs, schema.ClassReadOnly, "request container-functions log"))
	mux.Handle("POST "+APIPrefix+"/container-functions/{tail...}", s.auth(s.dispatchContainerPost, schema.ClassSuperUser, "request container-functions"))

	// M4-8：镜像仓库（FR-CMP-030~033）
	mux.Handle("GET "+APIPrefix+"/images", s.auth(s.handleListImages, schema.ClassReadOnly, "show images"))
	mux.Handle("GET "+APIPrefix+"/images/{name}", s.auth(s.handleGetImage, schema.ClassReadOnly, "show images"))
	mux.Handle("POST "+APIPrefix+"/images", s.auth(s.handlePostImage, schema.ClassSuperUser, "request images"))
	mux.Handle("DELETE "+APIPrefix+"/images/{name}", s.auth(s.handleDeleteImage, schema.ClassSuperUser, "request images delete"))

	// M3-8：告警列表（恢复收敛的不可收敛项落点，FR-OPS-010）
	mux.Handle("GET "+APIPrefix+"/alarms", s.auth(s.handleGetAlarms, schema.ClassReadOnly, "show alarms"))
	// M5-9：清除已 resolved 告警（FR-OPS-022）
	mux.Handle("POST "+APIPrefix+"/alarms:clear", cfgAPI(s.handleClearAlarms))
	// M5-1：事件流（SSE，FR-API-006 / FR-OPS-022）
	mux.Handle("GET "+APIPrefix+"/events", s.auth(s.handleEvents, schema.ClassReadOnly))
	// M5-2：Prometheus 指标（契约 security: []，无鉴权，FR-SYS-005）
	mux.Handle("GET "+APIPrefix+"/metrics", http.HandlerFunc(s.handleMetrics))
	// V1 收尾：OpenAPI 规范运行时副本（契约 security: []，无鉴权，FR-API-002/决策 #69）
	mux.Handle("GET "+APIPrefix+"/openapi.json", http.HandlerFunc(s.handleOpenAPISpec))
	// 决策 #115：Web 控制面增量 1（只读总览）——免构建前端随二进制内嵌、同源托管。
	// 无鉴权（与 /metrics、/openapi.json 同例）：静态资源不含敏感信息，且登录页必须先能加载；
	// 数据由页面经既有 REST 端点带 Bearer 取。放在 /api/v1 下是为了落进契约守护（见 ui.go）。
	mux.Handle("GET "+APIPrefix+"/ui", http.HandlerFunc(s.handleUIRedirect))
	mux.Handle("GET "+APIPrefix+"/ui/", http.HandlerFunc(s.handleUIAssets))

	// M5-6：配置备份/恢复/恢复出厂（FR-OPS-004~007）
	mux.Handle("GET "+APIPrefix+"/system/backup", s.auth(s.handleListBackups, schema.ClassReadOnly, "show system backup"))
	mux.Handle("POST "+APIPrefix+"/system/backup", cfgAPI(s.handleCreateBackup))
	mux.Handle("GET "+APIPrefix+"/system/backup/{file}", s.auth(s.handleDownloadBackup, schema.ClassReadOnly, "show system backup"))
	mux.Handle("POST "+APIPrefix+"/system/restore", cfgAPI(s.handleRestore))
	mux.Handle("POST "+APIPrefix+"/system:zeroize", cfgAPI(s.handleZeroize))

	// M5-3：数据面抓包（FR-OPS-042）
	mux.Handle("GET "+APIPrefix+"/vpp/capture", s.auth(s.handleGetCapture, schema.ClassReadOnly, "show vpp capture"))
	mux.Handle("POST "+APIPrefix+"/vpp/capture", cfgAPI(s.handlePostCapture))
	mux.Handle("DELETE "+APIPrefix+"/vpp/capture", cfgAPI(s.handleDeleteCapture))
	mux.Handle("GET "+APIPrefix+"/vpp/capture/{file}", s.auth(s.handleDownloadCapture, schema.ClassReadOnly, "show vpp capture"))

	// M5-8：TLS 证书（FR-SYS-011）
	mux.Handle("GET "+APIPrefix+"/system/tls", s.auth(s.handleGetTLS, schema.ClassReadOnly, "show system"))
	mux.Handle("PUT "+APIPrefix+"/system/tls", cfgAPI(s.handlePutTLS))
	mux.Handle("POST "+APIPrefix+"/system/tls:regenerate", cfgAPI(s.handlePostTLSRegenerate))

	// M5-5：硬件健康与阈值（FR-SYS-012）
	mux.Handle("GET "+APIPrefix+"/system/hardware", s.auth(s.handleGetHardware, schema.ClassReadOnly, "show system hardware"))
	mux.Handle("GET "+APIPrefix+"/system/health/thresholds", s.auth(s.handleGetHealthThresholds, schema.ClassReadOnly, "show system health"))
	mux.Handle("PUT "+APIPrefix+"/system/health/thresholds", cfgAPI(s.handlePutHealthThresholds))

	// M5-7：软件升级/回退、电源、NTP（FR-OPS-001~003）
	mux.Handle("POST "+APIPrefix+"/system/software", cfgAPI(s.handlePostSoftware))
	mux.Handle("POST "+APIPrefix+"/system/software:rollback", cfgAPI(s.handlePostSoftwareRollback))
	mux.Handle("POST "+APIPrefix+"/system:reboot", cfgAPI(s.handleSystemPower("reboot")))
	mux.Handle("POST "+APIPrefix+"/system:shutdown", cfgAPI(s.handleSystemPower("shutdown")))
	mux.Handle("POST "+APIPrefix+"/system/ntp:sync", cfgAPI(s.handlePostNTPSync))

	// M5-4：诊断归档与 core dump（FR-OPS-040/041）
	mux.Handle("GET "+APIPrefix+"/system/tech-support", s.auth(s.handleListTechSupport, schema.ClassReadOnly, "show system tech-support"))
	mux.Handle("POST "+APIPrefix+"/system/tech-support", cfgAPI(s.handleCreateTechSupport))
	mux.Handle("GET "+APIPrefix+"/system/tech-support/{file}", s.auth(s.handleDownloadTechSupport, schema.ClassReadOnly, "show system tech-support"))
	mux.Handle("GET "+APIPrefix+"/system/core-dumps", s.auth(s.handleListCoreDumps, schema.ClassReadOnly, "show system core-dumps"))
	mux.Handle("DELETE "+APIPrefix+"/system/core-dumps", cfgAPI(s.handleDeleteCoreDumps))

	// 资源 handlers 第一组（GET = show 等级 R；写 = configure 等级 S；FR-API-003 映射）
	mux.Handle("GET "+APIPrefix+"/system", s.auth(s.handleGetSystem, schema.ClassReadOnly, "show system"))
	mux.Handle("PUT "+APIPrefix+"/system", cfgAPI(s.handlePutSystem))
	mux.Handle("GET "+APIPrefix+"/interfaces", s.auth(s.handleGetInterfaces, schema.ClassReadOnly, "show interfaces"))
	mux.Handle("GET "+APIPrefix+"/interfaces/{name}", s.auth(s.handleGetInterface, schema.ClassReadOnly, "show interfaces"))
	mux.Handle("PUT "+APIPrefix+"/interfaces/{name}", cfgAPI(s.handlePutInterface))
	mux.Handle("PUT "+APIPrefix+"/interfaces/{name}/sriov", s.auth(s.handlePutSRIOV, schema.ClassSuperUser, "set interfaces sriov"))
	mux.Handle("PUT "+APIPrefix+"/interfaces/{name}/dpdk", s.auth(s.handlePutDPDK, schema.ClassSuperUser, "request interfaces bind-dpdk"))
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
// listener 创建监听套接字，并按 committed `system.api.max_sessions` 施加并发连接上限
// （FR-SYS-006；0/未设置 = 不限）。此前 max-sessions 在命令树与契约中均有声明，
// 但无任何代码使用它——属「声明了但无实现」（决策 #71）。
func (s *Server) listener(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if n := s.maxSessions(); n > 0 {
		s.log.Info("API 并发连接上限已启用", "max_sessions", n, "addr", addr)
		return netutil.LimitListener(ln, n), nil
	}
	return ln, nil
}

// maxSessions 读取 committed `system.api.max_sessions`（不可读/未设置 = 0 = 不限）。
func (s *Server) maxSessions() int {
	if s.engine == nil {
		return 0
	}
	cfg, err := s.engine.Committed()
	if err != nil || cfg.System == nil || cfg.System.API == nil {
		return 0
	}
	if cfg.System.API.MaxSessions < 0 {
		return 0
	}
	return cfg.System.API.MaxSessions
}

func (s *Server) ListenAndServe() error {
	ln, err := s.listener(s.http.Addr)
	if err != nil {
		return err
	}
	if s.tlsCert != "" && s.tlsKey != "" {
		s.log.Info("API 服务启动（HTTPS）", "addr", s.http.Addr)
		// FR-SYS-011：GetCertificate 每次握手读盘 → 换证/重签（PUT /system/tls 或
		// request system api tls regenerate）无需重启即时生效；读盘失败回退启动时加载的证书。
		certPath, keyPath := s.tlsCert, s.tlsKey
		var fallback *tls.Certificate
		if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
			fallback = &c
		}
		s.http.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12, // FR-SEC-004：TLS 1.2+
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
					return &c, nil
				}
				if fallback != nil {
					return fallback, nil
				}
				return nil, fmt.Errorf("加载证书 %s 失败", certPath)
			},
		}
		return s.http.ServeTLS(ln, "", "")
	}
	s.log.Info("API 服务启动（HTTP，仅限开发/测试）", "addr", s.http.Addr)
	return s.http.Serve(ln)
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
