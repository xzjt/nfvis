package api

// FR-SEC-007 / 决策 #25：口令哈希不得回显于任何 show/API 输出。
//
// 背景（V1 收尾复核发现的安全缺陷）：commit 的审计详情是完整 diff，而
// GET /audit-logs 只需 ClassReadOnly，导致 operator/read-only 账号可取得
// 他人 PBKDF2 哈希原文；配置视图（candidate/rollback 回显）同样含哈希。
// 本测试为「全路径脱敏」守护：任何新增的泄露路径都会在此失败。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"context"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
)

const leakedHashMarker = "pbkdf2$"

// sentinelHash 本次提交进 committed 的**哨兵哈希**：值由本测试给定，故可以精确断言
// 「这一串没有出现」——比裸前缀判据可靠（契约里 password_hash 的格式说明本身就含
// `pbkdf2$` 前缀，拿前缀判会把规范文本误报成泄露，属工具假红）。
const sentinelHash = "pbkdf2$sha256$600000$VIEWSALT$VIEWHASH"

// realHashRe 真哈希的形态：`pbkdf2$sha256$<迭代数>$…`。契约里的格式说明写的是
// `<iter>` 占位符（不是数字），不会命中；而任何**真**哈希必然命中——两条判据互补：
// 哨兵判"这一次的具体值"，正则判"任何真哈希"（防将来换成别的种子）。
var realHashRe = regexp.MustCompile(`pbkdf2\$sha256\$\d+\$`)

// seedUserWithPassword 经 API 建一个带口令的用户（走真实 commit + 审计链路）。
func seedUserWithPassword(t *testing.T, ts *httptest.Server, token, name string) {
	t.Helper()
	body := map[string]any{"name": name, "password": "Secret@98765", "class": "operator"}
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/login-users", token,
		body, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("建用户 %s: %d %s", name, status, data)
	}
}

