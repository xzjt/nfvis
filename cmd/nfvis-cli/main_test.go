// 脚本模式（-c）的判定单测：破坏性动作的确认问询在非交互下必须算失败。
//
// 由来：`nfvis-cli -c "request system reboot"` 曾经只把问句打印出来、什么也不做，
// 却以退出码 0 结束——自动化据此认为命令成功（假成功），顺带让冒烟套件对它判 ✓。
//
// 另覆盖 round86 R86-8：「值未变化」的空操作**不是失败**（脚本继续、退出码不受影响），
// 判据是服务端回的结构化标记 Warning，而不是输出文本前缀。
package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/cli"
	"github.com/xzjt/nfvis/pkg/cliclient"
)

// ---------- 脚本模式逐行执行（runScriptLines，runScript 的可测核心） ----------

// scriptBackend 脚本模式单测用的假 Backend：按行给出预置结果并记录执行顺序。
type scriptBackend struct {
	replies  map[string]cliclient.Result
	executed []string
}

func (b *scriptBackend) Execute(line, _ string) (cliclient.Result, error) {
	b.executed = append(b.executed, line)
	if res, ok := b.replies[line]; ok {
		return res, nil
	}
	return cliclient.Result{Output: "ok\n", Mode: "config", Prompt: "nfvis# "}, nil
}

func (b *scriptBackend) DynamicCandidates(string) ([]string, error) { return nil, nil }
func (b *scriptBackend) Logout() error                              { return nil }
func (b *scriptBackend) MetricsText() (string, error)               { return "", nil }

func (b *scriptBackend) DialConsole(string, string) (io.ReadWriteCloser, error) {
	return nil, errors.New("stub 不支持 console")
}

// noChangeResult 服务端「值未变化」提示的伪响应（文案与 internal/api 同源）。
func noChangeResult(stmt string) cliclient.Result {
	return cliclient.Result{
		Output:  "警告: 语句未产生配置变更（值未变化或尚未映射到模型），已继续：" + stmt + "\n",
		Mode:    "config",
		Prompt:  "nfvis# ",
		Warning: true,
	}
}

// TestRunScriptContinuesAfterNoChangeWarning ① 空操作不中止脚本：后续语句照旧执行、整体不算失败。
func TestRunScriptContinuesAfterNoChangeWarning(t *testing.T) {
	const noop = "set interfaces ens192 description cli-pre"
	be := &scriptBackend{replies: map[string]cliclient.Result{noop: noChangeResult(noop)}}
	sess := cli.New(be, "ssh")

	failed := runScriptLines(sess, strings.Join([]string{
		"configure", noop, "set system dns server 8.8.8.8", "commit",
	}, "\n"))
	if failed {
		t.Fatal("空操作（值未变化）不该判失败——脚本应继续，退出码 0")
	}
	want := []string{"configure", noop, "set system dns server 8.8.8.8", "commit"}
	if len(be.executed) != len(want) {
		t.Fatalf("空操作之后的语句必须照旧执行，实执行 %v", be.executed)
	}
	for i, w := range want {
		if be.executed[i] != w {
			t.Fatalf("第 %d 行应为 %q，实得 %q（全部：%v）", i, w, be.executed[i], be.executed)
		}
	}
}

// TestRunScriptStopsOnRealError ② 真错误仍中止且退出码非 0（「不要放宽真错误」）。
func TestRunScriptStopsOnRealError(t *testing.T) {
	const bad = "set system hostname"
	be := &scriptBackend{replies: map[string]cliclient.Result{
		bad: {Output: "%% 配置不完整，缺少取值: hostname\n", Mode: "config", Prompt: "nfvis# "},
	}}
	sess := cli.New(be, "ssh")

	failed := runScriptLines(sess, strings.Join([]string{
		"configure", bad, "set system dns server 8.8.8.8",
	}, "\n"))
	if !failed {
		t.Fatal("真错误应判失败（退出码非 0）")
	}
	if len(be.executed) != 2 || be.executed[1] != bad {
		t.Fatalf("失败行之后的语句不得执行，实执行 %v", be.executed)
	}
}

