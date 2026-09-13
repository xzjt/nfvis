// nfvis-cli NFViS JunOS 风格 CLI 入口（薄客户端，骨架 §3.1）。
// 行编辑/补全/提示符/历史/空闲超时在 internal/cli；本文件只做参数解析与装配。
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/xzjt/nfvis/internal/cli"
	"github.com/xzjt/nfvis/pkg/cliclient"
)

func main() {
	var (
		server   = flag.String("server", "http://127.0.0.1:8443", "nfvisd 地址")
		user     = flag.String("u", "admin", "用户名")
		password = flag.String("p", os.Getenv("NFVIS_PASSWORD"), "口令（缺省读 NFVIS_PASSWORD）")
		source   = flag.String("source", "ssh", "接入源（ssh|console）")
		cmdline  = flag.String("c", "", "执行多行命令后退出（换行分隔）")
		showVer  = flag.Bool("version", false, "输出版本后退出")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("nfvis-cli", cli.Version)
		return
	}

	client := cliclient.New(*server)
	fmt.Printf("连接 %s ...\n", *server)
	if err := client.Login(*user, *password); err != nil {
		fmt.Fprintf(os.Stderr, "%% 登录失败: %v\n", err)
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
