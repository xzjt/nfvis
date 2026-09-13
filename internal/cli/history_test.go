package cli

import (
	"strings"
	"testing"
	"time"
)

// ---------- W2：历史导航 / Ctrl-R 反查 / 空闲超时（FR-CLI-006） ----------

func TestHistoryAddDedupAdjacent(t *testing.T) {
	h := NewHistory()
	h.Add("show version")
	h.Add("show version") // 相邻去重
	h.Add("configure")
	h.Add("show version") // 非相邻：保留
	if h.Len() != 3 {
		t.Fatalf("历史应 3 条（相邻去重），实际 %d: %v", h.Len(), h.entries)
	}
}

func TestHistoryUpDownNavigation(t *testing.T) {
	h := NewHistory()
	h.Add("cmd1")
	h.Add("cmd2")
	h.Add("cmd3")

	// ↑：从最新开始
	got, _ := h.Up("draft")
	if got != "cmd3" {
		t.Fatalf("第一次 ↑ 应到 cmd3: %q", got)
	}
	got, _ = h.Up("draft")
	if got != "cmd2" {
		t.Fatalf("第二次 ↑ 应到 cmd2: %q", got)
	}
	got, _ = h.Up("draft")
	got, _ = h.Up("draft")
	if got != "cmd1" {
		t.Fatalf("到最旧应停留 cmd1: %q", got)
	}
	// ↓：更新，越过最新回到草稿
	got, _ = h.Down()
	if got != "cmd2" {
		t.Fatalf("↓ 应到 cmd2: %q", got)
	}
	got, _ = h.Down()
	got, _ = h.Down()
	if got != "draft" {
		t.Fatalf("越过最新应回草稿: %q", got)
	}
}

func TestCtrlRSearch(t *testing.T) {
	h := NewHistory()
	h.Add("show version")
	h.Add("set system hostname a")
	h.Add("show interfaces")
	h.Add("set system hostname b")

	h.SearchStart("")
	hit, _ := h.SearchStep("hostname")
	if hit != "set system hostname b" {
		t.Fatalf("反查应命中最新 hostname: %q", hit)
	}
	hit, _ = h.SearchStep("hostname")
	if hit != "set system hostname a" {
		t.Fatalf("再次反查应命中前一条: %q", hit)
	}
	// 无更多命中：停留
	hit, _ = h.SearchStep("hostname")
	if hit != "set system hostname a" {
		t.Fatalf("无更多命中应停留: %q", hit)
	}
	// 退出反查后浏览态复位
	h.SearchCancel()
	if h.Searching() {
		t.Fatalf("取消后不应处于反查态")
	}
}

func TestIdleGuardTimeout(t *testing.T) {
	now := time.Unix(1000000, 0)
	clock := func() time.Time { return now }
	g := NewIdleGuard(10*time.Minute, clock)

	if g.Expired() {
		t.Fatalf("初始不应超时")
	}
	now = now.Add(9 * time.Minute)
	if g.Expired() {
		t.Fatalf("9 分钟不应超时")
	}
	g.Touch()
	now = now.Add(9 * time.Minute)
	if g.Expired() {
		t.Fatalf("Touch 后 9 分钟不应超时")
	}
	now = now.Add(2 * time.Minute)
	if !g.Expired() {
		t.Fatalf("Touch 后 11 分钟应超时")
	}
	// 默认超时 10 分钟
	g2 := NewIdleGuard(0, clock)
	if g2.Timeout != DefaultIdleTimeout {
		t.Fatalf("默认超时应为 10 分钟: %v", g2.Timeout)
	}
}

func TestHistoryInEditor(t *testing.T) {
	// raw 编辑器与 History 的集成：Add 后 Up 取回
	h := NewHistory()
	h.Add("show version")
	got, _ := h.Up("")
	if got != "show version" || !strings.HasPrefix(got, "show") {
		t.Fatalf("编辑器历史集成: %q", got)
	}
}
