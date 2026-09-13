package cli

// W2：命令历史与空闲超时（FR-CLI-006）。
// History 管理命令历史（相邻去重）与 ↑/↓ 导航、Ctrl-R 反查；
// IdleGuard 判定会话空闲超时（system idle-timeout-minutes，默认 10 分钟）。

import (
	"strings"
	"sync"
	"time"
)

// History 命令历史。
type History struct {
	entries  []string
	pos      int    // 浏览位置（-1 = 不在浏览态）
	draft    string // 浏览前未提交的输入
	lastGet  string // Ctrl-R 当前命中
	searchOn bool
}

// NewHistory 构造历史。
func NewHistory() *History { return &History{pos: -1} }

// Add 追加一条命令（与上一条相同则去重）。
func (h *History) Add(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	if len(h.entries) > 0 && h.entries[len(h.entries)-1] == line {
		return // 相邻去重
	}
	h.entries = append(h.entries, line)
	h.pos = -1
	h.searchOn = false
}

// Len 历史条数。
func (h *History) Len() int { return len(h.entries) }

// Up ↑：更旧一条；已在最旧则停留。返回 (显示文本, 是否有变化)。
func (h *History) Up(current string) (string, bool) {
	if len(h.entries) == 0 {
		return current, false
	}
	if !h.inBrowse() {
		h.draft = current
		h.pos = len(h.entries) - 1
		return h.entries[h.pos], true
	}
	if h.pos > 0 {
		h.pos--
		return h.entries[h.pos], true
	}
	return h.entries[h.pos], false
}

// Down ↓：更新一条；越过最新则回到草稿。
func (h *History) Down() (string, bool) {
	if !h.inBrowse() {
		return "", false
	}
	if h.pos < len(h.entries)-1 {
		h.pos++
		return h.entries[h.pos], true
	}
	h.pos = -1
	return h.draft, true
}

// SearchStart Ctrl-R：进入反查态（首次 SearchStep 从最新开始）。
func (h *History) SearchStart(current string) {
	h.searchOn = true
	h.draft = current
	h.pos = len(h.entries) // SearchStep 从 pos-1 开始，即最新一条
	h.lastGet = ""
}

// SearchStep 反查：跳到上一条匹配 term 的历史；无更多命中则停留。
func (h *History) SearchStep(term string) (string, bool) {
	if !h.searchOn {
		h.SearchStart(term)
	}
	for i := h.pos - 1; i >= 0; i-- {
		if strings.Contains(h.entries[i], term) {
			h.pos = i
			h.lastGet = h.entries[i]
			return h.entries[i], true
		}
	}
	return h.lastGet, false
}

// SearchHit Ctrl-R 当前命中。
func (h *History) SearchHit() string { return h.lastGet }

// SearchCancel 退出反查态（Enter/ESC）。
func (h *History) SearchCancel() { h.searchOn = false; h.pos = -1 }

// Searching 是否处于反查态。
func (h *History) Searching() bool { return h.searchOn }

func (h *History) inBrowse() bool { return h.pos >= 0 }

// IdleGuard 空闲超时判定（FR-CLI-006）。
// ReadLine 在独立 goroutine 中按键调用 Touch，主循环轮询 Expired，故加锁。
type IdleGuard struct {
	Timeout time.Duration
	mu      sync.Mutex
	last    time.Time
	now     func() time.Time
}

// NewIdleGuard 构造（timeout ≤0 取默认 10 分钟）。
func NewIdleGuard(timeout time.Duration, now func() time.Time) *IdleGuard {
	if timeout <= 0 {
		timeout = DefaultIdleTimeout
	}
	if now == nil {
		now = time.Now
	}
	return &IdleGuard{Timeout: timeout, last: now(), now: now}
}

// DefaultIdleTimeout 默认空闲超时（FR-SEC-005/FR-CLI-006：10 分钟）。
const DefaultIdleTimeout = 10 * time.Minute

// Touch 记录一次活动。
func (g *IdleGuard) Touch() {
	g.mu.Lock()
	g.last = g.now()
	g.mu.Unlock()
}

// Expired 判定空闲是否已超时。
func (g *IdleGuard) Expired() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.now().Sub(g.last) > g.Timeout
}
