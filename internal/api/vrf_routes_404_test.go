package api

// R51-1 收口（决策 #135）：`GET /vrfs/{name}/routes` 对**不在 committed 中**的 VRF 返回 404，
// 而不是静默回空数组——否则「VRF 不存在」会被显示成「无路由」（与 CLI 决策 #76④ 的口径一致）。

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestVrfRoutesUnknownVrfIs404(t *testing.T) {
	ts := newTestServerOpts(t, Options{L3: fakeL3{}})
	token := loginAdmin(t, ts)

	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/vrfs/no-such-vrf/routes", token, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("不存在的 VRF 应 404，得到 %d %s", status, data)
	}
	if !strings.Contains(string(data), "不存在") {
		t.Fatalf("404 应说明 VRF 不存在: %s", data)
	}
}

// fakeL3 最小 L3 运行态（Routes 返回空表，与 VPP 对未知 VRF 的行为一致）。
type fakeL3 struct{}

func (fakeL3) Routes(context.Context, string) ([]RouteRow, error) { return nil, nil }
