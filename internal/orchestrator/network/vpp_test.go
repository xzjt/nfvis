package network

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.fd.io/govpp/core"
)

// ---------- M3-1：连接管理与版本锁定（FR-SYS-007）单测（假 Dialer/Session） ----------

type fakeSession struct {
	version      string
	verErr       error
	disconnected bool
}

func (f *fakeSession) Version() (string, error) { return f.version, f.verErr }
func (f *fakeSession) Disconnect()              { f.disconnected = true }

// fakeDialer 按调用次序返回预先编排的会话与事件通道。
type fakeDialer struct {
	mu       sync.Mutex
	calls    int
	sessions []*fakeSession
	events   []chan Event
	dialErr  error
}

func (d *fakeDialer) Dial(string, int, time.Duration) (Session, <-chan Event, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	i := d.calls
	d.calls++
	if d.dialErr != nil {
		return nil, nil, d.dialErr
	}
	if i >= len(d.sessions) {
		i = len(d.sessions) - 1
	}
	return d.sessions[i], d.events[i], nil
}

func (d *fakeDialer) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func connectedEvent() Event { return Event{State: StateConnected} }

func testConfig() Config {
	return Config{
		Socket:            "/tmp/fake.sock",
		RequiredVersion:   "26.06",
		ReconnectAttempts: 1,
		ReconnectInterval: time.Millisecond,
		RetryDelay:        5 * time.Millisecond,
		Log:               slog.New(slog.DiscardHandler),
	}
}

func TestConnectOnceVersionMismatch(t *testing.T) {
	ch := make(chan Event, 2)
	ch <- connectedEvent()
	sess := &fakeSession{version: "25.10-release"}
	d := &fakeDialer{sessions: []*fakeSession{sess}, events: []chan Event{ch}}
	m := NewManager(testConfig(), d)

	_, err := m.ConnectOnce(context.Background())
	var ve *VersionError
	if !errors.As(err, &ve) {
		t.Fatalf("非 26.06 应返回 VersionError: %v", err)
	}
	if ve.Got != "25.10-release" || ve.Required != "26.06" {
		t.Fatalf("VersionError 内容: %+v", ve)
	}
	if !sess.disconnected {
		t.Fatalf("版本不匹配应断开连接")
	}
	if m.State() != StateFailed {
		t.Fatalf("状态应为 failed: %v", m.State())
	}
}

func TestConnectOnceUnavailable(t *testing.T) {
	d := &fakeDialer{dialErr: errors.New("no such socket")}
	m := NewManager(testConfig(), d)

	_, err := m.ConnectOnce(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("连接失败应返回 ErrUnavailable: %v", err)
	}
	if m.State() != StateFailed {
		t.Fatalf("状态应为 failed: %v", m.State())
	}
}

func TestConnectOnceFailedEvent(t *testing.T) {
	ch := make(chan Event, 1)
	ch <- Event{State: StateFailed, Err: errors.New("timeout")}
	sess := &fakeSession{version: "26.06-release"}
	d := &fakeDialer{sessions: []*fakeSession{sess}, events: []chan Event{ch}}
	m := NewManager(testConfig(), d)

	_, err := m.ConnectOnce(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Failed 事件应视为不可用: %v", err)
	}
	if !sess.disconnected {
		t.Fatalf("连接失败应释放会话")
	}
}

func TestConnectOnceSuccess(t *testing.T) {
	ch := make(chan Event, 2)
	ch <- connectedEvent()
	sess := &fakeSession{version: "26.06-release"}
	d := &fakeDialer{sessions: []*fakeSession{sess}, events: []chan Event{ch}}
	m := NewManager(testConfig(), d)

	ver, err := m.ConnectOnce(context.Background())
	if err != nil {
		t.Fatalf("26.06 应连接成功: %v", err)
	}
	if ver != "26.06-release" || m.State() != StateConnected || m.Version() != "26.06-release" {
		t.Fatalf("版本/状态不符: ver=%q state=%v", ver, m.State())
	}
	m.Close()
	if !sess.disconnected {
		t.Fatalf("Close 应断开会话")
	}
}

