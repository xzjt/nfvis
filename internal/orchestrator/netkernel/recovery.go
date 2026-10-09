package netkernel

import (
	"context"
	"fmt"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// EnsureConsistent 恢复收敛：把 committed 配置按**依赖序**重放到内核。
//
// 与 VPP 实现的差别（如实说明）：内核是唯一事实源，本实现**不做「对比数据面实况再补差」**，
// 而是把声明式下发整体重放一遍——所有下发方法本身幂等（`ip link add` 容忍已存在、
// `ip addr add`/`ip route replace`/`nft flush+重建` 幂等），重放的结果与内核实况一致；
// nfvisd 重启后读视图直接查内核，也不需要靠重放重建进程内登记。
//
// 段序（R2-5，与提交编排 plan 的依赖序一致）：**转发前置 → 绑定族 → bond → 交换机 →
// 接口 → 镜像 → VRF → vxlan → dns-proxy → NAT**。旧段序把 interfaces/bonds 排在 virtual-switches 之前：
// 端口安全要求口已是 bridge 成员（否则如实拒绝），主机重启后必然失败、白名单静默不下发；
// 镜像源可以是 bond，同理要在 bonds 之后。症状是「重启后没了、再提交一次又好了」。
//
// 返回不可收敛项（逐条带对象路径），同时按 VPP 侧同码族落告警（R2-6：此前只进 journal）。
func (p *Provider) EnsureConsistent(ctx context.Context, cfg model.Config) []error {
	p.SetConfig(cfg)
	var errs []error
	var failures []network.Alarm
	// 转发前置条件（IPv4/IPv6 转发开关 + 数据面之间的 forward 放行）先于一切下发：
	// 内核数据面下产品自己就是那台路由器，这两条不成立时所有"下发成功"都换不来一个转发的包。
	collect := func(path string, err error) {
		if err == nil {
			return
		}
		errs = append(errs, fmt.Errorf("%s: %w", path, err))
		// 与 VPP 侧同码族：配置引用的口不存在是不可收敛（error），其余按暂时性未收敛（warning）。
		code, sev := network.AlarmUnconverged, network.SeverityWarning
		if notFound(err.Error(), err) {
			code, sev = network.AlarmIfaceMissing, network.SeverityError
		}
		failures = append(failures, network.Alarm{
			Severity: sev, Code: code, Message: err.Error(), Source: path,
		})
	}
	collect("forwarding", p.EnsureForwarding(ctx, cfg))
	// 绑定族对象先于接口/三层接口重放：接口声明里只写策略名，解析需要策略本体
	// （与提交编排 plan 的段序一致：ACL → QoS → 接口）。
	for _, acl := range cfg.Acls {
		collect("acls/"+acl.Name, p.ApplyACL(ctx, acl))
	}
	for _, q := range cfg.QosPolicies {
		collect("qos/"+q.Name, p.ApplyQos(ctx, q))
	}
	// bond 先于交换机/接口：成员口要（可能）先聚合成 LAG，镜像源也可能是 bond。
	for _, bond := range cfg.Bonds {
		collect("bonds/"+bond.Name, p.ApplyBond(ctx, bond))
	}
	// 交换机先于接口：接口层的端口安全要求该口已是 bridge 成员（R2-5）。
	for _, vs := range cfg.VirtualSwitches {
		collect("virtual-switches/"+vs.Name, p.ApplyBridgeDomain(ctx, vs))
	}
	// 决策 #437：DHCP 中继用户态实例的恢复重放——**恢复重放必须含 relay**（与 VPP 侧同纪律）：
	// 实例活在 nfvisd 进程内，进程重启后必然不在，不重放即静默丢中继。放在交换机段之后：
	// 实例要绑 bridge、要绑 BVI 的 v4 网关地址（ApplyBridgeDomain 已把地址落下）。声明未变时
	// Sync 幂等；起不来按未收敛项落告警（如实，不静默）。
	for _, vs := range cfg.VirtualSwitches {
		if vs.Type == "l3" || vs.DhcpRelayServer == "" {
			continue
		}
		collect("virtual-switches/"+vs.Name+"/dhcp-relay", p.ApplyDhcpRelay(ctx, vs))
	}
	// 决策 #438：DHCP 服务器的恢复重放——**恢复重放必须含它**（与 VPP 侧同纪律）：
	// 单播接收 socket 活在 nfvisd 进程内（进程重启后必然不在），内置 tap 是内核对象（按名复用，
	// 带外删了就重建）。放在交换机段之后：socket 要绑 bridge 的 BVI 地址、tap 要 enslave 到
	// 该 bridge（ApplyBridgeDomain 已把地址与 bridge 落下）。声明未变时 Sync 幂等；起不来
	// 按未收敛项落告警（如实，不静默）。
	for _, vs := range cfg.VirtualSwitches {
		if vs.Type == "l3" || !vs.DHCPServerEnabled() {
			continue
		}
		collect("virtual-switches/"+vs.Name+"/dhcp-server", p.ApplyDHCPServer(ctx, vs))
	}
	for _, iface := range cfg.Interfaces {
		collect("interfaces/"+iface.Name, p.enrichHeldPortErr(iface.Name, p.ApplyInterface(ctx, iface)))
	}
	// 镜像在交换机/接口之后：源口（物理口/bond）此刻已存在且已 up（ApplySpan 会置分析口 up）。
	for _, pm := range cfg.PortMirroring {
		collect("port-mirroring/"+pm.Name, p.ApplySpan(ctx, pm))
	}
	for _, vrf := range cfg.Vrfs {
		collect("vrfs/"+vrf.Name, p.ApplyVRF(ctx, vrf))
	}
	for _, vx := range cfg.VxlanTunnels {
		collect("vxlan/"+vx.Name, p.ApplyVxlan(ctx, vx, nil))
	}
	// 决策 #439：数据面 DNS 代理的恢复重放——**恢复重放必须含它**：域落点 socket 活在 nfvisd
	// 进程内（进程重启后必然不在），不重放即静默丢域内解析。放在交换机/VRF 段之后：落点地址
	// （L2 的 BVI 网关 / L3 的 l3-interface）必须已下发，socket 才绑得上；声明为空＝teardown
	// （Sync 关掉全部 socket，幂等）。
	collect("dns-proxy", p.ApplyDNSProxy(ctx, orchestrator.DNSProxyUpstreamsOf(cfg)))
	// NAT：声明为空时也调用一次——ApplyNAT 对空声明做的是**回收整张表**（不是留空表），
	// 这样 `delete nat` 之后残留的空表会被下一次收敛清掉。它要所有设备/VRF 先就位。
	natCfg := model.NatConfig{}
	if cfg.Nat != nil {
		natCfg = *cfg.Nat
	}
	collect("nat", p.ApplyNAT(ctx, natCfg))
	// 告警落点（R2-6）：与 VPP 侧同一 scope/码族；本次收敛成功的项由 Sync 自动消解
	// （不在 failures 里的活动告警一律 resolved，跨重启按内核实况重建）。
	if p.alarms != nil {
		p.alarms.Sync(alarmScopeRecovery, failures)
	}
	return errs
}

// enrichHeldPortErr 给「口在内核里不存在」的未收敛项点名真正的持有者（决策 #426③）。
//
// 由来（真机走查）：数据面切到内核后，VPP 时代接管过的口仍留在 vfio-pci——内核里没有
// 它的 netdev，`ApplyInterface` 报底座的 `Cannot find device`，于是恢复收敛只留下这一句，
// 操作者看不出「它在 DPDK 手里，先交还内核」。这里按既有告警口径（同一 code/source，
// 不新造告警码）把探测到的驱动名与 PCI 补进文案，并给照做路径。
//
// 只对**设备不存在**类错误附加（其它失败原因原样如实上报，不猜测）；探测取不到原样返回。
func (p *Provider) enrichHeldPortErr(ifname string, err error) error {
	if err == nil || p.heldPort == nil {
		return err
	}
	if !notFound(err.Error(), err) {
		return err
	}
	driver, pci, ok := p.heldPort(ifname)
	if !ok || driver == "" {
		return err
	}
	where := ""
	if pci != "" {
		where = "（PCI " + pci + "）"
	}
	return fmt.Errorf("%w；该口当前仍绑定在 %s 驱动上%s、内核里没有它——改用内核数据面请先交还："+
		"request interfaces %s unbind-dpdk --yes（内核未自动重新探测原生驱动时，按其提示补 to-driver <驱动名>）",
		err, driver, where, ifname)
}
