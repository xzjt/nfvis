package api

// 决策 #358：容器交互式终端的端点与凭证语义回归。
//
// 三条硬口径：
//  1. **凭证按资源键隔离**——VM 串口的 ticket 开不了容器 shell，反之亦然（同一张表、不同前缀）；
//  2. 一次性 + 过期（与串口同源，本用例把「一次」钉住）；
//  3. 前置：容器须运行中（409 并指向 start）；未接入 ⇒ 503；容器不存在 ⇒ 404。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// 凭证按资源键隔离：用 VM 的资源键签发的 ticket，在容器 shell 的 WS 上必须无效（反之亦然）。
func TestConsoleTicketResourceIsolation(t *testing.T) {
	tix := newConsoleTickets()
	vmTok, _, err := tix.issue(vmConsoleResource("vm-a"), "admin")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	ctTok, _, err := tix.issue(ctShellResource("ct-a"), "admin")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// ① 同类可用
	if _, ok := tix.consume(ctShellResource("ct-a"), ctTok); !ok {
		t.Fatal("容器 shell 的 ticket 应在容器 shell 上可用")
	}
	// ② 跨类不可用（容器 shell 的 WS 拿 VM 的 ticket ⇒ 无效）
	if _, ok := tix.consume(ctShellResource("vm-a"), vmTok); ok {
		t.Fatal("VM 串口的 ticket 不该能开容器 shell")
	}
	// ③ 同名不同类也不通用（名字相同、种类不同）
	same, _, _ := tix.issue(ctShellResource("x"), "admin")
	if _, ok := tix.consume(vmConsoleResource("x"), same); ok {
		t.Fatal("同名资源的两类 ticket 不该通用")
	}
	// ④ 一次性：上面 ① 已消费过 ct-a 的票
	if _, ok := tix.consume(ctShellResource("ct-a"), ctTok); ok {
		t.Fatal("ticket 应是一次性的（消费即失效）")
	}
	// ⑤ 过期即无效
	expTok, _, _ := tix.issue(ctShellResource("ct-b"), "admin")
	tix.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, ok := tix.consume(ctShellResource("ct-b"), expTok); ok {
		t.Fatal("过期 ticket 应无效")
	}
}

func TestContainerShellTicketEndpoint(t *testing.T) {
	ct := &execFakeCT{fakeCLIContainer: newFakeCLIContainer()}
	ts := newTestServerOpts(t, Options{Containers: ct})
	token := loginAdmin(t, ts)
	seedContainerCommit(t, ts, token, "ct-a")

	post := func(name string) (int, map[string]any) {
		status, _, raw := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/container-functions/"+name+"/shell", token, nil, nil)
		out := map[string]any{}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &out)
		}
		return status, out
	}

	// ① 运行中 ⇒ 200 + ws_url（带一次性 ticket）
	ct.states["ct-a"] = orchestrator.CTStateRunning
	status, out := post("ct-a")
	if status != http.StatusOK {
		t.Fatalf("运行中应 200，得 %d %v", status, out)
	}
	ws, _ := out["ws_url"].(string)
	if !strings.Contains(ws, "/container-functions/ct-a/shell/ws?ticket=") {
		t.Fatalf("ws_url 形状不符契约: %q", ws)
	}
	if out["expires_in"] == nil {
		t.Fatalf("应回 expires_in: %v", out)
	}

	// ② 未运行 ⇒ 409 且指向 start
	ct.states["ct-a"] = orchestrator.CTStateExited
	status, out = post("ct-a")
	if status != http.StatusConflict {
		t.Fatalf("非运行态应 409，得 %d %v", status, out)
	}
	if msg, _ := out["message"].(string); !strings.Contains(msg, "先 request container-functions ct-a start") {
		t.Fatalf("409 应指向 start: %q", msg)
	}

	// ③ 容器不存在（不在配置里）⇒ 404
	if status, _ = post("ct-nope"); status != http.StatusNotFound {
		t.Fatalf("未声明的容器应 404，得 %d", status)
	}

	// ④ 编排未接入 ⇒ 503
	ts2 := newTestServerOpts(t, Options{})
	token2 := loginAdmin(t, ts2)
	seedContainerCommit(t, ts2, token2, "ct-a")
	status, _, _ = cfgRequest(t, http.MethodPost, ts2.URL+APIPrefix+"/container-functions/ct-a/shell", token2, nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("编排未接入应 503，得 %d", status)
	}
}

// WS 端点：无 ticket / 错 ticket ⇒ 401（不升级）。
func TestContainerShellWSRejectsBadTicket(t *testing.T) {
	ct := &execFakeCT{fakeCLIContainer: newFakeCLIContainer()}
	ts := newTestServerOpts(t, Options{Containers: ct})
	token := loginAdmin(t, ts)
	seedContainerCommit(t, ts, token, "ct-a")
	ct.states["ct-a"] = orchestrator.CTStateRunning

	for _, q := range []string{"", "?ticket=deadbeef"} {
		status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/container-functions/ct-a/shell/ws"+q, token, nil, nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("无效 ticket 应 401，得 %d（q=%q）", status, q)
		}
	}
}

// CLI：`shell` 动作产出 ConsoleRequest（Kind=container）且为 S 档；前置不满足时给可照做的提示。
func TestCLIContainerShellDispatch(t *testing.T) {
	x, engine := newCLIKit(t)
	seedContainerConfig(t, x)
	ct := newFakeCLIContainer()
	ct.states["sbc-ct1"] = orchestrator.CTStateRunning
	ct.shell = &shellStream{}
	x.setComputeRuntime(nil, nil, nil, ct, nil)

	var gotName string
	x.issueShell = func(name, user string) (string, int, error) {
		gotName = name
		return "/container-functions/" + name + "/shell/ws?ticket=tok", 60, nil
	}

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request container-functions sbc-ct1 shell")
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("运行中应受理: %s", res.Output)
	}
	if res.Console == nil || res.Console.Kind != "container" || !strings.Contains(res.Console.WSURL, "/shell/ws") {
		t.Fatalf("应产出容器终端接管请求（Kind=container）: %+v", res.Console)
	}
	if gotName != "sbc-ct1" {
		t.Fatalf("签发凭证的资源名错: %q", gotName)
	}
	if !auditHas(t, engine, "container.shell") {
		t.Fatal("打开容器终端应入审计")
	}

	// 停机 ⇒ 拒绝并指向 start（不签发凭证）
	ct.states["sbc-ct1"] = orchestrator.CTStateExited
	gotName = ""
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "request container-functions sbc-ct1 shell")
	if !strings.Contains(res.Output, "未处于运行态") || !strings.Contains(res.Output, "start") {
		t.Fatalf("停机应拒绝并指向 start: %s", res.Output)
	}
	if gotName != "" {
		t.Fatal("停机时不该签发凭证")
	}

	// operator（S 档）不得执行
	res = x.Execute("bob", aaa.ClassOperator, "ssh", "request container-functions sbc-ct1 shell")
	if !strings.Contains(res.Output, "无权限") {
		t.Fatalf("operator 应被拒（S 档）: %s", res.Output)
	}

	// 多余 token ⇒ 明确拒绝（树校验给「无效命令」+ 可用形态；不得静默忽略多余 token）
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "request container-functions sbc-ct1 shell extra")
	if !strings.Contains(res.Output, "无效命令") && !strings.Contains(res.Output, "语法") {
		t.Fatalf("多余 token 应被拒并给可用形态: %s", res.Output)
	}
}
