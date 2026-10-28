package netkernel

// round51 P4：L2 网关**换 VRF 目标**时，上一位派生的网关 VRF 设备与它的域兜底路由不被回收。
//
// 缺陷形态（静态评审定位）：applyGateway 按声明算出目标 VRF（无显式 `gateway vrf` 时用派生的
// `vr-<交换机>`）、ensure 它、把 bridge 重挂到它名下——但从不回收**上一位派生**的设备。回收
// 只发生在「网关声明消失」（revokeGateway：仅当 bridge 当前 master 仍是派生名）与「交换机被删」
// （DeleteBridgeDomain）两条路径上，于是 `gateway ip <cidr>`（派生 vr-vs-lan）改成
// `gateway vrf <显式名>` 之后，内核里长期留一张空 VRF 与两条 `unreachable default`（决策 #446），
// 而读视图里都看不到它们（交换机详情只报网关地址、VRF 列表只报已声明的交换机）。
//
// 边界与 revokeGateway 同口径：**只回收派生名**，显式 `gateway vrf <名>` 的对象一律不动
// （它是操作者声明在 vrf 段里的对象，生命周期不由这台交换机决定）。

import (
	"context"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// TestApplyBridgeDomainSwitchesGatewayToExplicitVrfReclaimsDerived 派生 → 显式：回收派生
// 设备（含两族兜底路由），且在重挂 master 之前完成。
func TestApplyBridgeDomainSwitchesGatewayToExplicitVrfReclaimsDerived(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "ip -j addr show dev vs-lan", out: `[{"addr_info":[{"local":"10.10.10.1","prefixlen":24,"scope":"global"}]}]`},
		{prefix: "ip -d -j link show dev vs-lan", out: `[{"ifname":"vs-lan","master":"vr-vs-lan",` +
			`"linkinfo":{"info_kind":"bridge","info_slave_kind":"vrf"}}]`},
		{prefix: "ip -d -j link show dev vr-vs-lan", out: `[{"ifname":"vr-vs-lan","linkinfo":{"info_kind":"vrf"}}]`},
	}}
	p := New(f)
	vs := model.VirtualSwitch{Name: "vs-lan", Type: "l2",
		Gateway: &model.VSGateway{Vrf: "vs-l3", Addresses: []string{"10.10.10.1/24"}}}
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ip route del vrf vr-vs-lan unreachable default metric " + vrfFallbackMetric,
		"ip -6 route del vrf vr-vs-lan unreachable default metric " + vrfFallbackMetric,
		"ip link del vr-vs-lan",
		"ip link set dev vs-lan master vs-l3",
	} {
		if !f.has(want) {
			t.Fatalf("换网关 VRF 目标应下发 %q；实际：\n%s", want, f.joined())
		}
	}
	// 先回收旧派生设备、再重挂 master（设备删除会顺带把 bridge 从它上面摘下来）。
	reclaim, remaster := -1, -1
	for i, c := range f.calls {
		if c == "ip link del vr-vs-lan" {
			reclaim = i
		}
		if c == "ip link set dev vs-lan master vs-l3" {
			remaster = i
		}
	}
	if reclaim < 0 || remaster < 0 || reclaim > remaster {
		t.Fatalf("派生 VRF 的回收应发生在重挂 master 之前（reclaim=%d remaster=%d）：\n%s",
			reclaim, remaster, f.joined())
	}
	// 显式目标（操作者的对象）不得被顺手回收。
	if f.has("ip link del vs-l3") || f.has("ip route del vrf vs-l3") {
		t.Fatalf("显式 gateway vrf 的设备不得被回收；实际：\n%s", f.joined())
	}
}

// TestApplyBridgeDomainSwitchesGatewayToDerivedKeepsExplicitVRF 反方向（显式 → 派生）：
// 只 ensure/重挂派生设备，操作者声明的显式 VRF 设备一概不动。
func TestApplyBridgeDomainSwitchesGatewayToDerivedKeepsExplicitVRF(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "ip -j addr show dev vs-lan", out: `[{"addr_info":[{"local":"10.10.10.1","prefixlen":24,"scope":"global"}]}]`},
		{prefix: "ip -d -j link show dev vs-lan", out: `[{"ifname":"vs-lan","master":"vs-l3",` +
			`"linkinfo":{"info_kind":"bridge","info_slave_kind":"vrf"}}]`},
		{prefix: "ip -d -j link show dev vs-l3", out: `[{"ifname":"vs-l3","linkinfo":{"info_kind":"vrf"}}]`},
		{prefix: "ip -d -j link show dev vr-vs-lan", out: `[{"ifname":"vr-vs-lan","linkinfo":{"info_kind":"vrf"}}]`},
	}}
	p := New(f)
	vs := model.VirtualSwitch{Name: "vs-lan", Type: "l2",
		Gateway: &model.VSGateway{Addresses: []string{"10.10.10.1/24"}}}
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatal(err)
	}
	if !f.has("ip link set dev vs-lan master vr-vs-lan") {
		t.Fatalf("应把 bridge 重挂到派生网关 VRF；实际：\n%s", f.joined())
	}
	if f.has("ip link del vs-l3") || f.has("ip route del vrf vs-l3") {
		t.Fatalf("显式 VRF（操作者的对象）不得被回收；实际：\n%s", f.joined())
	}
	if f.has("ip link del vr-vs-lan") {
		t.Fatalf("目标就是派生设备时不得回收它；实际：\n%s", f.joined())
	}
}
