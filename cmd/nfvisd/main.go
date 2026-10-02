// nfvisd NFViS 守护进程入口（骨架 §3.5 启动装配）。
//
// M2 首块装配：SQLite 存储 → 配置事务引擎 → AAA → REST API。
// 底座 Provider 为空实现（NewNoopApplier，骨架 §5：M2 可完整演示 CLI/API
// 事务，不含真实网络），M3/M4 替换为 govpp/libvirt/docker 编排器并接入恢复收敛。
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/api"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/events"
	"github.com/xzjt/nfvis/internal/images"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/compute"
	"github.com/xzjt/nfvis/internal/orchestrator/container"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
	"github.com/xzjt/nfvis/internal/state"
	"github.com/xzjt/nfvis/internal/system"
	"github.com/xzjt/nfvis/internal/systemd"
	"os/exec"
	"strings"
)

func main() {
	if err := run(); err != nil {
		slog.Error("nfvisd 退出", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dbPath    = flag.String("db", "nfvis.db", "SQLite 存储路径")
		listen    = flag.String("listen", ":443", "API 监听地址")
		tlsCert   = flag.String("tls-cert", "", "TLS 证书 PEM 路径（与 -tls-key 成对；缺省自动生成自签证书）")
		tlsKey    = flag.String("tls-key", "", "TLS 私钥 PEM 路径")
		plaintext = flag.Bool("allow-plaintext", false, "强制明文 HTTP（开发/测试；显式给出即忽略已装/自签证书）")
		initAdmin = flag.String("init-admin-password", "", "首次启动引导 admin 用户的口令（缺省随机生成并打印一次）")
		vppSock   = flag.String("vpp-sock", envOr("NFVIS_VPP_SOCK", network.DefaultSocket), "VPP binary API 套接字")
		showVer   = flag.Bool("version", false, "输出版本后退出")
		// 安装期内核基线生成（FR-SYS-014 / 决策 #66）：安装器调用本开关生成 GRUB 片段与
		// fstab 行，保证与 CLI（request system kernel apply）**同一生成器**，避免双源。
		printBaseline = flag.Bool("print-kernel-baseline", false, "打印内核基线（GRUB 片段 + ---FSTAB--- + fstab 行）后退出")
		printHPSysctl = flag.Bool("print-hugepage-sysctl", false, "打印大页池 sysctl 片段（默认尺寸池钉值，配合 --hugepages-1g/--hugepages-2m）后退出")
		hp1g          = flag.Int("hugepages-1g", 0, "1G 大页数量（安装期基线）")
		hp2m          = flag.Int("hugepages-2m", 0, "2M 大页数量（安装期基线）")
		isoCores      = flag.String("isolated-cores", "", "隔离核列表，如 4-15（安装期基线）")
		thp           = flag.String("thp", "", "transparent_hugepages: always|madvise|never")
		iommu         = flag.String("iommu", "", "iommu: on|off|pt")
		lowLatency    = flag.Bool("low-latency", false, "低延迟参数组（mitigations=off 等；显式选择，降低安全缓解与可诊断性；VM 上自动省略 idle=poll/tsc=reliable）")
		tuned         = flag.String("tuned-profile", "", "tuned 性能档名")
		extraParams   = flag.String("kernel-params", "", "附加内核参数（空格分隔）")
		// libvirt AppArmor 放行（决策 #182）：同一实现供安装期脚本与运行期复核调用，避免双源。
		ensureAA = flag.Bool("ensure-libvirt-apparmor", false, "确保 libvirt 的 AppArmor 助手放行 NFViS 镜像/VM 路径后退出（幂等）")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("nfvisd", api.VersionStr)
		return nil
	}
	if *printBaseline {
		pageSize, count := "", 0
		if *hp1g > 0 {
			pageSize, count = "1G", *hp1g
		} else if *hp2m > 0 {
			pageSize, count = "2M", *hp2m
		}
		var extra []string
		if strings.TrimSpace(*extraParams) != "" {
			extra = strings.Fields(*extraParams)
		}
		d := system.DesiredFromConfig(pageSize, count, *isoCores, "", *thp, *iommu, *tuned, extra)
		if pageSize == "1G" && *hp2m > 0 {
			d.Hugepages2M = *hp2m // 双池（决策 #106）：--hugepages-1g 与 --hugepages-2m 可并用
		}
		d.LowLatency = *lowLatency
		d = system.EnrichDesired(d, "/")
		// 护栏：隔离核配置不合法（把核全隔离/越界）直接拒绝——写进 GRUB 要重启才会暴露，
		// 那时已进不了系统。报错走 stderr（stdout 是安装脚本要捕获的片段）。
		if err := system.ValidateDesired(d, "/"); err != nil {
			fmt.Fprintln(os.Stderr, "nfvisd: "+err.Error())
			os.Exit(1)
		}
		frag, fstab := system.GenerateBaseline(d)
		fmt.Print(frag)
		fmt.Println("---FSTAB---")
		fmt.Print(fstab)
		if fstab != "" {
			fmt.Println()
		}
		return nil
	}

	// R88-1 / 决策 #347：大页池 sysctl 片段（作用于**即将生效的内核基线默认尺寸池**的声明值）。
	// 安装期脚本用它写 /etc/sysctl.d/90-nfvis-hugepages.conf，与运行期 Apply/启动补写共用
	// 同一生成器。默认尺寸判据取即将生效的基线（优先产品写的 GRUB 片段、回退当前 cmdline）
	// ——apply 后重启前运行 cmdline 仍是旧基线，按它取会写错池（真机 round127）；取不到时
	// 生成器返回空，脚本据此不动该文件。
	if *printHPSysctl {
		d := system.KernelDesired{
			DefaultHugepageSize: system.EffectiveDefaultHugepageSize(""),
			Hugepages1G:         *hp1g,
			Hugepages2M:         *hp2m,
		}
		fmt.Print(system.GenerateHugepageSysctl(d))
		return nil
	}

	if *ensureAA {
		changed, err := (&system.AppArmorLibvirt{Runner: func(ctx context.Context, name string, args ...string) (string, error) {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			return string(out), err
		}}).Ensure(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, "nfvisd: "+err.Error())
			return err
		}
		if changed {
			fmt.Println("nfvisd: libvirt AppArmor 已放行 NFViS 镜像/VM 路径")
		} else {
			fmt.Println("nfvisd: libvirt AppArmor 无需改动（本机未装 libvirt 或已放行）")
		}
		return nil
	}

	// FR-OPS-030 / FR-SYS-004（决策 #69）：日志级别由 committed 配置驱动（启动与每次 commit 重载）；
	// 记录照常落本地（journald/stdout），并按配置转发远程 syslog。
	logLevel := new(slog.LevelVar)
	logLevel.Set(slog.LevelInfo)
	syslogFwd := system.NewSyslogForwarder(system.SyslogConfig{})
	defer func() { _ = syslogFwd.Close() }()
	log := slog.New(system.SyslogHandler{
		Inner: slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}),
		Fwd:   syslogFwd,
		App:   "nfvisd",
	})
	slog.SetDefault(log)

	// 装配顺序即依赖顺序（骨架 §3.5）
	store, err := config.OpenStore(*dbPath)
	if err != nil {
		return fmt.Errorf("打开存储: %w", err)
	}
	defer store.Close()

	// M3：VPP 数据面连接管理（FR-SYS-007）先于事务引擎装配（引擎需要下发编排器）。
	vppMgr := network.NewManager(network.Config{Socket: *vppSock, Log: log}, nil)
	defer vppMgr.Close()
	l2Provider := network.NewL2ProviderFunc(vppMgr.L2ClientFunc())
	l3Provider := network.NewL3ProviderFunc(vppMgr.L3ClientFunc())
	netProvider := network.NewL2Network(orchestrator.NewNoopNetwork(), l2Provider)
	netProvider.SetL3(l3Provider)
	netProvider.SetServices(network.NewServicesProviderFunc(vppMgr.SvcClientFunc()))
	// 决策 #335：交换机 DHCP 中继（VPP dhcp proxy；恢复重放含 relay）
	netProvider.SetDhcp(network.NewDhcpProviderFunc(vppMgr.DhcpClientFunc()))
	// 决策 #345：数据面 DNS 代理（自研域内转发器，punt socket；恢复重放含它）
	dnsProxy := network.NewDNSProxyProviderFunc(vppMgr.PuntClientFunc())
	// 优雅退出必须注销 punt（否则留一个指向已消失 socket 的注册＝域内 DNS 黑洞）
	defer func() { _ = dnsProxy.Close() }()
	netProvider.SetDNSProxy(dnsProxy)
	netProvider.SetACL(network.NewAclProviderFunc(vppMgr.AclClientFunc()))
	netProvider.SetNAT(network.NewNatProviderFunc(vppMgr.NatClientFunc()))
	netProvider.SetBond(network.NewBondProviderFunc(vppMgr.BondClientFunc()))
	netProvider.SetLldp(network.NewLldpProviderFunc(vppMgr.LldpClientFunc()))
	// M4-4：VNF vNIC（vhost-user）接入
	netProvider.SetVhostUser(network.NewVhostUserProviderFunc(vppMgr.VhostUserClientFunc()))
	// M4-7：容器 memif 接入
	netProvider.SetMemif(network.NewMemifProviderFunc(vppMgr.MemifClientFunc()))
	// V1 收尾（决策 #70）：声明式 interfaces[].sriov.vf_count 落地（同一实例亦供 API 命令式路径）
	sriovProvider := network.NewSRIOVProvider()
	netProvider.SetSRIOV(sriovProvider)
	// FR-NET-001（决策 #72）：网卡 DPDK 驱动接管（sysfs driver_override/bind/unbind）
	dpdkBinder := network.NewDPDKBinder()
	// 决策 #100（发现 #8）：绑定记录（口名 → PCI）。口一旦交 DPDK，内核就没有它的 netdev 了，
	// 而 startup.conf 的 dev 段以 PCI 为键——记录是「绑定那一刻」留下的唯一映射来源，
	// 也是「按口名解绑」的依据。不入 committed 配置（决策 #30：PCI 不随配置走）。
	dpdkBindings := network.NewBindings(network.DefaultBindingsPath)
	dpdkBinder.Bindings = dpdkBindings
	// 迁移：把当前部署的 startup.conf 里 dev <pci> { name <口> } 的映射并入记录，
	// 使「照旧手册带外播种」的存量安装不必重绑就能转由产品维护。
	if n, err := dpdkBindings.ImportStartupConf(network.DefaultStartupPath); err != nil {
		log.Warn("导入现有 startup.conf 的 DPDK 端口映射失败（可在数据面重启前重试）", "err", err)
	} else if n > 0 {
		log.Info("已从现有 startup.conf 导入 DPDK 端口映射", "count", n, "path", dpdkBindings.Path)
	}
	// M3-8：恢复收敛的不可收敛项落点（GET /alarms）
	alarms := network.NewAlarmStore()
	// NFR-006：告警也带「记录时时钟是否已同步」三态标记，探针与审计侧取同一个
	// system.ClockSynced（单源），未注入即未知（决策 #307）。
	alarms.SetClockProbe(system.ClockSynced)
	netProvider.SetAlarms(alarms)
	// 决策 #337 判据③：成员口 rx 计数读物（复用 #326 的运行态读数路径，不新造 VPP 查询）。
	netProvider.SetCounters(vppMgr.Runtime())
	// M5-1：事件总线（FR-API-006 / FR-OPS-020~022）。所有事件源经此汇聚，
	// 由 GET /events（SSE）推送；告警变更同时进入总线。
	bus := events.New()
	alarms.SetNotifier(func(a network.Alarm) {
		typ := events.TypeAlarmRaised
		if a.State == network.AlarmResolved {
			typ = events.TypeAlarmResolved
		}
		bus.Publish(typ, map[string]any{
			"id": a.ID, "severity": a.Severity, "code": a.Code,
			"message": a.Message, "source": a.Source, "state": a.State,
		})
		// FR-OPS-022（决策 #69）：告警变更同时转发远程 syslog（未配置目标时为空操作）。
		// 转发失败不阻塞告警链路（原因经 SyslogForwarder.LastError 可查）。
		_ = syslogFwd.Forward(alarmSyslogSeverity(a.Severity), "nfvisd", "alarm",
			fmt.Sprintf("[%s] %s source=%s state=%s", a.Code, a.Message, a.Source, a.State))
	})
	// M4-3：计算编排（libvirt）。连接失败（libvirtd 未起/无权限）降级为 NoopCompute
	// 并告警，不阻塞 nfvisd 启动；此时 VM 生命周期动作返回不可用。
	var (
		computeProvider orchestrator.ComputeProvider = orchestrator.NewNoopCompute()
		vmRuntime       api.VMRuntime
		libvirtConn     *compute.Conn
	)
	computeCfg := compute.DefaultConfig()
	computeCfg.URI = envOr("NFVIS_LIBVIRT_URI", compute.DefaultURI)
	// 决策 #314：启动路径的数据面前置判定。复用连接管理器的既有状态视图（与 /vpp/status 同源），
	// 不另写探测——VPP 未连接时 start/restart(off→start) 立即失败，不进入会阻塞的 vhost-user
	// 准备阶段（round95 真机：该阶段会一直等到客户端超时且留下 paused 残域）。
	computeCfg.DataPlaneProbe = func() error {
		v := vppMgr.StatusView(nil)
		if v.Connected {
			return nil
		}
		if v.LastError != "" {
			return errors.New(v.LastError)
		}
		return fmt.Errorf("VPP 连接状态为 %s", vppMgr.State())
	}
	// vhost-user socket 目录须存在且可被 QEMU/VPP 访问（M4-P0 记录 §5）。
	if err := os.MkdirAll(computeCfg.VhostDir, 0o755); err != nil {
		log.Warn("创建 vhost-user socket 目录失败", "dir", computeCfg.VhostDir, "err", err)
	}
	// 决策 #350：启动期 libvirt 连接的有界重试。开机竞态：libvirtd「Started」后仍要先做完
	// 既有 VM 的 autostart（大页 prealloc）才服务客户端握手，单次 10s 可能连不上，而一次
	// 失败即永久降级、每次开机后 VM 编排需人工恢复。重试**只在启动装配期**：任一次成功即
	// 按既有路径接入（成功路径行为不变），最终失败仍走既有降级 + 告警（文案不变）；运行期
	// 「不自动重连」语义不变。
	var (
		p    *compute.Provider
		conn *compute.Conn
		cerr error
	)
	libvirtConnect := func() error {
		// 单次尝试各自有界（沿用既有 10s 上界）。
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p, conn, cerr = compute.NewConnectedProvider(ctx, computeCfg)
		return cerr
	}
	if err := boundedRetry(libvirtConnectAttempts, libvirtRetryBackoff, func(attempt int, err error) {
		log.Warn("libvirt 连接未就绪，稍后重试", "attempt", attempt, "err", err)
	}, libvirtConnect); err != nil {
		// 连接失败/超时（含 libvirtd 假死被 Connect 的有界等待截断）→ 降级 NoopCompute
		// 并落告警，不阻塞启动（决策 #349：降级必须可见，不能只有一行日志）。
		log.Warn("计算编排未接入（libvirt 连接失败），VM 生命周期不可用", "uri", computeCfg.URI, "err", cerr)
		computeUnavailableAlarm(alarms, computeCfg.URI, cerr)
	} else {
		p.SetVFResolver(network.NewSysfsVFResolver()) // SR-IOV VF PCI 解析（FR-NET-021）
		p.SetAlarms(alarms)                           // M4-9：计算收敛告警落点
		computeProvider, libvirtConn, vmRuntime = p, conn, p
		defer func() { _ = libvirtConn.Close() }()
		log.Info("计算编排已接入", "uri", computeCfg.URI)
	}

	// M4-7：容器编排（Docker）。连接不可用（daemon 未起/权限不足）降级为 NoopContainer。
	ctCfg := container.DefaultConfig()
	ctCfg.Socket = envOr("NFVIS_DOCKER_HOST", ctCfg.Socket)
	var containerProvider orchestrator.ContainerProvider = orchestrator.NewNoopContainer()
	var ctRuntime api.ContainerRuntime
	ctProvider := container.NewConnectedProvider(ctCfg)
	// 探测有界（决策 #349）：dockerClient 的请求构造已全程 NewRequestWithContext，
	// 但调用方此前传的是 Background ⇒ dockerd 假死同样永久卡启动。只改调用方 ctx
	//（10s 上界），**不给共享 http.Client 加整体超时** —— 镜像导入等长操作共用
	// 同一客户端，全局超时会截断它们。
	ctProbeCtx, ctProbeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	st, perr := ctProvider.ContainerState(ctProbeCtx, "__nfvis_probe__")
	ctProbeCancel()
	if perr == nil || st != "" {
		ctProvider.SetAlarms(alarms) // M4-9：容器收敛告警落点
		containerProvider, ctRuntime = ctProvider, ctProvider
		log.Info("容器编排已接入", "socket", ctCfg.Socket)
	} else {
		log.Warn("容器编排未接入（Docker 连接失败），容器生命周期不可用", "socket", ctCfg.Socket, "err", perr)
		containerUnavailableAlarm(alarms, ctCfg.Socket, perr)
	}
	if err := os.MkdirAll(ctCfg.MemifDir, 0o755); err != nil {
		log.Warn("创建 memif socket 目录失败", "dir", ctCfg.MemifDir, "err", err)
	}

	// M4-8：镜像仓库（本地目录 + index.json；容器镜像删除经 Docker）
	imagesStore, ierr := images.Open(images.DefaultConfig())
	if ierr != nil {
		log.Warn("镜像仓库初始化失败", "err", ierr)
	} else {
		imagesStore.SetDockerRemover(func(ref string) error {
			return ctProvider.RemoveImage(context.Background(), ref)
		})
		// 容器镜像导入：docker save 归档经 `image load` 入 Docker 分层存储（FR-CMP-030/031）。
		imagesStore.SetDockerLoader(func(path, name string) error {
			return ctProvider.LoadImage(context.Background(), path, name)
		})
		// M5-1：镜像导入进度/状态事件
		imagesStore.SetProgressSink(func(name string, written, total int64) {
			bus.Publish(events.TypeImageImportProgress, map[string]any{
				"name": name, "written": written, "total": total,
			})
		})
		imagesStore.SetStateSink(func(name, typ, state string) {
			bus.Publish(events.TypeImageImportProgress, map[string]any{
				"name": name, "type": typ, "state": state,
			})
		})
	}

	netProvider.SetSocketDirs(computeCfg.VhostDir, ctCfg.MemifDir)
	applier := orchestrator.NewApplier(netProvider, computeProvider, containerProvider,
		orchestrator.WithVhostDir(computeCfg.VhostDir), orchestrator.WithMemifDir(ctCfg.MemifDir),
		// 非致命处置（如「已声明 DPDK 口尚未进数据面，本次延后收敛」）必须让操作者看得到：
		// 提交仍然回 [ok]，只靠 CLI 是看不出来的，故至少落日志（决策 #100）。
		orchestrator.WithWarn(func(msg string) { log.Warn(msg) }),
		// 提交期残渣/延后处置落告警（决策 #191/#192）：补偿未完成的残渣、以及「NAT 用过的表
		// 要等数据面重启才消失」都必须事后查得到——它们此前只出现在当次提交输出里。
		orchestrator.WithCommitAlarms(alarms))

	// 系统命令执行器（软件/证书/日志保留共用；与 SoftwareManager 一致带超时）
	runCmd := func(ctx context.Context, name string, args ...string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(cctx, name, args...).CombinedOutput()
		return string(out), err
	}

	// M5-8 / FR-SEC-004（决策 #72）：证书管理（FR-SYS-011）。未显式给 -tls-cert 时：
	// 已装管理证书 → 直接用；否则**自动生成自签证书**（FR-API-001「REST over HTTPS（自签证书，可换）」）。
	// 仅显式 -allow-plaintext（开发/测试）才退化为明文——此前缺省即明文，与规格相反。
	tlsMgr := system.NewTLSManager("", runCmd)
	// 自签证书的 SAN 由管理端按本机监听地址统一推导（决策 #99）；此处先按命令行给的监听地址，
	// 监听地址收敛后（下方 ResolveListenAddr）再更新一次。
	tlsMgr.SetListen(*listen)
	// -allow-plaintext 是**权威开关**：显式给出即走明文（即便磁盘上已有自签证书）。
	// 否则「已存在证书」会让该开关看起来无效——真机验证时即踩到：带 -allow-plaintext
	// 启动却仍以 HTTPS 服务，明文客户端全被拒。
	if *tlsCert == "" && !*plaintext {
		if _, ok := tlsMgr.Info(); ok {
			*tlsCert, *tlsKey = tlsMgr.CertPath(), tlsMgr.KeyPath()
			log.Info("使用已安装的管理证书启用 HTTPS", "cert", *tlsCert)
		} else {
			info, generated, err := tlsMgr.EnsureSelfSigned(hostnameOr("nfvis"))
			switch {
			case err != nil:
				log.Error("自动生成自签证书失败——API 将以明文提供，请立即用 set system api tls 安装证书", "err", err)
			case generated:
				*tlsCert, *tlsKey = tlsMgr.CertPath(), tlsMgr.KeyPath()
				log.Info("未提供证书，已自动生成自签证书并启用 HTTPS",
					"cert", *tlsCert, "fingerprint", info.Fingerprint)
			default:
				*tlsCert, *tlsKey = tlsMgr.CertPath(), tlsMgr.KeyPath()
			}
		}
	}
	if *tlsCert == "" {
		log.Warn("API 以明文 HTTP 提供服务（-allow-plaintext）：仅限开发/测试；生产请安装证书或用自签")
	}

	var eng *config.Engine // 供 OnCommitted 回调引用（NewEngine 之后赋值）
	var engineOpts config.Options
	// NFR-006：每条审计记录都带上「写入时宿主时钟是否已同步」的标记
	engineOpts.TimeSynced = system.ClockSynced
	if imagesStore != nil {
		engineOpts.ImageResolver = imagesStore // FR-CFG-011⑤：镜像存在性与类型匹配
	}
	// M5-1：commit 成功事件（config-committed）
	engineOpts.OnCommitted = func(revision int, user string) {
		bus.Publish(events.TypeConfigCommitted, map[string]any{"revision": revision, "user": user})
		// M5-8：提交后落实证书与日志保留策略（尽力而为，不阻塞 commit）
		go func() {
			if eng == nil {
				return
			}
			cfg, err := eng.Committed()
			if err != nil {
				return
			}
			applyTLSSettings(cfg, tlsMgr, log)
			// V1 收尾（决策 #69）：日志级别与远程 syslog 转发随配置热更新
			applySyslogSettings(cfg, logLevel, syslogFwd, log)
			if cfg.System != nil && cfg.System.Syslog != nil {
				if path, err := system.ApplyLogRetention(context.Background(), runCmd,
					cfg.System.Syslog.RetentionDays, cfg.System.Syslog.MaxSizeMB); err != nil {
					log.Warn("应用日志保留策略失败", "err", err)
				} else if path != "" {
					log.Info("日志保留策略已应用", "dropin", path)
				}
			}
		}()
	}
	engine, err := config.NewEngine(store, applier, engineOpts)
	if err != nil {
		return fmt.Errorf("装配事务引擎: %w", err)
	}
	eng = engine
	defer engine.Close()

	// FR-OPS-030 / FR-SYS-004（决策 #69）：按 committed 配置初始化日志级别与远程转发
	if cfg, err := engine.Committed(); err == nil {
		applySyslogSettings(cfg, logLevel, syslogFwd, log)
		// FR-SEC-001（决策 #72）：管理面仅监听管理网卡——通配监听收敛到管理口地址
		if addr, note := system.ResolveListenAddr(*listen, mgmtAddressOf(cfg), system.LocalAddrChecker()); note != "" {
			log.Info(note, "listen", addr)
			*listen = addr
			tlsMgr.SetListen(addr) // 收敛后的地址才是实际监听地址（决策 #99：证书 SAN 随之）
		}
	} else {
		log.Warn("读取 committed 配置失败，日志级别与远程转发采用缺省", "err", err)
	}

	// 管理口守卫事实（发现 #7，决策 #101）：装配层只负责给事实，判定在 network 包（可单测）。
	// 三条事实与 model 的 checkManagementIsolation 同源（都用 system.management.interface）——
	// 那条管的是"配置里不得把管理口用于数据面"，这里管的是"运行态动作不得动管理口"。
	mgmtFacts := func() network.ManagementFacts {
		f := network.ManagementFacts{DefaultRouteIface: network.DefaultRouteIfaceOf(network.DefaultRoutePath)}
		if cfg, err := engine.Committed(); err == nil && cfg.System != nil && cfg.System.Management != nil {
			f.DeclaredMgmtIface = cfg.System.Management.Interface
		}
		if host, _, err := net.SplitHostPort(*listen); err == nil && host != "" &&
			host != "0.0.0.0" && host != "::" {
			f.ListenIface = network.IfaceOfIP(host)
		}
		return f
	}
	{
		f := mgmtFacts()
		log.Info("管理口守卫事实（绑定/解绑这些口会被拒）",
			"declared", f.DeclaredMgmtIface, "default-route", f.DefaultRouteIface, "listen", f.ListenIface)
	}

	// M5-6：配置备份/恢复/恢复出厂（FR-OPS-004~007）
	sysOps := system.NewManager(system.DefaultConfig(), engine, imagesStore, api.VersionStr)

	// M5-3：数据面抓包（VPP pcap trace 经 CLI socket；FR-OPS-042）
	// 注意：vppctl -s 需 CLI socket（/run/vpp/cli.sock），不是二进制 API socket；
	// 传空由 vppctl 取缺省（同 diagController 的做法）。
	captureProvider := network.NewCaptureProvider(network.NewVppctlShell(""),
		func(name string) (uint32, bool, error) {
			c, err := vppMgr.L2ClientFunc()()
			if err != nil {
				return 0, false, err
			}
			defer c.Close()
			return c.SwInterfaceIndex(name)
		}, network.DefaultCaptureDir)

	// M5-7：软件升级/回退、电源、NTP（FR-OPS-001~003）
	swMgr := system.NewSoftwareManager(system.DefaultSoftwareDir,
		func(ctx context.Context, name string, args ...string) (string, error) {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			out, err := exec.CommandContext(cctx, name, args...).CombinedOutput()
			return string(out), err
		}, func() string { return api.VersionStr })

	// M5-5：硬件健康采集（FR-SYS-012）——BMC/IPMI 优先，降级 lm-sensors → /sys/class/thermal；磁盘 SMART
	hwProvider := system.NewHardwareProvider(func(ctx context.Context, name string, args ...string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, name, args...).CombinedOutput()
		return string(out), err
	})

	// M5-4：诊断归档（tech-support）与 core dump 管理（FR-OPS-040/041）
	coreDumps := system.NewCoreDumps("", 0)
	techSupport := system.NewTechSupport("", system.TechSupportSources{
		Version: func() any {
			host, _ := os.Hostname()
			view := vppMgr.StatusView(nil)
			return map[string]any{
				"nfvis": api.VersionStr, "hostname": host,
				"vpp": view.Version, "vpp_connected": view.Connected,
				"kernel": kernelRelease(),
			}
		},
		Config: func() (any, error) { return engine.Committed() },
		Audit:  func() (any, error) { return engine.AuditTrail(500, 0) },
		Status: func() (any, error) { return vppMgr.StatusView(nil), nil },
		Logs:   nfvisdLogTail,
		Cores:  coreDumps.List,
	}, api.VersionStr)

	aaaSvc := aaa.NewService(engine, nil)

	// 首次启动引导（附录 A #25）：无本地用户时创建 admin，随机口令仅打印一次
	created, oneTime, err := aaa.EnsureBootstrapAdmin(engine, aaaSvc, *initAdmin)
	if err != nil {
		return fmt.Errorf("初始化本地用户: %w", err)
	}
	if created && oneTime != "" {
		fmt.Printf("%% 首次启动已创建用户 admin (super-user)。一次性口令（仅显示一次，请立即修改）: %s\n", oneTime)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 决策 #183：SIGHUP（终端挂断）**不得**杀死守护进程。真机实测：console 打开的 VM 串口
	// pty 曾成为本进程的控制终端（打开时未带 O_NOCTTY），此后 stop 那台 VM → QEMU 关闭
	// pty master → 内核发 SIGHUP → nfvisd 退出，请求方只看到连接 EOF、systemd 静默重启。
	// 根因已在 OpenConsole 用 O_NOCTTY 修掉；这里再对 SIGHUP 免疫，杜绝同类路径重现。
	defer systemd.WatchHangup(func() {
		log.Warn("收到 SIGHUP（终端挂断），已忽略：本守护进程无重载语义，配置变更请经 commit")
	})()

	// 决策 #182：libvirt 的 AppArmor 助手放行 NFViS 镜像/VM 磁盘路径。安装期脚本做同一件事，
	// 但 libvirt 可能**晚于** nfvis 安装，或与它同一次 apt 事务而被后配置——那时安装期探测条件
	// 不成立、放行会静默漏掉：装机全程报成功，VNF 直到被启动才失败（round85 干净快照离线安装实测）。
	// 运行期幂等补齐与安装顺序无关；失败只告警不阻塞启动，周期巡检会再试。
	aaMgr := &system.AppArmorLibvirt{Runner: runCmd}
	// 决策 #329：大页池回收的写能力（按页尺寸写 sysfs）。与 API/CLI 装配的同一实现，
	// 巡检与 request system hugepages reclaim 不出现两套行为。
	hugepageSetter := system.NewSysfsHugepageSetter()
	// 启动期 Ensure 有界（15s，决策 #349）：Ensure 内部走 runCmd（10 分钟上界），
	// 底座异常时会在启动序列里叠一段长等待；启动期无须长等 —— 周期巡检本就幂等重试。
	aaCtx, aaCancel := context.WithTimeout(ctx, 15*time.Second)
	if changed, err := aaMgr.Ensure(aaCtx); err != nil {
		log.Warn("libvirt AppArmor 放行未完成", "err", err)
	} else if changed {
		log.Info("libvirt AppArmor 已放行 NFViS 镜像/VM 路径")
	}
	aaCancel()

	// R88-1 / 决策 #347：大页池 sysctl 钉值。VPP 包自带 /etc/sysctl.d/80-vpp.conf
	// （vm.nr_hugepages=1024，本意给 2M 池），而该 sysctl 只作用于**默认尺寸**池——历史上产品
	// 基线设了 default_hugepagesz=1G 时它就落到 1G 池上，开机按可用内存尽量分配，1G 池因此大于
	// 基线声明值（真机：声明 1、实际 4）。#347 后内核默认大页尺寸恒为 2M，该 sysctl 与 2M 池本意一致。
	// 按 cmdline 声明写 90 号落点钉回，与安装顺序无关（同 #182 的单源口径）。
	if changed, err := system.EnsureHugepageSysctlFromCmdline(""); err != nil {
		log.Warn("大页池 sysctl 钉值未完成", "err", err)
	} else if changed {
		log.Info("已按内核基线声明写入大页池 sysctl 片段（/etc/sysctl.d/90-nfvis-hugepages.conf，下次开机生效）")
	} else if system.EffectiveDefaultHugepageSize("") == "" {
		// 决策 #347：取不到（即将生效基线的）默认大页尺寸时保守不动该文件（不写不删），如实提示。
		log.Warn("未取到内核默认大页尺寸（default_hugepagesz），大页池 sysctl 片段保守未动")
	}

	// VPP 未运行时降级为告警并持续重连，不阻塞 nfvisd 启动。
	// M3-8：每次连接成功（首连=启动收敛，重连=VPP 重启重放）触发恢复收敛；
	// 单个对象失败不阻塞，未收敛项进告警表（FR-OPS-010/011）。
	var recoveryMu sync.Mutex
	runRecovery := func() {
		// VPP 连接（重）建立：先让状态型编排器的进程内登记失效，再做恢复收敛。
		// 带外 `systemctl restart vpp` 会清空 VPP 侧配置，而进程内登记仍在 → ApplyNAT 认为
		// 「已下发」跳过重放，NAT 静默失效（show nat44 空），须重启 nfvisd 才恢复
		// （round84 R84-21）。放在加锁之前：周期巡检占着锁时失效也已生效，随后到来的一次
		// 收敛必然全量重放。重放只发 add、不摘除（附录 A #35），且 add 方向幂等
		// （nat_govpp.go 把「已存在」按成功处理），可安全重复执行。
		//
		// 失效的**安全边界**由网络侧保证（network.InvalidateRuntimeState 的注释写明）：
		// 失效与 NAT 下发互斥、且恢复收敛会在 ApplyNAT 之前按配置重建 L3 侧登记——
		// 否则「inside/outside 解析暂时为空」会被 NAT 当成「配置里没有 inside/outside」，
		// 把插件特性删掉/关掉（真机实测：restart 后 show nat44 ei interfaces 与 addresses 全空）。
		netProvider.InvalidateRuntimeState()
		recoveryMu.Lock()
		defer recoveryMu.Unlock()
		cfg, err := engine.Committed()
		if err != nil {
			log.Error("恢复收敛：读取 committed 配置失败", "err", err)
			return
		}
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// 决策 #192：删表延后项的复核放在恢复收敛**之前**——数据面重启后那些表已随重启消失，
		// 先复核一次既有日志可查（EnsureConsistent 内部也会复核，幂等无害）。
		if names := netProvider.RetryDeferredVRFDeletes(rctx, cfg); len(names) > 0 {
			log.Info("删表延后项已清理", "vrfs", names)
		}
		if errs := netProvider.EnsureConsistent(rctx, cfg); len(errs) > 0 {
			for _, e := range errs {
				log.Warn("网络恢复收敛未收敛项", "err", e)
			}
		}
		// M4-9：计算/容器收敛（FR-OPS-010/012）——补建缺失 domain/容器；失败经告警 sink 上报。
		for _, e := range computeProvider.EnsureConsistent(rctx, cfg) {
			log.Warn("计算恢复收敛未收敛项", "err", e)
		}
		for _, e := range containerProvider.EnsureConsistent(rctx, cfg) {
			log.Warn("容器恢复收敛未收敛项", "err", e)
		}
		for _, e := range computeProvider.CheckVMAlarms(rctx, cfg) {
			log.Warn("VM 异常退出巡检", "err", e)
		}
		for _, e := range containerProvider.CheckContainerAlarms(rctx, cfg) {
			log.Warn("容器异常退出巡检", "err", e)
		}
		// M4-4：VNF vNIC 断连检测（FR-NET-023）——link down/缺失记为告警，恢复则消警。
		for _, e := range netProvider.CheckVnfPorts(rctx, cfg) {
			log.Warn("vNIC 状态检查", "err", e)
		}
		// V1 收尾（决策 #73）：物理业务口链路状态告警（FR-NET-003）
		for _, e := range netProvider.CheckInterfaceLinks(rctx, cfg) {
			log.Warn("物理口链路检查", "err", e)
		}
		// 决策 #192：删表延后项的复核（数据面重启后其表已消失；配置又声明回来则本就合法）。
		if names := netProvider.RetryDeferredVRFDeletes(rctx, cfg); len(names) > 0 {
			log.Info("删表延后项已清理", "vrfs", names)
		}
		log.Info("恢复收敛完成")
		// 决策 #348：VPP 已连接（数据面在线）即自动消解「启动拉起失败」告警（幂等）。
		vppAutostartAlarms(alarms, nil)
	}
	// M4-10：运行态异常退出巡检（FR-CMP-017/022）——VM crashed / 容器异常退出 → critical 告警；
	// 周期性（15s）检测，事件驱动实时告警随 M5 /events。与恢复收敛共用锁避免并发使用 VPP API。
	go func() {
		tk := time.NewTicker(15 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				recoveryMu.Lock()
				cfg, err := engine.Committed()
				if err == nil {
					for _, e := range computeProvider.CheckVMAlarms(ctx, cfg) {
						log.Warn("VM 状态巡检", "err", e)
					}
					for _, e := range containerProvider.CheckContainerAlarms(ctx, cfg) {
						log.Warn("容器状态巡检", "err", e)
					}
					for _, e := range netProvider.CheckVnfPorts(ctx, cfg) {
						log.Warn("vNIC 状态巡检", "err", e)
					}
					for _, e := range netProvider.CheckInterfaceLinks(ctx, cfg) {
						log.Warn("物理口链路巡检", "err", e)
					}
					// 决策 #192：删表延后项的复核（表一旦不在数据面就清登记并消警，
					// 不依赖「恰好又发生了一次 VPP 重连」）。
					netProvider.RetryDeferredVRFDeletes(ctx, cfg)
					// 决策 #321：残渣对账（IP 表 ∪ ACL ∪ bridge-domain）——按数据面实况逐次
					// 重建/消解 *LEFTOVER 告警，并把已复原对象的提交期补偿告警一并消解。
					for _, e := range netProvider.ReconcileResidue(ctx, cfg) {
						log.Warn("残渣对账未收敛项", "err", e)
					}
					// 决策 #333：恢复收敛告警族的按来源廉价复核（不做全量重放）——来源对象
					// 已不在 committed 配置即消解；RECOVERY_IFACE_MISSING 的口已出现在 VPP
					// 即消解；其余（UNCONVERGED 等）保守保留，权威重放仍只在 VPP 重连。
					for _, e := range netProvider.ReconcileRecoveryAlarms(ctx, cfg) {
						log.Warn("恢复收敛告警复核", "err", e)
					}
				}
				recoveryMu.Unlock()
			}
		}
	}()
	// M5-5：硬件阈值巡检（FR-SYS-012）——越限产生告警，恢复消警（与 /system/hardware 同源）
	go func() {
		interval := 60 * time.Second
		t := time.NewTicker(interval)
		defer t.Stop()
		check := func() {
			cfg, err := engine.Committed()
			if err != nil {
				return
			}
			th := model.HealthThresholds{}
			if cfg.System != nil && cfg.System.Health != nil {
				th = *cfg.System.Health
			}
			cctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			hh := hwProvider.Collect(cctx)
			v := hwProvider.Evaluate(&hh, th.CPUTempCelsius, th.DiskTempCelsius, th.DiskUsedPercent)
			if len(v) > 0 {
				alarms.Raise("hardware", network.SeverityWarning, "HARDWARE_THRESHOLD",
					"硬件健康越限: "+strings.Join(v, "; "), "system")
			} else {
				alarms.Resolve("hardware", "HARDWARE_THRESHOLD", "system")
			}
			// M5-8：证书临近过期告警（FR-SYS-011）
			if days, warn := tlsMgr.ExpiryAlarm(); warn {
				alarms.Raise("tls", network.SeverityWarning, "CERT_EXPIRING",
					fmt.Sprintf("API 证书将在 %d 天内过期", days), "system")
			} else {
				alarms.Resolve("tls", "CERT_EXPIRING", "system")
			}
			// 决策 #182：libvirt AppArmor 放行的周期复核（幂等）——覆盖「nfvisd 起来之后才装 libvirt」。
			if changed, err := aaMgr.Ensure(cctx); err != nil {
				log.Warn("libvirt AppArmor 放行未完成", "err", err)
			} else if changed {
				log.Info("libvirt AppArmor 已放行 NFViS 镜像/VM 路径")
			}
			// 决策 #329：大页池对账回收（复用本巡检，不新造定时器）——只回收「实际 > 声明且空闲」
			// 的多余页，在用页一律不动；写后回读确认才算收敛。收敛不掉（在用页挡住）时以
			// HUGEPAGE_POOL_SURPLUS 告警如实呈现（含谁在占用的可查证据），收敛后自动消警。
			hpRes := api.HugepageReconcile("/", cfg, hugepageSetter)
			for _, p := range hpRes.Pools {
				switch p.Action {
				case system.HugepageActionReclaimed:
					log.Info("大页池已回收空闲多余页", "size", p.PageSize,
						"before", p.ActualBefore, "after", p.ActualAfter, "reclaimed", p.Reclaimed)
				case system.HugepageActionVerifyFailed:
					log.Warn("大页池回收未收敛", "size", p.PageSize, "err", p.Error)
				}
			}
			// 决策 #329/#346：把对账结果落到告警表——SURPLUS（实际高于声明且收敛不掉）与
			// ORPHAN（存在无主占用页）。按内核实况重建、收敛后自动消解（同一对账位置与口径、
			// 各自独立 scope）。逻辑抽到 hugepageAlarms 便于单测。
			hugepageAlarms(alarms, hpRes)
			// 决策 #337：L2 环路疑似巡检（采样式，只告警不阻断）——对每个 L2 交换机读一次 MAC
			// 学习表与上一轮快照比较；独立 scope "loop"，连续多轮平静自动消警。
			for _, e := range netProvider.CheckLoop(cctx, cfg) {
				log.Warn("环路检测", "err", e)
			}
		}
		check()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				check()
			}
		}
	}()

	// 决策 #348：nfvisd 启动时确保 VPP 运行（重启后数据面自动恢复）。只在此处（连接管理 Run
	// 之前）调用一次，且是**发起式、不等待**：VPP 已在运行则不做动作；未运行则发起拉起
	// （systemctl start --no-block vpp）后立即返回，就绪由既有连接重试循环接管（不在这里等）。
	// 发起失败如实告警但**不阻塞启动**；VPP 是否真的可用由连接状态体现（不谎称可用）。
	// 注意：发起成功 ≠ 已在线，故这里**不消解**告警——VPP 连接成功时由 runRecovery 消解。
	if err := vppMgr.EnsureRunning(ctx); err != nil {
		log.Warn("启动时发起确保 VPP 运行未成功，数据面可能暂不可用（就绪由连接重试循环接管）", "err", err)
		vppAutostartAlarms(alarms, err)
	}
	vppMgr.OnConnect(func(version string) { go runRecovery() })
	go func() {
		if err := vppMgr.Run(ctx); err != nil {
			log.Error("VPP 连接管理退出", "err", err)
		}
	}()
	startupApplier := &network.Applier{Mgr: vppMgr,
		// 解析顺序：先系统事实（sysfs），netdev 已因 DPDK 接管而消失时再回退到绑定记录（决策 #100）
		PCI:      network.PCIResolverWithBindings(network.NewSysfsPCIResolver(), dpdkBindings),
		Bindings: dpdkBindings,
		// 掉口风险等处置只落日志：告警表按「恢复收敛」语义建/消（决策 #35），
		// 目前没有它的生命周期，硬塞进去只会留下永不消退的告警。
		Warn:      func(msg string) { log.Warn(msg) },
		Restarter: network.NewSystemctlRestarter(), RestartOnApply: true}

	// M4-4：VM 生命周期动作后刷新 vNIC 断连告警（FR-NET-023）。
	var (
		vmAPI     api.VMRuntime
		vmConsole api.VMConsoleRuntime
		vmSnaps   api.VMSnapshotRuntime
	)
	if vmRuntime != nil && p != nil {
		vmAPI = &vmController{Provider: p, net: netProvider, engine: engine, log: log}
		vmConsole = p // M4-5：串口 console（libvirt 域串口 ↔ WebSocket）
		vmSnaps = &snapshotController{p: p}
	}

	apiServer := api.New(engine, aaaSvc, api.Options{
		Addr:    *listen,
		TLSCert: *tlsCert,
		TLSKey:  *tlsKey,
		Log:     log,
		VPP: &vppController{mgr: vppMgr, applier: startupApplier, engine: engine,
			// socket 供起后健康探测用（与连接管理器同一套接字）
			socket: *vppSock},
		L2:    &l2Controller{net: netProvider},
		L3:    &l3Controller{net: netProvider},
		LLDP:  &lldpController{net: netProvider},
		State: state.New(vppMgr.Runtime()),
		SRIOV: sriovProvider,
		DPDK: &dpdkController{b: dpdkBinder, rec: dpdkBindings, logger: log, facts: mgmtFacts,
			// 数据面占用探测（发现 #13）：解绑前问 VPP「这个口还在你手里吗」
			dataplane: func(ifname string) (bool, error) {
				c, err := vppMgr.SvcClientFunc()()
				if err != nil {
					return false, err
				}
				defer c.Close()
				_, ok, err := c.SwInterfaceIndex(ifname)
				return ok, err
			}},
		Kernel:      system.NewBaselineApplier(),
		Hugepages:   system.NewSysfsHugepageSetter(), // 决策 #329：大页池回收（按页尺寸写 sysfs）
		NAT:         &natSessionsController{net: netProvider},
		Alarms:      &alarmController{store: alarms},
		Diag:        &diagController{diag: vppMgr.Diagnostics()},
		VM:          vmAPI,
		VMConsole:   vmConsole,
		VMSnapshots: vmSnaps,
		Containers:  ctRuntime,
		Images:      imagesStore,
		Events:      bus,
		SysOps:      sysOps,
		DiagOps:     &diagOpsController{tech: techSupport, cores: coreDumps},
		Capture:     &captureController{p: captureProvider},
		Software:    &softwareController{m: swMgr},
		Hardware:    &hardwareController{p: hwProvider},
		TLS:         &tlsController{m: tlsMgr},
		Ports:       &portInventoryController{net: netProvider}, // 决策 #83：运行态端口清单
		VppState:    &vppStateController{net: netProvider},      // 决策 #84：show 的运行态事实来源
		Versions:    system.NewVersionProbe(),                   // R37-2 收口（决策 #118）：组件版本探测
		LogSource:   nfvisdLogTail,
	})

	srvErr := make(chan error, 1)
	go func() { srvErr <- apiServer.ListenAndServe() }()
	log.Info("nfvisd 就绪", "db", *dbPath, "listen", *listen)
	// FR-OPS-013：通知 systemd 就绪并按 WATCHDOG_USEC 喂看门狗（未由 systemd 管理时空操作）。
	if err := systemd.Notify("READY=1"); err != nil {
		log.Warn("sd_notify(READY=1) 失败", "err", err)
	}
	if iv, ok := systemd.WatchdogInterval(); ok {
		go func() {
			t := time.NewTicker(iv)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if err := systemd.Notify("WATCHDOG=1"); err != nil {
						log.Warn("sd_notify(WATCHDOG=1) 失败", "err", err)
					}
				}
			}
		}()
		log.Info("systemd 看门狗已启用", "interval", iv.String())
	}

	select {
	case err := <-srvErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("API 服务异常退出: %w", err)
		}
	case <-ctx.Done():
		log.Info("收到退出信号，优雅停机")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := apiServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("API 停机超时", "err", err)
	}
	log.Info("nfvisd 已停止")
	return nil
}

