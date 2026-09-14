// M5-5：硬件健康采集与阈值告警（FR-SYS-012）。
//
// 采集优先级（逐级降级，均不可用时返回空集合而非报错）：
//   - BMC：/dev/ipmi0 存在且有 ipmitool → `ipmitool sdr -j`（BMC 存在）；无则降级。
//   - 温度/风扇：无 BMC 时用 `sensors -j`（lm-sensors）；再不行读 /sys/class/thermal（内核自带）。
//   - 磁盘：`smartctl -j -H -A /dev/<dev>`（无 smartctl 则 SMART 状态 unknown）；设备经 /sys/block 枚举。
//
// 阈值（`set system health thresholds`）：CPU 温度 / 磁盘温度 / 磁盘使用率，越限经 AlarmSink 产生
// warning（≥ 阈值）或 critical（≥ 阈值×1.1）告警；磁盘使用率由 statfs 计算。
package system

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// HealthSensor 一个传感器读数（契约 HardwareHealth.sensors）。
type HealthSensor struct {
	Name   string  `json:"name"`
	Type   string  `json:"type"` // temperature|fan|voltage|power
	Value  float64 `json:"value"`
	Unit   string  `json:"unit"`
	Status string  `json:"status"` // ok|warning|critical
}

// HealthDisk 一块磁盘（契约 HardwareHealth.disks）。
type HealthDisk struct {
	Device      string  `json:"device"`
	SmartStatus string  `json:"smart_status"` // passed|failed|unknown
	TempCelsius float64 `json:"temp_celsius,omitempty"`
	WearPercent float64 `json:"wear_percent,omitempty"`
}

// HardwareHealth 硬件健康快照（契约 HardwareHealth）。
type HardwareHealth struct {
	BMCPresent      bool           `json:"bmc_present"`
	Sensors         []HealthSensor `json:"sensors"`
	Disks           []HealthDisk   `json:"disks"`
	RootUsedPercent float64        `json:"root_used_percent,omitempty"`
}

// HardwareProvider 硬件健康采集（命令与文件系统路径可注入，便于单测）。
type HardwareProvider struct {
	run        Runner
	sysBlock   string // 缺省 /sys/block
	thermalDir string // 缺省 /sys/class/thermal
	bmcDev     string // 缺省 /dev/ipmi0
	rootPath   string // 缺省 /
}

// NewHardwareProvider 构造。
func NewHardwareProvider(run Runner) *HardwareProvider {
	return &HardwareProvider{run: run, sysBlock: "/sys/block", thermalDir: "/sys/class/thermal",
		bmcDev: "/dev/ipmi0", rootPath: "/"}
}

// SetPaths 覆盖探测路径（测试用）。
func (h *HardwareProvider) SetPaths(sysBlock, thermalDir, bmcDev, root string) {
	h.sysBlock, h.thermalDir, h.bmcDev, h.rootPath = sysBlock, thermalDir, bmcDev, root
}

// Collect 采集硬件健康（不评估阈值；阈值状态由 Evaluate 填充）。
func (h *HardwareProvider) Collect(ctx context.Context) HardwareHealth {
	out := HardwareHealth{Sensors: []HealthSensor{}, Disks: []HealthDisk{}}
	if _, err := os.Stat(h.bmcDev); err == nil {
		if raw, err := h.run(ctx, "ipmitool", "sdr", "-j"); err == nil {
			if s := parseIPMISensors(raw); len(s) > 0 {
				out.BMCPresent = true
				out.Sensors = s
			}
		}
	}
	if len(out.Sensors) == 0 {
		if raw, err := h.run(ctx, "sensors", "-j"); err == nil {
			out.Sensors = append(out.Sensors, parseLmSensors(raw)...)
		}
	}
	if len(out.Sensors) == 0 {
		out.Sensors = h.thermalZones()
	}
	out.Disks = h.disks(ctx)
	if pct, ok := rootUsedPercent(h.rootPath); ok {
		out.RootUsedPercent = pct
	}
	return out
}

// Evaluate 按阈值标注传感器状态（ok/warning/critical）。
func (h *HardwareProvider) Evaluate(hh *HardwareHealth, cpuTemp, diskTemp, diskUsed int) []string {
	var violations []string
	statusOf := func(v, limit float64) string {
		switch {
		case limit <= 0:
			return "ok"
		case v >= limit*1.1:
			return "critical"
		case v >= limit:
			return "warning"
		}
		return "ok"
	}
	for i := range hh.Sensors {
		s := &hh.Sensors[i]
		if s.Type != "temperature" {
			continue
		}
		limit := 0
		isDisk := strings.Contains(strings.ToLower(s.Name), "disk") || strings.Contains(strings.ToLower(s.Name), "nvme") || strings.Contains(strings.ToLower(s.Name), "ssd")
		if isDisk {
			limit = diskTemp
		} else {
			limit = cpuTemp
		}
		s.Status = statusOf(s.Value, float64(limit))
		if s.Status != "ok" {
			violations = append(violations, fmt.Sprintf("%s=%.1f%s（阈值 %d）", s.Name, s.Value, s.Unit, limit))
		}
	}
	for i := range hh.Disks {
		d := &hh.Disks[i]
		if diskTemp > 0 && d.TempCelsius > 0 {
			st := statusOf(d.TempCelsius, float64(diskTemp))
			if st != "ok" {
				violations = append(violations, fmt.Sprintf("disk %s 温度 %.0f℃（阈值 %d）", d.Device, d.TempCelsius, diskTemp))
			}
		}
	}
	if diskUsed > 0 && hh.RootUsedPercent >= float64(diskUsed) {
		violations = append(violations, fmt.Sprintf("根文件系统使用率 %.1f%%（阈值 %d%%）", hh.RootUsedPercent, diskUsed))
	}
	sort.Strings(violations)
	return violations
}

