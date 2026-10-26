package api

// round49（镜像名越界收口）：镜像名越界的**入口**守护（REST 面）。
//
// 仓库层（internal/images）已拒越界名，但入口必须把「名字不合法」如实回 400：
// 此前 REST 原样透传 name，越界名一路走到落盘路径上；且 400/500 的区分也决定
// 操作者能否看出「这是输入问题」而不是「服务器坏了」。

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/images"
)

// POST /images（导入与 URL 拉取两条路）对越界名都回 400，且仓库外文件不受影响。
func TestImagesPostRejectsTraversalName(t *testing.T) {
	store := newImagesStore(t)
	ts := newTestServerOpts(t, Options{Images: store})
	token := loginAdmin(t, ts)

	repo := store.Config().Dir
	outside := filepath.Join(filepath.Dir(repo), "outside.qcow2")
	if err := os.WriteFile(outside, []byte("MUST-STAY"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(store.Config().IncomingDir, "upload.qcow2")
	if err := os.WriteFile(src, []byte("EVIL"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 导入路径（incoming_file）
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/images", token,
		map[string]any{"name": "../outside.qcow2", "type": "vm-image", "incoming_file": src}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("越界名导入应 400: %d %s", status, data)
	}
	if !strings.Contains(string(data), "镜像名不合法") {
		t.Errorf("400 文案应说明名字不合法（可照做）: %s", data)
	}
	assertOutsideUntouched(t, outside)

	// URL 拉取路径（受理前同步校验）
	status, _, data = cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/images", token,
		map[string]any{"name": "../../outside.qcow2", "type": "vm-image",
			"url": "http://127.0.0.1:9/x.qcow2", "sha256": strings.Repeat("a", 64)}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("越界名拉取应 400: %d %s", status, data)
	}
	assertOutsideUntouched(t, outside)
}

// DELETE 拿到 images.ErrInvalidName（历史遗留越界条目）时映射 400，而不是 500。
func TestImagesDeleteInvalidNameMapsBadRequest(t *testing.T) {
	fake := &fakeImagesRuntime{deleteErr: fmt.Errorf("%w：%q 含 `..` 路径段", images.ErrInvalidName, "../outside.qcow2")}
	ts := newTestServerOpts(t, Options{Images: fake})
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodDelete, ts.URL+APIPrefix+"/images/legacy.qcow2", token, nil, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("名字不合法应 400（不是 500）: %d %s", status, data)
	}
	if !strings.Contains(string(data), "镜像名不合法") {
		t.Errorf("400 文案应说明名字不合法: %s", data)
	}
}

// CLI 三处入口（upload/download/delete）都经仓库层校验：越界名一律 %%、仓库外文件不动，
// 合法名照旧可用（别把限制收得比服务端更紧）。
func TestCLIImagesRejectsTraversalName(t *testing.T) {
	x, _ := newCLIKit(t)
	store := newImagesStore(t)
	x.setComputeRuntime(nil, nil, nil, nil, store)

	repo := store.Config().Dir
	outside := filepath.Join(filepath.Dir(repo), "outside.qcow2")
	if err := os.WriteFile(outside, []byte("MUST-STAY"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(store.Config().IncomingDir, "upload.qcow2")
	if err := os.WriteFile(src, []byte("EVIL"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := func(line string) string { return x.Execute("admin", aaa.ClassSuperUser, "ssh", line).Output }

	res := exec("request images upload name ../outside.qcow2 type vm-image file " + src)
	if !strings.Contains(res, "%%") || !strings.Contains(res, "镜像名不合法") {
		t.Fatalf("CLI 导入越界名应报失败并说明原因: %s", res)
	}
	res = exec("request images download name ../../outside.qcow2 type vm-image url http://127.0.0.1:9/x.qcow2 sha256 " +
		strings.Repeat("a", 64))
	if !strings.Contains(res, "%%") || !strings.Contains(res, "镜像名不合法") {
		t.Fatalf("CLI 拉取越界名应报失败并说明原因: %s", res)
	}
	if res := exec("request images delete name ../outside.qcow2 --yes"); !strings.Contains(res, "%%") {
		t.Fatalf("CLI 删除越界名应报失败: %s", res)
	}
	assertOutsideUntouched(t, outside)

	// 合法名照旧：导入成功、文件落在仓库里、源文件被清理。
	res = exec("request images upload name alpine.qcow2 type vm-image file " + src)
	if strings.Contains(res, "%%") {
		t.Fatalf("合法名不应被拒: %s", res)
	}
	if _, err := os.Stat(filepath.Join(repo, "alpine.qcow2")); err != nil {
		t.Fatalf("合法名应落盘到仓库: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("导入成功后应清理 incoming 源文件: %v", err)
	}
}

// fakeImagesRuntime 只用于端点映射的定向测试（其余方法不在本测试的路径上）。
type fakeImagesRuntime struct{ deleteErr error }

func (f *fakeImagesRuntime) List() []images.Meta { return nil }

func (f *fakeImagesRuntime) Get(name string) (images.Meta, bool) {
	return images.Meta{Name: name, Type: images.TypeVM, ImportState: images.StateReady}, true
}

func (f *fakeImagesRuntime) Download(context.Context, images.DownloadOptions) (images.Meta, error) {
	return images.Meta{}, nil
}

func (f *fakeImagesRuntime) ImportIncoming(name, typ, incomingFile, description string) (images.Meta, error) {
	return images.Meta{}, nil
}

func (f *fakeImagesRuntime) Delete(name string, refCount int) error { return f.deleteErr }

func assertOutsideUntouched(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("仓库外文件被动到（读不到即被删/改名）: %v", err)
	}
	if string(b) != "MUST-STAY" {
		t.Fatalf("仓库外文件内容被改: %q", b)
	}
}
