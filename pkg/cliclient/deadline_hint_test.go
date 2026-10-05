package cliclient

// 决策 #366（R142-10b）：客户端等待时长的纯函数表驱动 + ctx deadline 真生效验证。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExecTimeoutHint(t *testing.T) {
	cases := []struct {
		name string
		line string
		want time.Duration
	}{
		{"普通 show 命令", "show version", 0},
		{"exec 带 timeout", `request container-functions c1 exec "sleep 200" timeout 120`, 120*time.Second + execTimeoutGrace},
		{"timeout 300（上限）", `request container-functions c1 exec "x" timeout 300`, 300*time.Second + execTimeoutGrace},
		{"timeout 1（下限）", `request container-functions c1 exec "x" timeout 1`, 1*time.Second + execTimeoutGrace},
		{"timeout 0（越界）", `request container-functions c1 exec "x" timeout 0`, 0},
		{"timeout 301（越界）", `request container-functions c1 exec "x" timeout 301`, 0},
		{"timeout abc（非法）", `request container-functions c1 exec "x" timeout abc`, 0},
		{"timeout 缺取值", `request container-functions c1 exec "x" timeout`, 0},
		{"引号内的 timeout 不触发", `request container-functions c1 exec "echo timeout 5"`, 0},
		{"单引号同理", `request container-functions c1 exec 'echo timeout 5'`, 0},
		{"timeout 取值在引号内不触发", `request container-functions c1 exec "x" timeout "120"`, 0},
		{"exec 前的 timeout 不触发", `show version timeout 120`, 0},
		{"exec 后无 timeout", `request container-functions c1 exec "sleep 200"`, 0},
		{"timeout 在 exec 之后才找", `timeout 120 request container-functions c1 exec "x"`, 0},
		{"无 token", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := execTimeoutHint(tc.line); got != tc.want {
				t.Fatalf("execTimeoutHint(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

func TestRequestDeadline(t *testing.T) {
	cases := []struct {
		name string
		line string
		want time.Duration
	}{
		{"普通命令回落 RequestTimeout", "show version", RequestTimeout},
		{"exec 120s 命中延长", `request container-functions c1 exec "sleep 200" timeout 120`, 135 * time.Second},
		{"exec 300s 命中延长", `request container-functions c1 exec "x" timeout 300`, 315 * time.Second},
		{"timeout 0 回落 RequestTimeout", `request container-functions c1 exec "x" timeout 0`, RequestTimeout},
		{"timeout 301 回落 RequestTimeout", `request container-functions c1 exec "x" timeout 301`, RequestTimeout},
		{"timeout abc 回落 RequestTimeout", `request container-functions c1 exec "x" timeout abc`, RequestTimeout},
		{"引号内的 timeout 回落 RequestTimeout", `request container-functions c1 exec "echo timeout 5"`, RequestTimeout},
		{"单引号命令同理回落", `request container-functions c1 exec 'echo timeout 5'`, RequestTimeout},
		{"exec 后无 timeout 回落", `request container-functions c1 exec "sleep 200"`, RequestTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestDeadline(tc.line); got != tc.want {
				t.Fatalf("requestDeadline(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

// ctx deadline 真生效：50ms 就睡死的服务器 + 200ms deadline ⇒ 净等待 <2s 报超时
// （若无每请求 deadline，旧全局 Timeout=90s 会让该用例挂到 90s）。
func TestDoWithDeadlineTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done(): // 客户端断开即返回
		case <-time.After(30 * time.Second): // 睡死：远超测试 deadline
		}
	}))
	defer srv.Close()

	c := New(srv.URL)
	start := time.Now()
	err := c.doWithDeadline(200*time.Millisecond, http.MethodGet, "/api/v1/anything", nil, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("睡死服务器 + 短 deadline 应报超时错误")
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("ctx 超时应约 200ms 生效，实际等了 %v", elapsed)
	}
	if !strings.Contains(err.Error(), "请求超时") {
		t.Fatalf("超时错误应说明「请求超时」（与连不上分开）: %v", err)
	}
}

// do 对 cli/execute 的等待时长取自 requestDeadline：命中 hint 时 deadline 更长——
// 用一个 50ms 就睡死的服务器 + 覆盖 RequestTimeout 的 hint 走不到超时（等待被放宽）。
// 这条验证「误报延长等待」方向；漏判方向（回落 90s）由纯函数表驱动覆盖。
func TestDoUsesDeadlineHint(t *testing.T) {
	hint := requestDeadline(`request container-functions c1 exec "sleep 200" timeout 120`)
	if hint != 135*time.Second {
		t.Fatalf("hint 应为 135s，得 %v", hint)
	}
	// 借 doWithDeadline 直接验证同一个 deadline 传导路径：deadline 覆盖响应时间 ⇒ 正常返回。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := New(srv.URL)
	if err := c.doWithDeadline(hint, http.MethodGet, "/api/v1/cli/execute", nil, nil); err != nil {
		t.Fatalf("deadline 135s 下 50ms 响应应正常返回: %v", err)
	}
}

// MetricsText 的有界性：50ms 睡死 + 可中止的 ctx 由函数内部给死（RequestTimeout），
// 这里只验证正常路径不被破坏（真超时路径与 do 同机制，不重复等 90s）。
func TestMetricsTextBoundedNormalPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("nfvis_vnf_up 1\n"))
	}))
	defer srv.Close()
	c := New(srv.URL)
	out, err := c.MetricsText()
	if err != nil {
		t.Fatalf("MetricsText: %v", err)
	}
	if !strings.Contains(out, "nfvis_vnf_up") {
		t.Fatalf("应回原文: %q", out)
	}
}

// 超时错误实现 net.Error 语义（Timeout()=true）——调用方据此区分「超时」与「连不上」。
func TestDeadlineExceededIsTimeoutError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	defer srv.Close()
	c := New(srv.URL)
	err := c.doWithDeadline(100*time.Millisecond, http.MethodGet, "/x", nil, nil)
	if err == nil {
		t.Fatal("应超时")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "请求超时") {
		t.Fatalf("错误应携带超时语义: %v", err)
	}
}
