package api

// CLI 命令执行器（api/cli_bridge 的守护进程侧，骨架 §2 internal/api/cli_bridge.go）。
//
// 语义：nfvis-cli 本地用 schema 包做 ?/Tab 补全（§3.3 编译期共享，不依赖守护进程
// 存活），命令行经 POST /cli/execute 转发到此执行——命令树与执行器必须同源
//（AGENTS.md 常见错误第 1 条）。
//
// set/delete 语句按 schema 树驱动对 candidate 做变更（语句→模型执行期翻译）：
// 关键字 → 对象/数组容器（连字符转下划线）；实例参数 → 具名数组元素
//（身份字段见 identityFields）；取值叶子 → 叶子赋值（类型不符由反序列化校验兜底）。
// 权限逐命令校验：schema 节点 RequiredClass × aaa.Authorize（FR-SEC-002）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/events"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/schema"
	"github.com/xzjt/nfvis/internal/state"
	ksys "github.com/xzjt/nfvis/internal/system"
)

// authorizer 授权接口（aaa.Service 实现）。
type authorizer interface {
	Authorize(cfgClass string, required schema.Class, path ...string) bool
}

// identityFields 具名数组创建元素时的身份字段（与 model.Flatten 键控一致，默认 name）。
var identityFields = map[string]string{
	"ports":         "seq",
	"routes":        "prefix",
	"hugepages":     "page_size",
	"ntp":           "server",
	"numa":          "node",
	"rules":         "seq",
	"l3_interfaces": "interface",
	"per_dev":       "interface",
}

// CLIEResult 单条命令的执行结果。
type CLIEResult struct {
	Output string   `json:"output"`
	Mode   string   `json:"mode"`
	Path   []string `json:"path"`
	Prompt string   `json:"prompt"`
	// Console 非空表示本次命令要求 CLI 前端接管终端并桥接串口
	// （M4-12；FR-CMP-014）。前端经 pkg/cliclient.DialConsole 连 Console.WSURL，
	// Ctrl-] 退出后恢复行编辑。
	Console *ConsoleRequest `json:"console,omitempty"`
}

// ConsoleRequest 串口终端接管请求（CLI 前端与守护进程间的接管约定）。
type ConsoleRequest struct {
	VM    string `json:"vm"`
	WSURL string `json:"ws_url"`
}

// cliExecutor 守护进程侧 CLI 执行器。会话（模式/层级）按持有者+接入源隔离。
type cliExecutor struct {
	engine *config.Engine
	authz  authorizer
	diag   DiagRuntime        // 诊断命令（M3-9；nil = 报不可用）
	state  *state.State       // 接口计数快照（monitor；nil = 报不可用）
	l2     L2Runtime          // L2 运行态（mac-table；nil = 报未接入）
	l3     L3Runtime          // L3 运行态（routes；nil = 报未接入）
	lldp   LldpRuntime        // LLDP 邻居（nil = 报未接入）
	natRT  NatSessionsRuntime // NAT 会话（nil = 报未接入）
	alarms AlarmRuntime       // 告警表（nil = 报未接入）
	// 计算/容器/镜像运行态（M4-12；nil = 对应命令报未接入，与 HTTP 端点 503 一致）
	vm         VMRuntime
	console    VMConsoleRuntime
	snaps      VMSnapshotRuntime
	ct         ContainerRuntime
	images     ImagesRuntime
	ports      PortInventory               // 运行态端口清单（决策 #83；nil = show 空态不列端口）
	vppState   VppStateRuntime             // VPP 运行态快照（决策 #84；nil = 相关 show 报未接入）
	vpp        VppController               // VPP 连接管理器（show vpp 的版本/待重启；发现 #11）
	sys        SystemOpsRuntime            // 备份/恢复/恢复出厂（M5-6；nil = 命令报未接入）
	diagOps    DiagOpsRuntime              // 诊断归档/core dump（M5-4；nil = 命令报未接入）
	logs       func() ([]byte, error)      // 系统日志来源（show log system，M5-9；nil = 报不可用）
	capture    CaptureRuntime              // 数据面抓包（M5-3；nil = 报未接入）
	sw         SoftwareRuntime             // 软件升级/电源/NTP（M5-7；nil = 报未接入）
	hw         HardwareRuntime             // 硬件健康（M5-5；nil = 报未接入）
	sriov      SRIOVSetter                 // SR-IOV VF 数量（M3-7；nil = 命令报未接入）
	dpdk       DPDKSetter                  // 网卡 DPDK 驱动接管（FR-NET-001，决策 #72）
	kernel     ksys.KernelApplier          // 内核启动基线落地（FR-SYS-014；nil = 命令报未接入）
	tlsR       TlsRuntime                  // 证书管理（M5-8；nil = 报未接入）
	vppRestart func(context.Context) error // request vpp restart（M5-9；nil = 报未接入）
	events     *events.Bus                 // 事件总线（M5-1；nil = 不发布）
	mu         sync.Mutex
	sess       map[string]*cliSession
	// structured 当前命令的结构化输出快照（display json/xml 用；单命令执行期内有效）
	structured any
	// consolePending 本次命令要求前端接管串口时的接管请求（单命令执行期内有效）
	consolePending *ConsoleRequest
	// issueConsole 签发 console 一次性 ticket 并返回 ws 相对路径与有效期
	// （M4-12；由 Server.New 注入，复用 handleConsoleWS 的同一 ticket 表与审计落点）
	issueConsole func(vm, user string) (wsPath string, ttl int, err error)
}

type cliSession struct {
	Mode string
	Path []string
}

func newCLIExecutor(e *config.Engine, a authorizer) *cliExecutor {
	return &cliExecutor{engine: e, authz: a, sess: map[string]*cliSession{}}
}

// setRuntime 注入诊断与运行态数据源（M3-9；Server.New 装配，测试可省略）。
// setEventBus 注入事件总线（M5-1）：CLI 直连运行态的动作不经 HTTP handler，
// 需在执行器内显式发布 vnf-state-changed（同 M4-12 的审计处理）。
func (x *cliExecutor) setEventBus(bus *events.Bus) { x.events = bus }

// setPorts 注入运行态端口清单（决策 #83）：`<ifname>` 的候选与 `show interfaces physical`
// 的空态都取自真实端口，而不是「已写进配置的接口名」。
func (x *cliExecutor) setPorts(p PortInventory) { x.ports = p }

// setVppCtl 注入 VPP 连接管理器（发现 #11：`show vpp` 的版本/连接/待重启来自它，
// 而不是 stats 运行态——stats 不可用时这三项仍然给得出）。
func (x *cliExecutor) setVppCtl(v VppController) { x.vpp = v }

// setVppState 注入 VPP 运行态快照（决策 #84）：`show virtual-switches` 的列表/成员口/计数
// 与 `show interfaces physical` 的链接状态/速率/驱动自此取运行态事实（契约 §1.1 要求）。
func (x *cliExecutor) setVppState(v VppStateRuntime) { x.vppState = v }

// setSystemOps 注入系统运维能力（M5-6 备份/恢复/恢复出厂）。
func (x *cliExecutor) setSystemOps(sys SystemOpsRuntime) { x.sys = sys }

// setDiagOps 注入诊断能力（M5-4 tech-support / core dump）。
func (x *cliExecutor) setDiagOps(d DiagOpsRuntime) { x.diagOps = d }

// setLogSource 注入系统日志来源（M5-9 show log system）。
func (x *cliExecutor) setLogSource(f func() ([]byte, error)) { x.logs = f }

// setCapture 注入抓包能力（M5-3）。
func (x *cliExecutor) setCapture(c CaptureRuntime) { x.capture = c }

// setVPPRestart 注入 VPP 重启能力（request vpp restart）。
func (x *cliExecutor) setVPPRestart(f func(context.Context) error) { x.vppRestart = f }

// setSoftware 注入软件升级/电源/NTP 能力（M5-7）。
func (x *cliExecutor) setSoftware(s SoftwareRuntime) { x.sw = s }

// setHardware 注入硬件健康采集（M5-5）。
func (x *cliExecutor) setHardware(h HardwareRuntime) { x.hw = h }

// setSRIOV 注入 SR-IOV VF 设置能力（M5-9 收尾：request sriov 命令）。
func (x *cliExecutor) setSRIOV(s SRIOVSetter) { x.sriov = s }

// setDPDK 注入网卡 DPDK 驱动接管能力（FR-NET-001，决策 #72）。
func (x *cliExecutor) setDPDK(d DPDKSetter) { x.dpdk = d }

// setKernel 注入内核基线落地器（FR-SYS-014：request system kernel apply|rollback）。
func (x *cliExecutor) setKernel(k ksys.KernelApplier) { x.kernel = k }

// setTLS 注入证书管理（M5-8）。
func (x *cliExecutor) setTLS(t TlsRuntime) { x.tlsR = t }

// publishState 发布 VNF 状态变化事件（nil 总线时静默）。
func (x *cliExecutor) publishState(kind, name, state string) {
	if x.events != nil {
		x.events.Publish(events.TypeVNFStateChanged, map[string]any{
			"resource": kind, "name": name, "state": state,
		})
	}
}

func (x *cliExecutor) setRuntime(diag DiagRuntime, st *state.State) {
	x.diag, x.state = diag, st
}

