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

// ---------- 决策 #468（R24-B）：事件 payload 的 state 动作词 ----------
//
// 口径：动作→状态词改**显式映射**（start→starting、stop→stopping、restart→restarting），
// 不再用 action+"ing" 直接拼接——stop 拼接得拼写错误的 "stoping"，消费方按该值匹配会被
// 固定拼写绑定。REST（VNF/容器）与 CLI 直连两路必须同源同值（事件类型与其余字段不变）。

// vnfStateOf 取最近一条匹配 resource/name 的 vnf-state-changed 事件的 state（无则空串）。
func vnfStateOf(bus *events.Bus, resource, name string) string {
	state := ""
	for _, ev := range bus.Since(0) {
		if ev.Type != events.TypeVNFStateChanged {
			continue
		}
		if ev.Payload["resource"] == resource && ev.Payload["name"] == name {
			state, _ = ev.Payload["state"].(string)
		}
	}
	return state
}

// REST VNF 动作：start/restart 逐字不变，stop 必须是 stopping。
func TestVMActionStateWordInEvent(t *testing.T) {
	bus := events.New()
	ts := newTestServerOpts(t, Options{VM: newFakeVM(), Events: bus})
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)
	cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		vmBody("fw-vm"), map[string]string{"X-NFVIS-Auto-Commit": "true"})

	for _, tc := range []struct{ action, want string }{
		{"start", "starting"}, {"restart", "restarting"}, {"stop", "stopping"},
	} {
		status, _, data := cfgRequest(t, http.MethodPost,
			ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm:"+tc.action, token, nil, nil)
		if status != http.StatusAccepted {
			t.Fatalf("%s 应 202: %d %s", tc.action, status, data)
		}
		if got := vnfStateOf(bus, "virtual-machine-functions", "fw-vm"); got != tc.want {
			t.Errorf("VNF %s 事件 state = %q，want %q", tc.action, got, tc.want)
		}
	}
}

// REST 容器动作：与 VNF 同源同值。
func TestContainerActionStateWordInEvent(t *testing.T) {
	bus := events.New()
	ts := newTestServerOpts(t, Options{Containers: newFakeCLIContainer(), Events: bus})
	token := loginAdmin(t, ts)
	seedContainerCommit(t, ts, token, "ct-a")

	for _, tc := range []struct{ action, want string }{
		{"start", "starting"}, {"restart", "restarting"}, {"stop", "stopping"},
	} {
		status, _, data := cfgRequest(t, http.MethodPost,
			ts.URL+APIPrefix+"/container-functions/ct-a:"+tc.action, token, nil, nil)
		if status != http.StatusAccepted {
			t.Fatalf("%s 应 202: %d %s", tc.action, status, data)
		}
		if got := vnfStateOf(bus, "container-functions", "ct-a"); got != tc.want {
			t.Errorf("容器 %s 事件 state = %q，want %q", tc.action, got, tc.want)
		}
	}
}

// CLI 直连动作（不经 HTTP handler）与 REST 同源：`request … stop` 发布的 state 同样是 stopping。
func TestCLIActionStateWordInEvent(t *testing.T) {
	bus := events.New()
	x, _ := newCLIKit(t)
	x.setEventBus(bus)
	x.setComputeRuntime(newFakeVM(), nil, nil, newFakeCLIContainer(), nil)

	seedVMConfig(t, x)
	run(t, x, "admin", "super-user", "ssh",
		"configure",
		"set container-functions ct1 image alpine:3.20",
		"commit",
		"exit")

	run(t, x, "admin", "super-user", "ssh", "request virtual-machine-functions fw-vm stop")
	if got := vnfStateOf(bus, "virtual-machine-functions", "fw-vm"); got != "stopping" {
		t.Errorf("CLI VNF stop 事件 state = %q，want %q", got, "stopping")
	}
	run(t, x, "admin", "super-user", "ssh", "request container-functions ct1 stop")
	if got := vnfStateOf(bus, "container-functions", "ct1"); got != "stopping" {
		t.Errorf("CLI 容器 stop 事件 state = %q，want %q", got, "stopping")
	}
}

// 映射函数本身：三动作显式词 + 未知动作保留 action+"ing" 兜底。
func TestVNFActionStateMapping(t *testing.T) {
	for _, tc := range []struct{ action, want string }{
		{"start", "starting"},
		{"stop", "stopping"},
		{"restart", "restarting"},
		{"migrate", "migrateing"}, // 未知动作：保留拼接兜底（决策 #468）
	} {
		if got := vnfActionState(tc.action); got != tc.want {
			t.Errorf("vnfActionState(%q) = %q，want %q", tc.action, got, tc.want)
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
