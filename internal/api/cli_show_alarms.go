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
		fmt.Fprintf(&b, "%-10s %-10s %-22s %-20s %s\n", r.Severity, r.Code, r.Source,
			r.RaisedAt.Format("2006-01-02 15:04:05"), r.Message)
	}
	x.structured = map[string]any{"alarms": items}
	return b.String()
}