// setNetRuntime 注入网络运行态查询（mac-table/routes/邻居/NAT 会话；
// 契约 §1.1 的运行态 show 子命令，nil = 命令报“VPP 未接入”）。
func (x *cliExecutor) setNetRuntime(l2 L2Runtime, l3 L3Runtime, lldp LldpRuntime, nat NatSessionsRuntime, alarms AlarmRuntime) {
	x.l2, x.l3, x.lldp, x.natRT, x.alarms = l2, l3, lldp, nat, alarms
}

// setComputeRuntime 注入计算/容器/镜像运行态（M4-12；契约 §1.1 show 与 §1.2 request
// 的 VNF/容器/镜像命令，nil = 对应命令报“未接入”，与端点 503 语义一致）。
func (x *cliExecutor) setComputeRuntime(vm VMRuntime, console VMConsoleRuntime, snaps VMSnapshotRuntime, ct ContainerRuntime, imgs ImagesRuntime) {
	x.vm, x.console, x.snaps, x.ct, x.images = vm, console, snaps, ct, imgs
}

func promptOf(s *cliSession) string {
	if s.Mode == "config" {
		if len(s.Path) == 0 {
			return "nfvis# "
		}
		return "[edit " + strings.Join(s.Path, " ") + "] nfvis# "
	}
	return "nfvis> "
}

// Execute 执行一行命令。
func (x *cliExecutor) Execute(user, class, source, line string) CLIEResult {
	x.mu.Lock()
	defer x.mu.Unlock()
	key := user + "@" + source
	s := x.sess[key]
	if s == nil {
		s = &cliSession{Mode: "oper"}
		x.sess[key] = s
	}

	cmd, pipes, perr := splitPipes(strings.TrimSpace(line))
	x.structured = nil
	x.consolePending = nil
	var out string
	if perr != nil {
		out = "%% " + perr.Error() + "\n"
	} else if canon, cerr := x.canonicalize(s, cmd); cerr != nil {
		out = "%% " + cerr.Error() + "\n" // FR-CLI-004：歧义/未知命令在此报错并列出候选
	} else {
		out = x.dispatch(user, class, source, s, canon, cmd)
		out = x.applyPipes(out, pipes)
	}
	cur := x.sess[key]
	if cur == nil {
		cur = &cliSession{Mode: "oper"}
	}
	return CLIEResult{Output: out, Mode: cur.Mode, Path: append([]string{}, cur.Path...), Prompt: promptOf(cur), Console: x.consolePending}
}

// canonicalize 按当前模式/层级把命令 token 规整为规范关键字（FR-CLI-004：
// 无歧义前缀即可执行，与 Tab 补全同源）。set/delete/edit/show 的路径相对当前
// edit 层级解析；annotate 的注释文本与 load/save 文件名原样保留。
func (x *cliExecutor) canonicalize(s *cliSession, cmd string) ([]string, error) {
	toks := splitFieldsQuoted(cmd)
	if len(toks) == 0 {
		return nil, nil
	}
	if s.Mode != "config" {
		return schema.Canonicalize(schema.OperRoot(), toks)
	}
	head, err := schema.Canonicalize(schema.ConfigRoot(), toks[:1])
	if err != nil {
		return nil, err
	}
	switch head[0] {
	case "annotate":
		return append(head, toks[1:]...), nil // 注释文本含空格，仅规整命令字
	case "set", "delete", "edit", "show":
		// 配置模式 `show configuration ...` 委托操作模式查看 committed；
		// configuration 只在操作树建模，故先在操作树解析该前缀。
		if head[0] == "show" && len(toks) > 1 {
			if two, err := schema.Canonicalize(schema.OperRoot(), toks[:2]); err == nil &&
				len(two) == 2 && two[1] == "configuration" {
				rest, err := schema.Canonicalize(schema.OperRoot(), toks[2:])
				if err != nil {
					return nil, err
				}
				return append(two, rest...), nil
			}
		}
		base, _, err := schema.Match(schema.ConfigPathTree(), s.Path) // 层级可含身份取值
		if err != nil {
			return nil, err
		}
		rest, err := schema.Canonicalize(base, toks[1:])
		if err != nil {
			return nil, err
		}
		return append(head, rest...), nil
	default:
		return schema.Canonicalize(schema.ConfigRoot(), toks)
	}
}

func (x *cliExecutor) dispatch(user, class, source string, s *cliSession, t []string, raw string) string {
	if len(t) == 0 {
		return ""
	}
	if s.Mode == "oper" {
		return x.execOper(user, class, source, s, t)
	}
	return x.execConfig(user, class, source, s, t, raw)
}

func (x *cliExecutor) allow(class string, n *schema.Node, path ...string) bool {
	return x.authz.Authorize(class, n.RequiredClass(), path...)
}

// ---------- 操作模式 ----------

func (x *cliExecutor) execOper(user, class, source string, s *cliSession, t []string) string {
	switch t[0] {
	case "configure":
		if !x.allow(class, mustNode(schema.OperRoot(), "configure"), "configure") {
			return "%% 无权限进入配置模式（需 super-user）\n"
		}
		// FR-CFG-001：进入配置模式即取得 candidate（副本），被占用时报错
		if err := x.engine.Edit(config.Session{User: user, Source: source}); err != nil {
			return "%% " + err.Error() + "\n"
		}
		s.Mode = "config"
		s.Path = nil
		return ""
	case "exit", "quit":
		delete(x.sess, user+"@"+source)
		return ""
	case "wizard":
		// 初始化向导是 CLI 端交互编排（决策 #107）：REST/脚本路径无 TTY 不能问答，
		// 这里只给指引——交互式 nfvis-cli 在本地拦截 `wizard`，不会走到这里。
		return "%% wizard 是交互式向导，仅可在交互式 nfvis-cli 终端执行；" +
			"脚本/REST 请改用 set/request 语句（见用户手册「3.1 内核基线」）\n"
	case "show":
		return x.execOperShow(class, t[1:])
	case "ping":
		return x.execPing(class, t[1:])
	case "traceroute":
		return x.execTraceroute(class, t[1:])
	case "monitor":
		return x.execMonitor(class, t[1:])
	case "clear":
		return x.execClear(class, t[1:])
	case "request":
		return x.execRequest(user, class, source, t[1:])
	case "start":
		return x.execStart(class, source, t[1:])
	case "help":
		return x.execHelp(class, s, t[1:])
	}
	return fmt.Sprintf("%% 无效命令: %s（输入 ? 查看可用命令）\n", strings.Join(t, " "))
}

