package network

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// ---------- M3-5：SPAN / QoS / 接口下发（假 SvcClient） ----------

type fakeSvc struct {
	ifaces   map[string]uint32
	mtu      map[uint32]uint32
	spans    []string
	spanOff  []uint32
	policers map[string]uint32
	nextIdx  uint32
	pins     []string
	closed   int
	err      error
}

func newFakeSvc() *fakeSvc {
	return &fakeSvc{ifaces: map[string]uint32{"ens192": 1, "ens224": 2, "ens256": 3},
		mtu: map[uint32]uint32{}, policers: map[string]uint32{}, nextIdx: 10}
}

func (f *fakeSvc) Close() { f.closed++ }

func (f *fakeSvc) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	idx, ok := f.ifaces[ifname]
	return idx, ok, nil
}

func (f *fakeSvc) SetMTU(swIfIndex, mtu uint32) error {
	if f.err != nil {
		return f.err
	}
	f.mtu[swIfIndex] = mtu
	return nil
}

func (f *fakeSvc) SpanSet(from, to uint32, state string, _ bool) error {
	if f.err != nil {
		return f.err
	}
	f.spans = append(f.spans, state)
	return nil
}

func (f *fakeSvc) SpanDisable(from, to uint32) error {
	if f.err != nil {
		return f.err
	}
	f.spanOff = append(f.spanOff, from)
	return nil
}

func (f *fakeSvc) PolicerAddDel(name string, cirKbps uint32, cb uint64, add bool) (uint32, error) {
	if f.err != nil {
		return 0, f.err
	}
	if !add {
		delete(f.policers, name)
		return 0, nil
	}
	f.nextIdx++
	f.policers[name] = cirKbps
	return f.nextIdx, nil
}

func (f *fakeSvc) PolicerInput(swIfIndex uint32, name string, apply bool) error {
	if f.err != nil {
		return f.err
	}
	f.pins = append(f.pins, name+":"+map[bool]string{true: "on", false: "off"}[apply])
	return nil
}

func TestSpanApplyAndDelete(t *testing.T) {
	f := newFakeSvc()
	p := NewServicesProvider(f)
	pm := model.PortMirroring{Name: "pm1",
		Source: model.PMSource{Interface: "ens192", Direction: "ingress"}, Analyzer: "ens256"}
	if err := p.ApplySpan(context.Background(), pm); err != nil {
		t.Fatalf("ApplySpan: %v", err)
	}
	if len(f.spans) != 1 || f.spans[0] != "rx" {
		t.Fatalf("ingress 应为 rx: %v", f.spans)
	}
	if err := p.DeleteSpan(context.Background(), "pm1"); err != nil {
		t.Fatalf("DeleteSpan: %v", err)
	}
	if len(f.spanOff) != 1 || f.spanOff[0] != 1 {
		t.Fatalf("应关闭源口 span: %v", f.spanOff)
	}
	// 缺省方向 both
	f2 := newFakeSvc()
	if err := NewServicesProvider(f2).ApplySpan(context.Background(),
		model.PortMirroring{Name: "pm2", Source: model.PMSource{Interface: "ens192"}, Analyzer: "ens256"}); err != nil {
		t.Fatalf("ApplySpan(both): %v", err)
	}
	if f2.spans[0] != "rx_tx" {
		t.Fatalf("缺省应 rx_tx: %v", f2.spans)
	}
	// VNF 源不支持
	if err := NewServicesProvider(newFakeSvc()).ApplySpan(context.Background(),
		model.PortMirroring{Name: "pm3", Source: model.PMSource{Vnf: "vm1"}, Analyzer: "ens256"}); err == nil {
		t.Fatalf("VNF 源应报 M4")
	}
}

