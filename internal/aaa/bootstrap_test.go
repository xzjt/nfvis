package aaa

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// TestRandomPasswordIsShellSafe 覆盖决策 #80：
// 首启一次性口令会被用户从 journalctl 复制粘贴到 shell，因此生成的口令**不得**含
// 触发 shell 展开的字符（尤其 `!` 会触发交互式 bash 的历史展开
// `-bash: !Aa1: event not found`，导致刚拿到的口令根本用不了）。
func TestRandomPasswordIsShellSafe(t *testing.T) {
	for i := 0; i < 200; i++ {
		pw, err := randomPassword()
		if err != nil {
			t.Fatalf("randomPassword: %v", err)
		}
		// 满足默认策略（含复杂度）
		if bad := CheckPasswordPolicy(pw, &model.PasswordPolicy{Complexity: true}); len(bad) > 0 {
			t.Fatalf("生成口令不满足策略 %q: %v", pw, bad)
		}
		// bash 双引号内会被展开/有特殊含义的字符
		if strings.ContainsAny(pw, "!$`\\\"'") {
			t.Fatalf("生成口令含 shell 敏感字符（复制粘贴会失败）：%q", pw)
		}
	}
}
