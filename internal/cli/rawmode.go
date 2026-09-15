package cli

// W2：raw 模式行编辑器（FR-CLI-006，借鉴 prototype/editor.go）。
// ↑/↓ 历史、Ctrl-R 反查、?/Tab 补全、Ctrl-C 放弃、Ctrl-D 退出。
// 非 TTY 环境（管道/脚本）自动退化为行读取（ReadLine 的 plain 分支）。
//
// 输出统一经 e.out（NewEditor 下为 raw 感知的 crlfWriter，见 output.go）：
// raw 期间裸 \n 不回车，命令输出会阶梯错位，故禁止直接用 fmt.Print 写 stdout。

import (
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/term"

	"github.com/xzjt/nfvis/internal/schema"
)

// Completer 补全回调：给定当前行，返回补全后的行与该位置的全部候选。
// 候选供 Tab 多匹配与 `?` 列出（契约 §5.1/§5.2）；行文本未变即表示补全无进展。
type Completer func(string) (string, []schema.Candidate)

// Editor raw 模式行编辑器。
type Editor struct {
	in         *os.File
	out        io.Writer
	oldState   *term.State
	raw        bool
	history    *History
	idle       *IdleGuard
	complete   Completer
	line       []rune
	cursor     int
	savedLine  string
	lastWakeup time.Time
}

// NewEditor 构造（stdin TTY 时启用 raw 模式）。
func NewEditor(h *History, idle *IdleGuard) *Editor {
	if idle == nil {
		idle = NewIdleGuard(0, nil)
	}
	e := &Editor{in: os.Stdin, history: h, idle: idle}
	// raw 期间终端 OPOST 被关闭，裸 \n 不回车：命令输出必须经此转换（决策 #81）。
	e.out = &crlfWriter{w: os.Stdout, raw: func() bool { return e.raw }}
	if st, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
		e.oldState = st
		e.raw = true
	}
	return e
}

// SetCompleter 注入 Tab/? 补全回调（FR-CLI-003，由 Session 提供）。
func (e *Editor) SetCompleter(fn Completer) { e.complete = fn }

// Out 输出流（raw 感知）。REPL 的命令输出须改经此处，否则换行错位。
func (e *Editor) Out() io.Writer { return e.out }

// Close 恢复终端状态。
func (e *Editor) Close() {
	if e.raw {
		_ = term.Restore(int(e.in.Fd()), e.oldState)
		e.raw = false // 终端已恢复 NL→CRNL，后续输出不得再自行补 CR
		fmt.Fprintln(e.out)
	}
}

// IsRaw 是否处于 raw 模式。
func (e *Editor) IsRaw() bool { return e.raw }

// Suspend 临时退出 raw 模式（恢复终端默认信号处理，使 Ctrl-C 产生 SIGINT）；
// 返回原本是否处于 raw 模式，供 Resume 还原。monitor 实时刷新用（M3-9）。
func (e *Editor) Suspend() bool {
	if !e.raw || e.oldState == nil {
		return false
	}
	_ = term.Restore(int(e.in.Fd()), e.oldState)
	e.raw = false
	return true
}

// Resume 按 Suspend 的返回值重新进入 raw 模式。
func (e *Editor) Resume(wasRaw bool) {
	if !wasRaw {
		return
	}
	if st, err := term.MakeRaw(int(e.in.Fd())); err == nil {
		e.oldState = st
		e.raw = true
	}
}

// IdleExpired 空闲是否超时（每次按键刷新活动时间）。
func (e *Editor) IdleExpired() bool { return e.idle.Expired() }

