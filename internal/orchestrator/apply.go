package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// Applier 事务引擎 commit 阶段调用的下发接口：把配置从 old 声明式收敛到 new，
// 任一步失败时逆序补偿已执行操作，使底座回到变更前状态（骨架 §3.3「全有或全无」）。
type Applier interface {
	Apply(ctx context.Context, old, new model.Config) error
}

// ApplierOption NewApplier 可选配置。
type ApplierOption func(*orchApplier)

// WithVhostDir 设置 VNF vhost-user socket 目录（须与 compute.Config.VhostDir 一致）。
// WithMemifDir 设置容器 memif socket 目录（须与容器编排一致）。
func WithMemifDir(dir string) ApplierOption {
	return func(a *orchApplier) {
		if dir != "" {
			a.memifDir = dir
		}
	}
}

func WithVhostDir(dir string) ApplierOption {
	return func(a *orchApplier) {
		if dir != "" {
			a.vhostDir = dir
		}
	}
}

// WithWarn 注入告警回调（非致命但操作者需要知道的处置，如接口延后收敛；缺省丢弃）。
func WithWarn(warn func(string)) ApplierOption {
	return func(a *orchApplier) {
		if warn != nil {
			a.warn = warn
		}
	}
}

// WithCommitAlarms 注入提交期残渣/延后处置的告警落点（缺省丢弃：残渣仅进提交输出与日志）。
//
// 上报两类（见 alarm.go 的 CommitCompensationFailed / CommitVrfDeleteDeferred）：
// 补偿未完成留下的残渣（事后必须可见，否则数据面已与配置不同却无人知道）与
// 「表的删除延后到数据面重启」这类非致命处置（提交成功但尚未完全收敛）。
func WithCommitAlarms(sink CommitAlarmSink) ApplierOption {
	return func(a *orchApplier) {
		if sink != nil {
			a.alarms = sink
		}
	}
}

// NewApplier 组合三类 Provider 构造编排器。资源池（内核 cpuset/大页）由 M3
// 内核编排接入后追加为第一阶段；M1 仅编排 网络→计算→容器。
func NewApplier(net NetworkProvider, comp ComputeProvider, cont ContainerProvider, opts ...ApplierOption) Applier {
	a := &orchApplier{net: net, comp: comp, cont: cont, vhostDir: DefaultVhostDir, memifDir: DefaultMemifDir}
	for _, o := range opts {
		o(a)
	}
	return a
}

type orchApplier struct {
	net  NetworkProvider
	comp ComputeProvider
	cont ContainerProvider

	// warn 告警回调（延后收敛等非致命情形的可见化）。
	warn func(string)
	// alarms 提交期残渣/延后处置的落点（可空）。
	alarms CommitAlarmSink

	// vhostDir VNF vhost-user socket 目录（compute 与 network 必须一致，缺省 /run/nfvis/vhost）。
	vhostDir string
	// memifDir 容器 memif socket 目录（缺省 /run/nfvis/memif，FR-NET-022）。
	memifDir string
}

func (a *orchApplier) warnf(format string, args ...any) {
	if a.warn != nil {
		a.warn(fmt.Sprintf(format, args...))
	}
}

// raiseCommit 记一条提交期告警（无落点时只写日志：残渣至少要出现在守护进程日志里）。
func (a *orchApplier) raiseCommit(severity, code, source, message string) {
	a.warnf("%s", message)
	if a.alarms != nil {
		a.alarms.Raise(CommitScope, severity, code, message, source)
	}
}

// resolveCommit 消一条提交期告警（同一对象的计划操作重新成功执行 = 该对象已重新收敛）。
func (a *orchApplier) resolveCommit(code, source string) {
	if a.alarms != nil {
		a.alarms.Resolve(CommitScope, code, source)
	}
}

// dpdkManagedIfaces 返回声明了 DPDK 单网卡覆盖项的物理口集合。
//
// 只有 per-dev 项会让端口进入 VPP（startup.conf 的 `dev <pci> { name <口> }` 段即由此生成），
// 所以它同时就是「该口预期由 DPDK 接管」的判据（与 `vpp.dpdk.per-dev` 的取值校验同源）。
func dpdkManagedIfaces(vpp *model.VppConfig) map[string]bool {
	if vpp == nil || vpp.DPDK == nil {
		return nil
	}
	out := make(map[string]bool, len(vpp.DPDK.PerDev))
	for _, d := range vpp.DPDK.PerDev {
		if d.Interface != "" {
			out[d.Interface] = true
		}
	}
	return out
}

// ifaceApply 返回接口层的下发函数。该口已声明为 DPDK 端口时，「尚未进入数据面」
// **不再整体回滚提交**，而是延后到数据面重启后由恢复收敛补齐（决策 #100）：
// 否则「先声明端口、绑定后再提交」会死锁——提交要求端口已在 VPP，而端口进 VPP
// 要 `request vpp restart` 重生成 startup.conf，重启又要 committed 已落库。
//
// 延后覆盖两种「数据面还没准备好」的失败（决策 #186）：
//   - ErrIfaceUnavailable：口不在数据面（VPP 在跑，但该口还没被接管）；
//   - ErrL2Unavailable：**VPP 根本没在跑**（未连接，L2 客户端为空）。
//
// 第二种是真机实测漏掉的：从零首装/整机重启后 VPP 是停的（产品有意不自动拉起，见手册 §7.5），
// 而手册 §7.3 记载的首次声明顺序恰恰是「声明 → commit → request vpp restart」——
// 此时 commit 会以「VPP 未连接，L2 客户端不可用」整体失败并回滚，与该处承诺的「延后收敛」
// 相反，操作者只能自己猜到「先 request vpp restart」。两种情形对该口是同一件事：
// 配置先落库，口由数据面重启后的恢复收敛补齐。
//
// 只延后这两类「数据面未就绪」失败；其余失败原样冒泡（不吞真错误）。
func (a *orchApplier) ifaceApply(iface model.InterfaceConfig, dpdkManaged bool) func(context.Context) error {
	run := func(ctx context.Context) error { return a.net.ApplyInterface(ctx, iface) }
	if !dpdkManaged {
		return run
	}
	return func(ctx context.Context) error {
		err := run(ctx)
		if err == nil {
			return nil
		}
		switch {
		case errors.Is(err, ErrIfaceUnavailable):
			a.warnf("接口 %s 尚未进入数据面（已声明为 DPDK 端口）：本次提交不阻断，"+
				"执行 request vpp restart 后自动收敛", iface.Name)
			return nil
		case errors.Is(err, ErrL2Unavailable):
			a.warnf("接口 %s 的数据面尚未就绪（VPP 未运行）：本次提交不阻断，"+
				"执行 request vpp restart 后自动收敛", iface.Name)
			return nil
		}
		return err
	}
}

