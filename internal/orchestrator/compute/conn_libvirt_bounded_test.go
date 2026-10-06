package compute

// 底座调用有界化的单测守护：
//   - 决策 #349：Connect 握手有界（connectBounded）；
//   - 决策 #362：单次 RPC 硬上界（Conn.call）——go-libvirt 的单次 RPC 没有 per-call
//     deadline，仅给调用方 ctx 加 deadline 是不够的（只有被调方尊重 ctx 才有界），
//     故 call 在上界到期时关闭自持 conn 中断挂起调用。
//
// 假死现场 = 「socket 可连、协议握手零响应」：真 unix socket 在 Windows 开发机上
// 不可用，而 connectBounded 按 network/addr 参数化，故用 TCP 假服务（accept 后
// 不应答）复现。成功路径（真实协议握手）由真机集成覆盖（conn_libvirt.go 本就在
// check_coverage.sh 的 COVER_EXCLUDE 清单），这里不伪造 libvirt 协议应答。
// call 的用例不触碰真实 RPC：阻塞/返回都是注入的 fn，自持 conn 用 net.Pipe 观察。

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitalocean/go-libvirt"
)

// silentServer 起 TCP 假服务：accept 后不应答、不关闭（模拟 libvirtd 假死——
// accept 连接但对一切客户端零响应）。返回拨号地址。
func silentServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起假服务: %v", err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			// 不应答、不关闭：握手等不到任何字节
		}
	}()
	return ln.Addr().String()
}

// TestConnectBoundedTimeoutOnSilentServer 假死服务上 connectBounded 必须在 ctx
// 上界附近返回错误（而不是像包级 ConnectToURI 那样在 AuthList 握手上无限阻塞）。
func TestConnectBoundedTimeoutOnSilentServer(t *testing.T) {
	addr := silentServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	lv, raw, err := connectBounded(ctx, "tcp", addr, libvirt.ConnectURI("qemu:///system"))
	elapsed := time.Since(start)
	if err == nil {
		if lv != nil {
			_ = lv.Disconnect()
		}
		if raw != nil {
			_ = raw.Close()
		}
		t.Fatal("假死服务（accept 后零响应）上应返回错误")
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("应在 ctx 上界（300ms）附近返回，实际耗时 %s（err=%v）", elapsed, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("错误应包装 ctx 超时，实际: %v", err)
	}
}

// TestConnectBoundedNoGoroutineLeak 泄漏守护：超时关闭自持 conn 后，握手 goroutine
// 必须确定性退出（go-libvirt 解除路径：listen 退出 → waitAndDisconnect →
// deregisterAll 解除挂起的 getResponse）。轮询至多 5s 断言 goroutine 数回落基线 ±1。
func TestConnectBoundedNoGoroutineLeak(t *testing.T) {
	addr := silentServer(t)
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, _, err := connectBounded(ctx, "tcp", addr, libvirt.ConnectURI("qemu:///system")); err == nil {
		t.Fatal("假死服务上应返回错误")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		time.Sleep(50 * time.Millisecond)
		after := runtime.NumGoroutine()
		if after <= before+1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("握手 goroutine 未退出（泄漏）：基线 %d，现在 %d", before, after)
		}
	}
}

// TestConnectBoundedDialFailure 拨号失败（无监听）应立即返回错误，且不碰握手。
func TestConnectBoundedDialFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // 关掉：端口无监听

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := connectBounded(ctx, "tcp", addr, libvirt.ConnectURI("qemu:///system")); err == nil {
		t.Fatal("无监听端口应返回拨号错误")
	}
}

// TestLibvirtSocketPath socket 路径规则与 go-libvirt 同源：query 参数 socket 优先，
// 缺省 /var/run/libvirt/libvirt-sock。
func TestLibvirtSocketPath(t *testing.T) {
	u, err := url.Parse("qemu:///system")
	if err != nil {
		t.Fatalf("解析 URI: %v", err)
	}
	if got := libvirtSocketPath(u); got != "/var/run/libvirt/libvirt-sock" {
		t.Fatalf("缺省 socket 路径 = %q，应为 /var/run/libvirt/libvirt-sock", got)
	}

	u, err = url.Parse("qemu:///system?socket=/run/libvirt/custom.sock")
	if err != nil {
		t.Fatalf("解析 URI: %v", err)
	}
	if got := libvirtSocketPath(u); got != "/run/libvirt/custom.sock" {
		t.Fatalf("query socket 覆盖 = %q，应为 /run/libvirt/custom.sock", got)
	}
}

