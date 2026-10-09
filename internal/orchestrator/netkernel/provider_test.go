package netkernel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// fakeRunner 记录命令并按「命令前缀 → 输出」返回预置结果（单测不触碰宿主内核）。
type fakeRunner struct {
	calls []string
	// replies 键为「name arg1 arg2 ...」的前缀匹配（最长前缀优先），值为输出与错误。
	replies []fakeReply
	failAll error
}

type fakeReply struct {
	prefix string
	out    string
	err    error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, line)
	if f.failAll != nil {
		return "", f.failAll
	}
	// 最长前缀优先（越具体的规则越先命中）。
	best := -1
	var hit *fakeReply
	for i := range f.replies {
		r := &f.replies[i]
		if strings.HasPrefix(line, r.prefix) && len(r.prefix) > best {
			best, hit = len(r.prefix), r
		}
	}
	if hit != nil {
		return hit.out, hit.err
	}
	return "", nil
}

func (f *fakeRunner) has(substr string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func (f *fakeRunner) joined() string { return strings.Join(f.calls, "\n") }

// ---------- 命名与派生 ----------

func TestLinkNameTruncatesToIfnamsiz(t *testing.T) {
	if got := LinkName("vs-lan"); got != "vs-lan" {
		t.Fatalf("短名应原样保留，得到 %q", got)
	}
	long := "a-very-long-virtual-switch-name"
	got := LinkName(long)
	if len(got) > ifnameMax {
		t.Fatalf("超长名应截断到 %d，得到 %q（%d）", ifnameMax, got, len(got))
	}
	if LinkName(long) != got {
		t.Fatalf("同一输入必须得到同一名字（确定性）")
	}
	if LinkName("b-very-long-virtual-switch-name") == got {
		t.Fatalf("不同长名不应撞名")
	}
}

func TestVRFTableIDDeterministicAndPositive(t *testing.T) {
	a, b := VRFTableID("vs-l3"), VRFTableID("vs-l3")
	if a != b {
		t.Fatalf("同一 VRF 名必须派生同一表号")
	}
	if a <= 0 {
		t.Fatalf("表号必须为正，得到 %d", a)
	}
	if VRFTableID("vs-other") == a {
		t.Fatalf("不同 VRF 名不应撞表号")
	}
}

func TestPoolRangeConversion(t *testing.T) {
	if got := poolRange("10.0.0.1 to 10.0.0.10"); got != "10.0.0.1-10.0.0.10" {
		t.Fatalf("区间写法应转成 nft 形态，得到 %q", got)
	}
	if got := poolRange("10.0.0.1"); got != "10.0.0.1" {
		t.Fatalf("单地址应原样返回，得到 %q", got)
	}
}

// ---------- 接口与 bond ----------

func TestApplyInterfaceSetsMTUDescriptionAndState(t *testing.T) {
	// 接口级绑定族的收尾会走一次端口安全 Teardown（未声明=撤除）：它要读桥成员状态决定是否
	// 恢复学习（R2-22 起读失败会如实上抛），故这里给一条正常的 `ip -j -d link show` 事实。
	// 该口不是桥成员：没有学习可恢复，撤除照常按已达成。
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -j -d link show dev ens192",
		out:    `[{"ifname":"ens192","linkinfo":{}}]`,
	}}}
	p := New(f)
	enabled := true
	err := p.ApplyInterface(context.Background(), model.InterfaceConfig{
		Name: "ens192", MTU: 9000, Description: "wan", Enabled: &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ip link set dev ens192 mtu 9000",
		"ip link set dev ens192 alias wan",
		"ip link set dev ens192 up",
	} {
		if !f.has(want) {
			t.Fatalf("缺少命令 %q；实际：\n%s", want, f.joined())
		}
	}
}