// singlePercentResult 服务端**单 %** 错误的伪响应（来源：`%%` 写在 fmt 格式串里被折叠成 `%`，
// 真机 `show system <未知子命令>` 即此形）。
func singlePercentResult(line string) cliclient.Result {
	return cliclient.Result{
		Output: "% 无效命令: " + line + "（可用：uptime|cpu|memory|storage）\n",
		Mode:   "oper", Prompt: "nfvis> ",
	}
}

// runScriptCapture 跑脚本并捕获输出（runScriptLinesTo 的可测入口）。
func runScriptCapture(sess *cli.Session, script string) (bool, string) {
	var buf bytes.Buffer
	failed := runScriptLinesTo(sess, script, &buf)
	return failed, buf.String()
}

// TestRunScriptStopsAtUnknownCommandMiddleLine 决策 #320 / R100-1 真机场景：脚本中间一条未知命令，
// 必须**停在它**、报出行号与原文、其后语句**未执行**（用后续语句的特征串反证）。
//
// 回归的是「服务端单 % 错误不被判失败」：`%%` 写在 fmt 格式串里会渲染成单 %，脚本曾据此继续执行。
func TestRunScriptStopsAtUnknownCommandMiddleLine(t *testing.T) {
	const (
		bad   = "show system no-such-subcommand-xyz"
		later = "show vpp also-no-such-subcommand-abc"
	)
	be := &scriptBackend{replies: map[string]cliclient.Result{bad: singlePercentResult(bad)}}
	sess := cli.New(be, "ssh")

	failed, out := runScriptCapture(sess, strings.Join([]string{"show version", bad, later}, "\n"))
	if !failed {
		t.Fatal("未知命令（单 % 错误）应判失败并停止脚本")
	}
	if len(be.executed) != 2 || be.executed[1] != bad {
		t.Fatalf("应停在失败行、其后不执行，实执行 %v", be.executed)
	}
	for _, want := range []string{"第 2 行", "show system no-such-subcommand-xyz", "其余 1 行未执行"} {
		if !strings.Contains(out, want) {
			t.Errorf("停止报告应含 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "also-no-such-subcommand-abc") {
		t.Errorf("失败行之后的语句不得执行（特征串出现在输出里）：\n%s", out)
	}
}

// TestRunScriptStopsAtUnknownCommandFirstLine 未知命令在**首行**：报第 1 行、其余 2 行未执行。
func TestRunScriptStopsAtUnknownCommandFirstLine(t *testing.T) {
	const (
		bad  = "show system no-such-subcommand-xyz"
		next = "show vpp also-no-such-subcommand-abc"
	)
	be := &scriptBackend{replies: map[string]cliclient.Result{bad: singlePercentResult(bad)}}
	sess := cli.New(be, "ssh")

	failed, out := runScriptCapture(sess, strings.Join([]string{bad, next, "show version"}, "\n"))
	if !failed {
		t.Fatal("首行未知命令应判失败并停止脚本")
	}
	if len(be.executed) != 1 || be.executed[0] != bad {
		t.Fatalf("应停在首行、其后不执行，实执行 %v", be.executed)
	}
	for _, want := range []string{"第 1 行", "其余 2 行未执行"} {
		if !strings.Contains(out, want) {
			t.Errorf("停止报告应含 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "also-no-such-subcommand-abc") {
		t.Errorf("失败行之后的语句不得执行：\n%s", out)
	}
}

// TestRunScriptCleanAllExecuted 全绿脚本：逐行执行完、无失败、不打印停止报告。
func TestRunScriptCleanAllExecuted(t *testing.T) {
	be := &scriptBackend{}
	sess := cli.New(be, "ssh")

	failed, out := runScriptCapture(sess, "show version\nshow system api tokens\n")
	if failed {
		t.Fatalf("全绿脚本不该判失败：%s", out)
	}
	if len(be.executed) != 2 {
		t.Fatalf("全绿脚本应逐行执行完，实执行 %v", be.executed)
	}
	if strings.Contains(out, "未执行") {
		t.Errorf("全绿脚本不该打印停止报告：\n%s", out)
	}
}

// TestRunScriptFailureReportsLineNumberAndReason 停止报告须含行号、原文、原因与剩余行数。
func TestRunScriptFailureReportsLineNumberAndReason(t *testing.T) {
	const bad = "request system reboot"
	be := &scriptBackend{replies: map[string]cliclient.Result{
		bad: {Output: "Restart the system? [yes,no] \n", Mode: "oper", Prompt: "nfvis> "},
	}}
	sess := cli.New(be, "ssh")

	failed, out := runScriptCapture(sess, "show version\n"+bad+"\nshow version\n")
	if !failed {
		t.Fatal("非交互确认问询应判失败")
	}
	if len(be.executed) != 2 {
		t.Fatalf("问询之后的语句不得执行，实执行 %v", be.executed)
	}
	for _, want := range []string{"第 2 行", "request system reboot", "原因：", "其余 1 行未执行"} {
		if !strings.Contains(out, want) {
			t.Errorf("停止报告应含 %q：\n%s", want, out)
		}
	}
}

// TestRunScriptReportsPhysicalStartLineForMultiline 多行引号值语句：行号取该语句的**起始物理行**，
// 原文折成单行（不把值里的换行倒出来），剩余行数按逻辑行计。
func TestRunScriptReportsPhysicalStartLineForMultiline(t *testing.T) {
	const stmt = "set virtual-machine-functions fw cloud-init user-data \"#!/bin/sh\nbad\""
	be := &scriptBackend{replies: map[string]cliclient.Result{
		stmt: {Output: "% 无效命令: bad\n", Mode: "oper", Prompt: "nfvis> "},
	}}
	sess := cli.New(be, "ssh")

	failed, out := runScriptCapture(sess, "show version\n"+stmt+"\nshow version\n")
	if !failed {
		t.Fatal("多行值语句失败也应即停")
	}
	if len(be.executed) != 2 {
		t.Fatalf("失败行之后的语句不得执行，实执行 %v", be.executed)
	}
	for _, want := range []string{"第 2 行", "#!/bin/sh bad", "其余 1 行未执行"} {
		if !strings.Contains(out, want) {
			t.Errorf("停止报告应含 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "\nbad") {
		t.Errorf("多行值应折成单行，不应原样倒出换行：\n%s", out)
	}
}

// TestScriptSourcesSameFailFast 决策 #320：`-c` / `-f <file>` / `-f -`（stdin）三条来源
// **收敛到同一执行路径**（resolveScript → runScriptLinesTo），失败即停的语义与报告逐字一致。
func TestScriptSourcesSameFailFast(t *testing.T) {
	const (
		bad   = "show system no-such-subcommand-xyz"
		later = "show vpp also-no-such-subcommand-abc"
	)
	body := "show version\n" + bad + "\n" + later + "\n"

	filePath := filepath.Join(t.TempDir(), "stop.txt")
	if err := os.WriteFile(filePath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sources := map[string]func() (string, error){
		"-c":   func() (string, error) { return resolveScript(body, true, "", false, nil) },
		"-f":   func() (string, error) { return resolveScript("", false, filePath, true, nil) },
		"-f -": func() (string, error) { return resolveScript("", false, "-", true, strings.NewReader(body)) },
	}
	var wantOut string
	for name, load := range sources {
		t.Run(name, func(t *testing.T) {
			script, err := load()
			if err != nil {
				t.Fatalf("%s 解析失败: %v", name, err)
			}
			be := &scriptBackend{replies: map[string]cliclient.Result{bad: singlePercentResult(bad)}}
			sess := cli.New(be, "ssh")
			failed, out := runScriptCapture(sess, script)
			if !failed {
				t.Fatal("三条来源都应在失败行即停")
			}
			if len(be.executed) != 2 || be.executed[1] != bad {
				t.Fatalf("三条来源都应停在失败行，实执行 %v", be.executed)
			}
			if !strings.Contains(out, "第 2 行") || !strings.Contains(out, "其余 1 行未执行") {
				t.Fatalf("%s 的停止报告不完整：\n%s", name, out)
			}
			if strings.Contains(out, "also-no-such-subcommand-abc") {
				t.Fatalf("%s 失败行之后的语句被执行：\n%s", name, out)
			}
			if wantOut == "" {
				wantOut = out
			} else if out != wantOut {
				t.Errorf("%s 与其它来源的报告不一致：\n got %q\nwant %q", name, out, wantOut)
			}
		})
	}
}

// TestRunScriptWarningIsMarkedNotParsed 提示行的**语句文本自身含 `%%`** 时也不得误判为失败
// （判据是结构化标记，不是文本前缀——这正是不能靠字符串解析的原因）。
func TestRunScriptWarningIsMarkedNotParsed(t *testing.T) {
	const stmt = `set interfaces ens192 description "50%% loss"`
	be := &scriptBackend{replies: map[string]cliclient.Result{stmt: noChangeResult(stmt)}}
	sess := cli.New(be, "ssh")

	if failed := runScriptLines(sess, stmt+"\nshow version"); failed {
		t.Fatal("提示（Warning）不该因语句文本含 % 而判失败")
	}
	if len(be.executed) != 2 {
		t.Fatalf("提示之后的语句应照旧执行，实执行 %v", be.executed)
	}
}

// TestRunScriptMultilineQuotedValue 决策 #313：引号跨行的取值按**整段**执行——
// 多行 user-data 内联时，后续行必须是同一条语句的一部分（值内保留换行），
// 而不是被当成新命令逐条执行（旧行为：静默只取首行）。
func TestRunScriptMultilineQuotedValue(t *testing.T) {
	be := &scriptBackend{}
	sess := cli.New(be, "ssh")

	script := "configure\n" +
		"set virtual-machine-functions fw-vm cloud-init user-data \"#!/bin/sh\n" +
		"echo hi\"\n" +
		"commit"
	if failed := runScriptLines(sess, script); failed {
		t.Fatalf("多行引号值脚本不该失败，实执行 %v", be.executed)
	}
	if len(be.executed) != 3 {
		t.Fatalf("应执行 3 条语句（configure / set 多行值 / commit），实得 %d 条：%#v", len(be.executed), be.executed)
	}
	if !strings.Contains(be.executed[1], "#!/bin/sh\necho hi") {
		t.Fatalf("多行值应保留换行并作为一条语句执行，实得 %q", be.executed[1])
	}
}

// TestRunScriptStillRefusesConfirmPrompt 破坏性动作的非交互问询仍按失败处理（不得因本轮改动放宽）。
func TestRunScriptStillRefusesConfirmPrompt(t *testing.T) {
	const del = "request virtual-machine-functions fw-vm delete"
	be := &scriptBackend{replies: map[string]cliclient.Result{
		del: {Output: "Delete VNF 'fw-vm'? [yes,no] \n", Mode: "oper", Prompt: "nfvis> "},
	}}
	sess := cli.New(be, "ssh")

	if failed := runScriptLines(sess, del+"\nshow version"); !failed {
		t.Fatal("非交互确认问询应判失败（假成功回归）")
	}
	if len(be.executed) != 1 {
		t.Fatalf("问询之后的语句不得执行，实执行 %v", be.executed)
	}
}

// ---------- 决策 #309：脚本来源收敛（resolveScript）与 -f 文件解析 ----------

// TestResolveScriptMutualExclusion `-c` 与 `-f` 同时给出必须报错，且说清二者选一。
func TestResolveScriptMutualExclusion(t *testing.T) {
	_, err := resolveScript("show version", true, "cmds.txt", true, strings.NewReader(""))
	if err == nil {
		t.Fatal("-c 与 -f 同时给出必须报错")
	}
	for _, want := range []string{"-c", "-f", "选其一"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("互斥报错应含 %q：%v", want, err)
		}
	}
}

// TestResolveScriptInteractive 两者皆空＝交互模式：返回空文本、不报错。
func TestResolveScriptInteractive(t *testing.T) {
	s, err := resolveScript("", false, "", false, strings.NewReader("show version\n"))
	if err != nil || s != "" {
		t.Fatalf("两者皆空应回交互模式（空文本无错）：%q %v", s, err)
	}
}

// TestResolveScriptFlagC `-c` 文本原样（仅归一换行）。
func TestResolveScriptFlagC(t *testing.T) {
	s, err := resolveScript("show version\nshow interfaces", true, "", false, nil)
	if err != nil {
		t.Fatalf("-c 解析不应报错: %v", err)
	}
	if s != "show version\nshow interfaces" {
		t.Fatalf("-c 文本应原样: %q", s)
	}
}

// TestResolveScriptFromFileNormalizesCRLFAndBOM Windows 编写的文件（BOM + CRLF）须归一为 LF。
func TestResolveScriptFromFileNormalizesCRLFAndBOM(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cmds.txt")
	if err := os.WriteFile(p, []byte("\ufeffconfigure\r\nset system hostname fw-01\r\ncommit\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := resolveScript("", false, p, true, nil)
	if err != nil {
		t.Fatalf("读取脚本文件失败: %v", err)
	}
	want := "configure\nset system hostname fw-01\ncommit\n"
	if s != want {
		t.Fatalf("CRLF/BOM 应归一为 LF：\n got %q\nwant %q", s, want)
	}
	if strings.ContainsAny(s, "\r\ufeff") {
		t.Fatalf("归一后不得残留 CR/BOM：%q", s)
	}
}

// TestResolveScriptMissingFile 缺文件：报错须含路径、不静默。
func TestResolveScriptMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nope.txt")
	_, err := resolveScript("", false, p, true, nil)
	if err == nil {
		t.Fatal("缺文件必须报错")
	}
	if !strings.Contains(err.Error(), p) {
		t.Fatalf("报错须含路径 %q：%v", p, err)
	}
}

// TestResolveScriptEmptyFile 空/纯空白文件必须报错（不静默成功）。
func TestResolveScriptEmptyFile(t *testing.T) {
	for name, body := range map[string]string{"empty": "", "blank": "  \n\t\n"} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "s.txt")
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := resolveScript("", false, p, true, nil); err == nil {
				t.Fatal("空/纯空白脚本必须报错（不静默成功）")
			}
		})
	}
}

// TestResolveScriptStdin `-f -` 从 stdin 读脚本。
func TestResolveScriptStdin(t *testing.T) {
	s, err := resolveScript("", false, "-", true, strings.NewReader("show version\nshow vpp\n"))
	if err != nil {
		t.Fatalf("-f - 应读 stdin: %v", err)
	}
	if s != "show version\nshow vpp\n" {
		t.Fatalf("stdin 脚本文本不符：%q", s)
	}
}

// TestRunScriptCRLFFileExecutesClean CRLF 文件经 resolveScript → runScriptLines 后
// 逐行执行且**执行到的命令行不残留 `\r`**（与 -c 复用同一执行路径）。
func TestRunScriptCRLFFileExecutesClean(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cmds.txt")
	if err := os.WriteFile(p, []byte("configure\r\nset system dns server 8.8.8.8\r\ncommit\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script, err := resolveScript("", false, p, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	be := &scriptBackend{}
	sess := cli.New(be, "ssh")
	if failed := runScriptLines(sess, script); failed {
		t.Fatal("正常脚本不应判失败")
	}
	want := []string{"configure", "set system dns server 8.8.8.8", "commit"}
	if len(be.executed) != len(want) {
		t.Fatalf("应执行 %v，实得 %v", want, be.executed)
	}
	for i, w := range want {
		if be.executed[i] != w {
			t.Fatalf("第 %d 行应为 %q（不得残留 \\r），实得 %q", i, w, be.executed[i])
		}
	}
}

// ---------- 登录横幅（决策 #303） ----------

// bannerStub 登录横幅取数器的桩：记录**是否真的取了**——脚本模式的判据是"不取"，
// 比"不打印"更严（连端点都不该碰）。
type bannerStub struct {
	banner string
	calls  int
}

func (s *bannerStub) LoginBanner() (string, error) {
	s.calls++
	return s.banner, nil
}

// TestLoginBannerInteractiveOnly 登录横幅只在交互模式取与打印；脚本模式（-c）一个字都不输出。
//
// 脚本模式的成败判定看输出行首的 %/%%（见 runScriptLines），横幅文本会污染它与判定——
// 故脚本模式下**不取也不打印**（不与服务端建立这条无谓的请求）。
func TestLoginBannerInteractiveOnly(t *testing.T) {
	f := &bannerStub{banner: "仅限授权人员访问"}
	var out bytes.Buffer

	// 脚本模式：不取、不打印。
	if printLoginBanner(&out, "show version", f) {
		t.Fatal("脚本模式不应打印横幅")
	}
	if f.calls != 0 {
		t.Fatalf("脚本模式不应取横幅（会污染 %%/%% 判定），实际取了 %d 次", f.calls)
	}
	if out.Len() != 0 {
		t.Fatalf("脚本模式不应有任何输出：%q", out.String())
	}

	// 交互模式（-c 为空）：取一次、打印横幅（随后才是口令提示）。
	if !printLoginBanner(&out, "", f) {
		t.Fatal("交互模式有横幅应打印")
	}
	if f.calls != 1 {
		t.Fatalf("交互模式应恰好取一次横幅，实际 %d 次", f.calls)
	}
	if !strings.Contains(out.String(), "仅限授权人员访问") {
		t.Fatalf("输出应含横幅文本：%q", out.String())
	}
}

func TestConfirmRefusal(t *testing.T) {
	cases := []struct {
		name string
		out  string // 服务端回显（问询用例取自 internal/api 的实际文案）
		need bool
	}{
		// —— 需确认的破坏性动作：命中即拒绝 ——
		{"重启问询", "重启 nfvis 将中断全部业务。Restart the system? [yes,no] ", true},
		{"关机问询", "关机 nfvis 将中断全部业务。Shut down the system? [yes,no] ", true},
		{"删 VM 问询", "Delete VNF 'fw-vm'? [yes,no] ", true},
		{"删容器问询", "Delete container 'ct-1'? [yes,no] ", true},
		{"删镜像问询", "Delete image 'base.qcow2'? [yes,no] ", true},
		{"接口交 DPDK 问询", "将接口 ens224 unbind-dpdk（会中断该网卡流量） ''? [yes,no] ", true},
		{"安装软件包问询", "安装软件包 /tmp/nfvis_1.1.0_amd64.deb 将替换 nfvis 并重启 nfvisd。Continue? [yes,no] ", true},
		{"软件回退问询", "回退到上一版本将替换 nfvis 并重启 nfvisd。Continue? [yes,no] ", true},
		{"恢复出厂首次问询", "恢复出厂将清空全部配置、镜像与 VNF，并重置本地账号。 Continue? [yes,no] ", true},
		{"恢复出厂二次问询", "再次确认：此操作不可撤销。恢复出厂将清空全部配置、镜像与 VNF，并重置本地账号。 Proceed? [yes,no] ", true},
		{"问询无尾随空格", "Delete image 'base.qcow2'? [yes,no]", true},
		{"问询带换行", "Delete image 'base.qcow2'? [yes,no]\n", true},

		// —— 正常输出：不得误判 ——
		{"空输出", "", false},
		{"show 输出", "NFViS 1.1.39\n", false},
		{"删除成功回显", "镜像 cli-del.qcow2 已删除\n", false},
		{"服务端错误", "%% 镜像被引用，不可删除（1 个引用）\n", false},
		{"语法错误", "%% 语法: request images delete name <n>\n", false},
		{"配置模式回显", "commit 完成（revision 12）\n", false},
		// 回显历史（如 `show log audit` 里的旧记录）不是「正在问询」：判据只看结尾
		{"历史回显含问询文本", "audit: system.reboot 需确认 Restart the system? [yes,no] 已拒绝\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, need := confirmRefusal(tc.out)
			if need != tc.need {
				t.Fatalf("confirmRefusal(%q) = (%q, %v)，期望 need=%v", tc.out, msg, need, tc.need)
			}
			if !need {
				if msg != "" {
					t.Fatalf("非问询输出不该给文案：%q", msg)
				}
				return
			}
			// 命中：必须走脚本模式的错误口径（行首 %%），并说明「为什么没执行」与「怎么才能执行」
			if !strings.HasPrefix(msg, "%%") {
				t.Errorf("文案须以 %% 开头（脚本模式按错误处理）：%q", msg)
			}
			if !strings.HasSuffix(msg, "\n") {
				t.Errorf("文案须自带换行（runScript 直接打印）：%q", msg)
			}
			for _, want := range []string{"交互确认", "破坏性", "非交互", "--yes"} {
				if !strings.Contains(msg, want) {
					t.Errorf("文案应含 %q：%q", want, msg)
				}
			}
		})
	}
}

// 决策 #358 顺带收口：**脚本模式（-c/-f）无法接管终端**——结果里带 Console（串口 console /
// 容器 shell）时不得静默丢弃（否则脚本看到「正在打开 …」却什么都没开，与「只问不做」同一类假成功），
// 必须按失败处理并指引交互式 CLI。此前对 `request virtual-machine-functions … console` 也存在。
func TestRunScriptRejectsConsoleTakeover(t *testing.T) {
	const (
		console = "request virtual-machine-functions vm-a console"
		shell   = "request container-functions ct-a shell"
		later   = "show version"
	)
	be := &scriptBackend{replies: map[string]cliclient.Result{
		console: {Output: "正在打开 vm-a 的串口（Ctrl-] 退出，60 秒内有效）…\n",
			Console: &cliclient.ConsoleRequest{VM: "vm-a", WSURL: "/ws?ticket=x"}},
		shell: {Output: "正在打开 ct-a 的容器终端（Ctrl-] 退出，60 秒内有效）…\n",
			Console: &cliclient.ConsoleRequest{VM: "ct-a", WSURL: "/ws?ticket=y", Kind: "container"}},
	}}
	sess := cli.New(be, "ssh")

	for _, tc := range []struct{ cmd, name string }{{console, "vm-a"}, {shell, "ct-a"}} {
		be.executed = nil
		failed, out := runScriptCapture(sess, tc.cmd+"\n"+later)
		if !failed {
			t.Fatalf("%s：脚本模式带终端接管请求应判失败（不得静默丢弃）", tc.cmd)
		}
		if !strings.Contains(out, "非 TTY") || !strings.Contains(out, tc.name) {
			t.Fatalf("%s：应说明「非 TTY 不支持接管」并点名对象，实得：%s", tc.cmd, out)
		}
		if len(be.executed) != 1 {
			t.Fatalf("%s：应停在该行、其后语句不执行，实执行 %v", tc.cmd, be.executed)
		}
	}
}

// 决策 #370（R142 E9）：空脚本不再「假成功」——`-c ”`/`-f ”`（标志给了但值为空）报错；
// 两个标志都没给才是交互模式。
func TestResolveScriptEmptyValues(t *testing.T) {
	// ① 空值必须报错（此前静默进交互模式）
	if _, err := resolveScript("", true, "", false, nil); err == nil {
		t.Fatal("-c '' 应报错（空脚本假成功）")
	} else if !strings.Contains(err.Error(), "-c 的脚本为空") {
		t.Fatalf("文案应指明 -c 为空: %v", err)
	}
	if _, err := resolveScript("", false, "", true, nil); err == nil {
		t.Fatal("-f '' 应报错（空脚本假成功）")
	} else if !strings.Contains(err.Error(), "-f 的脚本为空") {
		t.Fatalf("文案应指明 -f 为空: %v", err)
	}
	// ② 纯空白同样算空
	if _, err := resolveScript(strings.Repeat(" ", 3), true, "", false, nil); err == nil {
		t.Fatal("-c 纯空白应报错")
	}
	// ③ 互斥：两个标志都给（哪怕一个为空）也报互斥
	if _, err := resolveScript("show version", true, "", true, nil); err == nil {
		t.Fatal("-c 与 -f 同时给出应报互斥")
	} else if !strings.Contains(err.Error(), "只能选其一") {
		t.Fatalf("互斥文案: %v", err)
	}
	// ④ 两个标志都没给 ⇒ 交互模式（空串、无错）
	if s, err := resolveScript("", false, "", false, nil); err != nil || s != "" {
		t.Fatalf("未给脚本标志应进交互模式（空串无错），得 %q %v", s, err)
	}
	// ⑤ 空文件仍报错（#309 既有口径不回归）
	empty := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(empty, []byte(strings.Repeat(" ", 2)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveScript("", false, empty, true, nil); err == nil {
		t.Fatal("空文件应报错")
	}
}
