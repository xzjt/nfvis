package api

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/images"
)

// writeDockerArchive 造最小 docker save 归档（manifest.json 内嵌 tags），写入仓库 incoming 目录，
// 返回归档路径。
func writeDockerArchive(t *testing.T, store *images.Store, tags ...string) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	manifest := `[{"Config":"x.json","RepoTags":["` + strings.Join(tags, `","`) + `"]}]`
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
	return src
}

// TestCLIUploadContainerImageReportsEmbeddedTag 决策 #312/#447：容器镜像上传时读出 tar 内嵌 tag，
// 并如实说明「Docker 侧引用」（按目录项名推导）与配置里用哪个名字——重命名不再静默，
// 且不再恒宣称「已按仓库名重打标签」。
func TestCLIUploadContainerImageReportsEmbeddedTag(t *testing.T) {
	x, _ := newCLIKit(t)
	store := newCLIImagesStore(t)
	store.SetDockerLoader(func(path, name string) error { return nil })
	x.setComputeRuntime(nil, nil, nil, nil, store)

	src := writeDockerArchive(t, store, "alpine:3.20")

	// 目录项名（alpine）与内嵌 tag（alpine:3.20）不一致：应成功，并说明 Docker 侧引用与唯一可用名。
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh",
		"request images upload name alpine type container-image file "+src)
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("容器镜像上传应成功: %s", res.Output)
	}
	if !strings.Contains(res.Output, "归档内嵌 tag 为 alpine:3.20") {
		t.Fatalf("输出应回显归档内嵌 tag: %s", res.Output)
	}
	if !strings.Contains(res.Output, "Docker 侧引用为 alpine:latest") {
		t.Fatalf("输出应说明 Docker 侧引用（目录项名 alpine → alpine:latest）: %s", res.Output)
	}
	if !strings.Contains(res.Output, "配置中请用 alpine 引用") {
		t.Fatalf("输出应说明配置中的唯一可用名: %s", res.Output)
	}
	// 元数据登记内嵌 tag（供 show images detail / REST / Web 回显）。
	m, ok := store.Get("alpine")
	if !ok || len(m.SourceTags) != 1 || m.SourceTags[0] != "alpine:3.20" {
		t.Fatalf("应登记 source_tags，实得 %+v ok=%v", m, ok)
	}
}

// TestCLIUploadContainerImageColonNameRef 决策 #447：目录项名含冒号（alpine:3.20）时它本身
// 就是 Docker 引用——说明里给出该引用，且不再宣称「已按仓库名重打标签 `<名>:latest`」。
func TestCLIUploadContainerImageColonNameRef(t *testing.T) {
	x, _ := newCLIKit(t)
	store := newCLIImagesStore(t)
	store.SetDockerLoader(func(path, name string) error { return nil })
	x.setComputeRuntime(nil, nil, nil, nil, store)

	src := writeDockerArchive(t, store, "alpine:3.20")

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh",
		"request images upload name alpine:3.20 type container-image file "+src)
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("含冒号目录项名的容器镜像上传应成功: %s", res.Output)
	}
	if !strings.Contains(res.Output, "Docker 侧引用为 alpine:3.20") {
		t.Fatalf("含冒号目录项名应原样作为 Docker 侧引用: %s", res.Output)
	}
	if strings.Contains(res.Output, "已按仓库名重打标签") {
		t.Fatalf("说明不再恒宣称「已按仓库名重打标签」: %s", res.Output)
	}
	if !strings.Contains(res.Output, "配置中请用 alpine:3.20 引用") {
		t.Fatalf("配置引用应为目录项名本身: %s", res.Output)
	}
}
