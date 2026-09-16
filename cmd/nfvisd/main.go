// nfvisd NFViS 守护进程入口（骨架 §3.5 启动装配）。
//
// M2 首块装配：SQLite 存储 → 配置事务引擎 → AAA → REST API。
// 底座 Provider 为空实现（NewNoopApplier，骨架 §5：M2 可完整演示 CLI/API
// 事务，不含真实网络），M3/M4 替换为 govpp/libvirt/docker 编排器并接入恢复收敛。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
		hp1g          = flag.Int("hugepages-1g", 0, "1G 大页数量（安装期基线）")
		hp2m          = flag.Int("hugepages-2m", 0, "2M 大页数量（安装期基线）")
		isoCores      = flag.String("isolated-cores", "", "隔离核列表，如 4-15（安装期基线）")
		thp           = flag.String("thp", "", "transparent_hugepages: always|madvise|never")
		iommu         = flag.String("iommu", "", "iommu: on|off|pt")
		tuned         = flag.String("tuned-profile", "", "tuned 性能档名")
		extraParams   = flag.String("kernel-params", "", "附加内核参数（空格分隔）")
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
		frag, fstab := system.GenerateBaseline(d)
		fmt.Print(frag)
		fmt.Println("---FSTAB---")
		fmt.Print(fstab)
		if fstab != "" {
			fmt.Println()
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
	// M3-8：恢复收敛的不可收敛项落点（GET /alarms）
	alarms := network.NewAlarmStore()
	netProvider.SetAlarms(alarms)
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
	// vhost-user socket 目录须存在且可被 QEMU/VPP 访问（M4-P0 记录 §5）。
	if err := os.MkdirAll(computeCfg.VhostDir, 0o755); err != nil {
		log.Warn("创建 vhost-user socket 目录失败", "dir", computeCfg.VhostDir, "err", err)
	}
	libvirtCtx, libvirtCancel := context.WithTimeout(context.Background(), 10*time.Second)
	p, conn, cerr := compute.NewConnectedProvider(libvirtCtx, computeCfg)
	libvirtCancel()
	if cerr != nil {
		log.Warn("计算编排未接入（libvirt 连接失败），VM 生命周期不可用", "uri", computeCfg.URI, "err", cerr)
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
	if st, err := ctProvider.ContainerState(context.Background(), "__nfvis_probe__"); err == nil || st != "" {
		ctProvider.SetAlarms(alarms) // M4-9：容器收敛告警落点
		containerProvider, ctRuntime = ctProvider, ctProvider
		log.Info("容器编排已接入", "socket", ctCfg.Socket)
	} else {
		log.Warn("容器编排未接入（Docker 连接失败），容器生命周期不可用", "socket", ctCfg.Socket, "err", err)
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
		imagesStore.SetDockerLoader(func(path string) error {
			return ctProvider.LoadImage(context.Background(), path)
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
		orchestrator.WithVhostDir(computeCfg.VhostDir), orchestrator.WithMemifDir(ctCfg.MemifDir))

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
	// -allow-plaintext 是**权威开关**：显式给出即走明文（即便磁盘上已有自签证书）。
	// 否则「已存在证书」会让该开关看起来无效——真机验证时即踩到：带 -allow-plaintext
	// 启动却仍以 HTTPS 服务，明文客户端全被拒。
	if *tlsCert == "" && !*plaintext {
		if _, ok := tlsMgr.Info(); ok {
			*tlsCert, *tlsKey = tlsMgr.CertPath(), tlsMgr.KeyPath()
			log.Info("使用已安装的管理证书启用 HTTPS", "cert", *tlsCert)
		} else {
			info, generated, err := tlsMgr.EnsureSelfSigned(hostnameOr("nfvis"), system.ListenSANs(*listen))
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
		}
	} else {
		log.Warn("读取 committed 配置失败，日志级别与远程转发采用缺省", "err", err)
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

	// VPP 未运行时降级为告警并持续重连，不阻塞 nfvisd 启动。
	// M3-8：每次连接成功（首连=启动收敛，重连=VPP 重启重放）触发恢复收敛；
	// 单个对象失败不阻塞，未收敛项进告警表（FR-OPS-010/011）。
	var recoveryMu sync.Mutex
	runRecovery := func() {
		recoveryMu.Lock()
		defer recoveryMu.Unlock()
		cfg, err := engine.Committed()
		if err != nil {
			log.Error("恢复收敛：读取 committed 配置失败", "err", err)
			return
		}
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
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
		log.Info("恢复收敛完成")
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

	vppMgr.OnConnect(func(version string) { go runRecovery() })
	go func() {
		if err := vppMgr.Run(ctx); err != nil {
			log.Error("VPP 连接管理退出", "err", err)
		}
	}()
	startupApplier := &network.Applier{Mgr: vppMgr, PCI: network.NewSysfsPCIResolver(),
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
		Addr:        *listen,
		TLSCert:     *tlsCert,
		TLSKey:      *tlsKey,
		Log:         log,
		VPP:         &vppController{mgr: vppMgr, applier: startupApplier, engine: engine},
		L2:          &l2Controller{net: netProvider},
		L3:          &l3Controller{net: netProvider},
		LLDP:        &lldpController{net: netProvider},
		State:       state.New(vppMgr.Runtime()),
		SRIOV:       sriovProvider,
		DPDK:        &dpdkController{b: dpdkBinder},
		Kernel:      system.NewBaselineApplier(),
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
}

func (c *vppController) Status(vpp *model.VppConfig) api.VppStatus {
	v := c.mgr.StatusView(vpp)
	return api.VppStatus{Version: v.Version, Connected: v.Connected,
		PendingRestart: v.PendingRestart, LastError: v.LastError}
}

func (c *vppController) Restart(ctx context.Context, _ *model.VppConfig) error {
	cfg, err := c.engine.Committed()
	if err != nil {
		return err
	}
	_, err = c.applier.Apply(ctx, &cfg)
	return err
}

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
			ResolvedAt: a.ResolvedAt, State: a.State})
	}
	return out
}

// Clear 删除已 resolved 告警（M5-9，FR-OPS-022）。
func (c *alarmController) Clear(id string, all bool) int { return c.store.Clear(id, all) }

// diagController 装配 api.DiagRuntime（M3-9）：ping/traceroute/clear 统计。
type diagController struct{ diag *network.Diagnostics }

func (c *diagController) Ping(ctx context.Context, host, source, vrf string, count int) (string, error) {
	return c.diag.Ping(ctx, network.PingRequest{Host: host, Source: source, VRF: vrf, Count: count})
}

func (c *diagController) Traceroute(ctx context.Context, host, vrf string) (string, error) {
	return c.diag.Traceroute(ctx, network.TracerouteRequest{Host: host, VRF: vrf})
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

// kernelRelease 读取内核版本（诊断归档用）。
func kernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// nfvisdLogTail 取 nfvisd 日志尾部（systemd 单元优先，其次系统日志尾部）。
func nfvisdLogTail() ([]byte, error) {
	if out, err := exec.Command("journalctl", "-u", "nfvisd", "--no-pager", "-n", "500").Output(); err == nil && len(out) > 0 {
		return out, nil
	}
	out, err := exec.Command("journalctl", "--no-pager", "-n", "200").Output()
	if err != nil {
		return []byte("(journalctl unavailable: " + err.Error() + ")\n"), nil
	}
	return out, nil
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
func (c *tlsController) Regenerate(hostname string, ips []string) (system.TlsInfo, error) {
	return c.m.Regenerate(hostname, ips)
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
		if _, err := m.Regenerate(host, nil); err != nil {
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
type dpdkController struct{ b *network.DPDKBinder }

func (c *dpdkController) SetDPDKBound(ctx context.Context, ifname string, bound bool, driver string) (string, string, error) {
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
