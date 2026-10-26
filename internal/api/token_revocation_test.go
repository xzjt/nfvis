package api

// 决策 #506 / #507（round49）：token 随用户降权/删除/改口令失效；终端 ticket 与已建立会话
// 随登出/吊销终止。
//
// 端点层守护分两块：
//  ① 身份与凭据——改 class / 重置口令 / 删用户 / 本人改口令之后，旧 token 立即不可用；
//     CLI 的 `set system login user <n> class <c>` 这条**不经主动吊销**的路径由
//     「按当前 committed 配置重算身份」兜住（本文件用 CLI 桥走真实这条路）。
//  ② 终端——ticket 绑定签发它的会话稳定 ID：吊销后①握手即 401；②已握手的会话底层流被关闭
//     （登记表发起的「取消流」，使 io.Copy 桥接两端随即返回）。

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	ksys "github.com/xzjt/nfvis/internal/system"
)

// ---------- ① 身份与凭据 ----------

// authKit 与 newTestServerOpts **同一装配**（真实引擎 + 引导 admin + 预置 viewer + 默认
// SysOps），但把引擎与 AAA 服务一并交回：需要「重新引导 admin」（zeroize 后重启的等价动作）
// 或直接改 committed 配置的用例用它。
//
// 为什么单列一份而不再改 newTestServerOpts：那是全包共用的既有装配，改签名要动全部调用点；
// 本套件只在「会话因账号表复位而结束」的少数用例里需要引擎句柄。
func authKit(t *testing.T, opts Options) (*httptest.Server, *config.Engine, *aaa.Service) {
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
	hash, _ := aaa.HashPassword("s3cret-Passw0rd!")
	sess := config.Session{User: "system", Source: "console"}
	if err := engine.Edit(sess); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	cfg, _ := engine.Committed()
	if cfg.System == nil {
		cfg.System = &model.SystemConfig{}
	}
	if cfg.System.Login == nil {
		cfg.System.Login = &model.SystemLogin{}
	}
	cfg.System.Login.Users = append(cfg.System.Login.Users,
		model.LoginUserConfig{Name: "viewer", PasswordHash: hash, Class: aaa.ClassReadOnly})
	if err := engine.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	if _, err := engine.Commit(context.Background(), sess, config.CommitOpts{Message: "预置 viewer"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	_ = engine.Release(sess)

	opts.Addr = ":0"
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.SysOps == nil {
		tmp := t.TempDir()
		opts.SysOps = ksys.NewManager(ksys.Config{
			Dir:         filepath.Join(tmp, "backup"),
			Captures:    filepath.Join(tmp, "captures"),
			CoreDumps:   filepath.Join(tmp, "coredumps"),
			TechSupport: filepath.Join(tmp, "tech-support"),
			VMs:         filepath.Join(tmp, "vms"),
		}, engine, nil, "test")
	}
	ts := httptest.NewServer(New(engine, authz, opts).Handler())
	t.Cleanup(ts.Close)
	return ts, engine, authz
}

// rebootstrapAdmin 「zeroize 后重启 nfvisd」的等价动作（启动时产品执行同一函数，
// cmd/nfvisd/main.go）：账号表已空 ⇒ 重新引导 admin，返回可直接登录的口令。
func rebootstrapAdmin(t *testing.T, engine *config.Engine, authz *aaa.Service) string {
	t.Helper()
	const pw = "Reboot-Passw0rd!"
	created, _, err := aaa.EnsureBootstrapAdmin(engine, authz, pw)
	if err != nil || !created {
		t.Fatalf("重新引导 admin: created=%v err=%v", created, err)
	}
	return pw
}

// asSuperUser 用指定口令重新登录 admin 并返回新 token（重新引导后或口令已换时用）。
func asSuperUser(t *testing.T, ts *httptest.Server, pw string) string {
	t.Helper()
	status, resp := login(t, ts, "admin", pw)
	if status != http.StatusOK || resp.Token == "" {
		t.Fatalf("admin 重新登录: %d", status)
	}
	return resp.Token
}

// TestTokenClassFollowsCommittedConfig CLI 配置路径（`configure` + `set system login user
// <n> class <c>` + `commit`）改 class 时**没有**主动吊销介入——此时唯一防线是校验期按当前
// committed 配置重算身份：同一 token 的下一请求立刻按新 class 判定（升权与降权两向）。
func TestTokenClassFollowsCommittedConfig(t *testing.T) {
	ts := newTestServer(t)
	adminTok := loginAdmin(t, ts)
	viewerTok := loginViewer(t, ts) // read-only

	superOnly := ts.URL + APIPrefix + "/system/login-users" // 写用户管理：S 档
	readOnly := ts.URL + APIPrefix + "/system/version"      // R 档

	// 基线：read-only 调 S 档 ⇒ 403，调 R 档 ⇒ 200
	if status, _, _ := cfgRequest(t, http.MethodGet, superOnly, viewerTok, nil, nil); status != http.StatusForbidden {
		t.Fatalf("基线：read-only 调 S 档应 403，实得 %d", status)
	}
	if status, _, _ := cfgRequest(t, http.MethodGet, readOnly, viewerTok, nil, nil); status != http.StatusOK {
		t.Fatalf("基线：read-only 调 R 档应 200")
	}

	// 经真实 CLI 路径升权（admin 的 ssh 会话），不触发任何主动吊销
	cliRun(t, ts, adminTok, "ssh", "configure")
	cliRun(t, ts, adminTok, "ssh", "set system login user viewer class super-user")
	cliRun(t, ts, adminTok, "ssh", "commit")

	// 同一 token：现在应能过 S 档（身份随配置，不留签发时的 read-only）
	if status, _, body := cfgRequest(t, http.MethodGet, superOnly, viewerTok, nil, nil); status != http.StatusOK {
		t.Fatalf("升权后同一 token 应可过 S 档（身份随当前配置），实得 %d %s", status, body)
	}

	// 降权回去：同一 token 立刻 403（只读端点不受影响）
	cliRun(t, ts, adminTok, "ssh", "set system login user viewer class read-only")
	cliRun(t, ts, adminTok, "ssh", "commit")
	if status, _, _ := cfgRequest(t, http.MethodGet, superOnly, viewerTok, nil, nil); status != http.StatusForbidden {
		t.Fatalf("降权后同一 token 应 403，实得 %d", status)
	}
	if status, _, _ := cfgRequest(t, http.MethodGet, readOnly, viewerTok, nil, nil); status != http.StatusOK {
		t.Fatalf("降权后只读端点仍应 200")
	}
	cliRun(t, ts, adminTok, "ssh", "exit")
}

// TestDeletedUserTokenRejectedHTTP 删除用户后旧 token 立即 401（两层叠加：主动吊销 + 按配置重算）。
func TestDeletedUserTokenRejectedHTTP(t *testing.T) {
	ts := newTestServer(t)
	adminTok := loginAdmin(t, ts)
	viewerTok := loginViewer(t, ts)

	if status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/version", viewerTok, nil, nil); status != http.StatusOK {
		t.Fatalf("前置：viewer token 应可用")
	}
	if status, _, body := cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/system/login-users/viewer", adminTok, nil, nil); status != http.StatusOK {
		t.Fatalf("删用户: %d %s", status, body)
	}
	if status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/version", viewerTok, nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("用户已删除，旧 token 应 401，实得 %d", status)
	}
	// 其会话也不该再出现在活动会话清单里
	_, tokens := listAPITokens(t, ts, adminTok)
	for _, e := range tokens {
		if e.User == "viewer" {
			t.Fatalf("已删除用户的会话不该还在清单里: %+v", e)
		}
	}
}

