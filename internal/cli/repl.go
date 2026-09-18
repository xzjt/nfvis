package cli

// W2：交互 REPL 循环（FR-CLI-006）。
// raw 模式行编辑（历史/Ctrl-R/补全）由 Editor 提供；本文件负责命令分发与
// 空闲超时登出。闲时 ReadLine 阻塞在 stdin 上，故在独立 goroutine 中读取、
// 主循环轮询 IdleGuard：超时即吊销 token 并提示（会话失效）。
// 非 TTY（管道/脚本）自动退化行读取，行为与重构前一致。

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"
)

// ErrIdleTimeout 会话空闲超时（FR-CLI-006）。
var ErrIdleTimeout = errors.New("会话空闲超时")

// REPL 交互循环。
type REPL struct {
	session *Session
	editor  *Editor
	history *History
	out     io.Writer
	poll    time.Duration // 空闲检测轮询间隔（测试可缩短）
}

// NewREPL 构造交互循环（stdin 为 TTY 时启用 raw 模式）。
func NewREPL(session *Session, history *History, idle *IdleGuard) *REPL {
	e := NewEditor(history, idle)
	e.SetCompleter(session.Complete)
	// 输出经编辑器的 raw 感知流：raw 期间裸 \n 不回车，直写 stdout 会阶梯错位（决策 #81①）。
	return &REPL{session: session, editor: e, history: history, out: e.Out(), poll: time.Second}
}

// Run 进入交互循环，直至 EOF（Ctrl-D）或空闲超时。
// 空闲超时登出后可被再次调用（token 已失效，需重新登录）。
func (r *REPL) Run() error {
	defer r.editor.Close()
	prompt := r.session.Prompt()
	for {
		line, err := r.readLine(prompt)
		switch {
		case errors.Is(err, ErrIdleTimeout):
			fmt.Fprintf(r.out, "%% 会话空闲超时（%s），已自动登出。\n", r.editor.idle.Timeout)
			r.teardown() // token 尚未吊销，此时仍可释放 candidate 与配置模式
			return nil
		case errors.Is(err, io.EOF):
			r.teardown()
			return nil
		case err != nil:
			return err
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasSuffix(trimmed, "?") { // 非 raw（管道/脚本）的 ? 列候选；raw 下由 Editor 按键即时处理
			printCandidates(r.out, r.session.Candidates(trimmed))
			continue
		}
		r.history.Add(trimmed)
		if cmd, interval, ok := monitorSpec(trimmed); ok { // monitor interfaces：本地轮询刷新（M3-9）
			r.runMonitor(cmd, interval)
			prompt = r.session.Prompt()
			continue
		}
		if strings.HasSuffix(line, "\t") { // 非 raw 退化的 Tab 补全
			line = r.session.CompleteLine(line)
		}
		cmd := strings.TrimSpace(line)
		out, next, console := r.session.ExecuteFull(cmd)
		prompt = next
		// 破坏性动作的交互确认（FR-CMP-013）：服务端返回问询文本（以 "[yes,no]" 结尾），
		// 本地读取答复；yes 则以 --yes 重发同一命令（执行期确认标记），否则中止。
		if strings.HasSuffix(strings.TrimSpace(out), "[yes,no]") {
			fmt.Fprint(r.out, out)
			if r.readConfirm("") {
				out, next, console = r.session.ExecuteFull(cmd + " " + confirmFlag)
				prompt = next
			} else {
				out = "已取消\n"
			}
		}
		if out != "" {
			fmt.Fprint(r.out, out)
			if !strings.HasSuffix(out, "\n") {
				fmt.Fprintln(r.out)
			}
		}
		if console != nil { // 串口接管（FR-CMP-014）：进入 console，Ctrl-] 退出后续打提示符
			r.runConsole(console)
			prompt = r.session.Prompt()
		}
	}
}

// teardown 会话收尾并回显各步输出（丢弃 candidate 时会提示，避免「未提交变更被静默丢弃」）。
func (r *REPL) teardown() {
	for _, out := range r.session.Teardown() {
		fmt.Fprint(r.out, out)
	}
}

// monitorSpec 识别 `monitor interfaces <ifname> [interval <sec>]` 与 `monitor vnf <name>`
// （都支持无歧义前缀缩写）；返回服务端快照命令行与刷新间隔。
//
// 两条都是**本地轮询**：服务端只给单次快照，刷新由 REPL 负责（附录 A #36）。
// `monitor vnf` 此前**只执行一次**——而契约 §1.3 与命令树都写的是「跟踪 VNF 状态/事件」，
// 服务端 monitorVNF 的注释也早已写着「实时刷新由 nfvis-cli REPL 轮询（与 monitor interfaces
// 同法）」——是 CLI 侧漏了（附录 A #92）。vnf 的树里没有 `interval` 子节点，故固定 1s。
func monitorSpec(line string) (cmd string, interval time.Duration, ok bool) {
	toks := strings.Fields(line)
	if len(toks) < 3 || !prefixOf(toks[0], "monitor") {
		return "", 0, false
	}
	switch {
	case prefixOf(toks[1], "interfaces"):
		interval = time.Second
		if len(toks) >= 5 && prefixOf(toks[3], "interval") {
			n, err := strconv.Atoi(toks[4])
			if err != nil || n <= 0 {
				return "", 0, false
			}
			interval = time.Duration(n) * time.Second
		} else if len(toks) != 3 {
			return "", 0, false
		}
		return strings.Join(toks[:3], " "), interval, true
	case prefixOf(toks[1], "vnf"):
		if len(toks) != 3 { // 语法就是 `monitor vnf <name>`；多给的 token 不认
			return "", 0, false
		}
		return strings.Join(toks[:3], " "), time.Second, true
	}
	return "", 0, false
}

// prefixOf s 是 full 的非空无歧义前缀（≥3 字符）。
func prefixOf(s, full string) bool {
	return len(s) >= 3 && len(s) <= len(full) && strings.HasPrefix(full, s)
}

// runMonitor 按 interval 轮询服务端单次快照并整屏刷新，Ctrl-C 退出（附录 A #36）。
// 非 TTY（管道/脚本）退化为单次快照，避免脚本挂死。
func (r *REPL) runMonitor(cmd string, interval time.Duration) {
	render := func() {
		out, _ := r.session.ExecuteLine(cmd)
		fmt.Fprint(r.out, "\x1b[2J\x1b[H") // 清屏并回原点
		fmt.Fprint(r.out, out)
		if !strings.HasSuffix(out, "\n") {
			fmt.Fprintln(r.out)
		}
	}
	if !r.editor.IsRaw() {
		render()
		return
	}
	wasRaw := r.editor.Suspend() // 恢复终端信号处理，使 Ctrl-C 产生 SIGINT
	defer r.editor.Resume(wasRaw)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	defer signal.Stop(stop)

	render()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			fmt.Fprintln(r.out, "^C")
			return
		case <-ticker.C:
			render()
		}
	}
}

// readLine 读取一行；闲时轮询空闲守卫，超时中断等待（读取 goroutine 随进程退出）。
func (r *REPL) readLine(prompt string) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := r.editor.ReadLine(prompt)
		ch <- result{line, err}
	}()
	tick := time.NewTicker(r.poll)
	defer tick.Stop()
	for {
		select {
		case res := <-ch:
			return res.line, res.err
		case <-tick.C:
			if r.editor.IdleExpired() {
				return "", ErrIdleTimeout
			}
		}
	}
}
