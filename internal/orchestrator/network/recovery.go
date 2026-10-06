package network

// M3-8：恢复收敛（FR-OPS-010/011）。
//
// 启动（首次连上 VPP）与 VPP 重启重连后，把 committed 配置逐对象重放到 VPP：
// 先清空各 Provider 的进程内登记表，使其按 VPP 实况重新判定对象是否存在并补齐
// （BD 经 bridge_domain_dump、ACL 经 acl_dump 按 tag、bond 经接口名查询），
// 再按事务 apply 的依赖顺序全量收敛。单个对象失败不阻塞其余对象，失败项转告警
// （GET /alarms）；配置引用的物理口被移除等不可收敛项标为 error 级别。
//
// 已知限制（附录 A #35）：govpp v0.13.0 的 sw_interface_details 无 bd_id，无法
// dump BD 成员/BVI 归属，故「nfvisd 进程内已登记」之外的成员关系（如跨进程删除
// 端口、已存在的 BVI）无法从 VPP 侧反查，只做补齐不做摘除；这类残留需删 BD 重建
// 或重启 VPP 消除。

import (
	"context"
	"errors"
	"fmt"

	"github.com/xzjt/nfvis/internal/orchestrator"

	"github.com/xzjt/nfvis/internal/model"
)

// ErrIfaceUnavailable 配置引用的接口在 VPP 中不存在（未由 DPDK 接管或被移除）。
// 属不可收敛项：恢复收敛据此转 error 级告警而非反复重试（FR-OPS-010）。
//
// 与 orchestrator.ErrIfaceUnavailable **是同一个 error 值**（决策 #100）：提交编排
// （orchestrator/apply.go）要识别该状态以决定「延后收敛而非整体回滚」，而依赖方向
// 不允许那里 import 本包。本包保留该名字，使既有调用方（含 API 层）的 errors.Is 判定不变。
var ErrIfaceUnavailable = orchestrator.ErrIfaceUnavailable

// ifaceMissingHint 接口不在数据面时的下一步提示（决策 #100）。
//
// 旧文案「（是否未由 DPDK 接管？）」在**已由 DPDK 接管、但尚未加载进数据面**时指错方向——
// 那恰恰是「声明了 DPDK 端口、等数据面重启」的正常过渡态（发现 #8 真机实测即为此）。
// 改为给出可照做的一步：先重启数据面，仍不行再查接管与命名。
const ifaceMissingHint = "（若该口由 DPDK 接管：重启数据面后才会出现，执行 request vpp restart；" +
	"否则请确认该口已由 DPDK 接管、且名称与数据面中的一致）"

// SetAlarms 注入告警表（恢复收敛的失败项落点）；未注入时仅返回错误列表。
// 同时转发给已装配的 DHCP 服务器 provider（决策 #359：池耗尽告警的落点——
// 两处注入顺序无关，后到者补接）。
func (n *L2Network) SetAlarms(a *AlarmStore) {
	n.alarms = a
	if n.dhcpServer != nil {
		n.dhcpServer.SetAlarms(a)
	}
}

