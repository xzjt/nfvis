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
func (n *L2Network) SetAlarms(a *AlarmStore) { n.alarms = a }

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
	}
	for _, vrf := range cfg.Vrfs {
		if err := n.ApplyVRF(ctx, vrf); err != nil {
			record("vrfs/"+vrf.Name, err)
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

	if n.alarms != nil {
		n.alarms.Sync(recoveryScope, failures)
	}
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
	if n.lldp != nil {
		n.lldp.reset()
	}
}
