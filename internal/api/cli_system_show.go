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
	ksys "github.com/xzjt/nfvis/internal/system"
)

// execShowSystemAPI：`show system api tokens`（决策 #301）——活动会话 / API Token 清单。
// 数据范围（super-user 全部 / 其他 class 仅自己）与「当前会话」标记的判据都在 aaa 实现
// （ListTokens / TokenView.Current）；执行器只负责渲染。callerTokenID 是发起本条命令的
// 会话 ID（ExecuteAs 注入），空串表示调用方没有稳定 ID 可比，标记列一律不加。
func (x *cliExecutor) execShowSystemAPI(user, class string, t []string) string {
	if len(t) != 1 || t[0] != "tokens" {
		return "%% 无效命令: show system api " + strings.Join(t, " ") + "（可用：tokens）\n"
	}
	if x.tokens == nil {
		return errRuntimeUnavailable
	}
	views := x.tokens.ListTokens(user, class)
	if len(views) == 0 {
		return "（无活动会话）\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-38s %-16s %-12s %-17s %-17s %s\n", "Token-ID", "User", "Class", "Issued", "Expires", "Session")
	items := make([]any, 0, len(views))
	for _, v := range views {
		current := v.Current(x.callerTokenID)
		items = append(items, map[string]any{
			"token_id": v.ID, "user": v.User, "class": v.Class,
			"issued_at": v.IssuedAt.Format(time.RFC3339), "expires_at": v.ExpiresAt.Format(time.RFC3339),
			"current": current,
		})
		mark := "-"
		if current {
			mark = "当前会话"
		}
		fmt.Fprintf(&b, "%-38s %-16s %-12s %-17s %-17s %s\n",
			v.ID, v.User, v.Class,
			v.IssuedAt.Format("2006-01-02 15:04"), v.ExpiresAt.Format("2006-01-02 15:04"), mark)
	}
	x.structured = map[string]any{"api_tokens": items}
	return b.String()
}

func (x *cliExecutor) execShowSystemDiag(t []string) string {
	if len(t) == 0 {
		return "%% 语法: show system <uptime|cpu|memory|storage|hugepages|metrics|core-dumps|tech-support>\n"
	}
	switch t[0] {
	case "tech-support", "core-dumps":
		if x.diagOps == nil {
			return errRuntimeUnavailable
		}
		return x.renderDiag(t)
	case "software":
		// 修 BUG-16：升级/回退的结论。`request system software add` 会重启 nfvisd 并断开
		// 承载该命令的连接，CLI 重连后靠这条读口拿「到底成没成」——不猜、不新增别的命令面。
		if len(t) < 2 || t[1] != "result" {
			return "%% 语法: show system software result\n"
		}
		return x.renderSoftwareResult()
	case "uptime", "cpu", "memory", "storage":
		return x.renderHostMetrics(t[0])
	case "hugepages":
		// 决策 #329/#346：数字（声明/内核实际/在用/实际持有/无主占用）+ 可回收，与其他三面（REST/Web）同源。
		return x.renderHugepagePools()
	case "metrics":
		// 决策 #356：历史时序读视图（`show system metrics history …`）。
		return x.renderMetricsHistory(t[1:])
	case "kernel":
		return x.renderKernelBaseline()
	case "hardware":
		return x.renderHardware()
	case "firewall":
		// 决策 #388：主机防火墙读视图（与 GET /system/firewall 同源）。
		return x.renderSystemFirewall()
	}
	return fmt.Sprintf("%% 无效命令: show system %s（可用：uptime|cpu|memory|storage|hugepages|metrics history|kernel|hardware|firewall|core-dumps|tech-support）\n", strings.Join(t, " "))
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
		return line("cpu-count", "nfvis_system_cpu_online_count", "%.0f") + line("cpu-util", "nfvis_system_cpu_utilization_ratio", "%.4f")
	case "memory":
		var b strings.Builder
		b.WriteString(line("mem-total", "nfvis_system_memory_total_bytes", "%.0f"))
		b.WriteString(line("mem-avail", "nfvis_system_memory_available_bytes", "%.0f"))
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
		trail, err := x.engine.AuditTrail(last, 0)
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
			// NFR-006：写入该条时宿主时钟**未与 NTP 同步**则带标记（时间戳可能不准）。
			// TimeSynced 为 nil 表示该记录**没有这个信息**（迁移前的老记录）——不加标记，
			// 也不谎称已同步。
			mark := ""
			if e.TimeSynced != nil && !*e.TimeSynced {
				mark = "  [时钟未同步]"
			}
			fmt.Fprintf(&b, "%-20s %-12s %-24s %-8s %s%s\n", e.Time.Format("2006-01-02 15:04:05"), e.User, e.Action, e.Result, detail, mark)
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
		// SMART 取不到时原因不能省：操作者要据此决定「装工具 / 换盘 / 查权限」，
		// 单看到一个 unknown 什么也做不了。原因不塞进窄列（会撞烂列宽、也读不成句），
		// 而是配对呈现为「设备: 原因」——正常盘的行与历史逐字节相同。
		var notes []string
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
			if d.SmartStatus == "unknown" && d.SmartReason != "" {
				notes = append(notes, fmt.Sprintf("%s: %s", d.Device, d.SmartReason))
			}
		}
		if len(notes) > 0 {
			b.WriteString("SMART unknown 原因:\n")
			for _, n := range notes {
				b.WriteString("  - " + n + "\n")
			}
		}
	}
	fmt.Fprintf(&b, "root-used      %.1f%%（阈值 %s）\n", hh.RootUsedPercent, thresholdText(th.DiskUsedPercent, "%"))
	if len(violations) > 0 {
		b.WriteString("越限: " + strings.Join(violations, "; ") + "\n")
	} else {
		b.WriteString("越限: （无）\n")
	}
	x.structured = map[string]any{"hardware": anyToTree(hh), "violations": violations}
	return b.String()
}

