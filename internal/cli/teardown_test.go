package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/pkg/cliclient"
)

// 决策 #82④：交互 REPL 退出必须做会话收尾。
//
// 服务端会话按 user@source 保留（与 token 生命周期无关），此前 EOF 直接返回、
// 空闲超时只吊销 token，都不释放配置模式与 candidate 锁——于是「管道演示过的会话」
// 会把状态留给下一次登录，下一次 `configure` 报 `%% 无效命令: configure`。

// modeStub 记录执行过的行，并按行返回模式（configure → config，其余 → oper）。
type modeStub struct {
	stubClient
	lines     []string
	loggedOut bool
}

func (s *modeStub) Execute(line, source string) (cliclient.Result, error) {
	s.lines = append(s.lines, line)
	mode := "oper"
	if line == "configure" {
		mode = "config"
	}
	return cliclient.Result{Output: "[ok] " + line + "\n", Mode: mode, Prompt: "nfvis# "}, nil
}

func (s *modeStub) Logout() error { s.loggedOut = true; return nil }

// assertTeardown 断言收到 discard → exit → Logout 的收尾序列。
func assertTeardown(t *testing.T, stub *modeStub) {
	t.Helper()
	joined := strings.Join(stub.lines, ",")
	if !strings.Contains(joined, "discard,exit") {
		t.Fatalf("退出应依次 discard 与 exit（实际执行: %v）", stub.lines)
	}
	if !stub.loggedOut {
		t.Fatalf("退出应吊销 token（实际执行: %v）", stub.lines)
	}
}

func TestREPLTeardownOnEOF(t *testing.T) {
	stub := &modeStub{}
	s := New(stub, "ssh")
	e, w := newPipeEditor(t)
	e.idle = &IdleGuard{Timeout: time.Hour, now: time.Now}
	var out bytes.Buffer
	rp := &REPL{session: s, editor: e, history: e.history, out: &out, poll: time.Hour}

	go func() {
		_, _ = w.WriteString("configure\n") // 进入配置模式后管道结束 → EOF
		w.Close()
	}()
	if err := rp.Run(); err != nil {
		t.Fatalf("Run 不应报错: %v", err)
	}
	assertTeardown(t, stub)
	if s.Mode != "oper" {
		t.Fatalf("退出后本地模式应回到 oper: %q", s.Mode)
	}
	if !strings.Contains(out.String(), "candidate") && !strings.Contains(out.String(), "[ok] discard") {
		t.Fatalf("收尾输出应回显（避免变更被静默丢弃）: %q", out.String())
	}
}

func TestREPLTeardownOnIdleTimeout(t *testing.T) {
	stub := &modeStub{}
	s := New(stub, "ssh")
	s.Mode = "config" // 退出时仍处于配置模式（超时前无输入，模式取会话初值）
	e, _ := newPipeEditor(t)
	e.idle = &IdleGuard{Timeout: time.Nanosecond, now: time.Now}
	var out bytes.Buffer
	rp := &REPL{session: s, editor: e, history: e.history, out: &out, poll: 2 * time.Millisecond}

	if err := rp.Run(); err != nil {
		t.Fatalf("空闲超时应正常收尾: %v", err)
	}
	if !strings.Contains(out.String(), "空闲超时") {
		t.Fatalf("应提示会话失效: %q", out.String())
	}
	assertTeardown(t, stub)
}

// 操作模式下退出只需吊销 token：不应多发 discard/exit（无 candidate 可言）。
func TestREPLTeardownInOperModeOnlyLogsOut(t *testing.T) {
	stub := &modeStub{}
	s := New(stub, "ssh")
	e, w := newPipeEditor(t)
	e.idle = &IdleGuard{Timeout: time.Hour, now: time.Now}
	rp := &REPL{session: s, editor: e, history: e.history, out: &bytes.Buffer{}, poll: time.Hour}

	go func() { w.Close() }() // 立即 EOF
	if err := rp.Run(); err != nil {
		t.Fatalf("Run 不应报错: %v", err)
	}
	if len(stub.lines) != 0 {
		t.Fatalf("oper 模式退出不应额外执行命令: %v", stub.lines)
	}
	if !stub.loggedOut {
		t.Fatalf("退出应吊销 token")
	}
}

// Teardown 的步骤顺序不可颠倒：exit 在存在未提交变更时会被服务端拒绝，须先 discard。
func TestSessionTeardownOrder(t *testing.T) {
	stub := &modeStub{}
	s := New(stub, "ssh")
	s.Mode = "config"
	outs := s.Teardown()
	if got := strings.Join(stub.lines, ","); got != "discard,exit" {
		t.Fatalf("收尾顺序应为 discard,exit: %q", got)
	}
	if !stub.loggedOut {
		t.Fatalf("收尾应吊销 token")
	}
	if len(outs) != 2 {
		t.Fatalf("应返回两步输出供调用方展示: %v", outs)
	}
}