// deleteVRF 删 L3 交换机，含「删表延后」口径（决策 #192）。
//
// NAT44 用过的表在 VPP 里带 `nat44-ei-hi` 引用（`vppctl show ip fib summary` 可见），
// 该引用**只有数据面重启才释放**：删 NAT 规则、关插件都不行，于是 `ip_table_add_del(del)`
// 返回 0 却删不掉表（round86 真机 R86-4 实证）。此前这会让「改 NAT 出接口 + 删旧 L3 交换机」
// 这类**同一提交内**的组合必然整体失败并回滚（R86-10）：配置与数据面都停在中间态，
// 操作者只能自己拆成两次提交并在中间 request vpp restart。
//
// 现在按产品既有的「延后收敛」口径处理（与 DPDK 端口的 #100/#186 同一套语义）：
// 配置侧照常删除（本轮提交成功），数据面那把**空表**的清理由数据面重启后的恢复收敛完成
// （VPP 重启即无此表；恢复收敛不再声明它，网络侧另有待清理登记兜底），并通过告警留痕——
// 「提交成功但表还在」这件事必须事后查得到，否则就是假成功。
//
// 只延后 `ErrVrfNotRemoved`（读回确认表仍在）：解绑失败、API 报错等真错误照旧冒泡，
// 由补偿按旧配置复原该 VRF。
func (a *orchApplier) deleteVRF(ctx context.Context, name string) error {
	err := a.net.DeleteVRF(ctx, name)
	if err == nil {
		// 该交换机本轮真的删干净了：清掉它历史上的「删除延后」告警。
		a.resolveCommit(CommitVrfDeleteDeferred, name)
		return nil
	}
	if !errors.Is(err, ErrVrfNotRemoved) {
		return err
	}
	a.raiseCommit(SeverityWarning, CommitVrfDeleteDeferred, name, fmt.Sprintf(
		"L3 交换机 %s 的表在数据面仍存在（NAT 用过该表后 VPP 不释放引用，只有重启数据面才会移除）："+
			"配置已删除，执行 request vpp restart 后自动清理；在此之前该表仍留在数据面", name))
	return nil
}

// 等价于 WithVhostDir 选项，供装配期后置调整。
func (a *orchApplier) SetVhostDir(dir string) {
	if dir != "" {
		a.vhostDir = dir
	}
}

// op 一次底座操作及撤销动作。
type op struct {
	desc string
	run  func(ctx context.Context) error
	undo func(ctx context.Context) error
	// compensateOnFail 该操作**失败时也执行 undo**：复合操作（一次调用内含多步底座动作）
	// 失败时可能已部分生效，undo 是把它复原的唯一途径。
	// 现状只有 VRF 两个操作置位：DeleteVRF 的顺序是「清地址 → 解绑接口回默认表 → 删表 → 读回」，
	// 任何一步失败都会把该交换机的接口留在默认表、地址已清空，而配置此时已回滚为「仍然声明它」
	// ——不复原就是「配置与数据面不一致且无人知道」（真机 round87 实测：删表读回失败后
	// ens192 留在默认表且丢了地址）。
	compensateOnFail bool
}

