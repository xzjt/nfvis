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
	"syscall"
	"time"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/api"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/orchestrator"
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

	engine, err := config.NewEngine(store, orchestrator.NewNoopApplier(), config.Options{})
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

	apiServer := api.New(aaaSvc, api.Options{
		Addr:    *listen,
		TLSCert: *tlsCert,
		TLSKey:  *tlsKey,
		Log:     log,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
