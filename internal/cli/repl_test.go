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

// ---------- round86 R86-8：「值未变化」是提示而非失败 ----------

// warningStub 首行回「语句未产生配置变更」的提示（Warning=true），其余照常。
type warningStub struct {
	stubClient
	lines []string
}

func (s *warningStub) Execute(line, source string) (cliclient.Result, error) {
	s.lines = append(s.lines, line)
	if line == "set system hostname nc-node" {
		return cliclient.Result{
			Output:  "警告: 语句未产生配置变更（值未变化或尚未映射到模型），已继续：" + line + "\n",
			Mode:    "config",
			Prompt:  "nfvis# ",
			Warning: true,
		}, nil
	}
	return cliclient.Result{Output: "ok\n", Mode: "config", Prompt: "nfvis# "}, nil
}

// TestSessionExecuteSurfacesWarning Execute 必须把服务端的结构化提示标记透传给
// 脚本模式（脚本据它继续；吞掉标记就等于退回「猜文本」）。
func TestSessionExecuteSurfacesWarning(t *testing.T) {
	s := New(&warningStub{}, "ssh")
	res := s.Execute("set system hostname nc-node")
	if !res.Warning {
		t.Fatalf("提示应带上结构化标记：%+v", res)
	}
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("提示行不得带错误前缀：%q", res.Output)
	}
	if res.Mode != "config" || res.Prompt != "nfvis# " {
		t.Fatalf("提示之后仍应更新模式/提示符：%+v", res)
	}
}

// TestREPLShowsNoChangeWarningAndContinues ③ 交互模式下该提示照常可读、**不中止**会话
// （交互本来就不中止，本轮只是把 %% 错误样式改成提示，行为不变）。
func TestREPLShowsNoChangeWarningAndContinues(t *testing.T) {
	stub := &warningStub{}
	s := New(stub, "ssh")
	e, w := newPipeEditor(t)
	e.idle = &IdleGuard{Timeout: time.Hour, now: time.Now}
	var out bytes.Buffer
	rp := &REPL{session: s, editor: e, history: e.history, out: &out, poll: time.Hour}

	go func() {
		_, _ = w.WriteString("set system hostname nc-node\nshow version\n")
		w.Close()
	}()
	if err := rp.Run(); err != nil {
		t.Fatalf("Run 不应报错: %v", err)
	}
	if len(stub.lines) < 2 || stub.lines[0] != "set system hostname nc-node" || stub.lines[1] != "show version" {
		t.Fatalf("提示不该中止会话（应继续执行下一行）：%v", stub.lines)
	}
	if !strings.Contains(out.String(), "警告: 语句未产生配置变更") {
		t.Fatalf("交互模式应显示提示：%q", out.String())
	}
	if strings.Contains(out.String(), "%%") {
		t.Fatalf("提示不得带错误前缀（%% 是错误样式）：%q", out.String())
	}
}
