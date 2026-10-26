package api

// round49：POST /images 的 multipart/form-data 上传路径（契约与实现漂移收口）。
//
// 契约定的是三选一（docs/NFViS-openapi.yaml 的 POST /images）：multipart/form-data
// （name/type/file[/description]）/ JSON incoming 导入 / JSON URL 拉取。Web 控制台「上传文件并导入」
// 走的正是 `new FormData()` + POST /images（internal/api/ui/js/views/images.js 的 imgImportFile），
// 而实现只 `decodeBody`（纯 JSON）——上传必然 400「JSON 不合法」，super-user 在控制台上传镜像文件
// 在当前构建上不可用。本组测试钉住四件事：
//   ① 上传可用（字段名与 Web 一致）；
//   ② 名字校验与 JSON 路径**同源**（走 images.ValidateImageName，越界名 400 且不落任何文件）；
//   ③ 临时文件一律落在 incoming 目录内（不用用户给的名字当路径）、任何失败路径都清掉；
//   ④ 超过总大小上限的请求被拒且不落残留。

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/images"
)

// postImageMultipart 构造 multipart/form-data 请求（字段顺序与 Web 的 FormData 一致：
// 文本字段在前、file 收尾），返回状态码与响应体。
//
// 注意 filename 是**客户端自报**的（不可信），与 fields["name"] 有意分开传——产品必须只认
// `name` 字段（契约字段），不能拿 filename 当镜像名。
func postImageMultipart(t *testing.T, ts *httptest.Server, token string,
	fields map[string]string, order []string, filename string, content []byte) (int, string) {
	t.Helper()
	body := new(bytes.Buffer)
	mw := multipart.NewWriter(body)
	for _, k := range order {
		if err := mw.WriteField(k, fields[k]); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+APIPrefix+"/images", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败（服务端可能提前关连接）: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// incomingEntries 返回 incoming 目录里的全部条目名（断言「无残留」用）。
func incomingEntries(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读 incoming 目录: %v", err)
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// Web 的「上传文件并导入」（name/type/file）必须真的能用：201 + 登记 + 文件进仓库 + 无残留。
func TestImagesPostMultipartImportsVMImage(t *testing.T) {
	store := newImagesStore(t)
	ts := newTestServerOpts(t, Options{VM: newFakeVM(), Images: store})
	token := loginAdmin(t, ts)

	content := []byte("QCOW2-BYTES")
	status, data := postImageMultipart(t, ts, token,
		map[string]string{"name": "web-upload.qcow2", "type": "vm-image", "description": "d"},
		[]string{"name", "type", "description"}, "客户端自报.qcow2", content)
	if status != http.StatusCreated {
		t.Fatalf("multipart 上传应 201: %d %s", status, data)
	}
	if !strings.Contains(data, `"import_state":"ready"`) {
		t.Errorf("响应应为 ready: %s", data)
	}
	// 只认 name 字段（不是客户端自报的 filename），描述透传。
	m, ok := store.Get("web-upload.qcow2")
	if !ok || m.Type != images.TypeVM {
		t.Fatalf("镜像未按 name 字段登记: %+v", m)
	}
	if m.Description != "d" {
		t.Errorf("description 未透传: %q", m.Description)
	}
	got, err := os.ReadFile(filepath.Join(store.Config().Dir, "web-upload.qcow2"))
	if err != nil || string(got) != string(content) {
		t.Fatalf("仓库里的镜像内容不对: %v %q", err, got)
	}
	if left := incomingEntries(t, store.Config().IncomingDir); len(left) != 0 {
		t.Errorf("incoming 目录不应残留临时文件: %v", left)
	}
}

// 越界名走 multipart 也必须 400（与 JSON 路径同源校验），且不落任何文件、仓库外文件不受影响。
func TestImagesPostMultipartRejectsTraversalName(t *testing.T) {
	store := newImagesStore(t)
	ts := newTestServerOpts(t, Options{VM: newFakeVM(), Images: store})
	token := loginAdmin(t, ts)

	// 仓库外哨兵：名字若被当成路径用，它会被覆盖/删除。
	outside := filepath.Join(filepath.Dir(store.Config().Dir), "outside.qcow2")
	if err := os.WriteFile(outside, []byte("MUST-STAY"), 0o644); err != nil {
		t.Fatal(err)
	}

	status, data := postImageMultipart(t, ts, token,
		map[string]string{"name": "../outside.qcow2", "type": "vm-image"},
		[]string{"name", "type"}, "x.qcow2", []byte("EVIL"))
	if status != http.StatusBadRequest {
		t.Fatalf("越界名应 400: %d %s", status, data)
	}
	if !strings.Contains(data, "镜像名不合法") {
		t.Errorf("400 文案应说明名字不合法（可照做）: %s", data)
	}
	assertOutsideUntouched(t, outside)
	// 名字校验在落盘之前——incoming 目录里不该出现任何文件。
	if left := incomingEntries(t, store.Config().IncomingDir); len(left) != 0 {
		t.Errorf("名字校验应在落盘之前，incoming 目录不应有任何文件: %v", left)
	}
}

// 导入失败（容器镜像未接入 Docker → ImportIncoming 报错）时临时文件必须被清理。
func TestImagesPostMultipartCleansTempOnImportFailure(t *testing.T) {
	store := newImagesStore(t) // 未注入 dockerLoad：容器镜像导入必失败
	ts := newTestServerOpts(t, Options{VM: newFakeVM(), Images: store})
	token := loginAdmin(t, ts)

	status, data := postImageMultipart(t, ts, token,
		map[string]string{"name": "alpine:3.20", "type": "container-image"},
		[]string{"name", "type"}, "alpine.tar", []byte("TARBALL"))
	if status != http.StatusBadRequest {
		t.Fatalf("未接入 Docker 时导入容器镜像应 400: %d %s", status, data)
	}
	if !strings.Contains(data, "未接入 Docker") {
		t.Errorf("错误应如实说明底层原因: %s", data)
	}
	left := incomingEntries(t, store.Config().IncomingDir)
	if len(left) != 0 {
		t.Fatalf("导入失败必须清理临时文件，incoming 目录残留: %v", left)
	}
}

// 超过总大小上限的上传必须被拒（413）且**不落任何残留**：incoming 目录空、仓库无该镜像。
// 上限在请求体层面生效（http.MaxBytesReader）——超量数据读都不读完，谈不上落盘。
func TestImagesPostMultipartSizeLimit(t *testing.T) {
	orig := maxImageUploadBytes
	maxImageUploadBytes = 1024 // 小体量上验证超限路径（生产恒为 4 GiB）
	defer func() { maxImageUploadBytes = orig }()

	store := newImagesStore(t)
	ts := newTestServerOpts(t, Options{VM: newFakeVM(), Images: store})
	token := loginAdmin(t, ts)

	status, data := postImageMultipart(t, ts, token,
		map[string]string{"name": "big.qcow2", "type": "vm-image"},
		[]string{"name", "type"}, "big.qcow2", make([]byte, 8*1024))
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("超过上限应 413: %d %s", status, data)
	}
	if !strings.Contains(data, "1024") {
		t.Errorf("413 文案应给出上限值（可照做）: %s", data)
	}
	if left := incomingEntries(t, store.Config().IncomingDir); len(left) != 0 {
		t.Errorf("超限请求不应落任何文件: %v", left)
	}
	if _, ok := store.Get("big.qcow2"); ok {
		t.Error("超限请求不应登记镜像")
	}
	if _, err := os.Stat(filepath.Join(store.Config().Dir, "big.qcow2")); !os.IsNotExist(err) {
		t.Errorf("超限请求不应在仓库里留下文件: %v", err)
	}
}

// 字段校验与 JSON 路径同源：type 非法 → 400（文案点名允许值）；name 缺 → 400。
//
// name 只出现在 **URL query** 时必须不算数——JSON 路径只读请求体，multipart 路径也只读表单体
// （r.PostFormValue），否则 `POST /images?name=…` 就能绕开 body 字段。
func TestImagesPostMultipartValidatesNameAndType(t *testing.T) {
	store := newImagesStore(t)
	ts := newTestServerOpts(t, Options{VM: newFakeVM(), Images: store})
	token := loginAdmin(t, ts)

	// type 非法：400，且不落任何文件（校验在落盘之前）。
	status, data := postImageMultipart(t, ts, token,
		map[string]string{"name": "x.qcow2", "type": "firmware"},
		[]string{"name", "type"}, "x.qcow2", []byte("BYTES"))
	if status != http.StatusBadRequest || !strings.Contains(data, "type 必须为") {
		t.Fatalf("type 非法应 400 并点名允许值: %d %s", status, data)
	}
	if left := incomingEntries(t, store.Config().IncomingDir); len(left) != 0 {
		t.Errorf("type 非法时不应落任何文件: %v", left)
	}

	// name 只写在 query 里：不算数（表单体里没有 name）→ 400。
	body := new(bytes.Buffer)
	mw := multipart.NewWriter(body)
	if err := mw.WriteField("type", "vm-image"); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("file", "x.qcow2")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte("BYTES"))
	_ = mw.Close()
	req, err := http.NewRequest(http.MethodPost,
		ts.URL+APIPrefix+"/images?name=query-only.qcow2", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respData, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(respData), "name 必填") {
		t.Fatalf("name 只写在 query 里不算数，应 400: %d %s", resp.StatusCode, respData)
	}
	if _, ok := store.Get("query-only.qcow2"); ok {
		t.Error("query 里的 name 不应被采纳")
	}
	if left := incomingEntries(t, store.Config().IncomingDir); len(left) != 0 {
		t.Errorf("不应留下任何文件: %v", left)
	}
}