// TestClassChangeRevokesUserTokens 改 class（REST PUT）主动吊销该用户全部会话。
func TestClassChangeRevokesUserTokens(t *testing.T) {
	ts := newTestServer(t)
	adminTok := loginAdmin(t, ts)
	opsTok := createSuperUserAndLogin(t, ts, adminTok, "ops2")

	if status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/login-users", opsTok, nil, nil); status != http.StatusOK {
		t.Fatalf("前置：ops2 token 应可过 S 档")
	}
	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system/login-users/ops2", adminTok,
		map[string]any{"class": "read-only"}, nil)
	if status != http.StatusOK {
		t.Fatalf("改 class: %d %s", status, body)
	}
	if status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/login-users", opsTok, nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("改 class 后旧 token 应 401（主动吊销），实得 %d", status)
	}
}

// TestPasswordResetRevokesUserTokens 重置口令（REST PUT）主动吊销该用户全部会话。
func TestPasswordResetRevokesUserTokens(t *testing.T) {
	ts := newTestServer(t)
	adminTok := loginAdmin(t, ts)
	viewerTok := loginViewer(t, ts)
	viewerTok2 := loginViewer(t, ts) // 同一用户两个会话：都应被吊销

	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/system/login-users/viewer", adminTok,
		map[string]any{"password": "Res3t-Passw0rd!"}, nil)
	if status != http.StatusOK {
		t.Fatalf("重置口令: %d %s", status, body)
	}
	for i, tok := range []string{viewerTok, viewerTok2} {
		if status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/version", tok, nil, nil); status != http.StatusUnauthorized {
			t.Fatalf("重置口令后第 %d 个旧 token 应 401，实得 %d", i+1, status)
		}
	}
	// 新口令可登录（改口令本身生效）
	if status, resp := login(t, ts, "viewer", "Res3t-Passw0rd!"); status != http.StatusOK || resp.Token == "" {
		t.Fatalf("新口令应可登录: %d", status)
	}
}

