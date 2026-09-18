package api

import (
	"strings"
	"testing"
)

// 发现 #12(b)：删掉管理口最后一个字段后必须回收空壳——留 `management: {}` 会让
// 「配置过又删除」与「从未配置」变成两种状态（`show interfaces management` 分支不同、
// 后续变更还会开始要求 commit confirmed）。
func TestDeleteManagementPrunesEmptyShell(t *testing.T) {
	x, eng := newCLIKit(t)
	// 首次声明管理口也须 commit confirmed（发现 #12(a)），确认后才算落地
	run(t, x, "admin", "super-user", "ssh",
		"configure", "set system management interface ens160", "commit confirmed 5")
	run(t, x, "admin", "super-user", "ssh", "configure", "commit")

	cfg, err := eng.Committed()
	if err != nil || cfg.System == nil || cfg.System.Management == nil {
		t.Fatalf("管理口应已配置: %+v %v", cfg.System, err)
	}
	// 两步：删字段（需 commit confirmed，因管理口变更会切断会话）→ 应连空壳一起回收
	run(t, x, "admin", "super-user", "ssh", "configure", "delete system management interface")
	if out := x.Execute("admin", "super-user", "ssh", "commit confirmed 5").Output; strings.Contains(out, "%%") {
		t.Fatalf("commit confirmed 应成功: %s", out)
	}
	run(t, x, "admin", "super-user", "ssh", "configure", "commit")
	cfg, err = eng.Committed()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.System != nil && cfg.System.Management != nil {
		t.Fatalf("删除最后一个字段后不应留下空壳: %+v", cfg.System.Management)
	}
}

// 发现 #16：`rollback 0` 要直接点明「丢弃改动请用 discard」。
func TestRollbackZeroPointsToDiscard(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", "super-user", "ssh", "configure") // 多行由 CLI 前端拆分，单测逐行执行
	out := x.Execute("admin", "super-user", "ssh", "rollback 0").Output
	if !strings.Contains(out, "discard") {
		t.Fatalf("rollback 0 的报错应指向 discard: %s", out)
	}
	if !strings.Contains(out, "从 1 起") {
		t.Fatalf("应说明编号从 1 起: %s", out)
	}
}
