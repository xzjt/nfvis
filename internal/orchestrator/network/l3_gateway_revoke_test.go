package network

// round49（外部走查）：删除 L2 网关（保留交换机）不下发拆除。
//
// 缺陷形态：提交计划对**变更的** L2 交换机只生成 ApplyBridgeDomain（provider.go）→
// l2.ApplyBridgeDomain + l3.ApplyGateway，而 ApplyGateway 遇 `gw == nil` 直接 return nil；
// 拆 BVI/专属表只发生在**整台交换机被删**时（DeleteBridgeDomain → DeleteGateway）。
// 于是 `delete virtual-switches vs-lan gateway ip 10.0.0.1/24` 提交成功、读视图干净，
// 而 VPP 里 BVI 仍在（地址仍在、仍在该专属 VRF 表里），域内主机继续把已删的网关当可用——
// 配置与数据面不一致且全程零报错。
//
// 本文件钉住「撤销语义」：gw == nil 即等价于 DeleteGateway（清地址 → 删 BVI → 回收专属表），
// 幂等、不误删共享/显式 VRF 的表；地址收缩由结果（BVI 上地址集合）钉住。

import (
	"context"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// bviOfFake 取假客户端上唯一的 BVI（ApplyGateway 建的）。
func bviOfFake(t *testing.T, f *fakeL3) uint32 {
	t.Helper()
	for idx := range f.bviBD {
		return idx
	}
	t.Fatal("假客户端上应已有一个 BVI")
	return 0
}

func TestL3GatewayRemovedTearsDownBVI(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	ctx := context.Background()
	vs := model.VirtualSwitch{Name: "vs-app", Type: "l2",
		Gateway: &model.VSGateway{Addresses: []string{"192.168.100.1/24", "2001:db8:100::1/64"}}}
	if err := p.ApplyGateway(ctx, vs); err != nil {
		t.Fatalf("ApplyGateway: %v", err)
	}
	bvi := bviOfFake(t, f)
	gwTable := TableID(GatewayVRFName("vs-app"))

	clearedBefore := len(f.cleared)
	if err := p.ApplyGateway(ctx, model.VirtualSwitch{Name: "vs-app", Type: "l2"}); err != nil {
		t.Fatalf("撤销网关（Gateway 声明消失、交换机保留）应成功: %v", err)
	}
	// 1) 先清 BVI 上的地址（v4/v6 都清——delAll 一次覆盖两族）
	sawClear := false
	for _, idx := range f.cleared[clearedBefore:] {
		if idx == bvi {
			sawClear = true
		}
	}
	if !sawClear {
		t.Fatalf("撤销网关必须清理 BVI %d 的地址（含 IPv6）；实际 cleared=%v", bvi, f.cleared)
	}
	// 2) 删 BVI
	sawDelete := false
	for _, idx := range f.bviGone {
		if idx == bvi {
			sawDelete = true
		}
	}
	if !sawDelete {
		t.Fatalf("撤销网关必须删除 BVI %d；实际 bviGone=%v", bvi, f.bviGone)
	}
	// 3) 回收专属网关 VRF 表（派生 vr-<交换机>）
	if f.tables[gwTable] {
		t.Fatalf("撤销网关必须回收专属 VRF 表 %d（vr-vs-app）", gwTable)
	}
	// 4) 登记清理：再撤一次幂等（不因 BVI 已不在而报错，也不再新增动作）
	errsBefore := len(f.bviGone) + len(f.cleared)
	if err := p.ApplyGateway(ctx, model.VirtualSwitch{Name: "vs-app", Type: "l2"}); err != nil {
		t.Fatalf("重复撤销网关应幂等: %v", err)
	}
	if got := len(f.bviGone) + len(f.cleared); got != errsBefore {
		t.Fatalf("重复撤销不应再下发动作（登记应已清）；实际新增 %d 条", got-errsBefore)
	}
}

// 显式 `gateway vrf <共享名>`：撤销网关只拆 BVI 与地址，**不动**操作者声明的共享表
// （该 VRF 是 vrf 段里的对象，生命周期不由这台交换机决定）。
func TestL3GatewayRemovedKeepsSharedVRFTable(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	ctx := context.Background()
	vs := model.VirtualSwitch{Name: "vs-dmz", Type: "l2",
		Gateway: &model.VSGateway{Vrf: "vs-shared", Addresses: []string{"10.10.10.1/24"}}}
	if err := p.ApplyGateway(ctx, vs); err != nil {
		t.Fatalf("ApplyGateway: %v", err)
	}
	bvi := bviOfFake(t, f)
	if err := p.ApplyGateway(ctx, model.VirtualSwitch{Name: "vs-dmz", Type: "l2"}); err != nil {
		t.Fatalf("撤销网关应成功: %v", err)
	}
	if len(f.bviGone) != 1 || f.bviGone[0] != bvi {
		t.Fatalf("共享 VRF 场景仍应删 BVI；实际 %v", f.bviGone)
	}
	if !f.tables[TableID("vs-shared")] {
		t.Fatal("共享/显式 VRF 的表不得被网关撤销删除（那是操作者的对象）")
	}
}

// 地址收缩（两地址 → 一个）：BVI 上最终只剩声明的那个。
//
// 说明（round49 核实）：VPP 侧 ApplyGateway 对 BVI 是「先 delAll 清空、再按声明逐条 add」，
// 故收缩本就被覆盖；此处以**结果**钉住它不回归（不经登记做差集，也就不会在重启后
// 留下「登记里没有、VPP 里却有」的陈旧地址）。
func TestL3GatewayAddressShrinkKeepsOnlyDeclared(t *testing.T) {
	f := newFakeL3()
	p := NewL3Provider(f)
	ctx := context.Background()
	p.ApplyGateway(ctx, model.VirtualSwitch{Name: "vs-shrink", Type: "l2",
		Gateway: &model.VSGateway{Addresses: []string{"10.0.0.1/24", "10.0.1.1/24"}}})
	bvi := bviOfFake(t, f)
	if len(f.addrs[bvi]) != 2 {
		t.Fatalf("前置：BVI 应有两个地址，实际 %v", f.addrs[bvi])
	}
	if err := p.ApplyGateway(ctx, model.VirtualSwitch{Name: "vs-shrink", Type: "l2",
		Gateway: &model.VSGateway{Addresses: []string{"10.0.0.1/24"}}}); err != nil {
		t.Fatalf("收缩应成功: %v", err)
	}
	if got := f.addrs[bvi]; len(got) != 1 || got[0] != "10.0.0.1/24" {
		t.Fatalf("收缩后 BVI 上应只剩 10.0.0.1/24，实际 %v", got)
	}
}

// 经装饰器（提交计划的真实路径）：变更的 L2 交换机只走 ApplyBridgeDomain，
// 网关声明消失时那里必须完成拆除。
func TestDecoratorApplyBridgeDomainTearsDownRemovedGateway(t *testing.T) {
	l2f := newFakeL2()
	l3f := newFakeL3()
	n := NewL2Network(nil, NewL2Provider(l2f))
	n.SetL3(NewL3Provider(l3f))
	ctx := context.Background()
	vs := l2vs("vs-gw", model.VSwitchPort{Seq: 0, Interface: "ens192"})
	vs.Gateway = &model.VSGateway{Addresses: []string{"10.9.9.1/24"}}
	if err := n.ApplyBridgeDomain(ctx, vs); err != nil {
		t.Fatalf("ApplyBridgeDomain(网关): %v", err)
	}
	bvi := bviOfFake(t, l3f)
	// 同一台交换机、网关声明被删（计划里就是这个 vs）
	vs.Gateway = nil
	if err := n.ApplyBridgeDomain(ctx, vs); err != nil {
		t.Fatalf("ApplyBridgeDomain(网关已删): %v", err)
	}
	if len(l3f.bviGone) != 1 || l3f.bviGone[0] != bvi {
		t.Fatalf("经装饰器也必须拆除 BVI %d；实际 %v", bvi, l3f.bviGone)
	}
	if l3f.tables[TableID(GatewayVRFName("vs-gw"))] {
		t.Fatal("经装饰器也必须回收派生网关表")
	}
}