func (x *cliExecutor) execOperShow(class string, t []string) string {
	if !x.allow(class, mustNode(schema.OperRoot(), "show"), append([]string{"show"}, t...)...) {
		return "%% 无权限执行 show\n"
	}
	switch {
	case len(t) == 1 && t[0] == "version":
		return "NFViS " + VersionStr + "\n"
	case len(t) >= 1 && t[0] == "configuration":
		if len(t) >= 2 && t[1] == "compare" {
			// show configuration compare rollback <n>
			if len(t) < 4 || t[2] != "rollback" {
				return "%% 语法: show configuration compare rollback <n>\n"
			}
			n, err := strconv.Atoi(t[3])
			if err != nil {
				return "%% rollback 编号须为整数\n"
			}
			diff, err := x.engine.Compare(n)
			if err != nil {
				return "%% " + err.Error() + "\n"
			}
			if diff == "" {
				return "（无差异）\n"
			}
			return diff + "\n"
		}
		cfg, err := x.engine.Committed()
		if err != nil {
			return "%% 读取配置失败: " + err.Error() + "\n"
		}
		if len(t) >= 2 && t[1] == "candidate" {
			cand, _, err := x.engine.Candidate()
			if err != nil {
				return "%% 无活跃 candidate 会话\n"
			}
			cfg = cand
		}
		tree := toJSONTree(cfg)
		x.structured = tree
		out := RenderConfigJSON(tree)
		if out == "" {
			return "（配置为空）\n"
		}
		return out + "\n"
	case len(t) >= 1 && t[0] == "interfaces":
		return x.execShowInterfaces(t[1:])
	case len(t) >= 1 && t[0] == "port-mirroring":
		return x.execShowPortMirroring(t[1:])
	case len(t) >= 1 && t[0] == "qos":
		return x.execShowQos(t[1:])
	case len(t) >= 1 && t[0] == "vpp":
		return x.execShowVpp(t[1:])
	case len(t) >= 1 && t[0] == "lldp":
		return x.execShowLldp(t[1:])
	case len(t) >= 1 && t[0] == "virtual-switches":
		return x.execShowVSwitches(t[1:])
	case len(t) >= 1 && t[0] == "vrfs":
		return x.execShowVrfs(t[1:])
	case len(t) >= 1 && t[0] == "nat":
		return x.execShowNat(t[1:])
	case len(t) >= 1 && t[0] == "protocols":
		return x.execShowProtocols(t[1:])
	case len(t) >= 1 && t[0] == "alarms":
		return x.execShowAlarms(t[1:])
	case len(t) >= 1 && t[0] == "acls":
		return x.execShowAcls(t[1:])
	case len(t) >= 1 && t[0] == "bonds":
		return x.execShowBonds(t[1:])
	case len(t) >= 1 && t[0] == "virtual-machine-functions":
		return x.execShowVMs(t[1:])
	case len(t) >= 1 && t[0] == "container-functions":
		return x.execShowContainers(t[1:])
	case len(t) >= 1 && t[0] == "images":
		return x.execShowImages(t[1:])
	case len(t) == 1 && t[0] == "resource-pools":
		return x.execShowResourcePools()
	case len(t) >= 3 && t[0] == "system" && t[1] == "configuration" && t[2] == "sessions":
		views, err := x.engine.Sessions()
		if err != nil {
			return "%% 查询失败: " + err.Error() + "\n"
		}
		if len(views) == 0 {
			return "（无持锁会话）\n"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Holder       Acquired            Last-Activity       Dirty\n")
		for _, v := range views {
			fmt.Fprintf(&b, "%-12s %-19s %-19s %v\n", v.Holder,
				v.AcquiredAt.Format("2006-01-02 15:04"), v.LastActivity.Format("2006-01-02 15:04"), v.Dirty)
		}
		return b.String()
	}
	if len(t) >= 2 && t[0] == "system" && t[1] != "configuration" {
		return x.execShowSystemDiag(t[1:]) // M5-4/M5-9：运行态信息 / 诊断归档 / 转储
	}
	if len(t) == 1 && t[0] == "tech-support" {
		return x.execShowSystemDiag(t) // show tech-support（契约 §1.1 顶层）
	}
	if len(t) == 1 && t[0] == "users" {
		return x.execShowUsers() // show users（M5-9）
	}
	if len(t) >= 1 && t[0] == "log" {
		return x.execShowLog(t[1:]) // show log system|audit|vnf（M5-9）
	}
	if len(t) >= 2 && t[0] == "vpp" && t[1] == "capture" {
		return x.execShowVppCapture() // M5-3：抓包会话状态与已导出 pcap 清单
	}
	return "%% 该 show 命令形式未支持。可用：version | configuration [candidate|compare rollback n] | system uptime|cpu|memory|storage|hugepages|hardware|core-dumps|tech-support | users | log system|audit|vnf | interfaces [physical|management|<ifname> [detail|statistics|sriov]] | virtual-switches | vrfs | vpp [threads|buffers|memory|capture] | acls | bonds | nat | port-mirroring | qos policies | protocols lldp neighbors | lldp neighbors | alarms | virtual-machine-functions | container-functions | images | resource-pools | system configuration sessions\n"
}

// ---------- 配置模式 ----------

func (x *cliExecutor) execConfig(user, class, source string, s *cliSession, t []string, raw string) string {
	switch t[0] {
	case "annotate":
		return x.cfgAnnotate(user, source, s, raw)
	case "load":
		return x.cfgLoad(user, source, t[1:])
	case "save":
		return x.cfgSave(user, source, t[1:])
	case "set", "delete":
		return x.execSetDelete(user, source, s, t[0], t[1:])
	case "show":
		if len(t) > 1 && t[1] == "configuration" {
			// 配置模式下查看 committed（委托操作模式 show configuration）
			return x.execOperShow(class, t[1:])
		}
		return x.cfgShow(user, source, s, t[1:])
	case "commit":
		return x.cfgCommit(user, source, s, t[1:])
	case "rollback":
		return x.cfgRollback(user, source, t[1:])
	case "discard":
		if err := x.engine.Discard(config.Session{User: user, Source: source}); err != nil {
			return "%% " + err.Error() + "\n"
		}
		return "candidate 已丢弃，会话锁已释放\n"
	case "edit":
		if len(t) < 2 {
			return "%% 语法: edit <path>\n"
		}
		if _, _, err := schema.Match(schema.ConfigPathTree(), append(append([]string{}, s.Path...), t[1:]...)); err != nil {
			return "%% " + err.Error() + "\n"
		}
		s.Path = append(s.Path, t[1:]...)
		return ""
	case "up":
		if len(s.Path) > 0 {
			s.Path = s.Path[:len(s.Path)-1]
		}
		return ""
	case "top":
		s.Path = nil
		return ""
	case "exit":
		if _, dirty, err := x.engine.Candidate(); err == nil && dirty {
			return "%% 存在未提交变更，先 commit 或 discard\n"
		}
		_ = x.engine.Release(config.Session{User: user, Source: source})
		s.Mode = "oper"
		s.Path = nil
		return ""
	case "run":
		if len(t) < 2 {
			return "%% 语法: run <操作模式命令>\n"
		}
		return x.execOper(user, class, source, s, t[1:])
	}
	return fmt.Sprintf("%% 无效命令: %s（输入 ? 查看可用命令）\n", strings.Join(t, " "))
}

func (x *cliExecutor) execSetDelete(user, source string, s *cliSession, op string, stmt []string) string {
	full := append(append([]string{}, s.Path...), stmt...)
	if len(full) == 0 {
		return fmt.Sprintf("%% 语法: %s <path> [value]\n", op)
	}
	if _, _, err := schema.Match(schema.ConfigPathTree(), full); err != nil {
		return "%% " + err.Error() + "\n"
	}
	sess := config.Session{User: user, Source: source}
	if err := x.engine.Edit(sess); err != nil {
		return "%% " + err.Error() + "\n"
	}
	cfg, _, err := x.engine.Candidate()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if op == "set" {
		if err := applyStatement(&cfg, full); err != nil {
			return "%% " + err.Error() + "\n"
		}
	} else {
		if err := deleteStatement(&cfg, full); err != nil {
			return "%% " + err.Error() + "\n"
		}
	}
	if err := x.engine.UpdateCandidate(sess, cfg); err != nil {
		return "%% " + err.Error() + "\n"
	}
	if op == "set" {
		return "[ok] " + strings.Join(maskStatementTokens(full), " ") + "\n"
	}
	return "已删除 " + strings.Join(full, " ") + "（未提交）\n"
}

func (x *cliExecutor) cfgShow(user, source string, s *cliSession, args []string) string {
	cfg, _, err := x.engine.Candidate()
	if err != nil {
		return "%% 无活跃 candidate 会话（先 set 或 configure）\n"
	}
	// FR-CFG-009：非持有者会话只读——展示 committed 而非持有者的 candidate
	views, _ := x.engine.Sessions()
	if len(views) == 0 || views[0].Holder != user+"@"+source {
		cfg, _ = x.engine.Committed()
	}
	tree := toJSONTree(cfg)
	sub, err := navigateJSON(tree, append(append([]string{}, s.Path...), args...))
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	x.structured = sub
	subMap, ok := sub.(map[string]any)
	if !ok {
		return scalarStringOf(sub) + "\n"
	}
	out := RenderConfigJSON(subMap)
	if out == "" {
		return "（无匹配配置）\n"
	}
	return out + "\n"
}

func (x *cliExecutor) cfgCommit(user, source string, s *cliSession, args []string) string {
	opts := config.CommitOpts{}
	switch {
	case len(args) > 0 && args[0] == "check":
		errs, err := x.engine.CommitCheck(config.Session{User: user, Source: source})
		if err != nil {
			return "%% " + err.Error() + "\n"
		}
		if len(errs) > 0 {
			return "校验失败（未提交）:\n" + formatVErrors(errs) + "\n"
		}
		return "校验通过\n"
	case len(args) > 0 && args[0] == "confirmed":
		if len(args) >= 2 {
			minutes, err := strconv.Atoi(args[1])
			if err != nil {
				return "%% confirmed 分钟数须为整数\n"
			}
			opts.ConfirmedMinutes = minutes
		} else {
			opts.ConfirmedMinutes = 10 // FR-CFG-003 缺省 10 分钟
		}
	case len(args) > 0 && args[0] == "and-quit":
		res := x.cfgCommit(user, source, s, nil)
		s.Mode = "oper"
		s.Path = nil
		return res
	}
	res, err := x.engine.Commit(context.Background(), config.Session{User: user, Source: source}, opts)
	if err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			return "校验失败（candidate 保留）:\n" + formatVErrors(ve.Errors) + "\n"
		}
		return "%% " + err.Error() + "\n"
	}
	out := fmt.Sprintf("commit 成功 (revision %d)", res.Revision)
	if res.ConfirmedUntil != nil {
		out += fmt.Sprintf("，confirmed 模式：%s 前再次 commit 确认，否则自动回滚", res.ConfirmedUntil.Format("15:04:05"))
	}
	for _, w := range res.Warnings {
		out += "\n" + w
	}
	// 内核启动基线与配置不一致时给出明确指引（FR-SYS-014 / FR-CMP-005）
	for _, w := range x.kernelBaselineWarnings() {
		out += "\n" + w
	}
	return out + "\n"
}

func (x *cliExecutor) cfgRollback(user, source string, args []string) string {
	n := 1
	if len(args) > 0 {
		v, err := strconv.Atoi(args[0])
		if err != nil {
			return "%% rollback 编号须为整数\n"
		}
		// 发现 #16：`rollback 0` 看着像「回到当前 committed（即丢弃改动）」，实则快照编号从 1 起
		// （引擎会报「配置快照不存在」）。误用者的第一反应就是它，故直接点明该用哪个命令。
		if v == 0 {
			return "%% rollback 的快照编号从 1 起（0 不存在）；要丢弃未提交改动请用 discard\n"
		}
		n = v
	}
	sess := config.Session{User: user, Source: source}
	if err := x.engine.Edit(sess); err != nil { // 未持锁时先进入编辑态
		return "%% " + err.Error() + "\n"
	}
	if err := x.engine.Rollback(sess, n); err != nil {
		return "%% " + err.Error() + "\n"
	}
	return fmt.Sprintf("candidate 已替换为快照 %d，需 commit 生效\n", n)
}

