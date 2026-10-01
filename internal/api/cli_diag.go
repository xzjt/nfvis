package api

// M3-9：CLI 操作命令的守护进程侧执行（ping/traceroute/monitor/clear）。
// 底座能力经 DiagRuntime 注入（network.Diagnostics 实现），本层不 import orchestrator。
// 命令树与执行器同源（schema，gen_test 守护，AGENTS.md 常见错误第 1 条）。

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xzjt/nfvis/internal/schema"
)

// DiagRuntime 诊断操作能力（编排器装配注入；nil = 命令报不可用）。
// ipv6（决策 #330）：ping 显式走 v6 平面（vppctl ping ipv6），traceroute 走宿主侧 ICMPv6。
type DiagRuntime interface {
	Ping(ctx context.Context, host, source, vrf string, count int, ipv6 bool) (string, error)
	Traceroute(ctx context.Context, host, vrf string, ipv6 bool) (string, error)
	ClearInterfaceStats(ctx context.Context, ifname string) error
}

// execPing：`ping [ipv6] <host> [source <ip>] [count <n>] [vrf <name>]`（决策 #330）。
// `ipv6` 是无值选择器（与 VPP CLI `ping ipv6 <addr>` 同形；命令树里也可写在目标之后，
// 故解析器按「单 token 扫描」容忍两种顺序，与命令树保持同源）；不给则照旧由 vppctl
// 按地址字面判族（IPv6 字面也能走通），既有 IPv4 写法一字不变。
func (x *cliExecutor) execPing(class string, t []string) string {
	const usage = "%% 语法: ping [ipv6] <host> [source <ip>] [count <n>] [vrf <name>]\n"
	if !x.allow(class, mustNode(schema.OperRoot(), "ping"), "ping") {
		return "%% 无权限执行 ping\n"
	}
	if x.diag == nil {
		return "%% ping 不可用（VPP 未接入）\n"
	}
	var host, source, vrf string
	count := 0
	ipv6 := false
	for i := 0; i < len(t); {
		switch t[i] {
		case "ipv6": // 无值选择器
			ipv6 = true
			i++
		case "source":
			if i+1 >= len(t) {
				return usage
			}
			source, i = t[i+1], i+2
		case "count":
			if i+1 >= len(t) {
				return usage
			}
			n, err := strconv.Atoi(t[i+1])
			if err != nil || n <= 0 {
				return "%% count 必须为正整数\n"
			}
			count, i = n, i+2
		case "vrf":
			if i+1 >= len(t) {
				return usage
			}
			vrf, i = t[i+1], i+2
		default:
			if host != "" {
				return usage // 只允许一个目标
			}
			host, i = t[i], i+1
		}
	}
	if host == "" {
		return usage
	}
	out, err := x.diag.Ping(context.Background(), host, source, vrf, count, ipv6)
	if err != nil {
		return appendErr(out, err)
	}
	return out
}

