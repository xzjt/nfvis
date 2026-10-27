package images

// 决策 #514（round50 只读评审，接 #505）：镜像**发布防覆盖被引用项** + Lookup/Names 只认 ready。
//
// 成因：ImportIncoming 与 URL Download 直接以同名覆盖仓库文件，引用保护只在 Delete；而 VM 主盘是
// `qemu-img create -b <仓库路径>` 的 backing 克隆——被引用镜像被换底后既有 VM 重启/重建会读到
// 错误底层（且事后删除会被「被引用」拒绝，无回退路径）。Lookup 还把 downloading 当可用，
// 下载中即可被新 VNF 引用。
//
// 本组测试钉三件事：
//  ① 同名镜像已被 committed 配置引用时，incoming 导入拒绝且**仓库文件与索引一字不改**；
//  ② 无引用时同名覆盖照旧放行（回归）；
//  ③ URL 拉取在**发布前**（改名/加载前）复核——拉取期间新增的引用同样挡住，不覆盖既有文件；
//  ④ Lookup/Names 只认 ready（downloading/failed 一律不可用）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// fakeRefConfig committed 配置的桩：Committed 返回给定配置（引用计数由 RefCount 现算）。
// refs 用原子 flag 控制，便于「下载期间配置新增引用」的时序复核。
type fakeRefConfig struct {
	cfg  model.Config
	refs *atomic.Bool // 非 nil 时：仅当为 true 才报告引用（默认按 cfg 静态返回）
}

func (f fakeRefConfig) Committed() (model.Config, error) {
	if f.refs != nil && f.refs.Load() {
		return model.Config{
			VirtualMachineFunctions: []model.VMFunction{{Name: "vm-ref", Image: "held.qcow2"}},
		}, nil
	}
	return f.cfg, nil
}

// importFile 造一个 incoming 源文件并导入（返回导入结果）。
func importFile(t *testing.T, s *Store, name, content string) {
	t.Helper()
	src := filepath.Join(s.Config().IncomingDir, "src-"+name)
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportIncoming(name, TypeVM, src, ""); err != nil {
		t.Fatalf("预置导入 %s: %v", name, err)
	}
}