// vppController 装配 api.VppController（M3-2）：状态来自连接管理器，
// 重启按当前 committed 全量配置重新生成 startup.conf 并重启 VPP。
type vppController struct {
	mgr     *network.Manager
	applier *network.Applier
	engine  *config.Engine
	// socket binary API 套接字（起后健康探测用；与连接管理器同源）
	socket string
	// probe 起后健康探针（可注入；缺省 vppBinaryProbe = binary API 能连上）
	probe vppProbeFunc
	// poolPages 读某页尺寸的运行期大页池（可注入；缺省读 sysfs；ok=false = 读不到不判定）
	poolPages func(size string) (int, bool)
	// connGen/connReady 连接管理器**自身会话**的世代与就绪判定（可注入；缺省读 mgr）。
	// 决策 #315：`request vpp restart` 在返回前必须确认管理器已换成**新连接**（与
	// /vpp/status 同一管理器，即决策 #314 的单一事实源）——只看裸 socket 探针不够：
	// VPP 重启后 govpp 报断连前的窗口里，管理器仍持有旧会话，立即查询会撞 broken pipe。
	connGen   func() uint64
	connReady func(since uint64) bool
	// healthTimeout/healthInterval 起后健康等待的有界参数（<=0 取缺省；测试可缩短）
	healthTimeout  time.Duration
	healthInterval time.Duration
}