// execTraceroute：`traceroute [ipv6] <host> [vrf <name>]`（决策 #330）。
// vrf 对 v4/v6 都明确报不支持（宿主侧 ICMP 无法经 VPP VRF 转发）；不静默降级成 v4。
func (x *cliExecutor) execTraceroute(class string, t []string) string {
	const usage = "%% 语法: traceroute [ipv6] <host> [vrf <name>]\n"
	if !x.allow(class, mustNode(schema.OperRoot(), "traceroute"), "traceroute") {
		return "%% 无权限执行 traceroute\n"
	}
	if x.diag == nil {
		return "%% traceroute 不可用（诊断未接入）\n"
	}
	var host, vrf string
	ipv6 := false
	for i := 0; i < len(t); {
		switch t[i] {
		case "ipv6":
			ipv6 = true
			i++
		case "vrf":
			if i+1 >= len(t) {
				return usage
			}
			vrf, i = t[i+1], i+2
		default:
			if host != "" {
				return usage
			}
			host, i = t[i], i+1
		}
	}
	if host == "" {
		return usage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	out, err := x.diag.Traceroute(ctx, host, vrf, ipv6)
	if err != nil {
		return appendErr(out, err)
	}
	return out
}

// execMonitor 返回单次接口计数快照；实时刷新由 nfvis-cli REPL 轮询（附录 A #36）。
func (x *cliExecutor) execMonitor(class string, t []string) string {
	if !x.allow(class, mustNode(schema.OperRoot(), "monitor"), "monitor") {
		return "%% 无权限执行 monitor\n"
	}
	if len(t) >= 2 && t[0] == "vnf" {
		return x.monitorVNF(t[1:])
	}
	if len(t) < 2 || t[0] != "interfaces" {
		return "%% 语法: monitor interfaces <ifname> [interval <sec>] | monitor vnf <name>\n"
	}
	ifname := t[1]
	switch {
	case len(t) == 2:
	case len(t) == 4 && t[2] == "interval":
		if n, err := strconv.Atoi(t[3]); err != nil || n <= 0 {
			return "%% interval 必须为正整数秒\n"
		}
	default:
		return "%% 语法: monitor interfaces <ifname> [interval <sec>]\n"
	}
	if x.state == nil {
		return "%% 接口统计不可用（运行态未接入）\n"
	}
	c, ok := x.state.InterfaceCounters(context.Background(), ifname)
	if !ok {
		// 如实描述（与 `show interfaces <n> statistics` 同口径）：stats 是接入了的，
		// 取不到数是**这一刻连接没就绪/读取失败**（VPP 重启后连接陈旧即属此列，
		// 取数路径会自行重连重试）；另一种成因是接口名本身不在数据面。
		return fmt.Sprintf("%% 接口 %s 统计暂不可用（不存在或 stats 连接未就绪）\n", ifname)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-12s %12s %12s %14s %14s %8s %8s\n",
		"Interface", "rx-pkts", "tx-pkts", "rx-bytes", "tx-bytes", "rx-drops", "tx-drops")
	fmt.Fprintf(&b, "%-12s %12d %12d %14d %14d %8d %8d\n",
		ifname, c.RxPackets, c.TxPackets, c.RxBytes, c.TxBytes, c.RxDrops, c.TxDrops)
	return b.String()
}

func (x *cliExecutor) execClear(class string, t []string) string {
	if !x.allow(class, mustNode(schema.OperRoot(), "clear"), "clear") {
		return "%% 无权限执行 clear（需 super-user）\n"
	}
	// 命令树仅 clear interfaces statistics [<ifname>]
	if len(t) < 2 || t[0] != "interfaces" || t[1] != "statistics" {
		return "%% 语法: clear interfaces statistics [<ifname>]\n"
	}
	if len(t) > 3 {
		return "%% 语法: clear interfaces statistics [<ifname>]\n"
	}
	if x.diag == nil {
		return "%% clear 不可用（VPP 未接入）\n"
	}
	ifname := ""
	if len(t) == 3 {
		ifname = t[2]
	}
	if err := x.diag.ClearInterfaceStats(context.Background(), ifname); err != nil {
		return "%% " + err.Error() + "\n"
	}
	if ifname == "" {
		return "已清零全部接口统计\n"
	}
	return fmt.Sprintf("已清零接口 %s 统计\n", ifname)
}

// appendErr 保留命令已有输出（如 ping 部分结果），追加错误行。
func appendErr(out string, err error) string {
	msg := "%% " + err.Error() + "\n"
	if out == "" {
		return msg
	}
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out + msg
}

// monitorVNF：`monitor vnf <name>`（契约 §1.3）——返回单次 VNF 状态 + 相关告警快照，
// 实时刷新由 nfvis-cli REPL 轮询（与 monitor interfaces 同法，附录 A #36）。
func (x *cliExecutor) monitorVNF(args []string) string {
	name := args[0]
	var lines []string
	if x.vm != nil {
		if st, err := x.vm.VMState(context.Background(), name); err == nil && st != "" {
			lines = append(lines, fmt.Sprintf("vnf %s: state=%s", name, st))
		} else if err != nil {
			lines = append(lines, fmt.Sprintf("vnf %s: 状态不可用（%v）", name, err))
		}
	} else {
		lines = append(lines, fmt.Sprintf("vnf %s: 计算运行态未接入", name))
	}
	if x.alarms != nil {
		rows := x.alarms.List("active")
		n := 0
		for _, r := range rows {
			if strings.Contains(r.Source, name) {
				lines = append(lines, fmt.Sprintf("  告警 %-8s %-14s %s", r.Severity, r.Code, r.Message))
				n++
			}
		}
		if n == 0 {
			lines = append(lines, "  活动告警: 无")
		}
	}
	return strings.Join(lines, "\n") + "\n"
}
