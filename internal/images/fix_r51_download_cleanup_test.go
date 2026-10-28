package images

// round51 R51-L10：`docker load` 失败时落盘的容器归档被泄漏在仓库目录里。
//
// 由来：`Download` 的容器分支有两条拒绝路径会 `os.Remove(dest)`（未接入 Docker、发布前复核被拒），
// 但紧挨着的 `dockerLoad` 失败分支直接返回——一个最大到上传上限（4 GiB）的
// `<repo>/.download-<hash>.archive` 永远留在仓库目录；重试也接不上（续传找的是 `<dest>.part`，
// 而这里是一个已完整的归档文件）。
//
// 本用例：URL 拉取一个「docker load 必然失败」的归档，断言仓库目录里不留任何归档残件，
// 且失败状态如实落进元数据（failed + 原因）。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixR51ContainerDownloadRemovesArchiveWhenDockerLoadFails(t *testing.T) {
	content := []byte(strings.Repeat("DOCKER-ARCHIVE-BYTES-", 300))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ct.tar" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	s := newStore(t)
	loaded := ""
	s.SetDockerLoader(func(path, name string) error { loaded = path; return errors.New("load 失败") })

	_, err := s.Download(context.Background(), DownloadOptions{
		Name: "ct", Type: TypeContainer, URL: srv.URL + "/ct.tar", SHA256: sha256Hex(content),
	})
	if err == nil || !strings.Contains(err.Error(), "docker load") {
		t.Fatalf("docker load 失败应如实报错: %v", err)
	}
	if loaded == "" {
		t.Fatal("前提失效：应已尝试 docker load")
	}

	// 关键断言：仓库目录里不得留下任何下载归档（含 .part）。
	repo := s.Config().Dir
	leftovers, gerr := filepath.Glob(filepath.Join(repo, ".download-*"))
	if gerr != nil {
		t.Fatal(gerr)
	}
	if len(leftovers) != 0 {
		t.Fatalf("docker load 失败后不得在仓库目录留下归档残件: %v", leftovers)
	}
	entries, rerr := os.ReadDir(repo)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".archive") || strings.HasSuffix(e.Name(), ".part") {
			t.Fatalf("仓库目录不应残留下载件: %s", e.Name())
		}
	}

	// 失败状态如实落进元数据（操作者能从 show images detail 看到原因）。
	m, ok := s.Get("ct")
	if !ok || m.ImportState != StateFailed || m.LastError == "" {
		t.Fatalf("失败应落成 failed + 原因，实际 %+v (ok=%v)", m, ok)
	}
}
