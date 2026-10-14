package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

type fakeConsoleRuntime struct {
	stream    *fakeConsoleStream
	vncStream *fakeConsoleStream // nil ⇒ 复用 stream
	vncErr    error
}

func (f *fakeConsoleRuntime) Console(context.Context, string) (io.ReadWriteCloser, error) {
	return f.stream, nil
}

func (f *fakeConsoleRuntime) VNC(context.Context, string) (io.ReadWriteCloser, error) {
	if f.vncErr != nil {
		return nil, f.vncErr
	}
	if f.vncStream != nil {
		return f.vncStream, nil
	}
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
		Kind      string `json:"kind"`
	}
	if err := json.Unmarshal(data, &got); err != nil || got.WSURL == "" || got.ExpiresIn <= 0 {
		t.Fatalf("响应不符: %s", data)
	}
	// 控制台形态分派（决策 #485，Task 10）：serial（缺省）形态随响应如实给出 kind。
	if got.Kind != "serial" {
		t.Fatalf("serial VM 的 kind 应为 serial: %s", data)
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

// 控制台形态分派（决策 #485，Task 10）：有效 console=vnc 的 VM，POST 控制台端点**不下发 ticket**
// ——只回 `kind=vnc`（VNC 是图形控制台、文本串口 WS 不适用），且不存在可用 ticket 可连 WS。
// 与 CLI 侧 TestConsoleCommandRejectsVNC 同源（同一 consoleFormOf 判定）。
func TestConsoleTicketVNCNoTicket(t *testing.T) {
	fc := &fakeConsoleRuntime{stream: newFakeConsoleStream("")}
	ts := newTestServerOpts(t, Options{VM: newFakeVM(), VMConsole: fc})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)
	// 实例级 startup 覆盖把有效控制台设为 vnc（实例覆盖 > 镜像画像 > 产品默认）。
	body := vmBody("fw-vm")
	body["startup"] = map[string]any{"console": "vnc"}
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		body, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("建 vnc VM: %d %s", status, data)
	}

	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm/console", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("vnc console 端点应 200（如实告知形态，非拒绝）: %d %s", status, data)
	}
	var got struct {
		Kind      string `json:"kind"`
		WSURL     string `json:"ws_url"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if got.Kind != "vnc" {
		t.Fatalf("有效 console=vnc 时 kind 应为 vnc: %s", data)
	}
	if got.WSURL != "" || got.ExpiresIn != 0 {
		t.Fatalf("vnc 不应下发 ticket（无 ws_url/expires_in）: %s", data)
	}
	// 无 ticket 即无从连入：带任意 ticket 的 WS 握手必须失败。
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + APIPrefix + "/virtual-machine-functions/fw-vm/console/ws?ticket=bogus"
	if _, err := websocket.Dial(u, "", ts.URL); err == nil {
		t.Fatal("vnc 未签发 ticket，console WS 握手应失败")
	}
}

// 决策 #485：VNC 图形查看器凭证端点（POST .../console/vnc）+ WS 代理。
// 覆盖：ticket 签发/消费、WS↔VNC 字节双向桥接、形态不符 409、VM 不存在 404、
// 以及**跨资源隔离**（vnc ticket 开不了串口 console，串口 ticket 开不了 vnc）。
func TestConsoleVNCTicketAndIsolation(t *testing.T) {
	fc := &fakeConsoleRuntime{stream: newFakeConsoleStream("RFB 003.008\n")}
	ts := newTestServerOpts(t, Options{VM: newFakeVM(), VMConsole: fc})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)

	// vnc VM（实例级 startup 覆盖）与一台缺省 serial VM（用于隔离对照）。
	vbody := vmBody("vnc-vm")
	vbody["startup"] = map[string]any{"console": "vnc"}
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		vbody, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("建 vnc VM: %d %s", status, data)
	}
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		vmBody("ser-vm"), map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("建 serial VM: %d %s", status, data)
	}

	// 形态不符（serial VM 申请 vnc 凭证）→ 409。
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions/ser-vm/console/vnc", token, nil, nil); status != http.StatusConflict {
		t.Fatalf("非 vnc VM 应 409: %d %s", status, data)
	}
	// VM 不存在 → 404。
	if status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions/ghost/console/vnc", token, nil, nil); status != http.StatusNotFound {
		t.Fatalf("VM 不存在应 404: %d", status)
	}

	// vnc ticket 签发。
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions/vnc-vm/console/vnc", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("vnc 凭证应 200: %d %s", status, data)
	}
	var got struct {
		WSURL     string `json:"ws_url"`
		ExpiresIn int    `json:"expires_in"`
		Kind      string `json:"kind"`
	}
	if err := json.Unmarshal(data, &got); err != nil || got.WSURL == "" || got.ExpiresIn <= 0 || got.Kind != "vnc" {
		t.Fatalf("vnc 凭证响应不符: %s", data)
	}
	vncTicket := got.WSURL[strings.Index(got.WSURL, "ticket=")+len("ticket="):]

	// WS 桥接：读到 VNC 服务问候，写入到达 VNC 流（字节原样透传）。
	vwsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + got.WSURL
	conn, err := websocket.Dial(vwsURL, "", ts.URL)
	if err != nil {
		t.Fatalf("vnc ws 连接失败: %v", err)
	}
	buf := make([]byte, len("RFB 003.008\n"))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "RFB 003.008\n" {
		t.Fatalf("应读到 VNC 问候: %q err=%v", buf, err)
	}
	if _, err := conn.Write([]byte("RFB 003.008\n")); err != nil {
		t.Fatalf("写入 vnc 流: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if w := fc.stream.wrote.String(); !strings.Contains(w, "RFB 003.008\n") {
		t.Fatalf("字节应到达 VNC 流: %q", w)
	}
	_ = conn.Close()

	// 串口 ticket（对照）：同 VM 名也开不了 vnc——资源键前缀隔离。
	_, _, sdata := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions/ser-vm/console", token, nil, nil)
	var sgot struct {
		WSURL string `json:"ws_url"`
	}
	if err := json.Unmarshal(sdata, &sgot); err != nil || sgot.WSURL == "" {
		t.Fatalf("serial 凭证响应不符: %s", sdata)
	}
	serialTicket := sgot.WSURL[strings.Index(sgot.WSURL, "ticket=")+len("ticket="):]

	// 隔离①：vnc ticket 拿去开**串口** console WS（同 VM 名 vnc-vm）应 401。
	if _, err := websocket.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+APIPrefix+
		"/virtual-machine-functions/vnc-vm/console/ws?ticket="+vncTicket, "", ts.URL); err == nil {
		t.Fatal("vnc ticket 不应能开串口 console（资源键应隔离）")
	}
	// 隔离②：串口 ticket 拿去开 **vnc** WS 应 401。
	if _, err := websocket.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+APIPrefix+
		"/virtual-machine-functions/ser-vm/console/vnc/ws?ticket="+serialTicket, "", ts.URL); err == nil {
		t.Fatal("串口 ticket 不应能开 vnc（资源键应隔离）")
	}

	// 审计：vnc 打开/关闭各一条 vm.vnc。
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/audit-logs?limit=50", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "vm.vnc") {
		t.Fatalf("审计应含 vm.vnc: %d %s", status, data)
	}
}

// tcpDialConsoleRuntime VNC 侧拨一个**真实 TCP 监听**（模拟域 VNC 服务就在 127.0.0.1:<port>）；
// 用来把 WS↔VNC 代理跑在真 socket 上，而不是内存流假件。
type tcpDialConsoleRuntime struct{ addr string }

func (r *tcpDialConsoleRuntime) Console(context.Context, string) (io.ReadWriteCloser, error) {
	return net.Dial("tcp", r.addr)
}
func (r *tcpDialConsoleRuntime) VNC(context.Context, string) (io.ReadWriteCloser, error) {
	return net.Dial("tcp", r.addr)
}

// 决策 #485：WS 代理对**真实 TCP 监听**双向透传——服务端先发 VNC 问候，客户端写入回环抵达 TCP 侧。
func TestConsoleVNCWSProxyToTCPListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听: %v", err)
	}
	defer ln.Close()

	got := make(chan string, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte("RFB-TCP-LISTENER\n"))
		buf := make([]byte, 64)
		n, _ := c.Read(buf)
		got <- string(buf[:n])
	}()

	ts := newTestServerOpts(t, Options{VM: newFakeVM(), VMConsole: &tcpDialConsoleRuntime{addr: ln.Addr().String()}})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)
	vbody := vmBody("vnc-vm")
	vbody["startup"] = map[string]any{"console": "vnc"}
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		vbody, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("建 vnc VM: %d %s", status, data)
	}
	_, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions/vnc-vm/console/vnc", token, nil, nil)
	var got2 struct {
		WSURL string `json:"ws_url"`
	}
	if err := json.Unmarshal(data, &got2); err != nil || got2.WSURL == "" {
		t.Fatalf("vnc 凭证响应不符: %s", data)
	}

	conn, err := websocket.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+got2.WSURL, "", ts.URL)
	if err != nil {
		t.Fatalf("ws 连接失败: %v", err)
	}
	defer conn.Close()
	buf := make([]byte, len("RFB-TCP-LISTENER\n"))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "RFB-TCP-LISTENER\n" {
		t.Fatalf("应读到 TCP 监听端的问候: %q err=%v", buf, err)
	}
	if _, err := conn.Write([]byte("PING-FROM-WS")); err != nil {
		t.Fatalf("写入: %v", err)
	}
	select {
	case s := <-got:
		if s != "PING-FROM-WS" {
			t.Fatalf("TCP 侧应收到 WS 写入，得到 %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TCP 侧未收到 WS 写入（代理未双向透传）")
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

// 决策 #374（R142 B4/E5）：ticket 表随访问清扫——未消费的一次性 ticket（如 -c 脚本模式
// 申请了却不接管终端）此前永久驻留（TTL 只在消费时校验）。
func TestConsoleTicketsPrunedOnIssue(t *testing.T) {
	clk := time.Unix(1_700_000_000, 0)
	tix := newConsoleTickets()
	tix.now = func() time.Time { return clk }
	tix.ttl = time.Minute

	// 签发 3 张都不消费
	for i := 0; i < 3; i++ {
		if _, _, err := tix.issue(ctShellResource("ct-a"), "admin"); err != nil {
			t.Fatalf("issue: %v", err)
		}
	}
	tix.mu.Lock()
	n := len(tix.m)
	tix.mu.Unlock()
	if n != 3 {
		t.Fatalf("前置：应有 3 条：%d", n)
	}
	// 过期后再签发一张 ⇒ 旧条目应被清扫（只剩新的一张）
	clk = clk.Add(2 * time.Minute)
	if _, _, err := tix.issue(ctShellResource("ct-a"), "admin"); err != nil {
		t.Fatalf("issue: %v", err)
	}
	tix.mu.Lock()
	n = len(tix.m)
	tix.mu.Unlock()
	if n != 1 {
		t.Fatalf("过期条目应被清扫（旧实现永久驻留），实得 %d", n)
	}
}

// 决策 #485 契约 §4.3：显式设定的 console 形态独立于 serial_console 开关——显式 telnet 即使
// serial_console=false，域名里仍有串口，console 端点应签发 ticket（不再被误拒）。
func TestConsoleTicketExplicitFormOverridesSerialOff(t *testing.T) {
	fc := &fakeConsoleRuntime{stream: newFakeConsoleStream("")}
	ts := newTestServerOpts(t, Options{VM: newFakeVM(), VMConsole: fc})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)

	body := vmBody("fw-vm")
	body["serial_console"] = false
	body["startup"] = map[string]any{"console": "telnet"}
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		body, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("建 VM: %d %s", status, data)
	}
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm/console", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("显式 telnet 应签发 ticket: %d %s", status, data)
	}
	var got struct {
		WSURL string `json:"ws_url"`
		Kind  string `json:"kind"`
	}
	if err := json.Unmarshal(data, &got); err != nil || got.Kind != "telnet" || got.WSURL == "" {
		t.Fatalf("应回 kind=telnet + ws_url: %s", data)
	}
}

// 未显式设定 + serial_console=false ⇒ 域内无串口，console 端点如实 409。
func TestConsoleTicketSerialOffNoConsole(t *testing.T) {
	fc := &fakeConsoleRuntime{stream: newFakeConsoleStream("")}
	ts := newTestServerOpts(t, Options{VM: newFakeVM(), VMConsole: fc})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)

	body := vmBody("fw-vm")
	body["serial_console"] = false
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		body, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("建 VM: %d %s", status, data)
	}
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm/console", token, nil, nil)
	if status != http.StatusConflict {
		t.Fatalf("未启用串口应 409: %d %s", status, data)
	}
	if !strings.Contains(string(data), "serial_console=false") {
		t.Fatalf("409 文案应点明 serial_console=false: %s", data)
	}
}

// hijackResponseWriter 是一个能被抓握（Hijack）的最小 http.ResponseWriter，
// 用于在进程内把 WS 服务端的字节写进 net.Pipe，从而直接读**线上原始帧**。
type hijackResponseWriter struct {
	conn net.Conn
	hdr  http.Header
	rw   *bufio.ReadWriter
}

func (w *hijackResponseWriter) Header() http.Header {
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}
func (w *hijackResponseWriter) Write(b []byte) (int, error) { return w.conn.Write(b) }
func (w *hijackResponseWriter) WriteHeader(int)             {}
func (w *hijackResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.rw == nil {
		w.rw = bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn))
	}
	return w.conn, w.rw, nil
}

// readUntilDoubleCRLF 逐字节读到 HTTP 头结束（\r\n\r\n）为止，返回读到的原始字节。
func readUntilDoubleCRLF(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		b, err := br.ReadByte()
		if err != nil {
			return out, err
		}
		out = append(out, b)
		if len(out) >= 4 && string(out[len(out)-4:]) == "\r\n\r\n" {
			return out, nil
		}
	}
}

// wsFrameOpcodeInProcess 在**进程内**完成一次 WS 握手并返回服务端→客户端**首个 WS 帧**的 opcode。
// 为何这么做：x/net 的客户端读帧时对 opcode 无感，看不出「二进制当文本发」这个缺陷；而把真实
// 客户端/代理夹在中间时，x/net 服务端握手在本环境会出现「握手后即关连接」的竞态（帧被丢）。
// 用 net.Pipe 直接对接握手后的帧写入，既确定性、又读到真正的线上字节——这正是 noVNC
// （binaryType=arraybuffer，逐字节读 RFB）依赖的东西。
func wsFrameOpcodeInProcess(t *testing.T, h http.Handler, path string) byte {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Origin", "http://nfvis.local")
	go func() {
		defer server.Close()
		h.ServeHTTP(&hijackResponseWriter{conn: server}, req)
	}()
	br := bufio.NewReader(client)
	raw, err := readUntilDoubleCRLF(br)
	if err != nil {
		t.Fatalf("读握手响应: %v（已读 %q）", err, raw)
	}
	if !strings.Contains(string(raw), "101") {
		t.Fatalf("WS 未升级: %q", raw)
	}
	b, err := br.ReadByte()
	if err != nil {
		t.Fatalf("读服务端帧首字节: %v", err)
	}
	return b & 0x0f
}

// frameConsoleRuntime 每次调用都返回**新的**假串口流（一条会话一个）——假流「读一次问候即阻塞」，
// 一条会话消费掉问候后再复用就只剩阻塞，故不能像 fakeConsoleRuntime 那样共享同一个流。
type frameConsoleRuntime struct {
	serial string
	vnc    string
}

func (r frameConsoleRuntime) Console(context.Context, string) (io.ReadWriteCloser, error) {
	return newFakeConsoleStream(r.serial), nil
}
func (r frameConsoleRuntime) VNC(context.Context, string) (io.ReadWriteCloser, error) {
	return newFakeConsoleStream(r.vnc), nil
}

// C1 回归：VNC WS 代理服务端→客户端必须发**二进制帧**（opcode 0x2），否则 noVNC 收到的是
// 文本帧（JS 里变字符串、Uint8Array 归零、RFB 握手拿不到字节 ⇒ 黑屏，非 UTF-8 还会被浏览器以
// 1007 关帧）。同时锁定**串口 console 仍为文本帧**（0x1）——文本终端语义不变。
func TestConsoleWSFrameOpcodeBinaryVsText(t *testing.T) {
	ts := newTestServerOpts(t, Options{VM: newFakeVM(), VMConsole: frameConsoleRuntime{serial: "HELLO-CONSOLE\r\n", vnc: "RFB 003.008\n"}})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)

	// VNC 形态 VM：服务端问候须以二进制帧送出（0x2）。
	vbody := vmBody("vnc-vm")
	vbody["startup"] = map[string]any{"console": "vnc"}
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		vbody, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("建 vnc VM: %d %s", status, data)
	}
	_, _, vdata := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions/vnc-vm/console/vnc", token, nil, nil)
	var vgot struct {
		WSURL string `json:"ws_url"`
	}
	if err := json.Unmarshal(vdata, &vgot); err != nil || vgot.WSURL == "" {
		t.Fatalf("vnc 凭证响应不符: %s", vdata)
	}
	vtok := vgot.WSURL[strings.Index(vgot.WSURL, "ticket=")+len("ticket="):]
	vpath := APIPrefix + "/virtual-machine-functions/vnc-vm/console/vnc/ws?ticket=" + vtok
	if op := wsFrameOpcodeInProcess(t, ts.Config.Handler, vpath); op != 0x2 {
		t.Fatalf("VNC 服务端→客户端首帧 opcode = 0x%x，期望 0x2（二进制）；noVNC 需要二进制帧读 RFB", op)
	}

	// 串口形态 VM（缺省 serial）：文本终端仍走文本帧（0x1）——既有行为不变。
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		vmBody("ser-vm"), map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("建 serial VM: %d %s", status, data)
	}
	_, _, sdata := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions/ser-vm/console", token, nil, nil)
	var sgot struct {
		WSURL string `json:"ws_url"`
	}
	if err := json.Unmarshal(sdata, &sgot); err != nil || sgot.WSURL == "" {
		t.Fatalf("serial 凭证响应不符: %s", sdata)
	}
	stok := sgot.WSURL[strings.Index(sgot.WSURL, "ticket=")+len("ticket="):]
	spath := APIPrefix + "/virtual-machine-functions/ser-vm/console/ws?ticket=" + stok
	if op := wsFrameOpcodeInProcess(t, ts.Config.Handler, spath); op != 0x1 {
		t.Fatalf("串口 console 首帧 opcode = 0x%x，期望 0x1（文本帧，行为须与既有文本终端一致）", op)
	}
}
