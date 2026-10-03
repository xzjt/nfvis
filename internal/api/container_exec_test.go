package api

// 决策 #357：容器内执行命令（`POST /container-functions/{name}:exec`）的端点契约回归。
//
// 如实口径（与决策行一致）：命令跑完（哪怕非 0 退出码）⇒ 200，退出码是**结果**不是失败；
// 超时 ⇒ 200 + `timed_out` 且**不带** `exit_code`（未知 ≠ 0）；不存在 ⇒ 404、非运行 ⇒ 409、
// 参数非法 ⇒ 400、编排未接入 ⇒ 503。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/container"
)

// execFakeCT 只实现本用例需要的部分：ContainerRuntime 的其余方法交给内嵌 fake。
type execFakeCT struct {
	*fakeCLIContainer
	execRes container.ExecResult
	execErr error
}

func (f *execFakeCT) ContainerExec(context.Context, string, string, time.Duration) (container.ExecResult, error) {
	return f.execRes, f.execErr
}

// seedContainerCommit 提交一份含单个容器的配置（端点前置：容器须在 committed 里）。
func seedContainerCommit(t *testing.T, ts *httptest.Server, token, name string) {
	t.Helper()
	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		map[string]any{
			"system":              map[string]any{"login": map[string]any{"users": []map[string]any{superUserDoc()}}},
			"container_functions": []map[string]any{{"name": name, "image": "alpine:3.20"}},
		},
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("提交容器配置: %d %s", status, body)
	}
}

func TestContainerExecEndpoint(t *testing.T) {
	ct := &execFakeCT{fakeCLIContainer: newFakeCLIContainer()}
	ts := newTestServerOpts(t, Options{Containers: ct})
	token := loginAdmin(t, ts)
	seedContainerCommit(t, ts, token, "ct-a")

	post := func(body map[string]any) (int, map[string]any) {
		status, _, raw := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/container-functions/ct-a:exec", token, body, nil)
		out := map[string]any{}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &out)
		}
		return status, out
	}

	// ① 正常跑完（非 0 退出码也是**成功**：退出码是结果）
	ct.execRes = container.ExecResult{ExitCode: 7, HasExitCode: true, Stdout: "out\n", Stderr: "err\n", Duration: 12 * time.Millisecond}
	status, out := post(map[string]any{"command": "echo hi"})
	if status != http.StatusOK {
		t.Fatalf("正常执行应 200，得 %d %v", status, out)
	}
	if out["exit_code"] != float64(7) || out["stdout"] != "out\n" || out["stderr"] != "err\n" {
		t.Fatalf("响应形状不符契约: %v", out)
	}
	if _, has := out["timed_out"]; has {
		t.Fatalf("未超时不该出现 timed_out: %v", out)
	}

	// ② 超时：200 + timed_out，且**不得**出现 exit_code（未知 ≠ 0）
	ct.execRes = container.ExecResult{Stdout: "partial", TimedOut: true, Duration: time.Second}
	status, out = post(map[string]any{"command": "sleep 60", "timeout_seconds": 1})
	if status != http.StatusOK || out["timed_out"] != true {
		t.Fatalf("超时应 200 + timed_out，得 %d %v", status, out)
	}
	if _, has := out["exit_code"]; has {
		t.Fatalf("超时时不得报退出码（未知 ≠ 0）: %v", out)
	}

	// ③ 截断如实上报
	ct.execRes = container.ExecResult{ExitCode: 0, HasExitCode: true, Stdout: "x", Truncated: true}
	_, out = post(map[string]any{"command": "yes"})
	if out["truncated"] != true {
		t.Fatalf("被裁剪应置 truncated: %v", out)
	}

	// ④ 非运行态 ⇒ 409（provider 判定，不靠字符串匹配）
	ct.execErr = orchestrator.ErrContainerNotRunning
	status, out = post(map[string]any{"command": "echo hi"})
	if status != http.StatusConflict {
		t.Fatalf("非运行态应 409，得 %d %v", status, out)
	}
	if msg, _ := out["message"].(string); msg == "" {
		t.Fatalf("409 应带可照做的说明: %v", out)
	}

	// ⑤ 容器不存在（provider 侧）⇒ 404
	ct.execErr = orchestrator.ErrVMNotFound
	if status, _ = post(map[string]any{"command": "echo hi"}); status != http.StatusNotFound {
		t.Fatalf("provider 报不存在应 404，得 %d", status)
	}

	// ⑥ 参数非法 ⇒ 400（空命令；超时越界）。0 = 缺省 30s，是**合法**取值。
	ct.execErr = nil
	if status, _ = post(map[string]any{"command": "   "}); status != http.StatusBadRequest {
		t.Fatalf("空命令应 400，得 %d", status)
	}
	for _, bad := range []map[string]any{
		{"command": "x", "timeout_seconds": -1},
		{"command": "x", "timeout_seconds": 301},
	} {
		if status, _ = post(bad); status != http.StatusBadRequest {
			t.Fatalf("超时越界应 400: %v", bad)
		}
	}
	ct.execRes = container.ExecResult{ExitCode: 0, HasExitCode: true}
	if status, _ = post(map[string]any{"command": "x", "timeout_seconds": 0}); status != http.StatusOK {
		t.Fatalf("timeout_seconds=0（缺省）应被接受，得 %d", status)
	}

	// ⑦ 配置里没有的容器 ⇒ 404（不落到编排层）
	if status, _ = post(map[string]any{"command": "echo hi"}); status != http.StatusOK {
		t.Fatalf("ct-a 仍在配置里，应 200，得 %d", status)
	}
	status, _, _ = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/container-functions/ct-nope:exec", token,
		map[string]any{"command": "echo hi"}, nil)
	if status != http.StatusNotFound {
		t.Fatalf("未声明的容器应 404，得 %d", status)
	}
}

