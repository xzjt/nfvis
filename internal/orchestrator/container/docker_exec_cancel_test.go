package container

// 决策 #375（R142 B7）：exec 的**超时判定**此前用 `wctx.Err() != nil`——而 wctx 派生自调用方
// ctx，于是调用方取消（客户端断开/上层取消）也被谎报成「超时」。这里用可注入的假 Docker HTTP
// 后端把两种失败钉住：等待窗口到期 ⇒ TimedOut；调用方取消 ⇒ 返回错误且 **不** TimedOut。
//
// 红绿口径：把判据改回 `wctx.Err() != nil`，两个 cancel 用例逐项按预期失败。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// execStartBlockServer 假 Docker 后端：create 立即回 exec id；start 请求一直阻塞到请求上下文
// 结束（模拟「命令仍在跑 / dockerd 不返回」）。用于覆盖 start 请求失败分支。
//
// 阻塞用 select{ctx.Done, stop}：ctx.Done 是主路径；stop 由清理关闭，兜底保证 srv.Close 不被
// 卡在未返回的 handler 上（测试体本身早已拿到结果）。
func execStartBlockServer(t *testing.T) *httptest.Server {
	t.Helper()
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"abc"}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/exec/abc/start"):
			select {
			case <-r.Context().Done():
			case <-stop:
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() { close(stop); srv.Close() })
	return srv
}

// execStreamBlockServer 假 Docker 后端：create 回 id；start 回 200 并写一个**不完整帧**
// （声明 10 字节载荷、只给 3 字节）后挂住——用于覆盖 demux 流中断分支。
func execStreamBlockServer(t *testing.T) *httptest.Server {
	t.Helper()
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"abc"}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/exec/abc/start"):
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			// 帧头（stream=1, len=10）+ 仅 3 字节载荷 ⇒ demux 读载荷时阻塞。
			_, _ = w.Write([]byte{1, 0, 0, 0, 0, 0, 0, 10, 'a', 'b', 'c'})
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-r.Context().Done():
			case <-stop:
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() { close(stop); srv.Close() })
	return srv
}

// start 请求分支：等待窗口到期 ⇒ 如实 TimedOut（正向路径不能被收紧误伤）。
func TestExecWaitWindowExpiryTimesOut(t *testing.T) {
	c := &dockerClient{http: &http.Client{}, base: execStartBlockServer(t).URL}
	res, err := c.Exec(context.Background(), "ct", "sleep 60", 150*time.Millisecond)
	if err != nil {
		t.Fatalf("等待窗口到期应如实报超时（不报错），得 %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("等待窗口到期应 TimedOut=true: %+v", res)
	}
}

// start 请求分支：调用方取消 ⇒ 返回错误且**不** TimedOut（旧实现谎报 TimedOut）。
func TestExecCallerCancelIsErrorNotTimeout(t *testing.T) {
	c := &dockerClient{http: &http.Client{}, base: execStartBlockServer(t).URL}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	res, err := c.Exec(ctx, "ct", "sleep 60", 30*time.Second)
	if err == nil {
		t.Fatalf("调用方取消应返回错误（不谎报超时）: %+v", res)
	}
	if res.TimedOut {
		t.Fatalf("调用方取消不得报 TimedOut: %+v", res)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消错误应可判为 context.Canceled: %v", err)
	}
}

// demux 流中断分支：等待窗口到期 ⇒ TimedOut。
func TestExecStreamInterruptWindowExpiryTimesOut(t *testing.T) {
	c := &dockerClient{http: &http.Client{}, base: execStreamBlockServer(t).URL}
	res, err := c.Exec(context.Background(), "ct", "sleep 60", 150*time.Millisecond)
	if err != nil {
		t.Fatalf("窗口到期应如实报超时: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("窗口到期应 TimedOut=true: %+v", res)
	}
}

// demux 流中断分支：调用方取消 ⇒ 返回错误（已读部分保留）、**不** TimedOut。
func TestExecStreamInterruptCallerCancelIsError(t *testing.T) {
	c := &dockerClient{http: &http.Client{}, base: execStreamBlockServer(t).URL}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	res, err := c.Exec(ctx, "ct", "sleep 60", 30*time.Second)
	if err == nil {
		t.Fatalf("流中断 + 调用方取消应返回错误: %+v", res)
	}
	if res.TimedOut {
		t.Fatalf("调用方取消不得报 TimedOut: %+v", res)
	}
}
