package main

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// ---------- 行编辑器：raw 模式 + ?/Tab 补全 + 历史 ----------
// 真实实现中补全候选来自 internal/schema（编译期共享）；原型直接用本地树。

const (
	escCUP = "\r\x1b[K" // 回行首并清除本行
	bell   = "\a"
	back10 = "\x1b[D"
)

type Editor struct {
	termFD    int
	oldState  *term.State
	raw       bool
	line      []rune
	cursor    int
	history   []string
	histPos   int // 浏览历史时的位置（-1 = 不在历史中）
	savedLine string
}

func RunREPL(e *Engine) {
	ed := NewEditor()
	defer ed.Close()
	fmt.Println("NFViS CLI 交互原型 —— 模拟后端")
	fmt.Println("演示：? 补全列表 / Tab 补全 / commit confirmed / rollback / compare")
	fmt.Println("试试: show vir<Tab>、show virtual-machine-functions <Tab>、configure、set system hostname x、commit confirmed")
	for {
		prompt := e.Prompt()
		line, err := ed.ReadLine(prompt, e)
		if err != nil {
			fmt.Println()
			return
		}
		if line == "" {
			continue
		}
		ed.pushHistory(line)
		out := e.Execute(line)
		if out == "\x00exit" {
			return
		}
		if out != "" {
			fmt.Print(out)
			if !strings.HasSuffix(out, "\n") {
				fmt.Println()
			}
		}
		// commit confirmed 超时回滚等异步输出后需要换行保护
	}
}

func NewEditor() *Editor {
	ed := &Editor{termFD: int(os.Stdin.Fd()), histPos: -1}
	if st, err := term.MakeRaw(ed.termFD); err == nil {
		ed.oldState = st
		ed.raw = true
	}
	return ed
}

func (ed *Editor) Close() {
	if ed.raw {
		term.Restore(ed.termFD, ed.oldState)
		fmt.Println()
	}
}

func (ed *Editor) pushHistory(line string) {
	if len(ed.history) == 0 || ed.history[len(ed.history)-1] != line {
		ed.history = append(ed.history, line)
	}
	ed.histPos = -1
}

// ReadLine 读取一行；raw=false 时退化为普通行输入（无补全）
func (ed *Editor) ReadLine(prompt string, e *Engine) (string, error) {
	if !ed.raw {
		fmt.Print(prompt)
		buf := make([]byte, 0, 256)
		tmp := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(tmp)
			if err != nil || (n == 1 && tmp[0] == '\n') {
				return string(buf), err
			}
			if n == 1 && tmp[0] != '\r' {
				buf = append(buf, tmp[0])
			}
		}
	}
	ed.line = nil
	ed.cursor = 0
	ed.histPos = -1
	fmt.Print(prompt)
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return "", err
		}
		if n == 0 {
			continue
		}
		b := buf[0]
		switch {
		case b == '\r' || b == '\n':
			fmt.Print("\r\n")
			return string(ed.line), nil
		case b == 0x7f || b == 0x08: // Backspace
			ed.backspace(prompt)
		case b == 0x03: // Ctrl-C：放弃当前行
			ed.line = nil
			ed.cursor = 0
			fmt.Print("\r\n")
			fmt.Print(prompt)
			fmt.Print(escCUP + prompt)
		case b == 0x04: // Ctrl-D：空行退出
			if len(ed.line) == 0 {
				fmt.Print("\r\n")
				return "", fmt.Errorf("eof")
			}
		case b == 0x09: // Tab
			ed.complete(prompt, e, false)
		case b == '?': // JunOS 风格：? 即列出候选并保留输入
			ed.complete(prompt, e, true)
		case b == 0x1b: // ESC 序列（或 Windows 扫描码前缀 0xE0 亦经此处理不了，另开分支）
			ed.handleEscape(prompt)
		case b >= 0x20 && b < 0x7f, b >= 0x80: // 可打印 ASCII 与 UTF-8 多字节
			ed.insertRune(b, prompt)
		}
	}
}

func (ed *Editor) insertRune(b byte, prompt string) {
	r := rune(b)
	if b >= 0x80 { // UTF-8 续字节简化处理：等待完整序列（原型按 latin-1 回退即可）
		r = rune(b)
	}
	if ed.cursor == len(ed.line) {
		ed.line = append(ed.line, r)
		ed.cursor++
		fmt.Print(string(r))
	} else {
		ed.line = append(ed.line[:ed.cursor], append([]rune{r}, ed.line[ed.cursor:]...)...)
		ed.cursor++
		ed.redraw(prompt)
	}
}

