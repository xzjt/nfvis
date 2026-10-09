package netkernel

import (
	"context"
	"fmt"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// 本文件把内核数据面接到「提交编排 + 恢复巡检 + 运行态读视图」所需的**同一套**方法面上
// （VPP 实现在 internal/orchestrator/network 的 *L2Network 上）。装配处因此无需按数据面
// 分叉：换的是实现，不是调用方。
//
// 口径（v3 决策 #404）：内核数据面**不维护进程内登记**，内核即事实源——故 VPP 侧的
// 「登记失效」「残渣对账」「延后删表复核」等维护动作在内核侧是**无对象可做的空操作**；
// 读视图能给出内核等价物的（MAC 表、路由、bridge 成员、接口状态、端口清单、DHCP 租约——
// 决策 #438 复用与 VPP 侧同一份服务器核心）给出真值，VPP 专有的（LLDP 邻居、NAT 会话、
// 风暴/端口安全实况）**如实报不可用**。

// SetSocketDirs 内核数据面不用 socket 目录（vNIC 走 virtio + tap，容器 vNIC 未支持）。
func (p *Provider) SetSocketDirs(string, string) {}

// InvalidateRuntimeState 无进程内登记可失效（内核是事实源，读视图直查内核）。
func (p *Provider) InvalidateRuntimeState() {}

// RetryDeferredVRFDeletes 无「延后删表」语义（内核 VRF 删除即时生效），无对象可复核。
func (p *Provider) RetryDeferredVRFDeletes(context.Context, model.Config) []string { return nil }

// ReconcileResidue / ReconcileRecoveryAlarms / ReconcileStorm 均为
// VPP 侧登记型对账；内核数据面无登记、无对应族，空操作。
// ReconcileDHCPServer 不在此列：内核侧有真实现（单播 socket 对账 + 复用的服务器核心巡检，
// 见 dhcpserver.go 的同名方法）。
func (p *Provider) ReconcileResidue(ctx context.Context, cfg model.Config) []error {
	// 内核数据面下本方法承担的不是"残渣对账"（那是 VPP 侧的登记型语义），而是**转发前置条件**
	// 的周期性对账：数据面设备集合随提交变化，放行链要跟着重建；转发开关也可能被宿主改掉。
	if err := p.EnsureForwarding(ctx, cfg); err != nil {
		return []error{err}
	}
	return nil
}
func (p *Provider) ReconcileRecoveryAlarms(context.Context, model.Config) []error { return nil }

// ReconcileProxy 内核数据面下没有 VPP 的 dhcp proxy，但有**同一族**的用户态中继实例需要周期性
// 对账（决策 #437）：声明了却没在跑的实例补启（启动失败、运行期收包失败被停等都能自愈）、
// 已不声明的停掉（交换机删除走 DeleteBridgeDomain，这里兜底）。与 VPP 侧
// network.L2Network.ReconcileProxy 的语义同构、方法名同源。
//
// 失败**如实进未收敛项**：返回错误（15s 巡检日志）之外，按 EnsureConsistent 的同一
// scope/code/source 建/消该交换机的告警——`show alarms` 事后可查；下一轮成功即自动消解。
func (p *Provider) ReconcileProxy(ctx context.Context, cfg model.Config) []error {
	var errs []error
	for _, vs := range cfg.VirtualSwitches {
		if vs.Type == "l3" || vs.DhcpRelayServer == "" {
			continue
		}
		src := "virtual-switches/" + vs.Name + "/dhcp-relay"
		err := p.relayMgr().Sync(ctx, vs)
		if err == nil {
			if p.alarms != nil {
				p.alarms.Resolve(alarmScopeRecovery, network.AlarmUnconverged, src)
			}
			continue
		}
		errs = append(errs, err)
		if p.alarms != nil {
			p.alarms.Raise(alarmScopeRecovery, network.SeverityWarning, network.AlarmUnconverged, err.Error(), src)
		}
	}
	errs = append(errs, p.relayMgr().StopUndeclared(cfg)...)
	return errs
}

func (p *Provider) ReconcileStorm(context.Context, model.Config) []error { return nil }

// CheckVnfPorts vNIC 断连检测：内核数据面下宿主 tap 由 libvirt 创建并挂 bridge，
// 产品侧没有可靠的 tap 名映射，故不做判定（如实不报，避免误报）。
func (p *Provider) CheckVnfPorts(context.Context, model.Config) []error { return nil }

// CheckLoop L2 环路检测：VPP 侧的分类表判据在内核数据面无对应物，不做判定（如实不报）。
func (p *Provider) CheckLoop(context.Context, model.Config) []error { return nil }

// CheckInterfaceLinks 物理业务口链路状态检查（内核数据面：直读 netdev operstate）。
//
// 语义与 VPP 侧同一口径：只检查**已声明且未显式禁用**的物理口；admin/link 任一未起即
// warning 告警（`INTERFACE_LINK_DOWN`），恢复 up 后自动消警；口不在内核里则跳过
// （缺口由恢复收敛的未收敛告警负责，不重复报）。R2-6：此前内核侧只返回错误、无告警落点。
func (p *Provider) CheckInterfaceLinks(ctx context.Context, cfg model.Config) []error {
	states, err := p.rt().InterfaceStates(ctx)
	if err != nil {
		return []error{fmt.Errorf("读取接口状态失败: %w", err)}
	}
	var errs []error
	expect := map[string]bool{}
	for _, iface := range cfg.Interfaces {
		if iface.Enabled != nil && !*iface.Enabled {
			continue // 显式禁用：用户意图，不告警
		}
		st, ok := states[iface.Name]
		if !ok {
			continue
		}
		expect[iface.Name] = true
		if st.AdminUp && st.LinkUp {
			if p.alarms != nil {
				p.alarms.Resolve(alarmScopeIfaceLink, network.AlarmIfaceLinkDown, iface.Name)
			}
			continue
		}
		reason := "链路 down（对端/网线/交换机端口）"
		switch {
		case !st.AdminUp && !st.LinkUp:
			reason = "管理态未启用且链路 down"
		case !st.AdminUp:
			reason = "管理态未启用（set interfaces " + iface.Name + " disable 或下发未生效）"
		}
		errs = append(errs, fmt.Errorf("接口 %s 未就绪：%s", iface.Name, reason))
		if p.alarms != nil {
			p.alarms.Raise(alarmScopeIfaceLink, network.SeverityWarning, network.AlarmIfaceLinkDown,
				fmt.Sprintf("物理口 %s 未就绪：%s", iface.Name, reason), iface.Name)
		}
	}
	// 对账清警：口已从配置删除/显式禁用时，其滞留告警一并消解（round86 口径）。
	if p.alarms != nil {
		orchestrator.ResolveStale(p.alarms, alarmScopeIfaceLink, expect)
	}
	return errs
}

// rt 惰性构造运行态读物（复用同一个 Runner，便于测试注入）。
func (p *Provider) rt() *Runtime { return NewRuntime(p.run) }

// MACTable 一台 L2 交换机（内核 bridge）的 MAC 表。
func (p *Provider) MACTable(ctx context.Context, name string) ([]network.MACTableEntry, error) {
	rows, err := p.rt().MACTable(ctx, name)
	if err != nil {
		return nil, err
	}
	out := make([]network.MACTableEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, network.MACTableEntry{MAC: r.MAC, Port: r.Port, VLAN: r.VLAN})
	}
	return out, nil
}

