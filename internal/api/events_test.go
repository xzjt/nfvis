package api

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/events"
)

// M5-1：/events（SSE）——订阅后实时收到发布的事件；支持 Last-Event-ID 补发。
func TestEventsSSE(t *testing.T) {
	bus := events.New()
	ts := newTestServerOpts(t, Options{Events: bus})
	token := loginAdmin(t, ts)

	req, err := http.NewRequest(http.MethodGet, ts.URL+APIPrefix+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	// 等订阅建立后发布事件
	deadline := time.Now().Add(2 * time.Second)
	for bus.Subscribers() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	bus.Publish(events.TypeVNFStateChanged, map[string]any{"name": "vm1", "state": "running"})

	reader := bufio.NewReader(resp.Body)
	gotType, gotData := "", ""
	for i := 0; i < 12; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("读取 SSE 帧: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			gotType = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			gotData = strings.TrimPrefix(line, "data: ")
		}
		if gotType != "" && gotData != "" {
			break
		}
	}
	if gotType != events.TypeVNFStateChanged {
		t.Fatalf("event 类型 = %q", gotType)
	}
	var ev events.Event
	if err := json.Unmarshal([]byte(gotData), &ev); err != nil {
		t.Fatalf("解析 data: %v (%s)", err, gotData)
	}
	if ev.Type != events.TypeVNFStateChanged || ev.Payload["state"] != "running" {
		t.Fatalf("事件内容: %+v", ev)
	}
}

// /events 未接入总线时 503。
func TestEventsUnavailable(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+APIPrefix+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("未接入总线应 503，实际 %d", resp.StatusCode)
	}
}

// config-committed 事件：引擎 commit 成功时发布（经 Options.OnCommitted 装配）。
func TestConfigCommittedEventPublished(t *testing.T) {
	bus := events.New()
	ts := newTestServerOpts(t, Options{Events: bus})
	token := loginAdmin(t, ts)

	// 一次真实配置变更并自动提交（PUT 接口描述）
	if status, _, data := cfgRequest(t, http.MethodPut, ts.URL+APIPrefix+"/interfaces/ens9f9", token,
		map[string]any{"name": "ens9f9", "description": "m5-events"},
		map[string]string{"X-NFVIS-Auto-Commit": "true"}); status != http.StatusOK {
		t.Fatalf("PUT interface: %d %s", status, data)
	}
	var found bool
	for _, ev := range bus.Since(0) {
		if ev.Type == events.TypeConfigCommitted {
			found = true
			if _, ok := ev.Payload["revision"]; !ok {
				t.Fatalf("config-committed 事件应含 revision: %+v", ev.Payload)
			}
		}
	}
	if !found {
		t.Fatalf("未收到 config-committed 事件: %+v", bus.Since(0))
	}
}

// M5-2：/metrics 无需鉴权即可抓取，含配置计数与告警计数。
func TestMetricsEndpoint(t *testing.T) {
	ts := newTestServerOpts(t, Options{Alarms: &fakeAlarmRuntime{rows: []AlarmRow{{
		ID: "alm-00001", Severity: "critical", Code: "VM_CRASHED", Message: "vm crashed",
		State: "active",
	}}}})
	// 无 Authorization 头
	resp, err := http.Get(ts.URL + APIPrefix + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	out := string(body)
	for _, want := range []string{
		"# TYPE nfvis_config_virtual_switches gauge",
		"nfvis_config_virtual_switches 0",
		"# TYPE nfvis_alarms_active gauge",
		`nfvis_alarms_active{severity="critical"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少 %q\n%s", want, out)
		}
	}
}

// M5-1：CLI 直连动作（不经 HTTP handler）也发布 vnf-state-changed。
func TestCLIActionPublishesVNFState(t *testing.T) {
	bus := events.New()
	x, engine := newCLIKit(t)
	x.setEventBus(bus)
	ct := newFakeCLIContainer()
	x.setComputeRuntime(nil, nil, nil, ct, nil)

	// 预置容器配置
	run(t, x, "admin", "super-user", "ssh",
		"configure",
		"set container-functions ct1 image alpine:3.20",
		"commit",
		"exit")
	_ = engine
	run(t, x, "admin", "super-user", "ssh", "request container-functions ct1 start")

	var found bool
	for _, ev := range bus.Since(0) {
		if ev.Type == events.TypeVNFStateChanged && ev.Payload["name"] == "ct1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("CLI start 应发布 vnf-state-changed: %+v", bus.Since(0))
	}
}
