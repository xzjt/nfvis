package images

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeTar 构造最小 tar（tests 夹具）：name → 内容。
func writeTar(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestReadDockerArchiveTags tar 内嵌 tag 解析（决策 #312）：有 tag、无 tag、缺 manifest、非 tar。
func TestReadDockerArchiveTags(t *testing.T) {
	// docker save 的 manifest.json：RepoTags 数组。
	withTags := writeTar(t, map[string]string{
		"manifest.json": `[{"Config":"x.json","RepoTags":["alpine:3.20"],"Layers":["a/layer.tar"]}]`,
		"x.json":        `{}`,
	})
	got, err := ReadDockerArchiveTagsFrom(bytes.NewReader(withTags))
	if err != nil || !reflect.DeepEqual(got, []string{"alpine:3.20"}) {
		t.Fatalf("应解析出内嵌 tag，实得 %#v err=%v", got, err)
	}

	// 未打标签的归档：RepoTags 为 null → 空列表（不报错，如实说没有）。
	untagged := writeTar(t, map[string]string{
		"manifest.json": `[{"Config":"x.json","RepoTags":null}]`,
	})
	got, err = ReadDockerArchiveTagsFrom(bytes.NewReader(untagged))
	if err != nil || len(got) != 0 {
		t.Fatalf("未打标签的归档应返回空列表且不报错，实得 %#v err=%v", got, err)
	}

	// 多条 tag + 重复去重（保持声明序）。
	multi := writeTar(t, map[string]string{
		"manifest.json": `[{"RepoTags":["a:1","b:2"]},{"RepoTags":["a:1"]}]`,
	})
	got, err = ReadDockerArchiveTagsFrom(bytes.NewReader(multi))
	if err != nil || !reflect.DeepEqual(got, []string{"a:1", "b:2"}) {
		t.Fatalf("多条 tag 应保序去重，实得 %#v err=%v", got, err)
	}

	// 缺 manifest.json（不是 docker save 产物）→ 明确报错（调用方据此说明「未解析到」）。
	noManifest := writeTar(t, map[string]string{"a/layer.tar": "x"})
	if _, err := ReadDockerArchiveTagsFrom(bytes.NewReader(noManifest)); err == nil {
		t.Fatal("缺 manifest.json 应报错")
	}

	// 非 tar 内容 → 报错，不 panic。
	if _, err := ReadDockerArchiveTagsFrom(bytes.NewReader([]byte("not a tar at all"))); err == nil {
		t.Fatal("非 tar 内容应报错")
	}
}

// TestReadDockerArchiveTagsFromFile 文件入口（导入路径实际调用它）。
func TestReadDockerArchiveTagsFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.tar")
	if err := os.WriteFile(p, writeTar(t, map[string]string{
		"manifest.json": `[{"RepoTags":["alpine:3.20"]}]`,
	}), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDockerArchiveTags(p)
	if err != nil || len(got) != 1 || got[0] != "alpine:3.20" {
		t.Fatalf("文件入口应读出 tag，实得 %#v err=%v", got, err)
	}
	if _, err := ReadDockerArchiveTags(filepath.Join(dir, "missing.tar")); err == nil {
		t.Fatal("文件不存在应报错")
	}
}

// TestImportContainerRecordsSourceTagsAndRetagConsistency 决策 #312：
// ① 导入把归档内嵌 tag 记入 SourceTags（目录项名与 tag 不一致时也有据可查）；
// ② 传给 docker loader 的名字 == 仓库登记的名字（即重打标签 `<名>:latest` 与候选/校验同源，
//
//	不会出现「候选里有但它用不了」的假绿）。
func TestImportContainerRecordsSourceTagsAndRetagConsistency(t *testing.T) {
	s := newStore(t)
	inc := s.Config().IncomingDir
	archive := filepath.Join(inc, "a.tar")
	if err := os.WriteFile(archive, writeTar(t, map[string]string{
		"manifest.json": `[{"RepoTags":["alpine:3.20"]}]`,
	}), 0o644); err != nil {
		t.Fatal(err)
	}

	var loadedName string
	s.SetDockerLoader(func(path, name string) error { loadedName = name; return nil })

	// 目录项名（alpine）与内嵌 tag（alpine:3.20）不一致——正是登记里的那个场景。
	const repoName = "alpine"
	m, err := s.ImportIncoming(repoName, TypeContainer, archive, "")
	if err != nil {
		t.Fatalf("导入应成功（目录项名可用）: %v", err)
	}
	if !reflect.DeepEqual(m.SourceTags, []string{"alpine:3.20"}) {
		t.Fatalf("应登记归档内嵌 tag，实得 %#v", m.SourceTags)
	}
	if loadedName != repoName {
		t.Fatalf("docker loader 的名字应等于仓库登记名（保证重打标签与候选同源）: %q != %q", loadedName, repoName)
	}
	if m.Name != repoName {
		t.Fatalf("登记名应为 %q，实得 %q", repoName, m.Name)
	}
	names := s.Names()
	if len(names) != 1 || names[0] != repoName {
		t.Fatalf("候选（Names）应恰为登记名 %q，实得 %#v", repoName, names)
	}
	if info, ok := s.Lookup(repoName); !ok || info.Type != TypeContainer {
		t.Fatalf("Lookup(%q) 应命中 container-image，实得 %+v ok=%v", repoName, info, ok)
	}
	// 归档里没有 manifest.json 时：导入照旧成功（放行看 docker load），SourceTags 为空——
	// 如实说「没有」，不编造。
	archive2 := filepath.Join(inc, "b.tar")
	if err := os.WriteFile(archive2, writeTar(t, map[string]string{"a/layer.tar": "x"}), 0o644); err != nil {
		t.Fatal(err)
	}
	m2, err := s.ImportIncoming("plain", TypeContainer, archive2, "")
	if err != nil {
		t.Fatalf("缺 manifest.json 不应阻断导入: %v", err)
	}
	if len(m2.SourceTags) != 0 {
		t.Fatalf("未解析到 manifest 时 SourceTags 应为空，实得 %#v", m2.SourceTags)
	}
	if !strings.Contains(m2.Name, "plain") {
		t.Fatalf("登记名不符: %q", m2.Name)
	}
}
