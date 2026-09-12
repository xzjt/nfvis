// nfvis-cli NFViS JunOS 风格 CLI 前端（薄客户端，骨架 §3.1）。
//
// ?/Tab 补全依据本地 schema 包（编译期共享，连接断开时仍可编辑提示，
// §3.3）；执行一律经 cliclient 转发 nfvisd——本文件不得 import
// internal/config 等业务包。
//
// 模式：
//
//	nfvis-cli                          交互模式（REPL）
//	nfvis-cli -c "configure\nset ..."  多行脚本模式（分号或换行分隔，
//	                                   任一行失败即停止）
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/xzjt/nfvis/internal/schema"
	"github.com/xzjt/nfvis/pkg/cliclient"
)

// cliSource CLI 接入源（影响 FR-CFG-012 管理口自锁判定：ssh 需 confirmed）。
var cliSource = "ssh"

// cliMode 会话模式与层级（本地渲染提示符与补全上下文）。
var (
	cliMode = "oper"
	cliPath []string
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
		fmt.Println("nfvis-cli", apiVersion)
		return
	}
	cliSource = *source

	client := cliclient.New(*server)
	fmt.Printf("连接 %s ...\n", *server)
	if err := client.Login(*user, *password); err != nil {
		fmt.Fprintf(os.Stderr, "%% 登录失败: %v\n", err)
		os.Exit(1)
	}

	if *cmdline != "" {
		lines := strings.FieldsFunc(*cmdline, func(r rune) bool { return r == '\n' || r == ';' })
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			res, err := client.Execute(line, cliSource)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%% %v\n", err)
				os.Exit(1)
			}
			cliMode, cliPath = res.Mode, res.Path
			if res.Output != "" {
				fmt.Print(res.Output)
				if !strings.HasSuffix(res.Output, "\n") {
					fmt.Println()
				}
			}
			if strings.Contains(res.Output, "%%") {
				os.Exit(1) // 脚本模式：任一行失败即停止
			}
		}
		return
	}

	repl(client)
}

// repl 交互模式：行读取 + 本地 ?/Tab 补全 + 远端执行。
func repl(client *cliclient.Client) {
	fmt.Println("NFViS CLI（M2）——? 列出候选 / Tab 补全 / commit confirmed 演示")
	fmt.Println("试试: configure → set system hostname demo → commit → show configuration")
	in := bufio.NewScanner(os.Stdin)
	prompt := "nfvis> "
	for {
		fmt.Print(prompt)
		if !in.Scan() {
			fmt.Println()
			return
		}
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		if line == "?" || strings.HasSuffix(line, " ?") {
			printCandidates(line)
			continue
		}
		if strings.HasSuffix(line, "\t") {
			line = completeLine(strings.TrimSuffix(line, "\t"))
			// 重新提示已补全的行（行级 REPL 不做光标定位）
			fmt.Println(line)
		}
		res, err := client.Execute(line, cliSource)
		if err != nil {
			fmt.Printf("%% %v\n", err)
			continue
		}
		cliMode, cliPath = res.Mode, res.Path
		prompt = res.Prompt
		if res.Output != "" {
			fmt.Print(res.Output)
			if !strings.HasSuffix(res.Output, "\n") {
				fmt.Println()
			}
		}
	}
}

// completionTokens 解析补全上下文：已完成 token 与正在输入的前缀。
func completionTokens(line string) ([]string, string) {
	tokens := strings.Fields(line)
	if len(tokens) == 0 {
		return nil, ""
	}
	return tokens[:len(tokens)-1], tokens[len(tokens)-1]
}

func rootForContext(tokens []string) *schema.Node {
	if len(tokens) > 0 {
		switch tokens[0] {
		case "set", "delete", "edit":
			return schema.ConfigRoot() // set/delete/edit 下挂配置语句树
		case "run":
			return schema.OperRoot()
		}
	}
	if cliMode == "config" {
		return schema.ConfigRoot()
	}
	return schema.OperRoot()
}

// printCandidates 处理 ?：列出当前位置候选（FR-CLI-001/002）。
func printCandidates(line string) {
	line = strings.TrimSuffix(line, "?")
	tokens, partial := completionTokens(line)
	root := rootForContext(tokens)
	cs := schema.Candidates(root, tokens, partial, nil) // 动态候选退化为占位提示（§5.3）
	if len(cs) == 0 {
		fmt.Println("%% 无可用候选")
		return
	}
	width := 0
	for _, c := range cs {
		if len(c.Token) > width {
			width = len(c.Token)
		}
	}
	width += 2
	for _, c := range cs {
		fmt.Printf("  %-*s%s\n", width, c.Token, c.Desc)
	}
}

// completeLine 处理 Tab：唯一匹配补全，多匹配补到公共前缀（FR-CLI-003/§5.2）。
func completeLine(line string) string {
	tokens, partial := completionTokens(line)
	root := rootForContext(tokens)
	cs := schema.Candidates(root, tokens, partial, nil)
	if len(cs) == 0 {
		return line
	}
	if len(cs) == 1 {
		return strings.TrimSpace(strings.TrimSuffix(line, partial) + cs[0].Token + " ")
	}
	common := commonPrefix(cs)
	if len(common) > len(partial) {
		return strings.TrimSuffix(line, partial) + common
	}
	return line
}

func commonPrefix(cs []schema.Candidate) string {
	if len(cs) == 0 {
		return ""
	}
	p := cs[0].Token
	for _, c := range cs[1:] {
		for !strings.HasPrefix(c.Token, p) {
			p = p[:len(p)-1]
			if p == "" {
				return ""
			}
		}
	}
	return p
}

const apiVersion = "1.0.0-dev"