// TestChangePasswordRevokesUserTokens 本人自助改口令成功后，其（含本人的）旧 token 立即失效
// ——口令已换，持旧口令建立的凭据不该继续有效。
func TestChangePasswordRevokesUserTokens(t *testing.T) {
	ts := newTestServer(t)
	viewerTok := loginViewer(t, ts)
	viewerTok2 := loginViewer(t, ts)

	status, _, body := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/login-users/viewer:change-password", viewerTok,
		map[string]any{"old_password": "s3cret-Passw0rd!", "new_password": "N3w-Passw0rd!"}, nil)
	if status != http.StatusOK {
		t.Fatalf("自助改口令: %d %s", status, body)
	}
	for i, tok := range []string{viewerTok, viewerTok2} {
		if status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/version", tok, nil, nil); status != http.StatusUnauthorized {
			t.Fatalf("改口令后第 %d 个旧 token 应 401，实得 %d", i+1, status)
		}
	}
}

// createSuperUserAndLogin 建一个 super-user 账号并登录，返回其 token（做「被降权/被吊销」的实验组）。
func createSuperUserAndLogin(t *testing.T, ts *httptest.Server, adminTok, name string) string {
	t.Helper()
	const pw = "Ops2-Passw0rd!"
	status, _, body := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/login-users", adminTok,
		map[string]any{"name": name, "kind": "user", "password": pw, "class": "super-user"}, nil)
	if status != http.StatusCreated {
		t.Fatalf("建 %s: %d %s", name, status, body)
	}
	code, resp := login(t, ts, name, pw)
	if code != http.StatusOK || resp.Token == "" {
		t.Fatalf("登录 %s: %d", name, code)
	}
	return resp.Token
}

// ---------- ② 终端 ticket 与会话 ----------

// consoleFixture 建一台 serial VM（含资源池前置）供 ticket/会话用例使用。
func consoleFixture(t *testing.T, fc *fakeConsoleRuntime) *httptest.Server {
	t.Helper()
	ts := newTestServerOpts(t, Options{VM: newFakeVM(), VMConsole: fc})
	adminTok := loginAdmin(t, ts)
	seedVMPool(t, ts, adminTok)
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", adminTok,
		vmBody("fw-vm"), map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("建 VM: %d %s", status, data)
	}
	return ts
}

// consoleTicketOf 申请串口 console ticket，返回相对 ws 路径。
func consoleTicketOf(t *testing.T, ts *httptest.Server, token, vm string) string {
	t.Helper()
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions/"+vm+"/console", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("申请 console 凭证: %d %s", status, data)
	}
	var got struct {
		WSURL string `json:"ws_url"`
	}
	if err := json.Unmarshal(data, &got); err != nil || got.WSURL == "" {
		t.Fatalf("凭证响应不符: %s", data)
	}
	return got.WSURL
}

// wsDialURL 把相对 ws 路径拼成可拨的地址。
func wsDialURL(ts *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + path
}

// TestConsoleTicketHandshakeRejectedAfterTokenRevoke ticket 绑定签发它的会话稳定 ID：
// 该会话被吊销后，**握手期**即复核失败（401），不再只是「TTL 内随便用」。
func TestConsoleTicketHandshakeRejectedAfterTokenRevoke(t *testing.T) {
	fc := &fakeConsoleRuntime{stream: newFakeConsoleStream("HELLO-CONSOLE\r\n")}
	ts := consoleFixture(t, fc)
	t1 := loginAdmin(t, ts)
	t2 := loginAdmin(t, ts) // 第二个会话，用来吊销 t1

	wsPath := consoleTicketOf(t, ts, t1, "fw-vm")

	// 吊销 t1（用 t2 吊销 t1 的稳定 ID）
	_, tokens := listAPITokens(t, ts, t1)
	id1 := currentTokenID(t, tokens)
	if code, body := revokeAPIToken(t, ts, t2, id1); code != http.StatusNoContent {
		t.Fatalf("吊销 t1: %d %s", code, body)
	}
	// 握手：应 401（ticket 本身未被消费过，但绑定会话已死）
	if _, err := websocket.Dial(wsDialURL(ts, wsPath), "", ts.URL); err == nil {
		t.Fatal("token 已吊销，ticket 握手应被拒（401）")
	}
}