// ---------- 语句 → 配置模型（JSON 树变更） ----------

// pruneEmptySingleton 把「被删空之后只剩空对象」的单例容器一并删掉（发现 #12(b)）。
//
// 由来：`delete system management interface` 删掉最后一个字段后留下 `system.management = {}`，
// 而空对象**不等于"没有配置"**——`show interfaces management` 会因此走另一条分支、
// 后续管理口变更还会开始要求 commit confirmed（真机残留过）。
// 放在这个合流点是因为 `delete` 有两条路径（别名表 / 通用树遍历），只修一条会漏（第一版即漏）。
func pruneEmptySingleton(tree map[string]any) {
	sys, _ := tree["system"].(map[string]any)
	if sys == nil {
		return
	}
	if mgmt, ok := sys["management"].(map[string]any); ok && len(mgmt) == 0 {
		delete(sys, "management")
	}
}

// applyStatement 按 schema 树驱动把 set 语句写入配置（语句→模型执行期翻译）。
// 先查语句别名表（CLI 嵌套与模型扁平不一致的语句），再走通用树遍历；
// Diff 兜底：语句必须真实落到模型（未映射语句会报错而非静默丢失）。
func applyStatement(cfg *model.Config, tokens []string) error {
	// 「不能单独成句的实例参数」在**派发之前**判：这条语句既可能走下面的别名表
	// （`system login user <n>` 就有专门的别名规则，会直接建出数组元素），也可能走通用
	// 树遍历，故不能只在任一条路径里拦——判据取自命令树（Node.RequireSub），单一真源。
	if err := checkRequireSub(tokens); err != nil {
		return err
	}
	if rule := matchAlias(tokens); rule != nil {
		before := *cfg
		tree := toJSONTree(*cfg)
		if err := rule.apply(tree, tokens, true); err != nil {
			return err
		}
		pruneEmptySingleton(tree)
		return commitTree(cfg, tree, before, tokens)
	}
	before := *cfg
	tree := toJSONTree(*cfg)
	if err := applyTokens(cfgPathRoot(), tree, tokens, true); err != nil {
		return err
	}
	pruneEmptySingleton(tree)
	return commitTree(cfg, tree, before, tokens)
}

// deleteStatement 按 schema 树驱动删除语句/子树。
func deleteStatement(cfg *model.Config, tokens []string) error {
	if rule := matchAlias(tokens); rule != nil {
		before := *cfg
		tree := toJSONTree(*cfg)
		if err := rule.apply(tree, tokens, false); err != nil {
			return err
		}
		pruneEmptySingleton(tree) // 发现 #12(b)：删空后不留空壳（两条路径都要走）
		return commitTree(cfg, tree, before, tokens)
	}
	before := *cfg
	tree := toJSONTree(*cfg)
	if err := applyTokens(cfgPathRoot(), tree, tokens, false); err != nil {
		return err
	}
	pruneEmptySingleton(tree)
	return commitTree(cfg, tree, before, tokens)
}

// commitTree JSON 树 → 强类型配置，并要求语句确实产生了变更。
func commitTree(cfg *model.Config, tree map[string]any, before model.Config, tokens []string) error {
	if err := fromJSONTree(tree, cfg); err != nil {
		return err
	}
	if model.Diff(before, *cfg) == "" {
		return fmt.Errorf("语句未产生配置变更（尚未映射到模型或值未变化）: %s", strings.Join(tokens, " "))
	}
	return nil
}

// ---------- 语句别名表（CLI 嵌套 ⇄ 模型扁平不一致的映射） ----------

// aliasRule 一条别名：pattern 中 "*" 匹配任意单 token；apply 对 JSON 树
// 直接落模型字段（set=true 赋值 / set=false 清除）。
type aliasRule struct {
	pattern []string
	apply   func(tree map[string]any, t []string, isSet bool) error
}

// matchAlias 在全部别名规则中按顺序匹配：先基础表（cliexec.go），再网络语句表
// （cli_aliases_net.go）。pattern 末位可用 "**" 表示匹配剩余全部 token
// （用于「一个关键字后跟不定长键值对」的语句，如 acls rule / nat rules）。
func matchAlias(tokens []string) *aliasRule {
	for _, rule := range allAliasRules() {
		if patternMatches(rule.pattern, tokens) {
			return rule
		}
	}
	return nil
}

// allAliasRules 汇总别名规则（顺序即匹配优先级）。
func allAliasRules() []*aliasRule {
	out := make([]*aliasRule, 0, len(statementAliases)+len(statementAliasesNet)+len(statementAliasesCompute)+len(statementAliasesSystem)+len(statementAliasesArray)+len(statementAliasesAuth))
	for i := range statementAliases {
		out = append(out, &statementAliases[i])
	}
	for i := range statementAliasesNet {
		out = append(out, &statementAliasesNet[i])
	}
	for i := range statementAliasesCompute {
		out = append(out, &statementAliasesCompute[i])
	}
	for i := range statementAliasesSystem {
		out = append(out, &statementAliasesSystem[i])
	}
	for i := range statementAliasesArray {
		out = append(out, &statementAliasesArray[i])
	}
	for i := range statementAliasesAuth {
		out = append(out, &statementAliasesAuth[i])
	}
	return out
}

// patternMatches 判断 pattern 与 tokens 是否匹配；"**" 只允许出现在末位。
func patternMatches(p, tokens []string) bool {
	if n := len(p); n > 0 && p[n-1] == "**" {
		if len(tokens) < n-1 {
			return false
		}
		for j := 0; j < n-1; j++ {
			if p[j] != "*" && p[j] != tokens[j] {
				return false
			}
		}
		return true
	}
	if len(p) != len(tokens) {
		return false
	}
	for j, seg := range p {
		if seg != "*" && seg != tokens[j] {
			return false
		}
	}
	return true
}

// elemByID 在具名数组 tree[arrKey] 中按身份值取元素（不存在报错）。
func elemByID(tree map[string]any, arrKey, ident string) (map[string]any, error) {
	arr, _ := tree[arrKey].([]any)
	fld := identityFields[arrKey]
	if fld == "" {
		fld = "name"
	}
	em, _ := selectElement(arr, fld, ident)
	if em == nil {
		return nil, fmt.Errorf("无匹配配置: %s", ident)
	}
	return em, nil
}

func numField(s string) (any, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil, fmt.Errorf("取值 %q 须为整数", s)
	}
	return float64(n), nil
}