func (a *orchApplier) Apply(ctx context.Context, old, new model.Config) error {
	if configEqual(old, new) {
		return nil
	}
	ops := a.plan(old, new)
	var executed []op
	for _, o := range ops {
		if err := o.run(ctx); err != nil {
			// 逆序补偿已执行操作；补偿失败仅追加说明，不掩盖原始错误
			errs := []string{fmt.Sprintf("下发失败 %s: %v", o.desc, err)}
			// 失败操作本身可能已部分生效（复合操作）：先按旧配置复原，使回滚真正回到变更前状态。
			if o.compensateOnFail {
				if cerr := o.undo(ctx); cerr != nil {
					errs = append(errs, fmt.Sprintf("补偿失败 %s: %v", o.desc, cerr))
					a.residueAlarm(o.desc, cerr)
				}
			}
			for i := len(executed) - 1; i >= 0; i-- {
				if cerr := executed[i].undo(ctx); cerr != nil {
					errs = append(errs, fmt.Sprintf("补偿失败 %s: %v", executed[i].desc, cerr))
					a.residueAlarm(executed[i].desc, cerr)
				}
			}
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		// 该对象本轮重新收敛成功：清掉它历史上的残渣告警（重试提交即为复原方式）。
		a.resolveCommit(CommitCompensationFailed, o.desc)
		executed = append(executed, o)
	}
	return nil
}

// residueAlarm 补偿未完成 → 告警（FR-OPS-014 的可追溯口径）。
//
// 残渣此前只出现在当次提交的输出里：操作者当时看得到，事后 `show alarms`、Web 总览页、
// 诊断包里都查不到，而数据面此时已经与配置不同（round86 登记 R86-9）。告警里带上
// 「下一步能做什么」，因为这类残渣的复原路径都是确定的（重试提交 / 重启数据面后再提交）。
func (a *orchApplier) residueAlarm(desc string, err error) {
	a.raiseCommit(SeverityError, CommitCompensationFailed, desc, fmt.Sprintf(
		"提交失败后的补偿未完成：%s 回滚失败（%v）。配置已回滚到上一版本，数据面该对象可能残留"+
			"中间状态（多出来的对象，或没有被恢复的地址/归属）；重新提交同一变更、或先执行 "+
			"request vpp restart 再提交即可复原。本条是进程内记录（重启后不再出现）；若残渣是可对账"+
			"的对象（IP 表/ACL/bridge-domain），启动/巡检对账会另行以 *_LEFTOVER 告警持续呈现，"+
			"对象清理后自动消解", desc, err))
}

// lldpEqual 判断 LLDP 配置是否变化（Protocols 指针比较已由 configEqualPtr 覆盖，此处冗余防御）。
func lldpEqual(old, new model.Config) bool { return configEqualPtr(old.Protocols, new.Protocols) }

// plan 生成操作序列：新增/变更在前（ACL→L2→L3→NAT/SPAN/QoS→VM→容器），
// 删除在后（ACL→容器→VM→L3/L2→NAT/SPAN/QoS），保证引用先建后删。
func (a *orchApplier) plan(old, new model.Config) []op {
	// 交换机端口集合 = 交换机侧声明 ∪ VNF/容器侧 vNIC 声明（FR-NET-020~023，决策 #170）：
	// 两侧必须在此合流后再 diff 与下发，否则「VNF 声明了 virtual-switch」既不触发 BD
	// 重下发、也不出现在成员集里（vhost-user 口静默不进 BD ← guest 无 L2 连通）。
	oldVSList, oldRefErrs := SwitchMembersOf(old, a.vhostDir, a.memifDir)
	newVSList, newRefErrs := SwitchMembersOf(new, a.vhostDir, a.memifDir)
	oldACLs := nameMap(old.Acls, func(x model.Acl) string { return x.Name })
	oldVSs := nameMap(oldVSList, func(x model.VirtualSwitch) string { return x.Name })
	oldVRFs := nameMap(old.Vrfs, func(x model.Vrf) string { return x.Name })
	newVRFs := nameMap(new.Vrfs, func(x model.Vrf) string { return x.Name })
	oldVMs := nameMap(old.VirtualMachineFunctions, func(x model.VMFunction) string { return x.Name })
	oldCTs := nameMap(old.ContainerFunctions, func(x model.ContainerFunction) string { return x.Name })
	oldIfaces := nameMap(old.Interfaces, func(x model.InterfaceConfig) string { return x.Name })
	oldBonds := nameMap(old.Bonds, func(x model.Bond) string { return x.Name })
	oldPMs := nameMap(old.PortMirroring, func(x model.PortMirroring) string { return x.Name })
	oldQoS := nameMap(old.QosPolicies, func(x model.QosPolicy) string { return x.Name })
	oldVxlan := nameMap(old.VxlanTunnels, func(x model.VxlanTunnel) string { return x.Name })

	var ops []op
	if err := vnicSwitchRefErr(newRefErrs); err != nil {
		// 声明无法归位 → 该 vNIC 不会进入任何 bridge-domain（正是「静默不通」的旧行为）。
		// 提交阶段直接失败，不猜测、不静默跳过。
		return []op{{desc: "vnf-vswitch-refs", run: func(context.Context) error { return err }}}
	}
	if len(oldRefErrs) > 0 {
		// 原配置的失效声明只影响回滚方向（回滚时那个 vNIC 本就无 BD 可挂），记告警不阻断提交。
		a.warnf("原配置有 %d 条 vNIC 的虚拟交换机声明无法归位，仅影响回滚方向: %s",
			len(oldRefErrs), joinErrs(oldRefErrs))
	}

	// —— VNF vNIC 接入（FR-NET-020/022/023）：必须先于 bridge-domain 下发，
	// 因为交换机端口按确定性接口名挂接（vhost-user / memif 接口需已存在）。——
	oldPorts := VnfPortsOf(old, a.vhostDir, a.memifDir)
	newPorts := VnfPortsOf(new, a.vhostDir, a.memifDir)
	oldByKey := portMapOf(oldPorts)
	newByKey := portMapOf(newPorts)
	for _, np := range newPorts {
		op, inOld := oldByKey[portKeyOf(np)]
		if inOld && configEqual(op, np) {
			continue
		}
		np, op := np, op
		ops = append(ops, op2(
			fmt.Sprintf("vnf-if[%s/%s]", np.VM, np.Interface),
			func(ctx context.Context) error { return a.net.ApplyVnfInterface(ctx, np) },
			inOld,
			func(ctx context.Context) error { return a.net.ApplyVnfInterface(ctx, op) },
			func(ctx context.Context) error { return a.net.DeleteVnfInterface(ctx, np.VM, np.Interface) },
		))
	}

	// —— 新增/变更：网络 ——
	for _, acl := range new.Acls {
		if o, ok := oldACLs[acl.Name]; !ok || !configEqual(o, acl) {
			ops = append(ops, applyOp(
				fmt.Sprintf("acl[%s]", acl.Name),
				func(ctx context.Context) error { return a.net.ApplyACL(ctx, acl) },
				acl.Name, ok,
				func(ctx context.Context) error { return a.net.ApplyACL(ctx, o) },
				func(ctx context.Context) error { return a.net.DeleteACL(ctx, acl.Name) },
			))
		}
	}
	// bond 须先于引用它的 L2 端口/L3 接口创建（FR-NET-017）
	for _, bond := range new.Bonds {
		if o, ok := oldBonds[bond.Name]; !ok || !configEqual(o, bond) {
			ops = append(ops, applyOp(
				fmt.Sprintf("bond[%s]", bond.Name),
				func(ctx context.Context) error { return a.net.ApplyBond(ctx, bond) },
				bond.Name, ok,
				func(ctx context.Context) error { return a.net.ApplyBond(ctx, o) },
				func(ctx context.Context) error { return a.net.DeleteBond(ctx, bond.Name) },
			))
		}
	}
	for _, vs := range newVSList {
		if vs.Type != "l2" {
			continue // L3 交换机数据经同名 VRF 条目编排（附录 B）
		}
		if o, ok := oldVSs[vs.Name]; !ok || !configEqual(o, vs) {
			ops = append(ops, applyOp(
				fmt.Sprintf("bridge-domain[%s]", vs.Name),
				func(ctx context.Context) error { return a.net.ApplyBridgeDomain(ctx, vs) },
				vs.Name, ok,
				func(ctx context.Context) error { return a.net.ApplyBridgeDomain(ctx, o) },
				func(ctx context.Context) error { return a.net.DeleteBridgeDomain(ctx, vs.Name) },
			))
			// 决策 #335：DHCP 中继作为 bridge-domain 之后的伴随操作下发（先有 BVI 地址与表
			// 才有 relay）。新配/改 server 下发 IsAdd=true，清 relay 按登记撤（IsAdd=false），
			// 声明未变时幂等跳过；undo 把中继收敛回旧声明（新交换机的删除向=整体撤销）。
			// 校验层已保证：配 relay 的交换机必有 IPv4 网关（中继源地址的来源）。
			ops = append(ops, applyOp(
				fmt.Sprintf("dhcp-relay[%s]", vs.Name),
				func(ctx context.Context) error { return a.net.ApplyDhcpRelay(ctx, vs) },
				vs.Name, ok,
				func(ctx context.Context) error { return a.net.ApplyDhcpRelay(ctx, o) },
				func(ctx context.Context) error {
					return a.net.ApplyDhcpRelay(ctx, model.VirtualSwitch{Name: vs.Name})
				},
			))
			// 决策 #359：DHCP 服务器作为 bridge-domain（与 relay）之后的伴随操作收敛——
			// tap 入 BD、server-id/下发网关都来自网关声明（先有 BVI 地址与 BD 才有 server）。
			// 新配/变更收敛到新声明，停用（声明里无池）＝teardown（删内置 tap、注销 punt、
			// 清租约文件）；undo 把服务器收敛回旧声明（新交换机的删除向=整体撤销，同 relay）。
			// 校验层已保证：仅 L2 + 已配 IPv4 BVI 网关可配，且与 relay 互斥。
			ops = append(ops, applyOp(
				fmt.Sprintf("dhcp-server[%s]", vs.Name),
				func(ctx context.Context) error { return a.net.ApplyDHCPServer(ctx, vs) },
				vs.Name, ok,
				func(ctx context.Context) error { return a.net.ApplyDHCPServer(ctx, o) },
				func(ctx context.Context) error {
					return a.net.ApplyDHCPServer(ctx, model.VirtualSwitch{Name: vs.Name})
				},
			))
		}
	}
	for _, vrf := range new.Vrfs {
		if o, ok := oldVRFs[vrf.Name]; !ok || !configEqual(o, vrf) {
			vrfOp := applyOp(
				VrfOpDesc(vrf.Name),
				func(ctx context.Context) error { return a.net.ApplyVRF(ctx, vrf) },
				vrf.Name, ok,
				func(ctx context.Context) error { return a.net.ApplyVRF(ctx, o) },
				func(ctx context.Context) error { return a.net.DeleteVRF(ctx, vrf.Name) },
			)
			// 复合操作（建表 → 配地址 → 置表 → 下路由）：失败时可能已部分生效，需按旧配置复原。
			vrfOp.compensateOnFail = true
			ops = append(ops, vrfOp)
			// 决策 #361：L3 接口一级的差分撤销（与 removedRoutes 同一风格），紧跟本次 ApplyVRF
			// 之后执行、且位于删除段之前——此前这两类撤销没有任何生产路径：ApplyVRF 只对仍声明
			// AclIn 的接口下发绑定（对「不再声明」不复核），声明里删掉 l3-interface 也不回收
			// （口留原表、地址不摘、deny 绑定继续拦；round142 真机实测）。
			//   - l3-acl-unbind：接口仍在、acl-in 被清 ⇒ 只撤绑定与伴随 macip；
			//   - del-l3-if：接口整条被删 ⇒ 回收（清地址/解绑/移回默认表/摘登记）。
			// 顺序也是「先解引用、后删被引用」（#342 同族）：绑定撤销先于删除段里的 del-acl，
			// 否则删 ACL 会撞上仍指向它的陈旧绑定。undo 与该 VRF 的 apply op 同源（ApplyVRF(old)），
			// 回滚时把旧声明的接口与绑定整体复原。
			if ok {
				for _, li := range l3ACLUnbinds(o, vrf) {
					li := li
					ops = append(ops, op{
						desc: fmt.Sprintf("l3-acl-unbind[%s/%s]", vrf.Name, l3IfaceKeyOf(li)),
						run: func(ctx context.Context) error {
							return a.net.UnbindL3IfaceACL(ctx, vrf.Name, li)
						},
						undo: func(ctx context.Context) error { return a.net.ApplyVRF(ctx, o) },
					})
				}
				for _, li := range removedL3Ifaces(o, vrf) {
					li := li
					ops = append(ops, op{
						desc: fmt.Sprintf("del-l3-if[%s/%s]", vrf.Name, l3IfaceKeyOf(li)),
						run: func(ctx context.Context) error {
							return a.net.DeleteL3Interface(ctx, vrf.Name, li)
						},
						undo: func(ctx context.Context) error { return a.net.ApplyVRF(ctx, o) },
					})
				}
			}
		}
	}
	// —— 新增/变更：VXLAN 隧道（决策 #383，FR-NET-019）——
	// 放在交换机（L2 BD）与 VRF **之后**：声明了 virtual-switch 的隧道口要加入该交换机的 BD，
	// BD 必须先在。变更撤旧由 provider 内部按**已下发登记的旧元组**完成（同 #380 的教训：
	// 改 local/remote/vni/dst-port 时先撤旧再建新）；undo 把隧道收敛回旧声明（收敛失败时会
	// 自行撤掉新元组——ApplyVxlan 的撤旧基于登记，回滚路径同样成立）。
	for _, vx := range new.VxlanTunnels {
		if o, ok := oldVxlan[vx.Name]; !ok || !configEqual(o, vx) {
			ops = append(ops, applyOp(
				fmt.Sprintf("vxlan[%s]", vx.Name),
				func(ctx context.Context) error { return a.net.ApplyVxlan(ctx, vx) },
				vx.Name, ok,
				func(ctx context.Context) error { return a.net.ApplyVxlan(ctx, o) },
				func(ctx context.Context) error { return a.net.DeleteVxlan(ctx, vx) },
			))
		}
	}

	// —— 新增/变更：数据面 DNS 代理（决策 #345，FR-NET-010）——
	// vpp 段 + 各交换机段的伴随操作：全局或任一交换机非空 ⇒ 注册 punt socket 并起域内转发器；
	// 全空 ⇒ 注销（VPP 恢复默认处理）。声明未变时 Sync 内部按状态幂等跳过；undo 收敛回旧声明。
	// 放在交换机/VRF 之后：启用只依赖注册，但按域上游的转发域（BVI/L3 接口索引）此刻才就绪。
	oldDNS := dnsProxyUpstreamsOf(old)
	newDNS := dnsProxyUpstreamsOf(new)
	if !reflect.DeepEqual(oldDNS, newDNS) {
		ops = append(ops, op{
			desc: "dns-proxy",
			run:  func(ctx context.Context) error { return a.net.ApplyDNSProxy(ctx, newDNS) },
			undo: func(ctx context.Context) error { return a.net.ApplyDNSProxy(ctx, oldDNS) },
		})
	}

	if !configEqualPtr(old.Nat, new.Nat) {
		newNat := model.NatConfig{}
		if new.Nat != nil {
			newNat = *new.Nat
		}
		ops = append(ops, op{
			desc: "nat",
			run:  func(ctx context.Context) error { return a.net.ApplyNAT(ctx, newNat) },
			undo: func(ctx context.Context) error {
				oldNat := model.NatConfig{}
				if old.Nat != nil {
					oldNat = *old.Nat
				}
				return a.net.ApplyNAT(ctx, oldNat)
			},
		})
	}
	for _, pm := range new.PortMirroring {
		if o, ok := oldPMs[pm.Name]; !ok || !configEqual(o, pm) {
			ops = append(ops, applyOp(
				fmt.Sprintf("span[%s]", pm.Name),
				func(ctx context.Context) error { return a.net.ApplySpan(ctx, pm) },
				pm.Name, ok,
				func(ctx context.Context) error { return a.net.ApplySpan(ctx, o) },
				func(ctx context.Context) error { return a.net.DeleteSpan(ctx, pm.Name) },
			))
		}
	}
	for _, q := range new.QosPolicies {
		if o, ok := oldQoS[q.Name]; !ok || !configEqual(o, q) {
			ops = append(ops, applyOp(
				fmt.Sprintf("qos[%s]", q.Name),
				func(ctx context.Context) error { return a.net.ApplyQos(ctx, q) },
				q.Name, ok,
				func(ctx context.Context) error { return a.net.ApplyQos(ctx, o) },
				func(ctx context.Context) error { return a.net.DeleteQos(ctx, q.Name) },
			))
		}
	}

	// —— 新增/变更：LLDP（FR-NET-018）——
	if !configEqualPtr(old.Protocols, new.Protocols) || !lldpEqual(old, new) {
		newL := (*model.LldpConfig)(nil)
		if new.Protocols != nil {
			newL = new.Protocols.LLDP
		}
		oldL := (*model.LldpConfig)(nil)
		if old.Protocols != nil {
			oldL = old.Protocols.LLDP
		}
		ops = append(ops, op{
			desc: "lldp",
			run:  func(ctx context.Context) error { return a.net.ApplyLLDP(ctx, newL) },
			undo: func(ctx context.Context) error { return a.net.ApplyLLDP(ctx, oldL) },
		})
	}

	// —— 新增/变更：接口层（MTU / ingress-policy 绑定；QoS 之后，保证 policer 已建）——
	dpdkPorts := dpdkManagedIfaces(new.Vpp)
	for _, iface := range new.Interfaces {
		if o, ok := oldIfaces[iface.Name]; !ok || !configEqual(o, iface) {
			ops = append(ops, applyOp(
				fmt.Sprintf("interface[%s]", iface.Name),
				a.ifaceApply(iface, dpdkPorts[iface.Name]),
				iface.Name, ok,
				func(ctx context.Context) error { return a.net.ApplyInterface(ctx, o) },
				func(ctx context.Context) error { return nil },
			))
		}
	}

	// —— 新增/变更：计算、容器 ——
	for _, vm := range new.VirtualMachineFunctions {
		oldVM, inOld := oldVMs[vm.Name]
		if inOld && configEqual(oldVM, vm) {
			continue
		}
		vm := vm
		alloc := model.AllocationFor(new, vm)
		ops = append(ops, op{
			desc: fmt.Sprintf("vm[%s]", vm.Name),
			run:  func(ctx context.Context) error { return a.comp.DefineVM(ctx, vm, alloc) },
			undo: func(ctx context.Context) error {
				if inOld {
					return a.comp.DefineVM(ctx, oldVM, model.AllocationFor(old, oldVM))
				}
				return a.comp.DeleteVM(ctx, vm.Name)
			},
		})
	}
	for _, ct := range new.ContainerFunctions {
		if o, ok := oldCTs[ct.Name]; !ok || !configEqual(o, ct) {
			ops = append(ops, applyOp(
				fmt.Sprintf("container[%s]", ct.Name),
				func(ctx context.Context) error { return a.cont.ApplyContainer(ctx, ct) },
				ct.Name, ok,
				func(ctx context.Context) error { return a.cont.ApplyContainer(ctx, o) },
				func(ctx context.Context) error { return a.cont.DeleteContainer(ctx, ct.Name) },
			))
		}
	}

	// —— 删除：**先解引用**（先删/解绑 ACL，此时引用它的接口仍必须存在）→ 容器 → VM →
	// 网络（bd/vrf → span/qos），与新增顺序相反（决策 #342，同 #196「先解引用、后删被引用」）。
	// ACL 删除内含「解绑引用它的接口」，若接口先被交换机/VM 删除，解绑必然撞 VPP -2 并整次回滚
	// （真机 round117/119/120 四次复现）。故 ACL 排在容器/VM/bd/vrf 之前。
	//
	// VXLAN 隧道在删除段**最前**：隧道口是 bridge-domain 的成员口，BD 还有成员时 VPP 拒绝
	// 删除（-120）；配置里消失的隧道必须在此显式撤销——只从配置移除会留下隧道条目与该 BD
	// 归属。身份取 `Name`（#382 的教训：差分键要能反映「同一对象」）。
	for _, vx := range old.VxlanTunnels {
		if _, ok := newVxlanNames(new)[vx.Name]; ok {
			continue
		}
		vx := vx
		ops = append(ops, op{
			desc: fmt.Sprintf("del-vxlan[%s]", vx.Name),
			run:  func(ctx context.Context) error { return a.net.DeleteVxlan(ctx, vx) },
			undo: func(ctx context.Context) error { return a.net.ApplyVxlan(ctx, vx) },
		})
	}
	for _, acl := range old.Acls {
		if _, ok := newACLNames(new)[acl.Name]; !ok {
			acl := acl
			ops = append(ops, op{
				desc: fmt.Sprintf("del-acl[%s]", acl.Name),
				run:  func(ctx context.Context) error { return a.net.DeleteACL(ctx, acl.Name) },
				undo: func(ctx context.Context) error { return a.net.ApplyACL(ctx, acl) },
			})
		}
	}
	for name := range oldCTs {
		if _, ok := nameMap(new.ContainerFunctions, func(x model.ContainerFunction) string { return x.Name })[name]; !ok {
			ct := oldCTs[name]
			ops = append(ops, op{
				desc: fmt.Sprintf("del-container[%s]", name),
				run:  func(ctx context.Context) error { return a.cont.DeleteContainer(ctx, name) },
				undo: func(ctx context.Context) error { return a.cont.ApplyContainer(ctx, ct) },
			})
		}
	}
	for name := range oldVMs {
		if _, ok := nameMap(new.VirtualMachineFunctions, func(x model.VMFunction) string { return x.Name })[name]; !ok {
			vm := oldVMs[name]
			ops = append(ops, op{
				desc: fmt.Sprintf("del-vm[%s]", name),
				run:  func(ctx context.Context) error { return a.comp.DeleteVM(ctx, name) },
				undo: func(ctx context.Context) error { return a.comp.DefineVM(ctx, vm, model.AllocationFor(old, vm)) },
			})
		}
	}
	for name := range oldVSs {
		if oldVSs[name].Type != "l2" {
			continue
		}
		if _, ok := newVSNames(new)[name]; !ok {
			vs := oldVSs[name]
			ops = append(ops, op{
				desc: fmt.Sprintf("del-bridge-domain[%s]", name),
				run:  func(ctx context.Context) error { return a.net.DeleteBridgeDomain(ctx, name) },
				undo: func(ctx context.Context) error { return a.net.ApplyBridgeDomain(ctx, vs) },
			})
		}
	}
	// 静态路由撤销：声明里已不再有的路由必须在**本次 diff** 里显式从 FIB 撤除。
	// 两条路径都要覆盖：① 只删路由叶子（同名 VRF 仍在声明里）；② 整台 L3 交换机被删
	// （连同其 VRF 条目）。二者此前都漏——ApplyVRF 只下发声明里的路由（只加不撤），
	// DeleteVRF 依赖删表，而 VPP 会保住仍被接口占用的表（`vppctl show ip table` 里
	// 该项仍带 locks:[interface:…]），残留路由因此长期留在 FIB 里、`show vrfs <n> routes`
	// 依旧读得到（真机实测）。撤销放在 del-vrf 之前：表还在时逐条撤干净，不依赖删表。
	// 恢复收敛不经本函数（EnsureConsistent 只补齐），故「只补齐不摘除」口径不变（附录 A #35）。
	for _, ov := range old.Vrfs {
		nv, still := newVRFs[ov.Name]
		var gone []model.Route
		if still {
			gone = removedRoutes(ov, nv)
		} else {
			gone = ov.Routes
		}
		for _, r := range gone {
			r := r
			ops = append(ops, op{
				desc: fmt.Sprintf("del-route[%s]", routeLabel(ov.Name, r)),
				run:  func(ctx context.Context) error { return a.net.DeleteRoute(ctx, ov.Name, r) },
				undo: func(ctx context.Context) error { return a.net.ApplyRoute(ctx, ov.Name, r) },
			})
		}
	}
	for name := range oldVRFs {
		if _, ok := newVRFNames(new)[name]; !ok {
			vrf := oldVRFs[name]
			ops = append(ops, op{
				desc: VrfDeleteOpDesc(name),
				run:  func(ctx context.Context) error { return a.deleteVRF(ctx, name) },
				undo: func(ctx context.Context) error { return a.net.ApplyVRF(ctx, vrf) },
				// 删表是复合操作（清地址 → 解绑接口 → 删表 → 读回）：失败时可能已把接口留在
				// 默认表、地址已清空，需按旧配置复原（见 op.compensateOnFail）。
				compensateOnFail: true,
			})
		}
	}
	// bond 删除须在引用它的交换机端口/L3 接口之后（上方 BD/VRF 段已解除引用），
	// 且必须真的下发到数据面：只从配置里移除会留下 BondEthernetX 与成员关系。
	for name := range oldBonds {
		if _, ok := newBondNames(new)[name]; !ok {
			bond := oldBonds[name]
			ops = append(ops, op{
				desc: fmt.Sprintf("del-bond[%s]", name),
				run:  func(ctx context.Context) error { return a.net.DeleteBond(ctx, name) },
				undo: func(ctx context.Context) error { return a.net.ApplyBond(ctx, bond) },
			})
		}
	}
	// vNIC 接入删除排在**所有引用它的转发域之后**（BD 摘成员、L3 交换机清地址/解绑，见上）。
	// 真机 round88（R88-6）：同一提交里删 VNF 又删引用其 vNIC 的 L3 交换机（vNIC 作
	// l3-interface），旧顺序把 vNIC 先删掉 → del-vrf 清地址时接口已不在（Invalid sw_if_index）
	// → 整次提交失败并回滚，「一次清干净」做不到。删除顺序按依赖倒序（先解引用、后删被引用）。
	for _, ov := range oldPorts {
		if _, ok := newByKey[portKeyOf(ov)]; ok {
			continue
		}
		ov := ov
		ops = append(ops, op{
			desc: fmt.Sprintf("del-vnf-if[%s/%s]", ov.VM, ov.Interface),
			run:  func(ctx context.Context) error { return a.net.DeleteVnfInterface(ctx, ov.VM, ov.Interface) },
			undo: func(ctx context.Context) error { return a.net.ApplyVnfInterface(ctx, ov) },
		})
	}
	for _, pm := range old.PortMirroring {
		if _, ok := newPMNames(new)[pm.Name]; !ok {
			pm := pm
			ops = append(ops, op{
				desc: fmt.Sprintf("del-span[%s]", pm.Name),
				run:  func(ctx context.Context) error { return a.net.DeleteSpan(ctx, pm.Name) },
				undo: func(ctx context.Context) error { return a.net.ApplySpan(ctx, pm) },
			})
		}
	}
	for _, q := range old.QosPolicies {
		if _, ok := newQoSNames(new)[q.Name]; !ok {
			q := q
			ops = append(ops, op{
				desc: fmt.Sprintf("del-qos[%s]", q.Name),
				run:  func(ctx context.Context) error { return a.net.DeleteQos(ctx, q.Name) },
				undo: func(ctx context.Context) error { return a.net.ApplyQos(ctx, q) },
			})
		}
	}
	return ops
}

// routeKeyOf 一条静态路由在**声明差分**里的身份：**前缀**（决策 #382 收敛）。
//
// 为什么只是前缀：VPP 侧对同一前缀是**替换**语义——`ip_route_add_del(is_add=true)` 用新路径集
// 覆盖该前缀（l3_govpp.go），故「同前缀的下一跳变更（含单值↔多值、多值增删）」的正确处理是
// **由 ApplyVRF 重新下发**，而不是发 del-route：del-route 段在 ApplyVRF **之后**执行，会把刚
// 下发的新路由删掉（真机实证：改下一跳后路由从 FIB 消失、配置与读视图一切正常——数据面黑洞）。
//
// 同一推理此前只用在 distance 上（key 不含 distance，避免「只改 distance」被判成
// 「旧路由消失 + 新路由出现」而误撤）；下一跳曾漏，本决策一并收敛。
func routeKeyOf(r model.Route) string { return r.Prefix }

// routeLabel 路由的展示标签（计划操作的描述与错误文案，如 vs-l3 10.0.0.0/24 via 10.0.0.254）。
func routeLabel(vrfName string, r model.Route) string {
	if r.NextHop == "" {
		return fmt.Sprintf("%s %s", vrfName, r.Prefix)
	}
	return fmt.Sprintf("%s %s via %s", vrfName, r.Prefix, r.NextHop)
}

// removedRoutes 返回 old 里声明、new 里不再声明的静态路由（去重，保持声明序）：
// 即声明驱动的撤销集合，供 del-route 计划操作使用。
func removedRoutes(old, new model.Vrf) []model.Route {
	keep := make(map[string]bool, len(new.Routes))
	for _, r := range new.Routes {
		keep[routeKeyOf(r)] = true
	}
	seen := make(map[string]bool, len(old.Routes))
	var out []model.Route
	for _, r := range old.Routes {
		k := routeKeyOf(r)
		if keep[k] || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, r)
	}
	return out
}

