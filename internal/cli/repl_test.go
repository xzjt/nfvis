package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/pkg/cliclient"
)

// ---------- W2：REPL 分发 / 空闲超时登出（FR-CLI-006） ----------

type logoutStub struct {
	stubClient
	loggedOut bool
	lines     []string
}

func (s *logoutStub) Execute(line, source string) (cliclient.Result, error) {
	s.lines = append(s.lines, line)
	return cliclient.Result{Output: "ok\n", Mode: "oper", Prompt: "nfvis> "}, nil
}

func (s *logoutStub) Logout() error { s.loggedOut = true; return nil }

// newPipeEditor 构造非 raw 编辑器（stdin 用管道，绕过 TTY）。
func newPipeEditor(t *testing.T) (*Editor, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close(); r.Close() })
	return &Editor{in: r, out: io.Discard, history: NewHistory()}, w
}

func TestREPLExecutesLinesAndRecordsHistory(t *testing.T) {
	stub := &logoutStub{}
	s := New(stub, "ssh")
	e, w := newPipeEditor(t)
	e.idle = &IdleGuard{Timeout: time.Hour, now: time.Now}
	var out bytes.Buffer
	rp := &REPL{session: s, editor: e, history: e.history, out: &out, poll: time.Hour}

	go func() {
		_, _ = w.WriteString("show version\nshow version\n")
		w.Close()
	}()
	if err := rp.Run(); err != nil {
		t.Fatalf("Run 不应报错: %v", err)
	}
	if len(stub.lines) != 2 {
		t.Fatalf("应执行 2 行: %v", stub.lines)
	}
	if !strings.Contains(out.String(), "ok") {
		t.Fatalf("应输出命令结果: %q", out.String())
	}
	// 相邻重复命令去重（FR-CLI-006）
	if e.history.Len() != 1 {
		t.Fatalf("相邻重复应去重，历史 %d 条: %v", e.history.Len(), e.history.entries)
	}
}

func TestREPLIdleTimeoutLogsOut(t *testing.T) {
	stub := &logoutStub{}
	s := New(stub, "ssh")
	e, _ := newPipeEditor(t)
	e.idle = &IdleGuard{Timeout: time.Nanosecond, now: time.Now} // 立即过期
	var out bytes.Buffer
	rp := &REPL{session: s, editor: e, history: e.history, out: &out, poll: 2 * time.Millisecond}

	if err := rp.Run(); err != nil {
		t.Fatalf("空闲超时应正常登出: %v", err)
	}
	if !stub.loggedOut {
		t.Fatalf("超时后应吊销会话（Logout）")
	}
	if !strings.Contains(out.String(), "空闲超时") {
		t.Fatalf("应提示会话失效: %q", out.String())
	}
}