// generation 当前连接世代（mgr 未接入 / 未注入时返回 0）。
func (c *vppController) generation() uint64 {
	if c.connGen != nil {
		return c.connGen()
	}
	if c.mgr != nil {
		return c.mgr.ConnGeneration()
	}
	return 0
}

// sessionReady 管理器自身会话是否已是新连接（未接管理器时不额外判定，保留既有探针语义）。
func (c *vppController) sessionReady(since uint64) bool {
	if c.connReady != nil {
		return c.connReady(since)
	}
	if c.mgr != nil {
		return c.mgr.ConnectedSince(since)
	}
	return true
}

// lastConnErr 管理器最近一次连接错误（用于把「为何没就绪」说清楚）。
func (c *vppController) lastConnErr() error {
	if c.mgr != nil {
		return c.mgr.LastError()
	}
	return nil
}

func (c *vppController) Status(vpp *model.VppConfig) api.VppStatus {
	v := c.mgr.StatusView(vpp)
	return api.VppStatus{Version: v.Version, Connected: v.Connected,
		PendingRestart: v.PendingRestart, LastError: v.LastError}
}

// Restart 按 committed 配置重生成 startup.conf 并重启 VPP（FR-SYS-009）。
//
// `systemctl restart` 返回 0 只说明「重启动作被接受」：VPP 起不来时（如大页池被清空）
// 进程会立刻 SEGV 退出，而 CLI/REST 都会把它当成功报给操作者——操作者看到成功、
// 数据面其实全挂（R84-4/R83-6）。故这里在返回成功前做三件事：
// 重启前按配置预检大页池；重启后有界等待裸 socket 可连（VPP 进程真的活着）；
// **并且**等 nfvisd 连接管理器把自身会话换成新连接（决策 #315）——否则「重启返回后
// 立即查询」会撞上旧会话的 `write: broken pipe`（round34/35 的既定现场）。
func (c *vppController) Restart(ctx context.Context, _ *model.VppConfig) error {
	cfg, err := c.engine.Committed()
	if err != nil {
		return err
	}
	if err := c.precheckHugepages(cfg.Vpp); err != nil {
		return err
	}
	// 记下重启前的连接世代：重启会杀掉旧连接，管理器必须重连才会前进（决策 #315）。
	since := c.generation()
	if _, err := c.applier.Apply(ctx, &cfg); err != nil {
		return err
	}
	return c.waitHealthy(ctx, since)
}

