package orchestrator

import (
	"context"
	"encoding/json"
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

	// vhostDir VNF vhost-user socket 目录（compute 与 network 必须一致，缺省 /run/nfvis/vhost）。
	vhostDir string
	// memifDir 容器 memif socket 目录（缺省 /run/nfvis/memif，FR-NET-022）。
	memifDir string
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
	oldACLs := nameMap(old.Acls, func(x model.Acl) string { return x.Name })
	oldVSs := nameMap(old.VirtualSwitches, func(x model.VirtualSwitch) string { return x.Name })
	oldVRFs := nameMap(old.Vrfs, func(x model.Vrf) string { return x.Name })
	oldVMs := nameMap(old.VirtualMachineFunctions, func(x model.VMFunction) string { return x.Name })
	oldCTs := nameMap(old.ContainerFunctions, func(x model.ContainerFunction) string { return x.Name })
	oldIfaces := nameMap(old.Interfaces, func(x model.InterfaceConfig) string { return x.Name })
	oldBonds := nameMap(old.Bonds, func(x model.Bond) string { return x.Name })
	oldPMs := nameMap(old.PortMirroring, func(x model.PortMirroring) string { return x.Name })
	oldQoS := nameMap(old.QosPolicies, func(x model.QosPolicy) string { return x.Name })

	var ops []op

	// —— VNF vNIC 接入（FR-NET-020/023）：必须先于 bridge-domain 下发，
	// 因为交换机端口按确定性接口名挂接 vhost-user 接口，接口需已存在。——
	oldPorts := collectVnfPorts(old)
	newPorts := collectVnfPorts(new)
	for _, p := range newPorts {
		oldP, inOld := oldPorts[p.key()]
		if inOld && configEqual(oldP.nic, p.nic) {
			continue
		}
		p := p
		newPort := a.toVnfPort(new, p)
		ops = append(ops, op{
			desc: fmt.Sprintf("vnf-if[%s]", p.key()),
			run:  func(ctx context.Context) error { return a.net.ApplyVnfInterface(ctx, newPort) },
			undo: func(ctx context.Context) error {
				if inOld {
					return a.net.ApplyVnfInterface(ctx, a.toVnfPort(old, oldP))
				}
				return a.net.DeleteVnfInterface(ctx, p.owner, p.nic.Name)
			},
		})
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
	for _, vs := range new.VirtualSwitches {
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
	for _, iface := range new.Interfaces {
		if o, ok := oldIfaces[iface.Name]; !ok || !configEqual(o, iface) {
			ops = append(ops, applyOp(
				fmt.Sprintf("interface[%s]", iface.Name),
				func(ctx context.Context) error { return a.net.ApplyInterface(ctx, iface) },
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
	// vNIC 接入删除：在 VM 删除之后（FR-NET-023）；VM 整体删除时其全部 vNIC 一并清理。
	for _, p := range oldPorts {
		if _, ok := newPorts[p.key()]; ok {
			continue
		}
		p := p
		oldPort := a.toVnfPort(old, p)
		ops = append(ops, op{
			desc: fmt.Sprintf("del-vnf-if[%s]", p.key()),
			run:  func(ctx context.Context) error { return a.net.DeleteVnfInterface(ctx, p.owner, p.nic.Name) },
			undo: func(ctx context.Context) error { return a.net.ApplyVnfInterface(ctx, oldPort) },
		})
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

// vnfPortRef 一个 VNF/容器的 vNIC 接入引用（确定性排序用）。
type vnfPortRef struct {
	owner     string // VM 名或容器名
	container bool   // true = 容器 memif vNIC
	nic       model.VnfInterface
}

func (p vnfPortRef) key() string { return p.owner + "/" + p.nic.Name }

// collectVnfPorts 收集需 VPP 侧接入的 vNIC：VM 的 vhost-user 与容器的 memif
// （按属主名、vNIC 名确定性排序）。sriov-vf 不经 VPP，不在此列。
func collectVnfPorts(cfg model.Config) map[string]vnfPortRef {
	out := map[string]vnfPortRef{}
	for _, vm := range cfg.VirtualMachineFunctions {
		for _, nic := range vm.Interfaces {
			if nic.Type != "vhost-user" {
				continue
			}
			ref := vnfPortRef{owner: vm.Name, nic: nic}
			out[ref.key()] = ref
		}
	}
	for _, ct := range cfg.ContainerFunctions {
		for _, nic := range ct.Interfaces {
			if nic.Type != "memif" {
				continue
			}
			ref := vnfPortRef{owner: ct.Name, container: true, nic: nic}
			out[ref.key()] = ref
		}
	}
	return out
}

// toVnfPort 组装下发用的 VnfPort（socket 路径与 compute/container 侧同源派生）。
func (a *orchApplier) toVnfPort(cfg model.Config, ref vnfPortRef) VnfPort {
	vrf := ""
	if isL3Switch(cfg, ref.nic.VirtualSwitch) {
		vrf = ref.nic.VirtualSwitch // L3 交换机与同名 VRF 对应（附录 B）
	}
	sock := VnfSocketPath(a.vhostDir, ref.owner, ref.nic.Name)
	if ref.nic.Type == "memif" {
		sock = MemifSocketPath(a.memifDir, ref.owner, ref.nic.Name)
	}
	return VnfPort{
		VM:            ref.owner, // 容器场景下为容器名（VnfPort.VM 语义 = 属主名）
		Interface:     ref.nic.Name,
		Type:          ref.nic.Type,
		VirtualSwitch: ref.nic.VirtualSwitch,
		MAC:           ref.nic.MAC,
		VLAN:          ref.nic.Vlan,
		Socket:        sock,
		VRF:           vrf,
	}
}

func isL3Switch(cfg model.Config, name string) bool {
	if name == "" {
		return false
	}
	for _, vs := range cfg.VirtualSwitches {
		if vs.Name == name {
			return vs.Type == "l3"
		}
	}
	return false
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
