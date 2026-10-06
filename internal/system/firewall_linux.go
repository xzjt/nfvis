//go:build linux

package system

import (
	"context"
	"os/exec"
	"strings"
)

// defaultFirewallExec 真实执行 nft（stdin 非空时经标准输入喂脚本，`nft -f -`）。
// 输出取 CombinedOutput：nft 的错误在 stderr，读视图/日志需要它才能如实说明原因。
func defaultFirewallExec(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "nft", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
