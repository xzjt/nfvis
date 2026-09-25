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

	"github.com/xzjt/nfvis/internal/config"
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
		if len(args) == 2 {
			// 单口运行态视图（链接/速率/驱动/计数，决策 #84 的运行态口径）
			return x.showPhysicalInterfaces(cfg, args[1])
		}
		// `physical` 可省（契约 §1.1，决策 #153）：带子命令时两种写法必须走**同一实现**。
		// 此前这里把 args[2] 整段丢掉、只按单口列表渲染——`show interfaces physical ens224
		// statistics` 打的是同一张表（无任何计数），与 `show interfaces ens224 statistics`
		// 不同源，属静默误答（契约「physical 可省」因此不成立）。
		return x.showOneInterface(cfg, args[1], args[2])
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

// showVppOverview `show vpp` 概览（发现 #11）：版本/连接/待重启来自**连接管理器**
// （不需要 stats 段），线程/buffer/内存来自 stats 运行态——后者不可用时仍给出前者，
// 而不是整条命令报「运行态不可用」。此前只打印后三者，连**版本**（命令全表声明的字段）
// 与 **pending_restart**（"下一步要不要 request vpp restart"的唯一指示）都看不到。
func (x *cliExecutor) showVppOverview() string {
	var b strings.Builder
	writeVppConnLines(&b, x.vpp, x.engine)
	if x.state == nil {
		if b.Len() == 0 {
			return errRuntimeUnavailable
		}
		return b.String()
	}
	ctx := context.Background()
	threads := x.state.Threads(ctx)
	buf, hasBuf := x.state.Buffers(ctx)
	mem, hasMem := x.state.Memory(ctx)
	out, _ := x.structured.(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	out["threads"] = len(threads)
	if hasBuf {
		out["buffers"] = anyToTree(buf)
	}
	if hasMem {
		out["memory"] = anyToTree(mem)
	}
	x.structured = out
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
}

// writeVppConnLines 写「连接/版本/待重启」三行（连接管理器提供；未装配则跳过）。
func writeVppConnLines(b *strings.Builder, vpp VppController, eng *config.Engine) {
	if vpp == nil || eng == nil {
		return
	}
	cfg, err := eng.Committed()
	if err != nil {
		return
	}
	st := vpp.Status(cfg.Vpp)
	version := st.Version
	if version == "" {
		version = "(未知)"
	}
	fmt.Fprintf(b, "version: %s\n", version)
	fmt.Fprintf(b, "connected: %s\n", yesNoCn(st.Connected))
	// 待重启是操作者的**下一步动作指示**（vpp 段变更后需 request vpp restart）
	fmt.Fprintf(b, "pending_restart: %s\n", yesNoCn(st.PendingRestart))
	if st.LastError != "" {
		fmt.Fprintf(b, "last_error: %s\n", st.LastError)
	}
}

func yesNoCn(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// showManagementInterface：管理口（内核侧，来自 system.management 配置）。
func (x *cliExecutor) showManagementInterface(cfg model.Config) string {
	sys := cfg.System
	if sys == nil || sys.Management == nil {
		return "（未配置管理口：set system management interface <ifname> / ip address <prefix>）\n"
	}
	m := sys.Management
	if m.Interface == "" && m.Address == "" && m.Gateway == "" {
		// 空对象（例如曾配置过又删除）与「从未配置」是同一件事：给同一句话，
		// 而不是退化成另一条带 %% 的提示——`show` 的「没有」是**空态**不是**错误**
		// （同「（无 core dump）」的口径；带 %% 会被真机冒烟按失败计，发现 #14）。
		return "（未配置管理口：set system management interface <ifname> / ip address <prefix>）\n"
	}
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
		fmt.Fprintln(&b, "提示：未指定管理网卡（set system management interface <ifname>）；"+
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
	if sub == "" {
		return x.showVppOverview()
	}
	if x.state == nil {
		return errRuntimeUnavailable
	}
	ctx := context.Background()
	switch sub {
	case "threads":
		threads := x.state.Threads(ctx)
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
	case "":
		// show vpp：概览（连接/版本/待重启 + 线程数 + buffer + 内存）
		threads := x.state.Threads(ctx)
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
		writeVppConnLines(&b, x.vpp, x.engine)
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

// filterLLDPNeighbors 按接口名过滤 LLDP 邻居表（ifname 为空 = 不过滤）。
//
// 纯函数：不碰底座、不碰执行器状态。抽出来的理由是**真机验不了**——nfvis-vm 无 LLDP 对端，
// 邻居表恒为空（`docs/evidence/m5/t07-span-lldp.txt`），而空表上「过滤生效」与「过滤被丢」
// 输出完全相同，故过滤规则只能靠桩数据单测（见 `cli_declared_arg_drop_test.go`）。
// 接口名按**原样精确匹配**（VPP 接口名大小写敏感，不做大小写折叠——折叠会把两个真实存在的口混成一个）。
func filterLLDPNeighbors(rows []LldpNeighborRow, ifname string) []LldpNeighborRow {
	out := make([]LldpNeighborRow, 0, len(rows))
	for _, r := range rows {
		if ifname == "" || r.Interface == ifname {
			out = append(out, r)
		}
	}
	return out
}

// showLLDPNeighbors LLDP 邻居表的**唯一渲染实现**：契约 §1.1 的 `show lldp neighbors
// [interface <ifname>]` 与等价写法 `show protocols lldp neighbors` 都走这里（同一读物
// `GET /protocols/lldp/neighbors`，两处各写一份必然漂移）。
//
// ifname 非空 = 只保留该口的邻居；**无匹配时给明确文案**：既回不到全量、也不是一句空话。
// 此前 `execShowLldp` 把过滤参数整段丢掉，于是操作者问「某个口有没有邻居」拿到的是全量表
// （决策 #84/#85 同族的静默误答）。
func (x *cliExecutor) showLLDPNeighbors(ifname string) string {
	if x.lldp == nil {
		return errRuntimeUnavailable
	}
	rows, err := x.lldp.Neighbors(context.Background())
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	rows = filterLLDPNeighbors(rows, ifname)
	if len(rows) == 0 {
		if ifname == "" {
			return "（无 LLDP 邻居）\n"
		}
		if !x.interfaceKnown(ifname) {
			// 名字本身不存在（配置里没声明、也不在 VPP 接口清单中）——不能与「该口无邻居」混为一谈：
			// 前者是敲错了名字，后者是这个口就是没有对端。
			return fmt.Sprintf("%% 接口 %s 未在配置中声明、也不在 VPP 接口清单中（show interfaces physical 看运行态清单）\n", ifname)
		}
		return fmt.Sprintf("（接口 %s 无 LLDP 邻居）\n", ifname)
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

// interfaceKnown 接口名是否为本机已知接口：committed 里声明过（与 `show interfaces <ifname>`
// 同一判据），或 VPP 运行态接口清单里有（与 `<ifname>` 动态候选同源，决策 #83）。
//
// 两侧都认，是因为邻居表来自 VPP（键是 VPP 接口名）、而操作者也常按配置里的名字提问。
// 端口清单未接入（nil）时只按配置判定；清单**查询失败**时按「已知」处理——
// 宁可少报一次错，也不冤枉一个真实存在的口（判「未知」必须有高置信度依据）。
func (x *cliExecutor) interfaceKnown(name string) bool {
	if name == "" {
		return false
	}
	if cfg, err := x.engine.Committed(); err == nil {
		for _, ifc := range cfg.Interfaces {
			if ifc.Name == name {
				return true
			}
		}
	}
	if x.ports == nil {
		return false
	}
	names, err := x.ports.VPPIfnames()
	if err != nil {
		return true
	}
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// execShowLldp：契约 §1.1 写法 `show lldp neighbors [interface <ifname>]`
// （等价 `show protocols lldp neighbors`）。
//
// token 校验走**显式白名单**（决策 #153 的口径：多余的 token 必须报错，不得静默忽略）：
// 此前只判 `args[0] != "neighbors"`，其余 token 一律透传丢掉——`interface <ifname>` 与
// 更难察觉的 `… interface <ifname> bogus` 都被静默吃掉。
func (x *cliExecutor) execShowLldp(args []string) string {
	if len(args) >= 1 && args[0] != "neighbors" {
		return invalidShowLldp(strings.Join(args, " "))
	}
	switch {
	case len(args) <= 1:
		// `show lldp`（域节点）/ `show lldp neighbors`：不按口过滤（lldp 下只有邻居表这一种读物）
		return x.showLLDPNeighbors("")
	case len(args) == 3 && args[1] == "interface":
		return x.showLLDPNeighbors(args[2])
	default:
		return invalidShowLldp(strings.Join(args, " "))
	}
}

// invalidShowLldp：`show lldp <未知/多余 token>` 的统一报错。
// 措辞与既有 `% 无效命令` 一致：给出可用写法（含按接口过滤的形态），便于直接照着敲。
func invalidShowLldp(rest string) string {
	return fmt.Sprintf("%% 无效命令: show lldp %s（可用：show lldp neighbors [interface <ifname>]）\n", rest)
}

// bufUnavailableReason buffer 池统计不可用的原因（决策 #68：不静默省略，
// 无具体原因时给通用说明）。
func bufUnavailableReason(buf state.Buffers) string {
	if buf.Reason != "" {
		return buf.Reason
	}
	return "statsclient 解码失败或 stats segment 未启用"
}
