//go:build linux

package network

// 决策 #68：vpp_get_stats 回退源的 Linux 实现（真机）。

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// vppGetStatsTool 经 VPP 自带工具读取 stats segment（与 VPP 同版本，解码保证正确）。
type vppGetStatsTool struct{ socket string }

// NewVppGetStatsTool 返回 vpp_get_stats 执行实现（socket 为空用工具缺省）。
func NewVppGetStatsTool(socket string) StatsTool { return vppGetStatsTool{socket: socket} }

func (t vppGetStatsTool) DumpMachine(ctx context.Context, pattern string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	args := make([]string, 0, 6)
	if t.socket != "" {
		args = append(args, "socket-name", t.socket)
	}
	args = append(args, "dump", "machine", pattern)
	out, err := exec.CommandContext(ctx, "vpp_get_stats", args...).CombinedOutput()
	// 连接失败时工具退出码非零且原因在 stderr（实测 "Couldn't connect to vpp, does … exist?"），
	// 取合并输出作为诊断而非返回裸 err。
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("vpp_get_stats: %s", msg)
	}
	return string(out), nil
}

// newDefaultStatsTool 由 binary API 套接字推导 stats 套接字（同目录 stats.sock）。
func newDefaultStatsTool(apiSock string) StatsTool {
	return NewVppGetStatsTool(StatsSocketFor(apiSock))
}
