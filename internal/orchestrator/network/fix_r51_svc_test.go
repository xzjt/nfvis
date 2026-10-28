package network

// round51 评审缺陷收口（N5：QoS 策略的 CIR/CBS 变更不得成为无声空操作）的守护用例。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// 修复前症状：`ApplyQos` 只发一条 `policer_add_del(add=true, name, cir, cb)`，而 VPP 的
// policer 以**名字**为键、适配器又把 -81（VALUE_EXIST）当幂等成功吞掉 ⇒ 改 CIR 后接口收下、
// 登记/读视图/审计都报新值，数据面却继续按旧速率计量（静默错配）。
// 旧假实现把速率就地覆盖，「改 CIR 在数据面不生效」因此在单测里完全不可见。
func TestFixR51N5QosRateChangeRebuildsPolicer(t *testing.T) {
	f := newFakeSvc()
	p := NewServicesProvider(f)
	ctx := context.Background()
	if err := p.ApplyQos(ctx, model.QosPolicy{Name: "pol1", Cir: 1000000, Cbs: 10000}); err != nil {
		t.Fatalf("首次 ApplyQos: %v", err)
	}
	if f.policers["pol1"] != 1000 { // 1 Mbps → 1000 kbps
		t.Fatalf("前置：速率应为 1000 kbps: %v", f.policers)
	}
	// 绑到口上：撤旧之前必须**先解绑**（policer 被接口特性引用时删不掉，同 storm 的「先摘引用后删对象」）
	if err := p.ApplyInterface(ctx, model.InterfaceConfig{Name: "ens192", IngressPolicy: "pol1"}); err != nil {
		t.Fatalf("前置绑定: %v", err)
	}
	before := len(f.addDel)

	if err := p.ApplyQos(ctx, model.QosPolicy{Name: "pol1", Cir: 2000000, Cbs: 10000}); err != nil {
		t.Fatalf("改速率: %v", err)
	}
	if f.policers["pol1"] != 2000 {
		t.Fatalf("数据面速率必须是新值（同名 add 是无声空操作 ⇒ 必须删旧建新）: %v", f.policers)
	}
	if got := f.addDel[before:]; len(got) != 2 || got[0] != "pol1:del" || got[1] != "pol1:add" {
		t.Fatalf("改速率应发「删旧 + 建新」: %v", got)
	}
	if n := len(f.pins); n < 2 || f.pins[n-2] != "pol1:off" || f.pins[n-1] != "pol1:on" {
		t.Fatalf("绑定应先解后接（policer 被引用时删不掉）: %v", f.pins)
	}
	if in, _ := svcRegs(p, "ens192"); in != "pol1" {
		t.Fatalf("重建后绑定登记应接回（登记=最后一个成功下发状态）: %q", in)
	}

	// 值未变：必须仍是幂等空操作（零 VPP 调用）——否则每次提交都会白拆白建
	addDel, pins, pouts := len(f.addDel), len(f.pins), len(f.pouts)
	if err := p.ApplyQos(ctx, model.QosPolicy{Name: "pol1", Cir: 2000000, Cbs: 10000}); err != nil {
		t.Fatalf("重复下发同值: %v", err)
	}
	if len(f.addDel) != addDel || len(f.pins) != pins || len(f.pouts) != pouts {
		t.Fatalf("值未变必须幂等（零下发）: addDel=%v pins=%v pouts=%v", f.addDel, f.pins, f.pouts)
	}

	// 出向绑定走 PolicerOutput，同样先解后接
	if err := p.ApplyInterface(ctx, model.InterfaceConfig{Name: "ens224", EgressPolicy: "pol1"}); err != nil {
		t.Fatalf("前置出向绑定: %v", err)
	}
	if err := p.ApplyQos(ctx, model.QosPolicy{Name: "pol1", Cir: 3000000, Cbs: 10000}); err != nil {
		t.Fatalf("再改速率: %v", err)
	}
	if f.policers["pol1"] != 3000 {
		t.Fatalf("出向绑定下改速率同样要生效: %v", f.policers)
	}
	if n := len(f.pouts); n < 2 || f.pouts[n-2] != "pol1:off" || f.pouts[n-1] != "pol1:on" {
		t.Fatalf("出向绑定应先解后接: %v", f.pouts)
	}
	if _, out := svcRegs(p, "ens224"); out != "pol1" {
		t.Fatalf("出向绑定登记应接回: %q", out)
	}
}

