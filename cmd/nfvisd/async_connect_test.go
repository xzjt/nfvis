package main

// 后台接入循环（决策 #351）驱动 runAsyncConnect 的单元守护：
//   - 第一次尝试前先等一个 interval（注入小间隔验证时序）；
//   - 成功 ⇒ onConnected 恰好一次并终结循环；
//   - 持续失败 ⇒ 静默重试、永不 onConnected；
//   - ctx 取消 ⇒ 退出（含连接尝试进行中被取消）。
//
// 间隔用毫秒级注入值，避免慢测；失败静默（不落日志）由实现保证——契约明文。

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// counter 并发安全的计数器（循环在 goroutine 里跑，断言在主 goroutine 读）。
type counter struct {
	mu sync.Mutex
	v  int
}

func (c *counter) add(n int) {
	c.mu.Lock()
	c.v += n
	c.mu.Unlock()
}

func (c *counter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.v
}

func waitDone(t *testing.T, done <-chan struct{}, timeout time.Duration) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("runAsyncConnect 未在 %s 内返回", timeout)
	}
}

func TestRunAsyncConnectSuccessOnFirstAttempt(t *testing.T) {
	ctx := context.Background()
	var attempts, connected counter
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAsyncConnect(ctx, 2*time.Millisecond, discardLogger(),
			func(context.Context) error { attempts.add(1); return nil },
			func(ctx context.Context) {
				connected.add(1)
				if err := ctx.Err(); err != nil {
					t.Errorf("onConnected 收到的 ctx 应可用: %v", err)
				}
			})
	}()
	waitDone(t, done, 2*time.Second)
	if attempts.get() != 1 {
		t.Fatalf("首次尝试成功即应终结: attempts=%d", attempts.get())
	}
	if connected.get() != 1 {
		t.Fatalf("onConnected 应恰好一次: %d", connected.get())
	}
}

func TestRunAsyncConnectSuccessAfterRetries(t *testing.T) {
	ctx := context.Background()
	var attempts, connected counter
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAsyncConnect(ctx, time.Millisecond, discardLogger(),
			func(context.Context) error {
				n := attempts.get() + 1
				attempts.add(1)
				if n < 3 {
					return errors.New("底座未就绪")
				}
				return nil
			},
			func(context.Context) { connected.add(1) })
	}()
	waitDone(t, done, 2*time.Second)
	if attempts.get() != 3 {
		t.Fatalf("应尝试 3 次后成功: %d", attempts.get())
	}
	if connected.get() != 1 {
		t.Fatalf("onConnected 应恰好一次: %d", connected.get())
	}
}

func TestRunAsyncConnectSilentOnPersistentFailure(t *testing.T) {
	var attempts, connected counter
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAsyncConnect(ctx, 5*time.Millisecond, discardLogger(),
			func(context.Context) error { attempts.add(1); return errors.New("底座仍未就绪") },
			func(context.Context) { connected.add(1) })
	}()
	waitDone(t, done, 2*time.Second)
	if attempts.get() < 1 {
		t.Fatal("持续失败期间应有多次尝试")
	}
	if connected.get() != 0 {
		t.Fatalf("持续失败不应触发 onConnected: %d", connected.get())
	}
}

func TestRunAsyncConnectWaitsOneIntervalBeforeFirstAttempt(t *testing.T) {
	// 100ms 后取消，而首次尝试要等 300ms：应当一次都没试过。
	var attempts, connected counter
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAsyncConnect(ctx, 300*time.Millisecond, discardLogger(),
			func(context.Context) error { attempts.add(1); return nil },
			func(context.Context) { connected.add(1) })
	}()
	waitDone(t, done, 2*time.Second)
	if attempts.get() != 0 {
		t.Fatalf("第一次尝试前必须先等一个 interval: attempts=%d", attempts.get())
	}
	if connected.get() != 0 {
		t.Fatalf("取消退出不应触发 onConnected: %d", connected.get())
	}
}

func TestRunAsyncConnectStopsOnCancelDuringConnect(t *testing.T) {
	var connected counter
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAsyncConnect(ctx, time.Millisecond, discardLogger(),
			// 连接尝试进行中被取消：connect 按 ctx 约定返回，循环应退出且不 onConnected。
			func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
			func(context.Context) { connected.add(1) })
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	waitDone(t, done, 2*time.Second)
	if connected.get() != 0 {
		t.Fatalf("取消退出不应触发 onConnected: %d", connected.get())
	}
}
