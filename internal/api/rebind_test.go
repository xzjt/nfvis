package api

// 决策 #471：管理面监听地址的运行期重收敛（收口 round24 缺陷 R24-E）。
//
// 背景：管理口首次声明（live commit）后 nfvisd 仍监听 *:443——system.ResolveListenAddr
// 只在启动装配期调用。「管理面仅监听管理网卡」此前延迟到下次启动。本文件钉住 Server.Rebind
// 的对外行为：换绑后新地址在服、旧地址拒连、ListenAndServe 不因换绑返回；新地址绑不上时
// 回退原地址继续服务；同地址幂等；Shutdown 语义不变。

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// newRebindServer 构造监听 127.0.0.1:0 的明文 HTTP server（测试不涉 TLS）。
func newRebindServer(t *testing.T) *Server {
	t.Helper()
	store, err := config.OpenStore(filepath.Join(t.TempDir(), "nfvis-rebind.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	eng, err := config.NewEngine(store, orchestrator.NewNoopApplier(), config.Options{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(eng.Close)
	return New(eng, aaa.NewService(eng, nil), Options{Addr: "127.0.0.1:0"})
}

// startRebindServer 后台启动 ListenAndServe，等初始监听就绪；清理时优雅停机并断言已返回。
// 返回（错误通道, 已返回通道）。
func startRebindServer(t *testing.T, srv *Server) (<-chan error, <-chan struct{}) {
	t.Helper()
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		errCh <- srv.ListenAndServe()
		close(done)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("Shutdown 后 ListenAndServe 未返回")
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for srv.BoundAddr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("BoundAddr 恒空：初始监听未记录实际绑定地址")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errCh, done
}

// serveStatus 对 addr 发一次匿名 GET（openapi.json 免鉴权），返回状态码。
func serveStatus(addr string) (int, error) {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + addr + APIPrefix + "/openapi.json")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// assertStillRunning 断言 ListenAndServe 未返回（换绑不得被视为致命退出）。
func assertStillRunning(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("ListenAndServe 因换绑返回（应继续服务）")
	case <-time.After(150 * time.Millisecond):
	}
}

// 换绑：新地址在服、旧地址拒连、监听循环不返回。
func TestRebindMovesListenerAndKeepsServing(t *testing.T) {
	srv := newRebindServer(t)
	_, done := startRebindServer(t, srv)

	old := srv.BoundAddr()
	if got, err := serveStatus(old); err != nil || got != http.StatusOK {
		t.Fatalf("初始监听应可服务: status=%d err=%v", got, err)
	}

	if err := srv.Rebind("127.0.0.1:0"); err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	fresh := srv.BoundAddr()
	if fresh == "" || fresh == old {
		t.Fatalf("换绑后绑定串应变化: old=%s new=%s", old, fresh)
	}
	if got, err := serveStatus(fresh); err != nil || got != http.StatusOK {
		t.Fatalf("新地址应可服务: status=%d err=%v", got, err)
	}
	if c, err := net.DialTimeout("tcp", old, 500*time.Millisecond); err == nil {
		_ = c.Close()
		t.Fatalf("旧地址 %s 换绑后不应再接受连接", old)
	}
	assertStillRunning(t, done)
}

// 失败安全：新地址绑不上时回退原地址继续服务并返回错误。
func TestRebindFallsBackWhenNewAddrUnavailable(t *testing.T) {
	srv := newRebindServer(t)
	_, done := startRebindServer(t, srv)
	old := srv.BoundAddr()

	// 占住一个端口，令 Rebind 目标绑定失败
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听: %v", err)
	}
	defer blocker.Close() //nolint:errcheck
	blocked := blocker.Addr().String()

	if err := srv.Rebind(blocked); err == nil {
		t.Fatal("绑定被占地址应返回错误（失败要如实上报）")
	}
	if got := srv.BoundAddr(); got != old {
		t.Fatalf("失败应回退原地址: want %s got %s", old, got)
	}
	if got, err := serveStatus(old); err != nil || got != http.StatusOK {
		t.Fatalf("回退后原地址应仍在服务: status=%d err=%v", got, err)
	}
	assertStillRunning(t, done)
}

// 幂等与入参校验：同地址两次 Rebind 返回 nil 且服务不中断；不合法地址拒绝且不动现有监听。
func TestRebindIdempotentAndRejectsInvalid(t *testing.T) {
	srv := newRebindServer(t)
	_, done := startRebindServer(t, srv)
	addr := srv.BoundAddr()

	for i := 1; i <= 2; i++ {
		if err := srv.Rebind(addr); err != nil {
			t.Fatalf("同地址 Rebind 第 %d 次应幂等成功: %v", i, err)
		}
	}
	if got := srv.BoundAddr(); got != addr {
		t.Fatalf("幂等不应改变绑定: want %s got %s", addr, got)
	}
	for _, bad := range []string{"", "没有端口", "127.0.0.1:99999x"} {
		if err := srv.Rebind(bad); err == nil {
			t.Fatalf("不合法地址 %q 应被拒绝", bad)
		}
	}
	if got, err := serveStatus(addr); err != nil || got != http.StatusOK {
		t.Fatalf("幂等/拒绝后应仍在服务: status=%d err=%v", got, err)
	}
	assertStillRunning(t, done)
}

// Shutdown 语义不回归：换绑后优雅停机仍让 ListenAndServe 以 ErrServerClosed 返回。
func TestShutdownReturnsAfterRebind(t *testing.T) {
	srv := newRebindServer(t)
	errCh, done := startRebindServer(t, srv)

	if err := srv.Rebind("127.0.0.1:0"); err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown 后 ListenAndServe 未返回")
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("期望 ErrServerClosed，得到 %v", err)
	}
}
