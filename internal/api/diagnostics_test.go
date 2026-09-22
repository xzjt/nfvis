package api

// 诊断视图的 REST 出口（决策 #123）：日志 / ping / traceroute / 清零统计。
// 判定口径必须与 CLI 一致——尤其 ping 的「未通即失败」（附录 A #89/#93）。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeDiag 诊断运行时的测试替身。
type fakeDiag struct {
	pingOut string
	pingErr error
	trOut   string
	trErr   error
	cleared string
	clrErr  error
}

func (f *fakeDiag) Ping(context.Context, string, string, string, int) (string, error) {
	return f.pingOut, f.pingErr
}
func (f *fakeDiag) Traceroute(context.Context, string, string) (string, error) {
	return f.trOut, f.trErr
}
func (f *fakeDiag) ClearInterfaceStats(_ context.Context, ifname string) error {
	f.cleared = ifname
	return f.clrErr
}

func diagServer(t *testing.T, d DiagRuntime) *httptest.Server {
	t.Helper()
	return newTestServerOpts(t, Options{Diag: d, LogSource: func() ([]byte, error) {
		return []byte("line-1\nline-2\nline-3\n"), nil
	}})
}

// 日志端点：文本原样返回；?last=n 只取末尾 n 行；未接入 → 503。
func TestSystemLogsEndpoint(t *testing.T) {
	ts := diagServer(t, &fakeDiag{})
	token := loginAdmin(t, ts)

	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/logs", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /system/logs: %d %s", status, body)
	}
	if !strings.Contains(string(body), "line-1") || !strings.Contains(string(body), "line-3") {
		t.Fatalf("应返回完整日志尾部：%s", body)
	}
	status, _, body = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/system/logs?last=1", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("last=1: %d %s", status, body)
	}
	if got := strings.TrimSpace(string(body)); got != "line-3" {
		t.Fatalf("last=1 应只回末行，得到 %q", got)
	}

	// 未接入日志来源 → 503（不谎报空日志）
	ts2 := newTestServer(t)
	token2 := loginAdmin(t, ts2)
	status, _, _ = cfgRequest(t, http.MethodGet, ts2.URL+APIPrefix+"/system/logs", token2, nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("未接入日志来源应 503，得到 %d", status)
	}
}

// ping：通 → 200 + output；未通 → 502 且把原始回显放进 detail（与 CLI 同口径）。
func TestPingEndpoint(t *testing.T) {
	ts := diagServer(t, &fakeDiag{pingOut: "3 sent, 3 received\n"})
	token := loginAdmin(t, ts)

	status, _, body := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/diagnostics/ping", token,
		map[string]any{"host": "192.0.2.1", "count": 3}, nil)
	if status != http.StatusOK {
		t.Fatalf("ping: %d %s", status, body)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	if out, _ := got["output"].(string); !strings.Contains(out, "3 received") {
		t.Fatalf("应回原始回显：%s", body)
	}

	// 未通：502 + 回显进 detail（客户端能照着排查）
	ts2 := diagServer(t, &fakeDiag{pingOut: "Statistics: 0 sent, 0 received", pingErr: errors.New("ping 未发出任何报文")})
	token2 := loginAdmin(t, ts2)
	status, _, body = cfgRequest(t, http.MethodPost, ts2.URL+APIPrefix+"/diagnostics/ping", token2,
		map[string]any{"host": "192.0.2.1"}, nil)
	if status != http.StatusBadGateway {
		t.Fatalf("未通应 502，得到 %d %s", status, body)
	}
	if !strings.Contains(string(body), "PING_FAILED") || !strings.Contains(string(body), "0 sent") {
		t.Fatalf("应给错误码与原始回显：%s", body)
	}

	// 缺 host → 400
	status, _, _ = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/diagnostics/ping", token, map[string]any{}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("缺 host 应 400，得到 %d", status)
	}
}

// traceroute：通 → 200；失败 → 502。
func TestTracerouteEndpoint(t *testing.T) {
	ts := diagServer(t, &fakeDiag{trOut: "1  192.0.2.1\n"})
	token := loginAdmin(t, ts)
	status, _, body := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/diagnostics/traceroute", token,
		map[string]any{"host": "192.0.2.1"}, nil)
	if status != http.StatusOK || !strings.Contains(string(body), "192.0.2.1") {
		t.Fatalf("traceroute: %d %s", status, body)
	}
}

// 清零统计：204；接口名透传给运行时（缺省 = 全部）。
func TestClearInterfaceStatsEndpoint(t *testing.T) {
	d := &fakeDiag{}
	ts := diagServer(t, d)
	token := loginAdmin(t, ts)

	status, _, body := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/interfaces:clear-statistics", token,
		map[string]any{"name": "ens2f0"}, nil)
	if status != http.StatusNoContent {
		t.Fatalf("clear-statistics: %d %s", status, body)
	}
	if d.cleared != "ens2f0" {
		t.Fatalf("接口名应透传，得到 %q", d.cleared)
	}
	status, _, _ = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/interfaces:clear-statistics", token, nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("缺省（全部）应 204，得到 %d", status)
	}
	if d.cleared != "" {
		t.Fatalf("缺省应为空接口名（全部），得到 %q", d.cleared)
	}
}
