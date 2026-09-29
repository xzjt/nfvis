package api

// R88-4 回归：/system/status 的 hostname 必须「配置优先、否则运行态主机名」。
//
// 真机 round88 现场（1.1.49 全新安装）：没人设过 `set system hostname`，于是该字段恒为空串，
// 控制台总览「系统 → 主机名」永远显示「—」——而机器实际有主机名（`hostname` = nfvis）。
// 面板承诺了「主机名」却答不出来，属"读视图缺字段/答非所问"类缺口（与 #189 接口 MTU 同族：
// 配置显式值优先、否则取运行态，两者都无才缺席）。
//
// 反方向也要防：配置里**显式声明**过 hostname 时必须用配置值（不能被内核主机名盖掉）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// statusHostname 读一次 /system/status 的 hostname 字段。
func statusHostname(t *testing.T, ts *httptest.Server, token string) string {
	t.Helper()
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/status", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /system/status: %d %s", status, body)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("响应不是对象: %v %s", err, body)
	}
	h, _ := got["hostname"].(string)
	return h
}

func TestSystemStatusHostnameFallsBackToHostname(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	want, err := os.Hostname()
	if err != nil || want == "" {
		t.Skipf("本环境取不到 os.Hostname（%v）：该用例只在本机有主机名时有意义", err)
	}
	if got := statusHostname(t, ts, token); got != want {
		t.Fatalf("未声明 hostname 时应回退运行态主机名 %q，实际 %q（旧实现恒为空串 → 控制台显示「—」）", want, got)
	}
}

func TestSystemStatusHostnamePrefersDeclaredValue(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)

	const declared = "nfvis-declared"
	status, _, body := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/configuration/candidate", token,
		map[string]any{"system": map[string]any{"hostname": declared,
			"login": map[string]any{"users": []map[string]any{superUserDoc()}}}},
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusOK {
		t.Fatalf("提交 hostname: %d %s", status, body)
	}
	if got := statusHostname(t, ts, token); got != declared {
		t.Fatalf("配置声明优先：期望 %q，实际 %q", declared, got)
	}
}
