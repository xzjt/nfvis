package main

// 决策 #513（round50）：凭据变更在**全部提交路径**收尾。
//
// 由来：主动吊销只挂在 REST 的用户 handler 三处。CLI `delete/set system login user …` + commit、
// restore、zeroize 等整文档提交不经过那里；`VerifyToken` 只保护后续 HTTP 请求，已升级的
// 串口/VNC/容器 shell 是长连接（不再发 HTTP），于是被删用户的容器 shell 仍可执行命令、
// `show system api tokens` 仍把它列为活动会话。
//
// 本文件钉住装配层接线（`loginIdentityFinalizer` + `main.go` 的 OnCommitted）：
//   - 受影响的用户名按前后差异算出（删除 / class 变 / 口令哈希变；zeroize 即全体）；
//   - 收尾经 `api.Server.RevokeUserSessions` 完成（token 立即失效 + 活动会话清单隐藏 +
//     终端复核不再活动）；
//   - 恢复型回滚（用户重新出现）不吊销、不误伤。
//
// 三层收尾里「终端被关闭」这一层的端到端行为由 internal/api 的
// TestRevokeUserSessionsClosesConsoleSessionAndTokens 覆盖；装配层是否把受影响用户名正确喂给它，
// 由本文件覆盖。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/api"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/system"
)

const idAdminPw = "s3cret-Passw0rd!"

// identityKit 装配一个与生产同形的现场：真实引擎 + 引擎背书的 AAA + api.Server +
// 提交后身份收尾器（钩子先赋值、finalizer 后创建，与 main.go 同序）。
type identityKit struct {
	ts     *httptest.Server
	engine *config.Engine
	authz  *aaa.Service
	sysOps *system.Manager
	fin    *loginIdentityFinalizer

	// revokedMu/revoked 记录统一收尾被调用的用户名（证明装配层真的把受影响用户名喂给了收尾）。
	revokedMu sync.Mutex
	revoked   []string
}

