// nfvis-cli NFViS JunOS 风格 CLI 入口（薄客户端，骨架 §3.1）。
// 行编辑/补全/提示符/历史/空闲超时在 internal/cli；本文件只做参数解析与装配。
package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/xzjt/nfvis/internal/cli"
	"github.com/xzjt/nfvis/pkg/cliclient"
)

func main() {
	var (
		// 缺省须与守护进程缺省一致：deploy/nfvis.service 设 NFVIS_LISTEN=:443，且 nfvisd
		// 未提供证书时自动自签并启用 HTTPS（决策 #72）。此前缺省是明文 http://…:8443，
		// 与守护进程默认不匹配 → 默认参数连不上（决策 #78）。
		// 可用 NFVIS_SERVER 覆盖（与 NFVIS_PASSWORD 同风格），便于脚本/自动化。
		server   = flag.String("server", envOr("NFVIS_SERVER", cliclient.DefaultServer), "nfvisd 地址（缺省读 NFVIS_SERVER）")
		user     = flag.String("u", "admin", "用户名")
		password = flag.String("p", os.Getenv("NFVIS_PASSWORD"), "口令（缺省读 NFVIS_PASSWORD）")
		source   = flag.String("source", "ssh", "接入源（ssh|console）")
		cmdline  = flag.String("c", "", "执行多行命令后退出（换行分隔）")
		showVer  = flag.Bool("version", false, "输出版本后退出")
		caFile   = flag.String("ca", "", "服务端证书 PEM（HTTPS 校验；缺省尝试固定本机 nfvisd 证书）")
		insecure = flag.Bool("insecure", false, "跳过 HTTPS 证书校验（仅限调试）")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("nfvis-cli", cli.Version)
		return
	}

	client := mustClient(*server, *caFile, *insecure)
	fmt.Printf("连接 %s ...\n", *server)
	if err := client.Login(*user, *password); err != nil {
		fmt.Fprintf(os.Stderr, "%% 登录失败: %v\n", err)
		if isConnErr(err) {
			// 最常见的两个原因：守护进程未起 / 监听地址与端口不是缺省。
			fmt.Fprintf(os.Stderr,
				"%% 提示: 确认 nfvisd 已运行（systemctl status nfvis）；"+
					"若其监听地址/端口非缺省（当前尝试 %s），用 -server 或 NFVIS_SERVER 指定，"+
					"并注意开发用 -allow-plaintext 时须把地址写成 http://…\n", *server)
		}
		os.Exit(1)
	}
	session := cli.New(client, *source)

	if *cmdline != "" {
		runScript(session, *cmdline)
		return
	}
	// 空闲超时取 system idle-timeout-minutes，缺省 10 分钟（FR-SEC-005/FR-CLI-006）。
	timeout := cli.DefaultIdleTimeout
	if m, err := client.IdleTimeoutMinutes(); err == nil && m > 0 {
		timeout = time.Duration(m) * time.Minute
	}
	repl := cli.NewREPL(session, cli.NewHistory(), cli.NewIdleGuard(timeout, nil))
	if err := repl.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "%% %v\n", err)
		os.Exit(1)
	}
}

// runScript 多行脚本模式：任一行失败即停止；结束时清理会话。
//
// 收尾必须清理（会话按 user@source 在服务端保留）：否则脚本会残留配置模式、
// 脏 candidate 与 candidate 会话锁，导致后续调用被按上一模式解释、
// 再次以 exit 收尾时报「存在未提交变更」并失败（见 docs/reviews/2026-09-13.md）。
func runScript(session *cli.Session, cmdline string) {
	failed := false
	for _, line := range strings.Split(cmdline, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out, _ := session.ExecuteLine(line)
		fmt.Print(out)
		if !strings.HasSuffix(out, "\n") {
			fmt.Println()
		}
		if strings.Contains(out, "%%") {
			failed = true
			break
		}
	}
	teardownScript(session)
	if failed {
		os.Exit(1)
	}
}

// teardownScript 退出配置模式（有 candidate 先丢弃）、释放会话并吊销 token。
func teardownScript(session *cli.Session) {
	if session.Mode == "config" {
		// discard 释放 candidate 与会话锁（无变更时也安全）
		if out, _ := session.ExecuteLine("discard"); strings.Contains(out, "%%") {
			fmt.Print(out)
		}
		if out, _ := session.ExecuteLine("exit"); strings.Contains(out, "%%") {
			fmt.Print(out)
		}
	}
	session.Logout()
}

// mustClient 构造 REST 客户端（FR-SEC-004：默认自签 HTTPS）。
//
// 校验策略：显式 -ca > 固定本机 nfvisd 证书（缺省，CLI 通常运行在一体机上）> 显式 -insecure。
// 均不满足时仍按系统信任库校验（自签会失败并给出明确提示）。
func mustClient(server, caFile string, insecure bool) *cliclient.Client {
	opts := cliclient.TLSOptions{CAFile: caFile, Insecure: insecure}
	if opts.CAFile == "" && !insecure && strings.HasPrefix(server, "https://") {
		if _, err := os.Stat(cliclient.DefaultServerCertPath); err == nil {
			opts.CAFile = cliclient.DefaultServerCertPath
		}
	}
	c, err := cliclient.NewWithTLS(server, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化客户端失败:", err)
		os.Exit(1)
	}
	return c
}

// envOr 取环境变量，为空时用默认值。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// isConnErr 判断是否为「连不上」类错误（网络/超时），用于给出排障提示。
// 证书校验失败等**不**算在内——那属于配置问题，报错本身已足够明确。
func isConnErr(err error) bool {
	var nerr net.Error
	if errors.As(err, &nerr) {
		return true
	}
	var oerr *net.OpError
	return errors.As(err, &oerr)
}
