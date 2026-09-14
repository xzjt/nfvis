package api

// FR-API-002（决策 #69）：GET /api/v1/openapi.json 返回随产品发布的规范。

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// 无鉴权可取（与 /metrics 同），内容为可解析的 OpenAPI 3.0 JSON 且覆盖关键路径。
func TestOpenAPISpecEndpoint(t *testing.T) {
	ts := newTestServerOpts(t, Options{})
	// 故意不带 Authorization 头
	resp, err := http.Get(ts.URL + APIPrefix + "/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)

	var spec struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(body, &spec); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	if spec.OpenAPI == "" {
		t.Fatal("缺少 openapi 版本字段")
	}
	// 规范自身也必须在 paths 中声明（契约自洽）
	for _, p := range []string{"/login", "/virtual-switches", "/openapi.json"} {
		if _, ok := spec.Paths[p]; !ok {
			t.Fatalf("paths 缺少 %s", p)
		}
	}
}

// 规范内容须与契约文件同源（docs/NFViS-openapi.yaml 的转换结果由 docscheck 守护，
// 此处断言运行时返回的 paths 数与非零，防止嵌入空文件）。
func TestOpenAPISpecNonEmpty(t *testing.T) {
	if len(openAPISpec) < 1000 {
		t.Fatalf("嵌入的规范过小（%d 字节），疑似生成失败", len(openAPISpec))
	}
}
