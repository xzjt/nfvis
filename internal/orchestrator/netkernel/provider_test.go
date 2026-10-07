package netkernel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
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
	f := &fakeRunner{}
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
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "bridge -j link show master vs-lan",
		out:    `[{"ifname":"ens192","master":"vs-lan"},{"ifname":"ens224","master":"vs-lan"}]`,
	}}}
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
	f := &fakeRunner{}
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

func TestDeleteVRFCleansMembersThenDeletesDevice(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "ip -j link show master vs-l3", out: `[{"ifname":"ens192.100"}]`},
		{prefix: "ip -j addr show dev ens192.100",
			out: `[{"addr_info":[{"local":"10.0.0.1","prefixlen":24}]}]`},
	}}
	p := New(f)
	if err := p.DeleteVRF(context.Background(), "vs-l3"); err != nil {
		t.Fatal(err)
	}
	if !f.has("ip addr del 10.0.0.1/24 dev ens192.100") {
		t.Fatalf("成员口地址应先清；实际：\n%s", f.joined())
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
	// ACL / QoS / 端口镜像 / 风暴抑制 / 端口安全已实现，不再在此列。
	cases := map[string]error{
		"LLDP":     p.ApplyLLDP(ctx, &model.LldpConfig{}),
		"中继":       p.ApplyDhcpRelay(ctx, model.VirtualSwitch{Name: "vs"}),
		"DHCP 服务器": p.ApplyDHCPServer(ctx, model.VirtualSwitch{Name: "vs"}),
		"DNS 代理":   p.ApplyDNSProxy(ctx, orchestrator.DNSProxyUpstreams{Global: []string{"8.8.8.8"}}),
	}
	for name, err := range cases {
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%s 应报 ErrUnsupported，得到 %v", name, err)
		}
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
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip link set dev bad0 master vs-lan",
		out:    "Cannot find device \"bad0\"",
		err:    errors.New("exit status 1"),
	}}}
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