// EnsureConsistent 恢复收敛（FR-OPS-010/011）：把 committed 配置全量重放到 VPP。
// 返回未收敛项（调用方记日志即可，不据此拒绝服务）；不可收敛项同时进入告警表。
func (n *L2Network) EnsureConsistent(ctx context.Context, cfg model.Config) []error {
	if n == nil || n.l2 == nil {
		return nil
	}
	n.resetProviders()

	var (
		errs     []error
		failures []Alarm
	)
	record := func(source string, err error) {
		code, sev := AlarmUnconverged, SeverityWarning
		if errors.Is(err, ErrIfaceUnavailable) {
			code, sev = AlarmIfaceMissing, SeverityError
		}
		errs = append(errs, fmt.Errorf("%s: %w", source, err))
		failures = append(failures, Alarm{Severity: sev, Code: code, Message: err.Error(), Source: source})
	}

	// 依赖顺序（决策 #322，修 R84-22）：**先建 VRF/L3 表与交换机，再置接口/成员**。
	// vNIC 接入重放会把声明的 vNIC 置入其所属 L3 交换机的表（FR-NET-020），而表若尚未建立，
	// VPP 报 `No such FIB / VRF (-3)` 成为未收敛项、要再重放一次才成功（round84 登记）。
	// 这里在重放 vNIC/交换机**之前**按配置预建全部声明表（幂等，复用各 Provider 的建表；
	// 与残留表对账共用 DeclaredTables 一份声明集口径）——首次收敛即成功，不靠「失败再重放」。
	if n.l3 != nil {
		for _, err := range n.l3.PrecreateTables(ctx, cfg) {
			record("ip-tables", err)
		}
	}

	// VNF/容器 vNIC 接入重放（FR-NET-020/022/023）：VPP 重启后 vhost-user/memif 接口
	// 会消失，须先于 BD 重放，交换机端口才能按名挂接。
	// 此处的 L3 置表登记可能失败（同名 Vrf 条目尚未重放、接口带地址不让换表 -114），
	// 故 VRF 落地之后还有一次专门的登记重建（见下方「vNIC 置表登记重建」）。
	ports := orchestrator.VnfPortsOf(cfg, n.vhostDir, n.memifDir)
	for _, port := range ports {
		if err := n.ApplyVnfInterface(ctx, port); err != nil {
			record(fmt.Sprintf("vnf-ports/%s/%s", port.VM, port.Interface), err)
		}
	}

	// 顺序与事务 apply 计划一致：被引用对象先建（ACL/bond → BD/VRF → NAT → SPAN/QoS
	// → LLDP → 接口层），保证策略与地址在接口启用前就绪。
	for _, acl := range cfg.Acls {
		if err := n.ApplyACL(ctx, acl); err != nil {
			record("acls/"+acl.Name, err)
		}
	}
	for _, b := range cfg.Bonds {
		if err := n.ApplyBond(ctx, b); err != nil {
			record("bonds/"+b.Name, err)
		}
	}
	// 声明集之外的 bond 必须拆除（否则残留 BondEthernetX 与成员关系，该物理口既是从属口
	// 又可能是 bridge-domain 成员，且无法原位重新声明为普通口）。
	if n.bond != nil {
		for _, err := range n.bond.PruneBonds(ctx, cfg.Bonds) {
			record("bonds", err)
		}
	}
	// bridge-domain 的成员口 = 交换机侧声明 ∪ VNF/容器侧 vNIC 声明（FR-NET-020~023，决策 #170）：
	// 合流后重放，VNF 侧声明的 vhost-user 口同样会被挂进 BD（否则进程重启后
	// VPP 里那个口就再也回不到 BD 里，guest 静默失去 L2 连通）。
	// 无法归位的声明进未收敛清单，不静默跳过。
	switches, refErrs := orchestrator.SwitchMembersOf(cfg, n.vhostDir, n.memifDir)
	for _, err := range refErrs {
		record("virtual-switches", err)
	}
	for _, vs := range switches {
		if vs.Type != "l2" { // L3 交换机经同名 Vrf 条目编排（附录 B）
			continue
		}
		if err := n.ApplyBridgeDomain(ctx, vs); err != nil {
			record("virtual-switches/"+vs.Name, err)
		}
		// 决策 #337：声明了学习上限的交换机重放同一条 bridge_domain_set_learn_limit——
		// VPP 重启后 BD 与上限一并复位，不重放即静默丢缓解手段（与 L2-2/#335 同族教训）。
		// 独立记源、失败不阻塞其余对象；resetProviders 已清空 L2Provider 登记，声明未变也会重放（幂等）。
		// 只补齐不摘除（附录 A #35）：清配置走提交编排（发默认值），恢复段不猜。
		if n.l2 != nil && vs.LearnLimit > 0 {
			if err := n.l2.ApplyLearnLimit(ctx, vs); err != nil {
				record("virtual-switches/"+vs.Name+"/learn-limit", err)
			}
		}
		// 决策 #335：声明了 relay 的交换机重放同一条 proxy 消息——VPP 重启后 proxy 运行态
		// 消失，不重放即静默丢中继（与 L2-2 同族教训）。独立记源、失败不阻塞其余对象；
		// resetProviders 已清空 DhcpProvider 登记，声明未变也会重新下发（幂等）。
		// 只补齐不摘除（附录 A #35）：清 relay 走提交编排（IsAdd=false），恢复段不猜。
		if n.dhcp != nil && vs.DhcpRelayServer != "" {
			if err := n.dhcp.SyncRelay(ctx, vs); err != nil {
				record("virtual-switches/"+vs.Name+"/dhcp-relay", err)
			}
		}
		// 决策 #359：启用 dhcp-server 的交换机重放整套服务器运行态——VPP 重启后 **tap 与
		// punt 注册全失**（round140 实测），不重放即静默丢域内 DHCP（与 L2-2/#335 同族教训）。
		// Sync 自带幂等：按 HostIfName 找存量 tap、无则建，重入 BD/置 up，**每次都重申 punt
		// 注册**（relay 的 proxy 会夺走 UDP/67——R140-1）；租约表在进程内保留、按文件恢复。
		// 独立记源、失败不阻塞其余对象；未启用的交换机不摘除（只补齐，附录 A #35——
		// 停用走提交编排的 teardown）。放在 ApplyBridgeDomain 之后：tap 要入的 BD 此刻已重建。
		if n.dhcpServer != nil && vs.DHCPServerEnabled() {
			if err := n.dhcpServer.Sync(ctx, vs); err != nil {
				record("virtual-switches/"+vs.Name+"/dhcp-server", err)
			}
		}
	}
	for _, vrf := range cfg.Vrfs {
		if err := n.ApplyVRF(ctx, vrf); err != nil {
			record("vrfs/"+vrf.Name, err)
		}
	}
	// VXLAN 隧道（决策 #383）：VPP 重启后隧道条目全失（接口与 BD 归属一并消失），不重放即
	// 静默丢 overlay（与 L2-2/#335 同族教训）。放在交换机/VRF **之后**：声明了 virtual-switch
	// 的隧道要入该 L2 交换机的 BD，BD 此刻已重建。走 ApplyVxlan 的**按 tag 判存量**路径
	// （prev=nil；不靠进程内登记/接口名——VPP 26.06 的 vxlan dump 恒空），tag 在则不重复建。
	// 只补齐不摘除（附录 A #35）：删隧道走提交编排。
	if n.vxlan != nil {
		for _, t := range cfg.VxlanTunnels {
			if err := n.ApplyVxlan(ctx, t, nil); err != nil {
				record("vxlan-tunnels/"+t.Name, err)
			}
		}
	}
	// L3 侧登记重建：恢复收敛开头已失效全部进程内登记，而 NAT 的 inside/outside 解析只读 L3 侧
	// 三张表（ifaces / ifaceTable / vnfs）——登记缺项会让 NAT 认为「该口不该有特性」而下发删除，
	// 登记整体为空时更会把插件当作「没有 NAT 配置」直接关掉：真机实测 `systemctl restart vpp`
	// 加 `systemctl restart nfvis` 后 `show nat44 ei interfaces` / `show nat44 ei addresses` 全空，
	// 而配置里规则/交换机/地址都在、日志无未收敛项。登记因此必须按**配置**重建，且落在
	// VRF 落地之后（两类登记都要表已存在、接口已解析）、ApplyNAT 之前。
	//
	// 两条来源都要走，缺一都会让 NAT 少一个转发域：
	//   - L3 交换机的 l3_interfaces（含 vlan 子接口）：按名与运行态核对 sw_if_index 后登记，
	//     解析不到按未收敛上报（不静默丢）；
	//   - 声明了 L3 交换机的 vNIC（VNF vhost-user / 容器 memif）：查运行态 → 置表 → 登记，
	//     该口同时进所属 VRF 的 inside 集合（它就是 guest 侧的发包口，见 AttachedIfaces）。
	// 幂等：登记是并集写入（重复收敛不产生重复项），vNIC 已登记且索引未变时为空操作；
	// 只补齐不摘除（附录 A #35）——配置里没有的登记不删，也不做任何运行态对象的摘除。
	if n.l3 != nil {
		for _, vrf := range cfg.Vrfs {
			for _, f := range n.l3.RegisterL3Interfaces(ctx, vrf) {
				record(f.Source, f.Err)
			}
		}
		for _, port := range ports {
			if port.VRF == "" || !n.vnfPortProviderReady(port.Type) {
				continue
			}
			if err := n.l3.SetVnfTable(ctx, port.VRF, VnfPortIfaceName(port)); err != nil {
				record(fmt.Sprintf("vnf-ports/%s/%s", port.VM, port.Interface), err)
			}
		}
	}
	// 数据面 DNS 代理（决策 #345）：VPP 重启后 punt 注册丢失，不重放即静默丢域内 DNS
	// （与 L2-2/#335/#337 同族教训）。独立记源、失败不阻塞其余对象；resetProviders 已把
	// DNSProxyProvider 标记为「未注册」，声明非空会重新注册并起转发器（幂等）。
	// 只补齐不摘除（附录 A #35）：停用走提交编排（注销），恢复段不猜。放在 VRF/登记之后：
	// 按域上游的转发域（BVI/L3 接口索引）此刻已登记，转发器可按来源域选上游。
	if n.dns != nil {
		want := orchestrator.DNSProxyUpstreams{}
		if cfg.Vpp != nil {
			want.Global = cfg.Vpp.DNSProxyServers
		}
		for _, vs := range cfg.VirtualSwitches {
			if len(vs.DNSProxyServers) == 0 {
				continue
			}
			if want.PerSwitch == nil {
				want.PerSwitch = map[string][]string{}
			}
			want.PerSwitch[vs.Name] = vs.DNSProxyServers
		}
		if err := n.ApplyDNSProxy(ctx, want); err != nil {
			record("vpp/dns-proxy", err)
		}
	}
	if cfg.Nat != nil {
		if err := n.ApplyNAT(ctx, *cfg.Nat); err != nil {
			record("nat", err)
		}
	}
	for _, pm := range cfg.PortMirroring {
		if err := n.ApplySpan(ctx, pm); err != nil {
			record("port-mirroring/"+pm.Name, err)
		}
	}
	for _, q := range cfg.QosPolicies {
		if err := n.ApplyQos(ctx, q); err != nil {
			record("qos/policies/"+q.Name, err)
		}
	}
	if cfg.Protocols != nil {
		if err := n.ApplyLLDP(ctx, cfg.Protocols.LLDP); err != nil {
			record("protocols/lldp", err)
		}
	}
	for _, iface := range cfg.Interfaces {
		if err := n.ApplyInterface(ctx, iface); err != nil {
			record("interfaces/"+iface.Name, err)
		}
	}

	// 删表延后项的复核（决策 #192）：删表时 VPP 报「读回仍存在」的交换机，在数据面重启后
	// 其表已随重启消失（VPP 的 IP 表是运行态），这里确认并清登记 + 消警；配置又把该交换机
	// 声明回来时同样清（表是合法存在）。单列一步：它不属于「按配置重放」。
	_ = n.RetryDeferredVRFDeletes(ctx, cfg)

	// 残渣对账（决策 #192 的 IP 表 ∪ 决策 #321 的 ACL/bridge-domain）：把「数据面存在、
	// 配置未声明」的对象变成可复查的事实与告警。它不靠进程内记忆、按数据面实况逐次重建，
	// 故**跨 nfvisd 重启仍然可见**；对象随数据面重启/清理消失后 Sync 自动消警。
	// 与 EnsureConsistent 的同一次 Sync 合用：同一份「声明集 ⇄ 实况」口径，不另造巡检。
	s := n.scanResidue(cfg)
	for _, e := range s.errs {
		record("residue-scan", e)
	}
	for _, it := range s.items {
		errs = append(errs, fmt.Errorf("%s: %s", it.Source(), it.Message()))
		failures = append(failures, Alarm{
			Severity: SeverityWarning, Code: it.Code(), Message: it.Message(), Source: it.Source(),
		})
	}

	if n.alarms != nil {
		n.alarms.Sync(recoveryScope, failures)
	}
	// 提交期补偿告警的消解（决策 #321）：该对象已对得上配置（配置声明了它，或它确实不在
	// 数据面）即不再是「数据面与配置不一致」，消解其 COMMIT_COMPENSATION_FAILED；
	// 不属于可核对类别的（VM/容器等）保持原样，如实不猜测。
	n.resolveCompensationAlarms(cfg, s)
	return errs
}

