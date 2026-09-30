package archtest

// 守护：deb 的 `Recommends` 必须声明 VM 功能的运行时依赖（决策 #306）。
//
// 由来：`qemu-utils`（qemu-img）与 `cloud-image-utils`（cloud-localds）缺了会让 `DefineVM`
// 直接失败、集成用例静默跳过，而 deb 的 Recommends 曾只列 vpp/libvirt/docker。本守护读
// `Makefile` 里生成 DEBIAN/control 的那行，确保这两个包名**真的在 Recommends 语义里**
// （只补 Recommends、不动 Depends 是决策 #306 的口径）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDebRecommendsDeclaresVMRuntimeDeps(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("读 Makefile: %v", err)
	}
	src := string(data)
	i := strings.Index(src, "Recommends:")
	if i < 0 {
		t.Fatalf("Makefile 里找不到 deb 的 Recommends 声明（生成 control 的那行）")
	}
	// 取到本行结束：control 是单行 printf，字段间是**字面** `\n`（反斜杠+n）。
	line := src[i:]
	if j := strings.Index(line, `\n`); j >= 0 {
		line = line[:j]
	}
	for _, pkg := range []string{"qemu-utils", "cloud-image-utils"} {
		if !strings.Contains(line, pkg) {
			t.Errorf("deb Recommends 缺 %s（VM 功能运行时依赖，决策 #306）：%s", pkg, line)
		}
	}
}
