package api

// round49（主会话补的交叉守护）：**契约声明 multipart/form-data 的端点，实现必须真的支持**。
//
// 由来：`POST /images` 在 openapi 里一直声明 `multipart/form-data`（`file: binary`），而 handler
// 只解 JSON —— Web 控制台「上传文件并导入」正是 `FormData`，于是该路径**必然 400**，而仓库里
// 没有任何守护会照出这条漂移（`openapi_test.go` 只验证规范被提供、路由守护不比对 content-type）。
// 本守护把「规范里出现的 multipart 请求体」与「实现已支持」对账：新增一条 multipart 声明却
// 没实现（或实现被删）会在这里报红，必须显式登记并说明验证测试落在哪里；另加一条行为级正控，
// 证明登记不是纸面承诺（同一路由用 multipart 真的不再被当成 JSON 拒掉）。

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
)

// supportedMultipartRoutes 已实现 multipart 的（方法, 路径）→ 验证它的测试名。
// 新增契约里的 multipart 端点时必须在此登记，否则 TestMultipartContractHasImplementation 报红。
var supportedMultipartRoutes = map[string]string{
	"POST /images":         "TestImagesPostMultipartImportsVMImage（internal/api/images_multipart_round49_test.go）",
	"POST /system/restore": "TestBackupRestoreZeroizeEndpoints（internal/api/system_ops_test.go，multipart 归档上传恢复）",
}

func TestMultipartContractHasImplementation(t *testing.T) {
	raw, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatalf("读取 openapi.json: %v", err)
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("解析 openapi.json: %v", err)
	}
	var declared []string
	for path, ops := range spec.Paths {
		for method, opRaw := range ops {
			if method == "parameters" {
				continue
			}
			var op struct {
				RequestBody struct {
					Content map[string]json.RawMessage `json:"content"`
				} `json:"requestBody"`
			}
			if err := json.Unmarshal(opRaw, &op); err != nil {
				continue
			}
			for ct := range op.RequestBody.Content {
				if strings.HasPrefix(ct, "multipart/") {
					declared = append(declared, strings.ToUpper(method)+" "+path)
				}
			}
		}
	}
	sort.Strings(declared)
	if len(declared) == 0 {
		t.Fatal("契约里没有任何 multipart 端点——若确已删净，请同时删掉本守护与 allowlist")
	}
	for _, route := range declared {
		if _, ok := supportedMultipartRoutes[route]; !ok {
			t.Fatalf("契约声明了 multipart 端点 %s 但未登记实现（Web/客户端会必然 400）："+
				"实现后把它加入 supportedMultipartRoutes 并写明验证测试", route)
		}
	}
	// 反向：allowlist 里的路由必须在契约里仍声明 multipart（声明被删要同步登记）
	for route := range supportedMultipartRoutes {
		found := false
		for _, d := range declared {
			if d == route {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("supportedMultipartRoutes 登记了 %s，但契约里已无该 multipart 声明——请同步", route)
		}
	}
}

// 行为级正控：该路由用 multipart 真的不再被按 JSON 拒掉。
// 具体业务语义（201/越界名 400/临时文件清理/413）在 images_multipart_round49_test.go。
func TestMultipartRouteActuallyAcceptsMultipart(t *testing.T) {
	ts := newTestServerOpts(t, Options{Images: &fakeImagesRuntime{}})
	token := loginAdmin(t, ts)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", "x.qcow2")
	_ = mw.WriteField("type", "vm-image")
	fw, err := mw.CreateFormFile("file", "x.qcow2")
	if err != nil {
		t.Fatalf("构造 multipart 文件字段: %v", err)
	}
	if _, err := fw.Write([]byte("data")); err != nil {
		t.Fatalf("写 multipart 内容: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("收尾 multipart: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+APIPrefix+"/images", &buf)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("发请求: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应: %v", err)
	}
	if body := string(raw); resp.StatusCode == http.StatusBadRequest && strings.Contains(body, "JSON 不合法") {
		t.Fatalf("契约声明 multipart，但实现仍在按 JSON 解析：%d %s", resp.StatusCode, body)
	}
}
