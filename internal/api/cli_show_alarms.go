package api

// show alarms 的 CLI 渲染（运行态告警表；与 GET /alarms 同源）。

import (
	"fmt"
	"strings"
)

// execShowAlarms：show alarms [active|resolved|all]（缺省 active）。
func (x *cliExecutor) execShowAlarms(args []string) string {
	if x.alarms == nil {
		return errRuntimeUnavailable
	}
	state := "active"
	if len(args) > 0 {
		switch args[0] {
		case "active", "resolved", "all":
			state = args[0]
		default:
			return fmt.Sprintf("%% 无效命令: show alarms %s（可用：active|resolved|all）\n", args[0])
		}
	}
	rows := x.alarms.List(state)
	if len(rows) == 0 {
		return fmt.Sprintf("（无 %s 告警）\n", state)
	}
	items := make([]any, 0, len(rows))
	var b strings.Builder
	fmt.Fprintf(&b, "%-10s %-10s %-22s %-20s %s\n", "Severity", "Code", "Source", "Raised", "Message")
	for _, r := range rows {
		items = append(items, anyToTree(r))
		// NFR-006：记录该告警时宿主时钟**未与 NTP 同步**则带标记（时间可能不准）。
		// nil 表示**没有这个信息**（未知），不加标记、也不谎称已同步——与 show log audit 同款。
		mark := ""
		if r.TimeSynced != nil && !*r.TimeSynced {
			mark = "  [时钟未同步]"
		}
		fmt.Fprintf(&b, "%-10s %-10s %-22s %-20s %s%s\n", r.Severity, r.Code, r.Source,
			r.RaisedAt.Format("2006-01-02 15:04:05"), r.Message, mark)
	}
	x.structured = map[string]any{"alarms": items}
	return b.String()
}