// TestLibvirtTransport 传输形态推导与 go-libvirt dialerForURI 同源（决定走哪条
// 有界路径：unix=自拨自持，其余=外层有界等待）。
func TestLibvirtTransport(t *testing.T) {
	cases := []struct {
		uri, want string
	}{
		{"qemu:///system", "unix"},
		{"qemu+unix:///system", "unix"},
		{"qemu+tcp://127.0.0.1:16509/system", "tcp"},
		{"qemu+ssh://host/system", "ssh"},
		{"qemu://host/system", "tls"},
	}
	for _, c := range cases {
		u, err := url.Parse(c.uri)
		if err != nil {
			t.Fatalf("解析 %s: %v", c.uri, err)
		}
		if got := libvirtTransport(u); got != c.want {
			t.Fatalf("libvirtTransport(%s) = %q，应为 %q", c.uri, got, c.want)
		}
	}
}

// newPipeConn 造一个「Conn.raw 持一端、测试持另一端」的假连接：raw 的关闭可被
// 对端确定性观察到（读 EOF），从而断言 call 的中断动作真的发生了。
func newPipeConn() (*Conn, net.Conn) {
	raw, peer := net.Pipe()
	return &Conn{l: libvirt.New(peer), uri: "qemu:///system", raw: raw}, peer
}

// assertPipeClosed 断言对端已关闭（读得到 EOF；写会 ErrClosedPipe）。带读 deadline，
// 避免误判时测试挂死。
func assertPipeClosed(t *testing.T, peer net.Conn) {
	t.Helper()
	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := peer.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("自持 conn 应已被关闭，但对端仍可读")
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("关闭后对端应读到 EOF，实际: %v", err)
	}
}

// assertPipeOpen 断言对端仍打开：读超时（os.ErrDeadlineExceeded）说明管道没被关，
// 而不是像关闭后那样立即 EOF。
func assertPipeOpen(t *testing.T, peer net.Conn) {
	t.Helper()
	_ = peer.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, err := peer.Read(make([]byte, 1))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("自持 conn 不应被关闭（期望读超时），实际: %v", err)
	}
	_ = peer.SetReadDeadline(time.Time{})
}

// TestCallQueuedTimeoutReportsBusyWithoutClosingConn（决策 #362 审查修正）：上界到期时
// 调用**还排在互斥队列里**（前面有合法地耗时较长的 RPC 正在执行，如快照创建/回滚）⇒
// 连接只是**忙**：不得关连接、不得打断前面的调用；返回 ErrConnBusy 供探活按健康跳过
// （dynamicCompute.Probe），避免「长操作被探活误杀 / 误判连接中断」。
func TestCallQueuedTimeoutReportsBusyWithoutClosingConn(t *testing.T) {
	c, peer := newPipeConn()
	defer peer.Close()
	release := make(chan struct{})
	executing := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- c.call(context.Background(), func(*libvirt.Libvirt) error {
			close(executing) // 第一段已进入执行（持有互斥、模拟长 RPC）
			<-release
			return nil
		})
	}()
	<-executing

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := c.call(ctx, func(*libvirt.Libvirt) error { return nil })
	if !errors.Is(err, ErrConnBusy) {
		t.Fatalf("排队超时应返回 ErrConnBusy，实际: %v", err)
	}
	assertPipeOpen(t, peer) // 关键：连接没被关（长操作不受牵连）

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("前面的调用应正常完成: %v", err)
	}
}