func TestApplyBondStaticUsesBalanceXorAndEnslavesMembers(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	err := p.ApplyBond(context.Background(), model.Bond{
		Name: "bond0", Members: []string{"ens192", "ens224"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.has("ip link add name bond0 type bond mode balance-xor") {
		t.Fatalf("静态聚合应映射为 balance-xor；实际：\n%s", f.joined())
	}
	if !f.has("ip link set dev ens192 master bond0") || !f.has("ip link set dev ens224 master bond0") {
		t.Fatalf("成员口应被 enslave；实际：\n%s", f.joined())
	}
}

func TestApplyBondLacpUses803ad(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	err := p.ApplyBond(context.Background(), model.Bond{
		Name: "bond0", Members: []string{"ens192"},
		Lacp: &model.Lacp{Mode: "active", Interval: "fast"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.has("type bond mode 802.3ad") || !f.has("lacp_rate fast") {
		t.Fatalf("LACP 应映射为 802.3ad + lacp_rate；实际：\n%s", f.joined())
	}
}

func TestDeleteBondToleratesMissingDevice(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip link del bond0",
		out:    "Cannot find device \"bond0\"",
		err:    errors.New("exit status 1"),
	}}}
	p := New(f)
	if err := p.DeleteBond(context.Background(), "bond0"); err != nil {
		t.Fatalf("设备不存在应按已达成处理，得到 %v", err)
	}
}

// ---------- L2 交换机（内核 bridge） ----------

func TestApplyBridgeDomainCreatesBridgeAndEnslavesPorts(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	err := p.ApplyBridgeDomain(context.Background(), model.VirtualSwitch{
		Name: "vs-lan", Type: "l2", VlanAccess: 100,
		Ports: []model.VSwitchPort{{Seq: 1, Interface: "ens192"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ip link add name vs-lan type bridge",
		"ip link set dev vs-lan type bridge vlan_filtering 1",
		"ip link set dev ens192 master vs-lan",
		"bridge vlan add dev ens192 vid 100 pvid untagged",
		"ip link set dev vs-lan up",
	} {
		if !f.has(want) {
			t.Fatalf("缺少命令 %q；实际：\n%s", want, f.joined())
		}
	}
}

func TestApplyBridgeDomainSkipsL3Switch(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	if err := p.ApplyBridgeDomain(context.Background(), model.VirtualSwitch{Name: "vs-l3", Type: "l3"}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("L3 交换机不应建 bridge，实际执行了：%s", f.joined())
	}
}

func TestApplyBridgeDomainReleasesRemovedMembers(t *testing.T) {
	// 现状：bridge 上挂着 ens224（配置里已删），apply 后应把它摘掉。
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "bridge -j link show master vs-lan",
			out: `[{"ifname":"ens192","master":"vs-lan"},{"ifname":"ens224","master":"vs-lan"}]`},
		// 物理桥口：linkinfo 里只有 info_slave_kind（没有 info_kind）——属产品可释放的成员。
		{prefix: "ip -d -j link show dev ens224",
			out: `[{"ifname":"ens224","master":"vs-lan","linkinfo":{"info_slave_kind":"bridge"}}]`},
	}}
	p := New(f)
	err := p.ApplyBridgeDomain(context.Background(), model.VirtualSwitch{
		Name: "vs-lan", Type: "l2",
		Ports: []model.VSwitchPort{{Seq: 1, Interface: "ens192"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.has("ip link set dev ens224 nomaster") {
		t.Fatalf("已从声明里删除的成员口应被释放；实际：\n%s", f.joined())
	}
	if f.has("ip link set dev ens192 nomaster") {
		t.Fatalf("仍声明的成员口不应被释放；实际：\n%s", f.joined())
	}
}

func TestApplyBridgeDomainGatewayAddressOnBridgeAndInVRF(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	err := p.ApplyBridgeDomain(context.Background(), model.VirtualSwitch{
		Name: "vs-lan", Type: "l2",
		Gateway: &model.VSGateway{Addresses: []string{"192.168.99.1/24"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.has("ip link add name vr-vs-lan type vrf table") {
		t.Fatalf("网关应落到专属 VRF；实际：\n%s", f.joined())
	}
	if !f.has("ip link set dev vs-lan master vr-vs-lan") {
		t.Fatalf("bridge 应入网关 VRF；实际：\n%s", f.joined())
	}
	if !f.has("ip addr replace 192.168.99.1/24 dev vs-lan") {
		t.Fatalf("网关地址应落在 bridge 上；实际：\n%s", f.joined())
	}
}

func TestDeleteBridgeDomainDeletesLink(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	if err := p.DeleteBridgeDomain(context.Background(), "vs-lan"); err != nil {
		t.Fatal(err)
	}
	if !f.has("ip link del vs-lan") {
		t.Fatalf("应删除 bridge；实际：\n%s", f.joined())
	}
}

// R2-1（P0）：释放循环只摘「本产品自己 enslave 的成员」——libvirt 自建的 VM tap（vnetN，
// linkinfo.info_kind=tun）与 ApplyVxlan 自管的隧道口（vxlan）绝不能摘：服务重启、升级、
// 甚至「给同一台交换机再加一个端口」的提交都会触发整台重放，摘掉后 VM 仍在跑、产品零报错，
// 宿主到 guest 的 L2 静默断流。
func TestApplyBridgeDomainKeepsLibvirtTapAndVxlanMembers(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "bridge -j link show master vs-lan", out: `[
			{"ifname":"ens192","master":"vs-lan"},
			{"ifname":"ens224","master":"vs-lan"},
			{"ifname":"vnet2","master":"vs-lan"},
			{"ifname":"vxlan7","master":"vs-lan"}]`},
		// 物理口：无 linkinfo（仍允许释放——它是本产品会 enslave 的那类）。
		{prefix: "ip -d -j link show dev ens224", out: `[{"ifname":"ens224","master":"vs-lan"}]`},
		// VM tap：libvirt 自建，产品从不由 ApplyBridgeDomain enslave。
		{prefix: "ip -d -j link show dev vnet2",
			out: `[{"ifname":"vnet2","master":"vs-lan","linkinfo":{"info_kind":"tun"}}]`},
		// vxlan：由 ApplyVxlan 自管归属，交换机重放不碰。
		{prefix: "ip -d -j link show dev vxlan7",
			out: `[{"ifname":"vxlan7","master":"vs-lan","linkinfo":{"info_kind":"vxlan"}}]`},
	}}
	p := New(f)
	err := p.ApplyBridgeDomain(context.Background(), model.VirtualSwitch{
		Name: "vs-lan", Type: "l2",
		Ports: []model.VSwitchPort{{Seq: 1, Interface: "ens192"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.has("ip link set dev ens224 nomaster") {
		t.Fatalf("已从声明里删除的物理成员口应被释放；实际：\n%s", f.joined())
	}
	for _, keep := range []string{"vnet2", "vxlan7"} {
		if f.has("ip link set dev " + keep + " nomaster") {
			t.Fatalf("%s 不是本产品 enslave 的成员，不得被摘除；实际：\n%s", keep, f.joined())
		}
	}
}

// ---------- L3 交换机（内核 VRF） ----------

func TestApplyVRFCreatesVRFInterfacesAndRoutes(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	err := p.ApplyVRF(context.Background(), model.Vrf{
		Name: "vs-l3",
		L3Interfaces: []model.L3Interface{{
			Interface: "ens192", Vlan: 100, Addresses: []string{"10.0.0.1/24"},
		}},
		Routes: []model.Route{{Prefix: "0.0.0.0/0", NextHop: "10.0.0.254", Distance: 10}},
	})
	if err != nil {
		t.Fatal(err)
	}
	table := fmt.Sprint(VRFTableID("vs-l3"))
	for _, want := range []string{
		"ip link add name vs-l3 type vrf table " + table,
		"ip link add link ens192 name ens192.100 type vlan id 100",
		"ip link set dev ens192.100 master vs-l3",
		"ip addr replace 10.0.0.1/24 dev ens192.100",
		"ip route replace vrf vs-l3 0.0.0.0/0 via 10.0.0.254 metric 10",
	} {
		if !f.has(want) {
			t.Fatalf("缺少命令 %q；实际：\n%s", want, f.joined())
		}
	}
}

func TestDeleteL3InterfaceRemovesAddressesAndVlanDevice(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -d -j link show dev ens192.100",
		out:    `[{"ifname":"ens192.100","master":"vs-l3","linkinfo":{"info_kind":"vlan","info_slave_kind":"vrf"}}]`,
	}}}
	p := New(f)
	err := p.DeleteL3Interface(context.Background(), "vs-l3", model.L3Interface{
		Interface: "ens192", Vlan: 100, Addresses: []string{"10.0.0.1/24"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ip addr del 10.0.0.1/24 dev ens192.100",
		"ip link set dev ens192.100 nomaster",
		"ip link del ens192.100",
	} {
		if !f.has(want) {
			t.Fatalf("缺少命令 %q；实际：\n%s", want, f.joined())
		}
	}
}

// R2-14：口当前挂在 VRF 上时才摘归属（正常撤销路径）。
func TestDeleteL3InterfaceDetachesVRFMaster(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -d -j link show dev ens224",
		out:    `[{"ifname":"ens224","master":"vs-l3","linkinfo":{"info_kind":"veth","info_slave_kind":"vrf"}}]`,
	}}}
	p := New(f)
	if err := p.DeleteL3Interface(context.Background(), "vs-l3",
		model.L3Interface{Interface: "ens224"}); err != nil {
		t.Fatal(err)
	}
	if !f.has("ip link set dev ens224 nomaster") {
		t.Fatalf("口在 VRF 上时应摘除归属；实际：\n%s", f.joined())
	}
}

// R2-14：迁移形态——口已被交换机（bridge）接管时不摘（否则把刚挂上的归属摘掉）。
func TestDeleteL3InterfaceKeepsForeignMaster(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -d -j link show dev ens224",
		out:    `[{"ifname":"ens224","master":"vs-lan","linkinfo":{"info_kind":"veth","info_slave_kind":"bridge"}}]`,
	}}}
	p := New(f)
	if err := p.DeleteL3Interface(context.Background(), "vs-l3",
		model.L3Interface{Interface: "ens224"}); err != nil {
		t.Fatal(err)
	}
	if f.has("ip link set dev ens224 nomaster") {
		t.Fatalf("口已被 bridge 接管，删除 l3-interface 不得摘掉归属；实际：\n%s", f.joined())
	}
}

func TestDeleteVRFCleansMembersThenDeletesDevice(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "ip -j link show master vs-l3", out: `[{"ifname":"ens192.100"}]`},
		{prefix: "ip -j addr show dev ens192.100",
			out: `[{"addr_info":[{"local":"10.0.0.1","prefixlen":24}]}]`},
		{prefix: "ip -d -j link show dev ens192.100",
			out: `[{"ifname":"ens192.100","master":"vs-l3","linkinfo":{"info_kind":"vlan","info_slave_kind":"vrf"}}]`},
	}}
	p := New(f)
	if err := p.DeleteVRF(context.Background(), "vs-l3"); err != nil {
		t.Fatal(err)
	}
	if !f.has("ip addr del 10.0.0.1/24 dev ens192.100") {
		t.Fatalf("成员口地址应先清；实际：\n%s", f.joined())
	}
	if !f.has("ip link set dev ens192.100 nomaster") {
		t.Fatalf("成员口应先出 VRF；实际：\n%s", f.joined())
	}
	if !f.has("ip link del ens192.100") {
		t.Fatalf("vlan 子接口应被回收；实际：\n%s", f.joined())
	}
	if !f.has("ip link del vs-l3") {
		t.Fatalf("VRF 设备应被删除；实际：\n%s", f.joined())
	}
}

func TestDeleteRouteShape(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	if err := p.DeleteRoute(context.Background(), "vs-l3",
		model.Route{Prefix: "10.9.0.0/24", NextHop: "10.0.0.9"}); err != nil {
		t.Fatal(err)
	}
	if !f.has("ip route del vrf vs-l3 10.9.0.0/24 via 10.0.0.9") {
		t.Fatalf("撤销路由命令形状不符；实际：\n%s", f.joined())
	}
}

// ---------- NAT（nftables） ----------

func TestApplyNATBuildsTableChainsAndRules(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	err := p.ApplyNAT(context.Background(), model.NatConfig{
		SourcePools: []model.NatSourcePool{{Name: "pool1", AddressRange: "203.0.113.1 to 203.0.113.5"}},
		Rules: []model.NatRule{
			{Seq: 10, MatchSource: "192.168.99.0/24", VirtualSwitch: "vs-lan",
				Action: model.NatAction{SourcePool: "pool1"}},
			{Seq: 20, MatchSource: "192.168.98.0/24", VirtualSwitch: "vs-lan",
				Action: model.NatAction{Interface: "ens224"}},
		},
		Static: []model.NatStatic{{InsideIP: "192.168.99.10", OutsideIP: "203.0.113.9"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"nft add table inet nfvis-nat",
		"nft add chain inet nfvis-nat postrouting { type nat hook postrouting priority srcnat ; }",
		"nft add chain inet nfvis-nat prerouting { type nat hook prerouting priority dstnat ; }",
		"nft flush chain inet nfvis-nat postrouting",
		"nft add rule inet nfvis-nat postrouting ip saddr 192.168.99.0/24 snat ip to 203.0.113.1-203.0.113.5",
		"nft add rule inet nfvis-nat postrouting ip saddr 192.168.98.0/24 oifname ens224 masquerade",
		"nft add rule inet nfvis-nat prerouting ip daddr 203.0.113.9 dnat ip to 192.168.99.10",
	} {
		if !f.has(want) {
			t.Fatalf("缺少命令 %q；实际：\n%s", want, f.joined())
		}
	}
}

// R2-13①：bond 已在场但属性与声明不同时**必须重建**——内核 bonding 的 mode/lacp_rate/
// xmit_hash_policy 不能原地改，旧实现把 `File exists` 一吞了之，于是 `set bonds b0 …
// lacp mode active` 提交成功、内核仍是 balance-xor（无运行态读视图可发现）。
func TestApplyBondRebuildsOnAttributeChange(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -d -j link show dev bond0",
		out: `[{"ifname":"bond0","linkinfo":{"info_kind":"bond",
		        "info_data":{"mode":"balance-xor","xmit_hash_policy":"layer2","lacp_rate":"slow"}}}]`,
	}}}
	p := New(f)
	err := p.ApplyBond(context.Background(), model.Bond{
		Name: "bond0", Members: []string{"ens192"},
		Lacp: &model.Lacp{Mode: "active", Interval: "fast"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.has("ip link del bond0") {
		t.Fatalf("属性与声明不同应重建 bond；实际：\n%s", f.joined())
	}
	if !f.has("type bond mode 802.3ad") || !f.has("lacp_rate fast") {
		t.Fatalf("应按声明重建为 802.3ad + lacp_rate fast；实际：\n%s", f.joined())
	}
	if idx := strings.Index(f.joined(), "ip link del bond0"); idx > strings.Index(f.joined(), "type bond mode 802.3ad") {
		t.Fatalf("必须先删后建；实际：\n%s", f.joined())
	}
	if !f.has("ip link set dev ens192 master bond0") {
		t.Fatalf("重建后仍要 enslave 成员；实际：\n%s", f.joined())
	}
}

// R2-13①：属性一致时不得重建（幂等重放/恢复收敛会反复走这条路径）。
func TestApplyBondKeepsMatchingAttributes(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -d -j link show dev bond0",
		out: `[{"ifname":"bond0","linkinfo":{"info_kind":"bond",
		        "info_data":{"mode":"802.3ad","xmit_hash_policy":"layer3+4","lacp_rate":"fast"}}}]`,
	}}}
	p := New(f)
	err := p.ApplyBond(context.Background(), model.Bond{
		Name: "bond0", Members: []string{"ens192"},
		Lacp: &model.Lacp{Mode: "active", Interval: "fast"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.has("ip link del bond0") {
		t.Fatalf("属性一致时不得重建；实际：\n%s", f.joined())
	}
}

// 真机（3.0.5~dev4）缺陷：内核 bonding **不允许 enslave 处于 up 的成员**
// （`Error: Device can not be enslaved while up.`）——成员必须先是 down 的。
// 修复前 `set bonds b0 members 1 ens192`（口是 up 的）提交直接失败并补偿。
func TestApplyBondEnslavesMembersWhileDown(t *testing.T) {
	r := newBondKernelRunner("ens192", "ens224") // 现场：两个口都是 up 的
	p := New(r)
	bond := model.Bond{Name: "b0", Members: []string{"ens192", "ens224"}}
	if err := p.ApplyBond(context.Background(), bond); err != nil {
		t.Fatalf("成员先 down 再 enslave 应成功（修复前这里是内核实测报错）：%v", err)
	}
	for _, m := range []string{"ens192", "ens224"} {
		iDown := r.callIndex("ip link set dev " + m + " down")
		iMaster := r.callIndex("ip link set dev " + m + " master b0")
		if iDown < 0 || iMaster < 0 || iDown > iMaster {
			t.Fatalf("成员 %s 必须先 down 再 enslave；实际：\n%s", m, r.joined())
		}
		iUp := r.callIndex("ip link set dev " + m + " up")
		if iUp < r.callIndex("ip link set dev b0 up") {
			t.Fatalf("成员 %s 应在 bond 自身 up 之后再置 up（从属口也要 up 才有流量）；实际：\n%s", m, r.joined())
		}
	}
	// 幂等重放：已 enslave 的成员不再被反复 down/up（重放不得抖动 LAG），且不报错。
	before := len(r.calls)
	if err := p.ApplyBond(context.Background(), bond); err != nil {
		t.Fatalf("重复 apply 应幂等：%v", err)
	}
	second := strings.Join(r.calls[before:], "\n")
	for _, m := range []string{"ens192", "ens224"} {
		if strings.Contains(second, "dev "+m+" down") {
			t.Fatalf("重放不得把已在位的成员 down 掉（会抖动 LAG）：\n%s", second)
		}
	}
}

// bondKernelRunner 模拟内核对 bonding 成员的既定行为：**up 的成员不能被 enslave**
// （`Error: Device can not be enslaved while up.`）；维护 admin 状态与归属，命令逐条记录，
// 供断言 down → master → （bond up）→ 成员 up 的次序。
type bondKernelRunner struct {
	fakeRunner
	up     map[string]bool
	slaves map[string]string // 成员 → master
}

func newBondKernelRunner(upDevs ...string) *bondKernelRunner {
	r := &bondKernelRunner{up: map[string]bool{}, slaves: map[string]string{}}
	for _, d := range upDevs {
		r.up[d] = true
	}
	return r
}

func (r *bondKernelRunner) callIndex(sub string) int { return strings.Index(r.joined(), sub) }

func (r *bondKernelRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "ip" && len(args) >= 3 && args[0] == "link" && args[1] == "set" && args[2] == "dev" {
		dev := args[3]
		if len(args) == 5 { // ip link set dev X up|down
			switch args[4] {
			case "up":
				r.up[dev] = true
			case "down":
				r.up[dev] = false
			}
		}
		if len(args) == 6 && args[4] == "master" { // ip link set dev X master B
			r.calls = append(r.calls, name+" "+strings.Join(args, " "))
			if r.up[dev] {
				return "Error: Device can not be enslaved while up.", errors.New("exit status 2")
			}
			r.slaves[dev] = args[5]
			return "", nil
		}
	}
	// 任一 link show 形态（`-j`/`-d` 任意组合）：按模拟状态回读。
	if name == "ip" && len(args) >= 5 {
		for i := range args {
			if args[i] == "dev" && i+1 < len(args) {
				dev := args[i+1]
				r.calls = append(r.calls, name+" "+strings.Join(args, " "))
				flags, master, slaveKind := "[]", "", ""
				if r.up[dev] {
					flags = `["UP","LOWER_UP"]`
				}
				if m := r.slaves[dev]; m != "" {
					master, slaveKind = fmt.Sprintf(`,"master":%q`, m), `,"linkinfo":{"info_slave_kind":"bond"}`
				}
				return fmt.Sprintf(`[{"ifname":%q,"flags":%s,"operstate":"up"%s%s}]`,
					dev, flags, master, slaveKind), nil
			}
		}
	}
	return r.fakeRunner.Run(ctx, name, args...)
}

// R2-13①：成员按声明收敛——已从声明里删掉的成员必须放开（旧实现只 enslave、从不释放，
// 该口永久留在 LAG，而 bond 没有运行态读视图可发现）。
func TestApplyBondReleasesRemovedMembers(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "ip -j link show master bond0",
			out: `[{"ifname":"ens192","master":"bond0"},{"ifname":"ens224","master":"bond0"}]`},
		{prefix: "ip -d -j link show dev ens224",
			out: `[{"ifname":"ens224","master":"bond0","linkinfo":{"info_kind":"veth","info_slave_kind":"bond"}}]`},
	}}
	p := New(f)
	err := p.ApplyBond(context.Background(), model.Bond{Name: "bond0", Members: []string{"ens192"}})
	if err != nil {
		t.Fatal(err)
	}
	if !f.has("ip link set dev ens224 nomaster") {
		t.Fatalf("已从声明里删除的成员应被释放；实际：\n%s", f.joined())
	}
	if f.has("ip link set dev ens192 nomaster") {
		t.Fatalf("仍声明的成员不应被释放；实际：\n%s", f.joined())
	}
}

// R2-13②：恢复/备份恢复路径（prev=nil）下，本地 vxlan 设备元组与声明不同时必须重建——
// 旧实现把 `File exists` 一吞了之，隧道保持旧 VNI/remote，配置与数据面不一致且无提示。
func TestApplyVxlanRebuildsOnLocalTupleMismatch(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -d -j link show dev vx1",
		out: `[{"ifname":"vx1","linkinfo":{"info_kind":"vxlan",
		        "info_data":{"id":100,"local":"10.0.0.1","remote":"10.0.0.2","dstport":4789}}}]`,
	}}}
	p := New(f)
	err := p.ApplyVxlan(context.Background(), model.VxlanTunnel{
		Name: "vx1", Vni: 200, Local: "10.0.0.1", Remote: "10.0.0.2", VirtualSwitch: "vs-lan",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !f.has("ip link del vx1") {
		t.Fatalf("本地元组与声明不同应重建隧道；实际：\n%s", f.joined())
	}
	if !f.has("id 200") {
		t.Fatalf("应按新元组重建；实际：\n%s", f.joined())
	}
	if strings.Index(f.joined(), "ip link del vx1") > strings.Index(f.joined(), "id 200") {
		t.Fatalf("必须先删后建；实际：\n%s", f.joined())
	}
}

// R2-13②：元组一致时不重建（重放幂等）。
func TestApplyVxlanKeepsMatchingLocalDevice(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -d -j link show dev vx1",
		out: `[{"ifname":"vx1","linkinfo":{"info_kind":"vxlan",
		        "info_data":{"id":100,"local":"10.0.0.1","remote":"10.0.0.2","dstport":4789}}}]`,
	}}}
	p := New(f)
	err := p.ApplyVxlan(context.Background(), model.VxlanTunnel{
		Name: "vx1", Vni: 100, Local: "10.0.0.1", Remote: "10.0.0.2",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.has("ip link del vx1") {
		t.Fatalf("元组一致时不得重建；实际：\n%s", f.joined())
	}
}

// ---------- VXLAN ----------

func TestApplyVxlanCreatesDeviceAndJoinsBridge(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	err := p.ApplyVxlan(context.Background(), model.VxlanTunnel{
		Name: "vx1", Vni: 100, Local: "10.0.0.1", Remote: "10.0.0.2", VirtualSwitch: "vs-lan",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !f.has("ip link add vx1 type vxlan id 100 local 10.0.0.1 remote 10.0.0.2 dstport 4789") {
		t.Fatalf("vxlan 设备参数不符；实际：\n%s", f.joined())
	}
	if !f.has("ip link set dev vx1 master vs-lan") {
		t.Fatalf("隧道口应入交换机 bridge；实际：\n%s", f.joined())
	}
}

func TestApplyVxlanRebuildsOnTupleChange(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	prev := &model.VxlanTunnel{Name: "vx1", Vni: 100, Local: "10.0.0.1", Remote: "10.0.0.2"}
	err := p.ApplyVxlan(context.Background(), model.VxlanTunnel{
		Name: "vx1", Vni: 200, Local: "10.0.0.1", Remote: "10.0.0.2",
	}, prev)
	if err != nil {
		t.Fatal(err)
	}
	if !f.has("ip link del vx1") {
		t.Fatalf("元组变化应先删旧设备；实际：\n%s", f.joined())
	}
	if !f.has("id 200") {
		t.Fatalf("应按新元组重建；实际：\n%s", f.joined())
	}
}

// ---------- 未实现族如实报错 ----------

func TestUnsupportedFamiliesReportError(t *testing.T) {
	p := New(&fakeRunner{})
	ctx := context.Background()
	// ACL / QoS / 端口镜像 / 风暴抑制 / 端口安全已实现，不再在此列；DHCP 中继也已实现
	// （决策 #437：nfvisd 内的用户态中继实例）——它不再报 ErrUnsupported，声明不完整时按
	// 如实错误上报（见 TestApplyDhcpRelayWithoutV4GatewayIsHonest）。
	cases := map[string]error{
		"LLDP":     p.ApplyLLDP(ctx, &model.LldpConfig{}),
		"DHCP 服务器": p.ApplyDHCPServer(ctx, model.VirtualSwitch{Name: "vs", DhcpServerPoolStart: "10.0.0.10"}),
		"DNS 代理":   p.ApplyDNSProxy(ctx, orchestrator.DNSProxyUpstreams{Global: []string{"8.8.8.8"}}),
	}
	for name, err := range cases {
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%s 应报 ErrUnsupported，得到 %v", name, err)
		}
	}
}

// 未实现族在**未声明**时必须是空操作：提交编排把它们当 bridge-domain 的伴随操作调用
// （每台 L2 交换机都走一次），若一律报「不支持」，任何一次普通提交都会被挡住
// ——真机走查实测过：只建一台 L2 交换机，提交却报 `dhcp-relay[vs-lan] 不受支持`。
func TestUnimplementedFamiliesAreNoopWhenUndeclared(t *testing.T) {
	p := New(&fakeRunner{})
	ctx := context.Background()
	empty := model.VirtualSwitch{Name: "vs"}
	if err := p.ApplyDhcpRelay(ctx, empty); err != nil {
		t.Fatalf("未声明 DHCP 中继应空操作，得到 %v", err)
	}
	if err := p.ApplyDHCPServer(ctx, empty); err != nil {
		t.Fatalf("未声明 DHCP 服务器应空操作，得到 %v", err)
	}
	if err := p.ApplyDNSProxy(ctx, orchestrator.DNSProxyUpstreams{}); err != nil {
		t.Fatalf("全局与各域都空时 DNS 代理应空操作，得到 %v", err)
	}
	if err := p.ApplyLLDP(ctx, nil); err != nil {
		t.Fatalf("未声明 LLDP 应空操作，得到 %v", err)
	}
	// 每交换机一次 DNS 代理伴随调用（按域上游为空列表）同样空操作。
	if err := p.ApplyDNSProxy(ctx, orchestrator.DNSProxyUpstreams{
		PerSwitch: map[string][]string{"vs": nil}}); err != nil {
		t.Fatalf("按域上游为空列表时应空操作，得到 %v", err)
	}
}

func TestUnsupportedReadViewsAreHonest(t *testing.T) {
	p := New(&fakeRunner{})
	ctx := context.Background()
	if _, err := p.LldpNeighbors(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("LLDP 邻居读视图应如实报不可用，得到 %v", err)
	}
	if _, err := p.NATSessions(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("NAT 会话读视图应如实报不可用，得到 %v", err)
	}
	if _, err := p.VxlanStates(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("VXLAN 运行态读视图应如实报不可用，得到 %v", err)
	}
	if _, ok := p.DHCPServerLeases("vs-lan"); ok {
		t.Fatalf("DHCP 租约读视图应如实报不可用")
	}
	// 风暴抑制与端口安全已实现：读视图改为**查内核**（空内核 ⇒ 未挂载，而不是"不支持"）。
	if sd, ok := p.StormDataplane(ctx, "ens192"); ok {
		t.Fatalf("空内核下风暴抑制不应报在位：%+v", sd)
	}
	if pd, ok := p.PortSecDataplane(ctx, "ens192"); ok {
		t.Fatalf("空内核下端口安全不应报在位：%+v", pd)
	}
}

// ---------- 运行态读视图 ----------

func TestMACTableParsesKernelFdb(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "bridge -j fdb show br vs-lan",
		out: `[{"mac":"aa:bb:cc:dd:ee:ff","dev":"ens192","vlan":100},
		       {"mac":"11:22:33:44:55:66","dev":"vs-lan","vlan":1,"flags":["self"]}]`,
	}}}
	rt := NewRuntime(f)
	rows, err := rt.MACTable(context.Background(), "vs-lan")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].MAC != "aa:bb:cc:dd:ee:ff" || rows[0].Port != "ens192" || rows[0].VLAN != 100 {
		t.Fatalf("MAC 表解析不符：%+v", rows)
	}
}

