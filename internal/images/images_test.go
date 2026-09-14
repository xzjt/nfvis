package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	inc := filepath.Join(dir, "incoming")
	if err := os.MkdirAll(inc, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(Config{Dir: filepath.Join(dir, "images"), IncomingDir: inc})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sha256Hex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestImportIncoming(t *testing.T) {
	s := newStore(t)
	inc := s.Config().IncomingDir
	content := []byte("qcow2-bytes")
	src := filepath.Join(inc, "upload.qcow2")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := s.ImportIncoming("ubuntu.qcow2", TypeVM, src, "演示")
	if err != nil {
		t.Fatalf("ImportIncoming: %v", err)
	}
	if m.SizeBytes != int64(len(content)) || m.SHA256 != sha256Hex(content) || m.Format != "qcow2" || m.ImportState != StateReady {
		t.Fatalf("元数据不符: %+v", m)
	}
	if m.ImportedAt.IsZero() {
		t.Fatal("应记录导入时间")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("导入成功后应清理 incoming 源文件: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Config().Dir, "ubuntu.qcow2")); err != nil {
		t.Errorf("应落盘到仓库: %v", err)
	}
	// Lookup（engine ImageResolver）
	if info, ok := s.Lookup("ubuntu.qcow2"); !ok || info.Type != TypeVM {
		t.Fatalf("Lookup 不符: %+v %v", info, ok)
	}

	// 越界路径拒绝
	outside := filepath.Join(t.TempDir(), "evil.qcow2")
	_ = os.WriteFile(outside, content, 0o644)
	if _, err := s.ImportIncoming("evil", TypeVM, outside, ""); err == nil || !strings.Contains(err.Error(), "必须位于") {
		t.Fatalf("incoming 目录外文件应拒绝: %v", err)
	}
	// 容器镜像：未接入 Docker 时拒绝
	ctFile := filepath.Join(inc, "alpine.tar")
	_ = os.WriteFile(ctFile, content, 0o644)
	if _, err := s.ImportIncoming("alpine:3.20", TypeContainer, ctFile, ""); err == nil ||
		!strings.Contains(err.Error(), "未接入 Docker") {
		t.Fatalf("未接入 Docker 时容器镜像导入应拒绝: %v", err)
	}
	// 注入 docker load 后：登记元数据、源文件清理、仓库不留文件
	loaded := ""
	s.SetDockerLoader(func(path string) error { loaded = path; return nil })
	m2, err := s.ImportIncoming("alpine:3.20", TypeContainer, ctFile, "")
	if err != nil {
		t.Fatalf("容器镜像导入: %v", err)
	}
	if loaded != ctFile || m2.Type != TypeContainer || m2.Format != "docker-archive" || m2.ImportState != StateReady {
		t.Fatalf("容器镜像导入结果不符: loaded=%s meta=%+v", loaded, m2)
	}
	if _, err := os.Stat(ctFile); !os.IsNotExist(err) {
		t.Errorf("容器镜像导入后应清理 incoming 源文件: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Config().Dir, "alpine:3.20")); !os.IsNotExist(err) {
		t.Errorf("容器镜像不应落盘仓库目录: %v", err)
	}
	s.SetDockerLoader(nil)
	// 非法 type
	if _, err := s.ImportIncoming("x", "bad", filepath.Join(inc, "upload.qcow2"), ""); err == nil {
		t.Fatal("非法 type 应拒绝")
	}
}

func TestDownloadWithSHA256AndFailure(t *testing.T) {
	content := []byte(strings.Repeat("IMAGE-DATA-", 500))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/img.qcow2" {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "img.qcow2", srvTime(), strings.NewReader(string(content)))
	}))
	defer srv.Close()

	s := newStore(t)
	var lastDone, lastTotal int64
	m, err := s.Download(context.Background(), DownloadOptions{
		Name: "img.qcow2", Type: TypeVM, URL: srv.URL + "/img.qcow2", SHA256: sha256Hex(content),
		Progress: func(done, total int64) { lastDone, lastTotal = done, total },
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if m.SHA256 != sha256Hex(content) || m.SizeBytes != int64(len(content)) || m.ImportState != StateReady {
		t.Fatalf("下载元数据不符: %+v", m)
	}
	if lastDone != int64(len(content)) || lastTotal != int64(len(content)) {
		t.Fatalf("进度回调不符: done=%d total=%d", lastDone, lastTotal)
	}

	// sha256 不匹配 → 失败状态 + 不落盘
	_, err = s.Download(context.Background(), DownloadOptions{
		Name: "bad.qcow2", Type: TypeVM, URL: srv.URL + "/img.qcow2",
		SHA256: strings.Repeat("0", 64),
	})
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("sha256 不匹配应报错: %v", err)
	}
	if m, ok := s.Get("bad.qcow2"); !ok || m.ImportState != StateFailed {
		t.Fatalf("失败应记 failed 状态: %+v %v", m, ok)
	}
	if _, err := os.Stat(filepath.Join(s.Config().Dir, "bad.qcow2")); !os.IsNotExist(err) {
		t.Error("校验失败不应留下镜像文件")
	}
	// Lookup 不返回 failed 镜像
	if _, ok := s.Lookup("bad.qcow2"); ok {
		t.Error("failed 镜像不应被 Lookup 命中")
	}

	// 404 → failed
	if _, err := s.Download(context.Background(), DownloadOptions{
		Name: "nope.qcow2", Type: TypeVM, URL: srv.URL + "/missing",
	}); err == nil {
		t.Fatal("404 应报错")
	}
	if _, err := s.Download(context.Background(), DownloadOptions{Name: "", URL: srv.URL}); err == nil {
		t.Fatal("缺 name/url 应报错")
	}
	if _, err := s.Download(context.Background(), DownloadOptions{Name: "x", URL: srv.URL, Type: "bad"}); err == nil {
		t.Fatal("非法 type 应报错")
	}
}

func TestDownloadResumeFromPartial(t *testing.T) {
	content := []byte(strings.Repeat("RESUME-DATA-", 1000))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "big.qcow2", srvTime(), strings.NewReader(string(content)))
	}))
	defer srv.Close()

	s := newStore(t)
	// 预置半截 .part，模拟上次中断。
	part := filepath.Join(s.Config().Dir, "big.qcow2.part")
	if err := os.WriteFile(part, content[:len(content)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := s.Download(context.Background(), DownloadOptions{
		Name: "big.qcow2", Type: TypeVM, URL: srv.URL + "/big.qcow2", SHA256: sha256Hex(content),
	})
	if err != nil {
		t.Fatalf("续传承接失败: %v", err)
	}
	if m.SizeBytes != int64(len(content)) || m.SHA256 != sha256Hex(content) {
		t.Fatalf("续传后内容/摘要不符: %+v（期望 %d）", m, len(content))
	}
	got, err := os.ReadFile(filepath.Join(s.Config().Dir, "big.qcow2"))
	if err != nil || string(got) != string(content) {
		t.Fatalf("续传文件内容不符: %v", err)
	}
}

func TestDeleteReferenceAndDocker(t *testing.T) {
	s := newStore(t)
	inc := s.Config().IncomingDir
	src := filepath.Join(inc, "a.qcow2")
	_ = os.WriteFile(src, []byte("x"), 0o644)
	if _, err := s.ImportIncoming("a.qcow2", TypeVM, src, ""); err != nil {
		t.Fatal(err)
	}
	// 被引用 → 拒绝
	if err := s.Delete("a.qcow2", 1); err == nil || !strings.Contains(err.Error(), "引用") {
		t.Fatalf("被引用镜像删除应报错: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Config().Dir, "a.qcow2")); err != nil {
		t.Fatal("被引用镜像不应被删除")
	}
	// 无引用 → 删除文件与索引
	if err := s.Delete("a.qcow2", 0); err != nil {
		t.Fatalf("删除: %v", err)
	}
	if _, ok := s.Get("a.qcow2"); ok {
		t.Fatal("索引应已移除")
	}
	if err := s.Delete("ghost", 0); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("不存在应报错: %v", err)
	}

	// 容器镜像：经注入的 Docker remover 删除
	called := ""
	s.SetDockerRemover(func(ref string) error { called = ref; return nil })
	if err := s.setMeta(Meta{Name: "alpine:3.20", Type: TypeContainer, ImportState: StateReady}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("alpine:3.20", 0); err != nil {
		t.Fatalf("删除容器镜像: %v", err)
	}
	if called != "alpine:3.20" {
		t.Fatalf("应经 Docker remover 删除: %q", called)
	}
	// 未接入 Docker → 报错
	if err := s.setMeta(Meta{Name: "busybox", Type: TypeContainer, ImportState: StateReady}); err != nil {
		t.Fatal(err)
	}
	s.SetDockerRemover(nil)
	if err := s.Delete("busybox", 0); err == nil || !strings.Contains(err.Error(), "Docker") {
		t.Fatalf("未接入 Docker 应报错: %v", err)
	}
}

func TestRefCountAndList(t *testing.T) {
	cfg := model.Config{
		VirtualMachineFunctions: []model.VMFunction{{Name: "v1", Image: "img-a"}, {Name: "v2", Image: "img-b"}},
		ContainerFunctions:      []model.ContainerFunction{{Name: "c1", Image: "img-a"}},
	}
	if got := RefCount(cfg, "img-a"); got != 2 {
		t.Fatalf("img-a 引用数应为 2，实际 %d", got)
	}
	if got := RefCount(cfg, "none"); got != 0 {
		t.Fatalf("无引用应为 0，实际 %d", got)
	}

	s := newStore(t)
	inc := s.Config().IncomingDir
	for _, n := range []string{"b.qcow2", "a.qcow2"} {
		p := filepath.Join(inc, n)
		_ = os.WriteFile(p, []byte(n), 0o644)
		if _, err := s.ImportIncoming(n, TypeVM, p, ""); err != nil {
			t.Fatal(err)
		}
	}
	list := s.List()
	if len(list) != 2 || list[0].Name != "a.qcow2" || list[1].Name != "b.qcow2" {
		t.Fatalf("List 应按名升序: %+v", list)
	}
	if got := s.Path("a.qcow2"); !strings.HasSuffix(got, "a.qcow2") {
		t.Fatalf("Path: %s", got)
	}
}

func TestHelpers(t *testing.T) {
	if formatOf("x.QCOW2") != "qcow2" || formatOf("noext") != "unknown" {
		t.Fatal("formatOf 不符")
	}
	start, ok := contentRangeStart("bytes 1024-2047/4096")
	if !ok || start != 1024 {
		t.Fatalf("contentRangeStart: %d %v", start, ok)
	}
	if _, ok := contentRangeStart("garbage"); ok {
		t.Fatal("非法 Content-Range 应返回 false")
	}
	if !equalFoldHex("ABCD", "abcd") || equalFoldHex("ab", "cd") {
		t.Fatal("equalFoldHex 不符")
	}
	if h, err := hashReader(strings.NewReader("abc")); err != nil || h != sha256Hex([]byte("abc")) {
		t.Fatalf("hashReader: %s %v", h, err)
	}
}

// srvTime httptest 内容服务的固定修改时间。
func srvTime() time.Time { return time.Unix(0, 0) }

// FR-SEC-004（决策 #71）：URL 拉取**默认强制** sha256——缺省即拒绝，不再静默跳过校验。
func TestDownloadRequiresSHA256(t *testing.T) {
	s := newStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	// 缺 sha256 → 拒绝，且不得先行登记 downloading
	if _, err := s.Download(context.Background(), DownloadOptions{Name: "x.qcow2", URL: srv.URL, Type: TypeVM}); err == nil {
		t.Fatal("URL 拉取缺 sha256 应被拒绝")
	} else if !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("错误信息应提及 sha256: %v", err)
	}
	if list := s.List(); list != nil {
		for _, m := range list {
			if m.Name == "x.qcow2" {
				t.Fatalf("拒绝时不应登记镜像条目: %+v", m)
			}
		}
	}

	// 格式非法（非 64 位十六进制）→ 拒绝
	if _, err := s.Download(context.Background(), DownloadOptions{
		Name: "x.qcow2", URL: srv.URL, Type: TypeVM, SHA256: "deadbeef",
	}); err == nil || !strings.Contains(err.Error(), "64") {
		t.Fatalf("非 64 位 hex 应被拒绝: %v", err)
	}
}

// FR-SEC-004（决策 #71⑤）：ValidateDownloadOptions 为同步可调用的校验入口。
func TestValidateDownloadOptions(t *testing.T) {
	ok := DownloadOptions{Name: "a.qcow2", Type: TypeVM, URL: "http://h/a", SHA256: strings.Repeat("a", 64)}
	if err := ValidateDownloadOptions(ok); err != nil {
		t.Fatalf("合法参数应通过: %v", err)
	}
	cases := []struct {
		name string
		o    DownloadOptions
		want string
	}{
		{"缺 name", DownloadOptions{Type: TypeVM, URL: "http://h/a", SHA256: strings.Repeat("a", 64)}, "name"},
		{"缺 url", DownloadOptions{Name: "a", Type: TypeVM, SHA256: strings.Repeat("a", 64)}, "url"},
		{"类型非法", DownloadOptions{Name: "a", Type: "bad", URL: "http://h/a", SHA256: strings.Repeat("a", 64)}, "type"},
		{"缺 sha256", DownloadOptions{Name: "a", Type: TypeVM, URL: "http://h/a"}, "sha256"},
		{"sha256 非 hex", DownloadOptions{Name: "a", Type: TypeVM, URL: "http://h/a", SHA256: strings.Repeat("z", 64)}, "64"},
		{"sha256 长度不足", DownloadOptions{Name: "a", Type: TypeVM, URL: "http://h/a", SHA256: "abcdef"}, "64"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateDownloadOptions(c.o)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("期望含 %q 的错误，得到 %v", c.want, err)
			}
		})
	}
}