// l3IfaceKeyOf 一条 l3-interface 在声明里的身份：接口名 + vlan 子接口。
//
// 刻意含 vlan：同一物理口按不同 vlan 声明的子接口在 VPP 侧是两个独立 sw_if_index，
// 只按接口名做键会把「vlan 100 改成 vlan 200」判成「接口还在」——旧子接口的地址与 ACL
// 绑定就留在了数据面（正是本决策在修的残留形态）。Vlan==0 时退化为接口名本身。
func l3IfaceKeyOf(li model.L3Interface) string {
	if li.Vlan > 0 {
		return fmt.Sprintf("%s.%d", li.Interface, li.Vlan)
	}
	return li.Interface
}

// removedL3Ifaces 返回 old 里声明、new 里不再声明的 l3-interface（去重、保持声明序）：
// 即「接口本身要被回收」的撤销集合，供 del-l3-if 计划操作调用 DeleteL3Interface
// （清地址、解绑 ACL/伴随 macip、移回默认表、摘登记；决策 #361）。
func removedL3Ifaces(old, new model.Vrf) []model.L3Interface {
	keep := make(map[string]bool, len(new.L3Interfaces))
	for _, li := range new.L3Interfaces {
		keep[l3IfaceKeyOf(li)] = true
	}
	seen := make(map[string]bool, len(old.L3Interfaces))
	var out []model.L3Interface
	for _, li := range old.L3Interfaces {
		k := l3IfaceKeyOf(li)
		if keep[k] || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, li)
	}
	return out
}

