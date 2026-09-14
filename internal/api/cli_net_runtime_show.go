package api

// F7（reviews 2026-09-13 第四轮）：`show virtual-switches <n> mac-table`、
// `show vrfs <n> routes`、`show nat [sessions]`、`show protocols lldp neighbors`
// 此前返回「依赖底座运行态，M3/M4 接入后可用」占位提示，而后端运行态端点（M3-7）
// 早已实现——CLI 层未接线。本文件把这些子命令接到运行态查询上，与 API 同源。

import (
	"context"
	"fmt"
	"strings"
)

const errRuntimeUnavailable = "%% VPP 未接入（编排器未装配），运行态不可用\n"

// execShowVSwitches：列表 / <name> [detail|ports|mac-table]。
// detail|ports 取 committed 配置视图；mac-table 取 VPP 运行态。
func (x *cliExecutor) execShowVSwitches(args []string) string {
	if len(args) >= 2 && args[1] == "mac-table" {
		return x.showMacTable(args[0])
	}
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if len(args) == 0 {
		items := make([]any, 0, len(cfg.VirtualSwitches))
		for _, vs := range cfg.VirtualSwitches {
			items = append(items, anyToTree(vs))
		}
		if len(items) == 0 {
			return "（无虚拟交换机）\n"
		}
		tree := map[string]any{"virtual_switches": items}
		x.structured = tree
		return RenderConfigJSON(tree) + "\n"
	}
	name := args[0]
	for _, vs := range cfg.VirtualSwitches {
		if vs.Name != name {
			continue
		}
		m, _ := anyToTree(vs).(map[string]any)
		// 运行态补充：L2 交换机的 MAC 表条数
		if vs.Type == "l2" && x.l2 != nil {
			if rows, err := x.l2.MACTable(context.Background(), name); err == nil {
				m["mac_table_entries"] = len(rows)
			}
		}
		x.structured = m
		return RenderConfigJSON(m) + "\n"
	}
	return fmt.Sprintf("%% 虚拟交换机 %s 不存在\n", name)
}

// showMacTable 渲染 MAC 学习表（VPP l2fib）。
func (x *cliExecutor) showMacTable(name string) string {
	if x.l2 == nil {
		return errRuntimeUnavailable
	}
	rows, err := x.l2.MACTable(context.Background(), name)
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if len(rows) == 0 {
		return "（MAC 表为空）\n"
	}
	items := make([]any, 0, len(rows))
	var b strings.Builder
	fmt.Fprintf(&b, "%-20s %-10s %s\n", "MAC", "VLAN", "Port")
	for _, r := range rows {
		items = append(items, anyToTree(r))
		fmt.Fprintf(&b, "%-20s %-10d %s\n", r.MAC, r.VLAN, r.Port)
	}
	x.structured = map[string]any{"mac_table": items}
	return b.String()
}

// execShowVrfs：列表 / <name> [detail|routes]。routes 取 VPP FIB 运行态。
func (x *cliExecutor) execShowVrfs(args []string) string {
	if len(args) >= 2 && args[1] == "routes" {
		return x.showVrfRoutes(args[0])
	}
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if len(args) == 0 {
		items := make([]any, 0, len(cfg.Vrfs))
		for _, v := range cfg.Vrfs {
			items = append(items, anyToTree(v))
		}
		if len(items) == 0 {
			return "（无 VRF）\n"
		}
		tree := map[string]any{"vrfs": items}
		x.structured = tree
		return RenderConfigJSON(tree) + "\n"
	}
	name := args[0]
	for _, v := range cfg.Vrfs {
		if v.Name != name {
			continue
		}
		m, _ := anyToTree(v).(map[string]any)
		if x.l3 != nil {
			if rows, err := x.l3.Routes(context.Background(), name); err == nil {
				rt := make([]any, 0, len(rows))
				for _, r := range rows {
					rt = append(rt, anyToTree(r))
				}
				m["fib_routes"] = rt
			}
		}
		x.structured = m
		return RenderConfigJSON(m) + "\n"
	}
	return fmt.Sprintf("%% VRF %s 不存在\n", name)
}

