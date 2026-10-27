package model

// round50（审计脱敏覆盖 `sshpass -p <值>` 分隔形态）：文本级脱敏（ScrubSecrets）的判据。
//
// 由来：短选项正则只匹配**贴值**形态 `-p<值>`（`mysql -p123456`），注释里明确把
// 「`-p` 与值之间有空格」排除以避开 `mkdir -p /a`。但 `sshpass -p <口令>` 是极常见形态，
// 命令原文会以明文进审计，并经 CLI `show log audit`、`GET /audit-logs`（read-only 可读）、
// 诊断包 `audit.json` 三出口泄露——而契约（openapi）承诺「口令一律不入审计」。
//
// 修法是**工具感知**：只识别 `sshpass` 之后 `-p` 再接空白与值的形态，不泛化为所有 `-p`。
// 两个方向都要钉：分隔形态的口令必须被掩码，且普通命令（`mkdir -p`、`ls -p`、
// `docker run -p`）逐字不变；掩码同样幂等（写侧与读侧可能各过一遍）。

import (
	"strings"
	"testing"
)

// TestScrubSecretsMasksSSHPassSeparatedValue `sshpass -p <口令>`（空白分隔、可带引号、
// 可夹在其它 sshpass 选项之间）必须掩码。
func TestScrubSecretsMasksSSHPassSeparatedValue(t *testing.T) {
	cases := []struct {
		why  string
		in   string
		want []string // 必须**不**出现的明文口令
	}{
		{"基本分隔形态", "exec 容器 ct-a: sshpass -p hunter2 ssh root@10.0.0.9", []string{"hunter2"}},
		{"值带单引号", "exec 容器 ct-a: sshpass -p 'P@ss w0rd' ssh root@10.0.0.9", []string{"P@ss", "w0rd"}},
		{"夹在其它选项之间", "exec 容器 ct-a: sshpass -o StrictHostKeyChecking=no -p seCr3t ssh root@h", []string{"seCr3t"}},
		{"行首即 sshpass", "sshpass -p topsecret scp f root@h:/tmp", []string{"topsecret"}},
	}
	for _, c := range cases {
		t.Run(c.why, func(t *testing.T) {
			got := ScrubSecrets(c.in)
			for _, leak := range c.want {
				if strings.Contains(got, leak) {
					t.Fatalf("sshpass 分隔形态的口令 %q 未被掩码: %s", leak, got)
				}
			}
			if !strings.Contains(got, RedactedPlaceholder) {
				t.Fatalf("应留下脱敏占位（脱敏≠掏空）: %s", got)
			}
			// 工具与其余命令部分必须保留，审计文本仍有价值
			if !strings.Contains(got, "sshpass") {
				t.Fatalf("sshpass 工具名应保留: %s", got)
			}
		})
	}
}

// TestScrubSecretsKeepsMkdirP 不泛化：`mkdir -p <目录>` 逐字不变（工具感知只认 sshpass）。
func TestScrubSecretsKeepsMkdirP(t *testing.T) {
	for _, cmd := range []string{
		"exec 容器 web: mkdir -p /data/keep/alive",
		"exec 容器 web: mkdir -p /a/b/c && touch /a/b/c/x",
		"exec 容器 web: cp -p /etc/hosts /tmp/hosts",
	} {
		if got := ScrubSecrets(cmd); got != cmd {
			t.Errorf("mkdir/cp -p 必须逐字不变:\n  in : %s\n  out: %s", cmd, got)
		}
	}
}

// TestScrubSecretsKeepsDockerPortPublish 不泛化：`docker run -p <端口映射>` 逐字不变，
// 以及裸 `ls -p`、`grep -p` 等同样不受影响。
func TestScrubSecretsKeepsDockerPortPublish(t *testing.T) {
	for _, cmd := range []string{
		"exec 容器 web: docker run -p 80:80 nginx",
		"exec 容器 web: docker run --rm -p 8080:80 -p 443:443 nginx",
		"exec 容器 web: ls -p /tmp",
		"exec 容器 web: find . -p",
	} {
		if got := ScrubSecrets(cmd); got != cmd {
			t.Errorf("-p 非 sshpass 形态必须逐字不变:\n  in : %s\n  out: %s", cmd, got)
		}
	}
}

// TestScrubSecretsSSHPassIdempotent 已脱敏文本再过一遍结果不变（写侧/读侧各过一遍）。
func TestScrubSecretsSSHPassIdempotent(t *testing.T) {
	for _, in := range []string{
		"exec 容器 ct-a: sshpass -p hunter2 ssh root@10.0.0.9",
		"exec 容器 ct-a: sshpass -o StrictHostKeyChecking=no -p seCr3t ssh root@h",
		"exec 容器 web: docker run -p 80:80 nginx",
		"exec 容器 web: mkdir -p /a/b",
	} {
		once := ScrubSecrets(in)
		if twice := ScrubSecrets(once); twice != once {
			t.Errorf("二次脱敏应与一次相同:\n  in   : %q\n  once : %q\n  twice: %q", in, once, twice)
		}
	}
}
