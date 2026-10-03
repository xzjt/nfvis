package main

// 底座常驻连接状态机（决策 #354；驱动 runBaseWatch）与 #351 一次性接入窄包装
// （runAsyncConnect，Docker 侧）的单元守护：
//
//   - #351 回归（Docker 窄包装）：第一次尝试前先等一个 interval、成功 ⇒ onConnected
//     恰好一次并终结循环、持续失败静默重试、ctx 取消 ⇒ 退出；
//   - #354 未接入态：失败静默（无日志）、成功 ⇒ 首接回调恰一次且**转入探活态**；
//   - #354 已接入态：单次探活失败不动作；成功清零失败计数（失败→成功→再失败不触发）；
//     连续 2 次失败 ⇒ 中断告警恰一次 + 复连段被调（有界 3 次）；
//   - #354 复连段：成功 ⇒ 消警恰一次 + EnsureConsistent 恰一次 + INFO「已恢复」；
//     3 次失败 ⇒ 告警保持（不重复 raise）且退回未接入态，下一节奏再试并走「恢复」口径；
//   - 探活上界（probeTimeout）与停机取消。
//
// 间隔全部毫秒级注入，断言用轮询等待（给足裕量），不依赖真实时钟节奏。

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
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
		t.Fatalf("驱动未在 %s 内返回", timeout)
	}
}

// waitFor 轮询等待条件成立（毫秒级节奏下给足裕量），超时即失败。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// watchAlarmSink 记录 Raise/Resolve 的告警落点假件（收尾回调与中断告警测试用）。
type watchAlarmSink struct {
	mu       sync.Mutex
	raised   []degradeAlarm
	resolved []string
}

func (s *watchAlarmSink) Raise(scope, severity, code, message, source string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.raised = append(s.raised, degradeAlarm{scope, severity, code, message, source})
}

func (s *watchAlarmSink) Resolve(scope, code, source string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolved = append(s.resolved, scope+"|"+code+"|"+source)
	return true
}

func (s *watchAlarmSink) raisedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.raised)
}

func (s *watchAlarmSink) resolveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.resolved)
}

// fastWatchOptions 毫秒级注入参数（单测用；字段显式给出，避免回落慢默认）。
func fastWatchOptions() watchOptions {
	return watchOptions{
		retryInterval:  time.Millisecond,
		attemptTimeout: 50 * time.Millisecond,
		attempts:       3,
		backoff:        time.Millisecond,
		probeInterval:  time.Millisecond,
		probeTimeout:   50 * time.Millisecond,
		failThreshold:  2,
		ensureTimeout:  100 * time.Millisecond,
	}
}

// ---- #351 回归：runAsyncConnect 的 Docker 一次性窄包装（签名与语义不变） ----

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

// ---- #354 未接入态 ----

// TestRunBaseWatchDisconnectedRetriesSilentThenProbeStarts：未接入态失败静默（不落日志）、
// 成功后首接回调恰一次且转入探活态（不再终结循环）。
func TestRunBaseWatchDisconnectedRetriesSilentThenProbeStarts(t *testing.T) {
	var connects, probes, first, reconnected, lost counter
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runBaseWatch(ctx, false, fastWatchOptions(), baseWatchDeps{
			log: logger,
			connect: func(context.Context) error {
				n := connects.get() + 1
				connects.add(1)
				if n < 3 {
					return errors.New("底座未就绪")
				}
				return nil
			},
			probe:            func(context.Context) error { probes.add(1); return nil },
			onFirstConnected: func(context.Context) { first.add(1) },
			onReconnected:    func(context.Context) { reconnected.add(1) },
			onLost:           func(error) { lost.add(1) },
		})
	}()
	waitFor(t, "首接成功", func() bool { return first.get() == 1 })
	waitFor(t, "转入探活态", func() bool { return probes.get() >= 2 })
	if connects.get() != 3 {
		t.Fatalf("2 次失败后第 3 次成功: connects=%d", connects.get())
	}
	if reconnected.get() != 0 || lost.get() != 0 {
		t.Fatalf("未接入路径不应触发复连/中断告警: reconnected=%d lost=%d", reconnected.get(), lost.get())
	}
	cancel()
	waitDone(t, done, 2*time.Second)
	if buf.Len() != 0 {
		t.Fatalf("未接入态重试应静默（不逐次落日志）: %s", buf.String())
	}
}

// ---- #354 已接入态：探活阈值 ----