// 决策 #435 顺带修复：iproute2 新版的 `bridge -j fdb` 用 `ifname`（成员口）与 `state`
// （permanent/self），**旧版才是 `dev`/`flags`**——只认旧键会让**整张 MAC 表恒为空**
// （真机实测：`bridge -j fdb` 出 21 行，按旧键解析后 0 行，`show … mac-table` 恒「表为空」）。
// 本用例钉住新形状（红-绿：把解析改回只认 `dev`/`flags` ⇒ 本用例得 0 条而失败）。
func TestMACTableParsesKernelFdbNewShape(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "bridge -j fdb show br vs-lan",
		out: `[{"mac":"00:50:56:c0:00:08","ifname":"ens192","flags":[],"master":"vs-lan","state":""},
		       {"mac":"00:0c:29:a9:61:8e","ifname":"ens192","vlan":1,"flags":[],"master":"vs-lan","state":"permanent"},
		       {"mac":"33:33:00:00:00:01","ifname":"ens192","flags":[],"master":"vs-lan","state":"self"}]`,
	}}}
	rt := NewRuntime(f)
	rows, err := rt.MACTable(context.Background(), "vs-lan")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("新形状应解析出 2 条（bridge 自身条目与 state=self 条目被过滤）：%+v", rows)
	}
	if rows[0].MAC != "00:50:56:c0:00:08" || rows[0].Port != "ens192" {
		t.Fatalf("新形状成员口应取 ifname=ens192：%+v", rows[0])
	}
}