// 起后健康校验参数：VPP 正常起约 1~2 秒，给足 15 秒（含 systemctl 停/起与 DPDK 初始化），
// 每 500ms 探一次；超时即如实报错，不把「没起来」报成成功。
const (
	vppHealthTimeout  = 15 * time.Second
	vppHealthInterval = 500 * time.Millisecond
)

// vppProbeFunc 一次起后健康探测：返回 nil 表示 binary API 可连（VPP 真的起来了）。
type vppProbeFunc func(ctx context.Context) error

// errSessionStale VPP 进程活着（裸 socket 探针通了），但 nfvisd 的连接管理器尚未把
// 自身会话换成新连接——此窗口内查询会在旧 socket 上报 broken pipe（决策 #315）。
var errSessionStale = errors.New("VPP 已起来，但数据面连接尚未重建（仍是重启前的旧会话）")

// waitHealthy 有界轮询确认 VPP 真的起来了**且查询可用**（决策 #315）：
//   - 裸 socket 探针：证明 VPP 进程活着（VPP 起不来时必须如实报错，R84-4）；
//   - 连接管理器会话就绪：证明「重启返回后立即查询」不会撞上陈旧会话（与 /vpp/status 同源）。
//
// 两者都满足才返回 nil。探针通了但管理器会话没在窗口内重建时，**不返回成功**——那正是
// 操作者立即查询得到 broken pipe 的现场；如实报出并给指引，绝不把「重启动作已接受」
// 当成「数据面可服务」。
func (c *vppController) waitHealthy(ctx context.Context, since uint64) error {
	timeout, interval := c.healthTimeout, c.healthInterval
	if timeout <= 0 {
		timeout = vppHealthTimeout
	}
	if interval <= 0 {
		interval = vppHealthInterval
	}
	probe := c.probe
	if probe == nil {
		probe = func(ctx context.Context) error { return vppBinaryProbe(ctx, c.socket) }
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	probeOK := false
	for {
		limit := time.Until(deadline)
		if limit <= 0 {
			break
		}
		if perr := probeWithin(ctx, probe, limit); perr == nil {
			probeOK = true
			if c.sessionReady(since) {
				return nil
			}
			// 探针通了（VPP 进程活着），但管理器还是一副旧会话：继续等它重连。
			lastErr = errSessionStale
		} else {
			lastErr = perr
		}
		if ctx.Err() != nil {
			// 等待被取消（如 REST 请求中断）：如实说「没等到」，不当作成功
			return fmt.Errorf("VPP 重启后未起来（等待被取消）: %v", ctx.Err())
		}
		// 剩余时间不够再等一轮：结束等待，如实报错
		if !time.Now().Add(interval).Before(deadline) {
			break
		}
		t := time.NewTimer(interval)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
		}
	}
	if probeOK {
		// VPP 起来了，但数据面连接没在窗口内恢复为可用会话：返回成功只会让操作者的
		// 下一条查询撞 broken pipe。如实报「数据面连接在重启窗口内不可用」并给指引。
		reason := errSessionStale.Error()
		if e := c.lastConnErr(); e != nil {
			reason += "；连接管理器最近错误：" + e.Error()
		}
		return fmt.Errorf("VPP 已起来，但数据面连接未在 %s 内恢复为可用会话（%s）；"+
			"此刻查询会报「数据面连接不可用」；请稍后重试，或 request vpp restart（自查：show vpp）",
			timeout, reason)
	}
	return fmt.Errorf("VPP 重启后未起来：%s 内 binary API 未连上（%v）；"+
		"请查 systemctl status vpp 与 journalctl -u vpp 的启动日志，数据面当前不可用", timeout, lastErr)
}