// currentTokenID 取清单里标 current 的会话 ID（调用方自己的会话）。
func currentTokenID(t *testing.T, tokens []apiTokenEntry) string {
	t.Helper()
	for _, e := range tokens {
		if e.Current {
			return e.TokenID
		}
	}
	t.Fatalf("清单里没有当前会话标记: %+v", tokens)
	return ""
}

// TestActiveConsoleSessionClosedOnTokenRevoke 已握手的串口会话在吊销后**底层流被关闭**
// （登记表发起的取消流 ⇒ io.Copy 桥接两端随即返回）。
func TestActiveConsoleSessionClosedOnTokenRevoke(t *testing.T) {
	fc := &fakeConsoleRuntime{stream: newFakeConsoleStream("HELLO-CONSOLE\r\n")}
	ts := consoleFixture(t, fc)
	t1 := loginAdmin(t, ts)

	wsPath := consoleTicketOf(t, ts, t1, "fw-vm")
	conn := dialConsole(t, ts, wsPath, "HELLO-CONSOLE\r\n")
	defer conn.Close()

	// 自吊销（等价于登出：吊销当前会话）
	_, tokens := listAPITokens(t, ts, t1)
	if code, body := revokeAPIToken(t, ts, t1, currentTokenID(t, tokens)); code != http.StatusNoContent {
		t.Fatalf("自吊销: %d %s", code, body)
	}
	assertStreamClosed(t, fc.stream, "串口 console（吊销）")
}

// TestActiveConsoleSessionClosedOnLogout 登出同样终止本会话已建立的终端。
func TestActiveConsoleSessionClosedOnLogout(t *testing.T) {
	fc := &fakeConsoleRuntime{stream: newFakeConsoleStream("HELLO-CONSOLE\r\n")}
	ts := consoleFixture(t, fc)
	t1 := loginAdmin(t, ts)

	wsPath := consoleTicketOf(t, ts, t1, "fw-vm")
	conn := dialConsole(t, ts, wsPath, "HELLO-CONSOLE\r\n")
	defer conn.Close()

	status, _ := postJSON(t, ts.URL+APIPrefix+"/logout", nil, map[string]string{"Authorization": "Bearer " + t1})
	if status != http.StatusNoContent {
		t.Fatalf("logout 应 204: %d", status)
	}
	assertStreamClosed(t, fc.stream, "串口 console（登出）")
}