func TestRoutesReadsVRFTable(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -j route show table ",
		out:    `[{"dst":"10.9.0.0/24","gateway":"10.0.0.9","metric":10}]`,
	}}}
	rt := NewRuntime(f)
	rows, err := rt.Routes(context.Background(), "vs-l3")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Prefix != "10.9.0.0/24" || rows[0].NextHop != "10.0.0.9" || rows[0].Distance != 10 {
		t.Fatalf("路由解析不符：%+v", rows)
	}
	if !f.has(fmt.Sprintf("ip -j route show table %d", VRFTableID("vs-l3"))) {
		t.Fatalf("应按派生表号查询；实际：\n%s", f.joined())
	}
}

func TestVPPIfnamesOnlyReturnsDeclaredProductDevices(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -j link show",
		out:    `[{"ifname":"vs-lan"},{"ifname":"vr-vs-lan"},{"ifname":"virbr0"},{"ifname":"ens192"}]`,
	}}}
	p := New(f)
	p.SetConfig(model.Config{VirtualSwitches: []model.VirtualSwitch{{
		Name: "vs-lan", Type: "l2", Gateway: &model.VSGateway{Addresses: []string{"192.168.99.1/24"}},
	}}})
	got, err := p.VPPIfnames()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, ",")
	if joined != "vs-lan,vr-vs-lan" {
		t.Fatalf("只应返回配置声明的产品设备（宿主机 virbr0 不算），得到 %q", joined)
	}
}

