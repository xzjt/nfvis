package api

// 决策 #324：`GET /cli/candidates` 按**调用会话的 class** 过滤，且与服务端运行期授权同源。
//
// 判据分三层：
//   ① operator 会话的候选**不含** super-user-only 入口（逐条断言 reboot/shutdown/zeroize/vpp/configure…）；
//   ② read-only 只读（request/configure 整族不可见）；
//   ③ super-user 不变（reboot 等可见）；
//   ④ **同源**——服务端过滤结果 == 用 schema.PresetClassLevel + Covers 对同一棵树做的客户端过滤，
//      即"服务端判定与客户端呈现"是同一套等级（不出现两套权限模型）。
// 另断言纵深防御：候选里不列出不代表能执行——operator 直发 `request system reboot` 仍被拒。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/schema"
)

// cliCandidateTokens 取某 token 位置上的候选 token 集合（服务端已按调用者 class 过滤）。
func cliCandidateTokens(t *testing.T, ts *httptest.Server, token, tokens string) []string {
	t.Helper()
	u := ts.URL + APIPrefix + "/cli/candidates?tokens=" + url.QueryEscape(tokens) + "&partial="
	status, body := getWithToken(t, u, token)
	if status != http.StatusOK {
		t.Fatalf("GET /cli/candidates(%q) 应 200: %d %s", tokens, status, body)
	}
	var cs []schema.Candidate
	if err := json.Unmarshal(body, &cs); err != nil {
		t.Fatalf("解析候选: %v（%s）", err, body)
	}
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Token)
	}
	return out
}

func hasTok(toks []string, want string) bool {
	for _, t := range toks {
		if t == want {
			return true
		}
	}
	return false
}

// mkOperatorUser 建一个 operator 用户并返回其 token（服务端权威 class = operator）。
func mkOperatorUser(t *testing.T, ts *httptest.Server, adminToken, name string) string {
	t.Helper()
	status, _, body := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/login-users", adminToken,
		map[string]any{"name": name, "kind": "user", "password": "Net0p-Passw0rd!", "class": "operator"}, nil)
	if status != http.StatusCreated {
		t.Fatalf("建 operator 用户: %d %s", status, body)
	}
	st, resp := login(t, ts, name, "Net0p-Passw0rd!")
	if st != http.StatusOK || resp.Token == "" {
		t.Fatalf("operator 登录失败: %d %+v", st, resp)
	}
	return resp.Token
}

func TestCLICandidatesFilteredByClassOperator(t *testing.T) {
	ts := newTestServer(t)
	admin := loginAdmin(t, ts)
	op := mkOperatorUser(t, ts, admin, "netop")

	// operator：request system 下只留 O 级子域，super-user 级的全部不列。
	sys := cliCandidateTokens(t, ts, op, "request,system")
	for _, absent := range []string{"reboot", "shutdown", "poweroff", "zeroize", "software", "kernel", "configuration", "ssh", "storage"} {
		if hasTok(sys, absent) {
			t.Errorf("operator 的 `request system ?` 不应列出 super-user-only 入口 %q：%v", absent, sys)
		}
	}
	for _, present := range []string{"tech-support", "core-dumps", "ntp", "api", "password"} {
		if !hasTok(sys, present) {
			t.Errorf("operator 的 `request system ?` 应保留 O 级入口 %q：%v", present, sys)
		}
	}

	// request 域：vpp（Su 整族）不列；images/virtual-machine-functions/system 保留。
	req := cliCandidateTokens(t, ts, op, "request")
	if hasTok(req, "vpp") {
		t.Errorf("operator 的 `request ?` 不应列出 super-user-only 的 vpp：%v", req)
	}
	for _, present := range []string{"images", "virtual-machine-functions", "container-functions", "system"} {
		if !hasTok(req, present) {
			t.Errorf("operator 的 `request ?` 应保留 %q：%v", present, req)
		}
	}

	// 顶层：configure / clear / start 是 S，operator 不可见；show / request / ping 可见
	// （wizard 与 monitor 是 O 级，operator 仍可见）。
	top := cliCandidateTokens(t, ts, op, "")
	for _, absent := range []string{"configure", "clear", "start"} {
		if hasTok(top, absent) {
			t.Errorf("operator 顶层候选不应含 super-user-only 的 %q：%v", absent, top)
		}
	}
	for _, present := range []string{"show", "request", "ping", "traceroute", "wizard", "monitor", "help"} {
		if !hasTok(top, present) {
			t.Errorf("operator 顶层候选应含 %q：%v", present, top)
		}
	}
}

