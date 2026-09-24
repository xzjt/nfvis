package api

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/schema"
)

// 《CLI 命令全表》里 `request` 族的 **class 列**必须与命令树的 `RequiredClass()` 一致。
//
// 为什么单独守这一族：`Su()`/`Op()` 标记都挂在 `request` 子树里（`request system reboot`、`request vpp`、
// `request images delete`…），而 round67（决策 #144）正是从"运行期判定与声明不一致"这条线上查出来的
// ——声明本身也可能与文档漂移：round69 就发现 `request system kernel apply|rollback` 的**代码树漏了 Su()**，
// 而《命令树》§3 与《命令全表》都写 S（operator 因此能写 GRUB 启动参数）。
//
// 判据：文档说 R/O/S，代码树就必须算出同一个 class（两侧同源）。命令文本里的占位符（`<ifname>`）与
// 可选组（`[to <path>]`）按"填探针值 / 整组略过"解析——与操作者实际敲命令的形态一致。
func TestRequestClassMatchesCommandTable(t *testing.T) {
	b, err := os.ReadFile("../../docs/NFViS-CLI命令全表.md")
	if err != nil {
		t.Fatalf("读取命令全表: %v", err)
	}
	want := map[string]schema.Class{"R": schema.ClassReadOnly, "O": schema.ClassOperator, "S": schema.ClassSuperUser}

	rows, checked, skipped := 0, 0, 0
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "| `request ") {
			continue
		}
		rows++
		// 单元格以 " | " 分隔；命令里的多选写成 `\|`（两侧无空格），故这样切是安全的。
		parts := strings.Split(line, " | ")
		if len(parts) < 4 {
			t.Errorf("命令全表这行切不出 4 个单元格：%s", line)
			continue
		}
		cmd := strings.Trim(strings.TrimPrefix(parts[0], "| "), "`")
		classLetter := strings.TrimSpace(parts[2])
		wantCls, ok := want[classLetter]
		if !ok {
			t.Errorf("%q 的 class 列是 %q（应为 R/O/S）", cmd, classLetter)
			continue
		}

		toks, ok := tokensOfTableCommand(cmd)
		if !ok {
			skipped++ // 含无法机械展开的写法（如 `a|b` 多选）——单独登记，不静默跳过
			t.Logf("跳过（写法无法机械展开）：%s", cmd)
			continue
		}
		n, _, err := schema.Match(schema.OperRoot(), toks)
		if err != nil {
			t.Errorf("%q（token=%v）在命令树里解析不到：%v", cmd, toks, err)
			continue
		}
		checked++
		if got := n.RequiredClass(); got != wantCls {
			t.Errorf("命令 %q：《命令全表》写 %s，命令树算出来是 %v——两侧必须同源（要么补 Su()/Op()，要么改表并说明理由）",
				cmd, classLetter, got)
		}
	}
	if rows < 40 {
		t.Fatalf("只读到 %d 行 request 命令——解析器可能失效（表格式变了？）", rows)
	}
	if skipped > 3 {
		t.Errorf("跳过 %d 行（上限 3）——跳过太多说明解析器跟不上表里的写法，本守护会失去判别力", skipped)
	}
	t.Logf("已核对 %d 条 request 命令（跳过 %d 条），全部与命令树一致", checked, skipped)
}

var tablePlaceholderRe = regexp.MustCompile(`^<.*>$`)

// tokensOfTableCommand 把全表里的命令文本转成可解析的 token 序列：
// 占位符（`<x>`、`<x|y>`）→ 探针取值；可选组（`[to <path>]`）→ 整组略过；
// 含关键字级多选（`a|b`）的行返回 ok=false（机械展开会有歧义，交人工）。
func tokensOfTableCommand(cmd string) ([]string, bool) {
	var out []string
	depth := 0
	for _, tk := range strings.Fields(cmd) {
		if strings.HasPrefix(tk, "[") {
			depth++
		}
		if depth == 0 {
			if tablePlaceholderRe.MatchString(tk) {
				out = append(out, "probe")
			} else if strings.Contains(tk, "|") {
				return nil, false // 关键字级多选：交人工判读
			} else {
				out = append(out, tk)
			}
		}
		if strings.HasSuffix(tk, "]") && depth > 0 {
			depth--
		}
	}
	return out, len(out) > 0
}