// ---------- 恢复收敛 ----------

func TestEnsureConsistentReplaysAndCollectsErrors(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		// 转发前置条件（内核数据面下先于一切下发）：IPv4/IPv6 转发开关已为 1。
		{prefix: "sysctl -n net.ipv4.ip_forward", out: "1\n"},
		{prefix: "sysctl -n net.ipv6.conf.all.forwarding", out: "1\n"},
		{prefix: "ip link set dev bad0 master vs-lan",
			out: "Cannot find device \"bad0\"", err: errors.New("exit status 1")},
	}}
	p := New(f)
	cfg := model.Config{
		Interfaces: []model.InterfaceConfig{{Name: "ens192", MTU: 1500}},
		VirtualSwitches: []model.VirtualSwitch{{
			Name: "vs-lan", Type: "l2",
			Ports: []model.VSwitchPort{{Seq: 1, Interface: "bad0"}},
		}},
	}
	errs := p.EnsureConsistent(context.Background(), cfg)
	if len(errs) == 0 {
		t.Fatalf("不可收敛项应逐条报出")
	}
	if !strings.Contains(errs[0].Error(), "virtual-switches/vs-lan") {
		t.Fatalf("错误应带对象路径，得到 %v", errs[0])
	}
	if !f.has("ip link set dev ens192 mtu 1500") {
		t.Fatalf("其余声明仍应重放；实际：\n%s", f.joined())
	}
	if got := p.config().VirtualSwitches; len(got) != 1 {
		t.Fatalf("收敛时应记录配置快照")
	}
}

