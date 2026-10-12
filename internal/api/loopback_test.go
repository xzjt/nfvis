package api

// 决策 #478：管理面在管理网卡之外**同时监听回环 127.0.0.1:<port>**，使本机 nfvis-cli 缺省
// （DefaultServer = https://127.0.0.1:443）可用，且不扩大对外暴露面。
//
// 本文件钉住：① 决策函数 loopbackListenAddr（通配/回环 → 不绑；具体非回环 → 127.0.0.1:port）；
// ② 主监听为具体非回环时**确实多出一个回环监听并能服务**；③ 主监听为通配/回环时**不重复绑**
// （不 EADDRINUSE、不误报）；④ **回环绑定失败不影响主监听**（主监听仍服务）；⑤ Rebind 主地址
// 变更后回环按新地址重新协调（具体↔通配切换）。
//
// 跨平台：Windows 上真绑「具体非回环地址」不可行——用可注入的监听工厂（Server.listenFn）
// 包装真实监听、上报另一个（具体非回环）Addr 来模拟该场景；真服务仍落在包装的真实监听上。

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/system"
)

// strAddr 自定义 net.Addr（overriding 监听上报串）。
type strAddr string

func (strAddr) Network() string  { return "tcp" }
func (a strAddr) String() string { return string(a) }

// renamedListener 包装真实监听，但 Addr() 上报另一个串——用于模拟「具体非回环主地址」。
// Accept/Close 透传到底层真实监听（服务实际落在真实地址上，可被测试访问）。
type renamedListener struct {
	net.Listener
	reported string
}

func (l renamedListener) Addr() net.Addr { return strAddr(l.reported) }

// freeTCPPort 取一个当前空闲的 127.0.0.1 端口。
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口: %v", err)
	}
	defer l.Close() //nolint:errcheck
	return l.Addr().(*net.TCPAddr).Port
}

// loopbackFactory 可注入的监听工厂：concreteHost==请求主机时返回「上报为具体非回环地址」的
// 真实监听（模拟具体非回环主监听）；failLoop 时令回环请求（host=127.0.0.1）绑定失败。
type loopbackFactory struct {
	concreteHost string
	loopPort     int
	failLoop     bool

	mu        sync.Mutex
	requested []string
	realMain  net.Listener
}

func (f *loopbackFactory) listen(network, addr string) (net.Listener, error) {
	host, _, _ := net.SplitHostPort(addr)
	f.mu.Lock()
	f.requested = append(f.requested, addr)
	f.mu.Unlock()

	if host == f.concreteHost {
		real, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.realMain = real
		f.mu.Unlock()
		return renamedListener{Listener: real, reported: net.JoinHostPort(f.concreteHost, strconv.Itoa(f.loopPort))}, nil
	}
	if f.failLoop && host == "127.0.0.1" {
		return nil, fmt.Errorf("注入：回环绑定失败")
	}
	return net.Listen(network, addr)
}

func (f *loopbackFactory) reqs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requested...)
}

func (f *loopbackFactory) realMainAddr() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.realMain == nil {
		return ""
	}
	return f.realMain.Addr().String()
}

// waitLoopback 轮询等待回环监听记录为 want（空串表示等待其被清除）。
func waitLoopback(t *testing.T, srv *Server, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if srv.LoopbackAddr() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("回环监听地址未收敛: want=%q got=%q", want, srv.LoopbackAddr())
}

