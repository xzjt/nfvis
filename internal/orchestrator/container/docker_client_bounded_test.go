package container

// 决策 #396（R171-16）：Docker 客户端**内部**对每次调用给硬上界（默认 dockerCallTimeout，
// 长操作按调用方 ctx 放宽）——不再按调用点选择性包裹。本文件用「睡死后端」把契约点名的
// 全部漏网点逐条钉住：
//   - State（ContainerState / shell 前置 / show container-functions 的 ctStateOf）
//   - Logs（ContainerLogs）
//   - start / stop / restart（持 p.mu，挂死即永久持有）
//   - 镜像 remove / load
//   - exec 收尾 GET /exec/{id}/json
//
// 红-绿口径：把对应方法改回裸 ctx（不加 boundedCtx），下列用例按预期无界挂起 ⇒ 失败。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// hangingBackend 永不主动应答的 HTTP 后端：请求挂到调用方 ctx 结束（有界）或测试清场
// （stop 关闭）。与 docker_bounded_open_test.go 的 deadBackend 同型，但清场不等 30s。
func hangingBackend(t *testing.T) *httptest.Server {
	t.Helper()
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(stop)
		srv.Close()
	})
	return srv
}

// assertBoundedFail 断言一次调用在测试时窗内有界失败（而不是无界挂起）。
func assertBoundedFail(t *testing.T, what string, call func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("%s：后端无响应时应有界失败，实际成功", what)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("%s：无界挂起（客户端内部硬上界缺失）", what)
	}
}

// TestClientStateBoundedUnderDeadBackend ContainerState 的底座调用（State）必须内部有界。
func TestClientStateBoundedUnderDeadBackend(t *testing.T) {
	shortDockerCallTimeout(t, 200*time.Millisecond)
	c := &dockerClient{http: &http.Client{}, base: hangingBackend(t).URL}
	assertBoundedFail(t, "State", func() error {
		_, _, err := c.State(context.Background(), "ct")
		return err
	})
}

// TestClientLogsBoundedUnderDeadBackend ContainerLogs 的底座调用（Logs，流式、不经 do）
// 必须内部有界。
func TestClientLogsBoundedUnderDeadBackend(t *testing.T) {
	shortDockerCallTimeout(t, 200*time.Millisecond)
	c := &dockerClient{http: &http.Client{}, base: hangingBackend(t).URL}
	assertBoundedFail(t, "Logs", func() error {
		_, err := c.Logs(context.Background(), "ct", 100)
		return err
	})
}

// TestClientLifecycleCallsBoundedUnderDeadBackend start/stop/restart/镜像 remove 经 do，
// 必须内部有界（这些调用在 Provider 里持 p.mu，无界即永久持有 ⇒ 提交/收敛一起阻塞）。
func TestClientLifecycleCallsBoundedUnderDeadBackend(t *testing.T) {
	shortDockerCallTimeout(t, 200*time.Millisecond)
	c := &dockerClient{http: &http.Client{}, base: hangingBackend(t).URL}
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"Start", func() error { return c.Start(context.Background(), "ct") }},
		{"Stop", func() error { return c.Stop(context.Background(), "ct") }},
		{"Restart", func() error { return c.Restart(context.Background(), "ct") }},
		{"RemoveImage", func() error { return c.RemoveImage(context.Background(), "alpine") }},
	} {
		assertBoundedFail(t, tc.name, tc.call)
	}
}

// TestClientLoadImageBoundedUnderDeadBackend 镜像载入（流式 body、不经 do）也必须内部有界。
func TestClientLoadImageBoundedUnderDeadBackend(t *testing.T) {
	shortDockerCallTimeout(t, 200*time.Millisecond)
	dir := t.TempDir()
	path := filepath.Join(dir, "img.tar")
	if err := os.WriteFile(path, []byte("not-a-real-tar"), 0o600); err != nil {
		t.Fatalf("造测试 tar: %v", err)
	}
	c := &dockerClient{http: &http.Client{}, base: hangingBackend(t).URL}
	assertBoundedFail(t, "LoadImage", func() error {
		return c.LoadImage(context.Background(), path, "alpine")
	})
}

// TestClientExecFinalInspectBounded exec 收尾的 `GET /exec/{id}/json`（退出码读取）必须
// 内部有界：create/start 正常、仅收尾挂死时，Exec 应在上界内返回（退出码记为 note、
// 输出保留），而不是无界挂起。
func TestClientExecFinalInspectBounded(t *testing.T) {
	shortDockerCallTimeout(t, 200*time.Millisecond)
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/containers/ct/exec":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"e1"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/exec/e1/start":
			w.WriteHeader(http.StatusOK) // 空多路复用流 ⇒ demux 干净结束
		default: // GET /exec/e1/json：挂死
			select {
			case <-stop:
			case <-r.Context().Done():
			}
		}
	}))
	t.Cleanup(func() {
		close(stop)
		srv.Close()
	})
	c := &dockerClient{http: &http.Client{}, base: srv.URL}

	done := make(chan ExecResult, 1)
	go func() {
		res, _ := c.Exec(context.Background(), "ct", "echo x", 5*time.Second)
		done <- res
	}()
	select {
	case res := <-done:
		if res.HasExitCode {
			t.Fatalf("收尾挂死时不应拿到退出码，得 %d", res.ExitCode)
		}
		if res.ExitCodeNote == "" {
			t.Fatal("收尾挂死应有 ExitCodeNote 如实说明退出码未读到")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("exec 收尾 GET /exec/{id}/json 无界挂起（内部硬上界缺失）")
	}
}

// TestClientBoundedCtxHonorsCallerDeadline 长操作按调用方 ctx 放宽（#396）：调用方给了
// 比缺省更长的 deadline 时，不得被缺省上界截断（否则大镜像 load 会被 10s 误杀）。
// 后端 500ms 后应答：缺省 200ms 会截断，调用方 3s 应放行。
func TestClientBoundedCtxHonorsCallerDeadline(t *testing.T) {
	shortDockerCallTimeout(t, 200*time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"State":{"Status":"running"}}`))
	}))
	t.Cleanup(srv.Close)
	c := &dockerClient{http: &http.Client{}, base: srv.URL}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if _, _, err := c.State(ctx, "ct"); err != nil {
		t.Fatalf("调用方 deadline 更宽时应放行（长操作放宽），实际: %v（耗时 %s）", err, time.Since(start))
	}
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Fatalf("应答在 500ms，不该更早返回，实耗 %s", elapsed)
	}
}