// R2-6：内核数据面也要有告警落点——恢复未收敛项此前只进 journal，`show alarms`/Web 总览/
// 诊断包/`/events` 全查不到（与 #191/#321/#333「未收敛项必须事后可查」的纪律冲突）。
// 收敛成功的项由 Sync 按同一 source 自动消解。
func TestEnsureConsistentRaisesAndResolvesRecoveryAlarms(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "sysctl -n", out: "1\n"},
		{prefix: "ip link set dev bad0 master vs-lan",
			out: `Cannot find device "bad0"`, err: errors.New("exit status 1")},
	}}
	p := New(f)
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	bad := model.Config{VirtualSwitches: []model.VirtualSwitch{{
		Name: "vs-lan", Type: "l2",
		Ports: []model.VSwitchPort{{Seq: 1, Interface: "bad0"}},
	}}}
	if errs := p.EnsureConsistent(context.Background(), bad); len(errs) == 0 {
		t.Fatalf("不可收敛项应逐条报出")
	}
	active := store.List("active")
	if len(active) != 1 || active[0].Source != "virtual-switches/vs-lan" {
		t.Fatalf("未收敛项应进告警（带对象路径），得到 %+v", active)
	}
	// 「设备不存在」按 VPP 侧同码族映射为 RECOVERY_IFACE_MISSING（error）。
	if active[0].Code != network.AlarmIfaceMissing || active[0].Severity != network.SeverityError {
		t.Fatalf("设备不存在应记 %s/error，得到 %+v", network.AlarmIfaceMissing, active[0])
	}

	// 同一声明改好（口存在）后再次收敛：同一 source 的告警自动消解。
	f2 := &fakeRunner{replies: []fakeReply{{prefix: "sysctl -n", out: "1\n"}}}
	p2 := New(f2)
	p2.SetAlarms(store)
	good := model.Config{VirtualSwitches: []model.VirtualSwitch{{
		Name: "vs-lan", Type: "l2",
		Ports: []model.VSwitchPort{{Seq: 1, Interface: "ens192"}},
	}}}
	if errs := p2.EnsureConsistent(context.Background(), good); len(errs) != 0 {
		t.Fatalf("修好后不应再有未收敛项：%v", errs)
	}
	if got := store.List("active"); len(got) != 0 {
		t.Fatalf("收敛成功后告警应消解，得到 %+v", got)
	}
}

// R2-6：物理业务口链路告警（FR-NET-003）在内核数据面下必须成立（此前恒 nil）。
func TestCheckInterfaceLinksRaisesAndResolvesAlarms(t *testing.T) {
	down := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -d -j link show",
		out: `[{"ifname":"ens192","flags":["UP"],"operstate":"down","mtu":1500},
		       {"ifname":"ens224","flags":["UP","LOWER_UP"],"operstate":"up","mtu":1500}]`,
	}}}
	p := New(down)
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	cfg := model.Config{Interfaces: []model.InterfaceConfig{{Name: "ens192"}, {Name: "ens224"}}}
	errs := p.CheckInterfaceLinks(context.Background(), cfg)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "ens192") {
		t.Fatalf("链路 down 的口应逐条报出，得到 %v", errs)
	}
	active := store.List("active")
	if len(active) != 1 || active[0].Code != network.AlarmIfaceLinkDown || active[0].Source != "ens192" {
		t.Fatalf("应记 %s 告警，得到 %+v", network.AlarmIfaceLinkDown, active)
	}

	// 链路恢复：同 source 消解；从配置里删掉的口其滞留告警同样清掉。
	up := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -d -j link show",
		out: `[{"ifname":"ens192","flags":["UP","LOWER_UP"],"operstate":"up","mtu":1500},
		       {"ifname":"ens224","flags":["UP","LOWER_UP"],"operstate":"up","mtu":1500}]`,
	}}}
	p2 := New(up)
	p2.SetAlarms(store)
	if errs := p2.CheckInterfaceLinks(context.Background(), model.Config{
		Interfaces: []model.InterfaceConfig{{Name: "ens192"}},
	}); len(errs) != 0 {
		t.Fatalf("链路恢复后不应再报：%v", errs)
	}
	if got := store.List("active"); len(got) != 0 {
		t.Fatalf("链路恢复后告警应消解，得到 %+v", got)
	}
}

// R2-6：显式禁用的口不发链路告警（用户意图，与 VPP 侧同口径）。
func TestCheckInterfaceLinksSkipsExplicitlyDisabled(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -d -j link show",
		out:    `[{"ifname":"ens192","flags":[],"operstate":"down","mtu":1500}]`,
	}}}
	p := New(f)
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	disabled := false
	errs := p.CheckInterfaceLinks(context.Background(), model.Config{
		Interfaces: []model.InterfaceConfig{{Name: "ens192", Enabled: &disabled}},
	})
	if len(errs) != 0 || len(store.List("active")) != 0 {
		t.Fatalf("显式禁用的口不应报链路告警：errs=%v alarms=%+v", errs, store.List("active"))
	}
}

// R2-5：恢复重放的段序必须是**依赖序**——「交换机 + 端口安全」要在一次收敛内成功。
// 旧段序 interfaces 在 virtual-switches 之前，而端口安全要求该口已是 bridge 成员
// （portsec 会如实拒绝：`需先挂到交换机上`），于是主机重启后白名单必然不下发（安全特性
// 静默失效），要「再提交一次」才好转——非确定性症状。bond 源镜像同族（镜像源要在 bonds 之后）。
func TestEnsureConsistentConvergesSwitchPortSecurityInOnePass(t *testing.T) {
	r := newOrderingRunner()
	p := New(r)
	cfg := model.Config{
		Interfaces: []model.InterfaceConfig{{
			Name: "ens192", PortSecurity: []model.PortSecMAC{"aa:bb:cc:dd:ee:ff"},
		}},
		VirtualSwitches: []model.VirtualSwitch{{
			Name: "vs-lan", Type: "l2",
			Ports: []model.VSwitchPort{{Seq: 1, Interface: "ens192"}},
		}},
	}
	if errs := p.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("交换机 + 端口安全应一次收敛成功；实际未收敛项：%v", errs)
	}
	for _, want := range []string{
		"ip link set dev ens192 master vs-lan",
		"bridge link set dev ens192 learning off",
	} {
		if !r.has(want) {
			t.Fatalf("缺少命令 %q；实际：\n%s", want, r.joined())
		}
	}
	// 段序真的是依赖序（而不是碰巧成功）：成员归属必须在端口安全之前下发。
	joined := r.joined()
	if strings.Index(joined, "ip link set dev ens192 master vs-lan") >
		strings.Index(joined, "bridge link set dev ens192 learning off") {
		t.Fatalf("bridge 成员归属必须先于端口安全；实际：\n%s", joined)
	}
}

// orderingRunner 维护「口挂在哪个 master 下」的模拟内核状态：只有
// `ip link set dev <口> master <master>` 执行过，后续 `ip -j -d link show dev <口>`
// 才报 `info_slave_kind=bridge`（portsec 的前置判据据此判定）——段序错了就必然失败。
// 其余命令转给 fakeRunner（sysctl 一律视为已就绪）。
type orderingRunner struct {
	fakeRunner
	bridged map[string]string // dev → master
}

func newOrderingRunner() *orderingRunner {
	return &orderingRunner{bridged: map[string]string{}}
}

func (r *orderingRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	switch {
	case name == "sysctl" && len(args) == 2 && args[0] == "-n":
		r.calls = append(r.calls, name+" "+strings.Join(args, " "))
		return "1\n", nil
	case name == "ip" && len(args) == 6 && args[0] == "link" && args[1] == "set" &&
		args[2] == "dev" && args[4] == "master":
		r.calls = append(r.calls, name+" "+strings.Join(args, " "))
		r.bridged[args[3]] = args[5]
		return "", nil
	case name == "ip" && len(args) >= 5 && args[3] == "show" && args[4] == "dev":
		dev := args[5]
		r.calls = append(r.calls, name+" "+strings.Join(args, " "))
		if master, ok := r.bridged[dev]; ok {
			return fmt.Sprintf(`[{"ifname":%q,"master":%q,"flags":["UP","LOWER_UP"],"operstate":"up",`+
				`"linkinfo":{"info_slave_kind":"bridge"}}]`, dev, master), nil
		}
		return fmt.Sprintf(`[{"ifname":%q,"flags":["UP","LOWER_UP"],"operstate":"up"}]`, dev), nil
	}
	return r.fakeRunner.Run(ctx, name, args...)
}

// ---------- 诊断 ----------

