package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/images"
)

func newImagesStore(t *testing.T) *images.Store {
	t.Helper()
	dir := t.TempDir()
	inc := filepath.Join(dir, "incoming")
	if err := os.MkdirAll(inc, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := images.Open(images.Config{Dir: filepath.Join(dir, "images"), IncomingDir: inc})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestImagesEndpoints(t *testing.T) {
	store := newImagesStore(t)
	ts := newTestServerOpts(t, Options{VM: newFakeVM(), Images: store})
	token := loginAdmin(t, ts)

	// 未装配 503 单独在 TestImagesUnavailable 验证
	// incoming 导入（201）
	src := filepath.Join(store.Config().IncomingDir, "base.qcow2")
	if err := os.WriteFile(src, []byte("qcow2"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/images", token,
		map[string]any{"name": "base.qcow2", "type": "vm-image", "incoming_file": src, "description": "d"}, nil)
	if status != http.StatusCreated || !strings.Contains(string(data), `"import_state":"ready"`) {
		t.Fatalf("导入应 201 ready: %d %s", status, data)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("导入后 incoming 源文件应清理")
	}

	// 列表 + 详情（ref_count）
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/images", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), "base.qcow2") {
		t.Fatalf("列表: %d %s", status, data)
	}
	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/images/base.qcow2", token, nil, nil)
	var detail map[string]any
	_ = json.Unmarshal(data, &detail)
	if status != http.StatusOK || detail["ref_count"] != float64(0) {
		t.Fatalf("详情: %d %s", status, data)
	}
	status, _, _ = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/images/ghost", token, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("不存在应 404: %d", status)
	}

	// 参数校验：url 与 incoming_file 二选一
	status, _, _ = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/images", token,
		map[string]any{"name": "x", "type": "vm-image"}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("缺 url/incoming_file 应 400: %d", status)
	}

	// 删除无引用镜像 → 200
	status, _, data = cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/images/base.qcow2", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("删除应 200: %d %s", status, data)
	}
}

func TestImagesDeleteReferenced(t *testing.T) {
	store := newImagesStore(t)
	ts := newTestServerOpts(t, Options{VM: newFakeVM(), Images: store})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)

	// 导入镜像并经配置引用（VM image=base.qcow2）
	src := filepath.Join(store.Config().IncomingDir, "base.qcow2")
	_ = os.WriteFile(src, []byte("qcow2"), 0o644)
	cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/images", token,
		map[string]any{"name": "base.qcow2", "type": "vm-image", "incoming_file": src}, nil)
	body := vmBody("fw-vm")
	body["image"] = "base.qcow2"
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		body, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("建 VM: %d %s", status, data)
	}
	// 删除被引用镜像 → 409
	status, _, data := cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/images/base.qcow2", token, nil, nil)
	if status != http.StatusConflict || !strings.Contains(string(data), "引用") {
		t.Fatalf("被引用删除应 409: %d %s", status, data)
	}
}

func TestImagesUnavailable(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/images", token, nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("未装配应 503: %d", status)
	}
}