// 全路径守护：审计日志、配置视图、diff、CLI show 均不得出现口令哈希。
func TestSecretsNeverEchoed(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	seedUserWithPassword(t, ts, token, "opssec")

	// 1) GET /audit-logs（缺陷原发路径；ClassReadOnly 即可读）
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/audit-logs?limit=50", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("audit-logs: %d %s", status, data)
	}
	if strings.Contains(string(data), leakedHashMarker) {
		t.Fatalf("审计日志泄露口令哈希:\n%s", data)
	}
	if !strings.Contains(string(data), "password-hash") {
		t.Fatalf("审计仍应记录「口令变更」这一事实（值脱敏）:\n%s", data)
	}

	// 2) GET /configuration/candidate（含 login 段时不得回显哈希）
	if status, _, _ := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		candidateWithUser(), nil); status != http.StatusOK {
		t.Fatalf("PUT candidate: %d", status)
	}
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/candidate", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET candidate: %d %s", status, data)
	}
	if strings.Contains(string(data), leakedHashMarker) || strings.Contains(string(data), "password_hash") {
		t.Fatalf("candidate 视图泄露口令哈希:\n%s", data)
	}

	// 3) GET /configuration/diff（JunOS 风格文本）
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/configuration/diff", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("diff: %d %s", status, data)
	}
	if strings.Contains(string(data), leakedHashMarker) {
		t.Fatalf("diff 文本泄露口令哈希:\n%s", data)
	}

	// 4) GET /system/login-users（列表端点）
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/login-users", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("login-users: %d %s", status, data)
	}
	if strings.Contains(string(data), leakedHashMarker) {
		t.Fatalf("login-users 泄露口令哈希:\n%s", data)
	}

	// 5) CLI show configuration / show log audit 直连路径：经事务引擎写入真实哈希
	x, eng := newCLIKit(t)
	seedCLIUserWithHash(t, x, eng)
	for _, line := range []string{"show configuration", "show log audit last 20"} {
		out := x.Execute("admin", aaa.ClassSuperUser, "ssh", line).Output
		if strings.Contains(out, leakedHashMarker) {
			t.Fatalf("CLI %q 泄露口令哈希:\n%s", line, out)
		}
	}

	// 6) 配置段出口**逐个**核实（2026-09-23 扩守护的重点）。
	//
	// 前提：committed 里必须真的存在一个哈希——否则这些端点回的都是空配置，
	// 断言全部落空（「空列表式假绿」，与 getList 恒跳过是同一类毛病）。
	// candidateWithUser 里那个用户的口令哈希就是哨兵值（sentinelHash）。
	if status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		candidateWithUser(), map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("提交含口令哈希的配置（哨兵）: %d %s", status, data)
	}
	// candidate / diff 两条出口要求本会话处于编辑态：直提是一次性事务、提交后不再持锁
	// （决策 #151），故按客户端流程再写一次候选（不带 Auto-Commit 头 = 纯候选写入）。
	if status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		candidateWithUser(), nil); status != http.StatusOK {
		t.Fatalf("写候选（编辑态）: %d %s", status, data)
	}
	// 每条出口都注明「它回的是哪一段配置」——新增配置出口时必须在此补一行；
	// 第 7 步的全路由扫描是兜底，不能替代这份清单（清单能发现"漏了哪一段"，
	// 扫描只能发现"这次回了哈希"）。
	for _, ep := range []struct{ path, why string }{
		{"/system", "system 段（含 login.users）——本轮修复点：脱敏前原样回显 password_hash"},
		{"/configuration", "整配置（committed）"},
		{"/configuration/candidate", "候选/committed 视图"},
		{"/configuration/history", "提交历史：有意只回元数据、不含配置正文"},
		{"/configuration/diff", "JunOS 风格差异文本：值经 model 渲染层打码"},
		{"/system/login-users", "用户与 class 列表：只投影 name/class"},
		{"/system/configuration/sessions", "持锁会话列表：无配置正文"},
		{"/resource-pools", "resource_pools 段"},
		{"/vpp/config", "vpp 段"},
		{"/acls", "acls 段"},
		{"/nat", "nat 段"},
		{"/qos/policies", "qos_policies 段"},
		{"/port-mirroring", "port_mirroring 段"},
		{"/bonds", "bonds 段"},
		{"/protocols/lldp", "protocols.lldp 段"},
		{"/system/kernel", "system.kernel 段（三方对照）"},
		{"/system/health/thresholds", "system.health 段"},
		{"/interfaces", "interfaces 段 + 运行态"},
		{"/virtual-switches", "virtual_switches 段"},
		{"/vrfs", "vrfs 段"},
		{"/virtual-machine-functions", "virtual_machine_functions 段"},
		{"/container-functions", "container_functions 段"},
		{"/cli/candidates", "CLI 动态候选：只回名字清单"},
	} {
		status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+ep.path, token, nil, nil)
		if status != http.StatusOK {
			t.Errorf("GET %s 应可读（%s）：状态 %d %s", ep.path, ep.why, status, data)
			continue
		}
		if body := string(data); strings.Contains(body, sentinelHash) || realHashRe.MatchString(body) {
			t.Errorf("GET %s 泄露口令哈希（%s）:\n%s", ep.path, ep.why, body)
		}
		if strings.Contains(string(data), "password_hash") {
			t.Errorf("GET %s 回了 password_hash 键（%s）:\n%s", ep.path, ep.why, data)
		}
	}
	// 脱敏 ≠ 掏空：非敏感字段必须还在，否则「没泄露」只是因为什么都没回（又一种假绿）。
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "sec-node") {
		t.Fatalf("GET /system 应回非敏感的 system 配置（hostname=sec-node）: %d %s", status, data)
	}

	// 7) 兜底：把 server.go 注册的**每一条 GET 路由**都打一遍，响应体里出现真哈希即失败。
	//    这一层不依赖人工清单——将来新增一个「直接回配置段」的端点（例如 GET /system/login），
	//    只要它把 committed 的 login.users 原样发出去，这里就红。
	//    路径参数统一填不存在的名字（这些路由本就该 404/503，回的是错误体）。
	//    已知且**有意**不在本扫描覆盖范围内的两条（如实登记，免得读者以为它们验过了）：
	//      · `GET /system/backup/{file}` 下载的备份归档**必然含**完整配置（含口令哈希）——
	//        归档要能恢复，脱敏就没法恢复；用占位文件名时它 404，故本扫描碰不到它。
	//      · `GET /events`（SSE 长连接，见下）。
	sweep := &http.Client{Timeout: 30 * time.Second}
	okCount := 0
	for _, rt := range registeredRoutes(t) {
		if rt.Method != http.MethodGet || strings.Contains(rt.Path, "{tail...}") {
			continue
		}
		// `/events` 是 SSE 长连接（按定义不会结束），本扫描跳过：它推的是事件元数据
		// （config-committed 只带 revision/user），不含配置正文，另有 events_test.go 覆盖。
		if rt.Path == "/events" {
			continue
		}
		p := rt.Path
		for _, ph := range []string{"{name}", "{file}", "{snapshot}", "{n}"} {
			p = strings.ReplaceAll(p, ph, "nosuch")
		}
		req, err := http.NewRequest(http.MethodGet, ts.URL+APIPrefix+p, nil)
		if err != nil {
			t.Fatalf("构造请求 GET %s: %v", p, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := sweep.Do(req)
		if err != nil {
			t.Errorf("GET %s: %v", p, err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			okCount++
		}
		if strings.Contains(string(body), sentinelHash) || realHashRe.MatchString(string(body)) {
			t.Errorf("GET %s 的响应含口令哈希（状态 %d）:\n%s", p, resp.StatusCode, body)
		}
	}
	if okCount < 10 {
		t.Errorf("全路由扫描只拿到 %d 个 200——扫描可能已失效（路由解析或占位替换出错），"+
			"此时上面的断言等于没跑", okCount)
	}
}

// candidateWithUser 构造含 login 用户（带哈希）的候选配置，用于验证视图脱敏。
// 哈希值取 sentinelHash：提交进 committed 后，第 6/7 步据此精确断言它没有出现在任何出口。
func candidateWithUser() map[string]any {
	return map[string]any{
		"system": map[string]any{
			"hostname": "sec-node",
			"login": map[string]any{
				"users": []map[string]any{
					{"name": "opssec", "class": "operator",
						"password_hash": sentinelHash},
				},
			},
		},
	}
}

// seedCLIUserWithHash 经事务引擎写入一个带真实哈希的用户，触发 CLI 侧审计与 show 路径
// （等价于 aaa 服务写入；不依赖口令交互）。
func seedCLIUserWithHash(t *testing.T, x *cliExecutor, eng *config.Engine) {
	t.Helper()
	sess := config.Session{User: "admin", Source: "ssh"}
	if err := eng.Edit(sess); err != nil {
		t.Fatalf("edit: %v", err)
	}
	cfg, _, err := eng.Candidate()
	if err != nil {
		t.Fatalf("candidate: %v", err)
	}
	if cfg.System == nil {
		cfg.System = &model.SystemConfig{}
	}
	if cfg.System.Login == nil {
		cfg.System.Login = &model.SystemLogin{}
	}
	cfg.System.Login.Users = append(cfg.System.Login.Users, model.LoginUserConfig{
		Name: "cliuser", Class: "operator",
		PasswordHash: "pbkdf2$sha256$600000$CLISALT$CLIHASH",
	})
	if err := eng.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("update candidate: %v", err)
	}
	if _, err := eng.Commit(context.Background(), sess, config.CommitOpts{Message: "seed"}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := eng.Release(sess); err != nil {
		t.Fatalf("release: %v", err)
	}
	_ = x
}

// 脱敏助手本身的直接单测：敏感键整个移除，非敏感键保留。
// 实现自决策 #149 起在 internal/model（api 的 redactView 只是它的转发），此处直接调实现。
func TestRedactConfigView(t *testing.T) {
	out, _ := json.Marshal(model.RedactSensitive(candidateWithUser()))
	s := string(out)
	if strings.Contains(s, "pbkdf2") || strings.Contains(s, "password_hash") {
		t.Fatalf("敏感字段未移除: %s", s)
	}
	for _, want := range []string{"opssec", "operator", "sec-node"} {
		if !strings.Contains(s, want) {
			t.Fatalf("非敏感字段 %q 不应被移除: %s", want, s)
		}
	}
}

// FR-API-007（决策 #70）：/audit-logs 的 offset 必须真正生效
// （契约早已声明 limit/offset 而实现静默忽略，属契约漂移）。
func TestAuditLogsOffsetEffective(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	// 产生两条审计（两次 commit）
	seedUserWithPassword(t, ts, token, "pg1")
	seedUserWithPassword(t, ts, token, "pg2")

	get := func(q string) []map[string]any {
		t.Helper()
		status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/audit-logs"+q, token, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("audit-logs%s: %d %s", q, status, data)
		}
		var rows []map[string]any
		if err := json.Unmarshal(data, &rows); err != nil {
			t.Fatalf("解析: %v", err)
		}
		return rows
	}
	page0 := get("?limit=1&offset=0")
	page1 := get("?limit=1&offset=1")
	if len(page0) != 1 || len(page1) != 1 {
		t.Fatalf("分页大小错误: %d %d", len(page0), len(page1))
	}
	if page0[0]["timestamp"] == page1[0]["timestamp"] {
		t.Fatalf("offset 未生效：两页返回同一条（%v）", page0[0])
	}
}
