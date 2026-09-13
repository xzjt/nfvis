package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// recProviders 记录下发调用序列的 mock Provider（验证依赖顺序与失败补偿）。
type recNet struct {
	calls  *[]string
	failOn string
}

func (n recNet) record(op string) error {
	*n.calls = append(*n.calls, op)
	if n.failOn == op {
		return fmt.Errorf("模拟失败: %s", op)
	}
	return nil
}

func (n recNet) ApplyInterface(ctx context.Context, iface model.InterfaceConfig) error {
	return n.record("iface:" + iface.Name)
}
func (n recNet) ApplyBond(ctx context.Context, bond model.Bond) error {
	return n.record("bond:" + bond.Name)
}
func (n recNet) DeleteBond(ctx context.Context, name string) error {
	return n.record("del-bond:" + name)
}
func (n recNet) ApplyLLDP(ctx context.Context, lldp *model.LldpConfig) error {
	return n.record("lldp")
}
func (n recNet) ApplyACL(ctx context.Context, acl model.Acl) error {
	return n.record("acl:" + acl.Name)
}
func (n recNet) DeleteACL(ctx context.Context, name string) error { return n.record("del-acl:" + name) }
func (n recNet) ApplyBridgeDomain(ctx context.Context, vs model.VirtualSwitch) error {
	return n.record("bd:" + vs.Name)
}
func (n recNet) DeleteBridgeDomain(ctx context.Context, name string) error {
	return n.record("del-bd:" + name)
}
func (n recNet) ApplyVRF(ctx context.Context, vrf model.Vrf) error {
	return n.record("vrf:" + vrf.Name)
}
func (n recNet) DeleteVRF(ctx context.Context, name string) error { return n.record("del-vrf:" + name) }
func (n recNet) ApplyNAT(ctx context.Context, nat model.NatConfig) error {
	return n.record("nat")
}
func (n recNet) ApplySpan(ctx context.Context, pm model.PortMirroring) error {
	return n.record("span:" + pm.Name)
}
func (n recNet) DeleteSpan(ctx context.Context, name string) error {
	return n.record("del-span:" + name)
}
func (n recNet) ApplyQos(ctx context.Context, q model.QosPolicy) error {
	return n.record("qos:" + q.Name)
}
func (n recNet) DeleteQos(ctx context.Context, name string) error {
	return n.record("del-qos:" + name)
}
func (n recNet) EnsureConsistent(ctx context.Context, cfg model.Config) []error { return nil }

type recCompute struct {
	calls  *[]string
	failOn string
}

func (c recCompute) DefineVM(ctx context.Context, vm model.VMFunction, alloc model.AllocatedResources) error {
	*c.calls = append(*c.calls, "vm:"+vm.Name)
	if c.failOn == "vm:"+vm.Name {
		return fmt.Errorf("模拟失败: DefineVM %s", vm.Name)
	}
	return nil
}
func (c recCompute) DeleteVM(ctx context.Context, name string) error {
	*c.calls = append(*c.calls, "del-vm:"+name)
	return nil
}
func (c recCompute) StartVM(context.Context, string) error                  { return nil }
func (c recCompute) StopVM(context.Context, string) error                   { return nil }
func (c recCompute) RestartVM(context.Context, string) error                { return nil }
func (c recCompute) VMState(context.Context, string) (string, error)        { return VMStateAbsent, nil }
func (c recCompute) EnsureConsistent(context.Context, model.Config) []error { return nil }

type recContainer struct {
	calls *[]string
}

func (c recContainer) ApplyContainer(ctx context.Context, ct model.ContainerFunction) error {
	*c.calls = append(*c.calls, "ct:"+ct.Name)
	return nil
}
func (c recContainer) DeleteContainer(ctx context.Context, name string) error {
	*c.calls = append(*c.calls, "del-ct:"+name)
	return nil
}
func (c recContainer) EnsureConsistent(ctx context.Context, cfg model.Config) []error { return nil }

func newRecApplier(netFail string) (Applier, *[]string) {
	calls := &[]string{}
	ap := NewApplier(
		recNet{calls: calls, failOn: netFail},
		recCompute{calls: calls, failOn: netFail},
		recContainer{calls: calls},
	)
	return ap, calls
}

