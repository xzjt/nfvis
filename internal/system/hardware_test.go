package system

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParseIPMISensors(t *testing.T) {
	raw := `[{"name":"CPU1 Temp","reading":58,"units":"degrees C","type":"Temperature"},
	          {"name":"FAN1","reading":4200,"units":"RPM","type":"Fan"},
	          {"name":"Total Power","reading":210,"units":"Watts","type":"Power Supply"}]`
	got := parseIPMISensors(raw)
	if len(got) != 3 {
		t.Fatalf("应解析 3 个传感器: %+v", got)
	}
	if got[0].Type != "temperature" || got[1].Type != "fan" || got[2].Type != "power" {
		t.Fatalf("类型判定: %+v", got)
	}
}

func TestParseLmSensors(t *testing.T) {
	raw := `{"coretemp-isa-0000":{"Package id 0":{"temp1_input":45.0,"temp1_max":84.0},
	        "Core 0":{"temp2_input":44.0}},"thinkpad-isa-0000":{"fan1":{"fan1_input":2100}}}`
	got := parseLmSensors(raw)
	var temps, fans int
	for _, s := range got {
		switch s.Type {
		case "temperature":
			temps++
		case "fan":
			fans++
		}
	}
	if temps != 2 || fans != 1 {
		t.Fatalf("温度 2 / 风扇 1，实际 temps=%d fans=%d（%+v）", temps, fans, got)
	}
}

func TestEvaluateThresholds(t *testing.T) {
	h := NewHardwareProvider(nil)
	hh := HardwareHealth{
		Sensors: []HealthSensor{
			{Name: "CPU1 Temp", Type: "temperature", Value: 70, Unit: "C"},
			{Name: "FAN1", Type: "fan", Value: 3000, Unit: "RPM"},
		},
		Disks:           []HealthDisk{{Device: "/dev/sda", SmartStatus: "passed", TempCelsius: 55}},
		RootUsedPercent: 95,
	}
	v := h.Evaluate(&hh, 85, 60, 90)
	if len(v) != 1 {
		t.Fatalf("仅磁盘使用率应越限: %+v（%v）", hh, v)
	}
	// 温度达到阈值×1.1 → critical
	hh2 := HardwareHealth{Sensors: []HealthSensor{{Name: "CPU1 Temp", Type: "temperature", Value: 95, Unit: "C"}}}
	_ = h.Evaluate(&hh2, 85, 0, 0)
	if hh2.Sensors[0].Status != "critical" {
		t.Fatalf("95 ≥ 85×1.1 应为 critical: %+v", hh2.Sensors[0])
	}
	// 未设阈值（0）不告警
	hh3 := HardwareHealth{Sensors: []HealthSensor{{Name: "CPU1 Temp", Type: "temperature", Value: 99, Unit: "C"}}}
	if v := h.Evaluate(&hh3, 0, 0, 0); len(v) != 0 || hh3.Sensors[0].Status != "ok" {
		t.Fatalf("未设阈值不应告警: %v %+v", v, hh3.Sensors[0])
	}
}

func TestCollectDegradesWithoutBMC(t *testing.T) {
	// 无 BMC 设备、无 ipmitool/sensors → 仅 /sys/class/thermal 兜底
	thermal := t.TempDir()
	_ = os.MkdirAll(filepath.Join(thermal, "thermal_zone0"), 0o755)
	_ = os.WriteFile(filepath.Join(thermal, "thermal_zone0", "temp"), []byte("47000\n"), 0o644)
	_ = os.WriteFile(filepath.Join(thermal, "thermal_zone0", "type"), []byte("x86_pkg_temp\n"), 0o644)
	sysBlock := t.TempDir()
	_ = os.MkdirAll(filepath.Join(sysBlock, "sda"), 0o755)

	fr := &fakeRunner{}
	h := NewHardwareProvider(fr.run)
	h.SetPaths(sysBlock, thermal, filepath.Join(t.TempDir(), "no-ipmi"), t.TempDir())

	hh := h.Collect(context.Background())
	if hh.BMCPresent {
		t.Fatal("无 BMC 设备不应报告 bmc_present")
	}
	if len(hh.Sensors) != 1 || hh.Sensors[0].Name != "x86_pkg_temp" || hh.Sensors[0].Value != 47 {
		t.Fatalf("应回退到 /sys/class/thermal: %+v", hh.Sensors)
	}
	if len(hh.Disks) != 1 || hh.Disks[0].Device != "/dev/sda" || hh.Disks[0].SmartStatus != "unknown" {
		t.Fatalf("无 smartctl 时 SMART 应为 unknown: %+v", hh.Disks)
	}
}
