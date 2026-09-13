package api

import (
	"strings"
	"testing"
)

// M5-9：show users / show log audit / request alarms clear.
func TestCLISystemShowAndAlarmsClear(t *testing.T) {
	x, _ := newCLIKit(t)

	out := run(t, x, "admin", "super-user", "ssh", "show users")
	if !strings.Contains(out, "admin") || !strings.Contains(out, "Class") {
		t.Fatalf("show users: %s", out)
	}

	// 产生一条审计记录后查看
	run(t, x, "admin", "super-user", "ssh", "configure", "set interfaces ens4f0 description x", "commit", "exit")
	out = run(t, x, "admin", "super-user", "ssh", "show log audit last 50")
	if !strings.Contains(out, "config.commit") {
		t.Fatalf("show log audit 应含 config.commit: %s", out)
	}

	// 清除告警（注入 fake 告警表）
	fa := &fakeCLIAlarms{rows: 2}
	x.setNetRuntime(nil, nil, nil, nil, fa)
	out = run(t, x, "admin", "super-user", "ssh", "request alarms clear all")
	if !strings.Contains(out, "已清除 2 条") {
		t.Fatalf("alarms clear: %s", out)
	}
	if fa.clearedAll != true {
		t.Fatalf("应以 all 清除: %+v", fa)
	}
	if got := x.Execute("admin", "super-user", "ssh", "request alarms clear"); !strings.Contains(got.Output, "语法") {
		t.Fatalf("缺参数应报语法: %s", got.Output)
	}
}

type fakeCLIAlarms struct {
	rows       int
	clearedAll bool
}

func (f *fakeCLIAlarms) List(state string) []AlarmRow {
	out := make([]AlarmRow, 0, f.rows)
	for i := 0; i < f.rows; i++ {
		out = append(out, AlarmRow{ID: "alm-00001", Severity: "warning", Code: "X", State: "resolved"})
	}
	return out
}
func (f *fakeCLIAlarms) Clear(id string, all bool) int {
	f.clearedAll = all
	n := f.rows
	f.rows = 0
	return n
}

// M5-9：show nat 渲染配置（池/规则）与会话。
func TestCLIShowNatRendersConfig(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", "super-user", "ssh",
		"configure",
		"set interfaces ens5f0 description inside",
		"set interfaces ens6f0 description uplink",
		"set virtual-switches nl3 type l3",
		"set virtual-switches nl3 l3-interface ens5f0 ip address 10.5.0.1/24",
		"set virtual-switches wan type l3",
		"set virtual-switches wan l3-interface ens6f0 ip address 203.0.113.1/24",
		"set nat rules 10 match source 10.5.0.0/24 virtual-switch nl3 action interface ens6f0",
		"commit",
		"exit")

	// 运行态未接入时仍应渲染配置行
	out := run(t, x, "admin", "super-user", "ssh", "show nat")
	if !strings.Contains(out, "rules 10 match source 10.5.0.0/24 virtual-switch nl3 action interface ens6f0") {
		t.Fatalf("show nat 应渲染规则: %s", out)
	}
}
