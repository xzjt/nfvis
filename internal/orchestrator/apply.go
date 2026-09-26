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
// 只延后「接口不在数据面」这一种失败；其余失败原样冒泡（不吞真错误）。
func (a *orchApplier) ifaceApply(iface model.InterfaceConfig, dpdkManaged bool) func(context.Context) error {
	run := func(ctx context.Context) error { return a.net.ApplyInterface(ctx, iface) }
	if !dpdkManaged {
		return run
	}
	return func(ctx context.Context) error {
		err := run(ctx)
		if err == nil || !errors.Is(err, ErrIfaceUnavailable) {
			return err
		}
		a.warnf("接口 %s 尚未进入数据面（已声明为 DPDK 端口）：本次提交不阻断，"+
			"执行 request vpp restart 后自动收敛", iface.Name)
		return nil
	}
}

// SetVhostDir 设置 vhost-user socket 目录（须与 compute.Config.VhostDir 一致）。
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
			for i := len(executed) - 1; i >= 0; i-- {
				if cerr := executed[i].undo(ctx); cerr != nil {
					errs = append(errs, fmt.Sprintf("补偿失败 %s: %v", executed[i].desc, cerr))
				}
			}
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		executed = append(executed, o)
	}
	return nil
}

// lldpEqual 判断 LLDP 配置是否变化（Protocols 指针比较已由 configEqualPtr 覆盖，此处冗余防御）。
func lldpEqual(old, new model.Config) bool { return configEqualPtr(old.Protocols, new.Protocols) }

// plan 生成操作序列：新增/变更在前（ACL→L2→L3→NAT/SPAN/QoS→VM→容器），
// 删除在后（容器→VM→L3/L2→NAT/SPAN/QoS→ACL），保证引用先建后删。
func (a *orchApplier) plan(old, new model.Config) []op {
	// 交换机端口集合 = 交换机侧声明 ∪ VNF/容器侧 vNIC 声明（FR-NET-020~023，决策 #170）：
	// 两侧必须在此合流后再 diff 与下发，否则「VNF 声明了 virtual-switch」既不触发 BD
	// 重下发、也不出现在成员集里（vhost-user 口静默不进 BD ← guest 无 L2 连通）。
	oldVSList, oldRefErrs := SwitchMembersOf(old, a.vhostDir, a.memifDir)
	newVSList, newRefErrs := SwitchMembersOf(new, a.vhostDir, a.memifDir)
	oldACLs := nameMap(old.Acls, func(x model.Acl) string { return x.Name })
	oldVSs := nameMap(oldVSList, func(x model.VirtualSwitch) string { return x.Name })
	oldVRFs := nameMap(old.Vrfs, func(x model.Vrf) string { return x.Name })
	oldVMs := nameMap(old.VirtualMachineFunctions, func(x model.VMFunction) string { return x.Name })
	oldCTs := nameMap(old.ContainerFunctions, func(x model.ContainerFunction) string { return x.Name })
	oldIfaces := nameMap(old.Interfaces, func(x model.InterfaceConfig) string { return x.Name })
	oldBonds := nameMap(old.Bonds, func(x model.Bond) string { return x.Name })
	oldPMs := nameMap(old.PortMirroring, func(x model.PortMirroring) string { return x.Name })
	oldQoS := nameMap(old.QosPolicies, func(x model.QosPolicy) string { return x.Name })

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
		}
	}
	for _, vrf := range new.Vrfs {
		if o, ok := oldVRFs[vrf.Name]; !ok || !configEqual(o, vrf) {
			ops = append(ops, applyOp(
				fmt.Sprintf("vrf[%s]", vrf.Name),
				func(ctx context.Context) error { return a.net.ApplyVRF(ctx, vrf) },
				vrf.Name, ok,
				func(ctx context.Context) error { return a.net.ApplyVRF(ctx, o) },
				func(ctx context.Context) error { return a.net.DeleteVRF(ctx, vrf.Name) },
			))
		}
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

	// —— 删除：容器 → VM → 网络（bd/vrf → span/qos → acl），与新增顺序相反 ——
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
	// vNIC 接入删除：在 bridge-domain 删除之后（BD 删除会摘除成员）；此时删除 vhost-user/memif 接口安全。
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

	for name := range oldVRFs {
		if _, ok := newVRFNames(new)[name]; !ok {
			vrf := oldVRFs[name]
			ops = append(ops, op{
				desc: fmt.Sprintf("del-vrf[%s]", name),
				run:  func(ctx context.Context) error { return a.net.DeleteVRF(ctx, name) },
				undo: func(ctx context.Context) error { return a.net.ApplyVRF(ctx, vrf) },
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
	return ops
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
