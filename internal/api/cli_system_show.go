package api

// M5-4/M5-9：`show system …`（运行态信息/诊断）、`show users`、`show log …` 的渲染。
// 运行态数据来自 DiagOpsRuntime、internal/metrics 主机采集与事务引擎（配置/审计）。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xzjt/nfvis/internal/metrics"
	"github.com/xzjt/nfvis/internal/model"
)

func (x *cliExecutor) execShowSystemDiag(t []string) string {
	if len(t) == 0 {
		return "%% 语法: show system <uptime|cpu|memory|storage|hugepages|core-dumps|tech-support>\n"
	}
	switch t[0] {
	case "tech-support", "core-dumps":
		if x.diagOps == nil {
			return errRuntimeUnavailable
		}
		return x.renderDiag(t)
	case "uptime", "cpu", "memory", "storage", "hugepages":
		return x.renderHostMetrics(t[0])
	case "hardware":
		return x.renderHardware()
	}
	return fmt.Sprintf("%% 无效命令: show system %s（可用：uptime|cpu|memory|storage|hugepages|core-dumps|tech-support）\n", strings.Join(t, " "))
}

func (x *cliExecutor) renderDiag(t []string) string {
	switch t[0] {
	case "tech-support":
		rows := x.diagOps.ListTechSupport()
		if len(rows) == 0 {
			return "（无诊断归档；生成：request system tech-support generate）\n"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%-44s %-10s %s\n", "File", "Size", "Created")
		items := make([]any, 0, len(rows))
		for _, f := range rows {
			items = append(items, anyToTree(f))
			fmt.Fprintf(&b, "%-44s %-10s %s\n", f.File, humanSize(f.SizeBytes), f.CreatedAt.Format("2006-01-02 15:04"))
		}
		x.structured = map[string]any{"tech_support": items}
		return b.String()
	case "core-dumps":
		rows := x.diagOps.ListCoreDumps()
		if len(rows) == 0 {
			return "（无 core dump）\n"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%-38s %-22s %-10s %s\n", "File", "Process", "Size", "Occurred")
		items := make([]any, 0, len(rows))
		for _, d := range rows {
			items = append(items, anyToTree(d))
			fmt.Fprintf(&b, "%-38s %-22s %-10s %s\n", d.File, d.Process, humanSize(d.SizeBytes), d.OccurredAt.Format("2006-01-02 15:04"))
		}
		x.structured = map[string]any{"core_dumps": items}
		return b.String()
	}
	return errRuntimeUnavailable
}

// renderHostMetrics 渲染主机运行态子集（数据源 internal/metrics，Linux 采集；非 Linux 为空）。
func (x *cliExecutor) renderHostMetrics(kind string) string {
	samples := metrics.HostMetrics()
	val := map[string]float64{}
	for _, s := range samples {
		if len(s.Labels) == 0 {
			val[s.Name] = s.Value
		}
	}
	line := func(label, name, format string) string {
		v, ok := val[name]
		if !ok {
			return fmt.Sprintf("%-14s -\n", label)
		}
		return fmt.Sprintf("%-14s "+format+"\n", label, v)
	}
	switch kind {
	case "uptime":
		v, ok := val["nfvis_system_uptime_seconds"]
		if !ok {
			return "（运行时长不可用）\n"
		}
		d := time.Duration(v) * time.Second
		return fmt.Sprintf("uptime         %s（%s）\n", d.Truncate(time.Second), formatUptime(d))
	case "cpu":
		return line("cpu-util", "nfvis_system_cpu_utilization_ratio", "%.4f")
	case "memory":
		var b strings.Builder
		b.WriteString(line("mem-total", "nfvis_system_memory_total_bytes", "%.0f"))
		b.WriteString(line("mem-avail", "nfvis_system_memory_available_bytes", "%.0f"))
		b.WriteString(line("hp-total", "nfvis_system_hugepages_total", "%.0f"))
		b.WriteString(line("hp-free", "nfvis_system_hugepages_free", "%.0f"))
		return b.String()
	case "hugepages":
		var b strings.Builder
		b.WriteString(line("hp-total", "nfvis_system_hugepages_total", "%.0f"))
		b.WriteString(line("hp-free", "nfvis_system_hugepages_free", "%.0f"))
		return b.String()
	case "storage":
		var b strings.Builder
		b.WriteString(line("disk-total", "nfvis_system_disk_total_bytes", "%.0f"))
		b.WriteString(line("disk-free", "nfvis_system_disk_free_bytes", "%.0f"))
		b.WriteString(line("disk-used", "nfvis_system_disk_used_ratio", "%.4f"))
		return b.String()
	}
	return errRuntimeUnavailable
}

func formatUptime(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
}

// execShowUsers：show users（来自 committed 配置的 login-users；口令哈希不外显）。
func (x *cliExecutor) execShowUsers() string {
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if cfg.System == nil || cfg.System.Login == nil || len(cfg.System.Login.Users) == 0 {
		return "（无本地用户）\n"
	}
	rows := make([]map[string]string, 0, len(cfg.System.Login.Users))
	for _, u := range cfg.System.Login.Users {
		cl := u.Class
		if cl == "" {
			cl = "read-only"
		}
		rows = append(rows, map[string]string{"name": u.Name, "class": cl})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["name"] < rows[j]["name"] })
	var b strings.Builder
	fmt.Fprintf(&b, "%-20s %s\n", "User", "Class")
	items := make([]any, 0, len(rows))
	for _, r := range rows {
		items = append(items, map[string]any{"name": r["name"], "class": r["class"]})
		fmt.Fprintf(&b, "%-20s %s\n", r["name"], r["class"])
	}
	x.structured = map[string]any{"users": items}
	return b.String()
}

// execShowLog：show log system|audit|vnf。
func (x *cliExecutor) execShowLog(t []string) string {
	if len(t) == 0 {
		return "%% 语法: show log <system|audit|vnf> [last <n>]\n"
	}
	last := 100
	if len(t) >= 3 && t[1] == "last" {
		if n, err := atoiSafe(t[2]); err == nil && n > 0 {
			last = n
		}
	}
	switch t[0] {
	case "audit":
		// 审计在 SQLite（FR-OPS-031），与 GET /audit-logs 同源
		trail, err := x.engine.AuditTrail(last)
		if err != nil {
			return "%% " + err.Error() + "\n"
		}
		if len(trail) == 0 {
			return "（无审计记录）\n"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%-20s %-12s %-24s %-8s %s\n", "Time", "User", "Action", "Result", "Detail")
		items := make([]any, 0, len(trail))
		for _, e := range trail {
			items = append(items, anyToTree(e))
			detail := e.Detail
			if len(detail) > 60 {
				detail = detail[:60] + "…"
			}
			detail = strings.ReplaceAll(detail, "\n", " ")
			fmt.Fprintf(&b, "%-20s %-12s %-24s %-8s %s\n", e.Time.Format("2006-01-02 15:04:05"), e.User, e.Action, e.Result, detail)
		}
		x.structured = map[string]any{"audit": items}
		return b.String()
	case "system":
		if x.logs == nil {
			return "（系统日志不可用：未接入日志来源）\n"
		}
		out, err := x.logs()
		if err != nil {
			return "%% " + err.Error() + "\n"
		}
		return tailLines(string(out), last)
	case "vnf":
		return "VNF 日志：容器经 request container-functions <n> log [last <n>]；VM 经 request virtual-machine-functions <n> console 读取串口。\n"
	}
	return "%% 无效命令: show log " + strings.Join(t, " ") + "（可用：system|audit|vnf）\n"
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return "（无日志）\n"
	}
	return strings.Join(lines, "\n") + "\n"
}

func atoiSafe(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// renderHardware：show system hardware（FR-SYS-012）。
func (x *cliExecutor) renderHardware() string {
	if x.hw == nil {
		return errRuntimeUnavailable
	}
	hh := x.hw.Collect(context.Background())
	th := model.HealthThresholds{}
	if cfg, err := x.engine.Committed(); err == nil && cfg.System != nil && cfg.System.Health != nil {
		th = *cfg.System.Health
	}
	violations := x.hw.Evaluate(&hh, th.CPUTempCelsius, th.DiskTempCelsius, th.DiskUsedPercent)
	var b strings.Builder
	fmt.Fprintf(&b, "bmc-present    %v\n", hh.BMCPresent)
	if len(hh.Sensors) == 0 {
		b.WriteString("sensors        （无可用传感器：无 BMC/IPMI，lm-sensors 与 /sys/class/thermal 均无读数）\n")
	} else {
		fmt.Fprintf(&b, "%-28s %-12s %-10s %s\n", "Sensor", "Type", "Value", "Status")
		for _, s := range hh.Sensors {
			fmt.Fprintf(&b, "%-28s %-12s %-10s %s\n", s.Name, s.Type, fmt.Sprintf("%.1f %s", s.Value, s.Unit), s.Status)
		}
	}
	if len(hh.Disks) == 0 {
		b.WriteString("disks          （未枚举到块设备）\n")
	} else {
		fmt.Fprintf(&b, "%-16s %-10s %-10s %s\n", "Disk", "SMART", "Temp", "Wear")
		for _, d := range hh.Disks {
			temp := "-"
			if d.TempCelsius > 0 {
				temp = fmt.Sprintf("%.0f C", d.TempCelsius)
			}
			wear := "-"
			if d.WearPercent > 0 {
				wear = fmt.Sprintf("%.0f%%", d.WearPercent)
			}
			fmt.Fprintf(&b, "%-16s %-10s %-10s %s\n", d.Device, d.SmartStatus, temp, wear)
		}
	}
	fmt.Fprintf(&b, "root-used      %.1f%%（阈值 %d%%）\n", hh.RootUsedPercent, th.DiskUsedPercent)
	if len(violations) > 0 {
		b.WriteString("越限: " + strings.Join(violations, "; ") + "\n")
	} else {
		b.WriteString("越限: （无）\n")
	}
	x.structured = map[string]any{"hardware": anyToTree(hh), "violations": violations}
	return b.String()
}
