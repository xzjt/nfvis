// 守护：**给操作者看的文本**里不得出现内部引用（FR-xxx / §x / 决策 #nn / 附录 A #nn）。
//
// 由来（2026-09-15，决策 #86）：CLI 帮助与报错里长期混着需求编号，例如
//
//	`commit   提交 candidate（FR-CFG-002/003）`
//	`%% …（FR-NET-012）`
//
// —— 操作者看到的是「FR-CFG-002/003」，既无信息量也读不通。需求可追溯（AGENTS 规则 2）
// 靠**代码注释与 docs/**，不靠给操作者看的文本；注释与 docs 里的引用是合法的，故本规则
// 只看**字符串字面量**，并跳过 `_test.go`（测试里可以引用编号）与 `prototype/`（演示代码）。
//
// 同时拦住「剥掉引用后留下的标点残渣」——这是本次清理真实踩到的形态：
//
//	（FR-CFG-011⑪）→（⑪）   FR-NET-003/FR-NET-001 → /001
//	（§4：仅 super-user）→（：仅 super-user）
package archtest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 内部引用本身
var refPatterns = []*regexp.Regexp{
	regexp.MustCompile(`FR-[A-Z]+-[0-9]+`),
	regexp.MustCompile(`§[0-9]`),
	regexp.MustCompile(`决策 #[0-9]+`),
	regexp.MustCompile(`附录 A #[0-9]+`),
}

// 剥掉引用后容易留下的残渣（避免「清了引用、留下垃圾」）
var residuePatterns = []*regexp.Regexp{
	regexp.MustCompile(`[\x{2460}-\x{2473}]`), // ①②…⑳
	regexp.MustCompile(`/#[0-9]`),
	regexp.MustCompile(`（[:；，、]`),
	regexp.MustCompile(`[：；，、]）`),
	regexp.MustCompile(`（\s*）`),
	regexp.MustCompile(`，，|：：|；；`),
}

// codePart 去掉行尾注释，返回代码部分（正确处理字符串/字符字面量里的 // 与转义）。
func codePart(line string) string {
	var inStr, inRune bool
	for i := 0; i < len(line); i++ {
		switch {
		case inStr:
			if line[i] == '\\' {
				i++
			} else if line[i] == '"' {
				inStr = false
			}
		case inRune:
			if line[i] == '\\' {
				i++
			} else if line[i] == '\'' {
				inRune = false
			}
		case line[i] == '"':
			inStr = true
		case line[i] == '\'':
			inRune = true
		case line[i] == '/' && i+1 < len(line) && line[i+1] == '/':
			return line[:i]
		}
	}
	return line
}

// TestUserVisibleTextHasNoInternalRefs 操作者可见文本不得含内部引用或剥离残渣。
func TestUserVisibleTextHasNoInternalRefs(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "build", "prototype", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(path)
		for i, line := range strings.Split(string(data), "\n") {
			code := codePart(line)
			if !strings.Contains(code, `"`) {
				continue
			}
			for _, re := range append(append([]*regexp.Regexp{}, refPatterns...), residuePatterns...) {
				if m := re.FindString(code); m != "" {
					offenders = append(offenders, rel+":"+itoa(i+1)+": "+strings.TrimSpace(code))
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历源码: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("操作者可见文本含内部引用/残渣 %d 处（需求可追溯请写在注释与 docs/ 里）：\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
