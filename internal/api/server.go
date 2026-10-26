// Package api 实现 NFViS REST server（FR-API-001/005/007）。
//
// M2 首块：路由骨架、Bearer Token 中间件、统一错误格式（Error{code,message,
// detail[]}）、认证端点；配置事务/资源 handlers 随 M2 后续任务按 OpenAPI
// 契约逐组落地。版本化路径 /api/v1/。
package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/netutil"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/events"
	"github.com/xzjt/nfvis/internal/metricshist"
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
	Addr           string // 监听地址（默认 :443）
	TLSCert        string // TLS 证书路径（FR-API-001，HTTPS；与 TLSKey 成对）
	TLSKey         string // TLS 私钥路径；二者为空 = 明文 HTTP（仅限开发/测试）
	Log            *slog.Logger
	VPP            VppController                  // VPP 数据面控制（M3-2；nil = /vpp/* 返回 503）
	L2             L2Runtime                      // L2 运行态查询（M3-3；nil = mac-table 503）
	L3             L3Runtime                      // L3 运行态查询（M3-4；nil = routes 503）
	LLDP           LldpRuntime                    // LLDP 邻居（M3-6；nil = 503）
	State          *state.State                   // 运行态聚合（M3-7；nil = 省略运行态字段）
	SRIOV          SRIOVSetter                    // SR-IOV VF 数量（M3-7；nil = 503）
	DPDK           DPDKSetter                     // 网卡 DPDK 驱动接管（FR-NET-001，决策 #72；nil = 503）
	NetReconcile   NetReconcileRuntime            // 数据面整段收敛入口（决策 #449 扩展；内核数据面注入，nil = 不触发）
	Kernel         ksys.KernelApplier             // 内核启动基线落地（FR-SYS-014；nil = 命令报未接入）
	Hugepages      ksys.HugepagePoolSetter        // 大页池回收（FR-SYS-002，决策 #329；nil = 命令/端点报未接入）
	HugepageRoot   string                         // 大页池 sysfs 根（决策 #329；空 = "/"，测试注入临时目录）
	NAT            NatSessionsRuntime             // NAT 会话（M3-7；nil = 503）
	Alarms         AlarmRuntime                   // 告警列表（M3-8；nil = 503）
	Diag           DiagRuntime                    // CLI 诊断命令（M3-9；nil = 命令报不可用）
	VM             VMRuntime                      // VM 生命周期（M4-3；nil = 生命周期动作 503、状态省略）
	VMConsole      VMConsoleRuntime               // VM 串口 console（M4-5；nil = console 端点 503）
	VMSnapshots    VMSnapshotRuntime              // VM 快照（M4-6；nil = 快照端点 503）
	Containers     ContainerRuntime               // 容器生命周期/日志（M4-7；nil = 503）
	Images         ImagesRuntime                  // 镜像仓库（M4-8；nil = 503）
	Events         *events.Bus                    // 事件总线（M5-1；nil = /events 503）
	SysOps         SystemOpsRuntime               // 备份/恢复/恢复出厂（M5-6；nil = 503）
	DiagOps        DiagOpsRuntime                 // 诊断归档/core dump（M5-4；nil = 503）
	LogSource      func() ([]byte, error)         // 系统日志来源（M5-9 show log system；nil = 报不可用）
	Capture        CaptureRuntime                 // 数据面抓包（M5-3；nil = 503）
	Software       SoftwareRuntime                // 软件升级/电源/NTP（M5-7；nil = 503）
	Hardware       HardwareRuntime                // 硬件健康采集（M5-5；nil = 503）
	TLS            TlsRuntime                     // 证书管理（M5-8；nil = 503）
	Ports          PortInventory                  // 运行态端口清单（决策 #83；nil = 接口名无动态候选）
	VppState       VppStateRuntime                // VPP 运行态快照（决策 #84；nil = 相关 show 报未接入）
	DHCPServer     DHCPServerRuntime              // DHCP 服务器运行态读物（决策 #359；nil = 租约端点 503）
	DHCPRelay      DHCPRelayRuntime               // 内核数据面 DHCP 中继实例运行态（决策 #437；nil = 详情的 dhcp_relay_note 不出现）
	DNSProxy       DNSProxyRuntime                // 内核数据面 DNS 代理转发器运行态（决策 #439；nil = /dns/proxy 无 runtime、CLI 无运行态块）
	LLDPState      LLDPStateRuntime               // 内核数据面 LLDP 收发代理运行态（决策 #451；nil = /protocols/lldp 无 runtime、VPP 侧恒如此）
	Vxlan          VxlanRuntime                   // VXLAN 隧道运行态读物（决策 #383；nil = 读视图如实报未接入）
	Storm          StormRuntime                   // 接口风暴抑制数据面实况（决策 #385；nil = detail 块如实报未接入）
	PortSec        PortSecRuntime                 // 接口端口安全数据面实况（决策 #389；nil = detail 块如实报未接入）
	Firewall       FirewallRuntime                // 主机防火墙数据面读数（决策 #388；nil = 读视图如实报未接入）
	PCIExists      func(bdf string) (bool, error) // 通用 PCI 直通设备存在性（FR-CMP-023；nil = detail 如实说无法核对）
	Versions       VersionsRuntime                // 组件版本探测（R37-2 收口，决策 #118；nil = 只回 NFViS 版本）
	MetricsHistory *MetricsHistoryOptions         // 历史时序存储（决策 #356；nil = 未启用）
}

// MetricsHistoryOptions 历史时序存储的装配信息（决策 #356）。Store 为空时仍如实回报路径与原因。
type MetricsHistoryOptions struct {
	Store    *metricshist.Store // 历史时序库（nil = 打不开/未启用）
	Path     string             // 库文件路径（Store 为空时也如实回报）
	Disabled string             // 未启用/不可用的原因（如实回报，不编造）
}

