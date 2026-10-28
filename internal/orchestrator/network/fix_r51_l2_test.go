package network

// round51 评审缺陷收口（N3：bridge-domain 拆除必须归属感知）的守护用例。
//
// 假实现（fakeL2）按 VPP 的真实语义建模：`SwInterfaceSetL2Bridge(enable=false)` 只按
// sw_if_index 摘除成员（**不看 bd_id**）、`DeleteSubif` 真删接口、`CreateSubif` 对已存在的
// 子接口回 -56、按确定性名可查回。故修复前的用例是红的。

import (
	"context"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// 修复前症状：同一次提交里「删交换机 A」+「建交换机 B（**同一物理口 + 同一 access VLAN**）」——
// apply 段先跑 B：ensureSubif 复用 A 当年建的 `<父口>.<vlan>`（索引 X）并把它登记到 B 名下；
// 删除段再跑 A：A 按 takeAll(bdA) 逐索引摘除成员，而 VPP 的 sw_interface_set_l2_bridge(enable=false)
// **只认 sw_if_index**，于是把这个已属于 B 的口从 **B 的** bridge-domain 里摘掉，紧接着
// DeleteSubif(X) 把接口本身删掉——B 的端口在数据面被静默黑洞（无告警），登记里还留死索引。
func TestFixR51N3DeleteAKeepsBTakeoverSubif(t *testing.T) {
	f := newFakeL2()
	p := NewL2Provider(f)
	ctx := context.Background()

	vsA := l2vs("vs-a", model.VSwitchPort{Seq: 0, Interface: "ens192"})
	vsA.VlanAccess = 100
	if err := p.ApplyBridgeDomain(ctx, vsA); err != nil {
		t.Fatalf("前置建 A: %v", err)
	}
	sub, ok := f.nameIdx["ens192.100"]
	if !ok {
		t.Fatalf("前置：A 应建出 ens192.100: %v", f.nameIdx)
	}

	// 同一次提交（顺序即 apply.go 的顺序）：先建 B（复用同一个子接口），再删 A。
	vsB := l2vs("vs-b", model.VSwitchPort{Seq: 0, Interface: "ens192"})
	vsB.VlanAccess = 100
	if err := p.ApplyBridgeDomain(ctx, vsB); err != nil {
		t.Fatalf("建 B: %v", err)
	}
	if f.bridge[sub] != BDID("vs-b") {
		t.Fatalf("前置：B 应接管该子接口（幂等复用）: %v", f.bridge)
	}

	if err := p.DeleteBridgeDomain(ctx, "vs-a"); err != nil {
		t.Fatalf("删 A: %v", err)
	}
	if f.bridge[sub] != BDID("vs-b") {
		t.Fatalf("A 的拆除把**B 的**端口从 BD 里摘掉了（VPP 只认 sw_if_index）: bridge=%v", f.bridge)
	}
	if _, ok := f.names[sub]; !ok {
		t.Fatalf("B 仍在用的子接口被 A 的回收删掉了: %v", f.names)
	}
	if f.bds[BDID("vs-a")] {
		t.Fatal("A 自己的 BD 仍应删除")
	}
	// B 照常可用：再次收敛 B 不报错、成员不变、子接口不被重复创建。
	before := len(f.subifs)
	if err := p.ApplyBridgeDomain(ctx, vsB); err != nil {
		t.Fatalf("B 再收敛: %v", err)
	}
	if f.bridge[sub] != BDID("vs-b") || len(f.subifs) != before {
		t.Fatalf("B 的成员与子接口应保持: bridge=%v subifs=%v", f.bridge, f.subifs)
	}
}

// 同形态的**物理口直挂**版本（无 VLAN 子接口时同样是被 VPP 按索引摘除）。
func TestFixR51N3DeleteAKeepsBPlainPort(t *testing.T) {
	f := newFakeL2()
	p := NewL2Provider(f)
	ctx := context.Background()
	vsA := l2vs("vs-a", model.VSwitchPort{Seq: 0, Interface: "ens192"})
	if err := p.ApplyBridgeDomain(ctx, vsA); err != nil {
		t.Fatalf("前置建 A: %v", err)
	}
	vsB := l2vs("vs-b", model.VSwitchPort{Seq: 0, Interface: "ens192"})
	if err := p.ApplyBridgeDomain(ctx, vsB); err != nil {
		t.Fatalf("建 B: %v", err)
	}
	if err := p.DeleteBridgeDomain(ctx, "vs-a"); err != nil {
		t.Fatalf("删 A: %v", err)
	}
	if f.bridge[1] != BDID("vs-b") {
		t.Fatalf("A 的拆除把 B 的物理口从 BD 里摘掉了: %v", f.bridge)
	}
}

// 「A 保留、只把端口改到别的口」时，A 的陈旧成员摘除同样不得动已被 B 接管的口
// （同一提交里 apply 段先跑 B、再跑 A 的变更）。
func TestFixR51N3StaleDetachKeepsTakeover(t *testing.T) {
	f := newFakeL2()
	p := NewL2Provider(f)
	ctx := context.Background()
	vsA := l2vs("vs-a", model.VSwitchPort{Seq: 0, Interface: "ens192"})
	vsA.VlanAccess = 100
	if err := p.ApplyBridgeDomain(ctx, vsA); err != nil {
		t.Fatalf("前置建 A: %v", err)
	}
	sub, ok := f.nameIdx["ens192.100"]
	if !ok {
		t.Fatalf("前置：A 应建出 ens192.100: %v", f.nameIdx)
	}

	// B 接管同一口（apply 段在前）
	vsB := l2vs("vs-b", model.VSwitchPort{Seq: 0, Interface: "ens192"})
	vsB.VlanAccess = 100
	if err := p.ApplyBridgeDomain(ctx, vsB); err != nil {
		t.Fatalf("建 B: %v", err)
	}
	// A 改到另一个口（原先的成员因此成 stale）
	vsA.Ports = []model.VSwitchPort{{Seq: 0, Interface: "ens224"}}
	if err := p.ApplyBridgeDomain(ctx, vsA); err != nil {
		t.Fatalf("A 改端口: %v", err)
	}
	if f.bridge[sub] != BDID("vs-b") {
		t.Fatalf("A 摘陈旧成员把 B 的口摘掉了: %v", f.bridge)
	}
	if _, ok := f.names[sub]; !ok {
		t.Fatalf("A 的子接口回收把 B 正在用的子接口删掉了: %v", f.names)
	}
	// A 自己换的口照常挂上
	newSub, ok := f.nameIdx["ens224.100"]
	if !ok || f.bridge[newSub] != BDID("vs-a") {
		t.Fatalf("A 的新端口应挂在自己的 BD 上: %v %v", f.nameIdx, f.bridge)
	}
}
