package main

// M13（round51 评审）：口令曾作为 `-p` 的**标志缺省值**从 NFVIS_PASSWORD 读入——
// Go 的 flag.PrintDefaults 会把非空字符串缺省值原样印出来（`(default "<口令>")`），
// 于是 `nfvis-cli -h`、`--help` 或任何拼错的标志都把它打到 stdout/stderr：
// 终端回滚缓冲、CI 日志、支持包都留着明文口令。
//
// 修法：-p 缺省恒为空串（取值移到 Parse 之后的 resolvePassword），用法路径不再有它。
// 本测试同时钉住「行为不变」：NFVIS_PASSWORD 仍被尊重、-p 仍优先。

import (
	"bytes"
	"strings"
	"testing"
)

// TestUsageDoesNotLeakPasswordFromEnv 用法输出（-h/--help/拼错标志都会走它）不得含口令，
// 且 -p 的标志缺省值必须为空串（否则 PrintDefaults 又会印出来）。
func TestUsageDoesNotLeakPasswordFromEnv(t *testing.T) {
	const secret = "Sup3r-Secret-PW!"
	t.Setenv("NFVIS_PASSWORD", secret)

	fs, flags := newFlagSet()
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	fs.PrintDefaults() // -h / --help / 拼错标志打印用法的那一步
	fs.Usage()

	if strings.Contains(buf.String(), secret) {
		t.Fatalf("用法输出不得含 NFVIS_PASSWORD 的值（-h 会把它打出去）：\n%s", buf.String())
	}
	if got := fs.Lookup("p").DefValue; got != "" {
		t.Fatalf("-p 的标志缺省值必须是空串（PrintDefaults 会原样印出非空缺省），实得 %q", got)
	}
	// 帮助文本仍在（不是靠删掉标志来消除泄露）。
	if !strings.Contains(buf.String(), "-p ") {
		t.Errorf("用法里仍应有 -p 的说明：\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "NFVIS_PASSWORD") {
		t.Errorf("用法应说明口令来源（NFVIS_PASSWORD）：\n%s", buf.String())
	}

	// 行为不变：环境变量仍被尊重；-p 显式给出时优先。
	if got := flags.resolvePassword(); got != secret {
		t.Fatalf("NFVIS_PASSWORD 仍应被尊重，实得 %q", got)
	}
	*flags.password = "explicit-pw"
	if got := flags.resolvePassword(); got != "explicit-pw" {
		t.Fatalf("-p 应优先于环境变量，实得 %q", got)
	}
}

// TestUsageWithoutEnvHasNoSecretShape 负控：未设 NFVIS_PASSWORD 时用法里也不能出现任何
// `(default …)` 形式的 -p 缺省（防将来有人又把环境变量塞回标志缺省）。
func TestUsageWithoutEnvHasNoSecretShape(t *testing.T) {
	t.Setenv("NFVIS_PASSWORD", "")
	fs, _ := newFlagSet()
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	fs.PrintDefaults()
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "-p ") && strings.Contains(line, "default") {
			t.Fatalf("-p 不应带缺省值：%q", line)
		}
	}
}
