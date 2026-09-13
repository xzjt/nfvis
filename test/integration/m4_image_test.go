//go:build integration

// M4-8 真机集成测试：URL 拉取（含断点续传/sha256）+ incoming 导入 + 导入 VM + 删除被引用镜像报错。
// 用本地 httptest 提供 qcow2 内容，避免依赖外网；仓库用临时目录。
package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/images"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/compute"
)

func TestImageRepoDownloadImportAndReferenceRealLibvirt(t *testing.T) {
	base := t.TempDir()
	inc := filepath.Join(base, "incoming")
	repo := filepath.Join(base, "images")
	vms := filepath.Join(base, "vms")
	for _, d := range []string{inc, repo, vms} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// "远端" qcow2 内容：用 qemu-img 生成真实 qcow2，保证 compute 侧可作 backing 克隆。
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skipf("跳过：未找到 qemu-img: %v", err)
	}
	tmpImg := filepath.Join(base, "seed.qcow2")
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", tmpImg, "16M").CombinedOutput(); err != nil {
		t.Fatalf("建测试 qcow2: %v: %s", err, out)
	}
	content, err := os.ReadFile(tmpImg)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "remote.qcow2", time.Unix(0, 0), strings.NewReader(string(content)))
	}))
	defer srv.Close()

	store, err := images.Open(images.Config{Dir: repo, IncomingDir: inc})
	if err != nil {
		t.Fatal(err)
	}

	// 断点续传：预置半截 .part
	part := filepath.Join(repo, "remote.qcow2.part")
	if err := os.WriteFile(part, content[:len(content)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	meta, err := store.Download(context.Background(), images.DownloadOptions{
		Name: "remote.qcow2", Type: images.TypeVM, URL: srv.URL + "/remote.qcow2",
		SHA256: hex.EncodeToString(sum[:]), Description: "M4-8 验收",
	})
	if err != nil {
		t.Fatalf("URL 拉取（续传）: %v", err)
	}
	if meta.SHA256 != hex.EncodeToString(sum[:]) || meta.SizeBytes != int64(len(content)) || meta.ImportState != images.StateReady {
		t.Fatalf("拉取元数据不符: %+v", meta)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Error("完成后不应残留 .part")
	}
	got, _ := os.ReadFile(filepath.Join(repo, "remote.qcow2"))
	if sha256.Sum256(got) != sum {
		t.Fatal("续传后文件内容与源不一致")
	}
	t.Logf("URL 拉取（断点续传）完成：size=%d sha256=%s", meta.SizeBytes, meta.SHA256[:16])

	// incoming 导入 + 源文件清理
	srcFile := filepath.Join(inc, "local.qcow2")
	if err := os.WriteFile(srcFile, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportIncoming("local.qcow2", images.TypeVM, srcFile, "incoming"); err != nil {
		t.Fatalf("incoming 导入: %v", err)
	}
	if _, err := os.Stat(srcFile); !os.IsNotExist(err) {
		t.Error("导入成功后 incoming 源文件应清理")
	}

	// 导入 VM：用 remote.qcow2 定义域（真实 libvirt；不启动，避免大页依赖）
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cfg := compute.DefaultConfig()
	cfg.URI = os.Getenv("NFVIS_LIBVIRT_URI")
	cfg.VMsDir, cfg.ImagesDir, cfg.VhostDir = vms, repo, filepath.Join(base, "vhost")
	for _, d := range []string{cfg.VMsDir, cfg.VhostDir} {
		_ = os.MkdirAll(d, 0o755)
	}
	p, conn, err := compute.NewConnectedProvider(ctx, cfg)
	if err != nil {
		t.Skipf("跳过 VM 导入环节（libvirt 不可用）: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = p.DeleteVM(context.Background(), "it-m4-8-vm")
	t.Cleanup(func() { _ = p.DeleteVM(context.Background(), "it-m4-8-vm") })

	vm := model.VMFunction{
		Name: "it-m4-8-vm", Image: "remote.qcow2",
		VCPU:   model.VMCpu{Count: 1},
		Memory: model.VMMemory{SizeMB: 256, Backing: "normal"},
	}
	cfgModel := model.Config{VirtualMachineFunctions: []model.VMFunction{vm}}
	if err := p.DefineVM(ctx, vm, model.AllocationFor(cfgModel, vm)); err != nil {
		t.Fatalf("导入 VM（以仓库镜像定义域）: %v", err)
	}
	if st, err := p.VMState(ctx, "it-m4-8-vm"); err != nil || st != "shutoff" {
		t.Fatalf("域应已定义且 shutoff: %q err=%v", st, err)
	}
	t.Log("已用仓库镜像定义 VM domain（shutoff）")

	// 删除被引用镜像 → 报错；删除 VM 后可删
	if got := images.RefCount(cfgModel, "remote.qcow2"); got != 1 {
		t.Fatalf("引用计数应为 1，实际 %d", got)
	}
	if err := store.Delete("remote.qcow2", images.RefCount(cfgModel, "remote.qcow2")); err == nil {
		t.Fatal("删除被引用镜像应报错")
	}
	if err := p.DeleteVM(ctx, "it-m4-8-vm"); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
	if err := store.Delete("remote.qcow2", 0); err != nil {
		t.Fatalf("无引用后删除应成功: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "remote.qcow2")); !os.IsNotExist(err) {
		t.Error("删除后镜像文件应移除")
	}
	t.Log("删除被引用镜像被拒绝，解除引用后删除成功")
}