func newIdentityKit(t *testing.T) *identityKit {
	t.Helper()
	store, err := config.OpenStore(filepath.Join(t.TempDir(), "nfvis.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	k := &identityKit{}
	engine, err := config.NewEngine(store, orchestrator.NewNoopApplier(), config.Options{
		// 与 main.go 同序：钩子先赋值（此刻 finalizer 尚未创建，闭包内先判 nil）。
		OnCommitted: func(int, string) {
			if k.fin != nil {
				k.fin.enqueue()
			}
		},
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(engine.Close)
	k.engine = engine
	k.authz = aaa.NewService(engine, nil)
	if _, _, err := aaa.EnsureBootstrapAdmin(engine, k.authz, idAdminPw); err != nil {
		t.Fatalf("引导 admin: %v", err)
	}

	tmp := t.TempDir()
	k.sysOps = system.NewManager(system.Config{
		Dir:         filepath.Join(tmp, "backup"),
		Captures:    filepath.Join(tmp, "captures"),
		CoreDumps:   filepath.Join(tmp, "coredumps"),
		TechSupport: filepath.Join(tmp, "tech-support"),
		VMs:         filepath.Join(tmp, "vms"),
	}, engine, nil, "test")

	srv := api.New(engine, k.authz, api.Options{Addr: ":0", Log: slog.New(slog.DiscardHandler), SysOps: k.sysOps})
	// 与 main.go 同一收尾入口：api.Server.RevokeUserSessions（测这里额外记录受影响用户名）。
	k.fin = newLoginIdentityFinalizer(engine.Committed, func(user string) []string {
		k.revokedMu.Lock()
		k.revoked = append(k.revoked, user)
		k.revokedMu.Unlock()
		return srv.RevokeUserSessions(user)
	}, slog.New(slog.DiscardHandler))
	t.Cleanup(k.fin.close)

	k.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(k.ts.Close)
	return k
}

func (k *identityKit) wasRevoked(user string) bool {
	k.revokedMu.Lock()
	defer k.revokedMu.Unlock()
	for _, u := range k.revoked {
		if u == user {
			return true
		}
	}
	return false
}

// ---------- HTTP 小工具（cmd/nfvisd 是独立包，不引 api 测试内部件） ----------

func idLogin(t *testing.T, ts *httptest.Server, user, pw string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": user, "password": pw})
	resp, err := http.Post(ts.URL+api.APIPrefix+"/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login 请求: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.Token
}

func idMustLogin(t *testing.T, ts *httptest.Server, user, pw string) string {
	t.Helper()
	status, tok := idLogin(t, ts, user, pw)
	if status != http.StatusOK || tok == "" {
		t.Fatalf("登录 %s: %d", user, status)
	}
	return tok
}

// idCLI 经 CLI 桥执行一行命令（`%%` 前缀判失败）。
func idCLI(t *testing.T, ts *httptest.Server, token, source, line string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"line": line, "source": source})
	req, err := http.NewRequest(http.MethodPost, ts.URL+api.APIPrefix+"/cli/execute", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("构造 CLI 请求: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("CLI 请求: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Output string `json:"output"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CLI %q: %d %s", line, resp.StatusCode, raw)
	}
	if strings.Contains(out.Output, "%%") {
		t.Fatalf("CLI %q 失败: %s", line, out.Output)
	}
	return out.Output
}

// idConfig 走「configure → 若干语句 → commit → exit」的 CLI 配置事务并释放编辑锁
// （exit 释放锁后，后续 zeroize/restore 的另一个来源才取得到引擎锁）。
func idConfig(t *testing.T, ts *httptest.Server, token string, lines ...string) {
	t.Helper()
	idCLI(t, ts, token, "ssh", "configure")
	for _, l := range lines {
		idCLI(t, ts, token, "ssh", l)
	}
	idCLI(t, ts, token, "ssh", "commit")
	idCLI(t, ts, token, "ssh", "exit")
}

// idCreateUser 建一个用户（REST 一次性直提：本族不含收尾，建用户无需收尾）。
func idCreateUser(t *testing.T, ts *httptest.Server, adminTok, name, pw, class string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name, "kind": "user", "password": pw, "class": class})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+api.APIPrefix+"/system/login-users", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("建用户请求: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("建用户 %s: %d %s", name, resp.StatusCode, raw)
	}
}

func idShowAPITokens(t *testing.T, ts *httptest.Server, token string) string {
	t.Helper()
	return idCLI(t, ts, token, "ssh", "show system api tokens")
}

// idStableID 取某用户在活动会话清单里的稳定 ID（供 ActiveTokenID 复核）。
func idStableID(t *testing.T, k *identityKit, user string) string {
	t.Helper()
	for _, v := range k.authz.ListTokens("admin", aaa.ClassSuperUser) {
		if v.User == user {
			return v.ID
		}
	}
	return ""
}

// ---------- 单测：差分算法 ----------

func TestAffectedLoginUsers(t *testing.T) {
	snap := map[string]loginFingerprint{
		"admin": {"super-user", "h-admin"},
		"ops":   {"operator", "h-ops"},
		"gone":  {"read-only", "h-gone"},
	}
	next := map[string]loginFingerprint{
		"admin": {"super-user", "h-admin"}, // 不变
		"ops":   {"read-only", "h-ops"},    // class 变
		"new":   {"operator", "h-new"},     // 新增（不吊销）
	}
	got := affectedLoginUsers(snap, next)
	want := []string{"gone", "ops"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("受影响集合不符：got=%v want=%v", got, want)
	}
	// 口令哈希变
	if got := affectedLoginUsers(
		map[string]loginFingerprint{"admin": {"super-user", "h-admin"}},
		map[string]loginFingerprint{"admin": {"super-user", "h-admin2"}},
	); len(got) != 1 || got[0] != "admin" {
		t.Fatalf("口令哈希变应判受影响：%v", got)
	}
	// 空目标（zeroize 清空账号）→ 全体
	if got := affectedLoginUsers(snap, map[string]loginFingerprint{}); strings.Join(got, ",") != "admin,gone,ops" {
		t.Fatalf("账号清空应判全体删除：%v", got)
	}
}

// ---------- 集成：CLI / zeroize / restore 提交路径 ----------

// TestCLIDeleteUserRevokesActiveTokensAndSessions CLI `delete system login user <n>` + commit
// 不经 REST 用户 handler，靠装配层身份收尾生效：token 立即失效、活动会话清单隐藏、
// 终端复核不再活动。
func TestCLIDeleteUserRevokesActiveTokensAndSessions(t *testing.T) {
	k := newIdentityKit(t)
	adminTok := idMustLogin(t, k.ts, "admin", idAdminPw)
	idCreateUser(t, k.ts, adminTok, "opsx", "Opsx-Passw0rd!", "operator")
	opsTok := idMustLogin(t, k.ts, "opsx", "Opsx-Passw0rd!")
	opsID := idStableID(t, k, "opsx")
	if opsID == "" || !k.authz.ActiveTokenID(opsID) {
		t.Fatalf("前置：opsx 会话应活动且在册")
	}
	if !strings.Contains(idShowAPITokens(t, k.ts, adminTok), "opsx") {
		t.Fatalf("前置：活动会话清单应含 opsx")
	}

	idConfig(t, k.ts, adminTok, "delete system login user opsx")
	k.fin.drain() // 确定性同步（生产为异步 worker）

	if _, err := k.authz.VerifyToken(opsTok); err == nil {
		t.Fatalf("删用户后旧 token 应立即失效")
	}
	if k.authz.ActiveTokenID(opsID) {
		t.Fatalf("终端复核应报该会话不再活动（据此关闭终端）")
	}
	if !k.wasRevoked("opsx") {
		t.Fatalf("装配层应把 opsx 喂给统一收尾")
	}
	if out := idShowAPITokens(t, k.ts, adminTok); strings.Contains(out, "opsx") {
		t.Fatalf("已删除用户不该再出现在活动会话清单：\n%s", out)
	}
}

// TestCLIClassChangeRevokesUserTokens CLI 改 class（+ commit）后该用户全部会话被吊销。
func TestCLIClassChangeRevokesUserTokens(t *testing.T) {
	k := newIdentityKit(t)
	adminTok := idMustLogin(t, k.ts, "admin", idAdminPw)
	idCreateUser(t, k.ts, adminTok, "opsx", "Opsx-Passw0rd!", "operator")
	opsTok := idMustLogin(t, k.ts, "opsx", "Opsx-Passw0rd!")

	idConfig(t, k.ts, adminTok, "set system login user opsx class read-only")
	k.fin.drain()

	if _, err := k.authz.VerifyToken(opsTok); err == nil {
		t.Fatalf("改 class 后旧 token 应立即失效")
	}
	if !k.wasRevoked("opsx") {
		t.Fatalf("改 class 应收尾该用户")
	}
	if _, err := k.authz.VerifyToken(adminTok); err != nil {
		t.Fatalf("admin 会话不应被误伤: %v", err)
	}
}

// TestCLIPasswordChangeRevokes CLI 改口令（+ commit）后持旧口令建立的会话失效；新口令可登录。
func TestCLIPasswordChangeRevokes(t *testing.T) {
	k := newIdentityKit(t)
	adminTok := idMustLogin(t, k.ts, "admin", idAdminPw)
	idCreateUser(t, k.ts, adminTok, "opsx", "Opsx-Passw0rd!", "operator")
	opsTok := idMustLogin(t, k.ts, "opsx", "Opsx-Passw0rd!")
	opsTok2 := idMustLogin(t, k.ts, "opsx", "Opsx-Passw0rd!")

	idConfig(t, k.ts, adminTok, "set system login user opsx password New-Opsx-Passw0rd!")
	k.fin.drain()

	for i, tok := range []string{opsTok, opsTok2} {
		if _, err := k.authz.VerifyToken(tok); err == nil {
			t.Fatalf("改口令后第 %d 个旧 token 应立即失效", i+1)
		}
	}
	if !k.wasRevoked("opsx") {
		t.Fatalf("改口令应收尾该用户")
	}
	if _, tok := idLogin(t, k.ts, "opsx", "New-Opsx-Passw0rd!"); tok == "" {
		t.Fatalf("新口令应可登录")
	}
}

// TestZeroizeRevokesAllSessions zeroize 清空账号 ⇒ 全体会话被收尾（含发起者自己）。
func TestZeroizeRevokesAllSessions(t *testing.T) {
	k := newIdentityKit(t)
	adminTok := idMustLogin(t, k.ts, "admin", idAdminPw)
	idCreateUser(t, k.ts, adminTok, "opsx", "Opsx-Passw0rd!", "operator")
	opsTok := idMustLogin(t, k.ts, "opsx", "Opsx-Passw0rd!")

	if _, err := k.sysOps.Zeroize(context.Background(), "admin"); err != nil {
		t.Fatalf("zeroize: %v", err)
	}
	k.fin.drain()

	if _, err := k.authz.VerifyToken(adminTok); err == nil {
		t.Fatalf("zeroize 后 admin 旧 token 应失效")
	}
	if _, err := k.authz.VerifyToken(opsTok); err == nil {
		t.Fatalf("zeroize 后 opsx 旧 token 应失效")
	}
	if !k.wasRevoked("admin") || !k.wasRevoked("opsx") {
		t.Fatalf("zeroize 应收尾全体用户（admin/opsx）")
	}
}

// TestRestoreDoesNotRevokeReappearingUser 恢复型回滚（用户重新出现）不吊销、不误伤：
// 删 opsx → opsx 会话被吊销；restore 把 opsx 带回来，但 admin 的会话不因此被吊销。
func TestRestoreDoesNotRevokeReappearingUser(t *testing.T) {
	k := newIdentityKit(t)
	adminTok := idMustLogin(t, k.ts, "admin", idAdminPw)
	idCreateUser(t, k.ts, adminTok, "opsx", "Opsx-Passw0rd!", "operator")
	opsTok := idMustLogin(t, k.ts, "opsx", "Opsx-Passw0rd!")

	// 备份（含 admin + opsx）
	f, err := k.sysOps.Backup()
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	path, err := k.sysOps.Path(f.File)
	if err != nil {
		t.Fatalf("backup path: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读归档: %v", err)
	}

	// 删 opsx：其会话被收尾
	idConfig(t, k.ts, adminTok, "delete system login user opsx")
	k.fin.drain()
	if _, err := k.authz.VerifyToken(opsTok); err == nil {
		t.Fatalf("删 opsx 后其 token 应失效")
	}
	adminID := idStableID(t, k, "admin")
	if adminID == "" {
		t.Fatalf("admin 会话应在册")
	}

	// 恢复：opsx 重新出现（新增，不吊销）；admin 指纹未变（不吊销）
	if _, _, err := k.sysOps.Restore(context.Background(), data, "admin"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	k.fin.drain()

	if _, err := k.authz.VerifyToken(adminTok); err != nil {
		t.Fatalf("恢复不应误伤在册且指纹未变的用户（admin）: %v", err)
	}
	if !k.authz.ActiveTokenID(adminID) {
		t.Fatalf("恢复后 admin 会话应仍活动")
	}
	// 重新出现的 opsx 可用旧口令登录（用户确实回来了）
	if _, tok := idLogin(t, k.ts, "opsx", "Opsx-Passw0rd!"); tok == "" {
		t.Fatalf("恢复后 opsx 应可登录")
	}
}

// ---------- 静态装配守护 ----------

// TestIdentityFinalizeWiringInMain main.go 必须在 OnCommitted 里驱动身份收尾，且收尾经
// api.Server 的统一入口（不新造第二套）。漏接线不会编译失败，也不会被任何单测抓到
// （各包单测都自己注入假件），故按本仓既有先例静态钉住。
func TestIdentityFinalizeWiringInMain(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{
		"identityFin = newLoginIdentityFinalizer(engine.Committed, func(user string) []string {",
		"return apiSrv.RevokeUserSessions(user)",
		"identityFin.enqueue()",
		"defer identityFin.close()",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("main.go 缺少 %q（决策 #513 的身份收尾接线）", want)
		}
	}
	iHook := strings.Index(src, "engineOpts.OnCommitted = func(revision int, user string) {")
	iEnqueue := strings.Index(src, "identityFin.enqueue()")
	if iHook < 0 || iEnqueue < 0 || iEnqueue < iHook {
		t.Fatalf("identityFin.enqueue() 应在 OnCommitted 回调体内（hook@%d enqueue@%d）", iHook, iEnqueue)
	}
}
