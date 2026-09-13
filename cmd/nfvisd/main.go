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
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
	"github.com/xzjt/nfvis/internal/state"
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
		tlsCert   = flag.String("tls-cert", "", "TLS 证书 PEM 路径（与 -tls-key 成对；缺省明文 HTTP，仅限开发）")
		tlsKey    = flag.String("tls-key", "", "TLS 私钥 PEM 路径")
		initAdmin = flag.String("init-admin-password", "", "首次启动引导 admin 用户的口令（缺省随机生成并打印一次）")
		vppSock   = flag.String("vpp-sock", envOr("NFVIS_VPP_SOCK", network.DefaultSocket), "VPP binary API 套接字（FR-SYS-007）")
		showVer   = flag.Bool("version", false, "输出版本后退出")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("nfvisd", api.VersionStr)
		return nil
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
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
	// M3-8：恢复收敛的不可收敛项落点（GET /alarms）
	alarms := network.NewAlarmStore()
	netProvider.SetAlarms(alarms)
	applier := orchestrator.NewApplier(netProvider, orchestrator.NewNoopCompute(), orchestrator.NewNoopContainer())

	engine, err := config.NewEngine(store, applier, config.Options{})
	if err != nil {
		return fmt.Errorf("装配事务引擎: %w", err)
	}
	defer engine.Close()

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
				log.Warn("恢复收敛未收敛项", "err", e)
			}
			return
		}
		log.Info("恢复收敛完成")
	}
	vppMgr.OnConnect(func(version string) { go runRecovery() })
	go func() {
		if err := vppMgr.Run(ctx); err != nil {
			log.Error("VPP 连接管理退出", "err", err)
		}
	}()
	startupApplier := &network.Applier{Mgr: vppMgr, PCI: network.NewSysfsPCIResolver(),
		Restarter: network.NewSystemctlRestarter(), RestartOnApply: true}

	apiServer := api.New(engine, aaaSvc, api.Options{
		Addr:    *listen,
		TLSCert: *tlsCert,
		TLSKey:  *tlsKey,
		Log:     log,
		VPP:     &vppController{mgr: vppMgr, applier: startupApplier, engine: engine},
		L2:      &l2Controller{net: netProvider},
		L3:      &l3Controller{net: netProvider},
		LLDP:    &lldpController{net: netProvider},
		State:   state.New(vppMgr.Runtime()),
		SRIOV:   network.NewSRIOVProvider(),
		NAT:     &natSessionsController{net: netProvider},
		Alarms:  &alarmController{store: alarms},
		Diag:    &diagController{diag: vppMgr.Diagnostics()},
	})

	srvErr := make(chan error, 1)
	go func() { srvErr <- apiServer.ListenAndServe() }()
	log.Info("nfvisd 就绪", "db", *dbPath, "listen", *listen)

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
