// nfvis-cli NFViS JunOS 风格 CLI 入口（薄客户端，骨架 §3.1）。
// 行编辑/补全/提示符/会话逻辑在 internal/cli；本文件只做参数解析与装配。
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

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
	repl(session)
}

// runScript 多行脚本模式：任一行失败即停止。
func runScript(session *cli.Session, cmdline string) {
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
			os.Exit(1)
		}
	}
}

// repl 交互模式：行读取 + 本地 ?/Tab 补全 + 远端执行。
func repl(session *cli.Session) {
	fmt.Println("NFViS CLI（M2）——? 列出候选 / Tab 补全 / commit confirmed 演示")
	in := bufio.NewScanner(os.Stdin)
	prompt := session.Prompt()
	for {
		fmt.Print(prompt)
		if !in.Scan() {
			fmt.Println()
			return
		}
		line := strings.TrimRight(in.Text(), "\t")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasSuffix(strings.TrimSpace(line), "?") {
			for _, c := range session.Candidates(line) {
				fmt.Printf("  %-24s%s\n", c.Token, c.Desc)
			}
			continue
		}
		if strings.HasSuffix(line, "\t") {
			line = session.CompleteLine(line)
			fmt.Println(line) // 行级 REPL 不做光标定位，重打补全结果
		}
		out, next := session.ExecuteLine(strings.TrimSpace(line))
		prompt = next
		if out != "" {
			fmt.Print(out)
			if !strings.HasSuffix(out, "\n") {
				fmt.Println()
			}
		}
	}
}
