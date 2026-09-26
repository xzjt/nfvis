package api

// 「已知前缀但形态不合法」的提示口径（round84 R84-11②）。
//
// 操作者敲的是**已知命令的错误形态**（`request images delete foo` 漏了 name 关键字）时，
// 非交互路径（`-c` 脚本 / REST `/cli/execute`）此前只回「无效命令: …（输入 ? 查看可用命令）」：
// 产品明明知道正确写法，却让操作者自己猜；而「输入 ?」只在交互式 REPL 里有用，
// 脚本路径下等于没有提示。**交互路径同款**的 `%% 语法: …` 才是能照敲的答案。
//
// 判据只有一条：前缀在命令树里**存不存在**。
//   - 存在（`request images delete` 是树里的节点）→ 按树列出该前缀下的可用形态；
//   - 不存在（真正未知的命令）→ 维持既有「无效命令」文案，不假装认识它。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

// TestRequestKnownPrefixGivesSyntaxHint：前缀已知 + 形态不合法 → 语法指引（不是「无效命令」）。
func TestRequestKnownPrefixGivesSyntaxHint(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setComputeRuntime(nil, nil, nil, nil, newCLIImagesStore(t))

	cases := []struct {
		cmd  string
		want string // 提示里应出现的可用形态（可直接照敲）
	}{
		// round84 真机实测的那条：漏了 `name` 关键字
		{"request images delete foo", "request images delete name <name>"},
		// 旧位置参数形态（树/契约都不认）
		{"request images delete img1", "request images delete name <name>"},
		// 域下的未知动作：列该域的子形态（`delete` 也要写到取值层，不能只写 `delete …`）
		{"request images bogus", "request images upload … | download … | delete name <name>"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.cmd, func(t *testing.T) {
			out := x.Execute("admin", aaa.ClassSuperUser, "ssh", c.cmd).Output
			if !strings.HasPrefix(out, "%") {
				t.Fatalf("形态不合法应报错（带 %% 前缀）: %q", out)
			}
			if strings.Contains(out, "无效命令") {
				t.Fatalf("前缀在命令树里存在，不该回「无效命令」（应按树给语法指引）: %q", out)
			}
			if !strings.Contains(out, "语法") {
				t.Fatalf("应给语法指引: %q", out)
			}
			if !strings.Contains(out, c.want) {
				t.Fatalf("提示里应给出可照敲的形态 %q，实得 %q", c.want, out)
			}
			// 交互路径的补全入口仍要指出来（两种路径给的是同一套东西）
			if !strings.Contains(out, "?") {
				t.Fatalf("应保留 `?` 补全指引: %q", out)
			}
		})
	}
}

// TestRequestUnknownCommandStillInvalid：真正未知的命令（前缀在树里不存在）**保持**原文案。
// 分级很清楚：认识它（前缀有效）就给正确写法，不认识它就只说明不认识。
func TestRequestUnknownCommandStillInvalid(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setComputeRuntime(nil, nil, nil, nil, newCLIImagesStore(t))

	for _, cmd := range []string{
		"foobar baz",       // 顶层就未知：根节点下没有这个关键字
		"reqimages delete", // 同样未知（缩写只对树里存在的关键字生效）
	} {
		out := x.Execute("admin", aaa.ClassSuperUser, "ssh", cmd).Output
		if !strings.Contains(out, "无效命令") {
			t.Fatalf("%q 应维持「无效命令」文案: %q", cmd, out)
		}
		if strings.Contains(out, "语法") {
			t.Fatalf("%q 不该假装认识（不应给语法指引）: %q", cmd, out)
		}
	}

	// 边界：命令本身已敲完整、只是多了 token —— 前缀已是叶子、无子形态可列，
	// 仍按既有「无效命令」口径（那是「多余参数」，不是「不会写」）。
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request vpp restart extra").Output
	if !strings.Contains(out, "无效命令") {
		t.Fatalf("多余 token 应维持「无效命令」: %q", out)
	}
}

// TestRequestSyntaxHintDoesNotBreakValidForms：提示口径的改动**不得**动到正常路径——
// 完整形态仍走各自的执行路径（删除镜像进确认流程、域校验仍按最深节点的 class 判权限）。
func TestRequestSyntaxHintDoesNotBreakValidForms(t *testing.T) {
	x, _ := newCLIKit(t)

	// 补全路径（`?` 给出的形态）必须真的能执行：`delete name <name>` 进删除确认
	x.setComputeRuntime(nil, nil, nil, nil, newCLIImagesStore(t))
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request images delete name img1").Output
	if !strings.Contains(out, "[yes,no]") {
		t.Fatalf("`request images delete name img1` 应进入删除确认（既有行为不回退）: %q", out)
	}

	// 权限判定仍在树校验之后按**最深节点**判（这里只读 class 发破坏性命令 → 无权限，
	// 且不得因为改了提示就把它说成语法错）
	out = x.Execute("admin", aaa.ClassReadOnly, "ssh", "request images delete name img1").Output
	if !strings.Contains(out, "无权限") {
		t.Fatalf("只读 class 应被权限拦下: %q", out)
	}
	if strings.Contains(out, "语法") {
		t.Fatalf("权限问题不该被说成语法问题: %q", out)
	}
}