// probeWithin 执行一次探测，最多等 limit：探测本身是不可取消的阻塞调用
// （govpp 连接路径自带超时，最坏约 7 秒），用协程兜底，保证整体等待不超过上限。
// 超时后探测协程自行结束（其内部路径有界），不会泄漏。
func probeWithin(ctx context.Context, probe vppProbeFunc, limit time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- probe(ctx) }()
	t := time.NewTimer(limit)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
		return fmt.Errorf("探测未在 %s 内返回", limit.Round(10*time.Millisecond))
	case <-ctx.Done():
		return ctx.Err()
	}
}

// vppBinaryProbe 探一次 VPP binary API：能建连（govpp 连接建立时会与 VPP 完成
// socket 注册与消息表握手）即视为健康。用**独立连接**探测，不碰连接管理器的会话，
// 故 VPP 未起来时只回报错误，不干扰重连状态机。
// 注意：连接建立路径自带超时（socket 等待与握手各 3 秒），不会无限阻塞。
func vppBinaryProbe(ctx context.Context, socket string) error {
	sess, events, err := network.NewGovppDialer().Dial(socket, 1, 0)
	if err != nil {
		return fmt.Errorf("连接 %s: %w", socket, err)
	}
	defer sess.Disconnect()
	select {
	case ev, ok := <-events:
		if !ok {
			return fmt.Errorf("连接 %s: 未返回连接结果", socket)
		}
		if ev.State != network.StateConnected {
			if ev.Err != nil {
				return fmt.Errorf("连接 %s: %v", socket, ev.Err)
			}
			return fmt.Errorf("连接 %s: 状态 %s", socket, ev.State)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("连接 %s: %v", socket, ctx.Err())
	}
}

// precheckHugepages 重启前按配置需求预检大页池（R84-4 的失败面）：
// 配置声明 hugepage-preference 时 startup.conf 会把它同时钉成 default-hugepage-size
// 与 main-heap-page-size（决策 #114），该尺寸的运行期池为 0 时 VPP 必然起不来
// （实测：池被运行期置 0 后 VPP 直接 SEGV）。此处明确拒绝，别让数据面白挂一次。
func (c *vppController) precheckHugepages(vpp *model.VppConfig) error {
	if vpp == nil || vpp.Memory == nil || vpp.Memory.HugepagePreference == "" {
		return nil
	}
	size := vpp.Memory.HugepagePreference
	pages, ok := c.poolPagesOf(size)
	if !ok || pages > 0 {
		return nil // 读不到池（非 sysfs 环境）或池非空：不做判定，交给起后健康校验
	}
	return fmt.Errorf("VPP 未重启：配置的大页偏好为 %s，但内核 %s 大页池当前为 0（%s）；"+
		"请先恢复该池（内核基线用 request system kernel apply + request system reboot，"+
		"或运行期写入该文件），再重启数据面",
		size, size, hugepageSysfsPath(size))
}

// poolPagesOf 读运行期大页池（可注入；缺省读 sysfs）。
func (c *vppController) poolPagesOf(size string) (int, bool) {
	if c.poolPages != nil {
		return c.poolPages(size)
	}
	return readHugepagePool(size)
}

// hugepageSysfsPath 某页尺寸的池大小文件路径（与 internal/system/kernel.go 的读取同源）。
func hugepageSysfsPath(size string) string {
	switch size {
	case "2M":
		return "/sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages"
	case "1G":
		return "/sys/kernel/mm/hugepages/hugepages-1048576kB/nr_hugepages"
	}
	return ""
}

// readHugepagePool 读某页尺寸的运行期池大小；ok=false 表示该尺寸不识别或读不到
// （非 Linux/无 sysfs 的环境不做判定，避免拿「读不到」当「池为 0」误拒）。
func readHugepagePool(size string) (int, bool) {
	path := hugepageSysfsPath(size)
	if path == "" {
		return 0, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	return n, true
}

// Version 最近一次成功连接探测到的 VPP 版本（决策 #118：/system/version 的 vpp 键取这里）。
func (c *vppController) Version() string { return c.mgr.Version() }

// envOr 读取环境变量，缺省返回 fallback。
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// l2Controller 装配 api.L2Runtime（M3-3）：把编排器 MAC 表返回转换为 API 契约结构。
type l2Controller struct{ net *network.L2Network }

func (c *l2Controller) MACTable(ctx context.Context, swName string) ([]api.MACTableRow, error) {
	rows, err := c.net.MACTable(ctx, swName)
	if err != nil {
		return nil, err
	}
	out := make([]api.MACTableRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, api.MACTableRow{MAC: r.MAC, Port: r.Port, VLAN: r.VLAN})
	}
	return out, nil
}

// portInventoryController 装配 api.PortInventory（决策 #83）：运行态端口清单。
// `<ifname>` 的候选与 `show interfaces physical` 的空态同源，取自真实端口而非已配置的接口名。
type portInventoryController struct{ net *network.L2Network }

func (c *portInventoryController) VPPIfnames() ([]string, error) { return c.net.VPPIfnames() }

func (c *portInventoryController) KernelIfnames() ([]string, error) { return c.net.KernelIfnames() }

// KernelIfFacts 内核侧物理口事实（决策 #302）：未接管口读视图的驱动/MAC/速率/状态取 sysfs。
func (c *portInventoryController) KernelIfFacts() ([]network.KernelIfFacts, error) {
	return c.net.KernelIfFacts()
}

// vppStateController 装配 api.VppStateRuntime（决策 #84）：bridge-domain 与接口的运行态，
// 供 `show virtual-switches`（列表/成员口/计数）与 `show interfaces physical`（链接状态/速率/驱动）。
type vppStateController struct{ net *network.L2Network }

func (c *vppStateController) BridgeDomains() ([]api.BridgeDomainState, error) {
	bds, err := c.net.BridgeDomains()
	if err != nil {
		return nil, err
	}
	out := make([]api.BridgeDomainState, 0, len(bds))
	for _, bd := range bds {
		st := api.BridgeDomainState{
			ID: bd.ID, Name: bd.Name, Learn: bd.Learn, Flood: bd.Flood, UuFlood: bd.UuFlood,
			Forward: bd.Forward, ArpTerm: bd.ArpTerm, MacAge: bd.MacAge,
		}
		for _, p := range bd.Ports {
			st.Ports = append(st.Ports, api.BridgeDomainPort{SwIfIndex: p.SwIfIndex, Name: p.Name, Shg: p.Shg})
		}
		out = append(out, st)
	}
	return out, nil
}

func (c *vppStateController) InterfaceStates() (map[string]api.InterfaceState, error) {
	m, err := c.net.InterfaceStates()
	if err != nil {
		return nil, err
	}
	out := make(map[string]api.InterfaceState, len(m))
	for name, st := range m {
		out[name] = api.InterfaceState{
			AdminUp: st.AdminUp, LinkUp: st.LinkUp, LinkSpeed: st.LinkSpeed, DevType: st.DevType,
			MTU: st.Mtu,
		}
	}
	return out, nil
}

// l3Controller 装配 api.L3Runtime（M3-4）：VRF 运行态 FIB。
type l3Controller struct{ net *network.L2Network }

func (c *l3Controller) Routes(ctx context.Context, vrfName string) ([]api.RouteRow, error) {
	rows, err := c.net.Routes(ctx, vrfName)
	if err != nil {
		return nil, err
	}
	out := make([]api.RouteRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, api.RouteRow{Prefix: r.Prefix, NextHop: r.NextHop, Distance: r.Distance})
	}
	return out, nil
}