// 值变更中途失败必须**如实上抛**且不留半套状态：
//   - 撤旧失败（旧实例仍在位）⇒ 绑定接回旧实例，登记与数据面都停在「本次请求之前」；
//   - 建新失败 ⇒ 按旧参数把实例建回来并接回绑定（否则接口停在「无 policer」而配置仍声明绑定，
//     补偿再跑一次 ApplyQos(旧值) 也补不回绑定）。
func TestFixR51N5QosRateChangeFailurePaths(t *testing.T) {
	newBound := func(t *testing.T) (*fakeSvc, *ServicesProvider) {
		t.Helper()
		f := newFakeSvc()
		p := NewServicesProvider(f)
		ctx := context.Background()
		if err := p.ApplyQos(ctx, model.QosPolicy{Name: "pol1", Cir: 1000000, Cbs: 10000}); err != nil {
			t.Fatalf("首次 ApplyQos: %v", err)
		}
		if err := p.ApplyInterface(ctx, model.InterfaceConfig{Name: "ens192", IngressPolicy: "pol1"}); err != nil {
			t.Fatalf("绑定: %v", err)
		}
		return f, p
	}
	ctx := context.Background()

	// ① 撤旧失败
	f, p := newBound(t)
	f.policerAddErr = func(name string, add bool) error {
		if !add {
			return errors.New("del boom")
		}
		return nil
	}
	err := p.ApplyQos(ctx, model.QosPolicy{Name: "pol1", Cir: 2000000, Cbs: 10000})
	if err == nil || !strings.Contains(err.Error(), "del boom") {
		t.Fatalf("撤旧失败应上抛底层原因: %v", err)
	}
	if !svcPolicer(p, "pol1") {
		t.Fatal("撤旧失败时 policer 登记应保持（登记=最后成功状态）")
	}
	if f.policers["pol1"] != 1000 {
		t.Fatalf("撤旧失败时旧实例与旧速率都必须原样在位: %v", f.policers)
	}
	if in, _ := svcRegs(p, "ens192"); in != "pol1" {
		t.Fatalf("撤旧失败时绑定应接回旧实例（不留半套绑定）: %q", in)
	}
	if n := len(f.pins); n < 2 || f.pins[n-2] != "pol1:off" || f.pins[n-1] != "pol1:on" {
		t.Fatalf("应「解绑 → 撤旧失败 → 接回」: %v", f.pins)
	}

	// ② 建新失败（按旧参数回落成功）
	f, p = newBound(t)
	first := true
	f.policerAddErr = func(name string, add bool) error {
		if add && first {
			first = false
			return errors.New("add boom")
		}
		return nil
	}
	err = p.ApplyQos(ctx, model.QosPolicy{Name: "pol1", Cir: 2000000, Cbs: 10000})
	if err == nil || !strings.Contains(err.Error(), "add boom") {
		t.Fatalf("建新失败应上抛底层原因: %v", err)
	}
	if f.policers["pol1"] != 1000 {
		t.Fatalf("建新失败后应按旧参数把实例建回来: %v", f.policers)
	}
	if in, _ := svcRegs(p, "ens192"); in != "pol1" {
		t.Fatalf("建新失败后绑定应接回: %q", in)
	}
	// 回落完整：补偿 ApplyQos(旧值) 是幂等空操作（值与登记一致）
	before := len(f.addDel)
	if err := p.ApplyQos(ctx, model.QosPolicy{Name: "pol1", Cir: 1000000, Cbs: 10000}); err != nil {
		t.Fatalf("补偿 ApplyQos(旧值): %v", err)
	}
	if len(f.addDel) != before {
		t.Fatalf("回落到位时补偿应是空操作: %v", f.addDel[before:])
	}
}

