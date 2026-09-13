//go:build integration

package network

// M3-1 真机集成测试（build tag integration，CI 不跑）。
// 在 nfvis-vm 上运行：make integration（需 VPP 已启动，NFVIS_VPP_SOCK 缺省 /run/vpp/api.sock）。

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestGovppConnectRealVPP(t *testing.T) {
	sock := os.Getenv("NFVIS_VPP_SOCK")
	if sock == "" {
		sock = DefaultSocket
	}
	m := NewManager(Config{Socket: sock, RequiredVersion: RequiredVPPVersion}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ver, err := m.ConnectOnce(ctx)
	if err != nil {
		t.Fatalf("连接 VPP %s 失败: %v", sock, err)
	}
	t.Logf("已连接 VPP %s @ %s", ver, sock)
	if m.State() != StateConnected {
		t.Fatalf("状态应为 connected: %v", m.State())
	}
	m.Close()
}