// ① 被引用镜像：incoming 导入拒绝（ErrReferenced），仓库文件与索引**均未变**。
func TestImportIncomingRejectsReferencedName(t *testing.T) {
	s := newStore(t)
	importFile(t, s, "held.qcow2", "ORIGINAL-CONTENT")
	// 让配置引用它（引用计数 1）。
	s.SetConfigSource(fakeRefConfig{cfg: model.Config{
		VirtualMachineFunctions: []model.VMFunction{{Name: "vm1", Image: "held.qcow2"}},
	}})

	warehouse := filepath.Join(s.Config().Dir, "held.qcow2")
	indexPath := filepath.Join(s.Config().Dir, "index.json")
	beforeFile, err := os.ReadFile(warehouse)
	if err != nil {
		t.Fatal(err)
	}
	beforeIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(s.Config().IncomingDir, "new.qcow2")
	if err := os.WriteFile(src, []byte("REPLACEMENT-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = s.ImportIncoming("held.qcow2", TypeVM, src, "")
	if err == nil {
		t.Fatal("被引用镜像的同名导入必须拒绝")
	}
	if !strings.Contains(err.Error(), "引用") || !strings.Contains(err.Error(), "不能覆盖") {
		t.Fatalf("错误应如实说明「被引用、不能覆盖」: %v", err)
	}
	// 拒绝本身用 ErrReferenced（HTTP 409 映射靠它）。
	if m, oerr := os.ReadFile(warehouse); oerr != nil || string(m) != string(beforeFile) {
		t.Fatalf("仓库文件不得被改动: %v %q", oerr, m)
	}
	if b, oerr := os.ReadFile(indexPath); oerr != nil || string(b) != string(beforeIndex) {
		t.Fatalf("索引不得被改动: %v %s", oerr, b)
	}
	// 元数据仍指向原内容（大小 = 原内容长度）。
	if m, ok := s.Get("held.qcow2"); !ok || m.SizeBytes != int64(len(beforeFile)) {
		t.Fatalf("元数据不得被改动: %+v ok=%v", m, ok)
	}
}

// ② 无引用时同名覆盖照旧放行（回归：不误伤合法替换）。
func TestImportIncomingAllowsUnreferencedReplace(t *testing.T) {
	s := newStore(t)
	importFile(t, s, "free.qcow2", "OLD")
	// 配置里没有任何引用（空配置）。
	s.SetConfigSource(fakeRefConfig{cfg: model.Config{}})

	src := filepath.Join(s.Config().IncomingDir, "repl.qcow2")
	if err := os.WriteFile(src, []byte("NEW-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := s.ImportIncoming("free.qcow2", TypeVM, src, "")
	if err != nil {
		t.Fatalf("无引用同名覆盖应放行: %v", err)
	}
	if m.SizeBytes != int64(len("NEW-CONTENT")) {
		t.Fatalf("覆盖后元数据应为新内容: %+v", m)
	}
	got, err := os.ReadFile(filepath.Join(s.Config().Dir, "free.qcow2"))
	if err != nil || string(got) != "NEW-CONTENT" {
		t.Fatalf("仓库文件应被替换: %v %q", err, got)
	}
}

// ③ URL 拉取在**发布前**复核：拉取期间配置新增了引用 ⇒ 改名之前被挡，既有仓库文件不动。
//
// 时序用进度回调制造确定性：读到最后一个字节（written==total）时把配置置为「已引用」，
// 于是 Download 里的**第二道**复核（改名之前）必然看到引用——这正是「若下载期间配置新增了
// 引用，发布前复核必须挡住」那条要求。
func TestDownloadPublishRejectsReferencedName(t *testing.T) {
	content := []byte(strings.Repeat("NEW-IMAGE-", 300))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	s := newStore(t)
	importFile(t, s, "held.qcow2", "ORIGINAL") // 仓库里已有同名文件（会被换底的目标）
	refs := &atomic.Bool{}
	s.SetConfigSource(fakeRefConfig{refs: refs})

	_, err := s.Download(context.Background(), DownloadOptions{
		Name: "held.qcow2", Type: TypeVM, URL: srv.URL + "/held.qcow2", SHA256: sha256Hex(content),
		Progress: func(done, total int64) { refs.Store(true) }, // 下载中配置新增引用
	})
	if err == nil {
		t.Fatal("拉取期间新增引用的同名镜像必须拒绝发布")
	}
	if !strings.Contains(err.Error(), "引用") || !strings.Contains(err.Error(), "不能覆盖") {
		t.Fatalf("错误应如实说明「被引用、不能覆盖」: %v", err)
	}
	// 既有仓库文件**不得被换底**（这是本决策要防的致命态）。
	got, rerr := os.ReadFile(filepath.Join(s.Config().Dir, "held.qcow2"))
	if rerr != nil || string(got) != "ORIGINAL" {
		t.Fatalf("既有仓库文件不得被换底: %v %q", rerr, got)
	}
}

// ③补充：受理前（第一道）复核——同名镜像一开始就已被引用时，Download 立刻拒绝且**不触碰索引**：
// 既有 ready 条目必须保持 ready（不能因为一次失败的覆盖尝试把它改成 downloading/failed）。
func TestDownloadRejectsReferencedBeforeTouchingIndex(t *testing.T) {
	s := newStore(t)
	importFile(t, s, "held.qcow2", "ORIGINAL")
	s.SetConfigSource(fakeRefConfig{cfg: model.Config{
		VirtualMachineFunctions: []model.VMFunction{{Name: "vm1", Image: "held.qcow2"}},
	}})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("SHOULD-NOT-BE-USED"))
	}))
	defer srv.Close()

	_, err := s.Download(context.Background(), DownloadOptions{
		Name: "held.qcow2", Type: TypeVM, URL: srv.URL, SHA256: strings.Repeat("a", 64),
	})
	if err == nil || !strings.Contains(err.Error(), "引用") {
		t.Fatalf("受理前复核应拒绝: %v", err)
	}
	if m, ok := s.Get("held.qcow2"); !ok || m.ImportState != StateReady {
		t.Fatalf("既有 ready 条目必须保持 ready（失败尝试不得改写它）: %+v ok=%v", m, ok)
	}
}

// ④ Lookup/Names 只认 ready：downloading 与 failed 一律不可用。
func TestLookupRequiresReady(t *testing.T) {
	s := newStore(t)
	for _, m := range []Meta{
		{Name: "ready.qcow2", Type: TypeVM, ImportState: StateReady},
		{Name: "downloading.qcow2", Type: TypeVM, ImportState: StateDownloading},
		{Name: "failed.qcow2", Type: TypeVM, ImportState: StateFailed},
	} {
		if err := s.setMeta(m); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := s.Lookup("ready.qcow2"); !ok {
		t.Fatal("ready 应可用")
	}
	if _, ok := s.Lookup("downloading.qcow2"); ok {
		t.Fatal("downloading 不应可用（下载中不能被新 VNF 引用）")
	}
	if _, ok := s.Lookup("failed.qcow2"); ok {
		t.Fatal("failed 不应可用")
	}

	names := map[string]bool{}
	for _, n := range s.Names() {
		names[n] = true
	}
	if !names["ready.qcow2"] {
		t.Fatalf("ready 应出现在可用名清单: %v", s.Names())
	}
	if names["downloading.qcow2"] || names["failed.qcow2"] {
		t.Fatalf("downloading/failed 不应出现在可用名清单: %v", s.Names())
	}
	// List（读视图）仍如实列出全部条目（含 downloading/failed）——只有「可用性判定」收窄。
	if len(s.List()) != 3 {
		t.Fatalf("List 应仍列出全部条目（3）: %+v", s.List())
	}
}