// waitHTTPStatus 轮询 addr 直到返回 want（超时报错并给出最后状态/错误）。
func waitHTTPStatus(t *testing.T, addr string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last int
	var lastErr error
	for time.Now().Before(deadline) {
		got, err := serveStatus(addr)
		if err == nil && got == want {
			return
		}
		last, lastErr = got, err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("地址 %s 未返回 %d: status=%d err=%v", addr, want, last, lastErr)
}

// 决策函数：通配/回环 → 不绑；具体非回环 IP → 127.0.0.1:<port>。
func TestLoopbackListenAddrDecision(t *testing.T) {
	cases := []struct {
		main     string
		want     string
		wantNeed bool
	}{
		{":443", "", false},                            // 通配
		{"0.0.0.0:443", "", false},                     // 通配（归一后即 :443，此处直给再验）
		{"[::]:443", "", false},                        // 通配 v6
		{"127.0.0.1:443", "", false},                   // 已是回环
		{"127.0.0.5:8443", "", false},                  // 回环网段内任意地址
		{"[::1]:443", "", false},                       // 回环 v6
		{"192.168.155.129:443", "127.0.0.1:443", true}, // 具体非回环 → 回环同端口
		{"10.1.2.3:8443", "127.0.0.1:8443", true},
		{"[2001:db8::1]:443", "127.0.0.1:443", true}, // 具体非回环 v6
		{"没有端口", "", false},                          // 非法：不绑
	}
	for _, c := range cases {
		got, need := loopbackListenAddr(c.main)
		if need != c.wantNeed || got != c.want {
			t.Errorf("loopbackListenAddr(%q) = (%q,%v)，期望 (%q,%v)", c.main, got, need, c.want, c.wantNeed)
		}
	}
}

// 主监听为通配时开服：不重复绑回环（工厂只被请求一次），无 EADDRINUSE/误报。
func TestListenAndServeNoLoopbackForWildcardMain(t *testing.T) {
	srv := newRebindServer(t)
	f := &loopbackFactory{}
	srv.listenFn = f.listen
	srv.http.Addr = ":0"

	_, done := startRebindServer(t, srv)
	if got := srv.LoopbackAddr(); got != "" {
		t.Fatalf("通配主监听不应有回环监听，got %q", got)
	}
	if reqs := f.reqs(); len(reqs) != 1 {
		t.Fatalf("通配主监听应只请求一次绑定，实际请求 %v", reqs)
	}
	assertStillRunning(t, done)
}

// 主监听本身为回环时开服：不重复绑回环。
func TestListenAndServeNoLoopbackForLoopbackMain(t *testing.T) {
	srv := newRebindServer(t) // Addr = 127.0.0.1:0
	f := &loopbackFactory{}
	srv.listenFn = f.listen

	_, done := startRebindServer(t, srv)
	if got := srv.LoopbackAddr(); got != "" {
		t.Fatalf("回环主监听不应再有回环监听，got %q", got)
	}
	if reqs := f.reqs(); len(reqs) != 1 {
		t.Fatalf("回环主监听应只请求一次绑定，实际请求 %v", reqs)
	}
	assertStillRunning(t, done)
}

// 主监听为具体非回环地址：**确实多出一个 127.0.0.1:<port> 监听并能服务**。
func TestListenAndServeBindsLoopbackForConcreteMain(t *testing.T) {
	srv := newRebindServer(t)
	port := freeTCPPort(t)
	f := &loopbackFactory{concreteHost: "192.0.2.10", loopPort: port}
	srv.listenFn = f.listen
	srv.http.Addr = "192.0.2.10:0"

	_, done := startRebindServer(t, srv)
	want := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	waitLoopback(t, srv, want)
	if got := srv.BoundAddr(); got != net.JoinHostPort("192.0.2.10", strconv.Itoa(port)) {
		t.Fatalf("主监听上报串不符: got %q", got)
	}
	// 回环监听确实在服务（本机 CLI 缺省可达）。
	waitHTTPStatus(t, want, http.StatusOK)
	assertStillRunning(t, done)
}

// 回环绑定失败不影响主监听：主监听仍正常服务，回环只记日志（无回环地址记录）、不致命。
func TestLoopbackBindFailureDoesNotAffectMain(t *testing.T) {
	srv := newRebindServer(t)
	port := freeTCPPort(t)
	f := &loopbackFactory{concreteHost: "192.0.2.10", loopPort: port, failLoop: true}
	srv.listenFn = f.listen
	srv.http.Addr = "192.0.2.10:0"

	_, done := startRebindServer(t, srv)
	if got := srv.LoopbackAddr(); got != "" {
		t.Fatalf("回环绑定失败时不应记录回环地址，got %q", got)
	}
	// 主监听（包装监听的真实底层地址）仍在服务——回环失败不得拖垮管理面。
	realMain := f.realMainAddr()
	if realMain == "" {
		t.Fatal("未捕获主监听的真实底层地址")
	}
	waitHTTPStatus(t, realMain, http.StatusOK)
	assertStillRunning(t, done)
}

// Rebind 主地址变更后回环按新地址协调：具体→通配（关回环）、通配→具体（补回环）。
func TestRebindReconcilesLoopback(t *testing.T) {
	srv := newRebindServer(t)
	port := freeTCPPort(t)
	f := &loopbackFactory{concreteHost: "192.0.2.10", loopPort: port}
	srv.listenFn = f.listen
	srv.http.Addr = "192.0.2.10:0" // 具体非回环 ⇒ 初建即应有回环

	_, done := startRebindServer(t, srv)
	want := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	waitLoopback(t, srv, want)

	// 换成通配主监听 ⇒ 回环应被关闭（通配已覆盖回环）。
	if err := srv.Rebind(":0"); err != nil {
		t.Fatalf("Rebind(:0): %v", err)
	}
	waitLoopback(t, srv, "")

	// 换回具体非回环 ⇒ 回环应按新主地址补回。
	if err := srv.Rebind("192.0.2.10:0"); err != nil {
		t.Fatalf("Rebind(具体): %v", err)
	}
	waitLoopback(t, srv, want)
	waitHTTPStatus(t, want, http.StatusOK)
	assertStillRunning(t, done)
}

// 具体主地址（带回环）→ **同端口通配**主监听：换绑须先放掉回环，否则绑通配会撞上自己
// 占着的 127.0.0.1:<port>（EADDRINUSE）而被误判失败。
//
// Windows 的「弱绑定」会容忍通配与具体地址并存、掩蔽该冲突，故在注入工厂里**探测**回环端口：
// 若绑通配前 127.0.0.1:<port> 仍被占用（= 回环未先释放），就按真实 OS 的 EADDRINUSE 返回失败。
func TestRebindConcreteToWildcardSamePort(t *testing.T) {
	srv := newRebindServer(t)
	port := freeTCPPort(t)
	loop := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	srv.listenFn = func(network, addr string) (net.Listener, error) {
		host, _, _ := net.SplitHostPort(addr)
		switch {
		case host == "192.0.2.10": // 具体非回环主监听（上报该地址，实际落在真实监听上）
			real, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			return renamedListener{Listener: real, reported: net.JoinHostPort("192.0.2.10", strconv.Itoa(port))}, nil
		case host == "": // 通配主监听：真实 OS 上会与「仍在的回环」冲突
			probe, err := net.Listen("tcp", loop)
			if err != nil {
				return nil, fmt.Errorf("模拟 EADDRINUSE：回环 %s 换绑前未释放", loop)
			}
			_ = probe.Close()
			return net.Listen(network, addr)
		default:
			return net.Listen(network, addr)
		}
	}
	srv.http.Addr = "192.0.2.10:0"

	_, done := startRebindServer(t, srv)
	waitLoopback(t, srv, loop)

	if err := srv.Rebind(":" + strconv.Itoa(port)); err != nil {
		t.Fatalf("具体→同端口通配换绑应成功（须先放掉回环）: %v", err)
	}
	waitLoopback(t, srv, "")               // 通配已覆盖回环 ⇒ 回环应被关闭
	waitHTTPStatus(t, loop, http.StatusOK) // 通配主监听覆盖 127.0.0.1:port
	assertStillRunning(t, done)
}

// TLS 路径：主监听与回环监听两者都以 HTTPS 服务（同一证书），验证两代 serve 共用 TLSConfig
// 无冲突（决策 #478 引入的 tlsOnce）。
func TestConcreteMainAndLoopbackServeTLS(t *testing.T) {
	srv := newRebindServer(t)
	dir := t.TempDir()
	mgr := system.NewTLSManager(dir, nil)
	mgr.SetListen("127.0.0.1:0")
	if _, err := mgr.RegenerateSelfSigned("nfvis"); err != nil {
		t.Fatalf("生成自签证书: %v", err)
	}
	srv.tlsCert, srv.tlsKey = mgr.CertPath(), mgr.KeyPath()

	port := freeTCPPort(t)
	f := &loopbackFactory{concreteHost: "192.0.2.10", loopPort: port}
	srv.listenFn = f.listen
	srv.http.Addr = "192.0.2.10:0"

	_, done := startRebindServer(t, srv)
	want := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	waitLoopback(t, srv, want)

	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // 测试自签
	}}
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := client.Get("https://" + want + APIPrefix + "/openapi.json")
		if err == nil {
			status := resp.StatusCode
			resp.Body.Close()
			if status != http.StatusOK {
				t.Fatalf("回环 HTTPS 状态 %d", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("回环 HTTPS 不可达: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	assertStillRunning(t, done)
}