// l3ACLUnbinds 返回「接口仍在声明里、acl-in 从有到无」的 l3-interface（去重、保持声明序）：
// 只需撤销数据面绑定（UnbindL3IfaceACL），接口本身不回收。
//
// 接口被整条删掉的不进这里（由 removedL3Ifaces 的 del-l3-if 一并解绑）；
// acl-in 由非空改成**另一个非空**同样不解绑（ApplyVRF 的绑定是替换语义，旧绑定随新绑定覆盖）。
func l3ACLUnbinds(old, new model.Vrf) []model.L3Interface {
	now := make(map[string]model.L3Interface, len(new.L3Interfaces))
	for _, li := range new.L3Interfaces {
		now[l3IfaceKeyOf(li)] = li
	}
	seen := make(map[string]bool, len(old.L3Interfaces))
	var out []model.L3Interface
	for _, li := range old.L3Interfaces {
		k := l3IfaceKeyOf(li)
		if li.AclIn == "" || seen[k] {
			continue
		}
		nl, still := now[k]
		if !still || nl.AclIn != "" {
			continue
		}
		seen[k] = true
		out = append(out, li)
	}
	return out
}

// portKeyOf/portMapOf VNF 端口按「属主/vNIC」索引（diff 用）。
func portKeyOf(p VnfPort) string { return p.VM + "/" + p.Interface }

