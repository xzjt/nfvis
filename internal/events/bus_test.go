package events

import (
	"sync"
	"testing"
	"time"
)

func TestBusPublishSubscribe(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe()
	defer cancel()
	if b.Subscribers() != 1 {
		t.Fatalf("订阅者应为 1，实际 %d", b.Subscribers())
	}
	ev := b.Publish(TypeAlarmRaised, map[string]any{"code": "VNF_CRASHED"})
	if ev.ID != 1 || ev.Type != TypeAlarmRaised {
		t.Fatalf("发布返回: %+v", ev)
	}
	select {
	case got := <-ch:
		if got.ID != ev.ID || got.Payload["code"] != "VNF_CRASHED" {
			t.Fatalf("订阅收到: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("超时未收到事件")
	}
}

func TestBusCancelClosesChannel(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe()
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("取消后通道应关闭")
	}
	if b.Subscribers() != 0 {
		t.Fatalf("取消后订阅者应为 0，实际 %d", b.Subscribers())
	}
	// 取消后再发布不应 panic（不得向已关闭通道发送）
	b.Publish(TypeConfigCommitted, nil)
}

// 慢订阅者不得阻塞发布方（缓冲满即丢弃）。
func TestBusSlowSubscriberDoesNotBlock(t *testing.T) {
	b := New()
	_, cancel := b.Subscribe()
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < subBuffer*4; i++ {
			b.Publish(TypeVNFStateChanged, nil)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("发布被慢订阅者阻塞")
	}
}

func TestBusSinceReplayAndHistoryTrim(t *testing.T) {
	b := New()
	for i := 0; i < 5; i++ {
		b.Publish(TypeVNFStateChanged, map[string]any{"i": i})
	}
	got := b.Since(2)
	if len(got) != 3 || got[0].ID != 3 {
		t.Fatalf("Since(2) = %+v", got)
	}
	if all := b.Since(0); len(all) != 5 {
		t.Fatalf("Since(0) 应返回 5 条，实际 %d", len(all))
	}
	// 历史上限：超出后只保留最近 histMaxLen 条
	for i := 0; i < histMaxLen+10; i++ {
		b.Publish(TypeVNFStateChanged, nil)
	}
	if all := b.Since(0); len(all) != histMaxLen {
		t.Fatalf("历史上限应为 %d，实际 %d", histMaxLen, len(all))
	}
}

func TestBusConcurrentPublishSubscribe(t *testing.T) {
	b := New()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, cancel := b.Subscribe()
			defer cancel()
			go func() {
				for range ch {
				}
			}()
			for j := 0; j < 50; j++ {
				b.Publish(TypeConfigCommitted, nil)
			}
		}()
	}
	wg.Wait()
	if b.Since(0)[0].ID == 0 {
		t.Fatal("应产生事件")
	}
}