func TestDiagPingAndVRFScope(t *testing.T) {
	f := &fakeRunner{}
	d := NewDiag(f)
	if _, err := d.Ping(context.Background(), "10.0.0.1", "", "", 3, false); err != nil {
		t.Fatal(err)
	}
	if !f.has("ping -c 3 10.0.0.1") {
		t.Fatalf("默认表 ping 命令不符；实际：\n%s", f.joined())
	}
	f.calls = nil
	if _, err := d.Ping(context.Background(), "10.0.0.1", "", "vs-l3", 0, true); err != nil {
		t.Fatal(err)
	}
	if !f.has("ip vrf exec vs-l3 ping -c 5 -6 10.0.0.1") {
		t.Fatalf("VRF 作用域应经 ip vrf exec，count 缺省为 5；实际：\n%s", f.joined())
	}
	if err := d.ClearInterfaceStats(context.Background(), "ens192"); !errors.Is(err, ErrStatsClearUnsupported) {
		t.Fatalf("清零统计应如实报不支持，得到 %v", err)
	}
}

// 同一次提交内的绑定：ACL 与 QoS 策略先由 ApplyACL/ApplyQos 下发，随后 ApplyVRF/
// ApplyInterface 才引用它们——此时 `p.config()` 仍是上一次收敛的快照，**不能**用它解析。
// 真机走查实测过：建 ACL 并在同一次提交里绑到三层接口，报「绑定了未下发的 ACL」。
func TestBindingResolvesFromSameCommitNotConfigSnapshot(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	ctx := context.Background()
	// 配置快照刻意留空（模拟"本次提交新建的对象"）。
	p.SetConfig(model.Config{})

	acl := model.Acl{Name: "acl-new", Rules: []model.AclRule{
		{Seq: 10, Source: "any", Destination: "any", Action: "permit"},
	}}
	if err := p.ApplyACL(ctx, acl); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	if err := p.ApplyVRF(ctx, model.Vrf{
		Name:         "vs-l3",
		L3Interfaces: []model.L3Interface{{Interface: "ens224", AclIn: "acl-new"}},
	}); err != nil {
		t.Fatalf("同一次提交里绑定刚下发的 ACL 应成功，得到 %v", err)
	}
	if !f.has("jump") {
		t.Fatalf("应下发到 ACL 链的跳转；实际：\n%s", f.joined())
	}

	// QoS 同族：策略先 ApplyQos，接口再引用。
	// 接口收尾的端口安全 Teardown 会读桥成员状态（R2-22 起读失败如实上抛），给一条正常事实。
	f2 := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -j -d link show dev ens192",
		out:    `[{"ifname":"ens192","linkinfo":{}}]`,
	}}}
	p2 := New(f2)
	p2.SetConfig(model.Config{})
	if err := p2.ApplyQos(ctx, model.QosPolicy{Name: "pol-new", Cir: 8000, Cbs: 1000}); err != nil {
		t.Fatalf("ApplyQos: %v", err)
	}
	if err := p2.ApplyInterface(ctx, model.InterfaceConfig{Name: "ens192", IngressPolicy: "pol-new"}); err != nil {
		t.Fatalf("同一次提交里绑定刚下发的 QoS 策略应成功，得到 %v", err)
	}
	if !f2.has("police") {
		t.Fatalf("应下发限速 policer；实际：\n%s", f2.joined())
	}

	// 真正未下发的名字仍要如实报错（不能因为"登记表里可能有"就放过）。
	if err := p.ApplyVRF(ctx, model.Vrf{
		Name:         "vs-l3b",
		L3Interfaces: []model.L3Interface{{Interface: "ens240", AclIn: "acl-nope"}},
	}); err == nil {
		t.Fatalf("绑定未下发的 ACL 应报错")
	}
}

// 删 L2 交换机必须一并回收它自己创建的网关 VRF，否则内核里长期留一张空 VRF
// （真机走查实测的残留：删了 vs-lan，`vr-vs-lan` 还在）。
func TestDeleteBridgeDomainReclaimsGatewayVRF(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	if err := p.DeleteBridgeDomain(context.Background(), "vs-lan"); err != nil {
		t.Fatal(err)
	}
	if !f.has("ip link del vs-lan") || !f.has("ip link del vr-vs-lan") {
		t.Fatalf("应同时删除 bridge 与其派生的网关 VRF；实际：\n%s", f.joined())
	}
}

// 内核数据面的转发前置条件（真机走查抓到：产品此前两条都不管，转发会**静默**全丢）。
func TestEnsureForwardingSetsSysctlAndAcceptsOnlyDataplaneDevices(t *testing.T) {
	// 转发开关的读值要**先 0 后 1**（写前关着、写后回读为 1），故用一个带状态的小 Runner。
	r := newSysctlStateRunner()
	p := New(r)
	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{
			{Name: "vs-lan", Type: "l2", Gateway: &model.VSGateway{Addresses: []string{"192.168.99.1/24"}}},
			{Name: "vs-l3", Type: "l3"},
		},
		Vrfs: []model.Vrf{{Name: "vs-l3", L3Interfaces: []model.L3Interface{{Interface: "ens224"}}}},
	}
	if err := p.EnsureForwarding(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	// R2-10：内核数据面下产品是路由器，v4/v6 都要打开（Ubuntu 缺省 v6 转发为 0，
	// v6 三层/路由/ACL 都「下发成功」却一个包不转发）。
	for _, key := range []string{"net.ipv4.ip_forward", "net.ipv6.conf.all.forwarding"} {
		if !r.has("sysctl -w " + key + "=1") {
			t.Fatalf("转发开关 %s 为 0 时应写入 1；实际：\n%s", key, r.joined())
		}
	}
	joined := r.joined()
	for _, want := range []string{
		"nft add table inet nfvis-forward",
		"hook forward priority -10",
		"iifname { ens224, vr-vs-lan, vs-l3, vs-lan }",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺少 %q；实际：\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "ens160") {
		t.Fatalf("放行链不得包含管理口；实际：\n%s", joined)
	}
}

// sysctlStateRunner 维护一张 sysctl 取值表：`-n <key>` 读当前值、`-w <key>=<v>` 写入。
// 用于验证「读 → 写 → 回读确认」这条路径（初值全为 0）。
type sysctlStateRunner struct {
	fakeRunner
	values map[string]string
}

func newSysctlStateRunner() *sysctlStateRunner {
	return &sysctlStateRunner{values: map[string]string{
		"net.ipv4.ip_forward":          "0",
		"net.ipv6.conf.all.forwarding": "0",
	}}
}

func (r *sysctlStateRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "sysctl" {
		r.calls = append(r.calls, name+" "+strings.Join(args, " "))
		switch {
		case len(args) == 2 && args[0] == "-n":
			if v, ok := r.values[args[1]]; ok {
				return v + "\n", nil
			}
			return "", errors.New("exit status 255")
		case len(args) == 2 && args[0] == "-w":
			kv := strings.SplitN(args[1], "=", 2)
			if len(kv) == 2 {
				r.values[kv[0]] = kv[1]
				return kv[0] + " = " + kv[1] + "\n", nil
			}
		}
	}
	return r.fakeRunner.Run(ctx, name, args...)
}

// R2-10：v6 转发开关写不动时如实报错（与 v4 同口径，点名开关）。
func TestEnsureForwardingReportsUnwritableIPv6Sysctl(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "sysctl -n net.ipv4.ip_forward", out: "1\n"},
		{prefix: "sysctl -n net.ipv6.conf.all.forwarding", out: "0\n"},
		{prefix: "sysctl -w net.ipv6.conf.all.forwarding=1",
			out: "sysctl: permission denied", err: errors.New("exit status 255")},
	}}
	p := New(f)
	err := p.EnsureForwarding(context.Background(), model.Config{})
	if err == nil || !strings.Contains(err.Error(), "net.ipv6.conf.all.forwarding") {
		t.Fatalf("写入失败应如实上报并点名 v6 开关，得到 %v", err)
	}
}