// TestCallHardBoundOnBlockedFnClosesSelfConn：fn 挂起（模拟 go-libvirt 挂死的
// getResponse）时，call 必须在 ctx 上界附近返回包装 ctx 超时的错误，并关闭自持 conn
// （中断手段 = 关 conn；#349 已核实 go-libvirt 随之 deregisterAll 解除挂起调用）。
func TestCallHardBoundOnBlockedFnClosesSelfConn(t *testing.T) {
	c, peer := newPipeConn()
	defer peer.Close()
	release := make(chan struct{})
	defer close(release) // 放行阻塞的 fn，避免测试结束仍挂着 goroutine

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.call(ctx, func(*libvirt.Libvirt) error {
		<-release // 不尊重 ctx、永不自行返回的调用
		return nil
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("阻塞 fn 应按 ctx 上界返回错误")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("错误应包装 ctx 超时，实际: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("应在 200ms 上界附近返回，实际 %s", elapsed)
	}
	assertPipeClosed(t, peer)
}

// TestCallPassesThroughFnErrorWithoutClosingConn：fn 正常返回错误时错误原样透传，
// 自持 conn 不被关闭（上界没到，不做中断）。
func TestCallPassesThroughFnErrorWithoutClosingConn(t *testing.T) {
	c, peer := newPipeConn()
	defer c.raw.Close()
	defer peer.Close()

	sentinel := errors.New("RPC 假错误")
	err := c.call(context.Background(), func(*libvirt.Libvirt) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("fn 错误应原样透传，实际: %v", err)
	}
	assertPipeOpen(t, peer)
}

// TestCallBoundFarLargerThanFnKeepsConn：上界远大于 fn 耗时时不误关连接。
func TestCallBoundFarLargerThanFnKeepsConn(t *testing.T) {
	c, peer := newPipeConn()
	defer c.raw.Close()
	defer peer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.call(ctx, func(*libvirt.Libvirt) error {
		time.Sleep(20 * time.Millisecond)
		return nil
	}); err != nil {
		t.Fatalf("正常返回的 fn 不应报错: %v", err)
	}
	assertPipeOpen(t, peer)
}

// TestCallClosedConnUsesExistingError：连接已 Close（l 置空）时走既有「连接已关闭」
// 错误；ctx 无 deadline（Background）也不影响——缺省上界只兜底，不改变错误语义。
func TestCallClosedConnUsesExistingError(t *testing.T) {
	c := &Conn{} // Close 之后的状态：l 为 nil
	if err := c.Define(context.Background(), "<domain/>"); !errors.Is(err, errLibvirtConnClosed) {
		t.Fatalf("应返回既有「连接已关闭」错误，实际: %v", err)
	}
}

// ---- 决策 #396：call 上界分层（只读/元数据 30s；作业类按调用方 deadline 放宽）----

// shortDefaultRPCTimeout 临时调小 defaultRPCTimeout（用完还原）。红-绿需要把「缺省上界」
// 压到测试可观测的量级，否则作业类的「放宽」要等 30s 才可辨。
func shortDefaultRPCTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := defaultRPCTimeout
	defaultRPCTimeout = d
	t.Cleanup(func() { defaultRPCTimeout = old })
}

// TestCallBoundLayering callBound 的分层语义（纯函数）：无 deadline 两档同缺省；调用方
// deadline 更短两档都取更短；调用方 deadline 更长时只读档被缺省封顶、作业档放宽。
func TestCallBoundLayering(t *testing.T) {
	// 无 deadline：两档均回落缺省上界。
	if got := callBound(context.Background(), false); got != defaultRPCTimeout {
		t.Fatalf("只读档无 deadline 应为缺省 %s，得 %s", defaultRPCTimeout, got)
	}
	if got := callBound(context.Background(), true); got != defaultRPCTimeout {
		t.Fatalf("作业档无 deadline 应回落缺省 %s，得 %s", defaultRPCTimeout, got)
	}

	// 调用方 deadline 更短：两档都取调用方。
	short, cancelShort := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShort()
	if got := callBound(short, false); got > 5*time.Second || got <= 0 {
		t.Fatalf("只读档应取更短的调用方 deadline（≈5s），得 %s", got)
	}
	if got := callBound(short, true); got > 5*time.Second || got <= 0 {
		t.Fatalf("作业档应取更短的调用方 deadline（≈5s），得 %s", got)
	}

	// 调用方 deadline 更长：只读档被缺省封顶，作业档放宽到调用方。
	long, cancelLong := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelLong()
	if got := callBound(long, false); got != defaultRPCTimeout {
		t.Fatalf("只读档应被缺省上界封顶为 %s，得 %s", defaultRPCTimeout, got)
	}
	if got := callBound(long, true); got <= defaultRPCTimeout || got > 5*time.Minute {
		t.Fatalf("作业档应放宽到调用方 deadline（> %s 且 ≤ 5m），得 %s", defaultRPCTimeout, got)
	}
}

// TestCallJobNotCappedByDefaultBound 作业类 RPC：调用方给了更长 deadline 时，不得被缺省
// 上界截断（红：旧行为只取更小值 ⇒ 150ms 即中断并关连接）。fn 睡 600ms——超过缺省 150ms、
// 但在调用方 3s deadline 内——应正常完成且连接未被关。
func TestCallJobNotCappedByDefaultBound(t *testing.T) {
	shortDefaultRPCTimeout(t, 150*time.Millisecond)
	c, peer := newPipeConn()
	defer c.raw.Close()
	defer peer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	err := c.callJob(ctx, func(*libvirt.Libvirt) error {
		time.Sleep(600 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatalf("作业类应按调用方 deadline 放宽（不被缺省上界截断），实际: %v（耗时 %s）", err, time.Since(start))
	}
	assertPipeOpen(t, peer) // 未到期 ⇒ 不关连接
}

// TestCallReadOnlyStillCappedByDefaultBound 只读类 RPC：即使调用方 deadline 更长，缺省
// 上界仍是上限（守卫：分层不能把只读档也一起放宽）。
func TestCallReadOnlyStillCappedByDefaultBound(t *testing.T) {
	shortDefaultRPCTimeout(t, 150*time.Millisecond)
	c, peer := newPipeConn()
	defer peer.Close()
	release := make(chan struct{})
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	err := c.call(ctx, func(*libvirt.Libvirt) error { <-release; return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("只读类超缺省上界应包装 DeadlineExceeded，实际: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("只读类应在缺省上界（150ms）附近返回，实耗 %s", elapsed)
	}
	assertPipeClosed(t, peer)
}

// TestCallJobHonorsCallerDeadlineAndInterrupts 作业类仍保留「执行中到期即关连接中断挂起
// RPC」的兜底：调用方 deadline 到期（此处 200ms，短于缺省）时按它中断。
func TestCallJobHonorsCallerDeadlineAndInterrupts(t *testing.T) {
	c, peer := newPipeConn()
	defer peer.Close()
	release := make(chan struct{})
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.callJob(ctx, func(*libvirt.Libvirt) error { <-release; return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("作业类调用方 deadline 到期应包装 DeadlineExceeded，实际: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("应在调用方 deadline（200ms）附近返回，实耗 %s", elapsed)
	}
	assertPipeClosed(t, peer)
}

// TestJobTierWiring 接线守护（源码扫描）：分层语义由上面的纯函数/行为用例覆盖，本用例补
// 「哪些方法走哪一档」的接线——避免日后有人把作业类方法改回 call（或把只读类改成 callJob）
// 而无察觉。判据：作业类经 callJob / withDomainMode(…, true …) / withSnapshotMode(…, true …)；
// 只读类不得出现 callJob。
func TestJobTierWiring(t *testing.T) {
	src, err := os.ReadFile("conn_libvirt.go")
	if err != nil {
		t.Fatalf("读取 conn_libvirt.go: %v", err)
	}
	text := string(src)
	body := func(name string) string {
		marker := "func (c *Conn) " + name + "("
		i := strings.Index(text, marker)
		if i < 0 {
			t.Fatalf("未找到方法 %s", name)
		}
		rest := text[i+len(marker):]
		if j := strings.Index(rest, "\nfunc "); j >= 0 {
			return rest[:j]
		}
		return rest
	}

	// 作业档：快照创建/回滚、大 xml 定义、DomainCreate 等待。
	if b := body("Define"); !strings.Contains(b, "callJob(") {
		t.Fatal("Define（大 xml 定义）应走作业档 callJob")
	}
	if b := body("SnapshotCreate"); !strings.Contains(b, "callJob(") {
		t.Fatal("SnapshotCreate（快照创建）应走作业档 callJob")
	}
	if b := body("Start"); !strings.Contains(b, "withDomainMode(ctx, true") {
		t.Fatal("Start（DomainCreate 等待）应走作业档 withDomainMode(…, true …)")
	}
	for _, m := range []string{"SnapshotRevert", "SnapshotDelete"} {
		if b := body(m); !strings.Contains(b, "withSnapshotMode(ctx, true") {
			t.Fatalf("%s（快照作业）应走作业档 withSnapshotMode(…, true …)", m)
		}
	}

	// 只读/元数据类不得误用作业档。
	for _, m := range []string{"State", "StateReason", "DumpXML", "SnapshotList", "Shutdown", "Destroy", "Reboot"} {
		if b := body(m); strings.Contains(b, "callJob(") {
			t.Fatalf("%s 是只读/元数据类，不得走作业档 callJob", m)
		}
	}
}
