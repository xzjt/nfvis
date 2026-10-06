//go:build !linux

package system

import (
	"context"
	"errors"
)

// defaultFirewallExec 非 Linux 平台不提供 nftables（本地开发/CI 返回不可用；
// 读视图据此如实报 applied=false + 原因，不编造）。
func defaultFirewallExec(ctx context.Context, stdin string, args ...string) (string, error) {
	return "", errors.New("本平台不支持 nftables（仅 Linux 宿主可用）")
}