func (ed *Editor) backspace(prompt string) {
	if ed.cursor == 0 {
		fmt.Print(bell)
		return
	}
	ed.line = append(ed.line[:ed.cursor-1], ed.line[ed.cursor:]...)
	ed.cursor--
	if ed.cursor == len(ed.line) {
		fmt.Print("\b \b") // 行尾删除：回退一格并抹除字符
	} else {
		ed.redraw(prompt) // 行中删除：整行重绘并恢复光标位置
	}
}

func (ed *Editor) redraw(prompt string) {
	fmt.Print("\r\x1b[K" + prompt + string(ed.line))
	// 光标移回 cursor 位置
	if back := len(ed.line) - ed.cursor; back > 0 {
		fmt.Printf("\x1b[%dD", back)
	}
}

// handleEscape 处理 ESC 序列与 Windows 扫描码（0xE0/0x00 前缀）
func (ed *Editor) handleEscape(prompt string) {
	var seq [3]byte
	if _, err := os.Stdin.Read(seq[:1]); err != nil {
		return
	}
	switch seq[0] {
	case '[':
		if _, err := os.Stdin.Read(seq[1:2]); err != nil {
			return
		}
		switch seq[1] {
		case 'A': // Up
			ed.historyNav(prompt, -1)
		case 'B': // Down
			ed.historyNav(prompt, 1)
		case 'C': // Right
			if ed.cursor < len(ed.line) {
				ed.cursor++
				fmt.Print("\x1b[C")
			}
		case 'D': // Left
			if ed.cursor > 0 {
				ed.cursor--
				fmt.Print("\x1b[D")
			}
		case 'H': // Home
			ed.cursor = 0
			ed.redraw(prompt)
		case 'F': // End
			ed.cursor = len(ed.line)
			ed.redraw(prompt)
		case '3': // Delete
			if _, err := os.Stdin.Read(seq[2:]); err == nil && seq[2] == '~' {
				if ed.cursor < len(ed.line) {
					ed.line = append(ed.line[:ed.cursor], ed.line[ed.cursor+1:]...)
					ed.redraw(prompt)
				}
			}
		}
	case 'O':
		if _, err := os.Stdin.Read(seq[1:2]); err == nil {
			switch seq[1] {
			case 'H':
				ed.cursor = 0
				ed.redraw(prompt)
			case 'F':
				ed.cursor = len(ed.line)
				ed.redraw(prompt)
			}
		}
	case 0xe0, 0x00: // Windows 控制台扫描码前缀
		if _, err := os.Stdin.Read(seq[1:2]); err != nil {
			return
		}
		switch seq[1] {
		case 'H':
			ed.historyNav(prompt, -1)
		case 'P':
			ed.historyNav(prompt, 1)
		case 'K':
			if ed.cursor > 0 {
				ed.cursor--
				fmt.Print("\x1b[D")
			}
		case 'M':
			if ed.cursor < len(ed.line) {
				ed.cursor++
				fmt.Print("\x1b[C")
			}
		}
	}
}

func (ed *Editor) historyNav(prompt string, dir int) {
	if len(ed.history) == 0 {
		return
	}
	if dir < 0 {
		if ed.histPos == -1 {
			ed.savedLine = string(ed.line)
			ed.histPos = len(ed.history) - 1
		} else if ed.histPos > 0 {
			ed.histPos--
		}
	} else {
		if ed.histPos == -1 {
			return
		}
		if ed.histPos < len(ed.history)-1 {
			ed.histPos++
		} else {
			ed.histPos = -1
			ed.line = []rune(ed.savedLine)
			ed.cursor = len(ed.line)
			ed.redraw(prompt)
			return
		}
	}
	ed.line = []rune(ed.history[ed.histPos])
	ed.cursor = len(ed.line)
	ed.redraw(prompt)
}