// Server NFViS REST server。
type Server struct {
	aaa          *aaa.Service
	engine       *config.Engine
	cliExec      *cliExecutor
	vpp          VppController
	l2           L2Runtime
	l3           L3Runtime
	lldp         LldpRuntime
	state        *state.State
	vppState     VppStateRuntime        // VPP 运行态快照（决策 #84/#116：CLI show 与 REST 同源）
	dhcpSrv      DHCPServerRuntime      // DHCP 服务器运行态读物（决策 #359：租约端点与 tap 过滤）
	dhcpRelay    DHCPRelayRuntime       // 内核数据面 DHCP 中继实例运行态（决策 #437：详情注记 dhcp_relay_note）
	dnsProxy     DNSProxyRuntime        // 内核数据面 DNS 代理转发器运行态（决策 #439：GET /dns/proxy 的 runtime 块）
	lldpState    LLDPStateRuntime       // 内核数据面 LLDP 收发代理运行态（决策 #451：GET /protocols/lldp 的 runtime 块）
	vxlan        VxlanRuntime           // VXLAN 隧道运行态读物（决策 #383：GET /vxlan-tunnels 与 CLI 同源）
	versions     VersionsRuntime        // 组件版本探测（R37-2 收口，决策 #118）
	logs         func() ([]byte, error) // 服务端日志来源（决策 #123：GET /system/logs）
	diag         DiagRuntime            // 诊断命令（决策 #123：/diagnostics/* 与清零统计）
	sriov        SRIOVSetter
	dpdk         DPDKSetter
	natSessions  NatSessionsRuntime
	alarms       AlarmRuntime
	vm           VMRuntime
	vmConsole    VMConsoleRuntime
	vmSnapshots  VMSnapshotRuntime
	containers   ContainerRuntime
	images       ImagesRuntime
	ports        PortInventory // 运行态端口清单（决策 #83；候选与 show 同源）
	events       *events.Bus
	sysOps       SystemOpsRuntime
	diagOps      DiagOpsRuntime
	capture      CaptureRuntime
	software     SoftwareRuntime
	hardware     HardwareRuntime
	tlsMgr       TlsRuntime
	kernel       ksys.KernelApplier      // 内核启动基线落地（决策 #146：REST 侧与 CLI 共用同一实现）
	hugepage     ksys.HugepagePoolSetter // 大页池回收（决策 #329：REST 侧与 CLI 共用同一实现）
	hugepageRoot string                  // 大页池 sysfs 根（决策 #329；空 = "/"）
	history      *MetricsHistoryOptions  // 历史时序存储（决策 #356；nil = 未启用）
	firewall     FirewallRuntime         // 主机防火墙数据面读数（决策 #388；nil = 读视图如实报未接入）
	consoleTix   *consoleTickets
	log          *slog.Logger
	mux          *http.ServeMux
	http         *http.Server
	tlsCert      string
	tlsKey       string
	// 决策 #471：监听地址运行期重收敛（收口 R24-E）。listenMu 保护下面三字段；
	// serveErr 常驻（缓冲 ≥4），只在 ListenAndServe 主循环读取——各代 serve 退出经它上报。
	listenMu sync.Mutex
	ln       net.Listener     // 当前监听（初始与换绑后）
	lnAddr   string           // 当前实际绑定串（归一形式：如 [::]:443 → :443）
	lnGen    uint64           // 监听代际：每起一代 serve 递增；旧代退出不视为致命
	serveErr chan serveResult // 各代 serve 退出上报
	// 决策 #478：主监听之外额外监听回环 127.0.0.1:<port>，使本机 nfvis-cli 缺省可用。
	// 主监听为通配/回环时不重复绑（通配已覆盖回环、且会 EADDRINUSE）；回环**尽力而为**
	// （绑定失败只记日志，绝不影响主监听），其 serve 退出只记日志、不进 serveErr 代际通道。
	loopLn   net.Listener // 回环监听（nil = 当前无；受 listenMu 保护）
	loopAddr string       // 回环监听地址（归一形式；受 listenMu 保护）
	// listenFn 监听套接字工厂（默认 net.Listen）。测试注入以模拟「具体非回环主地址」与
	// 「回环绑定失败」——Windows 上真绑具体非回环地址不可行。nil = net.Listen。
	listenFn func(network, addr string) (net.Listener, error)
	// tlsOnce 保证 s.http.TLSConfig 在所有 serve 代（主监听与回环）之前只设定一次，
	// 避免两个 serve goroutine 并发写——TLS 语义不变（GetCertificate 仍每握手读盘）。
	tlsOnce sync.Once
}

// serveResult 一次 serve 代退出上报（gen = 该代建立时的监听代际，决策 #471）。
type serveResult struct {
	gen uint64
	err error
}

// Handler 返回根 HTTP handler（测试与嵌套装配使用）。
func (s *Server) Handler() http.Handler { return s.logRequests(s.mux) }

