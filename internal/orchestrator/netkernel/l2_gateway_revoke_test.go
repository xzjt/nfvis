package netkernel

// round49（外部走查）：内核数据面下删除 L2 网关（保留交换机）同样不下发拆除。
//
// 缺陷形态（与 VPP 侧同族）：ApplyBridgeDomain 只在「有网关声明且地址非空」时调 applyGateway，
// 该函数只做 `ip addr replace` + `ip link set master`——声明消失时**什么都不做**，
// 地址收缩时旧地址也不会消失。于是 `delete virtual-switches vs-lan gateway ip <cidr>`
// 提交成功、读视图干净，而内核 bridge 上旧网关地址仍在、它仍挂在 `vr-vs-lan` 上
// （域内主机继续把已删的网关当可用）。
//
// 本文件钉住：撤销（清地址 → 退归属 → 回收派生网关 VRF 设备）与收缩（按内核实况删掉
// 声明外的地址），幂等、不碰显式 `gateway vrf <名>` 的对象。

import (
	"context"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// TestApplyBridgeDomainRevokesRemovedGateway 网关声明消失（交换机保留）时必须拆除：
// 按内核实况清掉 bridge 上的网关地址（v4/v6）、退出网关 VRF、回收派生的 `vr-<交换机>` 设备。
func TestApplyBridgeDomainRevokesRemovedGateway(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "ip -j addr show dev vs-lan", out: `[{"addr_info":[{"local":"192.168.99.1","prefixlen":24},` +
			`{"local":"2001:db8::1","prefixlen":64}]}]`},
		{prefix: "ip -d -j link show dev vs-lan", out: `[{"ifname":"vs-lan","master":"vr-vs-lan",` +
			`"linkinfo":{"info_kind":"bridge","info_slave_kind":"vrf"}}]`},
		{prefix: "ip -d -j link show dev vr-vs-lan", out: `[{"ifname":"vr-vs-lan","linkinfo":{"info_kind":"vrf"}}]`},
	}}
	p := New(f)
	if err := p.ApplyBridgeDomain(context.Background(), model.VirtualSwitch{Name: "vs-lan", Type: "l2"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ip addr del 192.168.99.1/24 dev vs-lan",
		"ip addr del 2001:db8::1/64 dev vs-lan",
		"ip link set dev vs-lan nomaster",
		"ip route del vrf vr-vs-lan unreachable default metric " + vrfFallbackMetric,
		"ip -6 route del vrf vr-vs-lan unreachable default metric " + vrfFallbackMetric,
		"ip link del vr-vs-lan",
	} {
		if !f.has(want) {
			t.Fatalf("撤销网关应下发 %q；实际：\n%s", want, f.joined())
		}
	}
	if f.has("ip addr replace 192.168.99.1/24 dev vs-lan") {
		t.Fatalf("撤销网关不得再下发声明里的地址；实际：\n%s", f.joined())
	}
}

// 收缩：声明里只剩一个地址时，内核实况里多出来的那个必须删掉，其余（含归属与派生 VRF）保持。
func TestApplyBridgeDomainShrinksGatewayAddresses(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "ip -j addr show dev vs-lan", out: `[{"addr_info":[{"local":"192.168.99.1","prefixlen":24},` +
			`{"local":"192.168.99.2","prefixlen":24}]}]`},
	}}
	p := New(f)
	vs := model.VirtualSwitch{Name: "vs-lan", Type: "l2",
		Gateway: &model.VSGateway{Addresses: []string{"192.168.99.1/24"}}}
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatal(err)
	}
	if !f.has("ip addr replace 192.168.99.1/24 dev vs-lan") {
		t.Fatalf("声明的地址照常下发；实际：\n%s", f.joined())
	}
	if !f.has("ip addr del 192.168.99.2/24 dev vs-lan") {
		t.Fatalf("已不声明的旧网关地址应删除；实际：\n%s", f.joined())
	}
	if f.has("ip addr del 192.168.99.1/24 dev vs-lan") {
		t.Fatalf("仍声明的地址不得删除；实际：\n%s", f.joined())
	}
	if f.has("ip link set dev vs-lan nomaster") || f.has("ip link del vr-vs-lan") {
		t.Fatalf("网关仍在声明里时不退归属、不回收网关 VRF；实际：\n%s", f.joined())
	}
}

// 幂等/空现场：从来没有网关的交换机上走撤销路径不得删除任何地址、不得报错
// （读取到的地址集合为空 ⇒ 空循环；`ip link del vr-<名>` 是既有的容忍式回收）。
func TestApplyBridgeDomainRevokeGatewayIdempotentOnEmpty(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	ctx := context.Background()
	vs := model.VirtualSwitch{Name: "vs-lan", Type: "l2"}
	for i := 0; i < 2; i++ {
		if err := p.ApplyBridgeDomain(ctx, vs); err != nil {
			t.Fatalf("第 %d 次撤销应幂等: %v", i+1, err)
		}
	}
	if f.has("ip addr del") {
		t.Fatalf("没有网关地址的现场不得下发删地址；实际：\n%s", f.joined())
	}
	if f.has("ip link set dev vs-lan nomaster") {
		t.Fatalf("读不到 VRF 归属时不得摘 master；实际：\n%s", f.joined())
	}
	if strings.Contains(f.joined(), "unreachable") {
		t.Fatalf("派生网关 VRF 不在场时不得下发兜底回收；实际：\n%s", f.joined())
	}
}

