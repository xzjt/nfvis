package api

// 决策 #326（收口 R84-16 / v2 待做 二.8）：`show virtual-switches <n> ports|statistics` 的
// 端口读视图。与 REST `GET /virtual-switches/{name}/ports` **同源**（都调
// model.DerivedSwitchPorts）：配置里静态声明的 `ports`（source=config）与 VNF/容器声明
// （`interfaces <nic> virtual-switch <vs>`）派生出的 vNIC 成员（source=vnf|container）并集，
// 逐条标注来源；运行态列（管理/链路/收发）按端口名尽力合并，取不到给 "-"（不编造）。
//
// 为什么不像 #84 那样只取 VPP 运行态：VNF/容器声明的 vNIC 是**配置事实**（是否在线是另一
// 个事实）——只取运行态时，VNF 未启动/数据面未连接就完全看不到这台交换机上「声明了什么」，
// 而操作者要看的正是这个。运行态成员仍可在 `show virtual-switches <name>`（detail，运行态
// 叠加）与列表里看到；本视图不丢信息。
//
// 交换机**未在 committed 声明**时退回运行态 BD 成员视图（保持 #84 的「未在配置也能看到」
// 语义，语义校验 S3/S4 依赖它）。

import (
	"context"
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// showVSwitchPorts 交换机成员端口读视图（决策 #326）。未在配置中声明时退回运行态视图。
func (x *cliExecutor) showVSwitchPorts(name string) string {
	if cfg, err := x.engine.Committed(); err == nil {
		for _, vs := range cfg.VirtualSwitches {
			if vs.Name == name {
				return x.renderSwitchPortView(cfg, name, x.runtimeBDPortNames(name))
			}
		}
	}
	return x.showRuntimeSwitchPorts(name)
}

// switchPortViews 端口读视图：配置派生并集（config/vnf/container）在前，随后补上
// **仅在运行态存在、配置未声明**的成员（source=runtime，保留 #84 的「VPP 里有的也看得见」）。
// 按展示名去重：静态口与运行态同名（如 ens224）只出配置那一条（带 source=config）。
func switchPortViews(cfg model.Config, name string, runtimePorts []string) []model.SwitchPortView {
	rows := model.DerivedSwitchPorts(cfg, name)
	seen := map[string]bool{}
	for _, r := range rows {
		seen[switchPortLabel(r)] = true
	}
	for _, rp := range runtimePorts {
		if rp == "" || seen[rp] {
			continue
		}
		seen[rp] = true
		rows = append(rows, model.SwitchPortView{Source: model.PortSourceRuntime, Port: rp, Interface: rp})
	}
	return rows
}

// runtimeBDPortNames 运行态该 BD 的成员口名（运行态不可用/该 BD 不在数据面时返回 nil）。
// 决策 #359：内置 DHCP tap 按 sw_if_index 过滤（用户不可见/不可删，不用名字匹配）。
func (x *cliExecutor) runtimeBDPortNames(name string) []string {
	bds, err := x.bdStates()
	if err != nil {
		return nil
	}
	var taps map[uint32]bool
	if x.dhcpSrv != nil {
		taps = x.dhcpSrv.DHCPTapIndexes()
	}
	for _, bd := range bds {
		if bd.Name != name {
			continue
		}
		out := make([]string, 0, len(bd.Ports))
		for _, p := range bd.Ports {
			if taps[p.SwIfIndex] {
				continue
			}
			out = append(out, p.Name)
		}
		return out
	}
	return nil
}

// renderSwitchPortView 渲染派生并集（配置 + VNF/容器声明 + 运行态成员）。
func (x *cliExecutor) renderSwitchPortView(cfg model.Config, name string, runtimePorts []string) string {
	rows := switchPortViews(cfg, name, runtimePorts)
	var b strings.Builder
	// 表头恒打印（即便无端口）：契约 §1.1「成员端口及状态/计数」的列名要始终可见，
	// 语义校验 S4 亦据此判定「是否含状态/计数字段」。
	fmt.Fprintf(&b, "%-20s %-9s %-7s %-7s %-12s %-12s\n", "Port", "Source", "Admin", "Link", "RxPkts", "TxPkts")
	states, _ := x.ifaceStates()
	items := make([]any, 0, len(rows))
	for _, p := range rows {
		label := switchPortLabel(p)
		row := map[string]any{
			"port": label, "source": p.Source,
			"interface": p.Interface, "vnf": p.VNF, "vnf_interface": p.VNFInterface,
			"container": p.Container, "container_interface": p.ContainerInterface,
		}
		admin, link, rx, tx := "-", "-", "-", "-"
		if st, ok := states[label]; ok {
			admin, link = yn(st.AdminUp), yn(st.LinkUp)
			row["admin"], row["link"] = st.AdminUp, st.LinkUp
		}
		if x.state != nil {
			if c, ok := x.state.InterfaceCounters(context.Background(), label); ok {
				rx, tx = fmt.Sprintf("%d", c.RxPackets), fmt.Sprintf("%d", c.TxPackets)
				row["rx_packets"], row["tx_packets"] = c.RxPackets, c.TxPackets
			}
		}
		fmt.Fprintf(&b, "%-20s %-9s %-7s %-7s %-12s %-12s\n", label, p.Source, admin, link, rx, tx)
		items = append(items, row)
	}
	if len(rows) == 0 {
		b.WriteString("（该交换机无成员端口——配置未声明 ports，也无 VNF/容器 vNIC 声明挂到它）\n")
	} else {
		// 来源说明：派生条目只读，删除要指向声明它的那一侧（决策 #326 的边界）。
		for _, p := range rows {
			if p.Source == model.PortSourceVNF {
				fmt.Fprintf(&b, "（来源 vnf: %s/%s 只读——在 VNF 侧删除：delete virtual-machine-functions %s interfaces %s virtual-switch %s）\n",
					p.VNF, p.VNFInterface, p.VNF, p.VNFInterface, name)
			}
			if p.Source == model.PortSourceContainer {
				fmt.Fprintf(&b, "（来源 container: %s/%s 只读——在容器侧删除：delete container-functions %s interfaces %s virtual-switch %s）\n",
					p.Container, p.ContainerInterface, p.Container, p.ContainerInterface, name)
			}
		}
	}
	x.structured = map[string]any{"name": name, "ports": items}
	return b.String()
}

// switchPortLabel 端口展示名：server 已在读视图条目里填好 VPP 侧接口名（model.DerivedSwitchPorts
// 用与建接口侧同源的命名规则算出），此处直接用；SR-IOV VF 等无 VPP 名时退回 `<owner>/<nic>`。
func switchPortLabel(p model.SwitchPortView) string {
	if p.Port != "" {
		return p.Port
	}
	switch {
	case p.Interface != "":
		return p.Interface
	case p.VNF != "":
		return p.VNF + "/" + p.VNFInterface
	case p.Container != "":
		return p.Container + "/" + p.ContainerInterface
	}
	return "-"
}

// showRuntimeSwitchPorts 运行态 BD 成员视图（交换机未在配置中声明时的回落，行为与 #84 一致）。
func (x *cliExecutor) showRuntimeSwitchPorts(name string) string {
	bds, err := x.bdStates()
	if err != nil {
		return fmt.Sprintf("%% 虚拟交换机运行态不可用: %v\n", err)
	}
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
	var b strings.Builder
	fmt.Fprintf(&b, "%-16s %-7s %-7s %-12s %-12s %s\n", "Port", "Admin", "Link", "RxPkts", "TxPkts", "Shg")
	items := make([]any, 0, len(bd.Ports))
	states, _ := x.ifaceStates()
	var taps map[uint32]bool
	if x.dhcpSrv != nil {
		taps = x.dhcpSrv.DHCPTapIndexes() // 决策 #359：内置 DHCP tap 不进用户端口视图
	}
	shown := 0
	for _, p := range bd.Ports {
		if taps[p.SwIfIndex] {
			continue
		}
		shown++
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
	if shown == 0 {
		b.WriteString("（该 BD 无成员口）\n")
	}
	x.structured = map[string]any{"bd_id": bd.ID, "name": bd.Name, "ports": items}
	return b.String()
}