// Run 在首次连接与断线重连时都回调 OnConnect（M3-8 恢复收敛接入点）。
func TestRunReconnects(t *testing.T) {
	ch1 := make(chan Event, 2)
	ch1 <- connectedEvent()
	ch1 <- Event{State: StateDisconnected}
	ch2 := make(chan Event, 1)
	ch2 <- connectedEvent()
	s1 := &fakeSession{version: "26.06-release"}
	s2 := &fakeSession{version: "26.06-release"}
	d := &fakeDialer{sessions: []*fakeSession{s1, s2}, events: []chan Event{ch1, ch2}}

	m := NewManager(testConfig(), d)
	connected := make(chan string, 2)
	m.OnConnect(func(v string) { connected <- v })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	for i := 0; i < 2; i++ { // 第一次为首连（启动收敛），第二次为重连（重放）
		select {
		case v := <-connected:
			if v != "26.06-release" {
				t.Fatalf("连接回调版本: %q", v)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("超时未触发第 %d 次连接回调", i+1)
		}
	}
	if d.callCount() < 2 {
		t.Fatalf("应至少拨号两次（断线重连），实际 %d", d.callCount())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run 正常退出应返回 nil: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run 未随 ctx 取消退出")
	}
}

// VPP 未运行时 Run 降级重试（不崩溃、不返回），直到 ctx 取消。
func TestRunRetriesWhenUnavailable(t *testing.T) {
	d := &fakeDialer{dialErr: errors.New("connection refused")}
	m := NewManager(testConfig(), d)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	time.Sleep(40 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("降级重试后取消应返回 nil: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run 未退出")
	}
	if d.callCount() < 2 {
		t.Fatalf("应重试拨号，实际 %d 次", d.callCount())
	}
}

// 版本不匹配为致命错误：Run 直接返回而不反复重试。
func TestRunVersionMismatchFatal(t *testing.T) {
	ch := make(chan Event, 1)
	ch <- connectedEvent()
	d := &fakeDialer{sessions: []*fakeSession{{version: "24.02"}}, events: []chan Event{ch}}
	m := NewManager(testConfig(), d)

	err := m.Run(context.Background())
	var ve *VersionError
	if !errors.As(err, &ve) {
		t.Fatalf("版本不匹配应致命返回: %v", err)
	}
	if d.callCount() != 1 {
		t.Fatalf("版本不匹配不应重试，实际拨号 %d 次", d.callCount())
	}
}

func TestVersionMatches(t *testing.T) {
	cases := []struct {
		got, req string
		want     bool
	}{
		{"26.06-release", "26.06", true},
		{"26.06", "26.06", true},
		{"25.10-release", "26.06", false},
		{"anything", "", true},
	}
	for _, c := range cases {
		if got := versionMatches(c.got, c.req); got != c.want {
			t.Fatalf("versionMatches(%q,%q)=%v want %v", c.got, c.req, got, c.want)
		}
	}
}

func TestStateAndVersionErrorStrings(t *testing.T) {
	for st, want := range map[State]string{
		StateConnected: "connected", StateNotResponding: "not-responding",
		StateFailed: "failed", StateDisconnected: "disconnected", State(99): "disconnected",
	} {
		if got := st.String(); got != want {
			t.Fatalf("State(%d).String()=%q want %q", st, got, want)
		}
	}
	ve := &VersionError{Got: "25.10", Required: "26.06"}
	if !strings.Contains(ve.Error(), "25.10") || !strings.Contains(ve.Error(), "版本不匹配") {
		t.Fatalf("VersionError.Error(): %q", ve.Error())
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}.withDefaults()
	if cfg.Socket != DefaultSocket || cfg.RequiredVersion != RequiredVPPVersion {
		t.Fatalf("缺省 socket/version: %+v", cfg)
	}
	if cfg.ReconnectAttempts <= 0 || cfg.ReconnectInterval <= 0 || cfg.RetryDelay <= 0 {
		t.Fatalf("缺省重连参数: %+v", cfg)
	}
	if cfg.Log == nil {
		t.Fatalf("缺省 Log 不应为 nil")
	}
}

// 空事件通道 + 已取消 ctx：ConnectOnce 走 ctx.Done 分支并释放会话。
func TestConnectOnceCtxCanceled(t *testing.T) {
	sess := &fakeSession{version: "26.06-release"}
	d := &fakeDialer{sessions: []*fakeSession{sess}, events: []chan Event{make(chan Event)}}
	m := NewManager(testConfig(), d)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.ConnectOnce(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ctx 取消应返回 ErrUnavailable: %v", err)
	}
	if !sess.disconnected {
		t.Fatalf("ctx 取消应释放会话")
	}
	if m.LastError() == nil {
		t.Fatalf("应记录 LastError")
	}
}

// 事件通道被关闭：watch 判为断开并触发重连（Run 循环），ctx 取消后退出。
func TestRunChannelClosedReconnects(t *testing.T) {
	ch := make(chan Event, 1)
	ch <- connectedEvent()
	close(ch)
	d := &fakeDialer{sessions: []*fakeSession{{version: "26.06-release"}}, events: []chan Event{ch}}
	// 后续拨号复用同一已关闭通道：ConnectOnce 立即判不可用并重试
	m := NewManager(testConfig(), d)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	time.Sleep(40 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run 取消应返回 nil: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run 未退出")
	}
	if d.callCount() < 2 {
		t.Fatalf("通道关闭后应重连，实际拨号 %d 次", d.callCount())
	}
}

func TestNotRespondingEventUnavailable(t *testing.T) {
	ch := make(chan Event, 1)
	ch <- Event{State: StateNotResponding}
	sess := &fakeSession{version: "26.06-release"}
	d := &fakeDialer{sessions: []*fakeSession{sess}, events: []chan Event{ch}}
	m := NewManager(testConfig(), d)
	if _, err := m.ConnectOnce(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("NotResponding 应视为不可用: %v", err)
	}
}

// mapConnState 把 govpp 状态映射为本地视图（不依赖真实连接）。
func TestMapConnState(t *testing.T) {
	cases := map[core.ConnectionState]State{
		core.Connected:     StateConnected,
		core.NotResponding: StateNotResponding,
		core.Disconnected:  StateDisconnected,
		core.Failed:        StateFailed,
	}
	for in, want := range cases {
		if got := mapConnState(in); got != want {
			t.Fatalf("mapConnState(%v)=%v want %v", in, got, want)
		}
	}
}
