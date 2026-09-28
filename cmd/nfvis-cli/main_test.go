// 脚本模式（-c）的判定单测：破坏性动作的确认问询在非交互下必须算失败。
//
// 由来：`nfvis-cli -c "request system reboot"` 曾经只把问句打印出来、什么也不做，
// 却以退出码 0 结束——自动化据此认为命令成功（假成功），顺带让冒烟套件对它判 ✓。
//
// 另覆盖 round86 R86-8：「值未变化」的空操作**不是失败**（脚本继续、退出码不受影响），
// 判据是服务端回的结构化标记 Warning，而不是输出文本前缀。
package main

import (
	"errors"
	"io"
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

func (b *scriptBackend) DialConsole(string) (io.ReadWriteCloser, error) {
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