// lldpController 装配 api.LldpRuntime（M3-6）：LLDP 邻居表。
type lldpController struct{ net *network.L2Network }

func (c *lldpController) Neighbors(ctx context.Context) ([]api.LldpNeighborRow, error) {
	rows, err := c.net.LldpNeighbors(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]api.LldpNeighborRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, api.LldpNeighborRow{Interface: r.Interface, ChassisID: r.ChassisID,
			PortID: r.PortID, TTL: r.TTL, LastHeard: r.LastHeard})
	}
	return out, nil
}

// natSessionsController 装配 api.NatSessionsRuntime（M3-7）。
type natSessionsController struct{ net *network.L2Network }

func (c *natSessionsController) Sessions(ctx context.Context) ([]api.NatSessionRow, error) {
	rows, err := c.net.NATSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]api.NatSessionRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, api.NatSessionRow{InsideIP: r.InsideIP, InsidePort: r.InsidePort,
			OutsideIP: r.OutsideIP, OutsidePort: r.OutsidePort, Protocol: r.Protocol,
			Bytes: r.Bytes, Packets: r.Packets})
	}
	return out, nil
}

// alarmController 装配 api.AlarmRuntime（M3-8）：恢复收敛告警的只读视图。
type alarmController struct{ store *network.AlarmStore }

func (c *alarmController) List(state string) []api.AlarmRow {
	rows := c.store.List(state)
	out := make([]api.AlarmRow, 0, len(rows))
	for _, a := range rows {
		out = append(out, api.AlarmRow{ID: a.ID, Severity: a.Severity, Code: a.Code,
			Message: a.Message, Source: a.Source, RaisedAt: a.RaisedAt,
			ResolvedAt: a.ResolvedAt, State: a.State, TimeSynced: a.TimeSynced})
	}
	return out
}

// Clear 删除已 resolved 告警（M5-9，FR-OPS-022）。
func (c *alarmController) Clear(id string, all bool) int { return c.store.Clear(id, all) }

// diagController 装配 api.DiagRuntime（M3-9）：ping/traceroute/clear 统计。
type diagController struct{ diag *network.Diagnostics }

func (c *diagController) Ping(ctx context.Context, host, source, vrf string, count int, ipv6 bool) (string, error) {
	return c.diag.Ping(ctx, network.PingRequest{Host: host, Source: source, VRF: vrf, Count: count, IPv6: ipv6})
}

func (c *diagController) Traceroute(ctx context.Context, host, vrf string, ipv6 bool) (string, error) {
	return c.diag.Traceroute(ctx, network.TracerouteRequest{Host: host, VRF: vrf, IPv6: ipv6})
}

func (c *diagController) ClearInterfaceStats(ctx context.Context, ifname string) error {
	return c.diag.ClearInterfaceStats(ctx, ifname)
}

// vmController 包装计算 Provider（M4-4）：生命周期动作后刷新 VNF vNIC 断连告警
// （FR-NET-023）。VM 关机导致 vhost-user 客户端断连 → VPP 接口 link down → warning 告警；
// 重新启动且客户端连上后自动消警。
type vmController struct {
	*compute.Provider
	net    *network.L2Network
	engine *config.Engine
	log    *slog.Logger
}

func (c *vmController) StartVM(ctx context.Context, name string) error {
	err := c.Provider.StartVM(ctx, name)
	c.refreshVnfAlarms()
	return err
}

// StartVMChecked 启动 + 回读域状态（决策 #311）。显式包装：不能用内嵌 Provider 的
// 提升方法，否则会绕过 refreshVnfAlarms（VM 启停后必须刷新 vNIC 断连告警）。
func (c *vmController) StartVMChecked(ctx context.Context, name string) (orchestrator.VMStartProbe, error) {
	probe, err := c.Provider.StartVMChecked(ctx, name)
	c.refreshVnfAlarms()
	return probe, err
}

func (c *vmController) StopVM(ctx context.Context, name string) error {
	err := c.Provider.StopVM(ctx, name)
	c.refreshVnfAlarms()
	return err
}

func (c *vmController) RestartVM(ctx context.Context, name string) error {
	err := c.Provider.RestartVM(ctx, name)
	c.refreshVnfAlarms()
	return err
}

func (c *vmController) refreshVnfAlarms() {
	cfg, err := c.engine.Committed()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, e := range c.net.CheckVnfPorts(ctx, cfg) {
		c.log.Warn("vNIC 状态检查", "err", e)
	}
}

// snapshotController 装配 api.VMSnapshotRuntime（M4-6）。
type snapshotController struct{ p *compute.Provider }

// requirePoweredOff 快照 create/rollback 需 VM 关机态（决策 #75，FR-CMP-015）。
//
// **必须放在这里**：CLI（api.cliExecutor）与 HTTP handler 是两条独立执行路径
// （前者直接调 VMSnapshotRuntime，不经 handler），把守卫只放在 handler 会漏掉 CLI——
// 本守卫初版即犯此错（真机实测 CLI 仍能对运行中 VM 建快照/回滚，VM 被静默重启）。
// 放在两侧共同依赖的实现处，才满足「命令树与执行器同源」。
//
// 依据：对运行中域 `DomainRevertToSnapshot(flags=0)` 实测**不报错但替换 QEMU 进程**
// （相当于静默重启该 VM），静默重启生产 VNF 不可接受。
func (c *snapshotController) requirePoweredOff(ctx context.Context, domain, op string) error {
	state, err := c.p.VMState(ctx, domain)
	if err != nil {
		return nil // 状态不可知时不阻断（与既有保守取向一致）
	}
	switch state {
	case orchestrator.VMStateRunning, orchestrator.VMStatePaused, orchestrator.VMStateCrashed:
		return fmt.Errorf("VM %s 当前为 %s，快照 %s 需先关机", domain, state, op)
	}
	return nil
}