// 编排未接入（Containers 为 nil）⇒ 503，且**不**谎报成功。
func TestContainerExecUnavailable(t *testing.T) {
	ts := newTestServerOpts(t, Options{})
	token := loginAdmin(t, ts)
	seedContainerCommit(t, ts, token, "ct-a")
	status, _, _ := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/container-functions/ct-a:exec", token,
		map[string]any{"command": "echo hi"}, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("编排未接入应 503，得 %d", status)
	}
}

// 决策 #357：CLI 侧渲染与「退出码是结果不是失败」的口径。
func TestCLIContainerExecRender(t *testing.T) {
	x, engine := newCLIKit(t)
	seedContainerConfig(t, x)
	ct := newFakeCLIContainer()
	x.setComputeRuntime(nil, nil, nil, ct, nil)

	// ① 正常跑完：输出两块 + 退出码；命令原文与超时透传
	ct.execResult = container.ExecResult{ExitCode: 0, HasExitCode: true, Stdout: "hello\n", Duration: 5 * time.Millisecond}
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", `request container-functions sbc-ct1 exec "echo hello"`)
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("正常执行不该报失败: %s", res.Output)
	}
	for _, want := range []string{"退出码: 0", "--- stdout ---", "hello", "--- stderr ---", "（无输出）"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("输出缺 %q：\n%s", want, res.Output)
		}
	}
	if ct.execCommand != "echo hello" {
		t.Fatalf("引号应被词法器剥掉，命令原文得 %q", ct.execCommand)
	}
	if ct.execTimeout != 30*time.Second {
		t.Fatalf("缺省超时应 30s，得 %v", ct.execTimeout)
	}
	if !auditHas(t, engine, "container.exec") {
		t.Fatal("exec 应入审计")
	}

	// ② 非 0 退出码仍是**成功**（退出码是结果）：不出现 %%
	ct.execResult = container.ExecResult{ExitCode: 127, HasExitCode: true, Stderr: "sh: nope: not found\n"}
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", `request container-functions sbc-ct1 exec "nope"`)
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("命令跑完（哪怕非 0）不该报失败: %s", res.Output)
	}
	if !strings.Contains(res.Output, "退出码: 127") {
		t.Fatalf("应如实回显退出码: %s", res.Output)
	}

	// ③ 超时：报失败且**不报**退出码（未知 ≠ 0）
	ct.execResult = container.ExecResult{Stdout: "partial", TimedOut: true, Duration: 3 * time.Second}
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", `request container-functions sbc-ct1 exec "sleep 60" timeout 3`)
	if !strings.Contains(res.Output, "%%") || !strings.Contains(res.Output, "未结束") {
		t.Fatalf("超时应报失败并说明: %s", res.Output)
	}
	if strings.Contains(res.Output, "退出码: 0") || strings.Contains(res.Output, "退出码: ") {
		t.Fatalf("超时时不得报退出码: %s", res.Output)
	}
	if !strings.Contains(res.Output, "partial") {
		t.Fatalf("超时应保留已读到的输出: %s", res.Output)
	}
	if ct.execTimeout != 3*time.Second {
		t.Fatalf("timeout 3 应透传，得 %v", ct.execTimeout)
	}

	// ④ 截断如实标注
	ct.execResult = container.ExecResult{ExitCode: 0, HasExitCode: true, Stdout: "x", Truncated: true}
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", `request container-functions sbc-ct1 exec "yes"`)
	if !strings.Contains(res.Output, "已截断") {
		t.Fatalf("截断应如实标注: %s", res.Output)
	}

	// ⑤ 忘加引号 ⇒ 给可照做的提示（不拼起来猜）
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", `request container-functions sbc-ct1 exec ip addr`)
	if !strings.Contains(res.Output, "含空格请加引号") {
		t.Fatalf("多余 token 应提示加引号: %s", res.Output)
	}

	// ⑥ 超时越界 ⇒ 拒绝（不静默夹取）
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", `request container-functions sbc-ct1 exec "x" timeout 999`)
	if !strings.Contains(res.Output, "1..300") {
		t.Fatalf("超时越界应报范围: %s", res.Output)
	}

	// ⑦ operator（S 档）不得执行
	res = x.Execute("bob", aaa.ClassOperator, "ssh", `request container-functions sbc-ct1 exec "id"`)
	if !strings.Contains(res.Output, "无权限") {
		t.Fatalf("operator 执行 exec 应被拒（S 档）: %s", res.Output)
	}
}