// TestActiveContainerShellClosedOnTokenRevoke 容器交互式终端与串口同源：吊销后流被关闭。
func TestActiveContainerShellClosedOnTokenRevoke(t *testing.T) {
	stream := newFakeConsoleStream("root@ct-a:/# \r\n")
	ct := &watchShellCT{fakeCLIContainer: newFakeCLIContainer(), stream: stream}
	ts := newTestServerOpts(t, Options{Containers: ct})
	t1 := loginAdmin(t, ts)
	seedContainerCommit(t, ts, t1, "ct-a")
	ct.states["ct-a"] = orchestrator.CTStateRunning

	status, _, raw := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/container-functions/ct-a/shell", t1, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("申请容器终端凭证: %d %s", status, raw)
	}
	var got struct {
		WSURL string `json:"ws_url"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || got.WSURL == "" {
		t.Fatalf("凭证响应不符: %s", raw)
	}
	conn := dialConsole(t, ts, got.WSURL, "root@ct-a:/# \r\n")
	defer conn.Close()

	_, tokens := listAPITokens(t, ts, t1)
	if code, body := revokeAPIToken(t, ts, t1, currentTokenID(t, tokens)); code != http.StatusNoContent {
		t.Fatalf("自吊销: %d %s", code, body)
	}
	assertStreamClosed(t, stream, "容器 shell（吊销）")
}

// TestCLIIssuedConsoleTicketStillWorks CLI 路径（`request virtual-machine-functions <n> console`）
// 签发的 ticket 拿不到 Bearer 上下文里的稳定 ID——空 ID 保守放行：该路径不得因本轮加固而失效。
func TestCLIIssuedConsoleTicketStillWorks(t *testing.T) {
	fc := &fakeConsoleRuntime{stream: newFakeConsoleStream("HELLO-CONSOLE\r\n")}
	ts := consoleFixture(t, fc)
	t1 := loginAdmin(t, ts)

	res := cliLine(t, ts, t1, "ssh", "request virtual-machine-functions fw-vm console")
	if res.Console == nil || res.Console.WSURL == "" {
		t.Fatalf("CLI 应产出 console 接管请求: %+v", res.Console)
	}
	conn := dialConsole(t, ts, res.Console.WSURL, "HELLO-CONSOLE\r\n")
	_ = conn.Close()
}

// watchShellCT 容器运行态 fake：只覆盖终端流（返回可观察关闭的流，其余交给内嵌 fake）。
type watchShellCT struct {
	*fakeCLIContainer
	stream *fakeConsoleStream
}

func (w *watchShellCT) ContainerShell(context.Context, string) (io.ReadWriteCloser, error) {
	return w.stream, nil
}

// dialConsole 建立 WS 会话并读到首帧问候（证明握手成功、会话已注册）。
func dialConsole(t *testing.T, ts *httptest.Server, wsPath, greeting string) *websocket.Conn {
	t.Helper()
	conn, err := websocket.Dial(wsDialURL(ts, wsPath), "", ts.URL)
	if err != nil {
		t.Fatalf("ws 连接失败: %v", err)
	}
	buf := make([]byte, len(greeting))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != greeting {
		t.Fatalf("应读到首帧问候 %q，实得 %q err=%v", greeting, buf, err)
	}
	return conn
}

// assertStreamClosed 等底层流被关闭：登记表以「关闭流」实现取消——这正是能中断 io.Copy 桥接
// 的最小可用手段（任一端读/写随 Close 出错返回，桥接随即收尾）。
func assertStreamClosed(t *testing.T, s *fakeConsoleStream, what string) {
	t.Helper()
	select {
	case <-s.closed:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s：吊销/登出后底层流应被关闭（桥接应被取消）", what)
	}
}

// ---------- 登记表单元守护 ----------

// TestConsoleSessionTable 登记表：按 token ID 与用户名两条索引、注销幂等、空键不no-op误伤。
func TestConsoleSessionTable(t *testing.T) {
	tbl := newConsoleSessionTable()

	var mu sync.Mutex
	closed := []string{}
	mark := func(name string) func() {
		return func() { mu.Lock(); closed = append(closed, name); mu.Unlock() }
	}

	unreg1 := tbl.register("tok-1", "alice", mark("s1"))
	tbl.register("tok-2", "alice", mark("s2"))
	tbl.register("tok-3", "bob", mark("s3"))
	tbl.register("", "bob", mark("s4")) // 无稳定 ID 的登记（CLI 路径）只按用户可关

	if tbl.count() != 4 {
		t.Fatalf("登记后应 4 条，实得 %d", tbl.count())
	}
	if n := tbl.closeByToken("tok-1"); n != 1 {
		t.Fatalf("closeByToken(tok-1) 应关 1 条，实得 %d", n)
	}
	if n := tbl.closeByToken("tok-2"); n != 1 {
		t.Fatalf("closeByToken(tok-2) 应关 1 条，实得 %d", n)
	}
	if n := tbl.closeByToken("tok-2"); n != 0 {
		t.Fatalf("重复关闭应 0 条，实得 %d", n)
	}
	if n := tbl.closeByToken(""); n != 0 {
		t.Fatalf("空 token ID 不应关闭任何会话（不误伤），实得 %d", n)
	}
	// 按用户兜底：alice 的两条已被按 ID 关掉，bob 还剩两条（含无 ID 的那条）
	if n := tbl.closeByUser("alice"); n != 0 {
		t.Fatalf("alice 的会话已被按 ID 关掉，实得 %d", n)
	}
	if n := tbl.closeByUser("bob"); n != 2 {
		t.Fatalf("closeByUser(bob) 应关 2 条（含无稳定 ID 的登记），实得 %d", n)
	}
	if tbl.count() != 0 {
		t.Fatalf("全部关闭后登记表应清空，实得 %d 条", tbl.count())
	}
	// 注销函数幂等（桥接结束时 defer 再调一次不应出问题）
	unreg1()
	unreg1()
	if tbl.count() != 0 {
		t.Fatalf("重复注销不得改变登记表，实得 %d 条", tbl.count())
	}

	mu.Lock()
	defer mu.Unlock()
	want := map[string]bool{"s1": true, "s2": true, "s3": true, "s4": true}
	if len(closed) != len(want) {
		t.Fatalf("关闭次数不符（不应重复关闭）: %v", closed)
	}
	for _, name := range closed {
		if !want[name] {
			t.Fatalf("未知会话被关闭: %v", closed)
		}
	}
}
