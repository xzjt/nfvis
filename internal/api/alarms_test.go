package api

// M3-8：GET /alarms 告警列表端点单测（FR-OPS-010）。

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

type fakeAlarmRuntime struct {
	lastState string
	rows      []AlarmRow
}

func (f *fakeAlarmRuntime) List(state string) []AlarmRow {
	f.lastState = state
	return f.rows
}

func (f *fakeAlarmRuntime) Clear(id string, all bool) int { return 0 }

func TestAlarmsEndpoint(t *testing.T) {
	raised := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	fake := &fakeAlarmRuntime{rows: []AlarmRow{{
		ID: "alm-00001", Severity: "error", Code: "RECOVERY_IFACE_MISSING",
		Message: "接口在 VPP 中不存在: ens224", Source: "virtual-switches/vs-b",
		RaisedAt: raised, State: "active",
	}}}
	ts := newTestServerOpts(t, Options{Alarms: fake})
	token := loginAdmin(t, ts)

	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/alarms?state=all", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /alarms: %d %s", status, data)
	}
	if fake.lastState != "all" {
		t.Fatalf("state 过滤应透传，实际 %q", fake.lastState)
	}
	var got []AlarmRow
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if len(got) != 1 || got[0].Code != "RECOVERY_IFACE_MISSING" || got[0].Source != "virtual-switches/vs-b" {
		t.Fatalf("告警内容不符: %+v", got)
	}
}

// 无活动告警时返回空数组而非 null。
func TestAlarmsEndpointEmpty(t *testing.T) {
	ts := newTestServerOpts(t, Options{Alarms: &fakeAlarmRuntime{}})
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/alarms", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /alarms: %d %s", status, data)
	}
	if string(data) != "[]\n" {
		t.Fatalf("空告警应返回 []，实际 %q", data)
	}
}

// 未装配告警表时返回 503。
func TestAlarmsEndpointUnavailable(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	status, _, _ := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/alarms", token, nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("未装配应 503，实际 %d", status)
	}
}
