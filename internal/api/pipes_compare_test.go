package api

// 发现 #4：契约 §3 声明了两种 compare 写法，而**能力早已实现**（Engine.CompareCandidate /
// Engine.Compare），只差管道解析器不认 `compare`——于是操作者在 commit 前看不到自己改了什么
// （事务模型的核心动作）。本文件锁住接线后的语义与"Tab 候选 ↔ 解析器"的一致性。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/schema"
)

func TestSplitPipesAcceptsCompare(t *testing.T) {
	for _, line := range []string{
		"show | compare",
		"show configuration | compare rollback 1",
		"show | compare rollback 9",
	} {
		if _, _, err := splitPipes(line); err != nil {
			t.Fatalf("%q 应被接受: %v", line, err)
		}
	}
	// 非法形态要明确报错（不能静默当普通过滤）
	for _, line := range []string{
		"show | compare rollback",
		"show | compare rollback abc",
		"show | compare rollback 0",
		"show | compare 5",
	} {
		if _, _, err := splitPipes(line); err == nil {
			t.Fatalf("%q 应被拒绝", line)
		}
	}
}

// 配置模式下 `show | compare` 给出 candidate ⇄ committed 的 diff。
func TestCLICompareCandidate(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", "super-user", "ssh", "configure",
		"set system hostname cmp-host")

	out := x.Execute("admin", "super-user", "ssh", "show | compare").Output
	if strings.Contains(out, "%%") {
		t.Fatalf("compare 不应报错:\n%s", out)
	}
	if !strings.Contains(out, "cmp-host") {
		t.Fatalf("diff 应含未提交的改动:\n%s", out)
	}
	// 未进入配置模式时（无编辑态会话）应明确报错，而不是给空 diff
	y, _ := newCLIKit(t)
	if out := y.Execute("admin", "super-user", "ssh", "show | compare").Output; !strings.Contains(out, "%%") {
		t.Fatalf("无编辑态会话时应报错:\n%s", out)
	}
}

// 操作模式 `show configuration | compare rollback <n>` 给出 committed ⇄ 历史快照的 diff。
func TestCLICompareRollback(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", "super-user", "ssh", "configure",
		"set system hostname rev1-host", "commit")
	run(t, x, "admin", "super-user", "ssh", "configure",
		"set system hostname rev2-host", "commit")

	out := x.Execute("admin", "super-user", "ssh", "show configuration | compare rollback 1").Output
	if strings.Contains(out, "%%") {
		t.Fatalf("compare rollback 不应报错:\n%s", out)
	}
	// 上一版是 rev1-host，当前是 rev2-host → diff 必须体现这次改名
	if !strings.Contains(out, "rev2-host") {
		t.Fatalf("diff 应体现两版差异:\n%s", out)
	}
	// 序号越界要给出可读错误
	if out := x.Execute("admin", "super-user", "ssh", "show configuration | compare rollback 99").Output; !strings.Contains(out, "%%") {
		t.Fatalf("越界序号应报错:\n%s", out)
	}
}

// compare 之后仍可继续接过滤管道（管道自左向右）。
func TestCLICompareThenFilter(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", "super-user", "ssh", "configure", "set system hostname cmp2-host")
	out := x.Execute("admin", "super-user", "ssh", "show | compare | count").Output
	if !strings.Contains(out, "计数:") {
		t.Fatalf("compare 之后应能继续 count:\n%s", out)
	}
}

// 守护：Tab 候选（schema.PipeKeywords）与解析器（splitPipes）必须同源——
// 两处漂移会让「?/Tab 列出的关键字」与「实际能用」不一致（本项目的老毛病）。
func TestPipeKeywordsMatchParser(t *testing.T) {
	for _, kw := range schema.PipeKeywords {
		if _, _, err := splitPipes("show | " + kw); err != nil {
			// 参数形式不同会报参数错，但**不得**报「未知管道」
			if strings.Contains(err.Error(), "未知管道") {
				t.Fatalf("Tab 候选里的 %q 解析器不认: %v", kw, err)
			}
		}
	}
}