var statementAliases = []aliasRule{
	// set virtual-switches <n> vlan access <vlan> → VSwitch.vlan_access
	{pattern: []string{"virtual-switches", "*", "vlan", "access", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			vs, err := elemByID(tree, "virtual_switches", t[1])
			if err != nil {
				return err
			}
			if !isSet {
				delete(vs, "vlan_access")
				return nil
			}
			v, err := numField(t[4])
			if err != nil {
				return err
			}
			vs["vlan_access"] = v
			return nil
		}},
	// set virtual-machine-functions <n> memory numa node <u> → memory.numa_node
	{pattern: []string{"virtual-machine-functions", "*", "memory", "numa", "node", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			vm, err := elemByID(tree, "virtual_machine_functions", t[1])
			if err != nil {
				return err
			}
			mem, _ := vm["memory"].(map[string]any)
			if mem == nil {
				if !isSet {
					return fmt.Errorf("无匹配配置: memory")
				}
				mem = map[string]any{}
				vm["memory"] = mem
			}
			if !isSet {
				delete(mem, "numa_node")
				return nil
			}
			v, err := numField(t[5])
			if err != nil {
				return err
			}
			mem["numa_node"] = v
			return nil
		}},
	// delete virtual-switches <n> vlan access（4 token 删除形态）
	{pattern: []string{"virtual-switches", "*", "vlan", "access"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			vs, err := elemByID(tree, "virtual_switches", t[1])
			if err != nil {
				return err
			}
			delete(vs, "vlan_access")
			return nil
		}},
	// delete virtual-machine-functions <n> memory numa node（5 token 删除形态）
	{pattern: []string{"virtual-machine-functions", "*", "memory", "numa", "node"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			vm, err := elemByID(tree, "virtual_machine_functions", t[1])
			if err != nil {
				return err
			}
			if mem, ok := vm["memory"].(map[string]any); ok {
				delete(mem, "numa_node")
			}
			return nil
		}},
	// set virtual-machine-functions <n> serial console enable → serial_console=true
	{pattern: []string{"virtual-machine-functions", "*", "serial", "console", "enable"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			vm, err := elemByID(tree, "virtual_machine_functions", t[1])
			if err != nil {
				return err
			}
			if !isSet {
				vm["serial_console"] = false
				return nil
			}
			vm["serial_console"] = true
			return nil
		}},
	// set vpp dpdk dev <ifname> [rx-queues|tx-queues|rx-descriptors|tx-descriptors <n>]
	// per-NIC 覆盖：模型字段是 per_dev 数组（决策 #18），与 CLI 的 dev 层级名不一致。
	// 同时兜住「全局默认」的 4/5-token 形式（rx-queues 等关键字），否则通用遍历会把
	// dev 当作数组容器（因 dev 下有 <ifname> 参数子节点），而模型 vpp.dpdk.dev 是**对象**
	// （VppDevDefault），导致 `cannot unmarshal array into ... VppDevDefault`。
	{pattern: []string{"vpp", "dpdk", "dev", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			if key, ok := dpdkDevDefaultKey(t[3]); ok {
				return dpdkDevDefault(tree, key, nil, isSet)
			}
			return dpdkPerDev(tree, t[3], "", nil, isSet)
		}},
	{pattern: []string{"vpp", "dpdk", "dev", "*", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			if key, ok := dpdkDevDefaultKey(t[3]); ok {
				n, err := numField(t[4])
				if err != nil {
					return err
				}
				return dpdkDevDefault(tree, key, n, isSet)
			}
			// 非全局默认关键字 → 是「单网卡单项」形式（契约 §2.9 的
			// `delete dpdk dev <ifname> <参数>`，与全局默认同为 5 token，故按关键字名区分）。
			if isSet {
				return fmt.Errorf("配置不完整，缺少取值: vpp dpdk dev %s %s", t[3], t[4])
			}
			return dpdkPerDev(tree, t[3], strings.ReplaceAll(t[4], "-", "_"), nil, false)
		}},
	{pattern: []string{"vpp", "dpdk", "dev", "*", "*", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			n, err := numField(t[5])
			if err != nil {
				return err
			}
			return dpdkPerDev(tree, t[3], strings.ReplaceAll(t[4], "-", "_"), n, isSet)
		}},
	// set system ntp server <ip|host> [prefer]（契约 §2.2）
	// 模型是对象数组 system.ntp[{server,prefer}]，CLI 多了 server 关键字层，且 prefer 是
	// **无值 flag**；通用遍历既落不到 ntp 键、也无法以 flag 结尾（会报「缺少取值」）。
	{pattern: []string{"system", "ntp", "server", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return ntpServer(tree, t[3], false, isSet)
		}},
	{pattern: []string{"system", "ntp", "server", "*", "prefer"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return ntpServer(tree, t[3], true, isSet)
		}},
	// set system dns server <ip> secondary <ip>（§2.2）
	// 通用遍历在消费完第一个 IP 后会下潜到**参数节点**，导致同级关键字 secondary 不可见
	// （报「未知语句: "secondary"」）；仅 `server <ip>` 单参形式原本可用。
	{pattern: []string{"system", "dns", "server", "*", "secondary", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return dnsServers(tree, []string{t[3], t[5]}, isSet)
		}},
	// set resource-pools cpu numa node <n> cores <core-list>（§2.6）
	// 模型是对象数组 cpu.numa[{node,cores}]，CLI 多一层 node 关键字，通用遍历会把
	// numa 写成对象（报 cannot unmarshal object into ... []model.NumaNode）。
	{pattern: []string{"resource-pools", "cpu", "numa", "node", "*", "cores", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return numaNode(tree, t[4], t[6], isSet)
		}},
	{pattern: []string{"resource-pools", "cpu", "numa", "node", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return numaNode(tree, t[4], "", isSet)
		}},
	// set virtual-switches <n> ports <seq> interface <if> [trunk vlans <list>|native <vlan>]
	{pattern: []string{"virtual-switches", "*", "ports", "*", "interface", "*", "trunk", "vlans", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return portTrunkApply(tree, t, isSet, "interface", 5)
		}},
	{pattern: []string{"virtual-switches", "*", "ports", "*", "interface", "*", "native", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return portNativeApply(tree, t, isSet, 5)
		}},
	// set virtual-switches <n> ports <seq> vnf <vm> interface <vnic> [trunk vlans <list>]
	{pattern: []string{"virtual-switches", "*", "ports", "*", "vnf", "*", "interface", "*", "trunk", "vlans", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return portTrunkApply(tree, t, isSet, "vnf", 7)
		}},
}

// portTrunkApply 端口成员 + trunk VLAN 列表（memberKind/interfaceIdx 按语句形态）。
func portTrunkApply(tree map[string]any, t []string, isSet bool, memberKind string, ifIdx int) error {
	port, err := portElem(tree, t[1], t[3])
	if err != nil {
		return err
	}
	if !isSet {
		delete(port, "trunk")
		return nil
	}
	if memberKind == "interface" {
		port["interface"] = t[ifIdx]
	} else {
		port["vnf"] = t[5]
		port["vnf_interface"] = t[7]
	}
	vlans, err := expandVlanList(t[len(t)-1])
	if err != nil {
		return err
	}
	port["trunk"] = vlans
	return nil
}

// portNativeApply 端口 native VLAN。
func portNativeApply(tree map[string]any, t []string, isSet bool, ifIdx int) error {
	port, err := portElem(tree, t[1], t[3])
	if err != nil {
		return err
	}
	if !isSet {
		delete(port, "native")
		return nil
	}
	port["interface"] = t[ifIdx]
	v, err := numField(t[len(t)-1])
	if err != nil {
		return err
	}
	port["native"] = v
	return nil
}

// portElem 取（或创建）交换机的指定序号端口元素。
func portElem(tree map[string]any, vsName, seq string) (map[string]any, error) {
	vs, err := elemByID(tree, "virtual_switches", vsName)
	if err != nil {
		return nil, err
	}
	arr, _ := vs["ports"].([]any)
	em, _ := selectElement(arr, "seq", seq)
	if em == nil {
		n, err := strconv.Atoi(seq)
		if err != nil {
			return nil, fmt.Errorf("端口序号 %q 须为整数", seq)
		}
		em = map[string]any{"seq": float64(n)}
		arr = append(arr, em)
		vs["ports"] = arr
	}
	return em, nil
}

