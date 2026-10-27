package main

// 决策 #516：提交后宿主收敛队列的守护。
//
// 队列的同步以 channel 完成（started / release / applied），不靠 sleep 当同步手段；
// 文中的 time.After 只作「等待有界化」的兜底与「不应发生」的负向断言，不承载正向时序。

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 最新修订必须最后生效：快速入队 1..4，最后被应用的必须是 4，且应用序列严格递增
// （旧配置不得覆盖新配置）。
func TestHostApplyQueueLatestRevisionWins(t *testing.T) {
	applied := make(chan int, 64)
	q := newHostApplyQueue(func(rev int) { applied <- rev })
	defer q.close()

	for _, rev := range []int{1, 2, 3, 4} {
		q.enqueue(rev)
	}

	var seq []int
	deadline := time.After(5 * time.Second)
	for {
		select {
		case rev := <-applied:
			seq = append(seq, rev)
			if rev == 4 {
				goto done
			}
		case <-deadline:
			t.Fatalf("等待最新修订 4 生效超时，已应用序列 %v", seq)
		}
	}
done:
	if last := seq[len(seq)-1]; last != 4 {
		t.Fatalf("最后生效的修订应为 4（最新），实际序列 %v", seq)
	}
	for i := 1; i < len(seq); i++ {
		if seq[i] <= seq[i-1] {
			t.Fatalf("应用序列必须严格递增（旧配置不得最后写宿主），实际 %v", seq)
		}
	}
}

// 未启动的中间修订可合并：1 在跑时入队 2/3/4，只有最新的 4 会被接着执行，2/3 被合并丢弃。
func TestHostApplyQueueCoalescesPending(t *testing.T) {
	started := make(chan int, 16)
	applied := make(chan int, 16)
	var mu sync.Mutex
	gates := map[int]chan struct{}{}

	q := newHostApplyQueue(func(rev int) {
		g := make(chan struct{})
		mu.Lock()
		gates[rev] = g
		mu.Unlock()
		started <- rev
		<-g
		applied <- rev
	})

	release := func(rev int) {
		mu.Lock()
		g := gates[rev]
		mu.Unlock()
		close(g)
	}
	waitStarted := func(rev int) {
		select {
		case got := <-started:
			if got != rev {
				t.Fatalf("期望开始应用修订 %d，实际 %d", rev, got)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("等待修订 %d 开始应用超时", rev)
		}
	}

	q.enqueue(1)
	waitStarted(1) // 1 已在跑并被闸门挡住——此时入队的都是「未启动」的中间修订
	q.enqueue(2)
	q.enqueue(3)
	q.enqueue(4) // 覆盖 2、3
	release(1)
	waitStarted(4) // 1 结束后只跑最新的 4（2、3 被合并）
	release(4)

	var got []int
	for i := 0; i < 2; i++ {
		select {
		case rev := <-applied:
			got = append(got, rev)
		case <-time.After(5 * time.Second):
			t.Fatalf("等待应用完成超时，已收到 %v", got)
		}
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 4 {
		t.Fatalf("中间修订应被合并，期望应用序列 [1 4]，实际 %v", got)
	}
	// 被合并的 2/3 绝不应生效。
	select {
	case rev := <-applied:
		t.Fatalf("被合并的修订不应生效，却看到 %d", rev)
	case <-time.After(50 * time.Millisecond):
	}
	q.close()
}

// 串行：同一时刻只能有一个应用在跑（不得并发）。
func TestHostApplyQueueSerializesRunning(t *testing.T) {
	var inflight int32
	var maxInflight int32
	release := make(chan struct{})
	started := make(chan int, 16)

	q := newHostApplyQueue(func(rev int) {
		cur := atomic.AddInt32(&inflight, 1)
		for {
			m := atomic.LoadInt32(&maxInflight)
			if cur <= m || atomic.CompareAndSwapInt32(&maxInflight, m, cur) {
				break
			}
		}
		started <- rev
		<-release
		atomic.AddInt32(&inflight, -1)
	})
	defer q.close()

	q.enqueue(1)
	select {
	case rev := <-started:
		if rev != 1 {
			t.Fatalf("期望先跑修订 1，实际 %d", rev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待修订 1 开始超时")
	}
	if got := atomic.LoadInt32(&inflight); got != 1 {
		t.Fatalf("应有 1 个应用在跑，实际 %d", got)
	}
	// 入队 2：1 还在跑（被 release 挡住），2 必须被排队，不能并发。
	q.enqueue(2)
	select {
	case rev := <-started:
		t.Fatalf("修订 %d 与 1 并发执行了（应串行）", rev)
	case <-time.After(100 * time.Millisecond):
	}
	close(release) // 放行：1 结束，worker 再跑 2
	select {
	case rev := <-started:
		if rev != 2 {
			t.Fatalf("期望接着跑修订 2，实际 %d", rev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待修订 2 开始超时")
	}
	q.close()
	if got := atomic.LoadInt32(&maxInflight); got != 1 {
		t.Fatalf("并发执行的应用数最大应为 1（串行），实际 %d", got)
	}
}