// TestRunBaseWatchSingleProbeFailureDoesNotAct：单次探活失败不动作、不告警、不复连。
func TestRunBaseWatchSingleProbeFailureDoesNotAct(t *testing.T) {
	var connects, probes, lost counter
	opts := fastWatchOptions()
	opts.retryInterval = time.Hour // 复连（若发生）后不再进下一轮接入，便于断言
	opts.probeTimeout = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runBaseWatch(ctx, true, opts, baseWatchDeps{
			log:     discardLogger(),
			connect: func(context.Context) error { connects.add(1); return nil },
			probe: func(context.Context) error {
				n := probes.get() + 1
				probes.add(1)
				if n%2 == 1 {
					return errors.New("单次抖动")
				}
				return nil
			},
			onFirstConnected: func(context.Context) {},
			onReconnected:    func(context.Context) {},
			onLost:           func(error) { lost.add(1) },
		})
	}()
	// 失败→成功→失败→成功…永不连续两次失败。
	waitFor(t, "多次探活", func() bool { return probes.get() >= 8 })
	if lost.get() != 0 {
		t.Fatalf("单次探活失败（此后探活成功清零）不应判中断: lost=%d", lost.get())
	}
	if connects.get() != 0 {
		t.Fatalf("不应触发复连: connects=%d", connects.get())
	}
	cancel()
	waitDone(t, done, 2*time.Second)
}

// TestRunBaseWatchConsecutiveProbeFailuresTriggerBoundedReconnect：连续 2 次失败 ⇒
// 中断告警恰一次 + 复连段有界尝试（3 次）被调；复连失败 ⇒ 告警保持且退回未接入态
// （下一节奏由 retryInterval 控制，本用例给 1 小时以精确计数）。
func TestRunBaseWatchConsecutiveProbeFailuresTriggerBoundedReconnect(t *testing.T) {
	var connects, probes, lost counter
	opts := fastWatchOptions()
	opts.retryInterval = time.Hour
	opts.probeTimeout = time.Millisecond
	opts.attemptTimeout = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runBaseWatch(ctx, true, opts, baseWatchDeps{
			log:              discardLogger(),
			connect:          func(context.Context) error { connects.add(1); return errors.New("复连不上") },
			probe:            func(context.Context) error { probes.add(1); return errors.New("探活失败") },
			onFirstConnected: func(context.Context) {},
			onReconnected:    func(context.Context) {},
			onLost:           func(error) { lost.add(1) },
		})
	}()
	waitFor(t, "复连段 3 次有界尝试", func() bool { return connects.get() >= 3 })
	time.Sleep(20 * time.Millisecond) // 若还会继续重试，此窗口内应可见
	if connects.get() != 3 {
		t.Fatalf("复连段应有界（至多 3 次），退回未接入态后等下一节奏: connects=%d", connects.get())
	}
	if lost.get() != 1 {
		t.Fatalf("中断告警应恰一次（告警保持不重复 raise）: lost=%d", lost.get())
	}
	if probes.get() < 2 {
		t.Fatalf("判中断前应有连续 2 次探活失败: probes=%d", probes.get())
	}
	cancel()
	waitDone(t, done, 2*time.Second)
}

// ---- #354 复连段：成功/失败两种走向（用生产收尾回调验证消警与 Ensure） ----

// TestRunBaseWatchReconnectSuccessResolvesAndEnsuresOnce：复连成功 ⇒
// 消警恰一次（同键）+ EnsureConsistent 恰一次（且在 recoveryMu 内）+ INFO「已恢复」。
func TestRunBaseWatchReconnectSuccessResolvesAndEnsuresOnce(t *testing.T) {
	sink := &watchAlarmSink{}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	ensureCalls := &counter{}
	var mu sync.Mutex
	ensure := func(ctx context.Context, cfg model.Config) []error {
		if mu.TryLock() {
			mu.Unlock()
			t.Errorf("EnsureConsistent 应在 recoveryMu 内执行")
		}
		ensureCalls.add(1)
		return nil
	}
	committed := func() (model.Config, error) { return model.Config{}, nil }

	var probes counter
	opts := fastWatchOptions()
	opts.retryInterval = time.Hour
	opts.probeTimeout = time.Millisecond
	opts.attemptTimeout = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runBaseWatch(ctx, true, opts, baseWatchDeps{
			log:     logger,
			connect: func(context.Context) error { return nil }, // 复连一次成功
			probe: func(context.Context) error {
				n := probes.get() + 1
				probes.add(1)
				if n <= 2 {
					return errors.New("连接已断")
				}
				return nil
			},
			onFirstConnected: computeConnectedCallback(false, logger, sink, "qemu:///system", &mu, committed, ensure),
			onReconnected:    computeConnectedCallback(true, logger, sink, "qemu:///system", &mu, committed, ensure),
			onLost:           func(err error) { computeLostAlarm(sink, "qemu:///system", err) },
		})
	}()
	waitFor(t, "复连成功并消警", func() bool { return sink.resolveCount() == 1 })
	cancel()
	waitDone(t, done, 2*time.Second)

	if sink.raisedCount() != 1 {
		t.Fatalf("中断应 raise 恰一次: %d", sink.raisedCount())
	}
	if ensureCalls.get() != 1 {
		t.Fatalf("复连成功应补跑 EnsureConsistent 恰一次: %d", ensureCalls.get())
	}
	if !strings.Contains(buf.String(), "libvirt 连接已恢复（自动复连成功）") {
		t.Fatalf("复连成功应有 INFO「已恢复」: %s", buf.String())
	}
	if strings.Contains(buf.String(), "计算编排已接入（后台）") {
		t.Fatalf("已接入启动不应走首接文案: %s", buf.String())
	}
}

