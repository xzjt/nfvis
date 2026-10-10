package container

// 决策 #447：容器镜像的 Docker 引用推导与删除对称。
//
// 红-绿口径：把实现改回旧行为（`LoadImage` 无条件 `c.tag(name, "latest")`、
// `RemoveImage` 按目录项名原样删、`DockerRefFor` 不存在）时，下列用例按预期失败：
//   - 「含冒号且与载入引用一致 ⇒ 零 tag 调用」会看到一次 tag 调用（旧实现无条件重打）；
//   - `RemoveImage("alpine")` 的 DELETE 路径是 `/images/alpine` 而非推导引用。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestDockerRefFor 推导表：含冒号原样（本身即合法 Docker 引用）、不含冒号补 :latest。
func TestDockerRefFor(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"alpine:3.20", "alpine:3.20"},   // 本仓既有用法：目录项名含冒号，原样
		{"alpine", "alpine:latest"},      // 不含冒号：沿用 #160/#312 语义
		{"a/b", "a/b:latest"},            // 含 / 无冒号：补 :latest
		{"reg:5000/img", "reg:5000/img"}, // registry 端口里的冒号不构成 tag 分隔
		{"a.b/c:1", "a.b/c:1"},           // 带 registry 风格 + tag：原样
	} {
		if got := DockerRefFor(tc.in); got != tc.want {
			t.Errorf("DockerRefFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSplitDockerRef 拆分规则：取**最后一个 `/` 之后**的冒号做 tag 分隔。
func TestSplitDockerRef(t *testing.T) {
	for _, tc := range []struct{ ref, repo, tag string }{
		{"alpine:3.20", "alpine", "3.20"},
		{"alpine", "alpine", ""}, // 无 tag：调用方按 latest 补
		{"a/b", "a/b", ""},
		{"reg:5000/img", "reg:5000/img", ""}, // 冒号在最后一个 / 之前 ⇒ registry 端口，不是 tag
		{"reg:5000/img:v1", "reg:5000/img", "v1"},
		{"a.b/c:1", "a.b/c", "1"},
	} {
		repo, tag := splitDockerRef(tc.ref)
		if repo != tc.repo || tag != tc.tag {
			t.Errorf("splitDockerRef(%q) = (%q, %q), want (%q, %q)", tc.ref, repo, tag, tc.repo, tc.tag)
		}
	}
}

// dockerAPIRequest 一条记账的 Docker API 请求。
type dockerAPIRequest struct {
	method string
	path   string
	query  url.Values
}

// dockerAPIRecorder 记账型 Docker API 假后端：回放一条 load 应答流，其余请求一律 200。
type dockerAPIRecorder struct {
	mu       sync.Mutex
	requests []dockerAPIRequest
}

func (d *dockerAPIRecorder) serve(loadStream string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.requests = append(d.requests, dockerAPIRequest{method: r.Method, path: r.URL.Path, query: r.URL.Query()})
		d.mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/images/load" {
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, loadStream)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func (d *dockerAPIRecorder) snapshot() []dockerAPIRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]dockerAPIRequest(nil), d.requests...)
}

// tagRequests 只取 tag 调用（POST /images/<ref>/tag）。
func (d *dockerAPIRecorder) tagRequests() []dockerAPIRequest {
	var out []dockerAPIRequest
	for _, req := range d.snapshot() {
		if req.method == http.MethodPost && strings.HasSuffix(req.path, "/tag") {
			out = append(out, req)
		}
	}
	return out
}

