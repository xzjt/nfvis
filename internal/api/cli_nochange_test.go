package api

// round86 R86-8：「值未变化」的空操作**不是错误**，是提示。
//
// 由来（真机 round86）：`nfvis-cli -c "<多行脚本>"` 里某条语义正确但取值已相同的语句
// 会输出 `%% 语句未产生配置变更…`，而脚本模式「任一行出错即停」→ 后续语句全部不执行、
// 候选被丢弃（套件前置块第一行 `set interfaces ens192 description cli-pre` 已是该值，
// 整块停摆、后面所有检查级联假红），幂等重跑根本不可用。
//
// 现在的口径：空操作给一行**不带 `%` 前缀**的提示，并在结果里带**结构化标记**
// （CLIEResult.Warning，经 /cli/execute 回传）——脚本模式据此继续、退出码不受影响；
// 真错误（语法/校验/权限/下发失败）仍照旧 `%%` + 中止 + 非零退出。
// 判据是标记而不是输出文本：语句自身可能含 `%`（如描述里的 "50%% loss"）。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/model"
)

// TestCLINoChangeIsWarningNotError set 的同值重设：提示而非错误，且脚本可继续。
func TestCLINoChangeIsWarningNotError(t *testing.T) {
	x, engine := newCLIKit(t)
	exec := func(line string) CLIEResult {
		t.Helper()
		return x.Execute("admin", aaa.ClassSuperUser, "ssh", line)
	}

	if res := exec("configure"); res.Output != "" || res.Warning {
		t.Fatalf("configure: %q warning=%v", res.Output, res.Warning)
	}
	// 首次赋值是真变更：照旧 [ok]，不带提示标记。
	first := exec("set system hostname nc-node")
	if !strings.Contains(first.Output, "[ok]") || first.Warning {
		t.Fatalf("首次设值应 [ok] 且非提示：%q warning=%v", first.Output, first.Warning)
	}
	before, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读取 candidate: %v", err)
	}

	// 同值重设：提示（无 `%%`）、带结构化标记、candidate 不变。
	again := exec("set system hostname nc-node")
	if strings.Contains(again.Output, "%%") || strings.HasPrefix(strings.TrimSpace(again.Output), "%") {
		t.Errorf("空操作不得带错误前缀（套件与脚本按行首 %% 判失败）：%q", again.Output)
	}
	if !again.Warning {
		t.Errorf("空操作应带结构化标记 Warning（脚本模式据此继续）：%q", again.Output)
	}
	if !strings.Contains(again.Output, "未产生配置变更") {
		t.Errorf("提示应说明原因（未产生配置变更）：%q", again.Output)
	}
	if !strings.Contains(again.Output, "set system hostname nc-node") {
		t.Errorf("提示应回引语句（含 set/delete），操作者才知道是哪一条：%q", again.Output)
	}
	after, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读取 candidate: %v", err)
	}
	if model.Diff(before, after) != "" {
		t.Errorf("空操作不该改动 candidate：%s", model.Diff(before, after))
	}

	// 后续语句照旧执行（这正是脚本模式要的「继续」）：落进 candidate 且能提交。
	next := exec("set system dns server 8.8.8.8")
	if next.Warning || strings.Contains(next.Output, "%%") {
		t.Fatalf("后续语句应正常执行：%q warning=%v", next.Output, next.Warning)
	}
	after2, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读取 candidate: %v", err)
	}
	if model.Diff(after, after2) == "" {
		t.Fatalf("后续语句未落进 candidate（空操作之后脚本被卡住的表现）")
	}
	if com := exec("commit"); !strings.Contains(com.Output, "commit 成功") {
		t.Fatalf("空操作与后续语句之后 commit 应成功：%q", com.Output)
	}
}

