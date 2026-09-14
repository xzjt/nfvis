// Package events 进程内事件总线（M5-1，FR-API-006 / FR-OPS-020~022）。
//
// 语义：单进程广播，发布非阻塞（订阅者缓冲满则丢弃该条，绝不阻塞调用方——
// 引擎在锁内发布，底座巡检亦复用）。保留最近 histMax 条历史，供 SSE 断线重连
// 按 Last-Event-ID 补发。
package events

import (
	"sync"
	"time"
)

// 事件类型（契约 Event.type）。
const (
	TypeAlarmRaised         = "alarm-raised"
	TypeAlarmResolved       = "alarm-resolved"
	TypeVNFStateChanged     = "vnf-state-changed"
	TypeImageImportProgress = "image-import-progress"
	TypeConfigCommitted     = "config-committed"
)

// Event 一条事件（契约 Event，附加自增 id 供 SSE Last-Event-ID 使用）。
type Event struct {
	ID        uint64         `json:"id"`
	Type      string         `json:"type"`
	Timestamp time.Time      `json:"timestamp"`
	Payload   map[string]any `json:"payload,omitempty"`
}

const (
	subBuffer  = 64
	histMaxLen = 256
)

// Bus 进程内事件总线（并发安全）。
type Bus struct {
	mu      sync.Mutex
	seq     uint64
	nextSub uint64
	subs    map[uint64]chan Event
	history []Event
	now     func() time.Time
}

// New 创建总线。
func New() *Bus {
	return &Bus{subs: map[uint64]chan Event{}, now: time.Now}
}

// Publish 发布一条事件并返回之。订阅者缓冲满时丢弃（不阻塞发布方）。
func (b *Bus) Publish(typ string, payload map[string]any) Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	ev := Event{ID: b.seq, Type: typ, Timestamp: b.now().UTC(), Payload: payload}
	b.history = append(b.history, ev)
	if len(b.history) > histMaxLen {
		b.history = append([]Event(nil), b.history[len(b.history)-histMaxLen:]...)
	}
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default: // 慢订阅者：丢弃，避免阻塞引擎/巡检
		}
	}
	return ev
}

// Subscribe 订阅事件。返回只读通道与取消函数（取消后通道关闭）。
func (b *Bus) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	id := b.nextSub
	b.nextSub++
	ch := make(chan Event, subBuffer)
	b.subs[id] = ch
	b.mu.Unlock()
	cancel := func() {
		b.mu.Lock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
		b.mu.Unlock()
	}
	return ch, cancel
}

// Since 返回 id 之后的历史事件（SSE 重连补发；id=0 返回全部保留历史）。
func (b *Bus) Since(id uint64) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Event, 0, len(b.history))
	for _, ev := range b.history {
		if ev.ID > id {
			out = append(out, ev)
		}
	}
	return out
}

// Subscribers 当前订阅者数（测试/诊断用）。
func (b *Bus) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
