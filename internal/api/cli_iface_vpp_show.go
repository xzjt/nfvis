package api

// M5-9 收尾：补齐契约 §1.1 中此前无分发分支的命令族
//   show interfaces [physical|management|<ifname> [detail|statistics|sriov]]
//   show port-mirroring / show qos policies / show vpp [threads|buffers|memory]
//   show lldp neighbors（契约写法；等价于 show protocols lldp neighbors）
// 运行态来源：state（线程/buffer/内存/接口计数）与 committed 配置；未接入时给明确提示。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
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
	// show interfaces physical|management
	if len(args) >= 1 && (args[0] == "physical" || args[0] == "management") {
		if args[0] == "management" {
			return x.showManagementInterface(cfg)
		}
		if len(args) == 1 {
			// 决策 #155：`physical` 选择器退役为等价写法——聚合表与裸摘要同为运行态清单
			return x.showInterfaceList(cfg)
		}
		// `physical` 可省（契约 §1.1，决策 #153/#155）：一切形态与裸写法同一实现
		sub := ""
		if len(args) >= 3 {
			sub = args[2]
		}
		return x.showOneInterface(cfg, args[1], sub)
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
	// show interfaces（决策 #155：运行态清单 = 配置声明 ∪ VPP 运行态口）
	return x.showInterfaceList(cfg)
}