// expandVlanList "100,200" → [100, 200]（JSON number，反序列化为 []int）。
func expandVlanList(s string) ([]any, error) {
	var out []any
	for _, part := range strings.Split(s, ",") {
		v, err := numField(strings.TrimSpace(part))
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func cfgPathRoot() *schema.Node { return schema.ConfigPathTree() }

func toJSONTree(c model.Config) map[string]any {
	b, _ := json.Marshal(&c)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// fromJSONTree JSON 树 → 强类型配置（类型不符即报错，语句被拒绝）。
func fromJSONTree(m map[string]any, c *model.Config) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	var out model.Config
	// DisallowUnknownFields：schema 关键字与模型 JSON 键不一致时（历史缺陷类型，如语句树
	// `login user`/`login class`/`password` 对应模型 `users`/`classes`/`password_hash`），
	// encoding/json 默认**静默忽略**该键 → 语句看似成功却「未产生配置变更」，比报错更难排查。
	// 这里改为显式报错，让整类「CLI 声明了但落不进模型」在第一次执行时就暴露。
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return describeTreeErr(err)
	}
	*c = out
	return nil
}

// describeTreeErr 把 JSON 解码报错翻译成可操作的中文口径（NFR-005 / 附录 A #82②）。
// 直接抛 Go 的 `json: unknown field "x"` 对用户没有价值——既没说明原因，也没给出下一步；
// 而该报错的成因几乎总是「关键字写错了位置」或「语句不完整」（如漏了实例名）。
func describeTreeErr(err error) error {
	const hint = "可用 ? 查看当前位置候选"
	if m := unknownFieldRe.FindStringSubmatch(err.Error()); m != nil {
		return fmt.Errorf("配置中不存在字段 %q（多为关键字位置有误或语句不完整；%s）", m[1], hint)
	}
	return fmt.Errorf("语句无法落到配置模型（请核对取值类型与位置；%s）: %v", hint, err)
}

var unknownFieldRe = regexp.MustCompile(`unknown field "([^"]+)"`)

func validateTreeJSON(tree map[string]any) error {
	var probe model.Config
	b, _ := json.Marshal(tree)
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields() // 同 fromJSONTree：不匹配的键必须显式报错，不得静默丢弃
	if err := dec.Decode(&probe); err != nil {
		return describeTreeErr(err)
	}
	return nil
}

// applyTokens 沿 schema 树消费 tokens，在 JSON 树上赋值（isSet）或删除。
//
// 状态机：关键字下钻区分三种容器——具名数组（有参数子节点，数组挂在其 JSON 键下）、
// 值叶子关键字（取值挂起 pendingKey，下一 token 即值）、对象容器（惰性建 map）。
// 取值后节点停留在值关键字/参数节点上（后续兄弟关键字是其子节点）。
// flag 语句（无值叶子）：当前仅 disable（映射 enabled=false，§2.3），其余经
// applyStatement 的 Diff 兜底报「未映射」。
func applyTokens(root *schema.Node, tree map[string]any, tokens []string, isSet bool) error {
	node := root
	cur := tree
	pendingArrKey := ""         // 关键字下挂具名数组，待参数 token 选择元素
	pendingKey := ""            // 值叶子关键字，待下一 token 赋值
	pendingIdentity := false    // 身份取值模式，待下一 token 选/建数组元素
	var pendingIVK *schema.Node // 身份取值关键字节点（如 page-size）
	pendingIVKSkipped := false  // 身份关键字 token 是否已被透明跳过

	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]

		// 身份取值：本 token 选/建具名数组的元素
		if pendingIdentity {
			if !pendingIVKSkipped {
				if tok != pendingIVK.Name {
					return fmt.Errorf("配置不完整: %s 后缺少 %s 的取值", pendingIVK.Name, pendingIVK.Name)
				}
				pendingIVKSkipped = true
				continue // 透明跳过身份关键字 token（如 page-size）
			}
			arr, _ := cur[pendingArrKey].([]any)
			ident := identityFields[pendingArrKey]
			if ident == "" {
				ident = "name"
			}
			elem, idx := selectElement(arr, ident, tok)
			if !isSet {
				if elem == nil {
					return fmt.Errorf("无匹配配置: %s", tok)
				}
				if i == len(tokens)-1 {
					cur[pendingArrKey] = append(arr[:idx], arr[idx+1:]...)
					return validateTreeJSON(tree)
				}
			}
			if elem == nil {
				elem = map[string]any{ident: typedScalar(tok)}
				arr = append(arr, elem)
				cur[pendingArrKey] = arr
			}
			cur = elem
			node = pendingIVK // 元素内子语句（如 count）是身份关键字的子节点
			pendingIdentity = false
			pendingArrKey = ""
			continue
		}

		// 取值挂起：本 token 即前一键头关键字的值
		if pendingKey != "" {
			if fn, ok := valueTransforms[pendingKey]; ok {
				v, err := fn(tok)
				if err != nil {
					return err
				}
				cur[pendingKey] = v
			} else {
				cur[pendingKey] = scalarForNode(node, tok)
			}
			pendingKey = ""
			// node 停留在值关键字上：后续兄弟关键字（如 page-size 下的 count）是其子节点
			if i == len(tokens)-1 {
				return validateTreeJSON(tree) // 语句结束
			}
			continue
		}

		// 0) 实例参数身份：具名数组容器关键字之后，**下一个 token 必定是实例名**，
		// 必须先于关键字匹配消费。此前该分支排在关键字匹配之后（「2b」），
		// 于是与子关键字同名的实例名会被短路成关键字、且因为 cur 仍停在祖先容器上，
		// 取值被写到祖先层级（`login user password X` → `login.password`），
		// 产出模型无法接受的树（用户只看到 `json: unknown field "password"`）。
		// 与 pendingIdentity（身份取值数组，见上）保持同一优先级。
		if p := firstParamOf(node); p != nil && pendingArrKey != "" {
			arr, _ := cur[pendingArrKey].([]any)
			ident := identityFields[pendingArrKey]
			if ident == "" {
				ident = "name"
			}
			elem, idx := selectElement(arr, ident, tok)
			if !isSet {
				if elem == nil {
					return fmt.Errorf("无匹配配置: %s", tok)
				}
				if i == len(tokens)-1 {
					cur[pendingArrKey] = append(arr[:idx], arr[idx+1:]...)
					return validateTreeJSON(tree)
				}
			}
			if elem == nil {
				elem = map[string]any{ident: typedScalar(tok)}
				arr = append(arr, elem)
				cur[pendingArrKey] = arr
			}
			cur = elem
			node = p
			pendingArrKey = ""
			if isSet && i == len(tokens)-1 {
				return validateTreeJSON(tree) // 末位实例参数即语句结束（原先误报「缺少取值」）
			}
			continue
		}

		// 1) 关键字匹配
		var child *schema.Node
		direct := false
		for _, c := range node.Children {
			if c.Kind == schema.Keyword && c.Name == tok {
				child, direct = c, true
				break
			}
		}
		if child == nil {
			// 层级回退（就近向上）：取值关键字/标量参数消费后 node 停在消费点上，而其**同级**
			// 关键字是更上层节点的子节点——如
			//   `api tls cert-file <p> key-file <p>`（key-file 与 cert-file 同级）
			//   `vmf x interfaces eth0 type memif virtual-switch vs`（与 type 同级）
			//   `vmf x interfaces eth0 virtual-switch vs mac <m> vlan <v>`（mac/vlan 再上一层）
			// schema.Node.parent 正是为「值/无子树参数消耗后的层级回退」回填的。
			// 就近匹配（先父、再祖父…）取语义上最近的关键字；cur 未随之变动，无需回退容器。
			for p := node.Parent(); p != nil; p = p.Parent() {
				for _, c := range p.Children {
					if c.Kind == schema.Keyword && c.Name == tok {
						child, node = c, p
						break
					}
				}
				if child != nil {
					break
				}
			}
		}
		if child != nil {
			// 必需的标量取值不得被同级关键字抢位（附录 A #91）：`dns server` 的首个子节点是
			// 必需的 <ip>，若直接把兄弟关键字 `secondary` 匹配掉，取值位就永远空着，语句最后会
			// 以「配置中不存在字段 "dns"」这种**指错方向**的报错收场（dns 明明是合法关键字）。
			// 只在「直接子节点命中」（非层级回退）时判，且只判 ScalarParam——实例参数的
			// 消费位置在更上面的分支处理，值叶子不受影响。
			if direct {
				if err := requireScalarBeforeKeyword(node, cur, tokens[:i], tok); err != nil {
					return err
				}
			}
			k := jsonKeyOf(child)
			// flag：disable 特例映射 enabled=false（§2.3）；其余 flag 走 Diff 兜底报错
			if !isSet && i == len(tokens)-1 && tok == "disable" {
				cur["enabled"] = false
				return validateTreeJSON(tree)
			}
			if isSet && i == len(tokens)-1 && tok == "disable" {
				cur["enabled"] = false
				return validateTreeJSON(tree)
			}
			if !isSet && i == len(tokens)-1 {
				if _, ok := cur[k]; !ok {
					return fmt.Errorf("无匹配配置: %s", tok)
				}
				delete(cur, k)
				return validateTreeJSON(tree)
			}
			if len(child.Children) > 0 && child.Children[0].IdentityValue {
				// 身份取值数组容器（如 hugepages）：首个子节点是身份取值关键字，
				// 其取值即元素身份；数组挂在本关键字的 JSON 键下
				if _, ok := cur[k]; !ok {
					if !isSet {
						return fmt.Errorf("无匹配配置: %s", tok)
					}
					cur[k] = []any{}
				}
				pendingArrKey = k
				pendingIVK = child.Children[0]
				pendingIVKSkipped = false
				pendingIdentity = true
				node = child
				continue
			}
			if fp := firstParamOf(child); fp != nil && fp.ScalarParam {
				// 标量参数关键字：透明层，取值由参数分支写入父容器
				node = child
				continue
			}
			switch {
			case firstParamOf(child) != nil: // 具名数组容器
				if _, ok := cur[k]; !ok {
					if !isSet {
						return fmt.Errorf("无匹配配置: %s", tok)
					}
					cur[k] = []any{}
				}
				pendingArrKey = k
			case singleValueOf(child) != nil: // 值叶子关键字
				pendingKey = k
			default: // 对象容器
				sub, ok := cur[k].(map[string]any)
				if !ok {
					if !isSet {
						return fmt.Errorf("无匹配配置: %s", tok)
					}
					sub = map[string]any{}
					cur[k] = sub
				}
				cur = sub
			}
			node = child
			continue
		}

		// 2a) 标量参数：取值写入父容器的标量字段（成员标量数组则追加）
		if p := firstParamOf(node); p != nil && p.ScalarParam {
			v := typedScalar(tok)
			// ScalarIsArray（SPA）：模型字段是数组，**首个取值也必须落成数组**——
			// 否则 ssh_keys/dns_servers 这类字段会先被写成字符串，与 []string 类型不符。
			if arr, ok := cur[p.ScalarJSONKey].([]any); ok {
				cur[p.ScalarJSONKey] = append(arr, v)
			} else if p.ScalarIsArray {
				cur[p.ScalarJSONKey] = []any{v}
			} else {
				cur[p.ScalarJSONKey] = v
			}
			if i == len(tokens)-1 {
				return validateTreeJSON(tree)
			}
			node = p // 后续兄弟关键字（如 vnf 下的 interface）是参数节点的子节点
			continue
		}

		// 2b) 实例参数：身份消费已前移到「0)」（必须先于关键字匹配），此处不再处理。

		return fmt.Errorf("未知语句: %q", tok)
	}

	if isSet {
		return fmt.Errorf("配置不完整，缺少取值: %s", strings.Join(tokens, " "))
	}
	return fmt.Errorf("无匹配配置: %s", strings.Join(tokens, " "))
}

func jsonKeyOf(n *schema.Node) string { return strings.ReplaceAll(n.Name, "-", "_") }

// checkRequireSub 语句若停在**标了 RequireSub 的实例参数**上，即报「语句不完整」并列出
// 该参数可用的子关键字。判据来自命令树（单一真源），在别名派发前统一判定——因为
// `system login user <n>` 这类语句是走别名表的，只拦通用遍历会漏（附录 A #90②）。
//
// 由来：`set system login user tester1` 单独成句时，CLI 回 [ok]、commit 报成功，落库却是
// {"name":"tester1"}——一个既无 password_hash 又无 class 的账号。既有口径本就要「不静默建
// 无口令账号」（决策 #82 只挡住了「用户名写成子关键字」那一类），这里把它堵全。
func checkRequireSub(tokens []string) error {
	n, _, err := schema.Match(cfgPathRoot(), tokens)
	if err != nil || n == nil || !n.RequireSub {
		return nil
	}
	// 子关键字是**该参数的兄弟**（`login user` 把 password/class 放成同级关键字，
	// 与 `login class` 把子节点挂在参数上不同），故从父层取可用关键字。
	var subs []string
	if p := n.Parent(); p != nil {
		for _, c := range p.Children {
			if c != n && c.Kind == schema.Keyword {
				subs = append(subs, c.Name)
			}
		}
	}
	if len(subs) == 0 {
		return nil
	}
	return fmt.Errorf("语句不完整: %s 之后还需指定 %s（可用 ? 查看当前位置候选）",
		strings.Join(tokens, " "), strings.Join(subs, " 或 "))
}