// New 构造 server 并注册路由。
func New(e *config.Engine, a *aaa.Service, opts Options) *Server {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	// 日志来源统一经 scrubLogSource 包裹（决策 #325）：CLI `show log system` 与
	// REST `GET /system/logs` 是**同一份** nfvisd 日志的两个出口，其中可能出现产品
	// 首启引导打印的一次性口令——三个出口（含诊断归档）共用同一份脱敏实现。
	logs := scrubLogSource(opts.LogSource)
	s := &Server{aaa: a, engine: e, cliExec: newCLIExecutor(e, a), vpp: opts.VPP, l2: opts.L2, l3: opts.L3, lldp: opts.LLDP, state: opts.State, vppState: opts.VppState, sriov: opts.SRIOV, dpdk: opts.DPDK, natSessions: opts.NAT, alarms: opts.Alarms, vm: opts.VM, vmConsole: opts.VMConsole, vmSnapshots: opts.VMSnapshots, containers: opts.Containers, images: opts.Images, ports: opts.Ports, events: opts.Events, sysOps: opts.SysOps, diagOps: opts.DiagOps, capture: opts.Capture, software: opts.Software, hardware: opts.Hardware, tlsMgr: opts.TLS, consoleTix: newConsoleTickets(), versions: opts.Versions, logs: logs, diag: opts.Diag, log: log}
	s.serveErr = make(chan serveResult, 8) // 决策 #471：常驻（缓冲 ≥4，serve 代退出不阻塞）
	s.listenFn = net.Listen                // 决策 #478：监听工厂默认实现（测试可替换）
	s.history = opts.MetricsHistory
	s.cliExec.setMetricsHistory(s.metricsHistoryView) // 决策 #356：CLI 渲染与 REST 同一读视图
	s.cliExec.setRuntime(opts.Diag, opts.State)
	s.cliExec.setPorts(opts.Ports)       // 决策 #83：show 的空态与 Tab 候选同源
	s.cliExec.setVppCtl(opts.VPP)        // 发现 #11：show vpp 的版本/连接/待重启
	s.cliExec.setVppState(opts.VppState) // 决策 #84：show 的运行态事实来源
	s.cliExec.setNetRuntime(opts.L2, opts.L3, opts.LLDP, opts.NAT, opts.Alarms)
	s.dhcpSrv = opts.DHCPServer
	s.cliExec.setDHCPServer(opts.DHCPServer) // 决策 #359：dhcp-leases 读命令与 detail 块同源
	s.dhcpRelay = opts.DHCPRelay
	s.cliExec.setDHCPRelay(opts.DHCPRelay) // 决策 #437：detail 的中继运行态注记与 REST 同源
	s.dnsProxy = opts.DNSProxy
	s.cliExec.setDNSProxy(opts.DNSProxy) // 决策 #439：show dns proxy 的运行态块与 REST runtime 同源
	s.lldpState = opts.LLDPState         // 决策 #451：GET /protocols/lldp 的运行态块（内核数据面）
	s.vxlan = opts.Vxlan
	s.cliExec.setVxlan(opts.Vxlan)        // 决策 #383：show vxlan tunnels 与 GET /vxlan-tunnels 同源
	s.cliExec.setStorm(opts.Storm)        // 决策 #385：show interfaces <if> detail 的 storm-control 块
	s.cliExec.setPortSec(opts.PortSec)    // 决策 #389：show interfaces <if> detail 的端口安全块
	s.firewall = opts.Firewall            // 决策 #388：GET /system/firewall 的数据面读数
	s.cliExec.setFirewall(s.firewallView) // 决策 #388：CLI 渲染与 REST 同一读视图
	s.cliExec.pciExists = opts.PCIExists  // FR-CMP-023：VM detail 的直通设备实测态（nil = 如实说无法核对）
	s.cliExec.setComputeRuntime(opts.VM, opts.VMConsole, opts.VMSnapshots, opts.Containers, opts.Images)
	s.cliExec.setEventBus(opts.Events) // M5-1：CLI 直连动作也发布 vnf-state-changed
	s.cliExec.setSystemOps(opts.SysOps)
	s.cliExec.setDiagOps(opts.DiagOps)
	s.cliExec.setLogSource(logs)
	s.cliExec.setVersions(opts.Versions) // R37-2 收口（决策 #118）：show version 七组件汇总
	s.cliExec.setCapture(opts.Capture)
	s.cliExec.setSoftware(opts.Software)
	s.cliExec.setHardware(opts.Hardware)
	s.cliExec.setSRIOV(opts.SRIOV)
	s.cliExec.setDPDK(opts.DPDK)
	s.cliExec.setNetReconcile(opts.NetReconcile) // 决策 #449 扩展：运维动作后的整段收敛入口
	s.cliExec.setKernel(opts.Kernel)
	s.kernel = opts.Kernel
	s.cliExec.setHugepages(opts.Hugepages) // 决策 #329：大页池回收（CLI 与 REST 共用）
	s.hugepage = opts.Hugepages
	s.hugepageRoot = opts.HugepageRoot
	s.cliExec.hugepageRoot = opts.HugepageRoot
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
	// 修 BUG-15：把「最近一次重启的生成期告警」接到 CLI 成功分支上。装配处的 VppController
	// 若实现可选读口 vppRestartWarnings 就取（内核数据面占位实现没有，不报）。
	s.cliExec.setVPPRestartWarnings(func() []string {
		if w, ok := s.vpp.(vppRestartWarnings); ok {
			return w.RestartWarnings()
		}
		return nil
	})
	// M4-12：CLI `request … console` 复用 console 端点同一 ticket 表（ws 桥接与审计同源）
	//
	// round49：把发起本条命令的会话稳定 ID 一并绑到 ticket 上——REST/Web 三条签发路径已绑，
	// 能据此在握手期复核凭据、并在吊销/登出时关闭已建会话。CLI 经 REST 桥（`/cli/execute`）
	// 执行时 callerTokenID 非空，同样受益；SSH/控制台直连没有 token（callerTokenID 为空），
	// 按空 ID 保守放行（与既有语义一致，不凭空推断「已吊销」）。
	s.cliExec.issueConsole = func(vm, user string) (string, int, error) {
		tok, ttl, err := s.consoleTix.issue(vmConsoleResource(vm), user, s.cliExec.callerTokenID)
		if err != nil {
			return "", 0, err
		}
		return fmt.Sprintf("%s/virtual-machine-functions/%s/console/ws?ticket=%s", APIPrefix, vm, tok), ttl, nil
	}
	// 决策 #358：CLI `request container-functions <n> shell` 与 REST 端点**共用同一 ticket 表**
	// （WS 桥接与审计同源；资源键前缀隔离，一张表的 ticket 开不了另一类会话）。
	s.cliExec.issueShell = func(name, user string) (string, int, error) {
		tok, ttl, err := s.consoleTix.issue(ctShellResource(name), user, s.cliExec.callerTokenID)
		if err != nil {
			return "", 0, err
		}
		return fmt.Sprintf("%s/container-functions/%s/shell/ws?ticket=%s", APIPrefix, name, tok), ttl, nil
	}
	mux := http.NewServeMux()

	// 认证（免 token，FR-API-001）
	mux.HandleFunc("POST "+APIPrefix+"/login", s.handleLogin)
	// 登录横幅（决策 #303）：**有意不进 s.auth**——横幅要在登录之前显示（Web 登录页与
	// nfvis-cli 登录提示前），与 POST /login 同级免 token。边界：handler 只回横幅文本本身，
	// 不回主机名/版本/用户等任何其他信息（登录前不给探查面）；无限流豁免，随既有中间件。
	mux.HandleFunc("GET "+APIPrefix+"/login-banner", s.handleLoginBanner)
	// 认证后端点：required class + 命令树路径（自定义 class ACL 判定用）
	mux.Handle("POST "+APIPrefix+"/logout", s.auth(s.handleLogout, schema.ClassReadOnly, "logout"))
	mux.Handle("GET "+APIPrefix+"/system/version", s.auth(s.handleVersion, schema.ClassReadOnly, "show version"))
	mux.Handle("GET "+APIPrefix+"/system/kernel", s.auth(s.handleGetKernel, schema.ClassReadOnly, "show system kernel"))
	// 决策 #146：这两条**契约里早就声明、服务端一直没注册**（幽灵端点，控制台点了如实报 404）。
	// class 与命令树/《命令全表》一致 = S（写 GRUB 启动参数、需重启生效）。
	mux.Handle("POST "+APIPrefix+"/system/kernel:apply", s.auth(s.handleKernelApply, schema.ClassSuperUser, "request system kernel apply"))
	mux.Handle("POST "+APIPrefix+"/system/kernel:rollback", s.auth(s.handleKernelRollback, schema.ClassSuperUser, "request system kernel rollback"))
	// 决策 #329：大页池三方数字（R）与空闲多余页回收（S，与 request system kernel apply 同档：
	// 都写宿主机内核侧状态；回收只动空闲页、不动在用页，但仍按 super-user 收敛入口）。
	mux.Handle("GET "+APIPrefix+"/system/hugepages", s.auth(s.handleGetHugepages, schema.ClassReadOnly, "show system hugepages"))
	mux.Handle("POST "+APIPrefix+"/system/hugepages:reclaim", s.auth(s.handleHugepagesReclaim, schema.ClassSuperUser, "request system hugepages reclaim"))

	// 配置事务（/configuration/*，configure 为 S 级权限，命令树 §4）
	cfgAPI := func(h http.HandlerFunc) http.Handler {
		return s.auth(h, schema.ClassSuperUser, "configure")
	}
	mux.Handle("GET "+APIPrefix+"/configuration", s.auth(s.handleGetConfiguration, schema.ClassReadOnly, "show configuration"))
	// 配置提交历史（决策 #142）：与 GET /configuration 同权限口径（只读查看，不涉 candidate）
	mux.Handle("GET "+APIPrefix+"/configuration/history", s.auth(s.handleConfigurationHistory, schema.ClassReadOnly, "show configuration history"))
	mux.Handle("GET "+APIPrefix+"/configuration/candidate", cfgAPI(s.handleGetCandidate))
	mux.Handle("PUT "+APIPrefix+"/configuration/candidate", cfgAPI(s.handlePutCandidate))
	mux.Handle("DELETE "+APIPrefix+"/configuration/candidate", cfgAPI(s.handleDeleteCandidate))
	mux.Handle("POST "+APIPrefix+"/configuration/commit", cfgAPI(s.handleCommit))
	mux.Handle("POST "+APIPrefix+"/configuration/commit:confirm", cfgAPI(s.handleCommitConfirm))
	mux.Handle("GET "+APIPrefix+"/configuration/diff", cfgAPI(s.handleDiff))
	mux.Handle("POST "+APIPrefix+"/configuration/check", cfgAPI(s.handleCheck))
	mux.Handle("POST "+APIPrefix+"/configuration/rollback/{n}", cfgAPI(s.handleRollback))
	mux.Handle("GET "+APIPrefix+"/system/configuration/sessions", cfgAPI(s.handleSessions))
	// 决策 #304：某 class 的生效权限视图（等价 CLI show configuration permissions <class>）。
	// R 类；边界（非 super 查他人 403、未知 class 404）单源落在 handler 调用的同一实现处。
	mux.Handle("GET "+APIPrefix+"/configuration/permissions", s.auth(s.handleGetPermissions, schema.ClassReadOnly, "show configuration permissions"))

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
	// 登录横幅管理面（决策 #303）：与用户管理同一写模式（cfgAPI 取锁 → 一次性事务直提 → 审计）
	mux.Handle("PUT "+APIPrefix+"/system/login-banner", cfgAPI(s.handlePutLoginBanner))
	mux.Handle("DELETE "+APIPrefix+"/system/login-banner", cfgAPI(s.handleDeleteLoginBanner))
	// {name}:change-password 含冒号后缀，ServeMux 通配符不支持——以 {tail...} 捕获后分发
	mux.Handle("POST "+APIPrefix+"/system/login-users/{tail...}", s.auth(s.dispatchLoginUsersPost, schema.ClassReadOnly, "request system password change"))
	// 决策 #301：活动会话 / API Token（GET ≡ show system api tokens；{id}:revoke 含冒号后缀，
	// ServeMux 通配符不支持——{tail...} 捕获后分发）。范围判定（super-user 全量 / 其他 class
	// 仅自己、404 不泄露存在性）单源落在 aaa.Service；这里只设最低 class 作纵深防御。
	mux.Handle("GET "+APIPrefix+"/system/api-tokens", s.auth(s.handleListAPITokens, schema.ClassReadOnly, "show system api tokens"))
	mux.Handle("POST "+APIPrefix+"/system/api-tokens/{tail...}", s.auth(s.dispatchAPITokensPost, schema.ClassReadOnly, "request system api token revoke"))
	mux.Handle("GET "+APIPrefix+"/system/status", s.auth(s.handleGetSystemStatus, schema.ClassReadOnly, "show system uptime"))
	// M3-2：VPP 数据面状态与重启（FR-SYS-007/009）
	mux.Handle("GET "+APIPrefix+"/vpp/status", s.auth(s.handleGetVppStatus, schema.ClassReadOnly, "show vpp"))
	mux.Handle("GET "+APIPrefix+"/vpp/config", s.auth(s.handleGetVppConfig, schema.ClassReadOnly, "show vpp"))
	mux.Handle("POST "+APIPrefix+"/vpp/restart", s.auth(s.handlePostVppRestart, schema.ClassSuperUser, "request vpp restart"))

	// 决策 #345：数据面 DNS 代理读视图（与 CLI `show dns proxy` 同源）
	mux.Handle("GET "+APIPrefix+"/dns/proxy", s.auth(s.handleGetDNSProxy, schema.ClassReadOnly, "show dns proxy"))

	// 决策 #383：VXLAN 隧道读视图（与 CLI `show vxlan tunnels` 同源；配置声明 × 数据面实况）
	mux.Handle("GET "+APIPrefix+"/vxlan-tunnels", s.auth(s.handleGetVxlanTunnels, schema.ClassReadOnly, "show vxlan tunnels"))

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
	// 决策 #485：VNC 图形查看器（Web 内置 noVNC）——凭证端点与串口 console **同权限档**，
	// ws 端点同样以一次性 ticket 鉴权；ticket 资源键隔离（vnc/ ≠ vm/）。
	mux.Handle("POST "+APIPrefix+"/virtual-machine-functions/{name}/console/vnc", s.auth(s.handleVNCTicket, schema.ClassSuperUser, "request virtual-machine-functions console"))
	mux.Handle("GET "+APIPrefix+"/virtual-machine-functions/{name}/console/vnc/ws", http.HandlerFunc(s.handleVNCWS))
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
	// 决策 #358：容器交互式终端（与 VM 串口 console 同一套 ticket + WS 管线）
	mux.Handle("POST "+APIPrefix+"/container-functions/{name}/shell", s.auth(s.handleContainerShellTicket, schema.ClassSuperUser, "request container-functions shell"))
	mux.Handle("GET "+APIPrefix+"/container-functions/{name}/shell/ws", http.HandlerFunc(s.handleContainerShellWS))

	// M4-8：镜像仓库（FR-CMP-030~033）
	mux.Handle("GET "+APIPrefix+"/images", s.auth(s.handleListImages, schema.ClassReadOnly, "show images"))
	mux.Handle("GET "+APIPrefix+"/images/{name}", s.auth(s.handleGetImage, schema.ClassReadOnly, "show images"))
	mux.Handle("POST "+APIPrefix+"/images", s.auth(s.handlePostImage, schema.ClassSuperUser, "request images"))
	mux.Handle("DELETE "+APIPrefix+"/images/{name}", s.auth(s.handleDeleteImage, schema.ClassSuperUser, "request images delete"))
	// 决策 #485：镜像启动画像读视图（与 CLI `show vm-image-profiles` 同源；写路径在配置模式）。
	mux.Handle("GET "+APIPrefix+"/vm-image-profiles", s.auth(s.handleGetVMImageProfiles, schema.ClassReadOnly, "show vm-image-profiles"))

	// M3-8：告警列表（恢复收敛的不可收敛项落点，FR-OPS-010）
	mux.Handle("GET "+APIPrefix+"/alarms", s.auth(s.handleGetAlarms, schema.ClassReadOnly, "show alarms"))
	// M5-9：清除已 resolved 告警（FR-OPS-022）
	mux.Handle("POST "+APIPrefix+"/alarms:clear", cfgAPI(s.handleClearAlarms))
	// M5-1：事件流（SSE，FR-API-006 / FR-OPS-022）
	mux.Handle("GET "+APIPrefix+"/events", s.auth(s.handleEvents, schema.ClassReadOnly))
	// M5-2：Prometheus 指标（契约 security: []，无鉴权，FR-SYS-005）
	mux.Handle("GET "+APIPrefix+"/metrics", http.HandlerFunc(s.handleMetrics))
	// 决策 #356：历史时序读视图（运维读视图，**要求 Bearer**——与 /metrics 的
	// 无鉴权是刻意的不对称：抓取方不需要历史，操作者需要）。
	mux.Handle("GET "+APIPrefix+"/metrics/history", s.auth(s.handleMetricsHistory, schema.ClassReadOnly, "show system metrics history"))
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
	// 决策 #143：下载件是完整 committed 配置（含口令哈希，恢复所必需故不脱敏），
	// 按 FR-SEC-007 的既有例外口径（0600、仅 super-user）与生成端同为 S。
	mux.Handle("GET "+APIPrefix+"/system/backup/{file}", s.auth(s.handleDownloadBackup, schema.ClassSuperUser, "show system backup"))
	mux.Handle("POST "+APIPrefix+"/system/restore", cfgAPI(s.handleRestore))
	mux.Handle("POST "+APIPrefix+"/system:zeroize", cfgAPI(s.handleZeroize))
	// 决策 #305：重置数据分区（恢复出厂数据状态，保留管理面可达）。与 zeroize 同权限档（S）。
	mux.Handle("POST "+APIPrefix+"/system:format-data", cfgAPI(s.handleFormatData))

	// 数据面抓包（FR-OPS-042）：抓包与数据面无关，端点用中立的新名 `/capture`；
	// `/vpp/capture` 保留为兼容别名（同一批 handler，语义/状态码/导出目录逐字相同）。
	mux.Handle("GET "+APIPrefix+"/capture", s.auth(s.handleGetCapture, schema.ClassReadOnly, "show capture"))
	mux.Handle("POST "+APIPrefix+"/capture", cfgAPI(s.handlePostCapture))
	mux.Handle("DELETE "+APIPrefix+"/capture", cfgAPI(s.handleDeleteCapture))
	mux.Handle("GET "+APIPrefix+"/capture/{file}", s.auth(s.handleDownloadCapture, schema.ClassReadOnly, "show capture"))
	mux.Handle("GET "+APIPrefix+"/vpp/capture", s.auth(s.handleGetCapture, schema.ClassReadOnly, "show capture"))
	mux.Handle("POST "+APIPrefix+"/vpp/capture", cfgAPI(s.handlePostCapture))
	mux.Handle("DELETE "+APIPrefix+"/vpp/capture", cfgAPI(s.handleDeleteCapture))
	mux.Handle("GET "+APIPrefix+"/vpp/capture/{file}", s.auth(s.handleDownloadCapture, schema.ClassReadOnly, "show capture"))

	// M5-8：TLS 证书（FR-SYS-011）
	mux.Handle("GET "+APIPrefix+"/system/tls", s.auth(s.handleGetTLS, schema.ClassReadOnly, "show system"))
	mux.Handle("PUT "+APIPrefix+"/system/tls", cfgAPI(s.handlePutTLS))
	mux.Handle("POST "+APIPrefix+"/system/tls:regenerate", cfgAPI(s.handlePostTLSRegenerate))
	mux.Handle("POST "+APIPrefix+"/system/ssh-host-key:regenerate",
		s.auth(s.handlePostSSHHostKeyRegenerate, schema.ClassSuperUser, "request system ssh host-key regenerate"))

	// M5-5：硬件健康与阈值（FR-SYS-012）
	mux.Handle("GET "+APIPrefix+"/system/hardware", s.auth(s.handleGetHardware, schema.ClassReadOnly, "show system hardware"))
	// 决策 #388：管理面主机防火墙读视图（与 CLI `show system firewall` 同一实现）。
	mux.Handle("GET "+APIPrefix+"/system/firewall", s.auth(s.handleGetFirewall, schema.ClassReadOnly, "show system firewall"))
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
	// 诊断视图（决策 #123）：与 CLI 同类权限
	mux.Handle("GET "+APIPrefix+"/system/logs", s.auth(s.handleSystemLogs, schema.ClassReadOnly, "show log system"))
	mux.Handle("POST "+APIPrefix+"/diagnostics/ping", s.auth(s.handlePing, schema.ClassOperator, "ping"))
	mux.Handle("POST "+APIPrefix+"/diagnostics/traceroute", s.auth(s.handleTraceroute, schema.ClassOperator, "traceroute"))
	mux.Handle("POST "+APIPrefix+"/interfaces:clear-statistics", s.auth(s.handleClearInterfaceStats, schema.ClassSuperUser, "clear interfaces statistics"))
	mux.Handle("POST "+APIPrefix+"/system/tech-support", cfgAPI(s.handleCreateTechSupport))
	// 下载保持 read-only（决策 #149）：诊断归档是**脱敏视图**（配置分节摘掉口令哈希、
	// 日志分节剥掉产品自己打印的一次性口令），故不必按秘密收紧 class——现场流程
	// 「operator 生成诊断包 → 自己下载送支持」因此不破。要**可恢复**的完整配置走配置备份
	// 导出（那条按决策 #143 是 super-user，且有意不脱敏：脱敏即无法恢复）。
	mux.Handle("GET "+APIPrefix+"/system/tech-support/{file}", s.auth(s.handleDownloadTechSupport, schema.ClassReadOnly, "show system tech-support"))
	mux.Handle("GET "+APIPrefix+"/system/core-dumps", s.auth(s.handleListCoreDumps, schema.ClassReadOnly, "show system core-dumps"))
	mux.Handle("DELETE "+APIPrefix+"/system/core-dumps", cfgAPI(s.handleDeleteCoreDumps))
	mux.Handle("POST "+APIPrefix+"/system/core-dumps:export",
		s.auth(s.handleExportCoreDumps, schema.ClassOperator, "request system core-dumps export"))

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
	// 决策 #359：DHCP 租约表（运行态；未配置 dhcp-server ⇒ 409）。
	mux.Handle("GET "+APIPrefix+"/virtual-switches/{name}/dhcp-leases", s.auth(s.handleGetDHCPLeases, schema.ClassReadOnly, "show virtual-switches"))
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
	// 决策 #396：**不加** WriteTimeout（明确登记不做，理由如下）。本 http.Server 同时承载
	// 两条**长驻流**——`GET /events`（SSE，心跳 20s、连接可能数小时）与 console/容器 shell
	// 的 WebSocket 升级；WriteTimeout 是对**整次响应写**的总时限，一旦设置，SSE 会在到期时
	// 被静默掐断（WebSocket 升级后虽由 hijack 接管、但升级前的写仍受限）。因此这里只保留
	// ReadHeaderTimeout（10s，防慢头攻击）。「dockerd/libvirt 假死时服务端不再无界挂起」的
	// 承诺改由**底座层**保证：compute.Conn.call 的上界分层（#396 ①）与 dockerClient 的内部
	// 硬上界（#396 ②）——即挂点在编排层就被有界化，不需要也不适合用连接级写超时兜底。
	s.http = &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.tlsCert, s.tlsKey = opts.TLSCert, opts.TLSKey
	return s
}

