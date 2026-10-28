package model

// round51 评审 L14：文本级脱敏（ScrubSecrets）此前不认 URL 里的 userinfo——
// `request system software add https://user:S3cret@repo/…` 的意图明细进审计后，只读账号
// 经 GET /audit-logs / show log audit / 诊断包就能读到明文口令。判据：掩掉 userinfo、
// 保留 scheme 与 host（审计要看得清「装的哪个仓库的包」），且普通 URL 与既有规则逐字不变。

import "testing"

func TestFixR51ScrubSecretsMasksURLUserinfo(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"https 用户口令", "request system software add https://user:S3cret@repo.example/nfvis.deb",
			"request system software add https://" + RedactedPlaceholder + "@repo.example/nfvis.deb"},
		{"http 无端口", "curl http://ops:P%40ss@10.0.0.5/pkg.deb",
			"curl http://" + RedactedPlaceholder + "@10.0.0.5/pkg.deb"},
		{"账号名恰为关键字（先整段掩掉）", "add https://password:S3cret@repo/x.deb",
			"add https://" + RedactedPlaceholder + "@repo/x.deb"},
		{"URL 与 sha256 同现", "add https://u:p@repo/x.deb sha256 deadbeef",
			"add https://" + RedactedPlaceholder + "@repo/x.deb sha256 deadbeef"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ScrubSecrets(c.in)
			if got != c.want {
				t.Fatalf("ScrubSecrets(%q) = %q，期望 %q", c.in, got, c.want)
			}
			if got2 := ScrubSecrets(got); got2 != got {
				t.Fatalf("脱敏须幂等：二次结果 %q ≠ %q", got2, got)
			}
		})
	}
}

// 普通 URL / 无凭据 URL / 既有覆盖形态逐字不变（不得因新增规则而误伤）。
func TestFixR51ScrubSecretsKeepsPlainURLsAndLongstandingForms(t *testing.T) {
	unchanged := []string{
		"request system software add https://repo.example/nfvis.deb",
		"request system software add http://10.0.0.5/pkg.deb sha256 abc123",
		"ssh://host/path",
		"scp user@host:/tmp/x.deb .", // 无冒号分隔的 userinfo：无口令可掩
		"mkdir -p /a/b",
		"docker run -p 80:80 nginx",
		"ls -p /tmp",
	}
	for _, cmd := range unchanged {
		if got := ScrubSecrets(cmd); got != cmd {
			t.Fatalf("不得误伤（%q）：%q", cmd, got)
		}
	}
	// 既有规则仍生效（回归：短选项贴值 / 键值 / sshpass 分隔形态）。
	if got := ScrubSecrets("mysql -p987654"); got != "mysql -p"+RedactedPlaceholder {
		t.Fatalf("短选项贴值规则应仍生效: %q", got)
	}
	if got := ScrubSecrets("--password=S3cret"); got != "--password="+RedactedPlaceholder {
		t.Fatalf("键值规则应仍生效: %q", got)
	}
	if got := ScrubSecrets("sshpass -p 'P@ss w0rd' ssh h"); got != "sshpass -p "+RedactedPlaceholder+" ssh h" {
		t.Fatalf("sshpass 分隔形态应仍生效: %q", got)
	}
}
