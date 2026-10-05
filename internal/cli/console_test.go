package cli

// M4-12：console 终端接管与破坏性动作交互确认（FR-CMP-013/014）。
//
// 两个行为都在 REPL 层完成：服务端返回问询/接管请求，REPL 读本地输入决定
// 是否以 --yes 重发、或进入串口透传（Ctrl-] 退出）。此处用管道输入 + 非 raw
// 编辑器覆盖非 TTY 路径与外发命令行，避免真实终端依赖。

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/pkg/cliclient"
)

// consoleStub 记录执行过的命令行，并可按行返回 console 接管请求 / 确认问询。
type consoleStub struct {
	lines    []string
	replies  map[string]cliclient.Result // 按命令行返回定制结果（可选）
	console  *cliclient.ConsoleRequest   // 首次匹配即回传
	dialed   []string
	dialErr  error
	dialConn io.ReadWriteCloser
}

func (s *consoleStub) Execute(line, source string) (cliclient.Result, error) {
	s.lines = append(s.lines, line)
	for k, r := range s.replies {
		if line == k {
			return r, nil
		}
	}
	return cliclient.Result{Output: "ok\n", Mode: "oper", Prompt: "nfvis> "}, nil
}
func (s *consoleStub) DynamicCandidates(string) ([]string, error) { return nil, nil }
func (s *consoleStub) Logout() error                              { return nil }
func (s *consoleStub) MetricsText() (string, error)               { return "", nil }

func (s *consoleStub) DialConsole(wsPath, what string) (io.ReadWriteCloser, error) {
	s.dialed = append(s.dialed, wsPath)
	if s.dialErr != nil {
		return nil, s.dialErr
	}
	return s.dialConn, nil
}

// nopCloser 可读写的哑流（Read 立即 EOF，Write 丢弃）。
type nopCloser struct{ io.Reader }

func (nopCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopCloser) Close() error                { return nil }

// TestREPLConfirmResendsWithFlag 问询后应答 yes → 以 --yes 重发；否则不重发。
func TestREPLConfirmResendsWithFlag(t *testing.T) {
	for _, tc := range []struct {
		answer   string
		wantFlag bool
	}{
		{"yes", true},
		{"no", false},
		{"", false},
	} {
		t.Run("answer="+tc.answer, func(t *testing.T) {
			stub := &consoleStub{replies: map[string]cliclient.Result{
				"request virtual-machine-functions fw-vm delete": {
					Output: "Delete VNF 'fw-vm'? [yes,no] ", Mode: "oper", Prompt: "nfvis> ",
				},
				"request virtual-machine-functions fw-vm delete --yes": {
					Output: "VNF fw-vm 已删除\n", Mode: "oper", Prompt: "nfvis> ",
				},
			}}
			s := New(stub, "ssh")
			e, w := newPipeEditor(t)
			e.idle = &IdleGuard{Timeout: time.Hour, now: time.Now}
			var out bytes.Buffer
			rp := &REPL{session: s, editor: e, history: e.history, out: &out, poll: time.Hour}

			go func() {
				_, _ = w.WriteString("request virtual-machine-functions fw-vm delete\n" + tc.answer + "\n")
				_ = w.Close()
			}()
			if err := rp.Run(); err != nil {
				t.Fatalf("Run: %v", err)
			}
			sent := strings.Join(stub.lines, "|")
			if tc.wantFlag && !strings.Contains(sent, "delete --yes") {
				t.Fatalf("应答 yes 应以 --yes 重发，实际发送: %s", sent)
			}
			if !tc.wantFlag && strings.Contains(sent, "--yes") {
				t.Fatalf("应答 %q 不应重发，实际发送: %s", tc.answer, sent)
			}
			if !tc.wantFlag && !strings.Contains(out.String(), "已取消") {
				t.Fatalf("未确认应提示已取消: %q", out.String())
			}
		})
	}
}

// TestREPLConsoleNonTTYNotice 非 TTY 下 console 命令不做终端接管并明确提示。
func TestREPLConsoleNonTTYNotice(t *testing.T) {
	stub := &consoleStub{replies: map[string]cliclient.Result{
		"request virtual-machine-functions fw-vm console": {
			Output:  "正在打开 fw-vm 的串口…\n",
			Mode:    "oper",
			Prompt:  "nfvis> ",
			Console: &cliclient.ConsoleRequest{VM: "fw-vm", WSURL: "/api/v1/virtual-machine-functions/fw-vm/console/ws?ticket=x"},
		},
	}}
	s := New(stub, "ssh")
	e, w := newPipeEditor(t)
	e.idle = &IdleGuard{Timeout: time.Hour, now: time.Now}
	var out bytes.Buffer
	rp := &REPL{session: s, editor: e, history: e.history, out: &out, poll: time.Hour}

	go func() {
		_, _ = w.WriteString("request virtual-machine-functions fw-vm console\n")
		_ = w.Close()
	}()
	if err := rp.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(stub.dialed) != 0 {
		t.Fatalf("非 TTY 不应发起 WS 连接: %v", stub.dialed)
	}
	if !strings.Contains(out.String(), "不支持交互式串口接管") {
		t.Fatalf("应明确提示不支持接管: %q", out.String())
	}
}