// ReadLine 读取一行；prompt 为提示符。返回 (line, err)。
// err == io.EOF 表示退出（Ctrl-D/EOF）。
func (e *Editor) ReadLine(prompt string) (string, error) {
	if !e.raw {
		return e.readPlain(prompt)
	}
	e.line = nil
	e.cursor = 0
	e.history.SearchCancel()
	fmt.Fprint(e.out, prompt)
	buf := make([]byte, 1)
	for {
		n, err := e.in.Read(buf)
		if err != nil {
			return "", err
		}
		if n == 0 {
			continue
		}
		e.idle.Touch()
		b := buf[0]
		switch {
		case b == '\r' || b == '\n':
			fmt.Fprint(e.out, "\r\n")
			return string(e.line), nil
		case b == 0x7f || b == 0x08: // Backspace
			e.backspace(prompt)
		case b == 0x03: // Ctrl-C：放弃当前行
			e.line = nil
			e.cursor = 0
			fmt.Fprint(e.out, "^C\r\n", prompt)
		case b == 0x04: // Ctrl-D：空行退出
			if len(e.line) == 0 {
				fmt.Fprint(e.out, "\r\n")
				return "", io.EOF
			}
		case b == 0x12: // Ctrl-R：反查
			e.search(prompt)
		case b == 0x1b: // ESC 序列
			e.handleEscape(prompt)
		case b == '\t':
			e.completeLine(prompt)
		case b == '?':
			e.helpLine(prompt) // 按键即时列出候选，不进入行文本（§5.1）
		case b >= 0x20 && b < 0x7f, b >= 0x80:
			e.insertRune(b, prompt)
		}
	}
}

// readPlain 非 TTY 退化路径。
func (e *Editor) readPlain(prompt string) (string, error) {
	fmt.Fprint(e.out, prompt)
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 1)
	for {
		n, err := e.in.Read(tmp)
		if err != nil || (n == 1 && tmp[0] == '\n') {
			return string(buf), err
		}
		if n == 1 && tmp[0] != '\r' {
			buf = append(buf, tmp[0])
		}
	}
}

// completeLine Tab：唯一匹配补全/多匹配补公共前缀（FR-CLI-003/§5.2）。
// 补全有进展时只改写行文本；多匹配且无进展才响铃并列出（与 `?` 同）。
func (e *Editor) completeLine(prompt string) {
	if e.complete == nil {
		return
	}
	nl, cs := e.complete(string(e.line))
	if nl != string(e.line) {
		e.line = []rune(nl)
		e.cursor = len(e.line)
		e.redraw(prompt)
		return
	}
	fmt.Fprint(e.out, "\a")
	if len(cs) > 1 {
		e.listCandidates(prompt, cs)
	}
}

// helpLine 输入 `?`：立即列出当前 token 位置的候选并回显已输入部分（FR-CLI-002/§5.1）。
// `?` 是按键即时行为，本身不进入行文本——行文本由回车提交执行。
func (e *Editor) helpLine(prompt string) {
	if e.complete == nil {
		return
	}
	_, cs := e.complete(string(e.line))
	if len(cs) == 0 {
		fmt.Fprint(e.out, "\a")
		return
	}
	e.listCandidates(prompt, cs)
}

// listCandidates 另起一行列出候选，随后重绘提示符与已输入文本（§5.1「并回显已输入部分」）。
// 先换行：`?`/Tab 是按键即时触发，光标仍停在提示符行尾，不换行会把首条候选粘在该行上。
func (e *Editor) listCandidates(prompt string, cs []schema.Candidate) {
	fmt.Fprint(e.out, "\r\n")
	printCandidates(e.out, cs)
	e.redraw(prompt)
}

func (e *Editor) insertRune(b byte, prompt string) {
	r := rune(b)
	if b >= 0x80 { // UTF-8 多字节：逐字节追加（简化处理，ASCII 为主）
		r = rune(b)
	}
	if e.cursor == len(e.line) {
		e.line = append(e.line, r)
		e.cursor++
		fmt.Fprint(e.out, string(r))
	} else {
		e.line = append(e.line[:e.cursor], append([]rune{r}, e.line[e.cursor:]...)...)
		e.cursor++
		e.redraw(prompt)
	}
}

func (e *Editor) backspace(prompt string) {
	if e.cursor == 0 {
		return
	}
	e.line = append(e.line[:e.cursor-1], e.line[e.cursor:]...)
	e.cursor--
	e.redraw(prompt)
}

func (e *Editor) redraw(prompt string) {
	fmt.Fprint(e.out, "\r\x1b[K"+prompt+string(e.line))
	if back := len(e.line) - e.cursor; back > 0 {
		fmt.Fprintf(e.out, "\x1b[%dD", back)
	}
}

