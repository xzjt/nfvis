package api

// R84-4：`request vpp restart` 不得把「VPP 没起来」报成成功。
//
// 起后健康校验落在注入的 restart 实现里（cmd/nfvisd 装配处：重启前预检大页池、
// 重启后有界等待 binary API 可连），CLI 侧只负责如实转达：
// 实现返回错误 → `%%` + 原因；返回 nil → 才是成功文案。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

func TestCLIRequestVPPRestartReportsFailure(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setVPPRestart(func(context.Context) error {
		return errors.New("VPP 重启后未起来：15s 内 binary API 未连上（连接 /run/vpp/api.sock: connection refused）")
	})

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request vpp restart").Output
	if !strings.Contains(out, "%%") {
		t.Fatalf("起后校验失败必须报错（`%%`），实际输出:\n%s", out)
	}
	if !strings.Contains(out, "未起来") {
		t.Fatalf("错误文案应说明 VPP 未起来/未连上，实际输出:\n%s", out)
	}
	if strings.Contains(out, "已按 committed 配置重启 VPP") {
		t.Fatalf("起后校验失败时不得出现成功文案，实际输出:\n%s", out)
	}
}

func TestCLIRequestVPPRestartReportsSuccess(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setVPPRestart(func(context.Context) error { return nil })

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request vpp restart").Output
	if strings.Contains(out, "%%") {
		t.Fatalf("重启并校验通过不该报错，实际输出:\n%s", out)
	}
	if !strings.Contains(out, "binary API 已连通") {
		t.Fatalf("成功文案应说明校验过 binary API，实际输出:\n%s", out)
	}
}