// vnicSwitchRefErr 把「vNIC 声明的虚拟交换机无法归位」汇总为一个错误（无则 nil）。
func vnicSwitchRefErr(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", joinErrs(errs))
}

// joinErrs 以分号连接多条错误文案。
func joinErrs(errs []error) string {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return strings.Join(msgs, "; ")
}

func portMapOf(ports []VnfPort) map[string]VnfPort {
	m := make(map[string]VnfPort, len(ports))
	for _, p := range ports {
		m[portKeyOf(p)] = p
	}
	return m
}

// op2 构造带「旧值恢复」补偿的操作。
func op2(desc string, run func(context.Context) error, inOld bool, applyOld, del func(context.Context) error) op {
	return op{desc: desc, run: run, undo: func(ctx context.Context) error {
		if inOld {
			return applyOld(ctx)
		}
		return del(ctx)
	}}
}

// applyOp 构造「下发 new」操作：撤销时若 old 中存在则恢复旧版本，否则删除。
func applyOp(desc string, run func(context.Context) error, name string, inOld bool, applyOld, del func(context.Context) error) op {
	return op{
		desc: desc,
		run:  run,
		undo: func(ctx context.Context) error {
			if inOld {
				return applyOld(ctx)
			}
			return del(ctx)
		},
	}
}