// resetProviders 清空各 Provider 的进程内登记表，使本次收敛按 VPP 实况重新判定
// 对象存在性（避免跨进程/跨 VPP 重启后的陈旧登记表把重放带偏）。
//
// 两点顺序/互斥上的讲究（round84 收尾）：
//   - 全程持 runtimeMu：失效不得与一次 ApplyNAT 交错，否则那次下发会按「inside 解析已空」
//     算期望集（少下发特性，甚至把既有特性当成配置里已删的项删掉）。
//   - **先清消费方（NAT）再清来源方（L3/L2）**：NAT 的特性/池登记是派生结果的影子，
//     先摘影子后摘来源，中间状态只会是「影子全无、来源尚在」，不会反过来。
func (n *L2Network) resetProviders() {
	n.runtimeMu.Lock()
	defer n.runtimeMu.Unlock()
	if n.nat != nil {
		n.nat.reset()
	}
	if n.acl != nil {
		n.acl.reset()
	}
	if n.bond != nil {
		n.bond.reset()
	}
	if n.l2 != nil {
		n.l2.reset()
	}
	if n.l3 != nil {
		n.l3.reset()
	}
	if n.svc != nil {
		n.svc.reset()
	}
	if n.dhcp != nil {
		n.dhcp.reset()
	}
	// 决策 #359：VPP（重）连接后 tap 与 punt 注册全失（round140 实测）——关闭内核侧 tap 收包、
	// 标记注册失效；租约表保留（服务器自己的状态，客户端续租不受 VPP 重启影响），随后的
	// 恢复收敛/巡检 Sync 会按声明重建 tap、重申注册并重开内核侧 AF_PACKET。
	if n.dhcpServer != nil {
		n.dhcpServer.reset()
	}
	if n.dns != nil {
		n.dns.reset()
	}
	// 决策 #383：VPP（重）连接后隧道条目全失（接口与平台标记一并消失）——VxlanProvider 不持
	// 进程内登记，恢复收敛直接按接口 tag 判存量（在则不动、不在则按配置建），故此处无需失效。
	if n.lldp != nil {
		n.lldp.reset()
	}
}
