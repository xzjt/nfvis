// 脚本模式（-c）的判定单测：破坏性动作的确认问询在非交互下必须算失败。
//
// 由来：`nfvis-cli -c "request system reboot"` 曾经只把问句打印出来、什么也不做，
// 却以退出码 0 结束——自动化据此认为命令成功（假成功），顺带让冒烟套件对它判 ✓。
package main

import (
	"strings"
	"testing"
)

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
