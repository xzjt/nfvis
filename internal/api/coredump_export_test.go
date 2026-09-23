package api

// 缺口②（决策 #126）：`POST /system/core-dumps:export` 与 CLI `request system core-dumps
// export <url>` 都走同一实现（`system.CoreDumps.ExportManifest`），且**失败必须报错**
// ——旧 CLI 只打印「已受理：N 个转储清单将导出至 …（POST）」而什么都没做。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

// seedExportCores 造一个带转储的 diagOps（返回目录路径便于断言）。
func seedExportCores(t *testing.T) (*testDiagOps, string) {
	t.Helper()
	d, dir := newTestDiagOps(t)
	if err := os.WriteFile(filepath.Join(dir, "core.nfvisd.99.1700000000"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return d, dir
}

func TestCoreDumpsExportEndpoint(t *testing.T) {
	d, _ := seedExportCores(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("应为 POST，实际 %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	ts := newTestServerOpts(t, Options{DiagOps: d})
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/core-dumps:export", token,
		map[string]any{"url": target.URL}, nil)
	if status != http.StatusOK {
		t.Fatalf("导出: %d %s", status, data)
	}
	for _, want := range []string{`"exported":1`, `"status":200`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("响应应含 %s: %s", want, data)
		}
	}
}

// 目标失败 → 502（上游失败），**不谎报成功**。
func TestCoreDumpsExportUpstreamFailure(t *testing.T) {
	d, _ := seedExportCores(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer target.Close()

	ts := newTestServerOpts(t, Options{DiagOps: d})
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/core-dumps:export", token,
		map[string]any{"url": target.URL}, nil)
	if status != http.StatusBadGateway {
		t.Fatalf("目标 500 应映射 502，得到 %d %s", status, data)
	}
	if strings.Contains(string(data), `"exported"`) {
		t.Fatalf("失败时不得回导出结果: %s", data)
	}
}

// 缺 url → 400；非 http/https → 400（入参问题，不是上游问题）。
func TestCoreDumpsExportValidation(t *testing.T) {
	d, _ := seedExportCores(t)
	ts := newTestServerOpts(t, Options{DiagOps: d})
	token := loginAdmin(t, ts)

	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/core-dumps:export", token,
		map[string]any{}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("缺 url 应 400，得到 %d %s", status, data)
	}
	status, _, data = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/core-dumps:export", token,
		map[string]any{"url": "file:///tmp/x"}, nil)
	if status != http.StatusBadRequest || !strings.Contains(string(data), "http") {
		t.Fatalf("非 http/https 应 400 且说明原因，得到 %d %s", status, data)
	}
}

// 权限：operator 可导出；read-only 被拒（与 CLI 的 O 级一致）。
func TestCoreDumpsExportPermission(t *testing.T) {
	d, _ := seedExportCores(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	ts := newTestServerOpts(t, Options{DiagOps: d})
	token := loginAdmin(t, ts)

	// 建 read-only 用户（走 REST，与真机验证同法）
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/login-users", token,
		map[string]any{"name": "ro", "class": "read-only", "password": "Ro@12345678"},
		map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("建 read-only 用户: %d %s", status, data)
	}
	status, _, data = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/login", "",
		map[string]any{"username": "ro", "password": "Ro@12345678"}, nil)
	if status != http.StatusOK {
		t.Fatalf("ro 登录: %d %s", status, data)
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &login); err != nil || login.Token == "" {
		t.Fatalf("取 ro token: %v %s", err, data)
	}
	status, _, data = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/system/core-dumps:export", login.Token,
		map[string]any{"url": target.URL}, nil)
	if status != http.StatusForbidden {
		t.Fatalf("read-only 导出应 403，得到 %d %s", status, data)
	}
}

// CLI 侧：真发请求（不再只打印「已受理」）；目标失败时输出 %% 且审计记失败。
func TestCLICoreDumpsExportReallyExports(t *testing.T) {
	d, _ := seedExportCores(t)
	x, _ := newCLIKit(t)
	x.setDiagOps(d)

	hit := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit++
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system core-dumps export "+target.URL).Output
	if hit != 1 {
		t.Fatalf("必须真的 POST 到目标（旧实现 hit=0），实际 %d 次；输出：%s", hit, out)
	}
	if !strings.Contains(out, "已导出 1 个转储清单") {
		t.Fatalf("输出应说明真的导出了几条: %s", out)
	}
	if strings.Contains(out, "已受理") {
		t.Fatalf("不得再用「已受理…将导出」的假成功措辞: %s", out)
	}

	// 目标失败 → %%（真机冒烟按失败计）
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system core-dumps export "+bad.URL).Output
	if !strings.HasPrefix(out, "%%") {
		t.Fatalf("目标失败必须报错（%% 开头）: %s", out)
	}
}

// CLI 侧：没有转储时不发请求、明确说"无"。
func TestCLICoreDumpsExportEmpty(t *testing.T) {
	d, _ := newTestDiagOps(t) // 空目录
	x, _ := newCLIKit(t)
	x.setDiagOps(d)
	hit := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit++ }))
	defer target.Close()

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system core-dumps export "+target.URL).Output
	if hit != 0 {
		t.Fatalf("无转储时不该发请求，实际 %d 次", hit)
	}
	if !strings.Contains(out, "无 core dump 可导出") {
		t.Fatalf("应明确说明无转储: %s", out)
	}
}