func TestQosApplyBindUnbind(t *testing.T) {
	f := newFakeSvc()
	p := NewServicesProvider(f)
	if err := p.ApplyQos(context.Background(), model.QosPolicy{Name: "pol1", Cir: 1000000, Cbs: 10000}); err != nil {
		t.Fatalf("ApplyQos: %v", err)
	}
	if f.policers["pol1"] != 1000 { // 1 Mbps → 1000 kbps
		t.Fatalf("policer 速率应为 1000 kbps: %v", f.policers)
	}
	// 接口绑定
	if err := p.ApplyInterface(context.Background(), model.InterfaceConfig{Name: "ens192", MTU: 9000, IngressPolicy: "pol1"}); err != nil {
		t.Fatalf("ApplyInterface: %v", err)
	}
	if f.mtu[1] != 9000 {
		t.Fatalf("应设 MTU: %v", f.mtu)
	}
	if len(f.pins) != 1 || f.pins[0] != "pol1:on" {
		t.Fatalf("应绑定策略: %v", f.pins)
	}
	// 重复下发同一策略：不重复绑定
	if err := p.ApplyInterface(context.Background(), model.InterfaceConfig{Name: "ens192", MTU: 9000, IngressPolicy: "pol1"}); err != nil {
		t.Fatalf("重复 ApplyInterface: %v", err)
	}
	if len(f.pins) != 1 {
		t.Fatalf("不应重复绑定: %v", f.pins)
	}
	// 解绑
	if err := p.ApplyInterface(context.Background(), model.InterfaceConfig{Name: "ens192", MTU: 9000}); err != nil {
		t.Fatalf("解绑: %v", err)
	}
	if len(f.pins) != 2 || f.pins[1] != "pol1:off" {
		t.Fatalf("应解绑: %v", f.pins)
	}
	// DeleteQos 幂等清理
	if err := p.DeleteQos(context.Background(), "pol1"); err != nil {
		t.Fatalf("DeleteQos: %v", err)
	}
	if _, ok := f.policers["pol1"]; ok {
		t.Fatalf("policer 应删除: %v", f.policers)
	}
}

func TestServicesErrors(t *testing.T) {
	f := newFakeSvc()
	f.err = errors.New("boom")
	p := NewServicesProvider(f)
	if err := p.ApplySpan(context.Background(), model.PortMirroring{Name: "x",
		Source: model.PMSource{Interface: "ens192"}, Analyzer: "ens256"}); err == nil {
		t.Fatalf("SPAN 错误应上抛")
	}
	if err := p.ApplyQos(context.Background(), model.QosPolicy{Name: "q", Cir: 1000}); err == nil {
		t.Fatalf("QoS 错误应上抛")
	}
	if err := p.ApplyInterface(context.Background(), model.InterfaceConfig{Name: "ens192"}); err == nil {
		t.Fatalf("接口错误应上抛")
	}
	// CIR 非正
	if err := NewServicesProvider(newFakeSvc()).ApplyQos(context.Background(), model.QosPolicy{Name: "q"}); err == nil ||
		!strings.Contains(err.Error(), "CIR") {
		t.Fatalf("CIR 非正应报错: %v", err)
	}
	// 不存在的接口
	if err := NewServicesProvider(newFakeSvc()).ApplyInterface(context.Background(),
		model.InterfaceConfig{Name: "ens999"}); err == nil || !strings.Contains(err.Error(), "DPDK") {
		t.Fatalf("缺失接口应提示 DPDK: %v", err)
	}
	// 工厂错误
	pf := NewServicesProviderFunc(func() (SvcClient, error) { return nil, ErrL2Unavailable })
	if err := pf.ApplyQos(context.Background(), model.QosPolicy{Name: "q", Cir: 1}); err == nil {
		t.Fatalf("工厂错误应上抛")
	}
}

func TestServicesProviderClosedEachCall(t *testing.T) {
	f := newFakeSvc()
	p := NewServicesProvider(f)
	_ = p.ApplyQos(context.Background(), model.QosPolicy{Name: "q", Cir: 1000})
	_ = p.DeleteQos(context.Background(), "q")
	_ = p.ApplyInterface(context.Background(), model.InterfaceConfig{Name: "ens192"})
	if f.closed != 3 {
		t.Fatalf("每次操作应关闭 channel: %d", f.closed)
	}
}
