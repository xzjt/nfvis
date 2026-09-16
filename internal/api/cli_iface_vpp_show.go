package api

// M5-9 收尾：补齐契约 §1.1 中此前无分发分支的命令族
//   show interfaces [physical|management|<ifname> [detail|statistics|sriov]]
//   show port-mirroring / show qos policies / show vpp [threads|buffers|memory]
//   show lldp neighbors（契约写法；等价于 show protocols lldp neighbors）
// 运行态来源：state（线程/buffer/内存/接口计数）与 committed 配置；未接入时给明确提示。

import (
	"context"
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/state"
)

// execShowInterfaces：契约 §1.1 的接口族。
func (x *cliExecutor) execShowInterfaces(args []string) string {
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	// show interfaces physical|management —— 列表
	if len(args) >= 1 && (args[0] == "physical" || args[0] == "management") {
		if args[0] == "management" {
			return x.showManagementInterface(cfg)
		}
		if len(args) == 1 {
			return x.showPhysicalInterfaces(cfg, "")
		}
		return x.showPhysicalInterfaces(cfg, args[1])
	}
	// show interfaces <ifname> [detail|statistics|sriov]
	if len(args) >= 1 {
		name := args[0]
		sub := ""
		if len(args) >= 2 {
			sub = args[1]
		}
		return x.showOneInterface(cfg, name, sub)
	}
	// show interfaces（摘要）
	if len(cfg.Interfaces) == 0 {
		return "（无已配置接口）\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-14s %-8s %-8s %-10s %s\n", "Interface", "Admin", "MTU", "Policy", "Description")
	items := make([]any, 0, len(cfg.Interfaces))
	for _, ifc := range cfg.Interfaces {
		items = append(items, anyToTree(ifc))
		admin := "up"
		if ifc.Enabled != nil && !*ifc.Enabled {
			admin = "down"
		}
		fmt.Fprintf(&b, "%-14s %-8s %-8d %-10s %s\n", ifc.Name, admin, ifc.MTU, ifc.IngressPolicy, ifc.Description)
	}
	x.structured = map[string]any{"interfaces": items}
	return b.String()
}

// showPhysicalInterfaces：物理口（配置 + **运行态**链接状态/速率/驱动/计数）。
//
// 契约 §1.1 要求「驱动、链接状态、速率、VF 数」（决策 #84）。此前表头只有
// Interface/Admin/RxPkts/TxPkts/Description，且 Admin 取自**配置的 enabled**——
// 实测把接口在 VPP 里置 down 后 CLI 仍显示 up。现在 Admin/Link/Speed/Driver 均取 VPP 运行态。
func (x *cliExecutor) showPhysicalInterfaces(cfg model.Config, only string) string {
	found := false
	var b strings.Builder
	items := make([]any, 0)
	states, stErr := x.ifaceStates()
	fmt.Fprintf(&b, "%-14s %-7s %-7s %-10s %-12s %-10s %-12s %s\n",
		"Interface", "Admin", "Link", "Speed", "Driver", "RxPkts", "TxPkts", "Description")
	for _, ifc := range cfg.Interfaces {
		if only != "" && ifc.Name != only {
			continue
		}
		found = true
		entry := map[string]any{"name": ifc.Name, "description": ifc.Description}
		admin, link, speed, driver := "-", "-", "-", "-"
		if st, ok := states[ifc.Name]; ok {
			admin, link = yn(st.AdminUp), yn(st.LinkUp)
			speed, driver = fmtSpeed(st.LinkSpeed), orDash(st.DevType)
			entry["admin_up"], entry["link_up"] = st.AdminUp, st.LinkUp
			entry["link_speed_kbps"], entry["driver"] = st.LinkSpeed, st.DevType
		}
		rx, tx := "-", "-"
		if x.state != nil {
			if c, ok := x.state.InterfaceCounters(context.Background(), ifc.Name); ok {
				rx, tx = fmt.Sprintf("%d", c.RxPackets), fmt.Sprintf("%d", c.TxPackets)
				entry["statistics"] = anyToTree(c)
			}
		}
		items = append(items, entry)
		fmt.Fprintf(&b, "%-14s %-7s %-7s %-10s %-12s %-10s %-12s %s\n",
			ifc.Name, admin, link, speed, driver, rx, tx, ifc.Description)
	}
	if !found {
		if only != "" {
			return fmt.Sprintf("%% 物理口 %s 未在配置中声明（先 set interfaces %s …）\n", only, only)
		}
		hint := x.physicalEmptyHint()
		if stErr != nil && len(cfg.Interfaces) == 0 {
			return hint
		}
		return hint
	}
	if stErr != nil {
		// 有配置项但运行态不可用：明确说明状态列为何是 "-"
		b.WriteString("%% 注: VPP 运行态不可用（" + stErr.Error() + "），Admin/Link/Speed/Driver 显示为 -\n")
	}
	x.structured = map[string]any{"interfaces": items}
	return b.String()
}

