package api

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// ---------- CLI 执行器（cli_bridge 守护进程侧） ----------

func newCLIKit(t *testing.T) (*cliExecutor, *config.Engine) {
	t.Helper()
	store, err := config.OpenStore(filepath.Join(t.TempDir(), "nfvis.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now
	engine, err := config.NewEngine(store, orchestrator.NewNoopApplier(), config.Options{Now: now})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(engine.Close)
	authz := aaa.NewService(engine, now)
	if _, _, err := aaa.EnsureBootstrapAdmin(engine, authz, "TestPassw0rd!1"); err != nil {
		t.Fatalf("引导 admin: %v", err)
	}
	return newCLIExecutor(engine, authz), engine
}

// run 依次执行多行命令，返回全部输出拼接。
func run(t *testing.T, x *cliExecutor, user, class, source string, lines ...string) string {
	t.Helper()
	var out strings.Builder
	for _, line := range lines {
		res := x.Execute(user, class, source, line)
		out.WriteString(res.Output)
		if strings.Contains(res.Output, "%%") {
			t.Fatalf("命令 %q 失败: %s", line, res.Output)
		}
	}
	return out.String()
}

func TestCLIConfigTransaction(t *testing.T) {
	x, _ := newCLIKit(t)

	// configure → set → show → commit → oper show → exit
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set system hostname cli-node",
		"set resource-pools hugepages page-size 1G count 32",
		"set resource-pools cpu isolated-cores 4-7",
	)

	// config 模式 show 渲染 candidate（JunOS 风格）
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show")
	if !strings.Contains(res.Output, "hostname cli-node") || !strings.Contains(res.Output, "page-size 1G") {
		t.Fatalf("show 应渲染 candidate:\n%s", res.Output)
	}
	if res.Prompt != "nfvis# " {
		t.Fatalf("顶层 config 提示符应为 nfvis#: %q", res.Prompt)
	}

	// 层级导航：edit system 后 show 只见子树
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "edit system")
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show")
	if strings.Contains(res.Output, "resource-pools") || !strings.Contains(res.Output, "cli-node") {
		t.Fatalf("[edit system] show 应只见子树:\n%s", res.Output)
	}
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "top")

	// 提交
	out := run(t, x, "admin", aaa.ClassSuperUser, "ssh", "commit")
	if !strings.Contains(out, "revision 3") { // rev1 初始 + rev2 引导 admin + 本次
		t.Fatalf("commit 输出应含 revision 3: %s", out)
	}

	// exit 回操作模式
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "exit")
	if res.Mode != "oper" || !strings.HasPrefix(res.Prompt, "nfvis>") {
		t.Fatalf("exit 后应回操作模式: %+v", res)
	}

	// 操作模式 show configuration 渲染 committed
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration")
	if !strings.Contains(res.Output, "cli-node") {
		t.Fatalf("show configuration 应渲染 committed:\n%s", res.Output)
	}
}

func TestCLISetNestedDeleteAndValidation(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set interfaces ens2f0 mtu 9000",
		"set virtual-switches vs-app type l2",
		"set interfaces ens2f0 description to-TOR",
		"set virtual-switches vs-app ports 1 interface ens2f0",
	)

	// 嵌套结构渲染
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-switches vs-app")
	if !strings.Contains(res.Output, "type l2") || !strings.Contains(res.Output, "interface ens2f0") {
		t.Fatalf("嵌套渲染不符:\n%s", res.Output)
	}

	// 删除子语句
	out := run(t, x, "admin", aaa.ClassSuperUser, "ssh", "delete interfaces ens2f0 description")
	if !strings.Contains(out, "已删除") {
		t.Fatalf("删除输出不符: %s", out)
	}
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces ens2f0")
	if strings.Contains(res.Output, "description") {
		t.Fatalf("删除后不应再有 description:\n%s", res.Output)
	}

	// 校验失败：commit 保留 candidate 并逐条列出（非法 mtu）
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "set interfaces ens2f0 mtu 99999")
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "commit")
	if !strings.Contains(res.Output, "校验失败") || !strings.Contains(res.Output, "mtu") {
		t.Fatalf("非法 mtu 应校验失败:\n%s", res.Output)
	}
	// 修正后再提交成功
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "set interfaces ens2f0 mtu 9000")
	out = run(t, x, "admin", aaa.ClassSuperUser, "ssh", "commit")
	if !strings.Contains(out, "commit 成功") {
		t.Fatalf("修正后应提交成功: %s", out)
	}
}

func TestCLIConfirmedRollbackAndCompare(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set system hostname v1",
		"commit",
		"set system hostname v2",
		"commit",
	)

	// confirmed 提交（FR-CFG-003）
	x.Execute("admin", aaa.ClassSuperUser, "ssh", "set system hostname v3")
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "commit confirmed 10")
	if !strings.Contains(res.Output, "confirmed") {
		t.Fatalf("confirmed 提交输出不符: %s", res.Output)
	}

	// compare rollback（FR-CFG-006，操作模式命令）
	x.Execute("admin", aaa.ClassSuperUser, "ssh", "exit")
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration compare rollback 1")
	if !strings.Contains(res.Output, "v2") || !strings.Contains(res.Output, "v3") {
		t.Fatalf("compare 应显示 v2→v3 差异:\n%s", res.Output)
	}

	// rollback 取历史快照为 candidate（FR-CFG-005，需再 commit 生效）
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "configure", "rollback 1")
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show")
	if !strings.Contains(res.Output, "v2") {
		t.Fatalf("rollback 后 candidate 应为 v2:\n%s", res.Output)
	}
	out := run(t, x, "admin", aaa.ClassSuperUser, "ssh", "commit")
	if !strings.Contains(out, "commit 成功") {
		t.Fatalf("rollback 后提交: %s", out)
	}
}

func TestCLIPermissionEnforced(t *testing.T) {
	x, _ := newCLIKit(t)

	// read-only class：不能进入配置模式（§4 矩阵）
	res := x.Execute("viewer", aaa.ClassReadOnly, "ssh", "configure")
	if !strings.Contains(res.Output, "无权限") {
		t.Fatalf("read-only 进入配置模式应被拒: %s", res.Output)
	}
	// super-user 正常进入
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "configure")
	if res.Mode != "config" {
		t.Fatalf("super-user 应可进入配置模式: %+v", res)
	}
}

func TestCLISessionIsolation(t *testing.T) {
	x, _ := newCLIKit(t)
	// 同一用户不同接入源 = 不同会话（决策 #26）
	x.Execute("admin", aaa.ClassSuperUser, "ssh", "configure")
	x.Execute("admin", aaa.ClassSuperUser, "console", "configure")
	x.Execute("admin", aaa.ClassSuperUser, "ssh", "set system hostname from-ssh")
	// console 会话的 candidate 不受 ssh 会话影响（尚未 edit）
	res := x.Execute("admin", aaa.ClassSuperUser, "console", "show")
	if strings.Contains(res.Output, "from-ssh") {
		t.Fatalf("console 会话不应看到 ssh 会话的变更:\n%s", res.Output)
	}
}