// 真机收尾回归：数据面设备被清空后（配置里已无交换机/接口/VRF）必须**回收**本产品的表——
// 旧行为（含 R2-16 的幂等重建）在无设备时保留空表/空链，`nft list tables` 长期留一张
// `inet nfvis-forward`，属 #410 F3 判定的同一类「留着空表」（同口径：`delete nat` 后整表回收）。
func TestEnsureForwardingReclaimsTableWhenNoDevices(t *testing.T) {
	r := newSysctlStateRunner()
	r.values["net.ipv4.ip_forward"] = "1"
	r.values["net.ipv6.conf.all.forwarding"] = "1"
	p := New(r)
	cfg := model.Config{}    // 已无任何数据面设备
	for i := 0; i < 2; i++ { // 幂等：重复收敛同样只删不建
		if err := p.EnsureForwarding(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	joined := r.joined()
	if !strings.Contains(joined, "nft delete table inet nfvis-forward") {
		t.Fatalf("无数据面设备时应回收本产品的表；实际：\n%s", joined)
	}
	for _, forbidden := range []string{
		"nft add table inet nfvis-forward",
		"nft add chain inet nfvis-forward",
		"nft flush chain inet nfvis-forward",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("无数据面设备时不得再建/改该表（%q）；实际：\n%s", forbidden, joined)
		}
	}
	// 15s 巡检同样走这条回收路径（ReconcileResidue → EnsureForwarding），无设备时能自愈。
	r2 := newSysctlStateRunner()
	r2.values["net.ipv4.ip_forward"] = "1"
	r2.values["net.ipv6.conf.all.forwarding"] = "1"
	p2 := New(r2)
	if errs := p2.ReconcileResidue(context.Background(), model.Config{}); len(errs) != 0 {
		t.Fatalf("巡检回收空表不应报错：%v", errs)
	}
	if !r2.has("nft delete table inet nfvis-forward") {
		t.Fatalf("巡检（15s 路径）也应回收空表；实际：\n%s", r2.joined())
	}
}

// R2-16：15s 巡检每次调用不得「删表→重建」（旧实现每轮 `nft delete table` 再建，
// 有一段表不存在的窗口，且 15s 一次地扰动）。改为表/链在位 + flush + 按当前设备集重写。
func TestEnsureForwardingRebuildsWithoutDeletingTable(t *testing.T) {
	r := newSysctlStateRunner()
	r.values["net.ipv4.ip_forward"] = "1"
	r.values["net.ipv6.conf.all.forwarding"] = "1"
	p := New(r)
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{{Name: "vs-lan", Type: "l2"}}}
	for i := 0; i < 2; i++ {
		if err := p.EnsureForwarding(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	joined := r.joined()
	if strings.Contains(joined, "nft delete table") {
		t.Fatalf("重建不得删表（窗口 + 无谓扰动）；实际：\n%s", joined)
	}
	if !strings.Contains(joined, "nft add table inet nfvis-forward") {
		t.Fatalf("应确保表在位；实际：\n%s", joined)
	}
	if strings.Count(joined, "nft flush chain inet nfvis-forward accept-dp") != 2 {
		t.Fatalf("每轮应按当前设备集重写规则（flush 后重加）；实际：\n%s", joined)
	}
}

// R2-2：宿主 FORWARD 链策略为 DROP 时，自建链的 accept **不能**豁免它（同 hook 的 base chain
// 相互独立，本仓库 internal/system/firewall.go 与 v2-round169 真机 A/B 已证）——检测到就落
// warning 告警并给照做命令；策略不再是 DROP 时同 source 消解。
func TestEnsureForwardingAlarmsWhenHostForwardPolicyDrops(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "sysctl -n", out: "1\n"},
		{prefix: "iptables -S FORWARD", out: "-P FORWARD DROP\n-A FORWARD -j DOCKER-USER\n"},
	}}
	p := New(f)
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{{Name: "vs-lan", Type: "l2"}},
		Vrfs:            []model.Vrf{{Name: "vs-l3"}},
	}
	if err := p.EnsureForwarding(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	active := store.List("active")
	if len(active) != 1 || active[0].Code != AlarmForwardPolicyDrop ||
		active[0].Severity != network.SeverityWarning {
		t.Fatalf("宿主 FORWARD 策略 DROP 应落 warning 告警 %s，得到 %+v", AlarmForwardPolicyDrop, active)
	}
	msg := active[0].Message
	for _, want := range []string{"iptables -I FORWARD 1 -i vs-lan -o vs-l3 -j ACCEPT", "nft insert rule"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("告警文案应给照做命令（含 %q）：%s", want, msg)
		}
	}

	// 策略恢复 ACCEPT：同一 source 消解。
	f2 := &fakeRunner{replies: []fakeReply{
		{prefix: "sysctl -n", out: "1\n"},
		{prefix: "iptables -S FORWARD", out: "-P FORWARD ACCEPT\n"},
	}}
	p2 := New(f2)
	p2.SetAlarms(store)
	if err := p2.EnsureForwarding(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := store.List("active"); len(got) != 0 {
		t.Fatalf("策略不再是 DROP 应消解告警，得到 %+v", got)
	}
}

// R2-2：无 iptables 时回落到 `nft -j list chain ip filter FORWARD` 的 policy；
// 两条路径都读不到时**保持现状**（不猜、不误消）。
func TestEnsureForwardingForwardPolicyReadFallbacks(t *testing.T) {
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{{Name: "vs-lan", Type: "l2"}}}
	nftDrop := &fakeRunner{replies: []fakeReply{
		{prefix: "sysctl -n", out: "1\n"},
		{prefix: "iptables -S FORWARD", out: "iptables: command not found", err: errors.New("exit status 127")},
		{prefix: "nft -j list chain ip filter FORWARD",
			out: `{"nftables":[{"metainfo":{}},{"chain":{"family":"ip","table":"filter","name":"FORWARD","policy":"drop"}}]}`},
	}}
	p := New(nftDrop)
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	if err := p.EnsureForwarding(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if active := store.List("active"); len(active) != 1 || active[0].Code != AlarmForwardPolicyDrop {
		t.Fatalf("iptables 缺失时应按 nft 的 policy 判定并告警，得到 %+v", active)
	}

	// 两条路径都读不到：不动（既有告警保持——不猜、也不误消）。
	unknown := &fakeRunner{replies: []fakeReply{
		{prefix: "sysctl -n", out: "1\n"},
		{prefix: "iptables -S FORWARD", err: errors.New("exit status 127")},
		{prefix: "nft -j list chain ip filter FORWARD", err: errors.New("exit status 1")},
	}}
	p2 := New(unknown)
	p2.SetAlarms(store)
	if err := p2.EnsureForwarding(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if active := store.List("active"); len(active) != 1 {
		t.Fatalf("读不到策略时应保持既有告警不变，得到 %+v", active)
	}
}

// 转发开关写不动时如实报错（不静默放过——内核数据面下它意味着一个包都转不出去）。
func TestEnsureForwardingReportsUnwritableSysctl(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "sysctl -n net.ipv4.ip_forward", out: "0\n"},
		{prefix: "sysctl -w net.ipv4.ip_forward=1",
			out: "sysctl: permission denied", err: errors.New("exit status 255")},
	}}
	p := New(f)
	err := p.EnsureForwarding(context.Background(), model.Config{})
	if err == nil || !strings.Contains(err.Error(), "ip_forward") {
		t.Fatalf("写入失败应如实上报并点名开关，得到 %v", err)
	}
}