// parseIPMISensors 解析 `ipmitool sdr -j`（数组，含 name/reading/units/type）。
func parseIPMISensors(raw string) []HealthSensor {
	var rows []struct {
		Name    string  `json:"name"`
		Reading float64 `json:"reading"`
		Units   string  `json:"units"`
		Type    string  `json:"type"`
	}
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil
	}
	out := make([]HealthSensor, 0, len(rows))
	for _, r := range rows {
		t := classifySensor(r.Name, r.Units)
		if t == "" {
			continue
		}
		out = append(out, HealthSensor{Name: r.Name, Type: t, Value: r.Reading, Unit: r.Units})
	}
	return out
}

// parseLmSensors 解析 `sensors -j`：递归收集含 "temp" 的 *_input 数值为温度。
func parseLmSensors(raw string) []HealthSensor {
	var root map[string]any
	if err := json.Unmarshal([]byte(raw), &root); err != nil {
		return nil
	}
	var out []HealthSensor
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			switch vv := v.(type) {
			case map[string]any:
				walk(prefix+k+"/", vv)
			case float64:
				lk := strings.ToLower(k)
				if !strings.HasSuffix(lk, "_input") {
					continue
				}
				// 温度项：tempN_input；风扇：fanN_input
				switch {
				case strings.HasPrefix(lk, "temp"):
					name := strings.TrimSuffix(prefix, "/")
					if idx := strings.Index(name, "/"); idx >= 0 {
						name = name[idx+1:]
					}
					out = append(out, HealthSensor{Name: name, Type: "temperature", Value: vv, Unit: "C"})
				case strings.HasPrefix(lk, "fan"):
					out = append(out, HealthSensor{Name: prefix + "fan", Type: "fan", Value: vv, Unit: "RPM"})
				}
			}
		}
	}
	walk("", root)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// thermalZones 读 /sys/class/thermal（内核自带，无需外部工具）。
func (h *HardwareProvider) thermalZones() []HealthSensor {
	entries, err := os.ReadDir(h.thermalDir)
	if err != nil {
		return nil
	}
	var out []HealthSensor
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "thermal_zone") {
			continue
		}
		base := filepath.Join(h.thermalDir, e.Name())
		raw, err := os.ReadFile(filepath.Join(base, "temp"))
		if err != nil {
			continue
		}
		milli, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
		if err != nil {
			continue
		}
		name := e.Name()
		if b, err := os.ReadFile(filepath.Join(base, "type")); err == nil {
			if t := strings.TrimSpace(string(b)); t != "" {
				name = t
			}
		}
		out = append(out, HealthSensor{Name: name, Type: "temperature", Value: milli / 1000.0, Unit: "C"})
	}
	return out
}

// disks 枚举块设备并读 SMART（无 smartctl 时 status=unknown）。
func (h *HardwareProvider) disks(ctx context.Context) []HealthDisk {
	entries, err := os.ReadDir(h.sysBlock)
	if err != nil {
		return []HealthDisk{}
	}
	var out []HealthDisk
	for _, e := range entries {
		n := e.Name()
		if !isBlockDeviceName(n) {
			continue
		}
		dev := "/dev/" + n
		d := HealthDisk{Device: dev, SmartStatus: "unknown"}
		if raw, err := h.run(ctx, "smartctl", "-j", "-H", "-A", dev); err == nil {
			var sc struct {
				SmartStatus struct {
					Passed bool `json:"passed"`
				} `json:"smart_status"`
				Temperature struct {
					Current float64 `json:"current"`
				} `json:"temperature"`
				NVMe struct {
					PercentUsed float64 `json:"percentage_used"`
				} `json:"nvme_smart_health_information_log"`
			}
			if json.Unmarshal([]byte(raw), &sc) == nil {
				if sc.SmartStatus.Passed {
					d.SmartStatus = "passed"
				} else {
					d.SmartStatus = "failed"
				}
				d.TempCelsius = sc.Temperature.Current
				d.WearPercent = sc.NVMe.PercentUsed
			}
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out
}

func isBlockDeviceName(n string) bool {
	for _, p := range []string{"sd", "nvme", "vd", "hd", "mmcblk"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

func classifySensor(name, units string) string {
	l := strings.ToLower(name)
	switch {
	case strings.Contains(l, "temp") || strings.EqualFold(units, "degrees C"):
		return "temperature"
	case strings.Contains(l, "fan") || strings.EqualFold(units, "RPM"):
		return "fan"
	case strings.Contains(l, "volt") || strings.EqualFold(units, "Volts"):
		return "voltage"
	case strings.Contains(l, "power") || strings.Contains(l, "watt"):
		return "power"
	}
	return ""
}
