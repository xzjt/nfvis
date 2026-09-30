package api

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

// TestCLIUploadContainerImageReportsEmbeddedTag 决策 #312：容器镜像上传时读出 tar 内嵌 tag
// 并在输出里明确告知「按仓库名重打标签、配置里用哪个名字」——重命名不再静默。
func TestCLIUploadContainerImageReportsEmbeddedTag(t *testing.T) {
	x, _ := newCLIKit(t)
	store := newCLIImagesStore(t)
	store.SetDockerLoader(func(path, name string) error { return nil })
	x.setComputeRuntime(nil, nil, nil, nil, store)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	manifest := `[{"Config":"x.json","RepoTags":["alpine:3.20"]}]`
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(manifest))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(manifest)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(store.Config().IncomingDir, "alpine.tar")
	if err := os.WriteFile(src, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	// 目录项名（alpine）与内嵌 tag（alpine:3.20）不一致：应成功，并说明内嵌 tag 与唯一可用名。
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh",
		"request images upload name alpine type container-image file "+src)
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("容器镜像上传应成功: %s", res.Output)
	}
	if !strings.Contains(res.Output, "alpine:3.20") {
		t.Fatalf("输出应回显归档内嵌 tag: %s", res.Output)
	}
	if !strings.Contains(res.Output, "alpine:latest") || !strings.Contains(res.Output, "请用 alpine 引用") {
		t.Fatalf("输出应说明重打标签与唯一可用名: %s", res.Output)
	}
	// 元数据登记内嵌 tag（供 show images detail / REST / Web 回显）。
	m, ok := store.Get("alpine")
	if !ok || len(m.SourceTags) != 1 || m.SourceTags[0] != "alpine:3.20" {
		t.Fatalf("应登记 source_tags，实得 %+v ok=%v", m, ok)
	}
}
