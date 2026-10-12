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

// TestCLINoChangeUserDataHint 决策 #474（round24 R24-O5）：`set … cloud-init user-data <路径>`
// 的模型值＝**路径串**——同一条 set 重跑时路径没变、照发「值未变化」提示，但 VM 下次启动
// 会按同一路径的**文件新内容**重建 seed 并生效（决策 #114）：提示语字面与「有效输入将变化」
// 不同步，操作者 stop+start 之前无从得知。故在既有提示行**后另起一行**追加「文件路径」口径；
// 判定与结构化标记（#190）不动，只多一行文本。
func TestCLINoChangeUserDataHint(t *testing.T) {
	const hint = "提示: user-data 为文件路径——文件内容若已变化，将在 VM 下次启动时重新生成 seed 并生效（运行中需 stop+start）"
	const stmt = "set virtual-machine-functions vnf-a cloud-init user-data /root/ud.yaml"
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "configure")

	// 首次设值是真变更：照旧 [ok]、无提示标记、无追加行。
	first := x.Execute("admin", aaa.ClassSuperUser, "ssh", stmt)
	if first.Warning || !strings.Contains(first.Output, "[ok]") || strings.Contains(first.Output, "提示: user-data") {
		t.Fatalf("首次设值应 [ok] 且无 user-data 提示行：%q warning=%v", first.Output, first.Warning)
	}

	// 同路径重跑：既有提示行 + 逐字追加行（两行，第二行固定文案）。
	again := x.Execute("admin", aaa.ClassSuperUser, "ssh", stmt)
	if !again.Warning || strings.Contains(again.Output, "%%") {
		t.Fatalf("同路径重跑应为提示而非错误：%q warning=%v", again.Output, again.Warning)
	}
	want := "警告: 语句未产生配置变更（值未变化或尚未映射到模型），已继续：" + stmt + "\n" + hint + "\n"
	if again.Output != want {
		t.Fatalf("输出应为既有提示行 + 逐字追加行:\nwant %q\n got %q", want, again.Output)
	}

	// edit 下的相对形态：s.Path 前缀参与 full 的邻接扫描，同样命中（与绝对形态逐字一致）。
	if res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "edit virtual-machine-functions vnf-a"); res.Output != "" {
		t.Fatalf("edit: %q", res.Output)
	}
	rel := x.Execute("admin", aaa.ClassSuperUser, "ssh", "set cloud-init user-data /root/ud.yaml")
	if !rel.Warning || strings.Contains(rel.Output, "%%") {
		t.Fatalf("相对形态同路径重跑应为提示：%q warning=%v", rel.Output, rel.Warning)
	}
	if rel.Output != want {
		t.Fatalf("相对形态输出应与绝对形态逐字一致:\nwant %q\n got %q", want, rel.Output)
	}
}

// TestCLINoChangeUserDataHintScope 决策 #474 的边界：只在 set 的「值未变化」分支、且 full 里
// `cloud-init` **紧邻** `user-data` 时追加——其它 set 语句与所有 delete 语句输出**逐字不变**。
func TestCLINoChangeUserDataHintScope(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "configure")

	// 负控 ①：其它 set 语句（值未变化）——输出与既有单行提示逐字一致。
	const mem = "set virtual-machine-functions vnf-a memory size-mb 1024"
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", mem)
	memAgain := x.Execute("admin", aaa.ClassSuperUser, "ssh", mem)
	wantMem := "警告: 语句未产生配置变更（值未变化或尚未映射到模型），已继续：" + mem + "\n"
	if !memAgain.Warning || memAgain.Output != wantMem {
		t.Fatalf("其它 set 语句输出应逐字不变:\nwant %q\n got %q", wantMem, memAgain.Output)
	}

	// 负控 ②：同在 cloud-init 段、但不是 user-data（hostname 同值重设）——不得追加。
	const host = "set virtual-machine-functions vnf-a cloud-init hostname vnf-a"
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", host)
	hostAgain := x.Execute("admin", aaa.ClassSuperUser, "ssh", host)
	if !hostAgain.Warning || strings.Contains(hostAgain.Output, "提示: user-data") {
		t.Fatalf("cloud-init 其它子键不得追加 user-data 提示行：%q warning=%v", hostAgain.Output, hostAgain.Warning)
	}

	// 负控 ③：delete … cloud-init user-data …——删除已配置的是实变更（“已删除”），
	// 再删已是真错误（无匹配配置）；两者都不得出现追加行（判定只挂在 set 的空操作分支）。
	const stmt = "set virtual-machine-functions vnf-a cloud-init user-data /root/ud.yaml"
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", stmt)
	del := x.Execute("admin", aaa.ClassSuperUser, "ssh", "delete virtual-machine-functions vnf-a cloud-init user-data")
	if !strings.Contains(del.Output, "已删除") || strings.Contains(del.Output, "提示: user-data") {
		t.Fatalf("删除 user-data 应报已删除且无追加行：%q", del.Output)
	}
	delAgain := x.Execute("admin", aaa.ClassSuperUser, "ssh", "delete virtual-machine-functions vnf-a cloud-init user-data")
	if !strings.Contains(delAgain.Output, "%%") || strings.Contains(delAgain.Output, "提示: user-data") {
		t.Fatalf("再次删除（无匹配）应真错误且无追加行：%q", delAgain.Output)
	}
}

// TestIsCloudInitUserDataStatement 助手判据的边界（决策 #474）：只认 set 且 `cloud-init`
// 与 `user-data` 两个 token **紧邻**；delete、非紧邻、含空格的单个取值 token 都不命中。
func TestIsCloudInitUserDataStatement(t *testing.T) {
	cases := []struct {
		name string
		op   string
		full []string
		want bool
	}{
		{"绝对/相对形态（full 含层级前缀）", "set",
			[]string{"virtual-machine-functions", "vnf-a", "cloud-init", "user-data", "/root/ud.yaml"}, true},
		{"delete 不命中（判定只挂 set 空操作分支）", "delete",
			[]string{"virtual-machine-functions", "vnf-a", "cloud-init", "user-data"}, false},
		{"非紧邻（hostname 夹在中间）", "set",
			[]string{"virtual-machine-functions", "vnf-a", "cloud-init", "hostname", "user-data"}, false},
		{"取值 token 自带空格不等于两个 token", "set",
			[]string{"virtual-machine-functions", "vnf-a", "description", "cloud-init user-data"}, false},
		{"空语句", "set", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCloudInitUserDataStatement(tc.op, tc.full); got != tc.want {
				t.Fatalf("isCloudInitUserDataStatement(%q, %q) = %v, want %v", tc.op, tc.full, got, tc.want)
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