// TestLoadImageAlignsDockerRef：载入后仅在「归档载入的引用 ≠ 推导引用」时才重打标签。
func TestLoadImageAlignsDockerRef(t *testing.T) {
	loadLine := func(ref string) string { return `{"stream":"Loaded image: ` + ref + `\n"}` + "\n" }
	for _, tc := range []struct {
		name       string
		dirName    string
		loadStream string
		wantErr    bool
		wantTagN   int
		wantRepo   string
		wantTag    string
	}{
		{
			name:       "含冒号且与载入引用一致：零 tag 调用（不抢占用户既有 tag）",
			dirName:    "alpine:3.20",
			loadStream: loadLine("alpine:3.20"),
			wantTagN:   0,
		},
		{
			name:       "含冒号但与载入引用不一致：恰一次重打，repo/tag 取自推导引用",
			dirName:    "alpine:3.20",
			loadStream: loadLine("alpine:3.19"),
			wantTagN:   1,
			wantRepo:   "alpine",
			wantTag:    "3.20",
		},
		{
			name:       "不含冒号：补 :latest 重打一次",
			dirName:    "alpine",
			loadStream: loadLine("alpine:3.20"),
			wantTagN:   1,
			wantRepo:   "alpine",
			wantTag:    "latest",
		},
		{
			name:       "registry 风格名含冒号（tag 缺省）：repo 保留端口冒号、tag 补 latest",
			dirName:    "reg:5000/img",
			loadStream: loadLine("reg:5000/img:old"),
			wantTagN:   1,
			wantRepo:   "reg:5000/img",
			wantTag:    "latest",
		},
		{
			name:       "loaded 为空（未解析到 tag）：既有错误保留",
			dirName:    "alpine",
			loadStream: `{"stream":"Step 1/1 : loading\n"}` + "\n",
			wantErr:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "img.tar")
			if err := os.WriteFile(path, []byte("fake-tar"), 0o600); err != nil {
				t.Fatal(err)
			}
			rec := &dockerAPIRecorder{}
			srv := httptest.NewServer(rec.serve(tc.loadStream))
			t.Cleanup(srv.Close)
			c := &dockerClient{http: &http.Client{}, base: srv.URL}

			err := c.LoadImage(context.Background(), path, tc.dirName)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "未解析到镜像 tag") {
					t.Fatalf("未解析到 tag 应报既有错误，得 %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadImage 应成功: %v", err)
			}
			tags := rec.tagRequests()
			if len(tags) != tc.wantTagN {
				t.Fatalf("tag 调用次数 = %d（%v），want %d", len(tags), tags, tc.wantTagN)
			}
			if tc.wantTagN == 1 {
				got := tags[0]
				if got.query.Get("repo") != tc.wantRepo || got.query.Get("tag") != tc.wantTag {
					t.Fatalf("tag 参数 = repo %q / tag %q，want repo %q / tag %q",
						got.query.Get("repo"), got.query.Get("tag"), tc.wantRepo, tc.wantTag)
				}
			}
		})
	}
}

// TestRemoveImageUsesDerivedRef：删除用**推导引用**（与 LoadImage 建立的引用严格对称）。
func TestRemoveImageUsesDerivedRef(t *testing.T) {
	for _, tc := range []struct{ ref, wantPath string }{
		{"alpine:3.20", "/images/alpine:3.20"}, // 含冒号：原样删（用户/目录项引用的 tag）
		{"alpine", "/images/alpine:latest"},    // 不含冒号：删产品载入时落成的引用
		{"a/b", "/images/a/b:latest"},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			rec := &dockerAPIRecorder{}
			srv := httptest.NewServer(rec.serve(""))
			t.Cleanup(srv.Close)
			c := &dockerClient{http: &http.Client{}, base: srv.URL}

			if err := c.RemoveImage(context.Background(), tc.ref); err != nil {
				t.Fatalf("RemoveImage(%q) 应成功: %v", tc.ref, err)
			}
			reqs := rec.snapshot()
			if len(reqs) != 1 {
				t.Fatalf("应恰有一次请求，得 %v", reqs)
			}
			if reqs[0].method != http.MethodDelete || reqs[0].path != tc.wantPath {
				t.Fatalf("RemoveImage(%q) 请求 = %s %s，want DELETE %s",
					tc.ref, reqs[0].method, reqs[0].path, tc.wantPath)
			}
		})
	}
}