// 显式 `gateway vrf <名>`：撤销网关只清地址与归属——**不**回收操作者声明在 vrf 段里的
// VRF 设备（它的生命周期不由这台交换机决定；与 DeleteBridgeDomain 同口径）。
func TestApplyBridgeDomainRevokeKeepsExplicitVRF(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "ip -j addr show dev vs-lan", out: `[{"addr_info":[{"local":"10.10.10.1","prefixlen":24}]}]`},
		{prefix: "ip -d -j link show dev vs-lan", out: `[{"ifname":"vs-lan","master":"my-vrf",` +
			`"linkinfo":{"info_kind":"bridge","info_slave_kind":"vrf"}}]`},
		{prefix: "ip -d -j link show dev my-vrf", out: `[{"ifname":"my-vrf","linkinfo":{"info_kind":"vrf"}}]`},
	}}
	p := New(f)
	if err := p.ApplyBridgeDomain(context.Background(), model.VirtualSwitch{Name: "vs-lan", Type: "l2"}); err != nil {
		t.Fatal(err)
	}
	if !f.has("ip addr del 10.10.10.1/24 dev vs-lan") || !f.has("ip link set dev vs-lan nomaster") {
		t.Fatalf("显式 VRF 场景仍须清地址、退归属；实际：\n%s", f.joined())
	}
	if f.has("ip link del my-vrf") || f.has("ip route del vrf my-vrf") {
		t.Fatalf("显式/共享 VRF 设备不得被网关撤销回收；实际：\n%s", f.joined())
	}
}

// 地址写法归一：配置写 `2001:0db8::1/64` 而内核实况回读 `2001:db8::1/64`（同一个地址）时，
// 不得把自己刚替换上去的地址当成「声明外」删掉。
func TestApplyBridgeDomainGatewayAddressWrittenDifferentlyNotDeleted(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "ip -j addr show dev vs-lan", out: `[{"addr_info":[{"local":"2001:db8::1","prefixlen":64}]}]`},
	}}
	p := New(f)
	vs := model.VirtualSwitch{Name: "vs-lan", Type: "l2",
		Gateway: &model.VSGateway{Addresses: []string{"2001:0db8::1/64"}}}
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatal(err)
	}
	if f.has("ip addr del 2001:db8::1/64 dev vs-lan") {
		t.Fatalf("同一地址的不同写法不得被判为「已不声明」；实际：\n%s", f.joined())
	}
}

// 真实 bridge 上总有内核自动生成的 IPv6 链路本地地址（fe80::/64，scope=link）——它不属于
// 网关声明，撤销/收缩**不得**动它：删掉它不会自动重生（要 down/up 设备或重开 IPv6），
// 会静默打瘸该 bridge 的 v6 邻居发现与链路本地连通。
func TestApplyBridgeDomainRevokeKeepsLinkLocalAddress(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "ip -j addr show dev vs-lan", out: `[{"addr_info":[` +
			`{"local":"192.168.99.1","prefixlen":24,"scope":"global"},` +
			`{"local":"fe80::c4a:11ff:fe2d:1a3f","prefixlen":64,"scope":"link"}]}]`},
		{prefix: "ip -d -j link show dev vs-lan", out: `[{"ifname":"vs-lan","master":"vr-vs-lan",` +
			`"linkinfo":{"info_kind":"bridge","info_slave_kind":"vrf"}}]`},
		{prefix: "ip -d -j link show dev vr-vs-lan", out: `[{"ifname":"vr-vs-lan","linkinfo":{"info_kind":"vrf"}}]`},
	}}
	p := New(f)
	if err := p.ApplyBridgeDomain(context.Background(), model.VirtualSwitch{Name: "vs-lan", Type: "l2"}); err != nil {
		t.Fatal(err)
	}
	if f.has("ip addr del fe80::c4a:11ff:fe2d:1a3f/64") {
		t.Fatalf("内核自动生成的链路本地地址不得删除；实际：\n%s", f.joined())
	}
	if !f.has("ip addr del 192.168.99.1/24 dev vs-lan") {
		t.Fatalf("声明的网关地址（global）仍须清理；实际：\n%s", f.joined())
	}
}

// 收缩同理：链路本地地址不参与「声明集合为准」的比较。
func TestApplyBridgeDomainShrinkKeepsLinkLocalAddress(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "ip -j addr show dev vs-lan", out: `[{"addr_info":[` +
			`{"local":"192.168.99.1","prefixlen":24,"scope":"global"},` +
			`{"local":"192.168.99.2","prefixlen":24,"scope":"global"},` +
			`{"local":"fe80::c4a:11ff:fe2d:1a3f","prefixlen":64,"scope":"link"}]}]`},
	}}
	p := New(f)
	vs := model.VirtualSwitch{Name: "vs-lan", Type: "l2",
		Gateway: &model.VSGateway{Addresses: []string{"192.168.99.1/24"}}}
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatal(err)
	}
	if f.has("ip addr del fe80::c4a:11ff:fe2d:1a3f/64") {
		t.Fatalf("链路本地地址不得删除；实际：\n%s", f.joined())
	}
	if !f.has("ip addr del 192.168.99.2/24 dev vs-lan") {
		t.Fatalf("声明外的 global 地址应删除；实际：\n%s", f.joined())
	}
}
