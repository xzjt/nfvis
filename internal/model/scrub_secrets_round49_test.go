package model

// round49（审计脱敏收口）：文本级脱敏（ScrubSecrets）的判据。
//
// 背景：容器 exec 的命令原文会进审计（可追溯），而审计有**多个出口**（CLI show log audit、
// GET /audit-logs、tech-support 的 audit.json）——命令里可能带口令（`mysql -p…`、
// `--password=…`、`Authorization: Bearer …`、内嵌 PEM 私钥）。契约（openapi）承诺
// 「口令、口令哈希与证书/私钥正文一律不入审计」，故这里锁定判据。
//
// 两个方向都要钉：**秘密必须被掩码**，**普通命令必须逐字不变**（否则审计文本失去价值，
// 而且「什么都改」等于没脱敏——没人看得出哪段被动过）。

import (
	"strings"
	"testing"
)

func TestScrubSecretsMasksSecrets(t *testing.T) {
	cases := []struct {
		why  string
		in   string
		want []string // 必须**不**出现的明文片段
	}{
		{"mysql 短选项贴值", "exec 容器 db: mysql -p123456 -h 10.0.0.1", []string{"123456"}},
		{"长选项等号", "exec 容器 db: mysqldump --password=hunter2 app", []string{"hunter2"}},
		{"长选项空白分隔", "exec 容器 db: pg_dump --password hunter2", []string{"hunter2"}},
		{"Authorization 头", `exec 容器 api: curl -H 'Authorization: Bearer abc.def.ghi' http://x`, []string{"abc.def.ghi"}},
		{"裸 Bearer", `exec 容器 api: curl -H "Bearer e30xOpaqueToken" http://x`, []string{"e30xOpaqueToken"}},
		{"api key（下划线）", "exec 容器 api: api_key=AIzaSySecretValue", []string{"AIzaSySecretValue"}},
		{"api-key 冒号", "exec 容器 api: api-key: sk-live-9f8e", []string{"sk-live-9f8e"}},
		{"token 键", "exec 容器 api: token=eyJhbGciOi", []string{"eyJhbGciOi"}},
		{"secret 键", "exec 容器 api: secret: VerySecret", []string{"VerySecret"}},
		{"pwd 键", "exec 容器 db: pwd=Passw0rd!", []string{"Passw0rd!"}},
		{"private key 键", "exec 容器 api: private_key=MFECAwEAAQ", []string{"MFECAwEAAQ"}},
		{"JSON 形态", `exec 容器 api: curl -d '{"token":"abc123"}' http://x`, []string{"abc123"}},
	}
	for _, c := range cases {
		t.Run(c.why, func(t *testing.T) {
			got := ScrubSecrets(c.in)
			for _, leak := range c.want {
				if strings.Contains(got, leak) {
					t.Fatalf("明文秘密 %q 未被掩码: %s", leak, got)
				}
			}
			if !strings.Contains(got, RedactedPlaceholder) {
				t.Fatalf("应留下脱敏占位（脱敏≠掏空）: %s", got)
			}
		})
	}
}

// PEM 私钥正文（多行块）必须整块掩码。
func TestScrubSecretsMasksPEMBlock(t *testing.T) {
	in := "exec 容器 api: sh -c 'cat > k.pem <<EOF\n" +
		"-----BEGIN RSA PRIVATE KEY-----\n" +
		"MIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Qu\n" +
		"KUpRKfFLfRYC9AIKjbJTWit+CqvjWYzvQwECAwEAAQJAIJLixBy2qpFoS4DSmoEm\n" +
		"-----END RSA PRIVATE KEY-----\nEOF'"
	got := ScrubSecrets(in)
	if strings.Contains(got, "MIIBOgIBAAJBAKj34Gkx") || strings.Contains(got, "BEGIN RSA PRIVATE KEY") {
		t.Fatalf("PEM 私钥块必须整块掩码: %s", got)
	}
	if !strings.Contains(got, RedactedPlaceholder) {
		t.Fatalf("应留下脱敏占位: %s", got)
	}
	if !strings.Contains(got, "cat > k.pem") {
		t.Fatalf("命令的非秘密部分应保留: %s", got)
	}
}

// 普通命令逐字不变（保守：没有「键 + 分隔符 + 值」就不动）。
func TestScrubSecretsKeepsOrdinaryCommands(t *testing.T) {
	for _, cmd := range []string{
		"exec 容器 web: ls -l /tmp",
		"exec 容器 web: echo hello",
		"exec 容器 web: ip addr show dev eth0",
		"exec 容器 web: tar -xzf /data/incoming/ct.tar -C /tmp",
		"exec 容器 web: mkdir -p /data/keep/alive",
		"exec 容器 web: openssl version",
		"exec 容器 web: df -h",
		"exec 容器 web: pwd",
		"exec 容器 web: du -sh /var/lib/nfvis",
	} {
		if got := ScrubSecrets(cmd); got != cmd {
			t.Errorf("普通命令必须逐字不变:\n  in : %s\n  out: %s", cmd, got)
		}
	}
}

// 幂等：写侧与读侧可能各过一遍（且归档里可能是已脱敏的历史行）。
func TestScrubSecretsIdempotent(t *testing.T) {
	for _, in := range []string{
		"exec 容器 db: mysql -p123456",
		"exec 容器 api: curl -H 'Authorization: Bearer abc.def' http://x",
		"exec 容器 db: mysqldump --password=hunter2 app",
		"exec 容器 web: ls -l /tmp",
		"",
	} {
		once := ScrubSecrets(in)
		if twice := ScrubSecrets(once); twice != once {
			t.Errorf("二次脱敏应与一次相同:\n  in   : %q\n  once : %q\n  twice: %q", in, once, twice)
		}
	}
}