// requireScalarBeforeKeyword 同级关键字若要被消费，其前面**必需的标量取值**必须已经给过。
// 只判直接子节点命中且只判 ScalarParam（见调用点注释与附录 A #91）。
func requireScalarBeforeKeyword(node *schema.Node, cur map[string]any, prefix []string, tok string) error {
	for _, c := range node.Children {
		if !c.ScalarParam || c.Optional {
			continue
		}
		if v, ok := cur[c.ScalarJSONKey]; ok && !emptyScalar(v) {
			continue // 已给过取值
		}
		return fmt.Errorf("语句不完整：%s 之后需要先给 %s 取值，再跟 %s（可用 ? 查看当前位置候选）",
			strings.Join(prefix, " "), c.Name, tok)
	}
	return nil
}

// emptyScalar 判断标量取值是否算「还没给」（缺席 / 空串 / 空数组都算没给）。
func emptyScalar(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	}
	return false
}

func firstParamOf(n *schema.Node) *schema.Node {
	for _, c := range n.Children {
		if c.Kind == schema.Param {
			return c
		}
	}
	return nil
}

func singleValueOf(n *schema.Node) *schema.Node {
	for _, c := range n.Children {
		if c.Kind == schema.Value {
			return c
		}
	}
	return nil
}

// reservedChildKeyword 若 name 是 path 所指节点的**子关键字名**则返回该名，否则返回空串。
// 用于别名层拒绝「把子关键字写在实例名位置」的笔误（如 `system login user password`：
// 照单全收会静默建出一个名为 password 的无口令账号，见附录 A #82③）。
// 取自 schema 树而非硬编码——子关键字增删时守卫自动跟随。
// 注意路径树用 cfgPathRoot()（与 applyTokens 同一棵树），ConfigRoot() 是补全用的带 set 前缀版本。
func reservedChildKeyword(path []string, name string) string {
	n, _, err := schema.Match(cfgPathRoot(), path)
	if err != nil || n == nil {
		return ""
	}
	for _, c := range n.Children {
		if c.Kind == schema.Keyword && c.Name == name {
			return name
		}
	}
	return ""
}

// selectElement 在具名数组中按身份值选元素（标量序列化比对，容忍数字/字符串差异）。
func selectElement(arr []any, ident, value string) (map[string]any, int) {
	for i, e := range arr {
		if em, ok := e.(map[string]any); ok {
			if v, ok := em[ident]; ok && scalarEq(v, value) {
				return em, i
			}
		}
	}
	return nil, -1
}

func scalarEq(v any, s string) bool {
	switch x := v.(type) {
	case string:
		return x == s
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64) == s
	case bool:
		return strconv.FormatBool(x) == s
	}
	return false
}

// typedScalar 取值类型推断：true/false → bool；整数 → number；其余 → string。
// valueTransforms 特定 JSON 键的取值变换（核列表 "4-7" → 展开的 int 数组）。
var valueTransforms = map[string]func(string) (any, error){
	"isolated_cores": expandCores,
	"cores":          expandCores,
	// 内核基线（FR-SYS-014）：nmi-watchdog 需写真实 bool（JSON 目标为 *bool）
	"nmi_watchdog": func(s string) (any, error) { return boolField(s) },
	"low_latency":  func(s string) (any, error) { return boolField(s) },
	// VLAN ID：模型字段一律为 int（VnfInterface.Vlan / VSwitchPort.NativeVlan）；
	// ParamType 为 "vlan" 时 scalarForNode 会保持字符串 → 类型不符（决策 #79）。
	"vlan":   func(s string) (any, error) { return numField(s) },
	"native": func(s string) (any, error) { return numField(s) },
}

func expandCores(s string) (any, error) {
	var out []any
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, err1 := strconv.Atoi(strings.TrimSpace(lo))
			b, err2 := strconv.Atoi(strings.TrimSpace(hi))
			if err1 != nil || err2 != nil || a > b {
				return nil, fmt.Errorf("核区间 %q 不合法", part)
			}
			for x := a; x <= b; x++ {
				out = append(out, float64(x))
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("核编号 %q 不合法", part)
		}
		out = append(out, float64(n))
	}
	return out, nil
}

func typedScalar(tok string) any {
	switch tok {
	case "true":
		return true
	case "false":
		return false
	}
	if n, err := strconv.ParseInt(tok, 10, 64); err == nil {
		return float64(n)
	}
	return tok
}

// scalarForNode 依 schema 取值节点类型决定 token 的 JSON 形态：数值/布尔类型按
// 字面解析，其余类型（string/core-list/size/ip 等）即使形似数字也保持字符串——
// 否则 `set vpp cpu corelist-workers 5` 会把字符串字段写成数字（FR-SYS-008）。
func scalarForNode(n *schema.Node, tok string) any {
	v := typedScalar(tok)
	if n == nil {
		return v
	}
	if n.Kind == schema.Keyword { // 取值关键字：类型在其值子节点上
		if sv := singleValueOf(n); sv != nil {
			n = sv
		}
	}
	switch n.ParamType {
	case "", "uint", "int", "number", "bool":
		return v
	}
	if _, isNum := v.(float64); isNum {
		return tok
	}
	return v
}

// navigateJSON 按 CLI 路径 token 定位 JSON 子树（show <path>）。
func navigateJSON(tree map[string]any, path []string) (any, error) {
	cur := any(tree)
	lastKey := "" // 下钻进数组时记录其 JSON 键（元素身份字段据此选取）
	for _, tok := range path {
		switch c := cur.(type) {
		case map[string]any:
			m := c
			if v, ok := m[strings.ReplaceAll(tok, "-", "_")]; ok {
				cur = v
				lastKey = strings.ReplaceAll(tok, "-", "_")
				continue
			}
			found := false
			for k, v := range m {
				arr, isArr := v.([]any)
				if !isArr {
					continue
				}
				ident := identityFields[k]
				if ident == "" {
					ident = "name"
				}
				if elem, _ := selectElement(arr, ident, tok); elem != nil {
					cur = elem
					lastKey = k
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("无匹配配置: %s", tok)
			}
		case []any:
			ident := identityFields[lastKey]
			if ident == "" {
				ident = "name"
			}
			elem, _ := selectElement(c, ident, tok)
			if elem == nil {
				return nil, fmt.Errorf("无匹配配置: %s", tok)
			}
			cur = elem
		default:
			return nil, fmt.Errorf("路径 %q 无下层配置", tok)
		}
	}
	return cur, nil
}

// ---------- JunOS 风格渲染 ----------

// RenderConfigJSON 把配置 JSON 树渲染为 JunOS 风格层级文本。
func RenderConfigJSON(m map[string]any) string {
	var b strings.Builder
	renderMap(&b, m, 0)
	renderAnnotations(&b, m)
	return strings.TrimRight(b.String(), "\n")
}

func renderMap(b *strings.Builder, m map[string]any, depth int) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pad := strings.Repeat("    ", depth)
	for _, k := range keys {
		renderValue(b, strings.ReplaceAll(k, "_", "-"), m[k], depth, pad)
	}
}

func renderValue(b *strings.Builder, display string, v any, depth int, pad string) {
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 {
			fmt.Fprintf(b, "%s%s;\n", pad, display)
			return
		}
		fmt.Fprintf(b, "%s%s {\n", pad, display)
		renderMap(b, x, depth+1)
		fmt.Fprintf(b, "%s}\n", pad)
	case []any:
		if len(x) == 0 {
			return
		}
		allScalar := true
		for _, e := range x {
			switch e.(type) {
			case map[string]any, []any:
				allScalar = false
			}
		}
		if allScalar {
			vals := make([]string, 0, len(x))
			for _, e := range x {
				vals = append(vals, scalarStringOf(e))
			}
			fmt.Fprintf(b, "%s%s [ %s ];\n", pad, display, strings.Join(vals, " "))
			return
		}
		for _, e := range x {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if ident := identityName(em); ident != "" {
				fmt.Fprintf(b, "%s%s %s {\n", pad, display, ident)
			} else {
				fmt.Fprintf(b, "%s%s {\n", pad, display)
			}
			renderMap(b, em, depth+1)
			fmt.Fprintf(b, "%s}\n", pad)
		}
	default:
		if display == "password-hash" {
			// FR-SEC-007：口令哈希在 show 输出中脱敏
			fmt.Fprintf(b, "%s%s «已隐藏»;\n", pad, display)
			return
		}
		fmt.Fprintf(b, "%s%s %s;\n", pad, display, scalarStringOf(v))
	}
}

// identityName 具名数组元素的展示身份。
func identityName(em map[string]any) string {
	for _, k := range []string{"name", "interface", "prefix", "seq", "node", "server", "page_size"} {
		if v, ok := em[k]; ok {
			return scalarStringOf(v)
		}
	}
	return ""
}

func scalarStringOf(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprintf("%v", v)
}

// formatVErrors 校验错误列表的 CLI 展示。
func formatVErrors(verrs []model.ValidateError) string {
	var b strings.Builder
	for _, ve := range verrs {
		b.WriteString("  - " + ve.Error() + "\n")
	}
	return b.String()
}

