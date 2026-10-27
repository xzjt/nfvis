package api

// 决策 #514（round50 只读评审，接 #505）：镜像**发布防覆盖被引用项** + 只认 ready 的 REST/CLI 出口。
//
// 覆盖两条用户可见出口：
//   - REST `POST /images`：同名镜像被 VNF/容器引用时导入/上传一律 **409 CONFLICT**（不是 400/500），
//     且仓库文件不被改动（与 DELETE 被引用同码）；
//   - CLI `request images upload`：被引用时以 `%%` 如实报错（含「不能覆盖」），不得假装导入成功。
//
// 装配侧由 internal/api/server.go 注入 committed 配置读口（CLI 与 REST 共用同一 store 实例），
// 故此处直接复用测试基建的 engine（见 newTestServerOpts）。

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/images"
	"github.com/xzjt/nfvis/internal/model"
)

// TestImagesPostReferencedConflict：镜像被 VNF 引用后，再对**同名**导入 ⇒ 409（不是 400/500），
// 且仓库里的文件一字不改（backing 克隆的既有 VM 不会被换底）。
func TestImagesPostReferencedConflict(t *testing.T) {
	store := newImagesStore(t)
	ts := newTestServerOpts(t, Options{VM: newFakeVM(), Images: store})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)

	// ① 导入 base.qcow2（内容 ORIGINAL）并经 VNF 引用它。
	src := filepath.Join(store.Config().IncomingDir, "base.qcow2")
	if err := os.WriteFile(src, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/images", token,
		map[string]any{"name": "base.qcow2", "type": "vm-image", "incoming_file": src}, nil); status != http.StatusCreated {
		t.Fatalf("导入 base.qcow2: %d %s", status, data)
	}
	body := vmBody("fw-vm")
	body["image"] = "base.qcow2"
	if status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		body, map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusCreated {
		t.Fatalf("建 VM 引用 base.qcow2: %d %s", status, data)
	}

	// ② 再次导入同名（不同内容）⇒ 409 CONFLICT（被引用不可覆盖）。
	src2 := filepath.Join(store.Config().IncomingDir, "newbase.qcow2")
	if err := os.WriteFile(src2, []byte("REPLACEMENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/images", token,
		map[string]any{"name": "base.qcow2", "type": "vm-image", "incoming_file": src2}, nil)
	if status != http.StatusConflict {
		t.Fatalf("被引用同名导入应 409（不是 %d）: %s", status, data)
	}
	if !strings.Contains(string(data), "引用") || !strings.Contains(string(data), "不能覆盖") {
		t.Fatalf("409 文案应如实说明「被引用、不能覆盖」: %s", data)
	}
	// 仓库文件不得被改动。
	if got, err := os.ReadFile(filepath.Join(store.Config().Dir, "base.qcow2")); err != nil || string(got) != "ORIGINAL" {
		t.Fatalf("仓库文件不得被换底: %v %q", err, got)
	}
}

// TestCLIImagesUploadReferencedFails：CLI 侧同名上传被引用镜像 ⇒ `%%` 如实报错（含「不能覆盖」）。
func TestCLIImagesUploadReferencedFails(t *testing.T) {
	x, _ := newCLIKit(t)
	store := newCLIImagesStore(t)
	// 直接注入一个「引用 base.qcow2」的配置事实（不经提交，聚焦本路径）。
	store.SetConfigSource(refStubConfig{cfg: model.Config{
		VirtualMachineFunctions: []model.VMFunction{{Name: "vm1", Image: "base.qcow2"}},
	}})
	x.setComputeRuntime(nil, nil, nil, nil, store)

	src := filepath.Join(store.Config().IncomingDir, "base.qcow2")
	if err := os.WriteFile(src, []byte("NEW"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := x.Execute("admin", "super-user", "ssh",
		"request images upload name base.qcow2 type vm-image file "+src)
	if !strings.Contains(res.Output, "%%") {
		t.Fatalf("被引用同名上传应报失败: %q", res.Output)
	}
	if !strings.Contains(res.Output, "引用") || !strings.Contains(res.Output, "不能覆盖") {
		t.Fatalf("失败文案应如实说明「被引用、不能覆盖」: %q", res.Output)
	}
	// 未落库、未在仓库留下文件。
	if _, ok := store.Get("base.qcow2"); ok {
		t.Fatal("被引用拒绝不应登记镜像")
	}
	if _, err := os.Stat(filepath.Join(store.Config().Dir, "base.qcow2")); !os.IsNotExist(err) {
		t.Fatalf("被引用拒绝不应在仓库留下文件: %v", err)
	}
}

// refStubConfig committed 配置的桩（只有 Committed）。
type refStubConfig struct{ cfg model.Config }

func (s refStubConfig) Committed() (model.Config, error) { return s.cfg, nil }

// 编译期确认桩满足 *images.Store.SetConfigSource 的参数类型（匿名接口）。
var _ interface {
	SetConfigSource(interface{ Committed() (model.Config, error) })
} = (*images.Store)(nil)
