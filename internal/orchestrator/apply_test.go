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
	bds    *[]model.VirtualSwitch // 非 nil 时记录每次 ApplyBridgeDomain 收到的交换机（含端口集合）
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
	if n.bds != nil {
		*n.bds = append(*n.bds, vs)
	}
	return n.record("bd:" + vs.Name)
}
func (n recNet) DeleteBridgeDomain(ctx context.Context, name string) error {
	return n.record("del-bd:" + name)
}
func (n recNet) ApplyDhcpRelay(ctx context.Context, vs model.VirtualSwitch) error {
	return n.record("dhcp-relay:" + vs.Name)
}
func (n recNet) ApplyDNSProxy(ctx context.Context, servers []string) error {
	return n.record("dns-proxy:" + strings.Join(servers, ","))
}
func (n recNet) ApplyVRF(ctx context.Context, vrf model.Vrf) error {
	return n.record("vrf:" + vrf.Name)
}
func (n recNet) DeleteVRF(ctx context.Context, name string) error { return n.record("del-vrf:" + name) }
func (n recNet) ApplyRoute(ctx context.Context, vrfName string, r model.Route) error {
	return n.record("route:" + routeLabel(vrfName, r))
}
func (n recNet) DeleteRoute(ctx context.Context, vrfName string, r model.Route) error {
	return n.record("del-route:" + routeLabel(vrfName, r))
}
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
func (n recNet) ApplyVnfInterface(ctx context.Context, port VnfPort) error {
	return n.record("vnf-if:" + port.VM + "/" + port.Interface)
}
func (n recNet) DeleteVnfInterface(ctx context.Context, vmName, ifaceName string) error {
	return n.record("del-vnf-if:" + vmName + "/" + ifaceName)
}

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
func (c recCompute) RefreshSeed(context.Context, model.VMFunction) error    { return nil }
func (c recCompute) StopVM(context.Context, string) error                   { return nil }
func (c recCompute) RestartVM(context.Context, string) error                { return nil }
func (c recCompute) VMState(context.Context, string) (string, error)        { return VMStateAbsent, nil }
func (c recCompute) EnsureConsistent(context.Context, model.Config) []error { return nil }
func (c recCompute) CheckVMAlarms(context.Context, model.Config) []error    { return nil }

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
func (c recContainer) StartContainer(context.Context, string) error                   { return nil }
func (c recContainer) StopContainer(context.Context, string) error                    { return nil }
func (c recContainer) RestartContainer(context.Context, string) error                 { return nil }
func (c recContainer) ContainerState(context.Context, string) (string, error) {
	return CTStateAbsent, nil
}
func (c recContainer) ContainerLogs(context.Context, string, int) (string, error) { return "", nil }
func (c recContainer) CheckContainerAlarms(context.Context, model.Config) []error { return nil }

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
	// 顺序：ACL 先于交换机（端口绑定引用 ACL），网络先于计算/容器（骨架 §3.3：网络→计算→容器）；
	// dhcp-relay 是 bridge-domain 之后的伴随操作（决策 #335，先有 BVI 地址与表才有 relay）。
	want := []string{"acl:acl-1", "bd:vs-1", "dhcp-relay:vs-1", "vrf:vrf-1", "vm:vm-1", "ct:ct-1"}
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
		Bonds:                   []model.Bond{{Name: "bond-old", Members: []string{"ens192"}}},
		VirtualMachineFunctions: []model.VMFunction{vmOf("vm-old")},
	}
	newCfg := model.Config{}
	if err := ap.Apply(context.Background(), old, newCfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !hasCall(*calls, "del-bd:vs-old") || !hasCall(*calls, "del-acl:acl-old") || !hasCall(*calls, "del-vm:vm-old") {
		t.Fatalf("删除操作缺失: %v", *calls)
	}
	// bond 删除必须下发到数据面（只从配置移除会留下 BondEthernetX 与成员关系）
	if !hasCall(*calls, "del-bond:bond-old") {
		t.Fatalf("bond 删除操作缺失: %v", *calls)
	}
	// 交换机/VNF 删除先于 ACL 删除（绑定解挂后再删 ACL），bond 删除先于其成员口相关的解挂
	bdIdx, aclIdx, bondIdx := -1, -1, -1
	for i, c := range *calls {
		switch c {
		case "del-bd:vs-old":
			bdIdx = i
		case "del-acl:acl-old":
			aclIdx = i
		case "del-bond:bond-old":
			bondIdx = i
		}
	}
	if bdIdx > aclIdx {
		t.Fatalf("删除顺序错误：bd 应先于 acl: %v", *calls)
	}
	if bondIdx < bdIdx {
		t.Fatalf("删除顺序错误：引用 bond 的交换机应先解除引用: %v", *calls)
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

// ---------- 决策 #170：VNF 侧 vNIC 声明与 bridge-domain 成员集必须合流 ----------

// lastBD 取记录中最后一次下发的指定交换机。
func lastBD(bds []model.VirtualSwitch, name string) (model.VirtualSwitch, bool) {
	for i := len(bds) - 1; i >= 0; i-- {
		if bds[i].Name == name {
			return bds[i], true
		}
	}
	return model.VirtualSwitch{}, false
}

// bdMemberIface 返回交换机成员集中该 vNIC 的 VPP 接口名（不在成员集则 ok=false）。
func bdMemberIface(vs model.VirtualSwitch, owner, nic string) (string, bool) {
	for _, p := range vs.Ports {
		switch {
		case p.Vnf == owner && p.VnfInterface == nic:
			return VnfIfaceName(p.Vnf, p.VnfInterface), true
		case p.Container == owner && p.ContainerInterface == nic:
			return MemifIfaceName(p.Container, p.ContainerInterface), true
		}
	}
	return "", false
}

// vmWithNic 构造一台只声明 vNIC 交换机归属的 VNF（不写交换机侧端口）。
func vmWithNic(vm, nic, vs string) model.VMFunction {
	return model.VMFunction{Name: vm, Image: "img",
		Interfaces: []model.VnfInterface{{Name: nic, Type: "vhost-user", VirtualSwitch: vs}}}
}

// newRecApplierCfg 记录 BD 下发的 applier。
func newRecApplierCfg() (Applier, *[]string, *[]model.VirtualSwitch) {
	calls := &[]string{}
	bds := &[]model.VirtualSwitch{}
	ap := NewApplier(recNet{calls: calls, bds: bds}, recCompute{calls: calls}, recContainer{calls: calls})
	return ap, calls, bds
}

// VNF 侧声明 virtual-switch 的 vNIC，其 vhost-user 口必须进入该交换机的 BD 成员集。
func TestApplyVnfNicDeclarationJoinsBridgeDomain(t *testing.T) {
	ap, _, bds := newRecApplierCfg()
	newCfg := model.Config{
		VirtualSwitches:         []model.VirtualSwitch{{Name: "vs-vnf", Type: "l2"}},
		VirtualMachineFunctions: []model.VMFunction{vmWithNic("vm-a", "eth0", "vs-vnf")},
	}
	if err := ap.Apply(context.Background(), model.Config{}, newCfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	vs, ok := lastBD(*bds, "vs-vnf")
	if !ok {
		t.Fatal("交换机未下发")
	}
	iface, ok := bdMemberIface(vs, "vm-a", "eth0")
	if !ok {
		t.Fatalf("vNIC 声明的 vhost 口应进入 BD 成员集: %+v", vs.Ports)
	}
	if iface != "vh-vm-a-eth0" {
		t.Fatalf("成员口接口名应为 vh-<vm>-<vnic>，实际 %s", iface)
	}
}

// vNIC 改挂另一台交换机：两台交换机都必须重新下发，旧成员集中不再含该口。
func TestApplyVnicReswitchDropsOldMember(t *testing.T) {
	ap, calls, bds := newRecApplierCfg()
	old := model.Config{
		VirtualSwitches: []model.VirtualSwitch{
			{Name: "vs-a", Type: "l2", Ports: []model.VSwitchPort{{Seq: 1, Interface: "ens192"}}},
			{Name: "vs-b", Type: "l2", Ports: []model.VSwitchPort{{Seq: 1, Interface: "ens224"}}},
		},
		VirtualMachineFunctions: []model.VMFunction{vmWithNic("vm-a", "eth0", "vs-a")},
	}
	newCfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{
			{Name: "vs-a", Type: "l2", Ports: []model.VSwitchPort{{Seq: 1, Interface: "ens192"}}},
			{Name: "vs-b", Type: "l2", Ports: []model.VSwitchPort{{Seq: 1, Interface: "ens224"}}},
		},
		VirtualMachineFunctions: []model.VMFunction{vmWithNic("vm-a", "eth0", "vs-b")},
	}
	if err := ap.Apply(context.Background(), old, newCfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !hasCall(*calls, "bd:vs-a") || !hasCall(*calls, "bd:vs-b") {
		t.Fatalf("两端交换机都应重新下发: %v", *calls)
	}
	if vsA, ok := lastBD(*bds, "vs-a"); !ok {
		t.Fatal("vs-a 未下发")
	} else if iface, still := bdMemberIface(vsA, "vm-a", "eth0"); still {
		t.Fatalf("原交换机成员集不应含该口（否则残留成员）: %s", iface)
	}
	if vsB, ok := lastBD(*bds, "vs-b"); !ok {
		t.Fatal("vs-b 未下发")
	} else if _, ok := bdMemberIface(vsB, "vm-a", "eth0"); !ok {
		t.Fatalf("新交换机成员集应含该口: %+v", vsB.Ports)
	}
}

// 删除 VNF：交换机本身没变也必须重新下发，成员集里不再含该 vhost 口。
func TestApplyVmDeleteDropsVnfMember(t *testing.T) {
	ap, calls, bds := newRecApplierCfg()
	old := model.Config{
		VirtualSwitches:         []model.VirtualSwitch{{Name: "vs-vnf", Type: "l2"}},
		VirtualMachineFunctions: []model.VMFunction{vmWithNic("vm-a", "eth0", "vs-vnf")},
	}
	newCfg := model.Config{VirtualSwitches: []model.VirtualSwitch{{Name: "vs-vnf", Type: "l2"}}}
	if err := ap.Apply(context.Background(), old, newCfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !hasCall(*calls, "bd:vs-vnf") {
		t.Fatalf("VNF 删除后交换机应重新下发以摘除成员: %v", *calls)
	}
	vs, ok := lastBD(*bds, "vs-vnf")
	if !ok {
		t.Fatal("交换机未下发")
	}
	if iface, still := bdMemberIface(vs, "vm-a", "eth0"); still {
		t.Fatalf("已删除 VNF 的 vhost 口不应留在成员集: %s", iface)
	}
	// vNIC 接口删除须在 BD 摘除之后（此时接口仍在，摘除才能成功）
	bdIdx, delIdx := -1, -1
	for i, c := range *calls {
		switch c {
		case "bd:vs-vnf":
			bdIdx = i
		case "del-vnf-if:vm-a/eth0":
			delIdx = i
		}
	}
	if bdIdx < 0 || delIdx < 0 || delIdx < bdIdx {
		t.Fatalf("顺序应为 摘除 BD 成员 → 删 vNIC 接口: %v", *calls)
	}
}

// 声明的交换机不存在：提交必须失败并点名，不得静默成功（也不得先做任何下发）。
func TestApplyUnresolvableVnicSwitchRefFailsLoud(t *testing.T) {
	ap, calls, _ := newRecApplierCfg()
	newCfg := model.Config{
		VirtualSwitches:         []model.VirtualSwitch{{Name: "vs-a", Type: "l2"}},
		VirtualMachineFunctions: []model.VMFunction{vmWithNic("vm-a", "eth0", "vs-ghost")},
	}
	err := ap.Apply(context.Background(), model.Config{}, newCfg)
	if err == nil || !strings.Contains(err.Error(), "vs-ghost") {
		t.Fatalf("应报虚拟交换机无法归位: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("归位失败时不应先下发其它对象: %v", *calls)
	}
}

// ---------- R84-28：静态路由的撤销（删路由叶子 / 删整台 L3 交换机） ----------
//
// 两条路径此前都漏：ApplyVRF 只下发声明里的路由（只加不撤），DeleteVRF 依赖删表而
// VPP 会保住仍被接口占用的表——残留路由因此长期留在 FIB 里（真机实测）。

// l3vrf 构造一台只声明静态路由的 L3 交换机（VRF 条目）。
func l3vrf(name string, routes ...model.Route) model.Vrf {
	return model.Vrf{Name: name, Routes: routes}
}

// 只删路由叶子（同名 L3 交换机仍在声明里）：被删的那条必须撤销，仍在声明的不误撤。
func TestApplyDeleteRouteLeafRevolvesOnlyThatRoute(t *testing.T) {
	ap, calls := newRecApplier("")
	kept := model.Route{Prefix: "0.0.0.0/0", NextHop: "10.99.88.254"}
	dropped := model.Route{Prefix: "10.99.89.0/24", NextHop: "10.99.89.2"}
	old := model.Config{Vrfs: []model.Vrf{l3vrf("vs-l3", kept, dropped)}}
	newCfg := model.Config{Vrfs: []model.Vrf{l3vrf("vs-l3", kept)}}
	if err := ap.Apply(context.Background(), old, newCfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !hasCall(*calls, "del-route:vs-l3 10.99.89.0/24 via 10.99.89.2") {
		t.Fatalf("被删的路由叶子应从数据面撤销: %v", *calls)
	}
	if hasCall(*calls, "del-route:vs-l3 0.0.0.0/0") {
		t.Fatalf("仍在声明的路由不得被误撤: %v", *calls)
	}
	if !hasCall(*calls, "vrf:vs-l3") {
		t.Fatalf("VRF 变更仍应按声明重下发: %v", *calls)
	}
}

// 整台 L3 交换机被删（连同其 VRF 条目）：该对象声明的路由必须先逐条撤，再删 VRF。
func TestApplyDeleteL3SwitchRevolvesItsRoutes(t *testing.T) {
	ap, calls := newRecApplier("")
	old := model.Config{
		VirtualSwitches: []model.VirtualSwitch{{Name: "vs-l3", Type: "l3"}},
		Vrfs:            []model.Vrf{l3vrf("vs-l3", model.Route{Prefix: "10.99.89.0/24", NextHop: "10.99.89.2"})},
	}
	if err := ap.Apply(context.Background(), old, model.Config{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !hasCall(*calls, "del-route:vs-l3 10.99.89.0/24 via 10.99.89.2") {
		t.Fatalf("交换机声明的静态路由应随对象撤销: %v", *calls)
	}
	routeIdx, vrfIdx := -1, -1
	for i, c := range *calls {
		switch {
		case c == "del-route:vs-l3 10.99.89.0/24 via 10.99.89.2":
			routeIdx = i
		case c == "del-vrf:vs-l3":
			vrfIdx = i
		}
	}
	// 撤销须在删 VRF 之前：表还在时逐条撤，不依赖删表（VPP 会保住仍被接口占用的表）
	if routeIdx < 0 || vrfIdx < 0 || routeIdx > vrfIdx {
		t.Fatalf("顺序应为 撤路由 → 删 VRF: %v", *calls)
	}
}

// 撤销失败时的补偿：把撤掉的路由加回来（全有或全无）。
func TestApplyRouteDeleteCompensatedOnFailure(t *testing.T) {
	ap, calls := newRecApplier("del-vrf:vs-l3")
	old := model.Config{
		VirtualSwitches: []model.VirtualSwitch{{Name: "vs-l3", Type: "l3"}},
		Vrfs:            []model.Vrf{l3vrf("vs-l3", model.Route{Prefix: "10.99.89.0/24", NextHop: "10.99.89.2"})},
	}
	if err := ap.Apply(context.Background(), old, model.Config{}); err == nil {
		t.Fatal("删 VRF 失败应整体报错")
	}
	if !hasCall(*calls, "route:vs-l3 10.99.89.0/24 via 10.99.89.2") {
		t.Fatalf("失败后应把已撤销的路由补偿回来: %v", *calls)
	}
}

// 同一前缀换下一跳：旧下一跳那条 path 必须撤销（否则 FIB 里留下两条并行的等价路径）。
func TestApplyRouteNextHopChangeRevolvesOldPath(t *testing.T) {
	ap, calls := newRecApplier("")
	old := model.Config{Vrfs: []model.Vrf{l3vrf("vs-l3", model.Route{Prefix: "10.99.89.0/24", NextHop: "10.99.89.2"})}}
	newCfg := model.Config{Vrfs: []model.Vrf{l3vrf("vs-l3", model.Route{Prefix: "10.99.89.0/24", NextHop: "10.99.89.3"})}}
	if err := ap.Apply(context.Background(), old, newCfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !hasCall(*calls, "del-route:vs-l3 10.99.89.0/24 via 10.99.89.2") {
		t.Fatalf("换下一跳时旧路径应撤销: %v", *calls)
	}
	if hasCall(*calls, "del-route:vs-l3 10.99.89.0/24 via 10.99.89.3") {
		t.Fatalf("新声明的下一跳不得被撤销: %v", *calls)
	}
}

// 只改 distance：VPP 侧下发不携带该字段，故不得判成「旧路由消失」而误撤仍在声明的路由。
func TestApplyRouteDistanceOnlyChangeDoesNotRevoke(t *testing.T) {
	ap, calls := newRecApplier("")
	old := model.Config{Vrfs: []model.Vrf{l3vrf("vs-l3", model.Route{Prefix: "10.99.89.0/24", NextHop: "10.99.89.2", Distance: 1})}}
	newCfg := model.Config{Vrfs: []model.Vrf{l3vrf("vs-l3", model.Route{Prefix: "10.99.89.0/24", NextHop: "10.99.89.2", Distance: 5})}}
	if err := ap.Apply(context.Background(), old, newCfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if hasCall(*calls, "del-route:") {
		t.Fatalf("只改 distance 不应撤销路由: %v", *calls)
	}
	if !hasCall(*calls, "vrf:vs-l3") {
		t.Fatalf("distance 变更仍应重下发该 VRF: %v", *calls)
	}
}

// R88-6 回归：删除顺序必须按依赖倒序——被 vNIC 引用的转发域（L3 交换机）先解引用，
// 再删 vNIC 本身。
//
// 真机 round88 现场：同一提交里「删 vnf-b」+「删把它的 vNIC 当 l3-interface 的 vs-nat」，
// 旧顺序先删 vNIC → del-vrf 清该接口地址时 VPP 报 Invalid sw_if_index(-2) → 整次提交
// 失败并回滚（用户一次清不干净，只能拆两次提交）。修法：del-vnf-if 排到 del-vrf 之后。
func TestApplyVnicDeleteAfterVrfThatReferencesIt(t *testing.T) {
	ap, calls := newRecApplier("")
	old := model.Config{
		VirtualMachineFunctions: []model.VMFunction{{Name: "vnf-a", Image: "img",
			Interfaces: []model.VnfInterface{{Name: "eth0", Type: "vhost-user"}}}},
		Vrfs: []model.Vrf{{Name: "vs-nat",
			L3Interfaces: []model.L3Interface{{Interface: "vh-vnf-a-eth0", Addresses: []string{"192.168.200.1/24"}}}}},
	}
	newCfg := model.Config{}
	if err := ap.Apply(context.Background(), old, newCfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	vrfIdx, ifIdx := -1, -1
	for i, c := range *calls {
		switch c {
		case "del-vrf:vs-nat":
			vrfIdx = i
		case "del-vnf-if:vnf-a/eth0":
			ifIdx = i
		}
	}
	if vrfIdx < 0 || ifIdx < 0 {
		t.Fatalf("应同时出现 del-vrf 与 del-vnf-if: %v", *calls)
	}
	if ifIdx < vrfIdx {
		t.Fatalf("删除顺序错误：引用 vNIC 的 L3 交换机（del-vrf）必须先于 vNIC 删除（del-vnf-if）: %v", *calls)
	}
}
