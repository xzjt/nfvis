package api

// M5-9 收尾：`help [command]` 与 `start shell` 的守护进程侧实现。
//
// 契约 §1.3：`help [command]` 显示帮助；`start shell` 为 S 类命令且**仅本地 console 允许**
//（SSH 登录禁用）。start shell 的交互式 shell 注入不在 V1 范围（缺本地控制台集成），
// 故按契约先落实安全语义（SSH 一律拒绝），console 路径给出明确说明并记入 V2。

import (
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/schema"
)

// execHelp：`help` 列出顶层命令；`help <cmd> [sub…]` 显示该节点的描述与子命令。
func (x *cliExecutor) execHelp(class string, s *cliSession, args []string) string {
	root := schema.OperRoot()
	if s.Mode == "config" {
		root = schema.ConfigRoot()
	}
	if len(args) == 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "可用命令（%s 模式）：\n", modeName(s.Mode))
		for _, c := range root.Children {
			if !x.allow(class, c, c.Name) {
				continue
			}
			fmt.Fprintf(&b, "  %-34s %s\n", c.Name, c.Desc)
		}
		b.WriteString("输入 `?` 列出当前上下文候选，Tab 补全；`help <命令>` 查看子命令。\n")
		return b.String()
	}
	node, err := schema.Find(root, args...)
	if err != nil {
		return fmt.Sprintf("%% 无此命令: %s\n", strings.Join(args, " "))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n", strings.Join(args, " "), node.Desc)
	if len(node.Children) == 0 {
		return b.String()
	}
	b.WriteString("子命令:\n")
	for _, c := range node.Children {
		name := c.Name
		if c.Optional {
			name = "[" + name + "]"
		}
		fmt.Fprintf(&b, "  %-34s %s\n", name, c.Desc)
	}
	return b.String()
}

func modeName(mode string) string {
	if mode == "config" {
		return "配置"
	}
	return "操作"
}

// execStart：`start shell` —— 契约要求仅本地 console 允许。
func (x *cliExecutor) execStart(class, source string, args []string) string {
	if len(args) == 0 || args[0] != "shell" {
		return fmt.Sprintf("%% 无效命令: start %s（可用：start shell）\n", strings.Join(args, " "))
	}
	if source != "console" {
		return "%% start shell 仅允许本地 console 会话（SSH 登录禁用，契约 §1.3）\n"
	}
	return "%% start shell 未在 V1 提供（缺本地控制台集成，列入 V2）；请使用 CLI 命令或经 SSH 登录宿主\n"
}