// Routes 一台 L3 交换机（内核 VRF 表）的静态路由。
func (p *Provider) Routes(ctx context.Context, name string) ([]network.RouteEntry, error) {
	rows, err := p.rt().Routes(ctx, name)
	if err != nil {
		return nil, err
	}
	out := make([]network.RouteEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, network.RouteEntry{Prefix: r.Prefix, NextHop: r.NextHop, Distance: r.Distance})
	}
	return out, nil
}

// BridgeDomains 全部**配置声明**的 L2 交换机的运行态（含成员口；按内核实况判定存在性）。
func (p *Provider) BridgeDomains() ([]network.BDRuntime, error) {
	bds, err := p.rt().BridgeDomains(context.Background(), p.config())
	if err != nil {
		return nil, err
	}
	out := make([]network.BDRuntime, 0, len(bds))
	for _, bd := range bds {
		// Learn/Flood 是 VPP bridge-domain 的概念；内核 bridge 默认就学习并洪泛未知单播/广播，
		// 故如实填 true——**不能**留零值：读视图会把 false 渲染成「down」，看起来像交换机坏了
		// （真机走查实测：`show virtual-switches` 的 Learn/Flood 两列显示 down）。
		// 端口安全关掉的是**单个端口**的学习（per-port learning off），不影响交换机级这一列。
		st := network.BDRuntime{ID: bd.ID, Name: bd.Name, Learn: true, Flood: true}
		for _, port := range bd.Ports {
			// 决策 #438：内置 DHCP tap（bridge 成员口，名形如 nfvisdh+8 位十六进制）不进用户
			// 端口视图——它与 VPP 侧按 sw_if_index 过滤的内置 tap 同一语义（用户不可见/不可删）；
			// 内核侧端口清单是**按名**枚举的，故这里按名过滤（严格字符集，不误伤用户接口）。
			if isProductDHCPTapName(port.Name) {
				continue
			}
			st.Ports = append(st.Ports, network.BDRuntimePort{Name: port.Name, Shg: port.Shg})
		}
		out = append(out, st)
	}
	return out, nil
}

// InterfaceStates 全部内核接口的运行态。
func (p *Provider) InterfaceStates() (map[string]network.SwIfInfo, error) {
	states, err := p.rt().InterfaceStates(context.Background())
	if err != nil {
		return nil, err
	}
	out := make(map[string]network.SwIfInfo, len(states))
	for name, st := range states {
		out[name] = network.SwIfInfo{
			Name: name, AdminUp: st.AdminUp, LinkUp: st.LinkUp,
			LinkSpeed: st.LinkSpeed, DevType: st.DevType, Mtu: st.MTU,
		}
	}
	return out, nil
}