// TestCLINoOpOnlyScriptCommitSucceeds 幂等重跑的最短形态：整段脚本只有空操作 + commit。
// 空操作不报错、commit 按「无变更」处理（不产生新修订），全程无 `%%`（脚本退出码 0）。
func TestCLINoOpOnlyScriptCommitSucceeds(t *testing.T) {
	x, _ := newCLIKit(t)
	const stmt = "set system hostname nc-idem"
	// 第一轮：真变更并提交（模拟「这台机器上一轮已配过」）。
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "configure", stmt, "commit", "exit")
	// 第二轮：原样重跑——`run` 自带「输出含 %% 即 fatal」判定，正是本用例要的断言。
	out := run(t, x, "admin", aaa.ClassSuperUser, "ssh", "configure", stmt, "commit", "exit")
	if !strings.Contains(out, "警告: 语句未产生配置变更") {
		t.Errorf("重跑应给出提示（而不是错误）：%q", out)
	}
	if !strings.Contains(out, "commit 成功") {
		t.Errorf("空操作之后 commit 仍应成功（无变更不产生新修订）：%q", out)
	}
}

// TestCLINoChangeDeleteIsWarning 删除形态走同一处 Diff 兜底：同样是提示而非错误。
// （注意区分：删「不存在的对象」是**真错误**（`无匹配配置`），只有「语句成立但配置
// 没变」才是提示——删除未配置过的 LLDP 属后者。）
func TestCLINoChangeDeleteIsWarning(t *testing.T) {
	x, engine := newCLIKit(t)
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "configure")
	if res.Output != "" {
		t.Fatalf("configure: %q", res.Output)
	}
	del := x.Execute("admin", aaa.ClassSuperUser, "ssh", "delete protocols lldp enable")
	if strings.Contains(del.Output, "%%") {
		t.Fatalf("删除未配置项应是提示而非错误：%q", del.Output)
	}
	if !del.Warning {
		t.Fatalf("删除空操作应带结构化标记 Warning：%q", del.Output)
	}
	if !strings.Contains(del.Output, "delete protocols lldp enable") {
		t.Errorf("提示应回引语句（含 set/delete）：%q", del.Output)
	}
	if _, _, err := engine.Candidate(); err != nil {
		t.Fatalf("提示之后候选会话应仍然可用: %v", err)
	}
}

// TestCLIRealErrorsStayErrors 「不要放宽真错误」：语法/取值/删除无匹配/权限类失败仍 `%%`
// 且**不带**提示标记（脚本模式照旧中止、退出码非 0）。
func TestCLIRealErrorsStayErrors(t *testing.T) {
	x, _ := newCLIKit(t)
	if res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "configure"); res.Output != "" {
		t.Fatalf("configure: %q", res.Output)
	}
	cases := []struct {
		name   string
		source string
		class  string
		line   string
	}{
		{"缺取值", "ssh", aaa.ClassSuperUser, "set system hostname"},
		{"取值类型不符", "ssh", aaa.ClassSuperUser, "set resource-pools hugepages page-size 1G count abc"},
		{"未知语句", "ssh", aaa.ClassSuperUser, "set system no-such-keyword x"},
		{"删除无匹配", "ssh", aaa.ClassSuperUser, "delete system ntp server 203.0.113.9"},
		// 权限拒绝（配置模式本身要求 super-user；用另一接入源 = 另一会话，从操作模式进）
		{"权限拒绝", "console", aaa.ClassReadOnly, "configure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := x.Execute("admin", tc.class, tc.source, tc.line)
			if !strings.Contains(res.Output, "%%") {
				t.Fatalf("真错误应仍报 %%：%q", res.Output)
			}
			if res.Warning {
				t.Errorf("真错误不得带提示标记（否则脚本会继续执行）：%q", res.Output)
			}
		})
	}
}

// TestCLINoChangeWarningOverHTTP 结构化标记必须**过 HTTP**到达 CLI 前端：
// 脚本模式读的是它，而不是自己解析输出文本。
func TestCLINoChangeWarningOverHTTP(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	cliLine(t, ts, token, "ssh", "configure")

	first := cliLine(t, ts, token, "ssh", "set system hostname nc-http")
	if first.Warning {
		t.Errorf("首次设值不该是提示：%q", first.Output)
	}
	again := cliLine(t, ts, token, "ssh", "set system hostname nc-http")
	if !again.Warning {
		t.Errorf("同值重设应回 warning=true（脚本模式据此继续）：%q", again.Output)
	}
	if strings.Contains(again.Output, "%%") {
		t.Errorf("提示行不得带错误前缀：%q", again.Output)
	}
}
