package api

// 决策 #513（round50）：`Server.RevokeUserSessions` 是装配层「提交路径身份收尾」调用的统一入口，
// 必须与 REST 用户 handler 复用**同一套三层收尾**（token / CLI 会话 / 终端）——本测试钉住三层里
// 可观察的两层：token 立即失效、已建立的串口终端底层流被关闭（登记表清空）。
//
// 分层理由：提交路径（CLI delete/set + commit、load/restore、zeroize）拿不到 REST 用户 handler
// 里挂的主动吊销，装配层按配置差异算出用户名后调用本方法——若本方法悄悄丢了一层，被删用户的
// 终端仍会活着。装配层如何算出受影响用户名由 cmd/nfvisd 的身份收尾单测覆盖。

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// newConsoleServer 建一个**保留 *Server 句柄**的串口 console 现场（与 consoleFixture 同一装配，
// 只是要拿 srv 调导出方法）。
func newConsoleServer(t *testing.T, fc *fakeConsoleRuntime) (*httptest.Server, *Server, *aaa.Service) {
	t.Helper()
	store, err := config.OpenStore(filepath.Join(t.TempDir(), "nfvis.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	engine, err := config.NewEngine(store, orchestrator.NewNoopApplier(), config.Options{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(engine.Close)
	authz := aaa.NewService(engine, nil)
	if _, _, err := aaa.EnsureBootstrapAdmin(engine, authz, "s3cret-Passw0rd!"); err != nil {
		t.Fatalf("引导 admin: %v", err)
	}
	srv := New(engine, authz, Options{
		Addr: ":0", Log: slog.New(slog.DiscardHandler),
		VM: newFakeVM(), VMConsole: fc,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv, authz
}

// TestRevokeUserSessionsClosesConsoleSessionAndTokens 导出入口一次收尾三层：
// 已握手的串口终端被关闭、token 立即失效、登记表清空。
func TestRevokeUserSessionsClosesConsoleSessionAndTokens(t *testing.T) {
	fc := &fakeConsoleRuntime{stream: newFakeConsoleStream("HELLO-CONSOLE\r\n")}
	ts, srv, authz := newConsoleServer(t, fc)
	adminTok := loginAdmin(t, ts)
	seedVMPool(t, ts, adminTok)
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", adminTok,
		vmBody("fw-vm"), map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("建 VM: %d %s", status, data)
	}
	wsPath := consoleTicketOf(t, ts, adminTok, "fw-vm")
	conn := dialConsole(t, ts, wsPath, "HELLO-CONSOLE\r\n")
	defer conn.Close()
	if srv.consoleTix.sessions.count() == 0 {
		t.Fatalf("前置：串口会话应已登记")
	}

	// 装配层对「admin 凭据已变」调用统一收尾
	ids := srv.RevokeUserSessions("admin")
	if len(ids) == 0 {
		t.Fatalf("收尾应返回被吊销的 token 稳定 ID")
	}
	if _, err := authz.VerifyToken(adminTok); err == nil {
		t.Fatalf("收尾后该用户 token 应立即失效")
	}
	assertStreamClosed(t, fc.stream, "导出入口收尾（串口 console）")
	if n := srv.consoleTix.sessions.count(); n != 0 {
		t.Fatalf("收尾后活动终端登记表应清空，实得 %d 条", n)
	}
	// 空用户名不动作（不误伤全体）
	if got := srv.RevokeUserSessions(""); got != nil {
		t.Fatalf("空用户名不应收尾任何会话，实得 %v", got)
	}
}