// listener 创建监听套接字，并按 committed `system.api.max_sessions` 施加并发连接上限
// （FR-SYS-006；0/未设置 = 不限）。此前 max-sessions 在命令树与契约中均有声明，
// 但无任何代码使用它——属「声明了但无实现」（决策 #71）。
func (s *Server) listener(addr string) (net.Listener, error) {
	dial := s.listenFn
	if dial == nil {
		dial = net.Listen // 防御：未经 New 构造（测试直装）时用默认实现
	}
	ln, err := dial("tcp", addr)
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

// ListenAndServe 阻塞提供服务；Shutdown 优雅退出。
//
// 决策 #471：监听按「代」管理——Rebind 换绑时关闭旧监听并起下一代 serve；旧代 serve
// 因监听被关而退出属换绑的正常产物，**不得**当作致命退出（否则 main 侧会看成服务异常）。
// 只有**当前代**的错误原样返回（保持既有语义：Shutdown 时返回 ErrServerClosed 等）。
func (s *Server) ListenAndServe() error {
	ln, err := s.listener(s.http.Addr)
	if err != nil {
		return err
	}
	if s.serveErr == nil { // 防御：未经 New 构造（测试直装）时按常驻通道初始化
		s.serveErr = make(chan serveResult, 8)
	}
	s.listenMu.Lock()
	s.startServeLocked(ln)
	s.reconcileLoopbackLocked() // 决策 #478：主监听建立后协调回环监听
	s.listenMu.Unlock()
	for {
		r := <-s.serveErr
		s.listenMu.Lock()
		cur := s.lnGen
		s.listenMu.Unlock()
		if r.gen != cur {
			// 换绑（Rebind）关旧监听导致的退出：忽略，继续等当前代
			s.log.Debug("旧监听代已退出（换绑所致，忽略）", "gen", r.gen, "current", cur, "err", r.err)
			continue
		}
		return r.err
	}
}

// serve 一代服务循环：在 ln 上提供服务，退出结果**带代际**上报（决策 #471）。
func (s *Server) serve(ln net.Listener, gen uint64) {
	s.serveErr <- serveResult{gen: gen, err: s.serveOn(ln)}
}

// startServeLocked 记录新监听并起下一代的 serve（调用方持有 listenMu）。
// lnAddr 取 ln.Addr().String() 归一——使「同一实际绑定」（如 ":443" 与 "[::]:443"）
// 在 Rebind 幂等比较中可比。
func (s *Server) startServeLocked(ln net.Listener) {
	s.ln = ln
	addr := ln.Addr().String()
	if n, err := normalizeListenAddr(addr); err == nil {
		addr = n
	}
	s.lnAddr = addr
	s.lnGen++
	gen := s.lnGen
	go s.serve(ln, gen)
}

// serveOn 在监听上提供服务（初始监听与换绑后的新监听共用）。
func (s *Server) serveOn(ln net.Listener) error {
	if s.tlsCert != "" && s.tlsKey != "" {
		// 决策 #478：主监听与回环监听共用同一 *http.Server，TLSConfig 必须在起任何 serve
		// 之前只设定一次（否则两个 serve goroutine 并发写）。闭包内仍每握手读盘 ⇔ 换证即时生效。
		s.tlsOnce.Do(func() { s.http.TLSConfig = s.buildTLSConfig() })
		s.log.Info("API 服务启动（HTTPS）", "addr", ln.Addr().String())
		return s.http.ServeTLS(ln, "", "")
	}
	s.log.Info("API 服务启动（HTTP，仅限开发/测试）", "addr", ln.Addr().String())
	return s.http.Serve(ln)
}

// buildTLSConfig 构造 TLS 配置（FR-SYS-011）：GetCertificate 每次握手读盘，换证/重签
// （PUT /system/tls 或 request system api tls regenerate）无需重启即时生效；读盘失败
// 回退启动时加载的证书。仅由 serveOn 经 tlsOnce 调用一次。
func (s *Server) buildTLSConfig() *tls.Config {
	certPath, keyPath := s.tlsCert, s.tlsKey
	var fallback *tls.Certificate
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		fallback = &c
	}
	return &tls.Config{
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
}

// Rebind 运行期换绑监听地址（决策 #471）。
//
// 提交成功后由 nfvisd 钩子调用：管理口首次声明/地址变更后，「管理面仅监听管理网卡」
// （FR-SEC-001）不再延迟到下次启动。失败安全：新地址绑不上时回退原地址继续服务并返回
// 错误（带因）供调用方如实记录；任何路径都不 panic、不退出进程，且换绑成功/回退都推进
// 监听代际，使旧代 serve 的退出不被 ListenAndServe 视为致命。
func (s *Server) Rebind(addr string) error {
	want, err := normalizeListenAddr(addr)
	if err != nil {
		return fmt.Errorf("监听地址 %q 不合法：%w", addr, err)
	}
	s.listenMu.Lock()
	defer s.listenMu.Unlock()
	if s.ln == nil {
		return fmt.Errorf("服务尚未开始监听，无法换绑为 %s", addr)
	}
	if want == s.lnAddr {
		return nil // 已是该地址：幂等，不动现有监听
	}
	old := s.lnAddr
	_ = s.ln.Close() // 先放掉旧监听（同端口换 IP 时新旧不允许并存）
	// 决策 #478：若新主监听本身会覆盖回环（通配/回环），必须先放掉回环监听——否则绑新
	// 主监听会撞上自己占着的 127.0.0.1:<port>（EADDRINUSE），换绑被误判失败。
	if _, need := loopbackListenAddr(want); !need {
		s.closeLoopbackLocked()
	}
	newLn, err := s.listener(want)
	if err != nil {
		// 回退：重新绑定原地址，保证失败时服务不中断
		back, berr := s.listener(old)
		if berr != nil {
			return fmt.Errorf("新监听地址 %s 绑定失败（%v）；回退原地址 %s 也失败：%w", addr, err, old, berr)
		}
		s.startServeLocked(back)
		s.reconcileLoopbackLocked() // 决策 #478：主地址未变，回环协调为幂等（保证状态自洽）
		return fmt.Errorf("新监听地址 %s 绑定失败（已回退 %s）：%w", addr, old, err)
	}
	s.startServeLocked(newLn)
	s.reconcileLoopbackLocked() // 决策 #478：换绑成功后按新主地址重新协调回环
	return nil
}

// BoundAddr 返回当前实际监听地址（归一形式；供日志与测试）。未开始监听时为空串。
func (s *Server) BoundAddr() string {
	s.listenMu.Lock()
	defer s.listenMu.Unlock()
	return s.lnAddr
}

// LoopbackAddr 返回当前回环监听地址（归一形式；未监听时为空串；供日志与测试）。
func (s *Server) LoopbackAddr() string {
	s.listenMu.Lock()
	defer s.listenMu.Unlock()
	return s.loopAddr
}

// loopbackListenAddr 是决策函数（纯函数，便于红-绿）：给定**归一后**的主监听地址，
// 判断是否需要额外监听回环 127.0.0.1:<port>（决策 #478）。
//
//	主监听为通配（""/0.0.0.0/:: → 归一为 ":port"）⇒ 不绑（通配已覆盖回环，重复绑会 EADDRINUSE）
//	主监听本身是回环（127.0.0.0/8、::1）⇒ 不绑
//	主监听为具体非回环 IP 字面 ⇒ 返回 "127.0.0.1:<port>"（port 取自主监听）
//
// 主机名（非 IP 字面）无法判定是否为回环，保守不绑（不猜测、避免 EADDRINUSE）；产品实际
// 经 ResolveListenAddr 收敛出的都是 IP 字面。仅接受 host:port 形式；解析失败返回不绑。
func loopbackListenAddr(mainAddr string) (string, bool) {
	host, port, err := net.SplitHostPort(mainAddr)
	if err != nil || port == "" {
		return "", false
	}
	if host == "" { // 通配（归一后的 ""、0.0.0.0、:: 都落到这里）
		return "", false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", false // 主机名：无法判定是否回环，保守不绑
	}
	if ip.IsUnspecified() || ip.IsLoopback() {
		return "", false
	}
	return net.JoinHostPort("127.0.0.1", port), true
}

// reconcileLoopbackLocked 按当前主监听地址协调回环监听（调用方持 listenMu）。
// 主监听为通配/回环 ⇒ 关闭多余回环；为具体非回环 ⇒ 幂等复用或新建。
// 回环**尽力而为**：任何失败只记日志，绝不影响主监听，也不返回错误。
func (s *Server) reconcileLoopbackLocked() {
	want, need := loopbackListenAddr(s.lnAddr)
	if !need {
		s.closeLoopbackLocked()
		return
	}
	if s.loopLn != nil && s.loopAddr == want {
		return // 已在该回环地址：幂等
	}
	s.closeLoopbackLocked() // 目标变了：先放掉旧回环
	ln, err := s.listener(want)
	if err != nil {
		// 诚实告警但绝不致命：主监听照常服务，本机 CLI 缺省暂不可用。
		s.log.Warn("回环监听绑定失败（本机 nfvis-cli 缺省将不可用，管理面服务不受影响）",
			"addr", want, "err", err)
		return
	}
	s.loopLn = ln
	s.loopAddr = want
	go s.serveLoopback(ln)
}

// closeLoopbackLocked 关闭并清空回环监听（调用方持 listenMu）。
func (s *Server) closeLoopbackLocked() {
	if s.loopLn != nil {
		_ = s.loopLn.Close()
		s.loopLn = nil
		s.loopAddr = ""
	}
}

// serveLoopback 在回环监听上提供服务（独立 goroutine，决策 #478）。
// 其退出**不进**主监听的 serveErr 代际通道——回环非必需，异常只记日志，不与主监听混淆。
func (s *Server) serveLoopback(ln net.Listener) {
	err := s.serveOn(ln)
	switch {
	case err == nil, errors.Is(err, http.ErrServerClosed), errors.Is(err, net.ErrClosed):
		s.log.Debug("回环监听已停止", "addr", ln.Addr().String())
	default:
		s.log.Warn("回环监听服务退出", "addr", ln.Addr().String(), "err", err)
	}
}

// normalizeListenAddr 归一监听地址串（决策 #471）：统一通配写法（""、0.0.0.0、:: →
// 空主机 + ":port"）、IP 字面（IPv4-mapped、IPv6 规范形）与数字端口的书写差异，
// 使「同一实际绑定」在 Rebind 幂等比较中可比。仅接受 host:port 形式；不合法返回错误
// （Rebind 直接拒绝，绝不动现有监听）。ln.Addr().String() 也经同一归一后存入 lnAddr。
func normalizeListenAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	if host != "" {
		if ip := net.ParseIP(host); ip == nil {
			host = strings.ToLower(host) // 主机名不区分大小写
		} else if ip.IsUnspecified() {
			host = "" // 0.0.0.0 / :: —— 通配的两种写法归一为「空主机」
		} else {
			host = ip.String()
		}
	}
	if n, err := strconv.Atoi(port); err == nil && n >= 0 && n <= 65535 {
		port = strconv.Itoa(n) // "0443" 与 "443" 同义
	}
	if host == "" {
		return ":" + port, nil
	}
	return net.JoinHostPort(host, port), nil
}

// Shutdown 优雅停机（决策 #478：同时关掉回环监听）。
func (s *Server) Shutdown(ctx context.Context) error {
	s.listenMu.Lock()
	s.closeLoopbackLocked()
	s.listenMu.Unlock()
	return s.http.Shutdown(ctx)
}

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