func mustNode(root *schema.Node, names ...string) *schema.Node {
	n, err := schema.Find(root, names...)
	if err != nil {
		return root
	}
	return n
}

// dnsServers 维护 system.dns_servers（字符串标量数组）。
// 用于 `set system dns server <ip> secondary <ip>` 一条语句写入两个地址。
func dnsServers(tree map[string]any, ips []string, isSet bool) error {
	sys, _ := tree["system"].(map[string]any)
	if sys == nil {
		if !isSet {
			return fmt.Errorf("无匹配配置: system dns")
		}
		sys = map[string]any{}
		tree["system"] = sys
	}
	cur, _ := sys["dns_servers"].([]any)
	indexOf := func(ip string) int {
		for i, v := range cur {
			if s, _ := v.(string); s == ip {
				return i
			}
		}
		return -1
	}
	if !isSet {
		for _, ip := range ips {
			i := indexOf(ip)
			if i < 0 {
				return fmt.Errorf("无匹配配置: system dns server %s", ip)
			}
			cur = append(cur[:i], cur[i+1:]...)
		}
		sys["dns_servers"] = cur
		return nil
	}
	for _, ip := range ips {
		if indexOf(ip) < 0 {
			cur = append(cur, ip)
		}
	}
	sys["dns_servers"] = cur
	return nil
}

// numaNode 维护 resource_pools.cpu.numa 数组（元素 {node, cores}，契约 §2.6）。
// cores 经 expandCores 展开为 int 数组（与 isolated-cores 同源）。
func numaNode(tree map[string]any, nodeTok, coresTok string, isSet bool) error {
	rp, _ := tree["resource_pools"].(map[string]any)
	if rp == nil {
		if !isSet {
			return fmt.Errorf("无匹配配置: resource-pools")
		}
		rp = map[string]any{}
		tree["resource_pools"] = rp
	}
	cpu, _ := rp["cpu"].(map[string]any)
	if cpu == nil {
		if !isSet {
			return fmt.Errorf("无匹配配置: resource-pools cpu")
		}
		cpu = map[string]any{}
		rp["cpu"] = cpu
	}
	arr, _ := cpu["numa"].([]any)
	elem, idx := selectElement(arr, "node", nodeTok)
	if !isSet {
		if elem == nil {
			return fmt.Errorf("无匹配配置: resource-pools cpu numa node %s", nodeTok)
		}
		if coresTok == "" {
			cpu["numa"] = append(arr[:idx], arr[idx+1:]...)
			return nil
		}
		if _, ok := elem["cores"]; !ok {
			return fmt.Errorf("无匹配配置: resource-pools cpu numa node %s cores", nodeTok)
		}
		delete(elem, "cores")
		return nil
	}
	if elem == nil {
		n, err := numField(nodeTok)
		if err != nil {
			return err
		}
		elem = map[string]any{"node": n}
		arr = append(arr, elem)
		cpu["numa"] = arr
	}
	cores, err := expandCores(coresTok)
	if err != nil {
		return err
	}
	elem["cores"] = cores
	return nil
}

// ntpServer 维护 system.ntp 数组（元素 {server, prefer}，契约 §2.2）。
// prefer 为 flag：`set system ntp server <ip> prefer` 置该服务器为首选；
// `delete system ntp server <ip> prefer` 取消首选；`delete system ntp server <ip>` 删除条目。
func ntpServer(tree map[string]any, addr string, prefer, isSet bool) error {
	sys, _ := tree["system"].(map[string]any)
	if sys == nil {
		if !isSet {
			return fmt.Errorf("无匹配配置: system ntp")
		}
		sys = map[string]any{}
		tree["system"] = sys
	}
	arr, _ := sys["ntp"].([]any)
	elem, idx := selectElement(arr, "server", addr)
	if !isSet {
		if elem == nil {
			return fmt.Errorf("无匹配配置: system ntp server %s", addr)
		}
		if !prefer {
			sys["ntp"] = append(arr[:idx], arr[idx+1:]...)
			return nil
		}
		if _, ok := elem["prefer"]; !ok {
			return fmt.Errorf("无匹配配置: system ntp server %s prefer", addr)
		}
		delete(elem, "prefer")
		return nil
	}
	if elem == nil {
		elem = map[string]any{"server": addr}
		arr = append(arr, elem)
		sys["ntp"] = arr
	}
	if prefer {
		elem["prefer"] = true
	}
	return nil
}

// dpdkDevDefaultKey 判断 token 是否为 vpp.dpdk.dev 的全局默认参数关键字，
// 是则返回模型 JSON 键（per_dev 之外的对象字段）。
func dpdkDevDefaultKey(tok string) (string, bool) {
	switch tok {
	case "rx-queues", "tx-queues", "rx-descriptors", "tx-descriptors":
		return strings.ReplaceAll(tok, "-", "_"), true
	}
	return "", false
}

// dpdkDevDefault 维护 vpp.dpdk.dev 对象（全局默认）。
// 模型里 vpp.dpdk.dev 是**对象**（VppDevDefault），per-NIC 覆盖在兄弟字段 per_dev（数组）——
// 故不能套用数组容器写法（决策 #18）。
func dpdkDevDefault(tree map[string]any, key string, val any, isSet bool) error {
	vpp, _ := tree["vpp"].(map[string]any)
	if vpp == nil {
		if !isSet {
			return fmt.Errorf("无匹配配置: vpp")
		}
		vpp = map[string]any{}
		tree["vpp"] = vpp
	}
	dpdk, _ := vpp["dpdk"].(map[string]any)
	if dpdk == nil {
		if !isSet {
			return fmt.Errorf("无匹配配置: vpp dpdk")
		}
		dpdk = map[string]any{}
		vpp["dpdk"] = dpdk
	}
	dev, _ := dpdk["dev"].(map[string]any)
	if dev == nil {
		if !isSet {
			return fmt.Errorf("无匹配配置: vpp dpdk dev %s", key)
		}
		dev = map[string]any{}
		dpdk["dev"] = dev
	}
	if !isSet {
		if _, ok := dev[key]; !ok {
			return fmt.Errorf("无匹配配置: vpp dpdk dev %s", key)
		}
		delete(dev, key)
		return nil
	}
	dev[key] = val
	return nil
}

// dpdkPerDev 维护 vpp.dpdk.per_dev 数组（per-NIC 覆盖）。
func dpdkPerDev(tree map[string]any, ifname, key string, val any, isSet bool) error {
	vpp, _ := tree["vpp"].(map[string]any)
	if vpp == nil {
		if !isSet {
			return fmt.Errorf("无匹配配置: vpp")
		}
		vpp = map[string]any{}
		tree["vpp"] = vpp
	}
	dpdk, _ := vpp["dpdk"].(map[string]any)
	if dpdk == nil {
		if !isSet {
			return fmt.Errorf("无匹配配置: vpp dpdk")
		}
		dpdk = map[string]any{}
		vpp["dpdk"] = dpdk
	}
	arr, _ := dpdk["per_dev"].([]any)
	elem, idx := selectElement(arr, "interface", ifname)
	if !isSet {
		if elem == nil {
			return fmt.Errorf("无匹配配置: vpp dpdk dev %s", ifname)
		}
		if key == "" {
			dpdk["per_dev"] = append(arr[:idx], arr[idx+1:]...)
			return nil
		}
		delete(elem, key)
		return nil
	}
	if elem == nil {
		elem = map[string]any{"interface": ifname}
		arr = append(arr, elem)
		dpdk["per_dev"] = arr
	}
	if key != "" {
		elem[key] = val
	}
	return nil
}

// splitFieldsQuoted 按空白切分命令，但**尊重双引号**：引号内的空白不切分、引号不保留。
// 反斜杠转义（" 与 \\）表示字面量。
//
// 由来（决策 #79）：`set … cloud-init ssh-key <key>` 的取值是 SSH 公钥，**必然含空格**，
// 而此前用 strings.Fields 切分 → 公钥被拆成多个 token → 报「未知语句」，
// 使 FR-CMP-016 的 CLI 注入路径实际不可用。引号是用户对「这是一整个取值」的自然表达，
// 故在解析入口统一支持，而不是为每个多词取值单开别名。
func splitFieldsQuoted(s string) []string {
	var out []string
	var b strings.Builder
	inQuote, started := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\'):
			b.WriteByte(s[i+1])
			i++
			started = true
		case c == '"':
			inQuote = !inQuote
			started = true
		case (c == ' ' || c == '\t') && !inQuote:
			if started {
				out = append(out, b.String())
				b.Reset()
				started = false
			}
		default:
			b.WriteByte(c)
			started = true
		}
	}
	if started {
		out = append(out, b.String()) // 未闭合引号：按到行尾为一个 token（宽容，不静默出错）
	}
	return out
}

// maskStatementTokens 对语句回显做脱敏：把敏感关键字（password）**紧随的取值**替换为占位符。
//
// 由来（决策 #79）：`set system login user <n> password <pw>` 成功后会回显整条语句，
// 明文口令因此出现在终端输出（以及任何捕获该输出的日志/会话录制里）。
// 配置模型只存 `password_hash`，且展示层已脱敏（决策 #70）——回显也不应例外。
func maskStatementTokens(tokens []string) []string {
	out := append([]string{}, tokens...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "password" {
			out[i+1] = "«已隐藏»"
		}
	}
	return out
}