func hasCall(calls []string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func vmOf(name string) model.VMFunction {
	return model.VMFunction{Name: name, Image: "img", VCPU: model.VMCpu{Count: 1}, Memory: model.VMMemory{SizeMB: 1024}}
}

func TestApplyOrderNetworkBeforeCompute(t *testing.T) {
	ap, calls := newRecApplier("")
	old := model.Config{}
	newCfg := model.Config{
		Acls:                    []model.Acl{{Name: "acl-1", Rules: []model.AclRule{{Seq: 10, Action: "permit"}}}},
		VirtualSwitches:         []model.VirtualSwitch{{Name: "vs-1", Type: "l2"}},
		Vrfs:                    []model.Vrf{{Name: "vrf-1"}},
		VirtualMachineFunctions: []model.VMFunction{vmOf("vm-1")},
		ContainerFunctions:      []model.ContainerFunction{{Name: "ct-1", Image: "img"}},
	}
	if err := ap.Apply(context.Background(), old, newCfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// 顺序：ACL 先于交换机（端口绑定引用 ACL），网络先于计算/容器（骨架 §3.3：网络→计算→容器）
	want := []string{"acl:acl-1", "bd:vs-1", "vrf:vrf-1", "vm:vm-1", "ct:ct-1"}
	if len(*calls) != len(want) {
		t.Fatalf("调用数不符: %v", *calls)
	}
	for i, w := range want {
		if (*calls)[i] != w {
			t.Fatalf("第 %d 步应为 %s，实际 %s（全部: %v）", i, w, (*calls)[i], *calls)
		}
	}
}

func TestApplyRemovalsAfterAdds(t *testing.T) {
	ap, calls := newRecApplier("")
	old := model.Config{
		VirtualSwitches:         []model.VirtualSwitch{{Name: "vs-old", Type: "l2"}},
		Acls:                    []model.Acl{{Name: "acl-old", Rules: []model.AclRule{{Seq: 10, Action: "permit"}}}},
		VirtualMachineFunctions: []model.VMFunction{vmOf("vm-old")},
	}
	newCfg := model.Config{}
	if err := ap.Apply(context.Background(), old, newCfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !hasCall(*calls, "del-bd:vs-old") || !hasCall(*calls, "del-acl:acl-old") || !hasCall(*calls, "del-vm:vm-old") {
		t.Fatalf("删除操作缺失: %v", *calls)
	}
	// 交换机/VNF 删除先于 ACL 删除（绑定解挂后再删 ACL）
	bdIdx, aclIdx := -1, -1
	for i, c := range *calls {
		if c == "del-bd:vs-old" {
			bdIdx = i
		}
		if c == "del-acl:acl-old" {
			aclIdx = i
		}
	}
	if bdIdx > aclIdx {
		t.Fatalf("删除顺序错误：bd 应先于 acl: %v", *calls)
	}
}

func TestApplyFailureCompensates(t *testing.T) {
	ap, calls := newRecApplier("vm:vm-1")
	old := model.Config{}
	newCfg := model.Config{
		Acls:                    []model.Acl{{Name: "acl-1", Rules: []model.AclRule{{Seq: 10, Action: "permit"}}}},
		VirtualSwitches:         []model.VirtualSwitch{{Name: "vs-1", Type: "l2"}},
		VirtualMachineFunctions: []model.VMFunction{vmOf("vm-1")},
	}
	err := ap.Apply(context.Background(), old, newCfg)
	if err == nil || !strings.Contains(err.Error(), "vm-1") {
		t.Fatalf("应返回 DefineVM 失败错误: %v", err)
	}
	// 已执行的 bd/acl 下发必须被补偿，底座回到变更前状态（骨架 §3.3 全有或全无）
	if !hasCall(*calls, "del-bd:vs-1") || !hasCall(*calls, "del-acl:acl-1") {
		t.Fatalf("失败后应逆序补偿已执行操作: %v", *calls)
	}
	if hasCall(*calls, "ct:") {
		t.Fatalf("失败后不应继续后续阶段: %v", *calls)
	}
}

func TestApplyUpdateCompensatesToOldState(t *testing.T) {
	ap, calls := newRecApplier("vm:vm-1")
	old := model.Config{
		VirtualSwitches: []model.VirtualSwitch{{Name: "vs-1", Type: "l2", VlanAccess: 100}},
	}
	newCfg := model.Config{
		VirtualSwitches:         []model.VirtualSwitch{{Name: "vs-1", Type: "l2", VlanAccess: 200}},
		VirtualMachineFunctions: []model.VMFunction{vmOf("vm-1")},
	}
	_ = ap.Apply(context.Background(), old, newCfg)
	// vs-1 变更：先下发新状态，失败后补偿重新下发旧配置（而非删除）
	if !hasCall(*calls, "bd:vs-1") {
		t.Fatalf("补偿应重新下发旧配置: %v", *calls)
	}
}

func TestApplyNoChangesNoCalls(t *testing.T) {
	ap, calls := newRecApplier("")
	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{{Name: "vs-1", Type: "l2", VlanAccess: 100}},
	}
	if err := ap.Apply(context.Background(), cfg, cfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("无差异不应产生调用: %v", *calls)
	}
}
