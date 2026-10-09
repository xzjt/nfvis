package api

// 决策 #432：容器读视图的 `restart_count`（Docker `State.RestartCount`）三面同源。
//
// 口径（规格 #432）：
//   - 状态读数把 Docker 的 `restarting` 并入 `running`（规格 #44，**不改**），故崩溃重启循环
//     只能靠重启次数与告警看见；
//   - 次数**取不到就省略**（容器不存在 / 编排未接入 / 查询失败 / 底座应答未给该字段），
//     **不得**回落为 0——「读不到」与「已重启 0 次」是两件事（0 次要照实发 0）。
//
// 红-绿：把读视图改回不取该字段（或取不到时回落 0 / 总是发 0）时，下列断言按预期失败。

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

// TestCLIContainerReadViewRestartCount CLI 详情读视图（`show container-functions <名> [detail]`）：
// 有值 ⇒ 渲染 `restart-count <n>`（`| display json` 同源给出 `restart_count`）；
// 取不到 ⇒ 该字段整体省略（不出现 0）。
func TestCLIContainerReadViewRestartCount(t *testing.T) {
	x, _ := newCLIKit(t)
	seedContainerConfig(t, x)
	ctRT := newFakeCLIContainer()
	ctRT.states["sbc-ct1"] = "running"
	ctRT.restarts["sbc-ct1"] = 5
	x.setComputeRuntime(nil, nil, nil, ctRT, nil)

	for _, line := range []string{
		"show container-functions sbc-ct1",
		"show container-functions sbc-ct1 detail",
	} {
		res := x.Execute("admin", aaa.ClassSuperUser, "ssh", line)
		if strings.Contains(res.Output, "%%") {
			t.Fatalf("%s 应成功: %s", line, res.Output)
		}
		if !strings.Contains(res.Output, "restart-count 5") {
			t.Fatalf("%s 应渲染重启次数（restart-count 5）:\n%s", line, res.Output)
		}
	}
	// `| display json` 与渲染同源（字段名按契约 = restart_count）。
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show container-functions sbc-ct1 | display json")
	if !strings.Contains(res.Output, `"restart_count": 5`) {
		t.Fatalf("display json 应给出 restart_count 与渲染同源:\n%s", res.Output)
	}

	// 取不到：应答里没有该字段 ⇒ 字段整体省略（**不**出现 0）。
	unknown := newFakeCLIContainer()
	unknown.states["sbc-ct1"] = "running"
	unknown.restartUnknown["sbc-ct1"] = true
	x.setComputeRuntime(nil, nil, nil, unknown, nil)
	for _, line := range []string{
		"show container-functions sbc-ct1 detail",
		"show container-functions sbc-ct1 detail | display json",
	} {
		res := x.Execute("admin", aaa.ClassSuperUser, "ssh", line)
		if strings.Contains(res.Output, "restart-count") || strings.Contains(res.Output, "restart_count") {
			t.Fatalf("取不到重启次数时不得给出该字段（更不得给 0）: %s\n%s", line, res.Output)
		}
	}

	// 读数失败（底座不可达）：状态如实降级为 "-"，次数同样省略。
	down := newFakeCLIContainer()
	down.err = errors.New("docker down")
	x.setComputeRuntime(nil, nil, nil, down, nil)
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show container-functions sbc-ct1 detail")
	if strings.Contains(res.Output, "restart-count") {
		t.Fatalf("读数失败时不得给出重启次数: %s", res.Output)
	}
	if !strings.Contains(res.Output, "state -") {
		t.Fatalf("读数失败时状态应如实降级为 \"-\": %s", res.Output)
	}

	// 已重启 0 次是**真实读数**：必须照实发 0（不能与「取不到」混为一谈）。
	zero := newFakeCLIContainer()
	zero.states["sbc-ct1"] = "running"
	x.setComputeRuntime(nil, nil, nil, zero, nil)
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show container-functions sbc-ct1 detail")
	if !strings.Contains(res.Output, "restart-count 0") {
		t.Fatalf("已重启 0 次应照实给出 0:\n%s", res.Output)
	}
}

// TestRESTContainerRestartCountEndpoint REST 读视图：GET 详情与列表都带 `restart_count`；
// 取不到时字段**不出现**（不是 0）；编排未接入时同样不出现且不报错。
func TestRESTContainerRestartCountEndpoint(t *testing.T) {
	ct := newFakeCLIContainer()
	ct.states["ct-a"] = "running"
	ct.restarts["ct-a"] = 7
	ts := newTestServerOpts(t, Options{Containers: ct})
	token := loginAdmin(t, ts)
	seedContainerCommit(t, ts, token, "ct-a")

	get := func(path string) map[string]any {
		t.Helper()
		status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+path, token, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, status, body)
		}
		return responseObject(t, body)
	}

	detail := get("/container-functions/ct-a")
	if v, ok := detail["restart_count"]; !ok {
		t.Fatalf("详情响应应带 restart_count: %v", detail)
	} else if n, ok := v.(float64); !ok || n != 7 {
		t.Fatalf("restart_count 应为 7，得 %v", v)
	}
	if detail["state"] != "running" {
		t.Fatalf("state 应仍为既有口径 running: %v", detail["state"])
	}
	list := get("/container-functions")
	if n, ok := list["restart_count"].(float64); !ok || n != 7 {
		t.Fatalf("列表元素应带同一读数，得 %v", list["restart_count"])
	}

	// 取不到 ⇒ 字段不出现（omitempty）。
	unknown := newFakeCLIContainer()
	unknown.states["ct-a"] = "running"
	unknown.restartUnknown["ct-a"] = true
	ts2 := newTestServerOpts(t, Options{Containers: unknown})
	token2 := loginAdmin(t, ts2)
	seedContainerCommit(t, ts2, token2, "ct-a")
	getRaw := func(ts string, tok string) map[string]any {
		t.Helper()
		status, _, body := cfgRequest(t, http.MethodGet, ts+APIPrefix+"/container-functions/ct-a", tok, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("GET 详情: %d %s", status, body)
		}
		var obj map[string]any // 每次新建：Unmarshal 进已有 map 是**合并**，会留下上一条响应的键
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatal(err)
		}
		return obj
	}
	raw := getRaw(ts2.URL, token2)
	if _, ok := raw["restart_count"]; ok {
		t.Fatalf("取不到重启次数时字段应省略（不得给 0）: %v", raw)
	}

	// 编排未接入（未注入 Containers）：状态与次数都省略，读视图照常 200。
	ts3 := newTestServerOpts(t, Options{})
	token3 := loginAdmin(t, ts3)
	seedContainerCommit(t, ts3, token3, "ct-a")
	raw = getRaw(ts3.URL, token3)
	if _, ok := raw["state"]; ok {
		t.Fatalf("编排未接入时不应有 state: %v", raw)
	}
	if _, ok := raw["restart_count"]; ok {
		t.Fatalf("编排未接入时不应有 restart_count: %v", raw)
	}
}