// showInterfaceList 接口运行态清单（决策 #155）：行 = 配置声明 ∪ VPP 运行态口
// ∪ 内核未接管口（决策 #302，收口 round81 F1：首装在接管前也能看见网卡），
// Admin/Link/Speed/Driver/计数全取运行态。旧摘要的 Admin 取自配置的 enabled、
// 旧聚合表只列声明口——两处都是 #84「字段取配置而非运行态」的残留，本轮收口。
// 来源列标注三类特殊情况：仅声明未生效（VPP 运行态里没有）、纯运行态口
// （派生口 bvi0/vh-* 或外部接管，未声明）、内核侧未接管的物理口（未接管）。
func (x *cliExecutor) showInterfaceList(cfg model.Config) string {
	states, stErr := x.ifaceStates()
	inv, invErr := x.vppIfaceNamesSafe()
	inInv := map[string]bool{}
	for _, n := range inv {
		inInv[n] = true
	}
	declared := map[string]string{} // name → description
	declaredNames := make([]string, 0, len(cfg.Interfaces))
	names := make([]string, 0, len(cfg.Interfaces)+len(inv))
	seen := map[string]bool{}
	for _, ifc := range cfg.Interfaces {
		declared[ifc.Name] = ifc.Description
		declaredNames = append(declaredNames, ifc.Name)
		if !seen[ifc.Name] {
			seen[ifc.Name] = true
			names = append(names, ifc.Name)
		}
	}
	for _, n := range inv {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	// 决策 #302：内核侧未接管的物理口并入清单（kernel − VPP 已接管 − 配置已声明）。
	untaken := map[string]bool{}
	for _, n := range untakenKernelIfnames(x.kernelIfnamesSafe(), inv, declaredNames) {
		untaken[n] = true
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)
	dpNameSp := x.dpRuntimeName()
	if len(names) == 0 {
		// 读数为空且读数路径报错：**两类失败各说一次**（状态快照与端口清单是两个独立来源，
		// 数据面卡住时往往一起失败——只报其一会让另一半的失败看不见）。
		var why []string
		if stErr != nil {
			why = append(why, dpNameSp+"运行态不可用（"+stErr.Error()+"）")
		}
		if invErr != nil {
			why = append(why, dpNameSp+"端口清单不可用（"+invErr.Error()+"）")
		}
		if len(why) > 0 {
			return fmt.Sprintf("（无接口；%s，清单可能不完整）\n", strings.Join(why, "；"))
		}
		return "（无接口）\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, ifaceListRowFmt,
		"Interface", "Admin", "Link", "Speed", "Driver", "RxPkts", "TxPkts", "Description", "备注")
	items := make([]any, 0, len(names))
	kernelDP := x.dpMode() == model.DataPlaneKernel
	for _, name := range names {
		desc, decl := declared[name]
		entry, admin, link, speed, driver, rx, tx := x.ifaceRuntimeRow(name, desc, states)
		inVPP := inInv[name]
		if _, ok := states[name]; ok {
			inVPP = true
		}
		source := "-"
		switch {
		case untaken[name]:
			// 未接管的内核口（决策 #302）：如实标注；不编造 VPP 侧事实（各列保持 -）
			source = "未接管"
		case decl && !inVPP:
			source = "已声明未生效"
		case !decl:
			source = "未声明"
		}
		// 决策 #431：内核数据面下「已声明却没进数据面」的口，备注列点名原因——① 仍绑
		// vfio-pci（附 PCI 与照做路径）；② 被 networkd 持有为 down（附人工做法）；取不到原因
		// 沿用既有「已声明未生效」。只在**内核数据面**且该口未生效（不在运行态，或运行态里
		// 管理态为 down）时点名；VPP 数据面渲染不受影响。
		if kernelDP && decl && !kernelIfaceAdminUp(states, name) {
			if reason, ok := x.kernelIfNotInDPReason(name); ok {
				source = reason
			}
		}
		entry["source"] = source
		items = append(items, entry)
		fmt.Fprintf(&b, ifaceListRowFmt, name, admin, link, speed, driver, rx, tx, desc, source)
	}
	if stErr != nil {
		// 运行态不可用：明确说明状态列为何是 "-"（按数据面点名，内核下不写「VPP 运行态」）
		b.WriteString("%% 注: " + dpNameSp + "运行态不可用（" + stErr.Error() + "），Admin/Link/Speed/Driver 显示为 -\n")
	}
	if invErr != nil {
		// 读数失败**如实带原因**（决策 #422）：端口清单查询失败（如数据面读数超时）与
		// 「这台机器真没有运行态口」是两件事，操作者要能看出是前者。与上面那条**并列**：
		// 两个来源各自独立，数据面卡住时常常一起失败，只报一条会把另一半的失败藏起来。
		b.WriteString(fmt.Sprintf("%% 注: %s端口清单不可用（%s），清单不含 %s运行态口（内核侧未接管口照列）\n",
			dpNameSp, invErr.Error(), dpNameSp))
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
	// 内核数据面下没有 VPP 线程/主堆可读：不要把「没读数」渲染成 `threads: 0`（假读数，
	// 会被读成「VPP 起来了却一个线程都没有」）——按数据面点名不可用，结构化输出也不带该字段。
	kernel := x.dpMode() == model.DataPlaneKernel
	if !kernel {
		out["threads"] = len(threads)
	}
	if hasBuf {
		out["buffers"] = anyToTree(buf)
	}
	if hasMem {
		out["memory"] = anyToTree(mem)
	}
	x.structured = out
	if kernel {
		fmt.Fprintf(&b, "threads: 不适用（%s）\n", kernelNoVppRuntime)
	} else {
		fmt.Fprintf(&b, "threads: %d\n", len(threads))
	}
	if hasBuf {
		fmt.Fprintf(&b, "buffers: pools=%d source=%s\n", len(buf.Pools), buf.Source)
		for _, pl := range buf.Pools {
			fmt.Fprintf(&b, "  %-10s used=%.0f available=%.0f cached=%.0f\n", pl.Name, pl.Used, pl.Available, pl.Cached)
		}
	} else {
		fmt.Fprintf(&b, "buffers: 运行态不可用（%s）\n", bufUnavailableReason(buf, x.dpMode()))
	}
	if hasMem {
		fmt.Fprintf(&b, "memory: total=%d used=%d free=%d\n", mem.Total, mem.Used, mem.Free)
	} else if kernel {
		fmt.Fprintf(&b, "memory: 运行态不可用（%s）\n", kernelNoVppRuntime)
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
	// 当前生效的数据面实现（v3 决策 #404）：内核数据面下这组读数整体不适用，先如实点明数据面，
	// 避免操作者把 `connected: no` 读成「VPP 该起来却没起来」。
	if st.Mode != "" {
		fmt.Fprintf(b, "dataplane: %s\n", st.Mode)
	}
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

// showOneInterface 单接口视图：裸写法 / physical <ifname> / detail / statistics / sriov
// 的**唯一实现**（决策 #153 收口「physical 可省」，#155 收口全形态同源）。
// 决策 #155：接口族全运行态——裸/physical/detail 全部回运行态单口视图（声明口与
// 派生口同一实现）；接口的**配置视图**退役到配置模式（edit interfaces <name> + show，
// 或 show configuration | display set）。statistics 一律回**带字段名的计数表**
// （列名与顺序同 REST 契约的 statistics 对象，见 renderIfaceCounters）；sriov 仍读声明
// （VF 配置只在声明口存在；未声明口回空态）。
func (x *cliExecutor) showOneInterface(cfg model.Config, name, sub string) string {
	desc, declared := "", false
	var ifc model.InterfaceConfig
	for _, i := range cfg.Interfaces {
		if i.Name == name {
			desc, declared, ifc = i.Description, true, i
			break
		}
	}
	switch sub {
	case "", "detail":
		if out, ok := x.ifaceRuntimeView(name, desc, declared, ifc); ok {
			return out
		}
		// 未声明且不在 VPP 运行态：先看内核侧（决策 #302）——未接管的物理口回内核事实
		// 视图（驱动/MAC/速率/状态取 sysfs，不编造数据面统计）；真未知名才报错。
		if out, ok := x.kernelIfaceView(name); ok {
			return out
		}
		// 未声明且不在 VPP 清单（决策 #154：清单查询成功才可判「不在」）
		names, invErr := x.vppIfaceNamesSafe()
		if invErr == nil && !ifaceInList(names, name) {
			return x.errIfaceUnknown(name)
		}
		// 清单未接入或查询失败：无从核对运行态，维持既有文案（只陈述「未声明」，不否认存在）；
		// 但把**失败原因**带上（决策 #422）——读数超时/连接不可用与「这个口真的不在」是两件事，
		// 操作者要能看出是前者，别把一次读数失败读成自己敲错了名字。
		if invErr != nil {
			return fmt.Sprintf("%% 接口 %s 未在配置中声明；%s端口清单亦不可用（%s）\n",
				name, x.dpRuntimeName(), invErr.Error())
		}
		return fmt.Sprintf("%% 接口 %s 未在配置中声明\n", name)
	case "statistics":
		if x.state != nil {
			if c, ok := x.state.InterfaceCounters(context.Background(), name); ok {
				x.structured = map[string]any{"interface": name, "statistics": anyToTree(c)}
				return renderIfaceCounters(name, c)
			}
		}
		// 未接管的内核口没有数据面统计（决策 #302）：如实区分于「连接未就绪」。
		kernel := x.dpMode() == model.DataPlaneKernel
		if _, ok := x.kernelIfaceFacts(name); ok {
			if kernel {
				return fmt.Sprintf("（接口 %s 未被数据面接管（当前数据面为 Linux 内核网络），无数据面统计）\n", name)
			}
			return fmt.Sprintf("（接口 %s 未被 VPP 接管，无数据面统计）\n", name)
		}
		// 如实描述：stats 是接入了的，取不到数是**这一刻连接没就绪/读取失败**
		// （VPP 重启后连接陈旧即属此列，取数路径会自行重连重试）。内核数据面下根本没有
		// VPP stats 通道，按数据面点名，别把人引去排查「stats 连接」。
		if kernel {
			return fmt.Sprintf("%% 接口 %s 统计暂不可用（%s）\n", name, kernelNoVppRuntime)
		}
		return fmt.Sprintf("%% 接口 %s 统计暂不可用（stats 连接未就绪）\n", name)
	case "sriov":
		if !declared {
			return fmt.Sprintf("（接口 %s 未在配置中声明，无 SR-IOV 配置）\n", name)
		}
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

// ifaceCountersRowFmt 接口计数表的行格式（表头与数据行共用同一份列宽定义——两处各写一份
// 必然漂移）。列名与顺序取自 REST 契约 `GET /interfaces/{name}` 的 `statistics` 对象
// （openapi `Interface.statistics` 的八个字段：rx_packets / tx_packets / rx_bytes / tx_bytes /
// rx_errors / tx_errors / rx_drops / tx_drops），与 `monitor interfaces` 的行格式同族。
const ifaceCountersRowFmt = "%-14s %12s %12s %14s %14s %9s %9s %8s %8s\n"

// renderIfaceCounters 单接口计数表的**唯一渲染实现**（决策 #427①）：此前 statistics 子命令
// 用 `%v` 打整个结构体（`interface ens192 statistics: {355512 417 …}`），一串裸数字既无字段名
// 也无列序，操作者不可判读（对照 vnf statistics 是有列名的表格）。字段名不另造：按 REST 契约
// 的 JSON 字段名逐一对齐（数据本是同一个 state.InterfaceCounters，`| display json` 与 REST
// 同字段名），数值映射逐列对应、顺序敏感（数值先转字符串，表头与数据行共用同一列宽格式）。
func renderIfaceCounters(name string, c state.InterfaceCounters) string {
	u := func(v uint64) string { return strconv.FormatUint(v, 10) }
	var b strings.Builder
	fmt.Fprintf(&b, "interface %s statistics:\n", name)
	fmt.Fprintf(&b, ifaceCountersRowFmt,
		"Interface", "rx-pkts", "tx-pkts", "rx-bytes", "tx-bytes", "rx-errors", "tx-errors", "rx-drops", "tx-drops")
	fmt.Fprintf(&b, ifaceCountersRowFmt, name,
		u(c.RxPackets), u(c.TxPackets), u(c.RxBytes), u(c.TxBytes),
		u(c.RxErrors), u(c.TxErrors), u(c.RxDrops), u(c.TxDrops))
	return b.String()
}

// ifaceRuntimeView 单口运行态视图（决策 #154/#155）：行格式与数据源同一实现
// （ifaceRuntimeRow/ifaceRowFmt）。declared=该口在配置中声明（描述列取声明值）；
// 未声明口仅在 VPP 清单可核时作答（查询出错 → 调用方按 #154 口径报错）。
// 注记三态：未声明（说明口径）、已声明但运行态未出现（状态列为 -）、运行态不可用。
//
// MTU 列为**有效 MTU**（R86-7）：配置显式值优先、否则运行态（VPP L3 MTU）；两者都取不到
// 显示 `-`，结构化输出不带 mtu 字段（「取不到就不给」，同 REST 读视图口径）。
func (x *cliExecutor) ifaceRuntimeView(name, desc string, declared bool, ifc model.InterfaceConfig) (string, bool) {
	names, invErr := x.vppIfaceNamesSafe()
	inInv := invErr == nil && ifaceInList(names, name)
	if !declared && !inInv {
		return "", false
	}
	states, stErr := x.ifaceStates()
	entry, admin, link, speed, driver, rx, tx := x.ifaceRuntimeRow(name, desc, states)
	mtuCol := "-"
	if mtu, ok := effectiveMTU(ifc.MTU, states[name]); ok {
		mtuCol = fmt.Sprintf("%d", mtu)
		entry["mtu"] = mtu
	}
	// QoS 方向绑定（决策 #331）：接口详情要能看出入向/出向各绑了哪条策略。
	if declared {
		if ifc.IngressPolicy != "" {
			entry["ingress_policy"] = ifc.IngressPolicy
		}
		if ifc.EgressPolicy != "" {
			entry["egress_policy"] = ifc.EgressPolicy
		}
	}
	_, inStates := states[name]
	var b strings.Builder
	switch {
	case !declared:
		fmt.Fprintf(&b, "（接口 %s 未在配置中声明，以下为运行态视图）\n", name)
	case !inStates && !inInv:
		fmt.Fprintf(&b, "（接口 %s 已声明，未在 %s运行态出现，状态列显示 -）\n", name, x.dpRuntimeName())
	}
	fmt.Fprintf(&b, ifaceRowFmt, "Interface", "Admin", "Link", "Speed", "MTU", "Driver", "RxPkts", "TxPkts", "Description")
	fmt.Fprintf(&b, ifaceRowFmt, name, admin, link, speed, mtuCol, driver, rx, tx, desc)
	if declared && (ifc.IngressPolicy != "" || ifc.EgressPolicy != "") {
		in, out := ifc.IngressPolicy, ifc.EgressPolicy
		if in == "" {
			in = "-"
		}
		if out == "" {
			out = "-"
		}
		fmt.Fprintf(&b, "QoS: 入向 %s / 出向 %s\n", in, out)
	}
	// 入向风暴抑制（决策 #385）：配置 + 数据面实测（policer/分类表/计数）三面同源。
	if declared && ifc.StormControl != nil && (ifc.StormControl.BroadcastKbps > 0 || ifc.StormControl.MulticastKbps > 0) {
		txt, sc := x.stormControlBlock(ifc)
		b.WriteString(txt)
		if cfg, ok := sc["storm_control"].(map[string]any); ok && len(cfg) > 0 {
			entry["storm_control"] = cfg
		}
		if rt, ok := sc["storm_control_runtime"]; ok {
			entry["storm_control_runtime"] = rt
		}
	}
	// 端口安全（决策 #389）：配置 + 数据面实况（macip tag 反查 + 绑定实况）三面同源。
	if declared && len(ifc.PortSecurity) > 0 {
		txt, ps := x.portSecBlock(ifc)
		b.WriteString(txt)
		if rt, ok := ps["port_security_runtime"].(map[string]any); ok {
			entry["port_security_runtime"] = rt
		}
	}
	if stErr != nil {
		b.WriteString("%% 注: " + x.dpRuntimeName() + "运行态不可用（" + stErr.Error() + "），Admin/Link/Speed/Driver 显示为 -\n")
	}
	x.structured = map[string]any{"interfaces": []any{entry}}
	return b.String(), true
}

// kernelIfaceView 未接管口的内核侧单口视图（决策 #302）：与运行态单口视图同一张表
// （ifaceRowFmt），值取 sysfs（Admin/Link/Speed/MTU/Driver），计数列如实为 -
// （不编造 VPP 侧统计），MAC 单独成行（表没有该列）。name 不是内核物理口时 ok=false，
// 由调用方继续原有的判错链。
func (x *cliExecutor) kernelIfaceView(name string) (string, bool) {
	f, ok := x.kernelIfaceFacts(name)
	if !ok {
		return "", false
	}
	admin := "down"
	if f.AdminUp {
		admin = "up"
	}
	link := "-"
	if f.LinkKnown {
		link = "down"
		if f.LinkUp {
			link = "up"
		}
	}
	speed, mtu := "-", "-"
	if f.SpeedMbps > 0 { // 内核 speed 本就以 Mbps 计；换算成 kbps 走同一渲染
		speed = fmtSpeed(f.SpeedMbps * 1000)
	}
	if f.MTU > 0 {
		mtu = fmt.Sprintf("%d", f.MTU)
	}
	var b strings.Builder
	if x.dpMode() == model.DataPlaneKernel {
		// 内核数据面下这个口不是「未被 VPP 接管」——数据面就是内核网络，它只是未进
		// 数据面端口清单；按数据面点名，同时保留「以内核事实作答」的本意。
		fmt.Fprintf(&b, "（接口 %s 未被数据面接管（当前数据面为 Linux 内核网络），以下为内核侧视图）\n", name)
	} else {
		fmt.Fprintf(&b, "（接口 %s 未被 VPP 接管，以下为内核侧视图）\n", name)
	}
	fmt.Fprintf(&b, ifaceRowFmt, "Interface", "Admin", "Link", "Speed", "MTU", "Driver", "RxPkts", "TxPkts", "Description")
	fmt.Fprintf(&b, ifaceRowFmt, name, admin, link, speed, mtu, orDash(f.Driver), "-", "-", "-")
	if f.MAC != "" {
		fmt.Fprintf(&b, "mac: %s\n", f.MAC)
	}
	entry := map[string]any{"name": name, "taken_over": false, "admin_up": f.AdminUp}
	if f.LinkKnown {
		entry["link_up"] = f.LinkUp
	}
	if f.SpeedMbps > 0 {
		entry["link_speed_kbps"] = f.SpeedMbps * 1000 // 与 VPP 口同单位（kbps）
	}
	if f.Driver != "" {
		entry["driver"] = f.Driver
	}
	if f.MAC != "" {
		entry["mac"] = f.MAC
	}
	if f.MTU > 0 {
		entry["mtu"] = f.MTU
	}
	x.structured = map[string]any{"interfaces": []any{entry}}
	return b.String(), true
}

// vppIfaceNamesSafe VPP 运行态接口清单：未接入或查询失败一律返回错误
// （判「不在清单」必须有成功的查询——宁可少报错，与 interfaceKnown 同取向）。
//
// 返回**错误原因**而不是一个布尔（决策 #422）：读数失败（数据面读数超时、连接不可用）
// 与「清单里确实没有这个口」在判据上是同一件事（都不能判「不在清单」），但对操作者是
// 两件事——调用方要把原因如实打出来，别让人把一次读数失败读成配置写错了。
func (x *cliExecutor) vppIfaceNamesSafe() ([]string, error) {
	if x.ports == nil {
		return nil, errors.New("运行态端口清单未接入")
	}
	names, err := x.ports.VPPIfnames()
	if err != nil {
		return nil, err
	}
	return names, nil
}

func ifaceInList(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// errIfaceUnknown 接口名既不在配置、也不在当前数据面运行态清单（清单查询成功才可判）的
// 统一文案——LLDP 过滤与 show interfaces 共用，防两处漂移（决策 #154）。
// 「不在 VPP 接口清单中」按数据面措辞：内核数据面下不存在 VPP 清单。
func (x *cliExecutor) errIfaceUnknown(name string) string {
	return fmt.Sprintf("%% 接口 %s 未在配置中声明、也不在 %s接口清单中（show interfaces physical 看运行态清单）\n",
		name, x.dpRuntimeName())
}

// ifaceRowFmt 接口运行态表的行格式（表头与数据行共用；单口视图含 MTU 列，
// 列表表 ifaceListRowFmt 不含——语义校验按列比对 vppctl，列表列不变，R86-7）。
const ifaceRowFmt = "%-14s %-7s %-7s %-10s %-7s %-12s %-10s %-12s %s\n"

// ifaceRuntimeRow 单口运行态数据与展示值（决策 #84 口径：Admin/Link/Speed/Driver 取
// VPP 运行态，收发计数取 state 快照）。物理口表与未声明运行态口视图共用（同一实现，
// 防两处渲染漂移）。
func (x *cliExecutor) ifaceRuntimeRow(name, description string, states map[string]InterfaceState) (entry map[string]any, admin, link, speed, driver, rx, tx string) {
	entry = map[string]any{"name": name, "description": description}
	admin, link, speed, driver = "-", "-", "-", "-"
	if st, ok := states[name]; ok {
		admin, link = yn(st.AdminUp), yn(st.LinkUp)
		speed, driver = fmtSpeed(st.LinkSpeed), orDash(st.DevType)
		entry["admin_up"], entry["link_up"] = st.AdminUp, st.LinkUp
		entry["link_speed_kbps"], entry["driver"] = st.LinkSpeed, st.DevType
	}
	rx, tx = "-", "-"
	if x.state != nil {
		if c, ok := x.state.InterfaceCounters(context.Background(), name); ok {
			rx, tx = fmt.Sprintf("%d", c.RxPackets), fmt.Sprintf("%d", c.TxPackets)
			entry["statistics"] = anyToTree(c)
		}
	}
	return
}

// execShowPortMirroring：show port-mirroring（配置视图；契约 §1.1 只声明无参形态）。
//
// 多余/未知 token 必须报错：此前 args 被**完全忽略**，`show port-mirroring bogus extra`
// 照样打印全部会话——操作者会以为「过滤生效了、这就是我要的那条」，与决策 #153
// （`show configuration` 的未知子命令）同族的静默误答。本命令的渲染不分形态，故明确
// 报「未支持」并列出可用写法，同族的 `show qos policies` 亦同口径。
func (x *cliExecutor) execShowPortMirroring(args []string) string {
	if len(args) != 0 {
		return fmt.Sprintf("%% 该 show 命令形式未支持: show port-mirroring %s（可用：show port-mirroring）\n",
			strings.Join(args, " "))
	}
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

// execShowQos：show qos policies —— 策略 + **方向绑定**（决策 #331：单看策略对象看不出
// 入向/出向被谁引用，故加「绑定」列：接口:in / 接口:out，与 REST 读视图同源）。
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
	views := qosPolicyViews(cfg)
	var b strings.Builder
	fmt.Fprintf(&b, "%-14s %-12s %-12s %s\n", "Policy", "CIR(bps)", "CBS(bytes)", "绑定方向（接口:in|out）")
	for _, v := range views {
		name, _ := v["name"].(string)
		cir, _ := v["cir"].(int)
		cbs, _ := v["cbs"].(int)
		binds, _ := v["bindings"].([]qosBinding)
		var parts []string
		for _, bd := range binds {
			dir := "in"
			if bd.Direction == "egress" {
				dir = "out"
			}
			parts = append(parts, bd.Interface+":"+dir)
		}
		col := "-"
		if len(parts) > 0 {
			col = strings.Join(parts, ",")
		}
		fmt.Fprintf(&b, "%-14s %-12d %-12d %s\n", name, cir, cbs, col)
	}
	items := make([]any, 0, len(views))
	for _, v := range views {
		items = append(items, anyToTree(v))
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
		// `show vpp` 概览的**唯一实现**（曾在此处另有一份 `case ""` 副本——两份渲染
		// 必然漂移，已删；数据面感知的线程/内存行以 showVppOverview 为准）。
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
			// 内核数据面下「无线程」不是 VPP 的空读数，而是根本没有 VPP 运行态可读：
			// 如实点名数据面，不打印会被读成「VPP 在线但无工作线程」的空表。
			if x.dpMode() == model.DataPlaneKernel {
				return "%% VPP 线程运行态不可用：" + kernelNoVppRuntime + "\n"
			}
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
	case "buffers":
		buf, ok := x.state.Buffers(ctx)
		if !ok {
			return fmt.Sprintf("%% buffer 池运行态不可用：%s\n", bufUnavailableReason(buf, x.dpMode()))
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
			if x.dpMode() == model.DataPlaneKernel {
				return "%% VPP 内存运行态不可用：" + kernelNoVppRuntime + "\n"
			}
			return "%% 内存运行态不可用\n"
		}
		x.structured = anyToTree(mem)
		return fmt.Sprintf("total=%d used=%d free=%d\n", mem.Total, mem.Used, mem.Free)
	case "runtime":
		return x.execShowVppRuntime(ctx, args[1:])
	case "capture":
		return x.execShowVppCapture()
	}
	return fmt.Sprintf("%% 无效命令: show vpp %s（可用：threads|buffers|memory|runtime|capture）\n", sub)
}

// execShowVppRuntime：`show vpp runtime [thread <id>]`——**线程级**运行态（决策 #200）。
//
// 口径（与手册同步）：给每线程的向量率与主循环速率、整机向量率、工作线程数、数据面运行时长；
// 数据源是 VPP stats segment（经与 VPP 同版本的 vpp_get_stats 解码，与 buffer/接口计数同一条通道）。
// VPP 26.06 的**按节点**明细（`show runtime` 主体那张 Calls/Vectors/Packet-Clocks 表）既不在
// stats segment 也无二进制 API——产品不解析 vppctl 文本，故如实只报线程级，并把这句话打印出来，
// 免得操作者以为「这台机器没有运行态」。
func (x *cliExecutor) execShowVppRuntime(ctx context.Context, rest []string) string {
	filter := -1
	switch {
	case len(rest) == 0:
	case len(rest) == 2 && rest[0] == "thread":
		id, err := strconv.Atoi(rest[1])
		if err != nil || id < 0 {
			return fmt.Sprintf("%% 线程号 %q 不合法\n", rest[1])
		}
		filter = id
	default:
		return "%% 用法: show vpp runtime [thread <id>]\n"
	}

	rs, ok := x.state.RuntimeStats(ctx)
	if !ok {
		return fmt.Sprintf("%% VPP runtime 运行态不可用：%s\n", runtimeUnavailableReason(rs, x.dpMode()))
	}
	// 线程名/绑核能从 show_threads 拿到就带上（best-effort：取不到不影响本命令）
	meta := map[uint32]state.Thread{}
	for _, th := range x.state.Threads(ctx) {
		meta[th.ID] = th
	}
	rows := rs.Threads
	if filter >= 0 {
		rows = nil
		for _, r := range rs.Threads {
			if r.ID == uint32(filter) {
				rows = []state.RuntimeThread{r}
				break
			}
		}
		if len(rows) == 0 {
			ids := make([]string, 0, len(rs.Threads))
			for _, r := range rs.Threads {
				ids = append(ids, strconv.Itoa(int(r.ID)))
			}
			return fmt.Sprintf("%% 无线程 %d 的运行态（可用线程：%s）\n", filter, strings.Join(ids, ","))
		}
	}

	x.structured = anyToTree(rs)
	var b strings.Builder
	fmt.Fprintf(&b, "source: %s\n", rs.Source)
	fmt.Fprintf(&b, "vector rate: %.2f\n", rs.VectorRate)
	fmt.Fprintf(&b, "worker threads: %.0f\n", rs.WorkerThreads)
	fmt.Fprintf(&b, "uptime: %.0fs\n", rs.UptimeSeconds)
	fmt.Fprintf(&b, "%-4s %-12s %-6s %-12s %s\n", "ID", "Name", "Core", "VectorRate", "LoopsRate")
	for _, r := range rows {
		th := meta[r.ID]
		fmt.Fprintf(&b, "%-4d %-12s %-6d %-12.2f %.2f\n", r.ID, th.Name, th.Core, r.VectorRate, r.LoopsRate)
	}
	b.WriteString("说明: 线程级运行态；按节点明细（逐节点的指令周期/向量数）无结构化来源，需要时用 vppctl show runtime\n")
	return b.String()
}

// kernelNoVppRuntime 内核数据面下「VPP 运行态读数」不可用的统一原因（buffer/runtime/
// 线程/内存几处共用同一句，避免各写一份漂移）。
const kernelNoVppRuntime = "当前数据面为 Linux 内核网络，无 VPP 运行态读数"

// runtimeUnavailableReason 取不可用原因（与 bufUnavailableReason 同口径：不静默省略）。
func runtimeUnavailableReason(rs state.RuntimeStats, mode string) string {
	if mode == model.DataPlaneKernel {
		return kernelNoVppRuntime
	}
	if rs.Reason != "" {
		return rs.Reason
	}
	return "stats segment 不可用（未取到 /sys 运行态计数）"
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
			return x.errIfaceUnknown(ifname)
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

// interfaceKnown 接口名是否为本机已知接口：committed 里声明过，或 VPP 运行态接口清单
// 里有（与 `<ifname>` 动态候选同源，决策 #83；`show interfaces <ifname>` 对两侧都作答，
// 决策 #154）。
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
func bufUnavailableReason(buf state.Buffers, mode string) string {
	// 内核数据面下本就没有 VPP 运行态可读：如实说清，别报成"解码失败"把人引向 VPP 排查。
	if mode == model.DataPlaneKernel {
		return kernelNoVppRuntime
	}
	if buf.Reason != "" {
		return buf.Reason
	}
	return "statsclient 解码失败或 stats segment 未启用"
}

// dpRuntimeName 当前数据面运行态的称呼（**带尾随空格**，模板里直接后接中文词即可）：
// VPP 数据面叫「VPP 」、内核数据面叫「Linux 内核网络」。「运行态不可用」「在 … 中不存在」
// 这类注记一律按它措辞——内核数据面下再点名 VPP 会把操作者引向一条不存在的路径。
func (x *cliExecutor) dpRuntimeName() string {
	if x.dpMode() == model.DataPlaneKernel {
		return "Linux 内核网络"
	}
	return "VPP "
}

// dpMode 当前生效的数据面实现。**按装配事实**：VppController 由装配处按数据面注入并
// 如实自报 Status().Mode（内核数据面报 kernel、VPP 数据面报 vpp）。committed 配置可能
// 已改而服务未重启，读视图若按 committed 作答就会与数据面实况相反（例如已 `set system
// dataplane kernel` 但进程仍装着 VPP：所有运行态读数实际来自 VPP）。控制器未装配或
// 未自报（旧装配/单测）时回落 committed；都取不到按 vpp。
func (x *cliExecutor) dpMode() string {
	if x.engine == nil {
		return model.DataPlaneVPP
	}
	cfg, err := x.engine.Committed()
	if err != nil {
		return model.DataPlaneVPP
	}
	return dataPlaneModeAssembled(x.vpp, cfg)
}