// renderKernelBaseline：内核启动基线三方对照（FR-SYS-014 / FR-CMP-005）。
//
//	内核基线 = /proc/cmdline（本次启动实际带入的参数）
//	运行实际 = /proc/meminfo、/sys（大页已分配、THP 当前策略、NMI watchdog）
//	配置期望 = committed 的 resource-pools（唯一真源）+ system kernel
func (x *cliExecutor) renderKernelBaseline() string {
	actual := ksys.ReadActual("/")
	desired, err := x.desiredKernelBaseline()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	diffs := ksys.Compare(desired, actual)
	// 决策 #459：已声明页尺寸若没有对应的 hugetlbfs 挂载点，也属「需重启/手工 mount 生效」的差异
	// （Compare 只比 cmdline）——否则会报「一致（无需重启）」而 2M 的 VM 其实还起不来。
	diffs = append(diffs, hugepageMountDiffs(desired)...)

	var b strings.Builder
	fmt.Fprintf(&b, "%-22s %-26s %s\n", "项", "内核基线(cmdline)", "运行实际")
	iso := ksys.IsolatedFromCmdline(actual.Cmdline)
	fmt.Fprintf(&b, "%-22s %-26s %s\n", "isolcpus", dash(iso), dash(iso))
	fmt.Fprintf(&b, "%-22s %-26s %s\n", "大页 1G", dash(ksys.HugepageFromCmdline(actual.Cmdline, "1G")), fmt.Sprintf("%d（free %d）", actual.Hugepages1G, actual.Hugepages1GFr))
	fmt.Fprintf(&b, "%-22s %-26s %s\n", "大页 2M", dash(ksys.HugepageFromCmdline(actual.Cmdline, "2M")), fmt.Sprintf("%d（free %d）", actual.Hugepages2M, actual.Hugepages2MFr))
	nmi := "-"
	if actual.NMIWatchdog != nil {
		nmi = fmt.Sprintf("%v", *actual.NMIWatchdog)
	}
	fmt.Fprintf(&b, "%-22s %-26s %s\n", "nmi_watchdog", "-", nmi)
	fmt.Fprintf(&b, "%-22s %-26s %s\n", "transparent_hugepage", "-", dash(actual.THP))
	b.WriteString("\n配置期望（resource-pools / system kernel 派生）：\n")
	fmt.Fprintf(&b, "  大页 1G=%s 2M=%s   isolcpus=%s   nmi_watchdog=%s   thp=%s\n",
		desiredInt(desired.Hugepages1G), desiredInt(desired.Hugepages2M), dash(desired.IsolatedCores), desiredBool(desired.NMIWatchdog), dash(desired.THP))
	if len(diffs) == 0 {
		b.WriteString("\n一致性：内核基线与配置期望一致（无需重启）\n")
		// 运行实际高于声明属运行期占用（内核不回收），不判不一致，但要说明清楚，
		// 否则操作者看到「实际 2（free 1）」与「一致」并存会以为漏判或想去重启。
		if notes := ksys.HugepageRuntimeNotes(desired, actual); len(notes) > 0 {
			b.WriteString("说明：" + strings.Join(notes, "；") + "\n")
		}
	} else {
		// 不一致项各自带处理口径（写 GRUB 基线重启 / 分配不足），不再统一断言「需写入 GRUB 并重启」——
		// 分配不足时 cmdline 基线本来就是对，照「写基线再重启」做永远不一致。
		b.WriteString("\n一致性：与配置期望不一致：\n")
		for _, d := range diffs {
			b.WriteString("  - " + d + "\n")
		}
	}
	x.structured = map[string]any{
		"cmdline": actual.Cmdline, "isolated_cores": iso,
		"hugepages_1g": actual.Hugepages1G, "hugepages_1g_free": actual.Hugepages1GFr,
		"hugepages_2m": actual.Hugepages2M, "hugepages_2m_free": actual.Hugepages2MFr,
		"thp": actual.THP, "desired": anyToTree(desired), "diffs": diffs,
	}
	return b.String()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// thresholdText 阈值文本：未设置（<=0，判定侧按「不产生告警」处理）时显示「未设置」而非 0，
// 否则显示数值 + 单位（如 90% / 85C）——避免同屏出现「阈值 0%」与「越限:（无）」的自相矛盾。
func thresholdText(limit int, unit string) string {
	if limit <= 0 {
		return "未设置"
	}
	return fmt.Sprintf("%d%s", limit, unit)
}

func desiredInt(v int) string {
	if v < 0 {
		return "不托管"
	}
	return fmt.Sprintf("%d", v)
}

func desiredBool(v *bool) string {
	if v == nil {
		return "不托管"
	}
	return fmt.Sprintf("%v", *v)
}

// renderSoftwareResult 渲染最近一次升级/回退的结论（修 BUG-16）。
//
// 数据源是服务端落盘的 `/var/lib/nfvis/software/last-result.json`：`request system software
// add` 会重启 nfvisd、断开承载该命令的连接，操作者只看到 EOF；重连后读这条命令即可知道
// 真实结果。**没有任何记录时如实说「无记录」**，不编造「上次成功」。
func (x *cliExecutor) renderSoftwareResult() string {
	return x.renderSoftwareResultPath(ksys.LastResultPath)
}

// packageStateForResult 实时读宿主上 nfvis 包的状态与版本（供读口交叉核对）。
//
// 走可选接口（`packageStateReader`）：注入的 SoftwareRuntime 若支持就取，否则返回错误——
// 调用方据此如实说「无法核对」，不假装一致（修 BUG-17：一条过期记录不能冒充现状）。
func (x *cliExecutor) packageStateForResult() (string, string, error) {
	r, ok := x.sw.(packageStateReader)
	if !ok || r == nil {
		return "", "", fmt.Errorf("当前装配未提供包状态读取")
	}
	return r.PackageState(context.Background())
}

// renderSoftwareResultPath 按给定路径渲染（路径可注入以便单测覆盖三种形态：
// 成功记录 / 失败记录 / 无记录）。
func (x *cliExecutor) renderSoftwareResultPath(path string) string {
	res, ok := ksys.ReadLastResultAt(path)
	if !ok {
		// 不能一口咬定「没操作过」：升级到**不带本记录功能**的版本（或被旧版覆盖）时，
		// 结论文件同样不存在，而升级可能已经成功。如实说明两种可能并给可独立核对的事实源。
		return "（无升级/回退结论记录：可能从未经 request system software add|rollback 操作，\n" +
			"  也可能所装版本不写此记录（该记录功能自 3.0.26 起）——\n" +
			"  请用 `show version` 与宿主 `dpkg -l nfvis` 核对实际安装状态）\n"
	}
	var b strings.Builder
	action := "升级"
	if res.Action == "rollback" {
		action = "回退"
	}
	verdict := "失败"
	switch {
	case res.OK:
		verdict = "成功"
	case res.Status == resultStatusPendingText:
		// 升级进行中/判定未完成时落的是 pending：这是**诚实**的「未定」，不是失败。
		// 分开报，否则操作者会把「还没定」读成「装坏了」。
		verdict = "未定（升级进行中或判定未完成）"
	}
	fmt.Fprintf(&b, "结果: %s\n", verdict)
	fmt.Fprintf(&b, "动作: %s\n", action)
	if res.Version != "" {
		fmt.Fprintf(&b, "版本: %s\n", res.Version)
	}
	if res.Previous != "" {
		fmt.Fprintf(&b, "原版本: %s\n", res.Previous)
	}
	if res.Status != "" {
		fmt.Fprintf(&b, "状态: %s\n", res.Status)
	}
	// 判不出/失败的原因与 Status 分开（Status 只放观测事实）——否则失败时读口只说「失败」
	// 却不说为什么，操作者仍无从下手。
	if res.Reason != "" {
		fmt.Fprintf(&b, "原因: %s\n", res.Reason)
	}
	if res.Package != "" {
		fmt.Fprintf(&b, "包: %s\n", res.Package)
	}
	// 修 BUG-17（读口这一半）：落盘结论可能与**当前事实**不符（结论是历史快照，且旧版本写入的
	// 记录不会自愈——真机实证：结论说 installed、实际 half-configured，重启后照旧）。
	// 能实时读到 dpkg 状态就交叉核对，**以当前事实为准**，绝不让过期记录冒充现状；
	// 读不到就如实说「无法核对」，不假装一致。
	curStatus, curVer, curErr := x.packageStateForResult()
	effOK := res.OK
	if curErr == nil && curStatus != "" {
		effOK = curStatus == pkgStatusInstalledText
		switch {
		case res.OK && !effOK:
			fmt.Fprintf(&b, "\n⚠ 记录与当前状态不符：记录为「成功」，当前 dpkg 状态是 %q——"+
				"记录已过期或升级并未真正落定，**以当前状态为准**。\n"+
				"  处理：dpkg --configure nfvis 收口，或 request system software rollback 回退。\n", curStatus)
			fmt.Fprintf(&b, "结论（按当前状态）: 未落定为成功\n")
		case !res.OK && effOK:
			fmt.Fprintf(&b, "\n注：记录为「失败」，当前 dpkg 状态是 %q（已装好）——"+
				"可能事后被修复或再次升级，**以当前状态为准**。\n", curStatus)
			fmt.Fprintf(&b, "结论（按当前状态）: 成功\n")
		default:
			fmt.Fprintf(&b, "当前状态核对: %s（与记录一致）\n", curStatus)
		}
	} else {
		fmt.Fprintf(&b, "当前状态核对: 读取失败（%v）——无法与记录交叉核对，以上仅为历史记录\n", curErr)
	}
	st := map[string]any{
		"ok": effOK, "action": res.Action, "version": res.Version,
		"previous": res.Previous, "status": res.Status, "package": res.Package,
	}
	if curErr == nil && curStatus != "" {
		st["current_status"] = curStatus
		st["current_version"] = curVer
		st["matches_record"] = res.Status == curStatus
	}
	x.structured = st
	return b.String()
}

// pkgStatusInstalledText dpkg 里「已装且已配置」的唯一状态行。
//
// 在 api 层复写一份而不是引用 internal/system 的私有常量：那边的常量是包内实现细节
// （它的取值本身是 dpkg 的契约字符串，不是产品自定义的），跨包引用要把它导出、扩大面；
// 这里只需要同一串字面量做**展示层判等**，取值由 dpkg 决定、不会各自漂移。
const pkgStatusInstalledText = "install ok installed"

// resultStatusPendingText 结论文件里「升级进行中、结论未定」的标记（与 internal/system 的
// resultStatusPending 同值）。跨包引用要导出内部常量、扩大面，而这里是 dpkg 之外**本产品约定的
// 落盘取值**——故按同一字面量判等，改一处必须同时改另一处（两边都有用例覆盖 pending 语义）。
const resultStatusPendingText = "pending"