func TestCLICandidatesFilteredByClassReadOnly(t *testing.T) {
	ts := newTestServer(t)
	_, vresp := login(t, ts, "viewer", "s3cret-Passw0rd!")
	top := cliCandidateTokens(t, ts, vresp.Token, "")
	for _, absent := range []string{"configure", "request", "ping", "clear", "start", "monitor", "wizard"} {
		if hasTok(top, absent) {
			t.Errorf("read-only 顶层候选不应含 %q（只读账号只能 show）：%v", absent, top)
		}
	}
	if !hasTok(top, "show") {
		t.Errorf("read-only 顶层候选应含 show：%v", top)
	}
}

func TestCLICandidatesFilteredByClassSuperUserUnchanged(t *testing.T) {
	ts := newTestServer(t)
	admin := loginAdmin(t, ts)
	sys := cliCandidateTokens(t, ts, admin, "request,system")
	for _, present := range []string{"reboot", "shutdown", "zeroize", "software", "kernel", "configuration"} {
		if !hasTok(sys, present) {
			t.Errorf("super-user 的 `request system ?` 应含 %q（不应被过滤）：%v", present, sys)
		}
	}
}

// TestCLICandidatesServerClientSameSource 断言「服务端判定 == 客户端呈现同源」：
// 服务端按 aaa.Authorize 过滤；客户端按 schema.PresetClassLevel(operator)+Covers 过滤同一棵树。
// 两者的候选集合必须逐条一致——否则就是两套权限模型（决策 #324 的核心约束）。
func TestCLICandidatesServerClientSameSource(t *testing.T) {
	ts := newTestServer(t)
	admin := loginAdmin(t, ts)
	op := mkOperatorUser(t, ts, admin, "netop2")

	lvl, ok := schema.PresetClassLevel("operator")
	if !ok {
		t.Fatal("schema.PresetClassLevel(operator) 应命中")
	}
	for _, tokens := range []string{"", "request", "request,system", "show", "show configuration"} {
		server := cliCandidateTokens(t, ts, op, tokens)
		client := candidateTokensClientSide(lvl, tokens)
		if strings.Join(server, ",") != strings.Join(client, ",") {
			t.Errorf("位置 %q 的候选在服务端与客户端不一致（两套权限模型？）：\n  服务端 %v\n  客户端 %v",
				tokens, server, client)
		}
	}
}

// candidateTokensClientSide 复刻 CLI 前端的本地过滤：同一棵树 + 同一等级映射（不读服务端）。
func candidateTokensClientSide(lvl schema.Class, tokens string) []string {
	var toks []string
	if tokens != "" {
		toks = strings.Split(tokens, ",")
	}
	cs := schema.CandidatesFiltered(schema.OperRoot(), toks, "", nil,
		func(_ []string, n *schema.Node) bool { return lvl.Covers(n.RequiredClass()) })
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Token)
	}
	return out
}

// TestOperatorStillDeniedByExecutor 纵深防御：候选不列出，不代表能执行——执行路径的既有拒绝不变。
func TestOperatorStillDeniedByExecutor(t *testing.T) {
	ts := newTestServer(t)
	admin := loginAdmin(t, ts)
	op := mkOperatorUser(t, ts, admin, "netop3")
	status, body := postJSON(t, ts.URL+APIPrefix+"/cli/execute", map[string]string{
		"line": "request system reboot", "source": "ssh",
	}, map[string]string{"Authorization": "Bearer " + op})
	if status != http.StatusOK {
		t.Fatalf("执行请求应 200（错误在输出里）: %d %s", status, body)
	}
	var res struct {
		Output string `json:"output"`
	}
	_ = json.Unmarshal(body, &res)
	if !strings.Contains(res.Output, "无权限") {
		t.Fatalf("operator 直发 super-user 命令仍应被拒（纵深防御）: %q", res.Output)
	}
}
