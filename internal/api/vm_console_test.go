package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// ---------- ticket 表 ----------

func TestConsoleTicketsSingleUseAndExpiry(t *testing.T) {
	ts := newConsoleTickets()
	tok, ttl, err := ts.issue("fw-vm", "admin")
	if err != nil || tok == "" || ttl <= 0 {
		t.Fatalf("issue: %q ttl=%d err=%v", tok, ttl, err)
	}
	if _, ok := ts.consume("fw-vm", tok); !ok {
		t.Fatal("首次消费应成功")
	}
	if _, ok := ts.consume("fw-vm", tok); ok {
		t.Fatal("ticket 必须一次性（二次消费应失败）")
	}
	if _, ok := ts.consume("other-vm", tok); ok {
		t.Fatal("ticket 绑定 VM 名，异名应失败")
	}
	if _, ok := ts.consume("fw-vm", ""); ok {
		t.Fatal("空 ticket 应失败")
	}

	// 过期：注入时钟。
	ts2 := newConsoleTickets()
	now := time.Now()
	ts2.now = func() time.Time { return now }
	tok2, _, _ := ts2.issue("fw-vm", "admin")
	ts2.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, ok := ts2.consume("fw-vm", tok2); ok {
		t.Fatal("过期 ticket 应失败")
	}
}

// ---------- HTTP / WebSocket ----------

// fakeConsoleStream 首帧问候后阻塞直到关闭；写入内容记入 wrote。
type fakeConsoleStream struct {
	greeting *strings.Reader
	wrote    strings.Builder
	closed   chan struct{}
	once     sync.Once
}

func newFakeConsoleStream(greeting string) *fakeConsoleStream {
	return &fakeConsoleStream{greeting: strings.NewReader(greeting), closed: make(chan struct{})}
}

func (f *fakeConsoleStream) Read(p []byte) (int, error) {
	if n, err := f.greeting.Read(p); n > 0 {
		return n, nil
	} else if err != io.EOF {
		return n, err
	}
	<-f.closed
	return 0, io.EOF
}

func (f *fakeConsoleStream) Write(p []byte) (int, error) {
	f.wrote.Write(p)
	return len(p), nil
}

func (f *fakeConsoleStream) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

type fakeConsoleRuntime struct{ stream *fakeConsoleStream }

func (f *fakeConsoleRuntime) Console(context.Context, string) (io.ReadWriteCloser, error) {
	return f.stream, nil
}

func TestConsoleTicketAndWebSocketBridge(t *testing.T) {
	fc := &fakeConsoleRuntime{stream: newFakeConsoleStream("HELLO-CONSOLE\r\n")}
	ts := newTestServerOpts(t, Options{VM: newFakeVM(), VMConsole: fc})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		vmBody("fw-vm"), map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("建 VM: %d %s", status, data)
	}

	// 申请 ticket
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm/console", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("console 凭证应 200: %d %s", status, data)
	}
	var got struct {
		WSURL     string `json:"ws_url"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &got); err != nil || got.WSURL == "" || got.ExpiresIn <= 0 {
		t.Fatalf("响应不符: %s", data)
	}

	// WebSocket 连接：收到串口问候，写入内容到达 guest 侧
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + got.WSURL
	conn, err := websocket.Dial(wsURL, "", ts.URL)
	if err != nil {
		t.Fatalf("ws 连接失败: %v", err)
	}
	buf := make([]byte, len("HELLO-CONSOLE\r\n"))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "HELLO-CONSOLE\r\n" {
		t.Fatalf("应读到串口问候: %q err=%v", buf, err)
	}
	if _, err := conn.Write([]byte("whoami\n")); err != nil {
		t.Fatalf("写入 console: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := fc.stream.wrote.String(); !strings.Contains(got, "whoami") {
		t.Fatalf("输入应到达串口: %q", got)
	}
	_ = conn.Close()

	// ticket 一次性：同 URL 再次握手应 401
	if _, err := websocket.Dial(wsURL, "", ts.URL); err == nil {
		t.Fatal("重复使用 ticket 应失败")
	}

	// 审计（FR-OPS-032）：open/close 各一条
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/audit-logs?limit=50", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "vm.console") {
		t.Fatalf("审计应含 vm.console: %d %s", status, data)
	}
}

func TestConsoleErrors(t *testing.T) {
	// 未装配 console：503
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm/console", token, nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("未装配应 503: %d", status)
	}

	// 已装配但 VM 不存在：404
	fc := &fakeConsoleRuntime{stream: newFakeConsoleStream("")}
	ts2 := newTestServerOpts(t, Options{VM: newFakeVM(), VMConsole: fc})
	token2 := loginAdmin(t, ts2)
	status, _, _ = cfgRequest(t, http.MethodPost, ts2.URL+APIPrefix+"/virtual-machine-functions/ghost/console", token2, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("VM 不存在应 404: %d", status)
	}
}

func TestConsoleWSRejectsBadTicket(t *testing.T) {
	fc := &fakeConsoleRuntime{stream: newFakeConsoleStream("")}
	ts := newTestServerOpts(t, Options{VM: newFakeVM(), VMConsole: fc})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)
	cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		vmBody("fw-vm"), map[string]string{"X-NFVIS-Auto-Commit": "true"})

	u := "ws" + strings.TrimPrefix(ts.URL, "http") + APIPrefix + "/virtual-machine-functions/fw-vm/console/ws?ticket=bogus"
	if _, err := websocket.Dial(u, "", ts.URL); err == nil {
		t.Fatal("无效 ticket 应拒绝握手")
	}
}