// handleEscape ↑/↓ 历史导航 + Windows 扫描码（0xE0 前缀）双分支。
func (e *Editor) handleEscape(prompt string) {
	var seq [3]byte
	if _, err := e.in.Read(seq[:1]); err != nil {
		return
	}
	switch seq[0] {
	case '[':
		if _, err := e.in.Read(seq[1:2]); err != nil {
			return
		}
		switch seq[1] {
		case 'A':
			e.historyNav(prompt, -1)
		case 'B':
			e.historyNav(prompt, 1)
		case 'C':
			if e.cursor < len(e.line) {
				e.cursor++
				fmt.Fprint(e.out, "\x1b[C")
			}
		case 'D':
			if e.cursor > 0 {
				e.cursor--
				fmt.Fprint(e.out, "\x1b[D")
			}
		case '3': // Delete
			if _, err := e.in.Read(seq[2:]); err == nil && seq[2] == '~' {
				if e.cursor < len(e.line) {
					e.line = append(e.line[:e.cursor], e.line[e.cursor+1:]...)
					e.redraw(prompt)
				}
			}
		}
	case 'O':
		if _, err := e.in.Read(seq[1:2]); err == nil {
			switch seq[1] {
			case 'H':
				e.cursor = 0
				e.redraw(prompt)
			case 'F':
				e.cursor = len(e.line)
				e.redraw(prompt)
			}
		}
	case 0xe0, 0x00: // Windows 控制台扫描码前缀
		if _, err := e.in.Read(seq[1:2]); err != nil {
			return
		}
		switch seq[1] {
		case 'H':
			e.historyNav(prompt, -1)
		case 'P':
			e.historyNav(prompt, 1)
		case 'K':
			if e.cursor > 0 {
				e.cursor--
				fmt.Fprint(e.out, "\x1b[D")
			}
		case 'M':
			if e.cursor < len(e.line) {
				e.cursor++
				fmt.Fprint(e.out, "\x1b[C")
			}
		}
	}
}

// historyNav ↑/↓：历史浏览（相邻去重由 History.Add 保证）。
func (e *Editor) historyNav(prompt string, dir int) {
	var shown string
	var changed bool
	if dir < 0 {
		shown, changed = e.history.Up(string(e.line))
	} else {
		shown, changed = e.history.Down()
	}
	if !changed {
		fmt.Fprint(e.out, "\a") // 响铃：到头
		return
	}
	e.line = []rune(shown)
	e.cursor = len(e.line)
	e.redraw(prompt)
}

// search Ctrl-R 反查：交互式输入检索词，Enter 接受、ESC 取消。
func (e *Editor) search(prompt string) {
	term := ""
	e.history.SearchStart(string(e.line))
	for {
		fmt.Fprintf(e.out, "\r\x1b[K%s(reverse-i-search `%s`): %s", prompt, term, e.history.SearchHit())
		buf := make([]byte, 1)
		if _, err := e.in.Read(buf); err != nil {
			return
		}
		switch b := buf[0]; b {
		case '\r', '\n':
			fmt.Fprint(e.out, "\r\n")
			e.line = []rune(e.history.SearchHit())
			e.cursor = len(e.line)
			e.history.SearchCancel()
			fmt.Fprint(e.out, prompt+string(e.line))
			return
		case 0x1b:
			e.history.SearchCancel()
			e.redraw(prompt)
			return
		case 0x7f, 0x08:
			if term != "" {
				term = term[:len(term)-1]
			}
		case 0x12: // 再次 Ctrl-R：下一条命中
			if hit, _ := e.history.SearchStep(term); hit != "" {
				fmt.Fprintf(e.out, "\r\x1b[K%s(reverse-i-search `%s`): %s", prompt, term, hit)
			}
			continue
		default:
			if b >= 0x20 && b < 0x7f {
				term += string(b)
			}
		}
		if hit, _ := e.history.SearchStep(term); hit != "" {
			fmt.Fprintf(e.out, "\r\x1b[K%s(reverse-i-search `%s`): %s", prompt, term, hit)
		}
	}
}