// showVrfRoutes 渲染 VRF 的 FIB 路由表（运行态）。
func (x *cliExecutor) showVrfRoutes(name string) string {
	if x.l3 == nil {
		return errRuntimeUnavailable
	}
	// 必须先校验 VRF 存在：VPP 对不存在的 VRF 返回空表，直接渲染会把「VRF 不存在」
	// 显示成「（FIB 无路由）」，与 `show vrfs <name>` 的报错口径不一致（决策 #76）。
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	found := false
	for _, v := range cfg.Vrfs {
		if v.Name == name {
			found = true
			break
		}
	}
	if !found {
		return fmt.Sprintf("%% VRF %s 不存在\n", name)
	}
	rows, err := x.l3.Routes(context.Background(), name)
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if len(rows) == 0 {
		return "（FIB 无路由）\n"
	}
	items := make([]any, 0, len(rows))
	var b strings.Builder
	fmt.Fprintf(&b, "%-20s %-20s %s\n", "Prefix", "NextHop", "Distance")
	for _, r := range rows {
		items = append(items, anyToTree(r))
		fmt.Fprintf(&b, "%-20s %-20s %d\n", r.Prefix, r.NextHop, r.Distance)
	}
	x.structured = map[string]any{"routes": items}
	return b.String()
}

// execShowNat：show nat [sessions]（运行态，VPP nat44 会话表）。
func (x *cliExecutor) execShowNat(args []string) string {
	if len(args) > 0 && args[0] != "sessions" {
		return fmt.Sprintf("%% 无效命令: show nat %s（可用：show nat [sessions]）\n", strings.Join(args, " "))
	}
	if x.natRT == nil {
		if head := x.natConfigLines(); head != "" {
			return head + "（运行态未接入：会话不可用）\n"
		}
		return errRuntimeUnavailable
	}
	rows, err := x.natRT.Sessions(context.Background())
	if err != nil {
		return x.natConfigLines() + "%% " + err.Error() + "\n"
	}
	if len(rows) == 0 {
		if head := x.natConfigLines(); head != "" {
			return head + "（无 NAT 会话）\n"
		}
		return "（无 NAT 配置与会话）\n"
	}
	items := make([]any, 0, len(rows))
	var b strings.Builder
	b.WriteString(x.natConfigLines())
	fmt.Fprintf(&b, "%-18s %-8s %-18s %-8s %-8s %s\n", "Inside", "Port", "Outside", "Port", "Proto", "Packets")
	for _, r := range rows {
		items = append(items, anyToTree(r))
		fmt.Fprintf(&b, "%-18s %-8d %-18s %-8d %-8d %d\n",
			r.InsideIP, r.InsidePort, r.OutsideIP, r.OutsidePort, r.Protocol, r.Packets)
	}
	x.structured = map[string]any{"nat_sessions": items}
	return b.String()
}

// natConfigLines 渲染 NAT 配置（池/规则/静态映射，来自 committed 配置，FR-NET-016）。
func (x *cliExecutor) natConfigLines() string {
	if x.engine == nil {
		return ""
	}
	cfg, err := x.engine.Committed()
	if err != nil || cfg.Nat == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range cfg.Nat.SourcePools {
		fmt.Fprintf(&b, "source-pool %s address-range %s;\n", p.Name, p.AddressRange)
	}
	for _, r := range cfg.Nat.Rules {
		action := "interface " + r.Action.Interface
		if r.Action.SourcePool != "" {
			action = "source-pool " + r.Action.SourcePool + " " + action
		}
		fmt.Fprintf(&b, "rules %d match source %s virtual-switch %s action %s;\n",
			r.Seq, r.MatchSource, r.VirtualSwitch, action)
	}
	for _, st := range cfg.Nat.Static {
		fmt.Fprintf(&b, "static %s to %s;\n", st.InsideIP, st.OutsideIP)
	}
	return b.String()
}

// execShowProtocols：show protocols lldp neighbors（运行态；FR-NET-018）。
func (x *cliExecutor) execShowProtocols(args []string) string {
	if len(args) >= 2 && args[0] == "lldp" && args[1] == "neighbors" {
		if x.lldp == nil {
			return errRuntimeUnavailable
		}
		rows, err := x.lldp.Neighbors(context.Background())
		if err != nil {
			return "%% " + err.Error() + "\n"
		}
		if len(rows) == 0 {
			return "（无 LLDP 邻居）\n"
		}
		items := make([]any, 0, len(rows))
		var b strings.Builder
		fmt.Fprintf(&b, "%-12s %-20s %-16s %s\n", "Interface", "Chassis", "Port", "TTL")
		for _, r := range rows {
			items = append(items, anyToTree(r))
			fmt.Fprintf(&b, "%-12s %-20s %-16s %d\n", r.Interface, r.ChassisID, r.PortID, r.TTL)
		}
		x.structured = map[string]any{"neighbors": items}
		return b.String()
	}
	return fmt.Sprintf("%% 无效命令: show protocols %s（可用：show protocols lldp neighbors）\n", strings.Join(args, " "))
}
