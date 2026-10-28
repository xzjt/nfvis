package cli

// L4（round51 评审）：空闲守卫只在 ReadLine（raw 路径）里 Touch，于是
//   - 串口/容器终端（按键由 copyConsole 的读取协程消费）、monitor 刷新、向导问答（bufio.Scanner）
//     这些阶段一律不刷新：阶段一结束回到 ReadLine，poll 立刻判「会话空闲超时」——
//     一直在打字的操作者被登出、候选被丢弃；
//   - 非 TTY 的 readPlain 路径**一次都不刷新**：慢速喂入的管道/协程会话中途被判超时，
//     剩余命令一条也不执行，而 Run 仍返回 nil（退出码 0）。
//
// 修法：这些阶段各自刷新守卫（见 rawmode.go touchIdle、console.go、repl.go）。
// 本文件钉住「活跃不被登出」与「真空闲仍超时」两侧。

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// dribbleReader 按 interval 逐片吐数据（模拟「操作者在慢慢敲键」），片尽返回 EOF。
type dribbleReader struct {
	chunks   []string
	interval time.Duration
}

func (d *dribbleReader) Read(p []byte) (int, error) {
	if len(d.chunks) == 0 {
		return 0, io.EOF
	}
	time.Sleep(d.interval)
	s := d.chunks[0]
	d.chunks = d.chunks[1:]
	n := copy(p, s)
	return n, nil
}

// TestConsoleInputRefreshesIdleGuard ① 串口阶段持续收到按键 ⇒ 不因阶段超过超时被判空闲。
// 红-绿：去掉 copyConsole 里的 touchIdle，本用例在阶段结束时守卫已过期（红）。
func TestConsoleInputRefreshesIdleGuard(t *testing.T) {
	var out bytes.Buffer
	e := &Editor{out: io.Discard, history: NewHistory()}
	e.idle = NewIdleGuard(30*time.Millisecond, time.Now)
	rp := &REPL{out: &out, editor: e}

	// 4 片 × 15ms = 60ms > 30ms 的超时：没有逐片刷新，结束时必然已过期。
	local := &dribbleReader{chunks: []string{"a", "b", "c", "d"}, interval: 15 * time.Millisecond}
	done := make(chan struct{})
	if !copyConsoleWithin(t, rp, io.Discard, local, done, 2*time.Second) {
		t.Fatal("copyConsole 应在本地下发结束后返回")
	}
	if e.IdleExpired() {
		t.Fatal("打了一路字（阶段时长超过超时）之后不得判空闲超时——那会登出活跃会话")
	}
	// 自校准：守卫本身仍然有效（静置后照样过期），否则上面的断言可能只是恒绿摆设。
	time.Sleep(45 * time.Millisecond)
	if !e.IdleExpired() {
		t.Fatal("静置后守卫应判过期（判据没有区分力）")
	}
}

// TestNonTTYSlowInputRefreshesIdleGuard ② 非 TTY（管道/协程）慢速喂入：一行还没喂完，
// 超时已过——逐字节刷新后该行仍能执行完（旧实现：无刷新 ⇒ 中途登出，一行都不执行）。
func TestNonTTYSlowInputRefreshesIdleGuard(t *testing.T) {
	stub := &logoutStub{}
	s := New(stub, "ssh")
	e, w := newPipeEditor(t)
	e.idle = NewIdleGuard(50*time.Millisecond, time.Now)
	var out bytes.Buffer
	rp := &REPL{session: s, editor: e, history: e.history, out: &out, poll: 5 * time.Millisecond}

	go func() {
		// 逐字节（每 20ms）喂一条命令：单行用时 ~260ms >> 50ms 超时。
		for _, b := range []byte("show version\n") {
			_, _ = w.Write([]byte{b})
			time.Sleep(20 * time.Millisecond)
		}
		_ = w.Close()
	}()
	if err := rp.Run(); err != nil {
		t.Fatalf("Run 不应报错: %v", err)
	}
	if len(stub.lines) != 1 || stub.lines[0] != "show version" {
		t.Fatalf("慢速喂入的行仍应执行（旧实现会中途登出、一行不执行）：执行了 %v，输出 %q",
			stub.lines, out.String())
	}
	if strings.Contains(out.String(), "空闲超时") {
		t.Fatalf("持续收到输入不得判空闲超时：%q", out.String())
	}
}

// TestGenuinelyIdleSessionStillTimesOut ③ 负控：真的没人输入（管道不写、不关）照样超时登出。
func TestGenuinelyIdleSessionStillTimesOut(t *testing.T) {
	stub := &logoutStub{}
	s := New(stub, "ssh")
	e, w := newPipeEditor(t) // 本地输入：永不写入
	defer w.Close()
	e.idle = NewIdleGuard(40*time.Millisecond, time.Now)
	var out bytes.Buffer
	rp := &REPL{session: s, editor: e, history: e.history, out: &out, poll: 5 * time.Millisecond}

	if err := rp.Run(); err != nil {
		t.Fatalf("空闲超时应正常登出: %v", err)
	}
	if !stub.loggedOut {
		t.Fatal("超时后应吊销会话（Logout）")
	}
	if len(stub.lines) != 0 {
		t.Fatalf("无输入时不该执行任何命令: %v", stub.lines)
	}
	if !strings.Contains(out.String(), "空闲超时") {
		t.Fatalf("应提示会话失效: %q", out.String())
	}
}
