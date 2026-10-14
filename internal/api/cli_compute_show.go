package api

// M4-12：`show virtual-machine-functions`、`show container-functions`、
// `show images`、`show resource-pools` 的 CLI 渲染（契约 §1.1；附录 A #49/#51）。
//
// 与 M3 网络运行态 show（cli_net_runtime_show.go）同法：配置视图取 committed
// 配置，运行态字段（state/计数/快照/引用计数）取已注入的运行态接口；x.structured
// 供 `| display json/xml`。运行态接口缺失时降级为「配置可用、运行态省略」而非报错。

import (
	"context"
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/images"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

const errComputeUnavailable = "%% 计算编排未接入（libvirt 未装配），运行态不可用\n"

// execShowVMs：列表 / <name> [detail|interfaces|statistics|snapshots]。
// detail|interfaces 取 committed 配置视图（+ 运行态 state）；statistics 经 VPP
// vhost-user 口计数；snapshots 经 libvirt 快照查询。
func (x *cliExecutor) execShowVMs(args []string) string {
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if len(args) == 0 { // 列表：名称/状态/vCPU/内存/镜像
		items := make([]any, 0, len(cfg.VirtualMachineFunctions))
		var b strings.Builder
		fmt.Fprintf(&b, "%-16s %-10s %-5s %-12s %-16s %s\n", "Name", "State", "vCPU", "Memory", "Image", "Serial")
		for _, vm := range cfg.VirtualMachineFunctions {
			st := x.vmStateOf(context.Background(), vm.Name)
			// 有效值（决策 #461）：pin/backing/页尺寸未显式声明时补产品的确定缺省，免得界面「—」
			// 而运行态有事实；与 REST 读视图（vmReadView）同一实现。
			eff := model.EffectiveVM(cfg, vm)
			row, _ := anyToTree(eff).(map[string]any)
			row["state"] = st
			injectCoresAssigned(row, cfg, vm.Name)
			items = append(items, row)
			fmt.Fprintf(&b, "%-16s %-10s %-5d %-12s %-16s %v\n",
				vm.Name, st, eff.VCPU.Count, memoryDisplay(eff.Memory), vm.Image, serialEnabled(vm))
		}
		if len(items) == 0 {
			return "（无 VNF）\n"
		}
		x.structured = map[string]any{"virtual_machine_functions": items}
		return b.String()
	}

	name := args[0]
	vm, ok := findVM(cfg, name)
	if !ok {
		return fmt.Sprintf("%% VNF %s 不存在\n", name)
	}
	sub := ""
	if len(args) >= 2 {
		sub = args[1]
	}
	switch sub {
	case "", "detail":
		// 有效值（决策 #461）：与 REST 读视图（vmReadView）同一实现——配置未显式声明时补确定缺省。
		eff := model.EffectiveVM(cfg, vm)
		m, _ := anyToTree(eff).(map[string]any)
		m["state"] = x.vmStateOf(context.Background(), name)
		injectCoresAssigned(m, cfg, name)
		x.structured = m
		out := RenderConfigJSON(m)
		// 决策 #485：有效启动画像块（逐项标来源）——域映射用的合并规则与这里同源。
		out += "\n" + x.effectiveStartupBlock(vm, cfg.VMImageProfiles)
		if len(vm.PCIDevices) > 0 {
			out += "\n" + x.vmPCIDevicesView(vm.PCIDevices)
		}
		return out + "\n"
	case "interfaces":
		items := make([]any, 0, len(vm.Interfaces))
		var b strings.Builder
		fmt.Fprintf(&b, "%-10s %-12s %-18s %-18s %s\n", "vNIC", "Type", "VirtualSwitch", "MAC", "Socket/VF")
		for _, nic := range vm.Interfaces {
			items = append(items, anyToTree(nic))
			fmt.Fprintf(&b, "%-10s %-12s %-18s %-18s %s\n",
				nic.Name, nic.Type, nic.VirtualSwitch, nic.MAC, x.nicSocketOrVF(name, nic))
		}
		if len(items) == 0 {
			return "（无 vNIC）\n"
		}
		x.structured = map[string]any{"interfaces": items}
		return b.String()
	case "statistics":
		return x.showVMStatistics(name)
	case "snapshots":
		return x.showVMSnapshots(name)
	}
	return fmt.Sprintf("%% 无效命令: show virtual-machine-functions %s %s（可用：detail|interfaces|statistics|snapshots）\n",
		name, sub)
}

// vmPCIDevicesView 直通 PCI 设备的配置值 + 实测态（FR-CMP-023）：
// 配置面值逐条列出（上方 pci-devices 已给 JSON 形态）；能查到 /sys/bus/pci/devices 时
// 附「已在系统中 / 未在系统中」实测态，查不到如实说查不到（不猜、不静默省略）。
func (x *cliExecutor) vmPCIDevicesView(devs []string) string {
	var b strings.Builder
	b.WriteString("直通 PCI 设备（BDF）:\n")
	for _, raw := range devs {
		state := "无法核对（未接入设备事实源）"
		norm, err := model.NormalizeBDF(raw)
		switch {
		case err != nil:
			state = "配置值非法（" + err.Error() + "）"
		case x.pciExists == nil:
			// 保持上面的「未接入设备事实源」
		default:
			ok, e := x.pciExists(norm)
			switch {
			case e != nil:
				state = "无法核对（" + e.Error() + "）"
			case ok:
				state = "已在系统中"
			default:
				state = "未在系统中（提交期存在性检查会拒绝）"
			}
		}
		fmt.Fprintf(&b, "  %-18s %s\n", raw, state)
	}
	return b.String()
}

// vmStateOf 查询 VM 运行态，未装配/出错时返回 "-"（列表仍可用）。
func (x *cliExecutor) vmStateOf(ctx context.Context, name string) string {
	if x.vm == nil {
		return "-"
	}
	st, err := x.vm.VMState(ctx, name)
	if err != nil || st == "" {
		return "-"
	}
	return st
}

// showVMStatistics 渲染 vhost-user 口计数（经 VPP 运行态；FR-CMP-011）。
func (x *cliExecutor) showVMStatistics(name string) string {
	if x.state == nil {
		return "%% vNIC 统计不可用（运行态未接入）\n"
	}
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	vm, ok := findVM(cfg, name)
	if !ok {
		return fmt.Sprintf("%% VNF %s 不存在\n", name)
	}
	if x.dpMode() == model.DataPlaneKernel {
		// 内核数据面下 vNIC 由 libvirt 以 virtio + vhost-net + 宿主 tap 承载，不是 VPP 的
		// vhost-user 口：既没有 `vh-*` 读名，也没有 VPP 逐口计数通道——按数据面如实说明，
		// 不让操作者以为「VM 起了就该有这张表」。
		return "%% vNIC 统计不可用：当前数据面为 Linux 内核网络（vNIC 由 libvirt 以 vhost-net tap 承载，无 VPP vhost-user 口计数）\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-10s %-24s %12s %12s %14s %14s\n",
		"vNIC", "Interface", "rx-pkts", "tx-pkts", "rx-bytes", "tx-bytes")
	items := make([]any, 0, len(vm.Interfaces))
	missing := 0
	for _, nic := range vm.Interfaces {
		if nic.Type != "vhost-user" {
			continue
		}
		ifname := orchestrator.VnfIfaceName(name, nic.Name)
		row := map[string]any{"vnic": nic.Name, "interface": ifname}
		c, ok := x.state.InterfaceCounters(context.Background(), ifname)
		if !ok {
			missing++
			row["available"] = false
			items = append(items, row)
			fmt.Fprintf(&b, "%-10s %-24s %12s %12s %14s %14s\n", nic.Name, ifname, "-", "-", "-", "-")
			continue
		}
		row["available"] = true
		row["rx_packets"], row["tx_packets"] = c.RxPackets, c.TxPackets
		row["rx_bytes"], row["tx_bytes"] = c.RxBytes, c.TxBytes
		items = append(items, row)
		fmt.Fprintf(&b, "%-10s %-24s %12d %12d %14d %14d\n",
			nic.Name, ifname, c.RxPackets, c.TxPackets, c.RxBytes, c.TxBytes)
	}
	if len(items) == 0 {
		return "（无 vhost-user vNIC）\n"
	}
	if missing == len(items) {
		b.WriteString("（vhost-user 口未在 VPP 找到计数：VM 未运行或接口未建立）\n")
	}
	x.structured = map[string]any{"statistics": items}
	return b.String()
}

// showVMSnapshots 渲染快照列表（libvirt 运行态；FR-CMP-015）。
func (x *cliExecutor) showVMSnapshots(name string) string {
	if x.snaps == nil {
		return errComputeUnavailable
	}
	rows, err := x.snaps.Snapshots(context.Background(), name)
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if len(rows) == 0 {
		return "（无快照）\n"
	}
	items := make([]any, 0, len(rows))
	var b strings.Builder
	fmt.Fprintf(&b, "%-24s %-20s %s\n", "Name", "Created", "Description")
	for _, r := range rows {
		items = append(items, anyToTree(r))
		created := "-"
		if r.CreatedAt != nil {
			created = r.CreatedAt.Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(&b, "%-24s %-20s %s\n", r.Name, created, r.Description)
	}
	x.structured = map[string]any{"snapshots": items}
	return b.String()
}

// execShowContainers：列表 / <name> [detail|interfaces]。
func (x *cliExecutor) execShowContainers(args []string) string {
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if len(args) == 0 { // 列表：名称/状态/vCPU/镜像
		// 决策 #396：运行态查询传有界 ctx（此前 Background ⇒ dockerd 假死时
		// `show container-functions` 无界挂起）。
		sctx, scancel := containerCallCtx()
		defer scancel()
		items := make([]any, 0, len(cfg.ContainerFunctions))
		var b strings.Builder
		fmt.Fprintf(&b, "%-16s %-10s %-6s %-16s %s\n", "Name", "State", "vCPU", "Image", "Restart")
		for _, ct := range cfg.ContainerFunctions {
			st := x.ctStateOf(sctx, ct.Name)
			row, _ := anyToTree(ct).(map[string]any)
			row["state"] = st
			items = append(items, row)
			fmt.Fprintf(&b, "%-16s %-10s %-6d %-16s %s\n",
				ct.Name, st, ct.VCPU, ct.Image, ct.RestartPolicy)
		}
		if len(items) == 0 {
			return "（无容器）\n"
		}
		x.structured = map[string]any{"container_functions": items}
		return b.String()
	}

	name := args[0]
	ct, ok := findContainer(cfg, name)
	if !ok {
		return fmt.Sprintf("%% 容器 %s 不存在\n", name)
	}
	sub := ""
	if len(args) >= 2 {
		sub = args[1]
	}
	switch sub {
	case "", "detail":
		m, _ := anyToTree(ct).(map[string]any)
		// 决策 #396：运行态查询传有界 ctx（同上）；决策 #432：状态与重启次数**同一次** inspect。
		dctx, dcancel := containerCallCtx()
		x.ctRuntimeView(dctx, name, m)
		dcancel()
		x.structured = m
		return RenderConfigJSON(m) + "\n"
	case "interfaces":
		items := make([]any, 0, len(ct.Interfaces))
		var b strings.Builder
		fmt.Fprintf(&b, "%-10s %-10s %-18s %s\n", "vNIC", "Type", "VirtualSwitch", "Socket")
		for _, nic := range ct.Interfaces {
			items = append(items, anyToTree(nic))
			fmt.Fprintf(&b, "%-10s %-10s %-18s %s\n",
				nic.Name, nic.Type, nic.VirtualSwitch, orchestrator.MemifSocketPath(orchestrator.DefaultMemifDir, name, nic.Name))
		}
		if len(items) == 0 {
			return "（无 vNIC）\n"
		}
		x.structured = map[string]any{"interfaces": items}
		return b.String()
	}
	return fmt.Sprintf("%% 无效命令: show container-functions %s %s（可用：detail|interfaces）\n", name, sub)
}

// ctStateOf 查询容器运行态，未装配/出错时返回 "-"。
func (x *cliExecutor) ctStateOf(ctx context.Context, name string) string {
	if x.ct == nil {
		return "-"
	}
	st, err := x.ct.ContainerState(ctx, name)
	if err != nil || st == "" {
		return "-"
	}
	return st
}

// ctRuntimeView 把容器运行态读数写入详情视图（决策 #432）：`state` 与既有行为逐字一致
// （未装配/查询失败为 "-"），`restart_count` 仅在**取到时**写入——Docker 应答里没有该字段时
// **省略**而不是给 0（「取不到」与「0 次」不是一回事）。
//
// 两个读数来自**同一次** inspect（ContainerStatus），不为读数新增第二次查询。
func (x *cliExecutor) ctRuntimeView(ctx context.Context, name string, row map[string]any) {
	if x.ct == nil {
		row["state"] = "-"
		return
	}
	st, restarts, known, err := x.ct.ContainerStatus(ctx, name)
	if err != nil || st == "" {
		row["state"] = "-"
		return
	}
	row["state"] = st
	if known {
		row["restart_count"] = restarts
	}
}

// execShowImages：列表 / <name> [detail]。读镜像仓库运行态 + 引用计数（FR-CMP-030~033）。
func (x *cliExecutor) execShowImages(args []string) string {
	if x.images == nil {
		return "%% 镜像仓库未接入\n"
	}
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	metas := x.images.List()
	if len(args) == 0 {
		items := make([]any, 0, len(metas))
		var b strings.Builder
		fmt.Fprintf(&b, "%-22s %-16s %-10s %-6s %s\n", "Name", "Type", "Size", "Refs", "State")
		for _, m := range metas {
			rc := images.RefCount(cfg, m.Name)
			items = append(items, imagesRow(m, rc))
			fmt.Fprintf(&b, "%-22s %-16s %-10s %-6d %s\n",
				m.Name, m.Type, humanSize(m.SizeBytes), rc, m.ImportState)
		}
		if len(items) == 0 {
			return "（镜像仓库为空）\n"
		}
		x.structured = map[string]any{"images": items}
		return b.String()
	}

	name := args[0]
	if len(args) >= 2 && args[1] != "detail" {
		return fmt.Sprintf("%% 无效命令: show images %s %s（可用：detail）\n", name, args[1])
	}
	for _, m := range metas {
		if m.Name != name {
			continue
		}
		row := imagesRow(m, images.RefCount(cfg, name))
		x.structured = row
		return RenderConfigJSON(row) + "\n"
	}
	return fmt.Sprintf("%% 镜像 %s 不存在\n", name)
}

// imagesRow 镜像元数据 + 引用计数（与 GET /images/{name} 视图同字段，附录 A #51）。
func imagesRow(m images.Meta, refCount int) map[string]any {
	row, _ := anyToTree(m).(map[string]any)
	if row == nil {
		row = map[string]any{}
	}
	row["ref_count"] = refCount
	return row
}

// execShowVMImageProfiles `show vm-image-profiles [<image>]`（决策 #485）：镜像级启动画像读视图。
// 取 committed 配置（与 REST `GET /vm-image-profiles` **同源**）；给定 <image> 时只列该画像、
// 不存在则如实报错。逐项显示的是**配置值**（不补产品默认）——补默认后的「有效画像」在
// `show virtual-machine-functions <name> detail` 的「有效启动画像」块里，两处口径不同、不混用。
func (x *cliExecutor) execShowVMImageProfiles(args []string) string {
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	profiles := cfg.VMImageProfiles
	if len(args) >= 1 {
		want := args[0]
		found := false
		for _, p := range profiles {
			if p.Image == want {
				profiles = []model.VMImageProfile{p}
				found = true
				break
			}
		}
		if !found {
			// 用字符串拼接而非 Sprintf：Sprintf 会把 `%%` 渲染成单个 `%`（错误前缀就没了）。
			return "%% 镜像画像 " + want + " 不存在\n"
		}
	}
	if len(profiles) == 0 {
		return "（无镜像启动画像）\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-20s %-10s %-22s %-9s %-15s %-11s %-6s %-8s %s\n",
		"镜像", "machine", "cpu", "firmware", "disk-interface", "boot-order", "accel", "console", "extra-args")
	items := make([]any, 0, len(profiles))
	for _, p := range profiles {
		items = append(items, anyToTree(p))
		fmt.Fprintf(&b, "%-20s %-10s %-22s %-9s %-15s %-11s %-6s %-8s %s\n",
			p.Image, orDash(p.Machine), vmProfileCPU(p), orDash(p.Firmware), orDash(p.DiskIface),
			orDash(p.BootOrder), orDash(p.Accel), orDash(p.Console), vmProfileExtraArgs(p.ExtraArgs))
	}
	x.structured = map[string]any{"vm_image_profiles": items}
	return b.String()
}

// vmProfileCPU 画像的 cpu 单元格：mode 或 mode/model（都空时 "-"）。
func vmProfileCPU(p model.VMImageProfile) string {
	switch {
	case p.CPUMode == "" && p.CPUModel == "":
		return "-"
	case p.CPUModel == "":
		return p.CPUMode
	case p.CPUMode == "":
		return p.CPUModel
	}
	return p.CPUMode + "/" + p.CPUModel
}

// extraArgsLedgerNote extra-args 的读视图标注（决策 #485，契约 §4.4 / 用户手册 §9.6）：
// 这些参数是窄自由口，**已绕过资源账本**——产品不再校验其资源含义，故读视图如实标注，
// 免得操作者以为它们仍受账本约束。CLI 两处读视图（`show vm-image-profiles` 表格列与
// VNF 详情的「有效启动画像」块）**共用**本常量，单一事实源。
const extraArgsLedgerNote = "（已绕过资源账本）"

// vmProfileExtraArgs 额外 QEMU 参数单元格：多条以空格连接，有值时标注「已绕过资源账本」，
// 空则「（无）」。CLI 两处读视图共用（见 extraArgsLedgerNote）。
func vmProfileExtraArgs(args []string) string {
	if len(args) == 0 {
		return "（无）"
	}
	return strings.Join(args, " ") + extraArgsLedgerNote
}

// effectiveStartupBlock `show virtual-machine-functions <name> detail` 的「有效启动画像」块
// （决策 #485）：逐项给出**有效值**并标出**来源**（实例覆盖 > 镜像画像 > 产品默认）。
// 合并规则与 `model.EffectiveStartup` 同源（域映射用的就是它），这里只额外判「谁给了这个值」。
func (x *cliExecutor) effectiveStartupBlock(vm model.VMFunction, profiles []model.VMImageProfile) string {
	var b strings.Builder
	b.WriteString("有效启动画像（合并规则：实例覆盖 > 镜像画像 > 产品默认）:\n")
	for _, f := range effectiveStartupFields(vm, profiles) {
		fmt.Fprintf(&b, "  %-14s %-26s [%s]\n", f.Label, f.Value, f.Source)
	}
	return b.String()
}

// startupFieldView 有效画像的一个字段：显示名 / 有效值 / 来源。
type startupFieldView struct {
	Label  string
	Value  string
	Source string
}

// effectiveStartupFields 逐字段算有效值与来源。来源按**谁提供了该值**判定（与
// `model.EffectiveStartup` 的合并顺序一致）：实例覆盖设了 → 实例覆盖；否则画像设了 →
// 镜像画像；都没设 → 产品默认（`EffectiveStartup` 已把默认填进 Value）。
func effectiveStartupFields(vm model.VMFunction, profiles []model.VMImageProfile) []startupFieldView {
	eff := model.EffectiveStartup(vm, profiles)
	var prof *model.VMImageProfile
	for i := range profiles {
		if profiles[i].Image == vm.Image {
			prof = &profiles[i]
			break
		}
	}
	ov := vm.Startup
	overrideExtraArgs := 0
	if ov != nil {
		overrideExtraArgs = len(ov.ExtraArgs)
	}
	source := func(overrideSet, profileSet bool) string {
		switch {
		case overrideSet:
			return "实例覆盖"
		case profileSet:
			return "镜像画像"
		default:
			return "产品默认"
		}
	}
	ovSet := func(f func(o model.VMStartupOverride) string) bool {
		return ov != nil && f(*ov) != ""
	}
	profSet := func(f func(p model.VMImageProfile) string) bool {
		return prof != nil && f(*prof) != ""
	}
	val := func(s string) string {
		if s == "" {
			return "—"
		}
		return s
	}
	// 控制台形态与实际域一致（决策 #485 契约 §4.3）：未显式设定且 serial_console 关闭时
	// 域里没有串口——读视图如实补一句，避免「看着有其实没生效」。
	consoleVal := val(eff.Console)
	if form, explicit := model.EffectiveConsole(vm, profiles); form != ConsoleKindVNC &&
		!explicit && vm.SerialConsole != nil && !*vm.SerialConsole {
		consoleVal += "（serial_console 已禁用，域内无串口）"
	}
	rows := []startupFieldView{
		{"machine", val(eff.Machine), source(ovSet(func(o model.VMStartupOverride) string { return o.Machine }),
			profSet(func(p model.VMImageProfile) string { return p.Machine }))},
		{"cpu mode", val(eff.CPUMode), source(ovSet(func(o model.VMStartupOverride) string { return o.CPUMode }),
			profSet(func(p model.VMImageProfile) string { return p.CPUMode }))},
		{"cpu model", val(eff.CPUModel), source(ovSet(func(o model.VMStartupOverride) string { return o.CPUModel }),
			profSet(func(p model.VMImageProfile) string { return p.CPUModel }))},
		{"firmware", val(eff.Firmware), source(ovSet(func(o model.VMStartupOverride) string { return o.Firmware }),
			profSet(func(p model.VMImageProfile) string { return p.Firmware }))},
		{"disk-interface", val(eff.DiskIface), source(ovSet(func(o model.VMStartupOverride) string { return o.DiskIface }),
			profSet(func(p model.VMImageProfile) string { return p.DiskIface }))},
		{"boot-order", val(eff.BootOrder), source(ovSet(func(o model.VMStartupOverride) string { return o.BootOrder }),
			profSet(func(p model.VMImageProfile) string { return p.BootOrder }))},
		{"accel", val(eff.Accel), source(ovSet(func(o model.VMStartupOverride) string { return o.Accel }),
			profSet(func(p model.VMImageProfile) string { return p.Accel }))},
		{"console", consoleVal, source(ovSet(func(o model.VMStartupOverride) string { return o.Console }),
			profSet(func(p model.VMImageProfile) string { return p.Console }))},
		{"extra-args", vmProfileExtraArgs(eff.ExtraArgs), source(overrideExtraArgs > 0, prof != nil && len(prof.ExtraArgs) > 0)},
	}
	return rows
}

// execShowResourcePools：show resource-pools（复用 GET /resource-pools 的同一视图函数，
// 避免第二份账本逻辑；FR-SYS-010 的 vpp-reserved 一并展示）。
func (x *cliExecutor) execShowResourcePools() string {
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	view := resourcePoolView(cfg, collectHugepagePools(x.hugepageRoot, cfg))
	x.structured = view
	return renderResourcePools(view)
}

// renderResourcePools 资源池视图的表格渲染（hugepages + cpu 两段）。
//
// 大页段数字取**内核实况**（决策 #480，与 Web「资源池」页同一措辞）：列名点明口径——
// 「已分配（在用）」= 内核在用页数（含数据面固定占用与 VNF/其它持有），
// 「可分配（VNF）」= 内核空闲页数 = VNF 实际可起页数；与 `show system hugepages` 同源，两处数字一致。
// 取不到内核值（-1）如实显示「取不到」（不编造）。CPU 段仍为配置账本口径（不改）。
func renderResourcePools(view map[string]any) string {
	var b strings.Builder
	hugepages, _ := view["hugepages"].([]map[string]any)
	fmt.Fprintf(&b, "Hugepages:\n")
	fmt.Fprintf(&b, "  %-8s %10s %18s %18s %18s\n", "页大小", "声明", "总数（内核实际）", "已分配（在用）", "可分配（VNF）")
	if len(hugepages) == 0 {
		fmt.Fprintf(&b, "  （未配置大页池）\n")
	}
	for _, hp := range hugepages {
		fmt.Fprintf(&b, "  %-8v %10v %18s %18s %18s\n", hp["page_size"], hp["count"],
			poolCell(hp["total"]), poolCell(hp["allocated"]), poolCell(hp["free"]))
	}
	fmt.Fprintf(&b, "  口径：数字取自内核实况（与 show system hugepages 同源）——"+
		"已分配（在用）＝内核在用页数（含数据面固定占用与 VNF/其它持有）；"+
		"可分配（VNF）＝内核空闲页数（VNF 实际可起页数）；取不到内核值显示「取不到」\n")
	cpu, _ := view["cpu"].(map[string]any)
	fmt.Fprintf(&b, "CPU (isolated cores):\n")
	if cpu == nil {
		fmt.Fprintf(&b, "  （未配置隔离核）\n")
		return b.String()
	}
	fmt.Fprintf(&b, "  isolated      : %s\n", coreList(cpu["isolated_cores"]))
	fmt.Fprintf(&b, "  vpp-reserved  : %s\n", coreList(cpu["vpp_reserved"]))
	fmt.Fprintf(&b, "  free          : %s\n", coreList(cpu["free"]))
	alloc, _ := cpu["allocated"].([]map[string]any)
	if len(alloc) == 0 {
		fmt.Fprintf(&b, "  allocated     : （无）\n")
	} else {
		for _, a := range alloc {
			fmt.Fprintf(&b, "  allocated     : %v %s\n", a["vnf"], coreList(a["cores"]))
		}
	}
	return b.String()
}

// poolCell 资源池大页段单元格渲染：-1（取不到内核值）如实显示「取不到」，其余照发
// （与 `show system hugepages` 的 hugepageNum 同口径）。
func poolCell(v any) string {
	switch n := v.(type) {
	case int:
		return hugepageNum(n)
	case float64:
		return hugepageNum(int(n))
	}
	return fmt.Sprint(v)
}

// coreList 核列表渲染（[]int → "4,5,6,7"，空 → "-"）。
func coreList(v any) string {
	var xs []int
	switch c := v.(type) {
	case []int:
		xs = c
	case []any:
		for _, e := range c {
			switch n := e.(type) {
			case int:
				xs = append(xs, n)
			case float64:
				xs = append(xs, int(n))
			}
		}
	}
	if len(xs) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(xs))
	for _, n := range xs {
		parts = append(parts, fmt.Sprintf("%d", n))
	}
	return strings.Join(parts, ",")
}

// injectCoresAssigned 往读视图树里注入**运行态派生**的 `vcpu.cores_assigned`（决策 #461）——
// 取自资源池账本（`model.AssignedCores`，与 `show resource-pools` 的「已分配」及域 XML 的
// `vcpupin` 同源；REST 读视图同源）。账本没分到核时**不注入**（界面「—」如实，不编造）。
func injectCoresAssigned(tree map[string]any, cfg model.Config, vmName string) {
	cores := model.AssignedCores(cfg, vmName)
	if len(cores) == 0 {
		return
	}
	if mv, ok := tree["vcpu"].(map[string]any); ok {
		mv["cores_assigned"] = cores
	}
}

// memoryDisplay 内存展示（size_mb + 页大小/backing 摘要）。
func memoryDisplay(m model.VMMemory) string {
	s := fmt.Sprintf("%dMB", m.SizeMB)
	if m.Backing == "normal" {
		return s + "/normal"
	}
	if m.HugepageSize != "" {
		return s + "/" + m.HugepageSize
	}
	return s
}

// serialEnabled 串口是否启用（缺省启用，FR-CMP-014）。
func serialEnabled(vm model.VMFunction) bool {
	return vm.SerialConsole == nil || *vm.SerialConsole
}

// nicSocketOrVF vNIC 的 socket 路径或 VF 绑定展示（vhost-user / sriov-vf / memif）。
// 内核数据面下 vhost-user 声明的 vNIC 由 libvirt 以 virtio + vhost-net + 宿主 tap 承载
// （没有 VPP，也就没有 vhost-user socket）——按数据面如实说明，不给出一条数据面里
// 并不存在的 socket 路径。
func (x *cliExecutor) nicSocketOrVF(vmName string, nic model.VnfInterface) string {
	switch {
	case nic.Type == "vhost-user":
		if x.dpMode() == model.DataPlaneKernel {
			return "（内核数据面：libvirt 自建 vhost-net tap，无 vhost-user socket）"
		}
		return orchestrator.VnfSocketPath(orchestrator.DefaultVhostDir, vmName, nic.Name)
	case nic.Sriov != nil:
		return fmt.Sprintf("%s vf%d", nic.Sriov.PhysicalInterface, nic.Sriov.VFID)
	case nic.Type == "memif":
		return orchestrator.MemifSocketPath(orchestrator.DefaultMemifDir, vmName, nic.Name)
	}
	return "-"
}

// humanSize 人类可读体积（IEC，1 位小数）。
func humanSize(n int64) string {
	if n <= 0 {
		return "-"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
