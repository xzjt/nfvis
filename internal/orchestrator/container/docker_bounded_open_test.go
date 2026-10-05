package container

// 决策 #366（R142-12）：dockerd 假死时 shell/exec 打开路径的**全链有界**。
//
// 真机 SIGSTOP 实测定形的挂点（本文件的背景）：WS 升级在 x/net 层、不碰 docker，
// 101 即时返回；真正挂住的是更早/更晚的 docker API 调用——State 检查与 exec create
// （HTTP 客户端无超时）。本文件用「睡死/阻塞的后端」把每一环的有界钉住：
//   - Exec / ExecShell 的 create（httptest 睡死服务，经 c.base 直连）
//   - Provider.ContainerShell 的 State（阻塞 fake api）
//
// 红绿口径：摘掉对应的有界（改回裸 ctx / 裸 State），下列断言逐项按预期失败。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// shortDockerCallTimeout 临时调小 dockerCallTimeout（用完还原）。
func shortDockerCallTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := dockerCallTimeout
	dockerCallTimeout = d
	t.Cleanup(func() { dockerCallTimeout = old })
}

// deadBackend 睡死 HTTP 后端：任何请求都不在测试时窗内应答。
func deadBackend(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestExecCreateBoundedUnderDeadBackend(t *testing.T) {
	shortDockerCallTimeout(t, 200*time.Millisecond)
	c := &dockerClient{http: &http.Client{}, base: deadBackend(t).URL}
	start := time.Now()
	_, err := c.Exec(context.Background(), "ct", "echo x", 5*time.Second)
	if err == nil {
		t.Fatal("后端睡死时 create 应有界失败")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("create 应在 dockerCallTimeout 内返回，实耗 %s", time.Since(start))
	}
	if !strings.Contains(err.Error(), "Docker 未在") {
		t.Fatalf("报错应可照做（Docker 未响应），得: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "已中止等待") {
		t.Fatalf("报错应保留超时因果，得: %v", err)
	}
}

func TestExecShellCreateBoundedUnderDeadBackend(t *testing.T) {
	shortDockerCallTimeout(t, 200*time.Millisecond)
	c := &dockerClient{http: &http.Client{}, base: deadBackend(t).URL}
	start := time.Now()
	_, err := c.ExecShell(context.Background(), "ct")
	if err == nil {
		t.Fatal("后端睡死时 shell 的 create 应有界失败")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("create 应在 dockerCallTimeout 内返回，实耗 %s", time.Since(start))
	}
	if !strings.Contains(err.Error(), "Docker 未在") {
		t.Fatalf("报错应可照做（Docker 未响应），得: %v", err)
	}
}

// blockingStateAPI State 阻塞到调用方 ctx 结束（模拟 dockerd 假死）。
type blockingStateAPI struct {
	dockerAPI
}

func (b *blockingStateAPI) State(ctx context.Context, _ string) (string, bool, error) {
	<-ctx.Done()
	return "", false, ctx.Err()
}

func TestContainerShellStateCheckBounded(t *testing.T) {
	shortDockerCallTimeout(t, 200*time.Millisecond)
	p := &Provider{api: &blockingStateAPI{}}
	done := make(chan error, 1)
	go func() {
		_, err := p.ContainerShell(context.Background(), "ct")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("State 挂死时 ContainerShell 应有界失败")
		}
		if !strings.Contains(err.Error(), "Docker 未在") {
			t.Fatalf("报错应可照做（Docker 未响应），得: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("State 挂死时 ContainerShell 挂住未归（有界缺失）")
	}
}
