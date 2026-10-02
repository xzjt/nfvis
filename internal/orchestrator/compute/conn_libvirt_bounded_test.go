package compute

// Connect 有界化的单测守护（决策 #349）。
//
// 假死现场 = 「socket 可连、协议握手零响应」：真 unix socket 在 Windows 开发机上
// 不可用，而 connectBounded 按 network/addr 参数化，故用 TCP 假服务（accept 后
// 不应答）复现。成功路径（真实协议握手）由真机集成覆盖（conn_libvirt.go 本就在
// check_coverage.sh 的 COVER_EXCLUDE 清单），这里不伪造 libvirt 协议应答。

import (
	"context"
	"errors"
	"net"
	"net/url"
	"runtime"
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
	lv, err := connectBounded(ctx, "tcp", addr, libvirt.ConnectURI("qemu:///system"))
	elapsed := time.Since(start)
	if err == nil {
		if lv != nil {
			_ = lv.Disconnect()
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
	if _, err := connectBounded(ctx, "tcp", addr, libvirt.ConnectURI("qemu:///system")); err == nil {
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
	if _, err := connectBounded(ctx, "tcp", addr, libvirt.ConnectURI("qemu:///system")); err == nil {
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
