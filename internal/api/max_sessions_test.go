package api

// FR-SYS-006（决策 #71）：`system.api.max-sessions` 必须真正生效。
// 此前该字段在命令树与 OpenAPI 中均有声明，但全仓无任何代码使用它
// （与 sriov.vf-count 同类：声明了但无实现，静默无效）。

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// newSrvForTest 直接装配 Server（需访问 maxSessions/listener 等非 HTTP 面）。
func newSrvForTest(t *testing.T) (*Server, *config.Engine) {
	t.Helper()
	store, err := config.OpenStore(filepath.Join(t.TempDir(), "nfvis.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	eng, err := config.NewEngine(store, orchestrator.NewNoopApplier(), config.Options{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(eng.Close)
	return New(eng, aaa.NewService(eng, nil), Options{}), eng
}

func commitMaxSessions(t *testing.T, eng *config.Engine, n int) {
	t.Helper()
	sess := config.Session{User: "test", Source: "test"}
	if err := eng.Edit(sess); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	cfg, _, err := eng.Candidate()
	if err != nil {
		t.Fatalf("Candidate: %v", err)
	}
	if cfg.System == nil {
		cfg.System = &model.SystemConfig{}
	}
	cfg.System.API = &model.APIConfig{MaxSessions: n}
	if err := eng.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	if _, err := eng.Commit(context.Background(), sess, config.CommitOpts{Message: "t"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := eng.Release(sess); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestMaxSessionsConfigPlumbing(t *testing.T) {
	srv, eng := newSrvForTest(t)
	if got := srv.maxSessions(); got != 0 {
		t.Fatalf("未配置时应为 0（不限）: %d", got)
	}
	commitMaxSessions(t, eng, 2)
	if got := srv.maxSessions(); got != 2 {
		t.Fatalf("max_sessions 未从 committed 生效: %d", got)
	}
	// 负值由 commit 校验拒绝（FR-SYS-006；maxSessions 侧亦防御性按不限处理）
	sess := config.Session{User: "test", Source: "test"}
	if err := eng.Edit(sess); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	cfg, _, _ := eng.Candidate()
	cfg.System.API = &model.APIConfig{MaxSessions: -1}
	if err := eng.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	if _, err := eng.Commit(context.Background(), sess, config.CommitOpts{}); err == nil {
		t.Fatal("负 max_sessions 应被 commit 校验拒绝")
	}
	_ = eng.Release(sess)
}

// 上限生效：配置 1 后，第二条连接不被 accept（LimitListener 阻塞）。
func TestMaxSessionsLimitsConnections(t *testing.T) {
	srv, eng := newSrvForTest(t)
	commitMaxSessions(t, eng, 1)

	ln, err := srv.listener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listener: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c // 持有不放，占满配额
		}
	}()

	c1, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("第一条连接: %v", err)
	}
	defer c1.Close()
	time.Sleep(200 * time.Millisecond) // 让第一条被 accept

	c2, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("第二条 TCP 连接应建立: %v", err)
	}
	defer c2.Close()
	_ = c2.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	if _, err := c2.Read(make([]byte, 1)); err == nil {
		t.Fatal("上限=1 时第二条连接不应被放行")
	} else if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("期望读超时（未被 accept），得到 %v", err)
	}
}

// 不限（0）时 listener 不应包限流器——多条连接均被 accept。
func TestMaxSessionsUnlimitedByDefault(t *testing.T) {
	srv, _ := newSrvForTest(t)
	ln, err := srv.listener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listener: %v", err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- struct{}{}
			_ = c
		}
	}()
	var conns []net.Conn
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("连接 %d: %v", i, err)
		}
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := 0; i < 3; i++ {
		select {
		case <-accepted:
		case <-time.After(time.Second):
			t.Fatalf("不限时应全部 accept（第 %d 条未通过）", i+1)
		}
	}
}
