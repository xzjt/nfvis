package orchestrator

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
)

// recProviders 记录下发调用序列的 mock Provider（验证依赖顺序与失败补偿）。
type recNet struct {
	calls    *[]string
	failOn   string
	bds      *[]model.VirtualSwitch // 非 nil 时记录每次 ApplyBridgeDomain 收到的交换机（含端口集合）
	dnsProxy *DNSProxyUpstreams     // 非 nil 时记录最近一次 ApplyDNSProxy 收到的声明
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
func (n recNet) ApplyDHCPServer(ctx context.Context, vs model.VirtualSwitch) error {
	return n.record("dhcp-server:" + vs.Name)
}
func (n recNet) ApplyDNSProxy(ctx context.Context, want DNSProxyUpstreams) error {
	if n.dnsProxy != nil {
		*n.dnsProxy = want
	}
	return n.record("dns-proxy")
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
func (n recNet) UnbindL3IfaceACL(ctx context.Context, vrfName string, li model.L3Interface) error {
	return n.record("l3-acl-unbind:" + vrfName + "/" + l3IfaceKeyOf(li))
}
func (n recNet) DeleteL3Interface(ctx context.Context, vrfName string, li model.L3Interface) error {
	return n.record("del-l3-if:" + vrfName + "/" + l3IfaceKeyOf(li))
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
func (c recContainer) ContainerExec(context.Context, string, string, time.Duration) (ExecResult, error) {
	return ExecResult{}, nil
}
func (c recContainer) ContainerShell(context.Context, string) (io.ReadWriteCloser, error) {
	return nil, nil
}
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
	// dhcp-relay 与 dhcp-server 是 bridge-domain 之后的伴随操作（决策 #335/#359：
	// 先有 BVI 地址与表才有 relay/server；server 的 tap 还要入 BD）。
	want := []string{"acl:acl-1", "bd:vs-1", "dhcp-relay:vs-1", "dhcp-server:vs-1", "vrf:vrf-1", "vm:vm-1", "ct:ct-1"}
	if len(*calls) != len(want) {
		t.Fatalf("调用数不符: %v", *calls)
	}
	for i, w := range want {
		if (*calls)[i] != w {
			t.Fatalf("第 %d 步应为 %s，实际 %s（全部: %v）", i, w, (*calls)[i], *calls)
		}
	}
}

// 决策 #345：改动 vpp.dns_proxy_servers / 交换机 dns_proxy_servers 应产生 dns-proxy 伴随操作，
// 并把完整声明（全局 + 按域）交给 provider；声明未变不产生操作。
func TestApplyDNSProxyPlan(t *testing.T) {
	dnsProxy := &DNSProxyUpstreams{}
	calls := &[]string{}
	ap := NewApplier(
		recNet{calls: calls, dnsProxy: dnsProxy},
		recCompute{calls: calls},
		recContainer{calls: calls},
	)
	newCfg := model.Config{
		Vpp:             &model.VppConfig{DNSProxyServers: []string{"8.8.8.8"}},
		VirtualSwitches: []model.VirtualSwitch{{Name: "vs-dns", Type: "l2", DNSProxyServers: []string{"10.0.0.53"}}},
	}
	if err := ap.Apply(context.Background(), model.Config{}, newCfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(dnsProxy.Global) != 1 || dnsProxy.Global[0] != "8.8.8.8" {
		t.Fatalf("全局上游应传给 provider: %+v", dnsProxy)
	}
	if len(dnsProxy.PerSwitch["vs-dns"]) != 1 || dnsProxy.PerSwitch["vs-dns"][0] != "10.0.0.53" {
		t.Fatalf("按域上游应传给 provider: %+v", dnsProxy)
	}
	// 声明未变：不重发 dns-proxy（plan 里 reflect.DeepEqual 判定）
	*dnsProxy = DNSProxyUpstreams{}
	if err := ap.Apply(context.Background(), newCfg, newCfg); err != nil {
		t.Fatalf("Apply(noop): %v", err)
	}
	if dnsProxy.Global != nil || dnsProxy.PerSwitch != nil {
		t.Fatalf("声明未变不应重发 dns-proxy: %+v", dnsProxy)
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
	// 决策 #342：ACL 解绑/删除**先于**被引用对象的删除（bd/vrf/VM）——ACL 删除内含解绑引用它的
	// 接口，接口先被删则解绑必撞 VPP -2 并整次回滚（round117/119/120）。bond 删除先于其成员口相关的解挂。
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
	if aclIdx < 0 || bdIdx < 0 || aclIdx > bdIdx {
		t.Fatalf("删除顺序错误：ACL 应先于其引用对象 bd/vrf/VM: %v", *calls)
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
// 决策 #382（round163 真机暴露）：**同前缀改下一跳不得撤旧**——del-route 段在 ApplyVRF **之后**
// 执行，会把刚下发的新路由删掉（真机实证：`10.99.1.0/24` 由 ECMP 两跳改回单跳后**不在任何表里**，
// 而配置与读视图一切正常——数据面黑洞）。正确语义：VPP 对同前缀是**替换**，由 ApplyVRF 重新下发即可；
// 只有**前缀**不再声明才发 del-route。旧行为（本用例此前断言「换下一跳时旧路径应撤销」）已按真机结论更正。
func TestApplyRouteNextHopChangeKeepsRoute(t *testing.T) {
	cases := []struct {
		name   string
		oldNH  string
		newNH  string
	}{
		{"单值→单值（改地址）", "10.99.89.2", "10.99.89.3"},
		{"单值→多值（转 ECMP）", "10.99.89.2", "10.99.89.2,10.99.89.3"},
		{"多值→单值（收窄 ECMP）", "10.99.89.2,10.99.89.3", "10.99.89.2"},
		{"多值增删", "10.99.89.2,10.99.89.3", "10.99.89.3,10.99.89.4"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ap, calls := newRecApplier("")
			old := model.Config{Vrfs: []model.Vrf{l3vrf("vs-l3", model.Route{Prefix: "10.99.89.0/24", NextHop: c.oldNH})}}
			newCfg := model.Config{Vrfs: []model.Vrf{l3vrf("vs-l3", model.Route{Prefix: "10.99.89.0/24", NextHop: c.newNH})}}
			if err := ap.Apply(context.Background(), old, newCfg); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if hasCall(*calls, "del-route:") {
				t.Fatalf("同前缀改下一跳不得撤销路由（由 ApplyVRF 替换下发）: %v", *calls)
			}
			if !hasCall(*calls, "vrf:vs-l3") {
				t.Fatalf("同前缀改下一跳仍应经 ApplyVRF 重下发: %v", *calls)
			}
		})
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

// ---------- 决策 #361：L3 接口 ACL 绑定的撤销（差分纯函数与计划顺序） ----------
//
// 缺陷形态（round142 实测）：`delete … l3-interface <if> acl-in <acl>` 或整条删 l3-interface
// 提交成功、读视图干净，而 VPP 侧绑定仍在（deny 继续拦）、口留原表、地址不摘。修法是提交编排
// 按旧/新声明求差分，构造 l3-acl-unbind 与 del-l3-if 两类撤销操作。

// l3if 构造一条 l3-interface 声明（aclIn 为空表示未绑定）。
func l3if(ifname, aclIn string, addrs ...string) model.L3Interface {
	return model.L3Interface{Interface: ifname, Addresses: addrs, AclIn: aclIn}
}

// ifaceKeys 取差分结果的接口身份（与 plan 的描述标签同源）。
func ifaceKeys(ifs []model.L3Interface) []string {
	out := make([]string, 0, len(ifs))
	for _, li := range ifs {
		out = append(out, l3IfaceKeyOf(li))
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestL3RevokeDiffs(t *testing.T) {
	cases := []struct {
		name        string
		old, new    model.Vrf
		wantRemoved []string // removedL3Ifaces（del-l3-if：整条回收）
		wantUnbind  []string // l3ACLUnbinds（l3-acl-unbind：只撤绑定）
	}{
		{
			name: "只改地址：两类撤销都不触发",
			old:  model.Vrf{Name: "vs", L3Interfaces: []model.L3Interface{l3if("ens192", "web", "10.0.0.1/24")}},
			new:  model.Vrf{Name: "vs", L3Interfaces: []model.L3Interface{l3if("ens192", "web", "10.0.0.2/24")}},
		},
		{
			name: "acl-in 换成另一个非空：不触发解绑（绑定是替换语义，ApplyVRF 自己覆盖）",
			old:  model.Vrf{Name: "vs", L3Interfaces: []model.L3Interface{l3if("ens192", "web")}},
			new:  model.Vrf{Name: "vs", L3Interfaces: []model.L3Interface{l3if("ens192", "db")}},
		},
		{
			name: "接口整条删除：只进回收集合（其 ACL 由回收路径一并解绑，不重复进解绑集合）",
			old: model.Vrf{Name: "vs", L3Interfaces: []model.L3Interface{
				l3if("ens192", "web"), l3if("ens224", "web")}},
			new:         model.Vrf{Name: "vs", L3Interfaces: []model.L3Interface{l3if("ens192", "web")}},
			wantRemoved: []string{"ens224"},
		},
		{
			name:       "acl-in 被清（接口仍在）：进解绑集合",
			old:        model.Vrf{Name: "vs", L3Interfaces: []model.L3Interface{l3if("ens192", "web")}},
			new:        model.Vrf{Name: "vs", L3Interfaces: []model.L3Interface{l3if("ens192", "")}},
			wantUnbind: []string{"ens192"},
		},
		{
			name:        "旧声明绑了 ACL 且接口整条删：进回收集合",
			old:         model.Vrf{Name: "vs", L3Interfaces: []model.L3Interface{l3if("ens192", "web")}},
			new:         model.Vrf{Name: "vs"},
			wantRemoved: []string{"ens192"},
		},
		{
			name: "vlan 子接口按 vlan 区分身份：换 vlan 视作旧子接口被删",
			old: model.Vrf{Name: "vs", L3Interfaces: []model.L3Interface{
				{Interface: "ens192", Vlan: 100, AclIn: "web"}}},
			new: model.Vrf{Name: "vs", L3Interfaces: []model.L3Interface{
				{Interface: "ens192", Vlan: 200, AclIn: "web"}}},
			wantRemoved: []string{"ens192.100"},
		},
		{
			name: "去重且保持声明序",
			old: model.Vrf{Name: "vs", L3Interfaces: []model.L3Interface{
				l3if("ens224", "web"), l3if("ens192", "web"), l3if("ens224", "web")}},
			new:         model.Vrf{Name: "vs"},
			wantRemoved: []string{"ens224", "ens192"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ifaceKeys(removedL3Ifaces(tc.old, tc.new)); !equalStrings(got, tc.wantRemoved) {
				t.Fatalf("removedL3Ifaces = %v, want %v", got, tc.wantRemoved)
			}
			if got := ifaceKeys(l3ACLUnbinds(tc.old, tc.new)); !equalStrings(got, tc.wantUnbind) {
				t.Fatalf("l3ACLUnbinds = %v, want %v", got, tc.wantUnbind)
			}
		})
	}
}

// 计划顺序：l3-acl-unbind / del-l3-if 排在**本 VRF 的 ApplyVRF 之后、删除段的 del-acl 之前**
// （先解引用、后删被引用，#342 同族——否则删 ACL 会撞上仍指向它的陈旧绑定）。
func TestApplyPlanL3RevokeBeforeDeleteACL(t *testing.T) {
	calls := &[]string{}
	oa, ok := NewApplier(recNet{calls: calls}, recCompute{calls: calls}, recContainer{calls: calls}).(*orchApplier)
	if !ok {
		t.Fatal("NewApplier 应返回 *orchApplier")
	}
	old := model.Config{
		Acls: []model.Acl{{Name: "web", Rules: []model.AclRule{{Seq: 10, Action: "deny", Source: "any", Destination: "any"}}}},
		Vrfs: []model.Vrf{{Name: "vs-l3", L3Interfaces: []model.L3Interface{
			l3if("ens192", "web", "10.0.0.1/24"), // acl-in 被清
			l3if("ens224", "web", "10.0.1.1/24"), // 整条被删
		}}},
	}
	newCfg := model.Config{
		Vrfs: []model.Vrf{{Name: "vs-l3", L3Interfaces: []model.L3Interface{
			l3if("ens192", "", "10.0.0.1/24"),
		}}},
	}
	ops := oa.plan(old, newCfg)
	descs := make([]string, 0, len(ops))
	for _, o := range ops {
		descs = append(descs, o.desc)
	}
	pos := func(desc string) int {
		for i, d := range descs {
			if d == desc {
				return i
			}
		}
		return -1
	}
	vrfI, unbindI, delIfI, delAclI := pos("vrf[vs-l3]"), pos("l3-acl-unbind[vs-l3/ens192]"),
		pos("del-l3-if[vs-l3/ens224]"), pos("del-acl[web]")
	if vrfI < 0 || unbindI < 0 || delIfI < 0 || delAclI < 0 {
		t.Fatalf("计划缺少预期操作（vrf=%d unbind=%d del-if=%d del-acl=%d）: %v",
			vrfI, unbindI, delIfI, delAclI, descs)
	}
	if !(vrfI < unbindI && vrfI < delIfI) {
		t.Fatalf("撤销应排在本 VRF 的 ApplyVRF 之后: %v", descs)
	}
	if !(unbindI < delAclI && delIfI < delAclI) {
		t.Fatalf("解引用（解绑/回收）应先于删 ACL: %v", descs)
	}
}

// 差分撤销真的接到 provider（run 接线与参数正确），执行顺序同计划断言。
func TestApplyL3RevokeExecutesBeforeDeleteACL(t *testing.T) {
	ap, calls := newRecApplier("")
	old := model.Config{
		Acls: []model.Acl{{Name: "web", Rules: []model.AclRule{{Seq: 10, Action: "deny", Source: "any", Destination: "any"}}}},
		Vrfs: []model.Vrf{{Name: "vs-l3", L3Interfaces: []model.L3Interface{
			l3if("ens192", "web", "10.0.0.1/24"),
			l3if("ens224", "web", "10.0.1.1/24"),
		}}},
	}
	newCfg := model.Config{
		Vrfs: []model.Vrf{{Name: "vs-l3", L3Interfaces: []model.L3Interface{
			l3if("ens192", "", "10.0.0.1/24"),
		}}},
	}
	if err := ap.Apply(context.Background(), old, newCfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	find := func(want string) int {
		for i, c := range *calls {
			if c == want {
				return i
			}
		}
		return -1
	}
	unbindI := find("l3-acl-unbind:vs-l3/ens192")
	delIfI := find("del-l3-if:vs-l3/ens224")
	delAclI := find("del-acl:web")
	if unbindI < 0 || delIfI < 0 || delAclI < 0 {
		t.Fatalf("撤销操作未下发（unbind=%d del-if=%d del-acl=%d）: %v", unbindI, delIfI, delAclI, *calls)
	}
	if !(unbindI < delAclI && delIfI < delAclI) {
		t.Fatalf("执行顺序错误：解引用应先于删 ACL: %v", *calls)
	}
}

// 决策 #361：NetworkProvider 新增的两个撤销方法在 noopNetwork 上为空操作（M1/M2 过渡实现
// 与接口必须同步——方法集由 NewNoopNetwork 的返回类型在编译期强制）。
func TestNoopNetworkL3Revoke(t *testing.T) {
	n := NewNoopNetwork()
	li := model.L3Interface{Interface: "ens192", Addresses: []string{"10.0.0.1/24"}, AclIn: "web"}
	if err := n.UnbindL3IfaceACL(context.Background(), "vs-l3", li); err != nil {
		t.Fatalf("noop UnbindL3IfaceACL: %v", err)
	}
	if err := n.DeleteL3Interface(context.Background(), "vs-l3", li); err != nil {
		t.Fatalf("noop DeleteL3Interface: %v", err)
	}
}