// fmtSpeed 链路速率：kbps → 人类可读；0 表示 VPP 未上报（DPDK 口常见）。
func fmtSpeed(kbps uint32) string {
	switch {
	case kbps == 0:
		return "-"
	case kbps >= 1_000_000:
		return fmt.Sprintf("%dG", kbps/1_000_000)
	case kbps >= 1000:
		return fmt.Sprintf("%dM", kbps/1000)
	default:
		return fmt.Sprintf("%dK", kbps)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// physicalEmptyHint 空态提示：列出**运行态**端口（决策 #83）。
// 此前只给一句「先 set interfaces … 声明」，用户其实无从知道该写哪个名字——
// 已由 DPDK 接管的口在内核中不存在，而候选当时又只列「已配置」的名字。
func (x *cliExecutor) physicalEmptyHint() string {
	var b strings.Builder
	b.WriteString("（无已声明物理口；先 set interfaces <ifname> description … 声明）\n")
	if x.ports == nil {
		return b.String()
	}
	names, err := x.ports.VPPIfnames()
	if err != nil {
		fmt.Fprintf(&b, "%% 端口清单不可用（VPP 未接入或查询失败）：%v\n", err)
		return b.String()
	}
	if len(names) == 0 {
		b.WriteString("（VPP 中暂无接口；网卡可能尚未由 DPDK 接管 —— 未接管的内核网卡见 " +
			"`request interfaces <n> bind-dpdk`）\n")
		return b.String()
	}
	b.WriteString("VPP 中的接口（已被 DPDK 接管，可直接 set interfaces <ifname> … 声明）：\n")
	for _, n := range names {
		fmt.Fprintf(&b, "  %s\n", n)
	}
	return b.String()
}

// showManagementInterface：管理口（内核侧，来自 system.management 配置）。
func (x *cliExecutor) showManagementInterface(cfg model.Config) string {
	sys := cfg.System
	if sys == nil || sys.Management == nil {
		return "（未配置管理口：set system management interface <ifname> / ip address <prefix>）\n"
	}
	m := sys.Management
	x.structured = anyToTree(m)
	var b strings.Builder
	fmt.Fprintf(&b, "%-12s %-22s %-18s %s\n", "Interface", "Address", "Gateway", "Plane")
	// 管理网卡名取自配置（决策 #71）；此前硬编码显示 "mgmt0"，而系统中并无该接口。
	name := m.Interface
	if name == "" {
		name = "(未指定)"
	}
	fmt.Fprintf(&b, "%-12s %-22s %-18s %s\n", name, m.Address, m.Gateway, "management(kernel)")
	if m.Interface == "" {
		fmt.Fprintln(&b, "%% 提示：未指定管理网卡（set system management interface <ifname>）；"+
			"指定后 commit 强制校验该网卡不得被数据面引用")
	}
	return b.String()
}

// showOneInterface：单个接口（detail|statistics|sriov）。
func (x *cliExecutor) showOneInterface(cfg model.Config, name, sub string) string {
	for _, ifc := range cfg.Interfaces {
		if ifc.Name != name {
			continue
		}
		m, _ := anyToTree(ifc).(map[string]any)
		if x.state != nil {
			if c, ok := x.state.InterfaceCounters(context.Background(), name); ok {
				m["statistics"] = anyToTree(c)
			}
		}
		switch sub {
		case "":
			x.structured = m
			return RenderConfigJSON(m) + "\n"
		case "detail", "statistics":
			if sub == "statistics" {
				if st, ok := m["statistics"]; ok {
					x.structured = map[string]any{"interface": name, "statistics": st}
					return fmt.Sprintf("interface %s statistics: %v\n", name, st)
				}
				return fmt.Sprintf("%% 接口 %s 统计运行态不可用（stats 未接入）\n", name)
			}
			x.structured = m
			return RenderConfigJSON(m) + "\n"
		case "sriov":
			if ifc.Sriov == nil {
				return fmt.Sprintf("（接口 %s 未配置 SR-IOV VF）\n", name)
			}
			mm, _ := anyToTree(ifc.Sriov).(map[string]any)
			x.structured = map[string]any{"interface": name, "sriov": mm}
			return RenderConfigJSON(x.structured.(map[string]any)) + "\n"
		default:
			return fmt.Sprintf("%% 无效命令: show interfaces %s %s（可用：detail|statistics|sriov）\n", name, sub)
		}
	}
	return fmt.Sprintf("%% 接口 %s 未在配置中声明\n", name)
}

// execShowGenericConfig：show port-mirroring / show qos policies（配置视图）。
func (x *cliExecutor) execShowPortMirroring(args []string) string {
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if len(cfg.PortMirroring) == 0 {
		return "（无端口镜像会话）\n"
	}
	items := make([]any, 0, len(cfg.PortMirroring))
	for _, pm := range cfg.PortMirroring {
		items = append(items, anyToTree(pm))
	}
	tree := map[string]any{"port_mirroring": items}
	x.structured = tree
	return RenderConfigJSON(tree) + "\n"
}

func (x *cliExecutor) execShowQos(args []string) string {
	if len(args) >= 1 && args[0] != "policies" {
		return fmt.Sprintf("%% 无效命令: show qos %s（可用：show qos policies）\n", strings.Join(args, " "))
	}
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if len(cfg.QosPolicies) == 0 {
		return "（无 QoS 限速策略）\n"
	}
	items := make([]any, 0, len(cfg.QosPolicies))
	var b strings.Builder
	fmt.Fprintf(&b, "%-14s %-12s %s\n", "Policy", "CIR(bps)", "CBS(bytes)")
	for _, p := range cfg.QosPolicies {
		items = append(items, anyToTree(p))
		fmt.Fprintf(&b, "%-14s %-12d %d\n", p.Name, p.Cir, p.Cbs)
	}
	x.structured = map[string]any{"qos_policies": items}
	return b.String()
}

// execShowVpp：show vpp [threads|buffers|memory]（运行态来自 state）。
func (x *cliExecutor) execShowVpp(args []string) string {
	sub := ""
	if len(args) >= 1 {
		sub = args[0]
	}
	// capture 走抓包运行态（M5-3），不依赖 state
	if sub == "capture" {
		return x.execShowVppCapture()
	}
	if x.state == nil {
		return errRuntimeUnavailable
	}
	ctx := context.Background()
	switch sub {
	case "", "threads":
		threads := x.state.Threads(ctx)
		if sub == "threads" {
			if len(threads) == 0 {
				return "（无线程运行态）\n"
			}
			items := make([]any, 0, len(threads))
			var b strings.Builder
			fmt.Fprintf(&b, "%-10s %-12s %-8s %s\n", "Name", "Type", "Core", "ID")
			for _, th := range threads {
				items = append(items, anyToTree(th))
				fmt.Fprintf(&b, "%-10s %-12s %-8d %d\n", th.Name, th.Type, th.Core, th.ID)
			}
			x.structured = map[string]any{"threads": items}
			return b.String()
		}
		// show vpp：概览（线程数 + buffer + 内存）
		buf, hasBuf := x.state.Buffers(ctx)
		mem, hasMem := x.state.Memory(ctx)
		out := map[string]any{"threads": len(threads)}
		if hasBuf {
			out["buffers"] = anyToTree(buf)
		}
		if hasMem {
			out["memory"] = anyToTree(mem)
		}
		x.structured = out
		var b strings.Builder
		fmt.Fprintf(&b, "threads: %d\n", len(threads))
		if hasBuf {
			fmt.Fprintf(&b, "buffers: pools=%d source=%s\n", len(buf.Pools), buf.Source)
			for _, pl := range buf.Pools {
				fmt.Fprintf(&b, "  %-10s used=%.0f available=%.0f cached=%.0f\n", pl.Name, pl.Used, pl.Available, pl.Cached)
			}
		} else {
			fmt.Fprintf(&b, "buffers: 运行态不可用（%s）\n", bufUnavailableReason(buf))
		}
		if hasMem {
			fmt.Fprintf(&b, "memory: total=%d used=%d free=%d\n", mem.Total, mem.Used, mem.Free)
		} else {
			fmt.Fprintln(&b, "memory: 运行态不可用")
		}
		return b.String()
	case "buffers":
		buf, ok := x.state.Buffers(ctx)
		if !ok {
			return fmt.Sprintf("%% buffer 池运行态不可用：%s\n", bufUnavailableReason(buf))
		}
		x.structured = anyToTree(buf)
		var b strings.Builder
		fmt.Fprintf(&b, "source: %s\n", buf.Source)
		for _, pl := range buf.Pools {
			fmt.Fprintf(&b, "%-10s used=%.0f available=%.0f cached=%.0f\n", pl.Name, pl.Used, pl.Available, pl.Cached)
		}
		return b.String()
	case "memory":
		mem, ok := x.state.Memory(ctx)
		if !ok {
			return "%% 内存运行态不可用\n"
		}
		x.structured = anyToTree(mem)
		return fmt.Sprintf("total=%d used=%d free=%d\n", mem.Total, mem.Used, mem.Free)
	case "runtime":
		// 契约 §1.1：每线程指令周期/向量率；govpp runtime 未接入时为明确提示
		return "%% VPP runtime 统计未接入（govpp runtime 解码限制）\n"
	case "capture":
		return x.execShowVppCapture()
	}
	return fmt.Sprintf("%% 无效命令: show vpp %s（可用：threads|buffers|memory|runtime|capture）\n", sub)
}

// execShowLldp：契约 §1.1 写法 `show lldp neighbors`（等价 `show protocols lldp neighbors`）。
func (x *cliExecutor) execShowLldp(args []string) string {
	if len(args) >= 1 && args[0] != "neighbors" {
		return fmt.Sprintf("%% 无效命令: show lldp %s（可用：show lldp neighbors）\n", strings.Join(args, " "))
	}
	return x.execShowProtocols([]string{"lldp", "neighbors"})
}

// bufUnavailableReason buffer 池统计不可用的原因（决策 #68：不静默省略，
// 无具体原因时给通用说明）。
func bufUnavailableReason(buf state.Buffers) string {
	if buf.Reason != "" {
		return buf.Reason
	}
	return "statsclient 解码失败或 stats segment 未启用"
}