// VPPIfnames 内核数据面下「已交数据面的口」＝产品按配置自持的虚拟设备
// （bridge/VRF/vxlan/bond/vlan 子接口）。
//
// 与 VPP 侧同名方法的语义对齐（返回数据面端口名），只是来源从 VPP dump 换成内核设备。
// 判据取**配置声明集合**而不是设备类型：宿主机上 virbr0/docker0 也是 bridge，
// 按类型选会把它们当成产品端口。
func (p *Provider) VPPIfnames() ([]string, error) {
	return p.rt().DataplaneIfnames(context.Background(), p.config())
}

// KernelIfnames 内核侧物理口名（与 VPP 侧同一份 sysfs 口径）。
func (p *Provider) KernelIfnames() ([]string, error) { return network.KernelIfnamesAll() }

// KernelIfFacts 内核侧物理口事实（与 VPP 侧同一份 sysfs 口径）。
func (p *Provider) KernelIfFacts() ([]network.KernelIfFacts, error) {
	return network.KernelIfFactsAll()
}

// LldpNeighbors LLDP 邻居：内核数据面尚未实现（如实报不可用，不返回空表冒充「无邻居」）。
func (p *Provider) LldpNeighbors(context.Context) ([]network.LldpNeighbor, error) {
	return nil, unsupported("LLDP 邻居")
}

// NATSessions NAT 会话：内核数据面的会话表在 conntrack，尚未接入读视图（如实报不可用）。
func (p *Provider) NATSessions(context.Context) ([]network.NATSession, error) {
	return nil, unsupported("NAT 会话表")
}

// VxlanStates VXLAN 运行态：内核 vxlan 设备的存量由 EnsureConsistent 的声明重放保证，
// 读视图尚未接入（如实报不可用，避免把「查不到」显示成「不存在」）。
func (p *Provider) VxlanStates(context.Context) (map[string]network.VxlanState, error) {
	return nil, unsupported("VXLAN 运行态")
}

// DHCPServerLeases / DHCPServerActiveLeases / DHCPTapIndexes：DHCP 服务器的读视图（决策 #438）。
//
// 内核数据面下这三处**不是**「未接入的空实现」——它们接到与 VPP 侧**同一份**服务器核心
// （network.DHCPServerProvider）的运行态上：
//   - 租约表/生效租约数：进程内状态，与传输面无关（ok=false = 该交换机尚未收敛）；
//   - TapIndexes：产品自持的内置 tap 的内核 ifindex 集合（端口读视图按它过滤；内核侧端口
//     视图是**按名**枚举的，故另有按名过滤，见 BridgeDomains）。
func (p *Provider) DHCPServerLeases(name string) ([]network.DHCPLease, bool) {
	srv, _ := p.dhcpComponents()
	return srv.Leases(name)
}
func (p *Provider) DHCPServerActiveLeases(name string) (int, bool) {
	srv, _ := p.dhcpComponents()
	return srv.ActiveLeases(name)
}
func (p *Provider) DHCPTapIndexes() map[uint32]bool {
	srv, _ := p.dhcpComponents()
	return srv.TapIndexes()
}

// 编译期断言：内核数据面满足装配层使用的完整方法面。
var _ interface {
	orchestrator.NetworkProvider
	SetSocketDirs(string, string)
	InvalidateRuntimeState()
	RetryDeferredVRFDeletes(context.Context, model.Config) []string
	CheckVnfPorts(context.Context, model.Config) []error
	CheckInterfaceLinks(context.Context, model.Config) []error
	ReconcileResidue(context.Context, model.Config) []error
	ReconcileRecoveryAlarms(context.Context, model.Config) []error
	ReconcileDHCPServer(context.Context, model.Config) []error
	ReconcileProxy(context.Context, model.Config) []error
	ReconcileStorm(context.Context, model.Config) []error
	CheckLoop(context.Context, model.Config) []error
	MACTable(context.Context, string) ([]network.MACTableEntry, error)
	Routes(context.Context, string) ([]network.RouteEntry, error)
	BridgeDomains() ([]network.BDRuntime, error)
	InterfaceStates() (map[string]network.SwIfInfo, error)
	VPPIfnames() ([]string, error)
	KernelIfnames() ([]string, error)
	KernelIfFacts() ([]network.KernelIfFacts, error)
	LldpNeighbors(context.Context) ([]network.LldpNeighbor, error)
	NATSessions(context.Context) ([]network.NATSession, error)
	VxlanStates(context.Context) (map[string]network.VxlanState, error)
	DHCPServerLeases(string) ([]network.DHCPLease, bool)
	DHCPServerActiveLeases(string) (int, bool)
	DHCPTapIndexes() map[uint32]bool
	StormDataplane(context.Context, string) (network.StormDataplane, bool)
	PortSecDataplane(context.Context, string) (network.PortSecDataplane, bool)
} = (*Provider)(nil)