// TestSessionExecuteFullSurfacesConsole ExecuteFull 应回传 console 接管请求。
func TestSessionExecuteFullSurfacesConsole(t *testing.T) {
	stub := &consoleStub{replies: map[string]cliclient.Result{
		"request virtual-machine-functions fw-vm console": {
			Output:  "opening\n",
			Mode:    "oper",
			Prompt:  "nfvis> ",
			Console: &cliclient.ConsoleRequest{VM: "fw-vm", WSURL: "/ws?ticket=abc"},
		},
	}}
	s := New(stub, "ssh")
	out, prompt, console := s.ExecuteFull("request virtual-machine-functions fw-vm console")
	if out != "opening\n" || prompt != "nfvis> " {
		t.Fatalf("输出/提示符异常: %q %q", out, prompt)
	}
	if console == nil || console.VM != "fw-vm" || console.WSURL != "/ws?ticket=abc" {
		t.Fatalf("应回传 console 接管请求: %+v", console)
	}
	// 普通命令不产生接管请求
	_, _, none := s.ExecuteFull("show version")
	if none != nil {
		t.Fatalf("普通命令不应产生接管请求: %+v", none)
	}
}

// TestREPLConsoleNonTTYDialErrorSkipped 非 TTY 下连 WS 失败也不应挂死（此处不拨号）。
func TestREPLConsoleNonTTYDialErrorSkipped(t *testing.T) {
	stub := &consoleStub{
		dialErr: errors.New("should not dial"),
		replies: map[string]cliclient.Result{
			"request virtual-machine-functions fw-vm console": {
				Output:  "opening\n",
				Mode:    "oper",
				Prompt:  "nfvis> ",
				Console: &cliclient.ConsoleRequest{VM: "fw-vm", WSURL: "/ws?ticket=x"},
			},
		},
	}
	s := New(stub, "ssh")
	e, w := newPipeEditor(t)
	e.idle = &IdleGuard{Timeout: time.Hour, now: time.Now}
	var out bytes.Buffer
	rp := &REPL{session: s, editor: e, history: e.history, out: &out, poll: time.Hour}
	go func() {
		_, _ = w.WriteString("request virtual-machine-functions fw-vm console\n")
		_ = w.Close()
	}()
	if err := rp.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "不支持交互式串口接管") {
		t.Fatalf("非 TTY 应提示而非拨号: %q", out.String())
	}
	_ = os.Stdout
}

// ---------- 决策 #310：console 断开后主循环不得阻塞在 Read ----------

// copyConsoleWithin 在限时内运行 copyConsole，返回是否在限时内返回（false = 仍阻塞）。
func copyConsoleWithin(t *testing.T, rp *REPL, stream io.Writer, localIn io.Reader, done <-chan struct{}, limit time.Duration) bool {
	t.Helper()
	fin := make(chan struct{})
	go func() {
		rp.copyConsole(stream, localIn, done, "串口")
		close(fin)
	}()
	select {
	case <-fin:
		return true
	case <-time.After(limit):
		return false
	}
}

// TestCopyConsoleExitsOnServerClose 回归（决策 #310）：服务端断开（done 关闭）时，
// 即便本地输入一直阻塞（用户没按键），copyConsole 也必须立即返回——旧实现直接阻塞在
// stdin 的 Read 上，done 收到信号也看不到，必须再按一次键才能退出。
func TestCopyConsoleExitsOnServerClose(t *testing.T) {
	pr, pw := io.Pipe() // 本地输入：永不写入，模拟「没人按键」
	defer pw.Close()
	var out bytes.Buffer
	rp := &REPL{out: &out}
	done := make(chan struct{})
	go func() {
		time.Sleep(30 * time.Millisecond)
		close(done)
	}()
	if !copyConsoleWithin(t, rp, io.Discard, pr, done, 2*time.Second) {
		t.Fatal("服务端已断开但仍阻塞（需按键才退出），决策 #310 回归")
	}
	if !strings.Contains(out.String(), "串口已断开") {
		t.Fatalf("应提示串口已断开: %q", out.String())
	}
}

// TestCopyConsoleExitsOnLocalEOF 本地输入 EOF（管道关闭）即返回并触发退出路径。
func TestCopyConsoleExitsOnLocalEOF(t *testing.T) {
	var out bytes.Buffer
	rp := &REPL{out: &out}
	done := make(chan struct{}) // 永不关闭：退出只能来自本地 EOF
	if !copyConsoleWithin(t, rp, io.Discard, strings.NewReader(""), done, 2*time.Second) {
		t.Fatal("本地 EOF 后仍阻塞")
	}
	if strings.Contains(out.String(), "串口已断开") {
		t.Fatalf("本地 EOF 不应报服务端断开: %q", out.String())
	}
}

// TestCopyConsoleCtrlBracketExits Ctrl-] 照旧退出；退出键自身不转发给服务端。
func TestCopyConsoleCtrlBracketExits(t *testing.T) {
	var out, sent bytes.Buffer
	rp := &REPL{out: &out}
	done := make(chan struct{})
	if !copyConsoleWithin(t, rp, &sent, strings.NewReader("hi\x1d"), done, 2*time.Second) {
		t.Fatal("Ctrl-] 应退出")
	}
	if sent.String() != "hi" {
		t.Fatalf("Ctrl-] 之前的字节应转发、退出键不转发：%q", sent.String())
	}
	if !strings.Contains(out.String(), "已退出串口") {
		t.Fatalf("应提示已退出串口: %q", out.String())
	}
}

// TestCopyConsoleForwardsInput 本地按键原样转发到服务端，本地 EOF 后返回。
func TestCopyConsoleForwardsInput(t *testing.T) {
	var out, sent bytes.Buffer
	rp := &REPL{out: &out}
	done := make(chan struct{})
	if !copyConsoleWithin(t, rp, &sent, strings.NewReader("hello"), done, 2*time.Second) {
		t.Fatal("本地 EOF 后应返回")
	}
	if sent.String() != "hello" {
		t.Fatalf("本地输入应原样转发：%q", sent.String())
	}
}
