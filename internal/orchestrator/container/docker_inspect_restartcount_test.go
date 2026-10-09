package container

// 决策 #432 的**真机字段位置**守护：Docker inspect 应答里 `RestartCount` 在**顶层**，不在 `State`。
//
// 由来（真机实测，Docker 29.1.3，nfvis-vm）：
//   `docker inspect ct-2 --format '{{.RestartCount}}'` → 10
//   `docker inspect ct-2 --format '{{.State.RestartCount}}'` → template parsing error:
//       map has no entry for key "RestartCount"
//   `State` 的键只有 Dead/Error/ExitCode/FinishedAt/OOMKilled/Paused/Pid/Restarting/Running/
//       StartedAt/Status —— 没有 RestartCount。
// 首版实现按 `State.RestartCount` 读，于是**永远** RestartsKnown=false：读视图省略该字段、
// 崩溃重启循环告警（CONTAINER_RESTART_LOOP）也不触发；单测当时用的是「按实现假设捏的」假应答，
// 所以全绿也挡不住——本条用**真机应答形状**钉死位置。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDockerInspectRestartCountIsTopLevel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/containers/ct-2/json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// 真机应答的**相关字段形状**（顶层 RestartCount + State.Status；State 内无 RestartCount）。
		_, _ = w.Write([]byte(`{"Id":"f77","Name":"/ct-2","RestartCount":10,
			"State":{"Status":"restarting","Running":false,"Restarting":true,"ExitCode":1,"OOMKilled":false}}`))
	}))
	t.Cleanup(srv.Close)

	c := &dockerClient{http: &http.Client{}, base: srv.URL}
	facts, exists, err := c.Inspect(context.Background(), "ct-2")
	if err != nil || !exists {
		t.Fatalf("Inspect 应成功：exists=%v err=%v", exists, err)
	}
	if !facts.RestartsKnown || facts.RestartCount != 10 {
		t.Fatalf("顶层 RestartCount 应被读到：known=%v count=%d（旧实现按 State.RestartCount 读 ⇒ known=false）",
			facts.RestartsKnown, facts.RestartCount)
	}
	if facts.RawState != "restarting" {
		t.Fatalf("原始状态应为 restarting，得 %q", facts.RawState)
	}
}

// 兼容回退：应答把 RestartCount 放在 State 里（非本底座形状）时也应读到，不因位置差异丢读数。
func TestDockerInspectRestartCountStateFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"State":{"Status":"running","RestartCount":7}}`))
	}))
	t.Cleanup(srv.Close)

	c := &dockerClient{http: &http.Client{}, base: srv.URL}
	facts, exists, err := c.Inspect(context.Background(), "ct-x")
	if err != nil || !exists {
		t.Fatalf("Inspect 应成功：exists=%v err=%v", exists, err)
	}
	if !facts.RestartsKnown || facts.RestartCount != 7 {
		t.Fatalf("State 内回退位置应被读到：known=%v count=%d", facts.RestartsKnown, facts.RestartCount)
	}
}

// 两边都没有该字段 ⇒ 如实「取不到」（不给 0，不编造）。
func TestDockerInspectRestartCountAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"State":{"Status":"running"}}`))
	}))
	t.Cleanup(srv.Close)

	c := &dockerClient{http: &http.Client{}, base: srv.URL}
	facts, _, err := c.Inspect(context.Background(), "ct-y")
	if err != nil {
		t.Fatalf("Inspect 应成功：%v", err)
	}
	if facts.RestartsKnown {
		t.Fatalf("应答无该字段时应为「取不到」，得 known=true count=%d", facts.RestartCount)
	}
}