// TestRunBaseWatchReconnectFailureThenRetryOnDisconnectedRhythm：复连段 3 次失败 ⇒
// 告警保持（不重复 raise）、退回未接入态；下一节奏接入成功走「恢复」口径（消警 + 复连回调）。
func TestRunBaseWatchReconnectFailureThenRetryOnDisconnectedRhythm(t *testing.T) {
	sink := &watchAlarmSink{}
	var connects, probes, first, reconnected, lost counter
	opts := fastWatchOptions()
	opts.retryInterval = 2 * time.Millisecond
	opts.probeTimeout = time.Millisecond
	opts.attemptTimeout = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runBaseWatch(ctx, true, opts, baseWatchDeps{
			log: discardLogger(),
			connect: func(context.Context) error {
				n := connects.get() + 1
				connects.add(1)
				if n <= 3 {
					return errors.New("复连不上")
				}
				return nil // 第 4 次（退回未接入态后的下一节奏）成功
			},
			probe: func(context.Context) error {
				n := probes.get() + 1
				probes.add(1)
				if n <= 2 {
					return errors.New("连接已断")
				}
				return nil
			},
			onFirstConnected: func(context.Context) { first.add(1) },
			onReconnected: func(context.Context) {
				reconnected.add(1)
				sink.Resolve("compute", alarmCodeComputeUnavailable, "libvirt")
			},
			onLost: func(err error) { lost.add(1); computeLostAlarm(sink, "qemu:///system", err) },
		})
	}()
	waitFor(t, "退回未接入态后接入成功（恢复口径）", func() bool { return reconnected.get() == 1 })
	if lost.get() != 1 {
		t.Fatalf("告警应保持（不重复 raise）: lost=%d", lost.get())
	}
	if first.get() != 0 {
		t.Fatalf("曾接入过，再次接入应走「恢复」而非首接: first=%d", first.get())
	}
	if connects.get() < 4 {
		t.Fatalf("复连 3 次失败后应退回未接入态并在下一节奏重试: connects=%d", connects.get())
	}
	if sink.raisedCount() != 1 || sink.resolveCount() != 1 {
		t.Fatalf("raise/resolve 应各恰一次: raised=%d resolved=%d", sink.raisedCount(), sink.resolveCount())
	}
	cancel()
	waitDone(t, done, 2*time.Second)
}

// ---- 探活上界与停机取消 ----

// TestRunBaseWatchProbeTimeoutCountsAsFailure：探活调用被 probeTimeout 上界截断时
// 计为本次失败（注入的假件按 ctx 约定返回），连续 2 次仍判中断。
func TestRunBaseWatchProbeTimeoutCountsAsFailure(t *testing.T) {
	var probes, lost counter
	opts := fastWatchOptions()
	opts.retryInterval = time.Hour
	opts.probeTimeout = 3 * time.Millisecond
	opts.attemptTimeout = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runBaseWatch(ctx, true, opts, baseWatchDeps{
			log:     discardLogger(),
			connect: func(context.Context) error { return errors.New("复连不上") },
			probe: func(ctx context.Context) error {
				probes.add(1)
				<-ctx.Done() // 模拟一次不返回的 RPC：由上界截断
				return ctx.Err()
			},
			onFirstConnected: func(context.Context) {},
			onReconnected:    func(context.Context) {},
			onLost:           func(error) { lost.add(1) },
		})
	}()
	waitFor(t, "上界截断计为失败并判中断", func() bool { return lost.get() == 1 })
	if probes.get() < 2 {
		t.Fatalf("应有连续 2 次探活失败: probes=%d", probes.get())
	}
	cancel()
	waitDone(t, done, 2*time.Second)
}

// TestRunBaseWatchStopsOnCancelWhileConnected：已接入态 ctx 取消即退出（不告警、不复连）。
func TestRunBaseWatchStopsOnCancelWhileConnected(t *testing.T) {
	var probes, lost, connects counter
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runBaseWatch(ctx, true, fastWatchOptions(), baseWatchDeps{
			log:              discardLogger(),
			connect:          func(context.Context) error { connects.add(1); return nil },
			probe:            func(context.Context) error { probes.add(1); return nil },
			onFirstConnected: func(context.Context) {},
			onReconnected:    func(context.Context) {},
			onLost:           func(error) { lost.add(1) },
		})
	}()
	waitFor(t, "探活开始", func() bool { return probes.get() >= 2 })
	cancel()
	waitDone(t, done, 2*time.Second)
	if lost.get() != 0 || connects.get() != 0 {
		t.Fatalf("取消不应触发告警/复连: lost=%d connects=%d", lost.get(), connects.get())
	}
}
