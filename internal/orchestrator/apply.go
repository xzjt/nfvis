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

// NewApplier 组合三类 Provider 构造编排器。资源池（内核 cpuset/大页）由 M3
// 内核编排接入后追加为第一阶段；M1 仅编排 网络→计算→容器。
func NewApplier(net NetworkProvider, comp ComputeProvider, cont ContainerProvider) Applier {
	return &orchApplier{net: net, comp: comp, cont: cont}
}

type orchApplier struct {
	net  NetworkProvider
	comp ComputeProvider
	cont ContainerProvider
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

// plan 生成操作序列：新增/变更在前（ACL→L2→L3→NAT/SPAN/QoS→VM→容器），
// 删除在后（容器→VM→L3/L2→NAT/SPAN/QoS→ACL），保证引用先建后删。
func (a *orchApplier) plan(old, new model.Config) []op {
	oldACLs := nameMap(old.Acls, func(x model.Acl) string { return x.Name })
	oldVSs := nameMap(old.VirtualSwitches, func(x model.VirtualSwitch) string { return x.Name })
	oldVRFs := nameMap(old.Vrfs, func(x model.Vrf) string { return x.Name })
	oldVMs := nameMap(old.VirtualMachineFunctions, func(x model.VMFunction) string { return x.Name })
	oldCTs := nameMap(old.ContainerFunctions, func(x model.ContainerFunction) string { return x.Name })
	oldPMs := nameMap(old.PortMirroring, func(x model.PortMirroring) string { return x.Name })
	oldQoS := nameMap(old.QosPolicies, func(x model.QosPolicy) string { return x.Name })

	var ops []op

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

	// —— 新增/变更：计算、容器 ——
	for _, vm := range new.VirtualMachineFunctions {
		if o, ok := oldVMs[vm.Name]; !ok || !configEqual(o, vm) {
			ops = append(ops, applyOp(
				fmt.Sprintf("vm[%s]", vm.Name),
				func(ctx context.Context) error { return a.comp.DefineVM(ctx, vm) },
				vm.Name, ok,
				func(ctx context.Context) error { return a.comp.DefineVM(ctx, o) },
				func(ctx context.Context) error { return a.comp.DeleteVM(ctx, vm.Name) },
			))
		}
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
				undo: func(ctx context.Context) error { return a.comp.DefineVM(ctx, vm) },
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