func (c *snapshotController) SnapshotCreate(ctx context.Context, domain, name, desc string) error {
	if err := c.requirePoweredOff(ctx, domain, "create"); err != nil {
		return err
	}
	return c.p.SnapshotCreate(ctx, domain, name, desc)
}

func (c *snapshotController) Snapshots(ctx context.Context, domain string) ([]api.SnapshotRow, error) {
	infos, err := c.p.Snapshots(ctx, domain)
	if err != nil {
		return nil, err
	}
	rows := make([]api.SnapshotRow, 0, len(infos))
	for _, i := range infos {
		row := api.SnapshotRow{Name: i.Name, SizeBytes: i.SizeBytes, Description: i.Description}
		if !i.CreatedAt.IsZero() {
			ts := i.CreatedAt
			row.CreatedAt = &ts
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (c *snapshotController) SnapshotRevert(ctx context.Context, domain, name string) error {
	if err := c.requirePoweredOff(ctx, domain, "rollback"); err != nil {
		return err
	}
	return c.p.SnapshotRevert(ctx, domain, name)
}

func (c *snapshotController) SnapshotDelete(ctx context.Context, domain, name string) error {
	return c.p.SnapshotDelete(ctx, domain, name)
}

// diagOpsController 诊断归档/core dump 的 API 适配器（M5-4）。
type diagOpsController struct {
	tech  *system.TechSupport
	cores *system.CoreDumps
}

func (d *diagOpsController) GenerateTechSupport() (system.File, error) {
	f, err := d.tech.Generate()
	if err != nil {
		return f, err
	}
	d.cores.Prune() // 顺带按容量滚动清理转储（FR-OPS-041）
	return f, nil
}
func (d *diagOpsController) ListTechSupport() []system.File { return d.tech.List() }
func (d *diagOpsController) TechSupportPath(name string) (string, error) {
	return d.tech.Path(name)
}
func (d *diagOpsController) ListCoreDumps() []system.CoreDump { return d.cores.List() }
func (d *diagOpsController) DeleteCoreDumps(file string) (int, error) {
	return d.cores.Delete(file)
}
func (d *diagOpsController) ExportCoreDumps(ctx context.Context, url string) (int, int, error) {
	return d.cores.ExportManifest(ctx, url)
}

// kernelRelease 读取内核版本（诊断归档用）。
func kernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// nfvisdLogTail 取 nfvisd 日志尾部（systemd 单元优先，其次系统日志尾部）。
//
// 两个坑（决策 #184，真机实测）：
//  1. **单元名**：产品安装的单元是 `nfvis.service`（开发态才叫 nfvisd）——查 `-u nfvisd`
//     永远查不到东西，`show log system` 与诊断包 logs.txt 因此恒空；
//  2. **`-- No entries --` 是 journalctl 打到 **stdout** 的提示行**（rc=0、长度>0）——
//     用 `len(out) > 0` 判「拿到了日志」会把这条提示当成日志，设计好的「回退到系统日志尾部」
//     永不触发。
//
// 故：逐个候选单元名尝试，并用 journalctlHasNoEntries 识别「无条目」提示后再回退。
func nfvisdLogTail() ([]byte, error) {
	for _, unit := range []string{"nfvis", "nfvisd"} {
		out, err := exec.Command("journalctl", "-u", unit, "--no-pager", "-n", "500").Output()
		if err == nil && len(out) > 0 && !journalctlHasNoEntries(out) {
			return out, nil
		}
	}
	out, err := exec.Command("journalctl", "--no-pager", "-n", "200").Output()
	if err != nil {
		return []byte("(journalctl unavailable: " + err.Error() + ")\n"), nil
	}
	if journalctlHasNoEntries(out) {
		return []byte("(journalctl 无条目：本次启动以来系统日志为空)\n"), nil
	}
	return out, nil
}

// journalctlHasNoEntries 判定 journalctl 输出是否为「无条目」提示行（而非日志内容）。
// 形如 `-- No entries --`（可带前导空白）；日志正常时该行不会出现，故按子串判定即可。
func journalctlHasNoEntries(out []byte) bool {
	return bytes.Contains(out, []byte("No entries"))
}

// captureController 装配 api.CaptureRuntime（M5-3）。
type captureController struct{ p *network.CaptureProvider }

func (c *captureController) Status() (*api.CaptureSessionRow, []api.CaptureFileRow) {
	active, files := c.p.Status()
	var a *api.CaptureSessionRow
	if active != nil {
		a = &api.CaptureSessionRow{Interface: active.Interface, Captured: active.Captured,
			StartedAt: active.StartedAt, MaxDepth: active.MaxDepth}
	}
	rows := make([]api.CaptureFileRow, 0, len(files))
	for _, f := range files {
		rows = append(rows, api.CaptureFileRow{Name: f.Name, SizeBytes: f.SizeBytes, CreatedAt: f.CreatedAt})
	}
	return a, rows
}

func (c *captureController) Start(ctx context.Context, ifname string, count int, filterACL string) error {
	return c.p.Start(ctx, ifname, count, filterACL)
}

func (c *captureController) Stop(ctx context.Context, export bool) (api.CaptureFileRow, error) {
	f, err := c.p.Stop(ctx, export)
	return api.CaptureFileRow{Name: f.Name, SizeBytes: f.SizeBytes, CreatedAt: f.CreatedAt}, err
}

func (c *captureController) Path(name string) (string, error) { return c.p.Path(name) }

// softwareController 装配 api.SoftwareRuntime（M5-7）。
type softwareController struct{ m *system.SoftwareManager }

func (c *softwareController) Add(ctx context.Context, pkg, sha string) (system.SoftwareResult, error) {
	return c.m.Add(ctx, pkg, sha)
}
func (c *softwareController) Rollback(ctx context.Context) (system.SoftwareResult, error) {
	return c.m.Rollback(ctx)
}
func (c *softwareController) Reboot(ctx context.Context) error   { return c.m.Reboot(ctx) }
func (c *softwareController) Shutdown(ctx context.Context) error { return c.m.Shutdown(ctx) }
func (c *softwareController) NTPSync(ctx context.Context, servers []string) (string, error) {
	return c.m.NTPSync(ctx, servers)
}

// hardwareController 装配 api.HardwareRuntime（M5-5）。
type hardwareController struct{ p *system.HardwareProvider }

func (c *hardwareController) Collect(ctx context.Context) system.HardwareHealth {
	return c.p.Collect(ctx)
}
func (c *hardwareController) Evaluate(hh *system.HardwareHealth, cpuTemp, diskTemp, diskUsed int) []string {
	return c.p.Evaluate(hh, cpuTemp, diskTemp, diskUsed)
}

// tlsController 装配 api.TlsRuntime（M5-8）。
type tlsController struct{ m *system.TLSManager }

func (c *tlsController) Info() (system.TlsInfo, bool) { return c.m.Info() }
func (c *tlsController) Install(certPEM, keyPEM string) (system.TlsInfo, error) {
	return c.m.Install(certPEM, keyPEM)
}
func (c *tlsController) RegenerateSelfSigned(hostname string) (system.TlsInfo, error) {
	return c.m.RegenerateSelfSigned(hostname)
}
func (c *tlsController) RegenerateSSHHostKeys(ctx context.Context) error {
	return c.m.RegenerateSSHHostKeys(ctx)
}

// applyTLSSettings 按 committed 配置落实证书（FR-SYS-011）：cert-file/key-file 安装外部证书；
// 声明 tls_self_signed 且尚无证书时生成自签证书。失败仅告警（不阻塞 commit）。
func applyTLSSettings(cfg model.Config, m *system.TLSManager, log *slog.Logger) {
	if cfg.System == nil || cfg.System.API == nil {
		return
	}
	api := cfg.System.API
	if api.CertFile != "" && api.KeyFile != "" {
		certPEM, err1 := os.ReadFile(api.CertFile)
		keyPEM, err2 := os.ReadFile(api.KeyFile)
		if err1 != nil || err2 != nil {
			log.Warn("读取配置的证书文件失败", "cert", api.CertFile, "key", api.KeyFile, "err", err1)
			return
		}
		if _, err := m.Install(string(certPEM), string(keyPEM)); err != nil {
			log.Warn("安装配置的证书失败", "err", err)
		} else {
			log.Info("已按配置安装外部证书", "cert", api.CertFile)
		}
		return
	}
	if api.TLSSelfSigned {
		if _, ok := m.Info(); ok {
			return // 已有证书（避免每次 commit 重签）
		}
		host, _ := os.Hostname()
		if _, err := m.RegenerateSelfSigned(host); err != nil {
			log.Warn("生成自签证书失败", "err", err)
		} else {
			log.Info("已生成自签证书", "path", m.CertPath())
		}
	}
}

// applySyslogSettings 按 committed 配置联动日志级别与远程 syslog 转发
// （FR-OPS-030 级别可配 / FR-SYS-004 远程 syslog，决策 #69）。
// 级别缺省 info；非法值由 commit 校验拦截，此处按 info 兜底。
func applySyslogSettings(cfg model.Config, level *slog.LevelVar, fwd *system.SyslogForwarder, log *slog.Logger) {
	var sc *model.SyslogConfig
	if cfg.System != nil {
		sc = cfg.System.Syslog
	}

	lvl := slog.LevelInfo
	remote := system.SyslogConfig{}
	if sc != nil {
		switch sc.Level {
		case "debug":
			lvl = slog.LevelDebug
		case "warn":
			lvl = slog.LevelWarn
		case "error":
			lvl = slog.LevelError
		}
		remote = system.SyslogConfig{
			Host: sc.RemoteHost, Port: sc.RemotePort,
			Facility: sc.Facility, Severity: sc.Severity,
		}
	}
	changed := level.Level() != lvl
	level.Set(lvl)
	fwd.Configure(remote)
	if changed {
		log.Info("日志级别已应用", "level", lvl.String())
	}
	if remote.Enabled() {
		log.Info("远程 syslog 转发已配置",
			"host", remote.Host, "port", remote.Port,
			"facility", remote.Facility, "severity", remote.Severity)
	}
}

// alarmSyslogSeverity 告警 severity → RFC 5424 severity（FR-OPS-022，决策 #69）。
func alarmSyslogSeverity(sev string) int {
	switch sev {
	case "critical":
		return 2 // crit
	case "error":
		return 3 // err
	case "warning":
		return 4 // warning
	default:
		return 6 // info
	}
}

// hostnameOr 取主机名（失败时用兜底值）。
func hostnameOr(fallback string) string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return fallback
}

// mgmtAddressOf 取 committed 的管理口地址（未配置返回 ""）。
func mgmtAddressOf(cfg model.Config) string {
	if cfg.System == nil || cfg.System.Management == nil {
		return ""
	}
	return cfg.System.Management.Address
}

// dpdkController 把 network.DPDKBinder 适配为 api.DPDKSetter（FR-NET-001，决策 #72）。
type dpdkController struct {
	b      *network.DPDKBinder
	rec    *network.Bindings
	logger *slog.Logger
	// dataplane 探测「该口此刻是否在数据面（VPP）中」，用于解绑守卫（发现 #13）。
	// nil = 无法探测（不拦）。
	dataplane func(ifname string) (bool, error)
	// facts 提供管理口守卫所需事实（发现 #7；nil = 守卫未装配，仅限测试装配）。
	facts func() network.ManagementFacts
}

// checkManagement 判定目标是否为管理口（发现 #7，决策 #101：默认拒绝，不提供显式越过）。
//
// 目标可能是 PCI 地址（接管后内核已无 netdev，解绑时只能按 PCI 定位）→ 先解析成口名
// （内核 netdev 名或绑定记录），解析不出按「未知」处理、不拦（理由见 ResolveIfaceName 注释）。
// checkNotInDataplane 解绑前确认该口已离开数据面（发现 #13）。
//
// 探测不到（VPP 未连接等）**不拦**：数据面都没跑，就没有谁占着它，此时解绑是安全的。
func (c *dpdkController) checkNotInDataplane(target string) error {
	if c.dataplane == nil {
		return nil
	}
	name, ok := network.ResolveIfaceName(target, c.rec)
	if !ok {
		return nil
	}
	inDP, err := c.dataplane(name)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("无法探测接口是否在数据面中，跳过解绑守卫", "ifname", name, "err", err)
		}
		return nil
	}
	return network.CheckUnbindAllowed(name, inDP)
}