// complete 处理 ?/Tab。keepInput=true（?）时先列出候选再重绘输入行；
// false（Tab）时唯一匹配自动补全，多匹配响铃列出。
func (ed *Editor) complete(prompt string, e *Engine, keepInput bool) {
	line := string(ed.line)
	tokens := strings.Fields(line)
	trailingSpace := len(line) > 0 && (line[len(line)-1] == ' ' || line[len(line)-1] == '\t')
	if trailingSpace {
		// 补全的是新 token
	} else if len(tokens) > 0 {
		tokens = tokens[:len(tokens)-1]
	}
	prefix := ""
	if !trailingSpace && len(line) > 0 {
		parts := strings.Fields(line)
		prefix = parts[len(parts)-1]
	}

	root := e.completionRoot()
	node, depth := walkTree(root, tokens)
	var cands [][2]string
	if node != nil {
		cands = node.candidates(prefix, e)
	}
	// walkTree 对 KindValue/参数已消耗 token 的情形：depth 表示已匹配的 token 数
	// 若 node 是 KindValue 且我们正处在其值的位置，candidates 为空——给出类型提示
	if node != nil && node.Kind == KindValue && depth == len(tokens) {
		cands = nil
	}

	if len(cands) == 0 {
		fmt.Print(bell)
		if keepInput {
			fmt.Print("\r\n%% 无可用补全")
			fmt.Print("\r\n" + escCUP + prompt + string(ed.line))
			if back := len(ed.line) - ed.cursor; back > 0 {
				fmt.Printf("\x1b[%dD", back)
			}
		}
		return
	}

	if !keepInput && len(cands) == 1 && cands[0][0] != prefix {
		// 唯一匹配：补全
		add := strings.TrimPrefix(cands[0][0], prefix)
		ed.line = append(ed.line[:ed.cursor], []rune(add+" ")...)
		ed.cursor = len(ed.line)
		fmt.Print(add + " ")
		return
	}
	if !keepInput && len(cands) == 1 && cands[0][0] == prefix {
		// 已输入完整：补空格
		ed.line = append(ed.line, ' ')
		ed.cursor = len(ed.line)
		fmt.Print(" ")
		return
	}

	// 多候选（或 ?）：列出
	fmt.Print("\r\n")
	// 计算公共前缀供 Tab 补全
	common := commonPrefix(cands)
	if !keepInput && strings.HasPrefix(common, prefix) && len(common) > len(prefix) {
		add := strings.TrimPrefix(common, prefix)
		ed.line = append(ed.line[:ed.cursor], []rune(add)...)
		ed.cursor = len(ed.line)
	}
	width := 0
	for _, c := range cands {
		if len(c[0]) > width {
			width = len(c[0])
		}
	}
	width += 2
	for _, c := range cands {
		fmt.Printf("  %-*s%s\n", width, c[0], c[1])
	}
	fmt.Print(escCUP + prompt + string(ed.line))
	if back := len(ed.line) - ed.cursor; back > 0 {
		fmt.Printf("\x1b[%dD", back)
	}
}

// walkTree 沿树匹配 tokens，返回（当前节点, 已匹配 token 数）
func walkTree(root *Node, tokens []string) (*Node, int) {
	n := root
	depth := 0
	for _, tk := range tokens {
		if n.Kind == KindValue {
			// 该 token 是本节点的值，节点子树通常为空
			depth++
			continue
		}
		c := n.child(tk)
		if c == nil {
			// 关键字未命中：尝试参数节点（token 作为实例名消耗）
			for _, ch := range n.Children {
				if ch.Kind == KindParam {
					c = ch
					break
				}
			}
			if c == nil {
				break
			}
		}
		n = c
		depth++
	}
	return n, depth
}

// completionRoot 按模式返回当前补全根：操作树 or 配置命令树
// （配置模式下 set/delete/show 的 path 部分由 configPathTree 接管）
func (e *Engine) completionRoot() *Node {
	return &Node{Children: e.completionChildren()}
}

func (e *Engine) completionChildren() []*Node {
	if e.Mode == "oper" {
		return operTree().Children
	}
	// 配置模式：顶级命令 + set/delete/show 挂配置路径树
	top := configTopTree().Children
	var out []*Node
	for _, t := range top {
		switch t.Name {
		case "set", "delete", "show", "edit":
			n := *t
			n.Children = append([]*Node{}, configPathTree().Children...)
			out = append(out, &n)
		case "run":
			n := *t
			n.Children = operTree().Children
			out = append(out, &n)
		default:
			out = append(out, t)
		}
	}
	return out
}

func commonPrefix(cands [][2]string) string {
	if len(cands) == 0 {
		return ""
	}
	p := cands[0][0]
	for _, c := range cands[1:] {
		for !strings.HasPrefix(c[0], p) {
			p = p[:len(p)-1]
			if p == "" {
				return ""
			}
		}
	}
	return p
}
