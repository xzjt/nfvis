package api

// M3-8：GET /alarms 告警列表端点单测（FR-OPS-010）。

import (
	"encoding/json"
	"net/http"
	"strings"
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

// 决策 #307：GET /alarms 每行按契约形状带 time_synced——有值必须真的发得出来（true/false），
// **未知（探针未接入）时才省略**（nil，与审计侧同口径：缺失=未知，不代表已同步）。
func TestAlarmTimeSyncedShape(t *testing.T) {
	no := false
	rows := []AlarmRow{
		{ID: "alm-1", Severity: "warning", Code: "RECOVERY_UNCONVERGED", Message: "未同步",
			RaisedAt: time.Now(), State: "active", TimeSynced: &no},
		{ID: "alm-2", Severity: "warning", Code: "RECOVERY_UNCONVERGED", Message: "未知",
			RaisedAt: time.Now(), State: "active"}, // TimeSynced==nil ⇒ 合法缺席
	}
	ts := newTestServerOpts(t, Options{Alarms: &fakeAlarmRuntime{rows: rows}})
	token := loginAdmin(t, ts)
	status, _, data := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/alarms?state=all", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /alarms: %d %s", status, data)
	}
	var got []map[string]any
	if err := json.Unmarshal(data, &got); err != nil || len(got) != 2 {
		t.Fatalf("解析响应: %v %s", err, data)
	}
	if v, ok := got[0]["time_synced"].(bool); !ok || v {
		t.Errorf("第 1 条应带 time_synced=false（契约字段必须真的发出来），实得 %v", got[0]["time_synced"])
	}
	if _, ok := got[1]["time_synced"]; ok {
		t.Errorf("未知态（nil）应省略 time_synced，不得编造，实得 %v", got[1]["time_synced"])
	}
}

// 决策 #307：show alarms 人类可读输出——未同步时消息后标 [时钟未同步]，未知/已同步不标。
func TestShowAlarmsClockMark(t *testing.T) {
	no, yes := false, true
	fake := &fakeAlarmRuntime{rows: []AlarmRow{
		{ID: "alm-1", Severity: "warning", Code: "RECOVERY_UNCONVERGED", Message: "A 未同步",
			Source: "s1", RaisedAt: time.Now(), State: "active", TimeSynced: &no},
		{ID: "alm-2", Severity: "warning", Code: "RECOVERY_UNCONVERGED", Message: "B 已同步",
			Source: "s2", RaisedAt: time.Now(), State: "active", TimeSynced: &yes},
		{ID: "alm-3", Severity: "warning", Code: "RECOVERY_UNCONVERGED", Message: "C 未知",
			Source: "s3", RaisedAt: time.Now(), State: "active"},
	}}
	x, _ := newCLIKit(t)
	x.alarms = fake
	out := x.Execute("admin", "super-user", "ssh", "show alarms").Output
	if strings.Count(out, "[时钟未同步]") != 1 {
		t.Fatalf("应恰有 1 条未同步标记（仅 A），实际输出:\n%s", out)
	}
	// 标记必须落在 A 那一行，而不是 B/C。
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "A 未同步") && !strings.Contains(line, "[时钟未同步]") {
			t.Errorf("未同步告警行应带标记: %s", line)
		}
		if (strings.Contains(line, "B 已同步") || strings.Contains(line, "C 未知")) &&
			strings.Contains(line, "[时钟未同步]") {
			t.Errorf("已同步/未知告警行不得带标记: %s", line)
		}
	}
}
