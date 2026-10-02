package main

// 决策 #350：启动装配期 libvirt 连接的有界重试（boundedRetry 纯函数）的
// 次数/退避/回调语义守护。重试是装配层策略：connect 的单次有界语义由调用方
// 自行保证（这里用桩函数，本函数只管次数与节奏），日志经 onRetry 回调交调用方。

import (
	"errors"
	"testing"
	"time"
)

type retryRecorder struct {
	calls    int
	attempts []int   // onRetry 收到的 attempt 序列
	errs     []error // onRetry 收到的错误序列
}

func TestBoundedRetryFirstAttemptSucceeds(t *testing.T) {
	r := &retryRecorder{}
	err := boundedRetry(3, time.Second, r.onRetry, func() error {
		r.calls++
		return nil
	})
	if err != nil {
		t.Fatalf("首次成功应返回 nil：%v", err)
	}
	if r.calls != 1 {
		t.Fatalf("connect 应恰好被调 1 次，实际 %d", r.calls)
	}
	if len(r.attempts) != 0 {
		t.Fatalf("首次成功不应有 onRetry 回调：%v", r.attempts)
	}
}

func TestBoundedRetrySucceedsOnSecondAttempt(t *testing.T) {
	r := &retryRecorder{}
	secondErr := errors.New("首次连接未就绪")
	err := boundedRetry(3, 0, r.onRetry, func() error {
		r.calls++
		if r.calls < 2 {
			return secondErr
		}
		return nil
	})
	if err != nil {
		t.Fatalf("第 2 次成功应返回 nil：%v", err)
	}
	if r.calls != 2 {
		t.Fatalf("connect 应被调 2 次，实际 %d", r.calls)
	}
	if len(r.attempts) != 1 || r.attempts[0] != 1 {
		t.Fatalf("onRetry 应恰被调 1 次且 attempt=1，实际 %v", r.attempts)
	}
	if len(r.errs) != 1 || !errors.Is(r.errs[0], secondErr) {
		t.Fatalf("onRetry 应收到首次失败原因，实际 %v", r.errs)
	}
}

func TestBoundedRetryAllFailReturnsLastError(t *testing.T) {
	r := &retryRecorder{}
	lastErr := errors.New("末次失败")
	err := boundedRetry(3, 0, r.onRetry, func() error {
		r.calls++
		if r.calls == 3 {
			return lastErr
		}
		return errors.New("前两次失败")
	})
	if !errors.Is(err, lastErr) {
		t.Fatalf("应返回最后一次的错误，实际 %v", err)
	}
	if r.calls != 3 {
		t.Fatalf("connect 应被调 attempts(3) 次，实际 %d", r.calls)
	}
	if len(r.attempts) != 3 {
		t.Fatalf("onRetry 应被调 3 次（含最后一次），实际 %v", r.attempts)
	}
	for i, want := range []int{1, 2, 3} {
		if r.attempts[i] != want {
			t.Fatalf("onRetry 序号应从 1 连续递增，实际 %v", r.attempts)
		}
	}
}

func TestBoundedRetryNonPositiveAttemptsTreatedAsOne(t *testing.T) {
	for _, attempts := range []int{0, -1} {
		r := &retryRecorder{}
		err := boundedRetry(attempts, 0, r.onRetry, func() error {
			r.calls++
			return errors.New("恒失败")
		})
		if err == nil {
			t.Fatalf("attempts=%d 全败应返回错误", attempts)
		}
		if r.calls != 1 {
			t.Fatalf("attempts=%d 应按 1 次处理，实际 connect 被调 %d 次", attempts, r.calls)
		}
	}
}

func TestBoundedRetryNilConnectDefended(t *testing.T) {
	if err := boundedRetry(3, 0, nil, nil); err == nil {
		t.Fatal("connect 为 nil 应报错（防御）")
	}
}

func TestBoundedRetryBackoffElapsed(t *testing.T) {
	// 只断言耗时下界（系统调度可能让实际值更长），避免 flaky。
	const backoff = 20 * time.Millisecond
	r := &retryRecorder{}
	start := time.Now()
	err := boundedRetry(2, backoff, r.onRetry, func() error {
		r.calls++
		return errors.New("失败")
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("全败应返回错误")
	}
	if r.calls != 2 {
		t.Fatalf("connect 应被调 2 次，实际 %d", r.calls)
	}
	if elapsed < backoff {
		t.Fatalf("非首次尝试前应等待 backoff（≥%s），实际 %s", backoff, elapsed)
	}
}

func (r *retryRecorder) onRetry(attempt int, err error) {
	r.attempts = append(r.attempts, attempt)
	r.errs = append(r.errs, err)
}
