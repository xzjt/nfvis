package api

// 决策 #396（R171-16）③：CLI 容器调用点传有界 ctx。
//
// 契约点名的漏网点在 CLI 侧此前全是 context.Background()：shell 前置 State、ContainerLogs、
// 容器 start/stop/restart，以及 show container-functions 的 ctStateOf。dockerd 假死时这些
// 调用会无界挂起（且 ExecuteAs 全程持 cliExecutor.mu ⇒ 阻塞所有 CLI 会话）。本用例用「阻塞
// 到 ctx 结束」的运行态 fake 把每一处钉住：调用点必须传有界 ctx，命令才能在时窗内返回。
//
// 红-绿口径：把对应调用点改回 context.Background()，下列子用例按预期无界挂起 ⇒ 失败。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/aaa"
)

// blockingCT 运行态 fake：契约点名的每个调用都阻塞到 ctx 结束（模拟 dockerd 假死），
// 返回 ctx.Err()。未覆盖的方法由内嵌 fake 提供。
type blockingCT struct{ *fakeCLIContainer }

func blockUntilCtx(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (b *blockingCT) StartContainer(ctx context.Context, _ string) error { return blockUntilCtx(ctx) }
func (b *blockingCT) StopContainer(ctx context.Context, _ string) error  { return blockUntilCtx(ctx) }
func (b *blockingCT) RestartContainer(ctx context.Context, _ string) error {
	return blockUntilCtx(ctx)
}
func (b *blockingCT) ContainerState(ctx context.Context, _ string) (string, error) {
	return "", blockUntilCtx(ctx)
}
func (b *blockingCT) ContainerLogs(ctx context.Context, _ string, _ int) (string, error) {
	return "", blockUntilCtx(ctx)
}

// shortContainerCallTimeout 临时调小 containerCallTimeout（用完还原）。
func shortContainerCallTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := containerCallTimeout
	containerCallTimeout = d
	t.Cleanup(func() { containerCallTimeout = old })
}

// runBounded 在 goroutine 里执行一条 CLI 命令，断言其在时窗内返回（不无界挂起）。
func runBounded(t *testing.T, x *cliExecutor, line string) (string, bool) {
	t.Helper()
	done := make(chan string, 1)
	go func() { done <- x.Execute("admin", aaa.ClassSuperUser, "ssh", line).Output }()
	select {
	case out := <-done:
		return out, true
	case <-time.After(3 * time.Second):
		return "", false
	}
}

// newBlockingCLI 造一个「容器运行态全部阻塞到 ctx 结束」的 CLI 执行器（每个子用例独立
// 一份，避免某个子用例的挂起经 cliExecutor.mu 级联到其它子用例，红-绿各自独立）。
func newBlockingCLI(t *testing.T) *cliExecutor {
	t.Helper()
	x, _ := newCLIKit(t)
	seedContainerConfig(t, x)
	x.setComputeRuntime(nil, nil, nil, &blockingCT{newFakeCLIContainer()}, nil)
	// shell 的前置 State 在 issueShell 之后才判，故注入一个占位签发器以覆盖该路径。
	x.issueShell = func(name, user string) (string, int, error) { return "/ws/ct", 30, nil }
	return x
}

func TestCLIContainerCallsBounded(t *testing.T) {
	shortContainerCallTimeout(t, 200*time.Millisecond)

	// request 族：dockerd 假死 ⇒ 有界失败（%% 行）。
	for _, line := range []string{
		"request container-functions sbc-ct1 start",
		"request container-functions sbc-ct1 stop",
		"request container-functions sbc-ct1 restart",
		"request container-functions sbc-ct1 log",
	} {
		line := line
		t.Run(line, func(t *testing.T) {
			x := newBlockingCLI(t)
			out, ok := runBounded(t, x, line)
			if !ok {
				t.Fatalf("%s 无界挂起（CLI 调用点未传有界 ctx）", line)
			}
			if !strings.Contains(out, "%%") {
				t.Fatalf("dockerd 假死时应如实报错，得: %q", out)
			}
		})
	}

	// show container-functions：运行态查询有界 ⇒ 命令返回（state 如实降级为 "-"）。
	t.Run("show container-functions", func(t *testing.T) {
		x := newBlockingCLI(t)
		out, ok := runBounded(t, x, "show container-functions")
		if !ok {
			t.Fatal("show container-functions 无界挂起（ctStateOf 未传有界 ctx）")
		}
		if !strings.Contains(out, "sbc-ct1") {
			t.Fatalf("列表应仍渲染配置实体，得: %q", out)
		}
	})

	// shell 前置 State：有界 ⇒ 命令返回（State 超时被忽略、照旧签发 ticket）。
	t.Run("request container-functions shell", func(t *testing.T) {
		x := newBlockingCLI(t)
		if _, ok := runBounded(t, x, "request container-functions sbc-ct1 shell"); !ok {
			t.Fatal("shell 前置 State 无界挂起（未传有界 ctx）")
		}
	})
}