func nameMap[T any](items []T, key func(T) string) map[string]T {
	m := make(map[string]T, len(items))
	for _, it := range items {
		m[key(it)] = it
	}
	return m
}

func newVSNames(c model.Config) map[string]bool {
	m := map[string]bool{}
	for _, vs := range c.VirtualSwitches {
		m[vs.Name] = true
	}
	return m
}

func newVRFNames(c model.Config) map[string]bool {
	m := map[string]bool{}
	for _, v := range c.Vrfs {
		m[v.Name] = true
	}
	return m
}

func newBondNames(c model.Config) map[string]bool {
	m := map[string]bool{}
	for _, b := range c.Bonds {
		m[b.Name] = true
	}
	return m
}

func newACLNames(c model.Config) map[string]bool {
	m := map[string]bool{}
	for _, a := range c.Acls {
		m[a.Name] = true
	}
	return m
}

func newPMNames(c model.Config) map[string]bool {
	m := map[string]bool{}
	for _, p := range c.PortMirroring {
		m[p.Name] = true
	}
	return m
}

func newQoSNames(c model.Config) map[string]bool {
	m := map[string]bool{}
	for _, q := range c.QosPolicies {
		m[q.Name] = true
	}
	return m
}

func newVxlanNames(c model.Config) map[string]bool {
	m := map[string]bool{}
	for _, t := range c.VxlanTunnels {
		m[t.Name] = true
	}
	return m
}

