// nfvis-cli-proto —— CLI 补全引擎薄演示（T0-2）。
//
// M2 之后产品实现已归位：命令树在 internal/schema（nfvisd 与 nfvis-cli 编译期
// 共享）、行编辑/补全/历史在 internal/cli、事务引擎在 internal/config。本原型
// 不再自带这些逻辑的副本（曾与产品漂移），改为直接引用 internal/schema 的命令树，
// 演示 ? 列候选、Tab 补全与无歧义缩写消歧，保留命令树的教学价值。
//
// 需要事务/行编辑/history 的真实交互，请使用 nfvis-cli（cmd/nfvis-cli，经
// nfvisd API）。
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/xzjt/nfvis/internal/schema"
)

func main() {
	in := bufio.NewScanner(os.Stdin)
	fmt.Println("nfvis-cli-proto（T0-2 薄演示，命令树引用 internal/schema）")
	fmt.Println("? 列候选 / Tab 补全 / 输入命令演示缩写消歧；Ctrl-D 退出")
	for {
		fmt.Print("nfvis> ")
		if !in.Scan() {
			fmt.Println()
			return
		}
		line := in.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		root := rootFor(trimmed)
		if strings.HasSuffix(trimmed, "?") { // 列候选
			tokens, partial := split(line)
			for _, c := range schema.Candidates(root, tokens, partial, nil) {
				fmt.Printf("  %-28s%s\n", c.Token, c.Desc)
			}
			continue
		}
		if strings.HasSuffix(line, "\t") { // Tab 补全
			fmt.Println(complete(root, line))
			continue
		}
		// 其余：演示无歧义前缀消歧（FR-CLI-004），歧义时列出候选
		toks, err := schema.Canonicalize(root, strings.Fields(trimmed))
		if err != nil {
			fmt.Println("%", err)
			continue
		}
		fmt.Println(strings.Join(toks, " "))
	}
}

// rootFor 依首个 token 选择补全根（与 nfvis-cli 的上下文规则一致）。
func rootFor(line string) *schema.Node {
	fields := strings.Fields(line)
	if len(fields) > 0 {
		switch fields[0] {
		case "set", "delete", "edit":
			return schema.ConfigRoot()
		}
	}
	return schema.OperRoot()
}

// split 解析补全上下文：已完成 token 与正在输入的前缀（尾随空白=新 token）。
func split(line string) ([]string, string) {
	line = strings.TrimRight(line, "?")
	trailing := len(line) > 0 && (line[len(line)-1] == ' ' || line[len(line)-1] == '\t')
	tokens := strings.Fields(line)
	switch {
	case trailing || len(tokens) == 0:
		return tokens, ""
	default:
		return tokens[:len(tokens)-1], tokens[len(tokens)-1]
	}
}

// complete Tab：唯一匹配补全并附空格，多匹配补到公共前缀。
func complete(root *schema.Node, line string) string {
	line = strings.TrimRight(line, "\t")
	tokens, partial := split(line)
	cs := schema.Candidates(root, tokens, partial, nil)
	if len(cs) == 0 {
		return line
	}
	base := strings.TrimSuffix(line, partial)
	if len(cs) == 1 {
		return base + cs[0].Token + " "
	}
	p := cs[0].Token
	for _, c := range cs[1:] {
		for !strings.HasPrefix(c.Token, p) {
			p = p[:len(p)-1]
		}
	}
	if len(p) > len(partial) {
		return base + p
	}
	return line
}
