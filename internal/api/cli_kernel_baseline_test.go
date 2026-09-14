package api

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	ksys "github.com/xzjt/nfvis/internal/system"
)

// TestKernelBaselineApplyAndShow request system kernel apply 写入基线并在
// show system kernel 中反映一致性/pending；rollback 工作。
func TestKernelBaselineApplyAndShow(t *testing.T) {
	x, _ := newCLIKit(t)
	root := t.TempDir()
	calls := 0
	x.setKernel(&ksys.BaselineApplier{Root: root, Runner: func(string, ...string) error { calls++; return nil }})

	// 配置大页与隔离核（resource-pools 是唯一真源）
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set resource-pools hugepages page-size 1G count 8",
		"set resource-pools cpu isolated-cores 4-15",
		"commit",
		"exit",
	)
	// show system kernel：三方对照可读（测试环境 /proc 为真实宿主，差异必然存在）
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show system kernel").Output
	if !strings.Contains(out, "配置期望") || !strings.Contains(out, "isolcpus=4-15") {
		t.Fatalf("show system kernel 输出异常: %s", out)
	}
	// apply 写基线
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system kernel apply").Output
	if !strings.Contains(out, "内核基线已写入") {
		t.Fatalf("apply 失败: %s", out)
	}
	if calls != 1 {
		t.Fatalf("应调用一次 update-grub，实际 %d", calls)
	}
	// rollback
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system kernel rollback").Output
	if !strings.Contains(out, "需重启生效") {
		t.Fatalf("rollback 失败: %s", out)
	}
}

// TestKernelBaselineUnavailable 未装配时明确报未接入。
func TestKernelBaselineUnavailable(t *testing.T) {
	x, _ := newCLIKit(t)
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system kernel apply").Output
	if !strings.Contains(out, "未装配") {
		t.Fatalf("未装配应明确提示: %s", out)
	}
}