func (c *dpdkController) checkManagement(target string) error {
	if c.facts == nil {
		return nil
	}
	name, ok := network.ResolveIfaceName(target, c.rec)
	if !ok {
		return nil
	}
	return network.CheckManagementPort(name, c.facts())
}

func (c *dpdkController) SetDPDKBound(ctx context.Context, ifname string, bound bool, driver string) (string, string, error) {
	// 管理口守卫（发现 #7，决策 #101）：**先于任何 sysfs 动作**判定，命中即拒绝。
	// 落点在这里是因为 CLI 执行器与 REST handler 都经本方法（决策 #75：约束要放在两侧共同依赖处）。
	if err := c.checkManagement(ifname); err != nil {
		return "", "", err
	}
	// 解绑守卫（发现 #13）：仍被数据面占用的口不能直接解绑——实测会把 CLI 执行器占死、
	// 并把网卡留在无驱动。正确顺序是「配置里删声明 → request vpp restart → 再解绑」。
	if !bound {
		if err := c.checkNotInDataplane(ifname); err != nil {
			return "", "", err
		}
	}
	var pci string
	var err error
	if bound {
		pci, err = c.b.Bind(ctx, ifname, driver)
	} else {
		// driver 在解绑语义下表示「交还给哪个内核驱动」（缺省由内核自动探测）
		pci, err = c.b.Unbind(ctx, ifname, driver)
	}
	if err != nil {
		return "", "", err
	}
	// 绑定记录（决策 #100）：接管后内核无 netdev，这是最后一次能拿到口名→PCI 的时机；
	// 解绑则按 PCI 删除（操作者给的常是 PCI 地址）。记录失败只影响后续生成，故只告警。
	if c.rec != nil {
		var rerr error
		if bound {
			rerr = c.rec.Set(ifname, pci)
		} else {
			rerr = c.rec.DeleteByPCI(pci)
		}
		if rerr != nil && c.logger != nil {
			c.logger.Warn("更新 DPDK 绑定记录失败（数据面重启可能需要重新解析端口）",
				"ifname", ifname, "pci", pci, "err", rerr)
		}
	}
	// 必须按 **PCI** 回读驱动：绑定到 DPDK 后内核网卡即消失，
	// 按接口名解析会失败并把结果误报为「无驱动」（真机实测踩到）。
	// 解绑后内核驱动重新探测需要一点时间，故轮询等待。
	var cur string
	for i := 0; i < 15; i++ {
		if cur, _ = c.b.DriverOf(pci); cur != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return pci, cur, nil
}

// vppAutostartAlarms 把一次「启动时确保 VPP 运行」的结果落到告警表（决策 #348）。
//
// 与 #329/#346 同口径：失败（未能发起拉起）→ Raise
// VPP_AUTOSTART_FAILED（warning，scope "vpp_autostart"，source 为 vpp 单元）；
// VPP 已在运行（err==nil）或恢复在线（runRecovery 调用）→ Resolve 自动消解。
func vppAutostartAlarms(sink alarmSink, err error) {
	if err == nil {
		sink.Resolve("vpp_autostart", network.AlarmVPPAutostartFailed, "vpp")
		return
	}
	sink.Raise("vpp_autostart", network.SeverityWarning, network.AlarmVPPAutostartFailed,
		"nfvisd 启动时未能发起拉起 VPP："+err.Error()+
			"。数据面当前不可用；处置：systemctl status vpp / journalctl -u vpp 查因，"+
			"或手工 systemctl start vpp，随后 show vpp 确认连接（VPP 恢复在线后本告警自动消解）",
		"vpp")
}

// alarmSink 是大页池告警建/消所需的最小告警表能力（由 *network.AlarmStore 实现；
// 抽成接口便于单测注入假告警表）。
type alarmSink interface {
	Raise(scope, severity, code, message, source string)
	Resolve(scope, code, source string) bool
}

// hugepageAlarms 把一次大页池对账结果落到告警表（决策 #329 SURPLUS + #346 ORPHAN）。
//
// 两条告警同一对账位置与口径：按内核实况**重建**（不靠进程内记忆，跨 nfvisd 重启仍可见）、
// 收敛后**自动消解**；各自独立 scope（SURPLUS 在 "hugepages"、ORPHAN 在 "hugepages_orphan"）。
// 无主占用（Orphan>0）按进入对账时观测到的内核实况如实报（这类页产品侧不可回收，只作可见性）；
// 内核实况不再有无主占用即自动消解。
func hugepageAlarms(sink alarmSink, res system.HugepageReconcileResult) {
	if bad := res.Unconverged(); len(bad) > 0 {
		msgs := make([]string, 0, len(bad))
		for _, p := range bad {
			msgs = append(msgs, fmt.Sprintf("%s：声明 %d、实际 %d、在用 %d%s",
				p.PageSize, p.Declared, p.ActualAfter, p.InUse, hugepageBlockerText(p)))
		}
		sink.Raise("hugepages", network.SeverityWarning, system.HugepageSurplusAlarmCode,
			"大页池实际高于声明且未能收敛（在用页不动）："+strings.Join(msgs, "；")+
				"。处置：停掉持页的 VNF 后再 request system hugepages reclaim，或调整声明值（set resource-pools hugepages … count <n>，需 reboot）",
			"system")
	} else {
		sink.Resolve("hugepages", system.HugepageSurplusAlarmCode, "system")
	}

	if orphaned := res.Orphaned(); len(orphaned) > 0 {
		msgs := make([]string, 0, len(orphaned))
		for _, p := range orphaned {
			msgs = append(msgs, fmt.Sprintf("%s：在用 %d、实际持有 %d、无主占用 %d 页",
				p.PageSize, p.InUse, p.Held, p.Orphan))
		}
		sink.Raise("hugepages_orphan", network.SeverityWarning, system.HugepageOrphanAlarmCode,
			"大页池存在无主占用页（分配了却无任何进程/inode 引用）："+strings.Join(msgs, "；")+
				"。这类页多为被进程预留但未使用的大页（如数据面 DPDK 预留），不在空闲链表上、产品侧写 nr_hugepages 释放不了——"+
				"request system hugepages reclaim 不会动它们，需从预留者一侧释放",
			"system")
	} else {
		sink.Resolve("hugepages_orphan", system.HugepageOrphanAlarmCode, "system")
	}
}

// hugepageBlockerText 把「谁在占用」的可查证据拼成告警文案的一小段（决策 #329）。
// 没有具体证据时如实说明（内核只给池总量与空闲数），不编造持有者。
func hugepageBlockerText(p system.HugepagePoolResult) string {
	if len(p.Blockers) == 0 {
		return ""
	}
	return "（占用者：" + strings.Join(p.Blockers, "；") + "）"
}

// 启动期底座降级告警码（决策 #349 契约面：scope/source/码三要素经 show alarms 与
// GET /alarms/active 呈现）。
const (
	alarmCodeComputeUnavailable   = "COMPUTE_UNAVAILABLE"
	alarmCodeContainerUnavailable = "CONTAINER_UNAVAILABLE"
)

// computeUnavailableAlarm 启动期 libvirt 未接入的降级告警（scope compute、source
// libvirt、severity warning）。文案要素：发生了什么（启动时未接入、已降级、VM
// 生命周期动作不可用、已有配置声明不受影响）、独立事实源手查路径、恢复路径、
// 以及「产品不自动重连」的如实边界。抽成纯函数（hugepageAlarms 先例）便于单测
// 锁住 scope/source/级别与文案要素。
func computeUnavailableAlarm(sink alarmSink, uri string, err error) {
	sink.Raise("compute", network.SeverityWarning, alarmCodeComputeUnavailable,
		"启动时未接入 libvirt，已降级运行：VM 生命周期动作不可用，已有配置声明不受影响（原因："+errReason(err)+"）。"+
			"请查底座实况：systemctl status libvirtd、journalctl -u libvirtd、virsh -c "+uri+" list。"+
			"底座恢复后执行 systemctl restart nfvis 恢复接入；当前产品不自动重连",
		"libvirt")
}

// containerUnavailableAlarm 启动期 Docker 未接入的降级告警（scope container、
// source docker、severity warning）。文案要素与 computeUnavailableAlarm 同一口径。
func containerUnavailableAlarm(sink alarmSink, socket string, err error) {
	sink.Raise("container", network.SeverityWarning, alarmCodeContainerUnavailable,
		"启动时未接入 Docker，已降级运行：容器生命周期动作不可用，已有配置声明不受影响（原因："+errReason(err)+"）。"+
			"请查底座实况：systemctl status docker、journalctl -u docker（socket: "+socket+"）。"+
			"底座恢复后执行 systemctl restart nfvis 恢复接入；当前产品不自动重连",
		"docker")
}

// errReason 告警文案里的原因兜底（nil 时如实说未知，不拼出「原因: <nil>」）。
func errReason(err error) string {
	if err == nil {
		return "未知"
	}
	return err.Error()
}

// 启动期 libvirt 连接重试参数（写入契约防膨胀）：3 次尝试（首次 + 2 次重试），每次沿用
// 既有 10s 上界，尝试间固定退避 1s ⇒ 启动期 libvirt 总上界 ≈32s。只对 libvirt 加重试
// （观测到的开机竞态只有它）；Docker 维持单次有界探测，不为未观测场景扩面。
const (
	libvirtConnectAttempts = 3
	libvirtRetryBackoff    = 1 * time.Second
)

// boundedRetry 以固定次数调用 connect（调用方自行保证单次有界）；非首次尝试前等待 backoff。
// 每次失败经 onRetry 上报（attempt 从 1 开始、含最后一次）；返回最后一次的错误（全败）或 nil。
// attempts <= 0 视为 1；connect 为 nil 时报错（防御）。纯函数：不在函数里打日志，失败经
// onRetry 回调交调用方决定（可单测锁住次数/退避/回调语义）。
func boundedRetry(attempts int, backoff time.Duration, onRetry func(attempt int, err error), connect func() error) error {
	if attempts <= 0 {
		attempts = 1
	}
	if connect == nil {
		return errors.New("boundedRetry：connect 为 nil")
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			time.Sleep(backoff)
		}
		err := connect()
		if err == nil {
			return nil
		}
		lastErr = err
		if onRetry != nil {
			onRetry(attempt, err)
		}
	}
	return lastErr
}
