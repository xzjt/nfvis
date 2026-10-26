package system

// round49（审计脱敏收口）：诊断归档的 audit.json 必须脱敏——审计 detail 是自由文本（容器 exec 的命令
// 原文等），可能含口令 / Bearer 令牌 / PEM 私钥正文，而诊断包是**要交给支持人员**的件
// （FR-SEC-007 / 决策 #149 的口径）。同时它仍须是**合法 JSON**（读不出来的分节等于丢了证据）。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const tsPEMBlock = "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
	"b3BlbnNzaC1rZXktdjEAAAABb3BlbnNzaC1rZXktdjEAAAAABG5vbmU\n" +
	"-----END OPENSSH PRIVATE KEY-----"

func TestTechSupportAuditSectionScrubbed(t *testing.T) {
	dir := t.TempDir()
	tsu := NewTechSupport(dir, TechSupportSources{
		Audit: func() (any, error) {
			return []map[string]any{
				{"action": "container.exec", "result": "success", "detail": "exec 容器 db: mysql -p123456 -h 10.0.0.1"},
				{"action": "container.exec", "result": "success", "detail": `exec 容器 api: curl -H 'Authorization: Bearer abc.def.ghi' http://x`},
				{"action": "container.exec", "result": "success", "detail": "exec 容器 api: sh -c 'cat > k.pem <<EOF\n" + tsPEMBlock + "\nEOF'"},
				{"action": "config.commit", "result": "success", "detail": "提交 3 项变更"},
			}, nil
		},
	}, "test")
	f, err := tsu.Generate()
	if err != nil {
		t.Fatalf("生成诊断归档: %v", err)
	}
	section := readTarSection(t, filepath.Join(dir, f.File), "audit.json")

	// ① 仍是合法 JSON：读不出来的分节等于丢证据（宁可少改，也不能改坏结构）
	var tree any
	if err := json.Unmarshal([]byte(section), &tree); err != nil {
		t.Fatalf("audit.json 必须是合法 JSON: %v\n%s", err, section)
	}
	// ② 明文秘密不得出现在归档里
	for _, leak := range []string{"123456", "abc.def.ghi", "b3BlbnNzaC1rZXktdjE"} {
		if strings.Contains(section, leak) {
			t.Fatalf("诊断包里不得有明文秘密 %q:\n%s", leak, section)
		}
	}
	// ③ 脱敏 ≠ 掏空：占位符与**非秘密内容**都要还在（否则这张归档对支持人员没用）
	if !strings.Contains(section, "«已隐藏»") {
		t.Fatalf("应留下脱敏占位: %s", section)
	}
	for _, want := range []string{"提交 3 项变更", "container.exec", "mysql -p"} {
		if !strings.Contains(section, want) {
			t.Fatalf("非秘密内容 %q 应原样保留: %s", want, section)
		}
	}
}

// readTarSection 取归档里某个分节的原文。
func readTarSection(t *testing.T, archive, name string) string {
	t.Helper()
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("读归档: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("解 gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("读 tar: %v", err)
		}
		if h.Name != name {
			continue
		}
		body, _ := io.ReadAll(tr)
		return string(body)
	}
	t.Fatalf("归档里没有分节 %s", name)
	return ""
}