// DeleteSpan 的「接口已不在数据面」容错（与 DeleteGateway/DeleteBridgeDomain/ACL 解绑同族）。
//
// 修复前症状：镜像的源/目的口消失（同一次提交里删了 VNF vNIC 或该接口元素、口被重建换了
// 索引、带外 request vpp restart 之后登记里还是旧索引）时，VPP 对
// `sw_interface_span_enable_disable` 回 `Invalid sw_if_index (-2)`，而 DeleteSpan 把它当硬错
// ⇒ 整次提交回滚，操作者无法在一次提交里把「删口 + 撤镜像」做干净（登记每次都被保留、下次
// 再撞同一个码）。镜像随接口一起消失，属**已达成**：容错、摘登记；其余错误照旧上抛。
func TestFixR51N7DeleteSpanToleratesMissingIface(t *testing.T) {
	f := newFakeSvc()
	p := NewServicesProvider(f)
	ctx := context.Background()
	pm := model.PortMirroring{Name: "pm1",
		Source: model.PMSource{Interface: "ens192", Direction: "both"}, Analyzer: "ens224"}
	if err := p.ApplySpan(ctx, pm); err != nil {
		t.Fatalf("前置 ApplySpan: %v", err)
	}
	// 源口后来从数据面消失（假装 -2 现场）
	f.deadIfaces[1] = true
	if err := p.DeleteSpan(ctx, "pm1"); err != nil {
		t.Fatalf("源口已不在数据面时应按已达成（-2 容错），不得打断整次提交: %v", err)
	}
	if len(f.spanOff) != 1 {
		t.Fatalf("仍应下发一次关闭（记录事实行为）: %v", f.spanOff)
	}
	// 登记必须摘掉（镜像确实没了；留着会让下次撤镜像再撞同一个码、且读视图说镜像还在）
	if err := p.DeleteSpan(ctx, "pm1"); err != nil {
		t.Fatalf("重复删除应幂等: %v", err)
	}
	if len(f.spanOff) != 1 {
		t.Fatalf("摘登记后不得再下发关闭: %v", f.spanOff)
	}

	// 目的口（分析口）消失同理
	f2 := newFakeSvc()
	p2 := NewServicesProvider(f2)
	if err := p2.ApplySpan(ctx, pm); err != nil {
		t.Fatalf("前置 ApplySpan: %v", err)
	}
	f2.deadIfaces[2] = true
	if err := p2.DeleteSpan(ctx, "pm1"); err != nil {
		t.Fatalf("分析口已不在数据面时同样按已达成: %v", err)
	}

	// 其余错误照旧上抛、登记保留（可重试）
	f3 := newFakeSvc()
	p3 := NewServicesProvider(f3)
	if err := p3.ApplySpan(ctx, pm); err != nil {
		t.Fatalf("前置 ApplySpan: %v", err)
	}
	f3.spanOffErr = func(_, _ uint32) error { return errors.New("vpp down") }
	if err := p3.DeleteSpan(ctx, "pm1"); err == nil || !strings.Contains(err.Error(), "vpp down") {
		t.Fatalf("非「接口已消失」的错误必须上抛: %v", err)
	}
	if !svcSpanTracked(p3, "pm1") {
		t.Fatal("非容错类失败时登记必须保留（重试可再关，决策 #363）")
	}
	// 重试（此时注入的错误已撤）成功并摘登记
	f3.spanOffErr = nil
	if err := p3.DeleteSpan(ctx, "pm1"); err != nil {
		t.Fatalf("重试关闭: %v", err)
	}
	if svcSpanTracked(p3, "pm1") {
		t.Fatal("成功关闭后登记应摘除")
	}
}
