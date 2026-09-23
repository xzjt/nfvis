package system

// 缺口②（决策 #126）：`request system core-dumps export <url>` 的真实现——
// 把转储**清单**（JSON）POST 到目标 URL。此前 CLI 只打印「已受理…将导出」而**什么都没做**
// （假成功），本用例锁住"真发请求 / 失败必须报错 / 目标不合法即拒"。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 造一个含两个转储的目录（文件名符合 isCoreFile 规则）。
func seedCores(t *testing.T) *CoreDumps {
	t.Helper()
	dir := t.TempDir()
	for _, n := range []string{"core.nfvisd.1234.1700000000", "nginx.core"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return NewCoreDumps(dir, 0)
}

func TestExportManifestPostsManifest(t *testing.T) {
	cores := seedCores(t)

	var got map[string]any
	var method, ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, ctype = r.Method, r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("目标端解析 body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, status, err := cores.ExportManifest(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("导出应成功: %v", err)
	}
	if method != http.MethodPost {
		t.Fatalf("必须 POST，实际 %s", method)
	}
	if !strings.HasPrefix(ctype, "application/json") {
		t.Fatalf("Content-Type 应为 JSON，实际 %q", ctype)
	}
	if n != 2 || status != http.StatusOK {
		t.Fatalf("应导出 2 条 / HTTP 200，得到 %d / %d", n, status)
	}
	// 清单形状：hostname + exported_at + core_dumps[]（本体不在这里传）
	for _, k := range []string{"hostname", "exported_at", "core_dumps"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("清单缺字段 %q: %v", k, got)
		}
	}
	rows, _ := got["core_dumps"].([]any)
	if len(rows) != 2 {
		t.Fatalf("清单应含 2 条转储，得到 %d", len(rows))
	}
	first, _ := rows[0].(map[string]any)
	for _, k := range []string{"file", "process", "size_bytes", "occurred_at"} {
		if _, ok := first[k]; !ok {
			t.Fatalf("转储条目缺字段 %q: %v", k, first)
		}
	}
}

// 非 2xx → 报错（**不许假成功**），且把目标状态带出来。
func TestExportManifestNon2xxIsError(t *testing.T) {
	cores := seedCores(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n, status, err := cores.ExportManifest(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("目标 500 时必须报错（不能像旧实现那样只打印「已受理」）")
	}
	if n != 2 || status != http.StatusInternalServerError {
		t.Fatalf("应回报 2 条 / HTTP 500，得到 %d / %d", n, status)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("错误应含目标状态: %v", err)
	}
}

// 目标不可达 → 报错。
func TestExportManifestUnreachableIsError(t *testing.T) {
	cores := seedCores(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 关掉 → 连接被拒
	if _, _, err := cores.ExportManifest(context.Background(), url); err == nil {
		t.Fatal("目标不可达时必须报错")
	}
}

// 只允许 http/https：其它 scheme 直接拒（不当成"导出成功"）。
func TestExportManifestRejectsNonHTTPScheme(t *testing.T) {
	cores := seedCores(t)
	for _, u := range []string{"file:///tmp/x.json", "ftp://host/x", "不是地址"} {
		_, _, err := cores.ExportManifest(context.Background(), u)
		if err == nil {
			t.Fatalf("%q 应被拒", u)
		}
	}
}

// 无转储时：清单为空但仍真发请求（条数 0），由调用方决定怎么呈现。
func TestExportManifestEmptyIsStillSent(t *testing.T) {
	cores := NewCoreDumps(t.TempDir(), 0)
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	n, status, err := cores.ExportManifest(context.Background(), srv.URL)
	if err != nil || !hit {
		t.Fatalf("空清单也应真发请求: hit=%v err=%v", hit, err)
	}
	if n != 0 || status != http.StatusNoContent {
		t.Fatalf("应为 0 条 / 204，得到 %d / %d", n, status)
	}
}