// configEqual 通过 JSON 比较模型值（Go map 序列化按键排序，结果确定）。
func configEqual(a, b any) bool {
	return reflect.DeepEqual(jsonKey(a), jsonKey(b))
}

func configEqualPtr[T any](a, b *T) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return configEqual(*a, *b)
}

// dnsProxyUpstreamsOf 从配置提取数据面 DNS 代理的期望上游（决策 #345）：全局 + 各交换机按域。
// 只收**真配了上游**的交换机（空声明不入 PerSwitch，与「全空即停用」判据一致）。
func dnsProxyUpstreamsOf(cfg model.Config) DNSProxyUpstreams {
	want := DNSProxyUpstreams{}
	if cfg.Vpp != nil {
		want.Global = append([]string{}, cfg.Vpp.DNSProxyServers...)
	}
	for _, vs := range cfg.VirtualSwitches {
		if len(vs.DNSProxyServers) == 0 {
			continue
		}
		if want.PerSwitch == nil {
			want.PerSwitch = map[string][]string{}
		}
		want.PerSwitch[vs.Name] = append([]string{}, vs.DNSProxyServers...)
	}
	return want
}

func jsonKey(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}

// NewNoopApplier M2 阶段的空底座实现（骨架 §5：M2 可完整演示 CLI/API 事务，
// 不含真实网络）。下发即成功、恢复收敛为空集；M3/M4 以真实 Provider 替换。
func NewNoopApplier() Applier { return noopApplier{} }

type noopApplier struct{}

func (noopApplier) Apply(context.Context, model.Config, model.Config) error { return nil }
