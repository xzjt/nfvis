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
			row, _ := anyToTree(vm).(map[string]any)
			row["state"] = st
			items = append(items, row)
			fmt.Fprintf(&b, "%-16s %-10s %-5d %-12s %-16s %v\n",
				vm.Name, st, vm.VCPU.Count, memoryDisplay(vm.Memory), vm.Image, serialEnabled(vm))
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
		m, _ := anyToTree(vm).(map[string]any)
		m["state"] = x.vmStateOf(context.Background(), name)
		x.structured = m
		return RenderConfigJSON(m) + "\n"
	case "interfaces":
		items := make([]any, 0, len(vm.Interfaces))
		var b strings.Builder
		fmt.Fprintf(&b, "%-10s %-12s %-18s %-18s %s\n", "vNIC", "Type", "VirtualSwitch", "MAC", "Socket/VF")
		for _, nic := range vm.Interfaces {
			items = append(items, anyToTree(nic))
			fmt.Fprintf(&b, "%-10s %-12s %-18s %-18s %s\n",
				nic.Name, nic.Type, nic.VirtualSwitch, nic.MAC, nicSocketOrVF(name, nic))
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
		items := make([]any, 0, len(cfg.ContainerFunctions))
		var b strings.Builder
		fmt.Fprintf(&b, "%-16s %-10s %-6s %-16s %s\n", "Name", "State", "vCPU", "Image", "Restart")
		for _, ct := range cfg.ContainerFunctions {
			st := x.ctStateOf(context.Background(), ct.Name)
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
		m["state"] = x.ctStateOf(context.Background(), name)
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

// execShowResourcePools：show resource-pools（复用 GET /resource-pools 的同一视图函数，
// 避免第二份账本逻辑；FR-SYS-010 的 vpp-reserved 一并展示）。
func (x *cliExecutor) execShowResourcePools() string {
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	view := resourcePoolView(cfg)
	x.structured = view
	return renderResourcePools(view)
}

// renderResourcePools 资源池视图的表格渲染（hugepages + cpu 两段）。
func renderResourcePools(view map[string]any) string {
	var b strings.Builder
	hugepages, _ := view["hugepages"].([]map[string]any)
	fmt.Fprintf(&b, "Hugepages:\n")
	fmt.Fprintf(&b, "  %-10s %8s %10s %8s\n", "PageSize", "Total", "Allocated", "Free")
	if len(hugepages) == 0 {
		fmt.Fprintf(&b, "  （未配置大页池）\n")
	}
	for _, hp := range hugepages {
		fmt.Fprintf(&b, "  %-10v %8v %10v %8v\n", hp["page_size"], hp["total"], hp["allocated"], hp["free"])
	}
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
func nicSocketOrVF(vmName string, nic model.VnfInterface) string {
	switch {
	case nic.Type == "vhost-user":
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
