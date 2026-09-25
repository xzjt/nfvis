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

// execShowVSwitches：列表 / <name> [detail|ports|statistics|mac-table]。
//
// 契约 §1.1 的语义（决策 #84）：列表 = 「全部虚拟交换机摘要」、`ports` = 「成员端口**及状态/计数**」、
// `statistics` = 「每端口收发计数」——**都取 VPP 运行态**（BD 的 BD-Tag 即交换机名）。
// 此前列表/ports/statistics 取自 committed 配置（后两者甚至回落到同一份配置 dump），
// 于是「VPP 里存在但未写入配置」的 BD 不显示、也没有任何状态/计数。
func (x *cliExecutor) execShowVSwitches(args []string) string {
	if len(args) >= 2 && args[1] == "mac-table" {
		return x.showMacTable(args[0])
	}
	bds, err := x.bdStates()
	if err != nil {
		// 运行态不可用：明确说明，**不**退回配置视图（那正是缺陷来源）
		return fmt.Sprintf("%% 虚拟交换机运行态不可用: %v\n", err)
	}
	if len(args) == 0 {
		if len(bds) == 0 {
			return "（VPP 中无 bridge-domain）\n"
		}
		cfgNames := x.vswitchConfigNames()
		var b strings.Builder
		fmt.Fprintf(&b, "%-10s %-22s %-6s %-6s %-6s %s\n", "BD-ID", "Name", "Learn", "Flood", "Ports", "Note")
		items := make([]any, 0, len(bds))
		for _, bd := range bds {
			note := ""
			if !cfgNames[bd.Name] {
				note = "未在配置中（运行态存在）"
			}
			name := bd.Name
			if name == "" {
				name = "-"
			}
			fmt.Fprintf(&b, "%-10d %-22s %-6s %-6s %-6d %s\n", bd.ID, name,
				yn(bd.Learn), yn(bd.Flood), len(bd.Ports), note)
			items = append(items, bdView(bd))
		}
		x.structured = map[string]any{"virtual_switches": items}
		return b.String()
	}
	name := args[0]
	var bd *BridgeDomainState
	for i := range bds {
		if bds[i].Name == name {
			bd = &bds[i]
			break
		}
	}
	if bd == nil {
		return fmt.Sprintf("%% 虚拟交换机 %s 在 VPP 中不存在（show virtual-switches 看运行态列表）\n", name)
	}
	sub := ""
	if len(args) >= 2 {
		sub = args[1]
	}
	switch sub {
	case "ports", "statistics":
		// 契约：成员端口**及状态/计数**（statistics 侧重每端口收发计数）
		var b strings.Builder
		fmt.Fprintf(&b, "%-16s %-7s %-7s %-12s %-12s %s\n", "Port", "Admin", "Link", "RxPkts", "TxPkts", "Shg")
		items := make([]any, 0, len(bd.Ports))
		states, _ := x.ifaceStates()
		for _, p := range bd.Ports {
			row := map[string]any{"port": p.Name, "sw_if_index": p.SwIfIndex, "shg": p.Shg}
			admin, link, rx, tx := "-", "-", "-", "-"
			if st, ok := states[p.Name]; ok {
				admin, link = yn(st.AdminUp), yn(st.LinkUp)
				row["admin"], row["link"] = st.AdminUp, st.LinkUp
			}
			if x.state != nil {
				if c, ok := x.state.InterfaceCounters(context.Background(), p.Name); ok {
					rx, tx = fmt.Sprintf("%d", c.RxPackets), fmt.Sprintf("%d", c.TxPackets)
					row["rx_packets"], row["tx_packets"] = c.RxPackets, c.TxPackets
				}
			}
			fmt.Fprintf(&b, "%-16s %-7s %-7s %-12s %-12s %d\n", p.Name, admin, link, rx, tx, p.Shg)
			items = append(items, row)
		}
		if len(bd.Ports) == 0 {
			b.WriteString("（该 BD 无成员口）\n")
		}
		x.structured = map[string]any{"bd_id": bd.ID, "name": bd.Name, "ports": items}
		return b.String()
	default:
		// detail 及不带子命令：运行态（状态 + 成员口）叠加配置的类型信息
		m := bdView(*bd)
		if cfg, err := x.engine.Committed(); err == nil {
			for _, vs := range cfg.VirtualSwitches {
				if vs.Name == name {
					m["configured_type"] = vs.Type
				}
			}
		}
		if x.l2 != nil {
			if rows, err := x.l2.MACTable(context.Background(), name); err == nil {
				m["mac_table_entries"] = len(rows)
			}
		}
		x.structured = m
		return RenderConfigJSON(m) + "\n"
	}
}

// bdView BD 运行态的对外形态（structured 快照与 detail 渲染共用）。
func bdView(bd BridgeDomainState) map[string]any {
	ports := make([]any, 0, len(bd.Ports))
	for _, p := range bd.Ports {
		ports = append(ports, map[string]any{"port": p.Name, "sw_if_index": p.SwIfIndex, "shg": p.Shg})
	}
	return map[string]any{
		"bd_id": bd.ID, "name": bd.Name,
		"learn": bd.Learn, "flood": bd.Flood, "uu_flood": bd.UuFlood,
		"forward": bd.Forward, "arp_term": bd.ArpTerm, "mac_age": bd.MacAge,
		"ports": ports,
	}
}

// vswitchConfigNames committed 配置里的交换机名集合（用于标注「未在配置中」）。
func (x *cliExecutor) vswitchConfigNames() map[string]bool {
	out := map[string]bool{}
	if cfg, err := x.engine.Committed(); err == nil {
		for _, vs := range cfg.VirtualSwitches {
			out[vs.Name] = true
		}
	}
	return out
}

// yn 布尔 → up/down（运行态列）。
func yn(b bool) string {
	if b {
		return "up"
	}
	return "down"
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
//
// 与 `show lldp neighbors [interface <ifname>]` 是**同一读物的等价写法**（契约 §1.1）：
// 渲染走唯一实现 `showLLDPNeighbors`，不在这里复制一份。过滤参数只声明在
// `show lldp neighbors` 一侧（树里 `show protocols lldp neighbors` 没有 `interface` 子节点），
// 故这里的多余 token 必须报错并指向等价写法——此前同样被静默丢掉、回全量邻居表。
func (x *cliExecutor) execShowProtocols(args []string) string {
	if len(args) >= 2 && args[0] == "lldp" && args[1] == "neighbors" {
		if len(args) != 2 {
			return fmt.Sprintf("%% 无效命令: show protocols %s（可用：show protocols lldp neighbors；"+
				"按接口过滤：show lldp neighbors interface <ifname>）\n", strings.Join(args, " "))
		}
		return x.showLLDPNeighbors("")
	}
	return fmt.Sprintf("%% 无效命令: show protocols %s（可用：show protocols lldp neighbors）\n", strings.Join(args, " "))
}
