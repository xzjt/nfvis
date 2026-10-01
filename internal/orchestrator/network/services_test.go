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
	state    map[uint32]bool
	spans    []string
	spanOff  []uint32
	policers map[string]uint32
	nextIdx  uint32
	pins     []string
	pouts    []string
	closed   int
	err      error
}

func newFakeSvc() *fakeSvc {
	return &fakeSvc{ifaces: map[string]uint32{"ens192": 1, "ens224": 2, "ens256": 3},
		mtu: map[uint32]uint32{}, state: map[uint32]bool{}, policers: map[string]uint32{}, nextIdx: 10}
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

func (f *fakeSvc) SetState(swIfIndex uint32, up bool) error {
	if f.err != nil {
		return f.err
	}
	f.state[swIfIndex] = up
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

func (f *fakeSvc) PolicerOutput(swIfIndex uint32, name string, apply bool) error {
	if f.err != nil {
		return f.err
	}
	f.pouts = append(f.pouts, name+":"+map[bool]string{true: "on", false: "off"}[apply])
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
	if !f.state[1] {
		t.Fatalf("缺省应 up: %v", f.state)
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
	// 解绑（并验证 disable 映射 down）
	disabled := false
	if err := p.ApplyInterface(context.Background(), model.InterfaceConfig{Name: "ens192", MTU: 9000, Enabled: &disabled}); err != nil {
		t.Fatalf("解绑: %v", err)
	}
	if f.state[1] {
		t.Fatalf("enabled=false 应 down: %v", f.state)
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

// TestQosEgressBindUnbind 决策 #331：出向绑定走 policer_output，与入向**并存且各自独立**；
// 同接口同时绑两向、改一向不动另一向；DeleteQos 把两向都解绑后再删。
func TestQosEgressBindUnbind(t *testing.T) {
	f := newFakeSvc()
	p := NewServicesProvider(f)
	if err := p.ApplyQos(context.Background(), model.QosPolicy{Name: "pol1", Cir: 1000000}); err != nil {
		t.Fatalf("ApplyQos: %v", err)
	}
	if err := p.ApplyQos(context.Background(), model.QosPolicy{Name: "pol2", Cir: 2000000}); err != nil {
		t.Fatalf("ApplyQos: %v", err)
	}
	// 同一接口：入向 pol1、出向 pol2 —— 两者并存
	if err := p.ApplyInterface(context.Background(), model.InterfaceConfig{
		Name: "ens192", IngressPolicy: "pol1", EgressPolicy: "pol2"}); err != nil {
		t.Fatalf("ApplyInterface: %v", err)
	}
	if len(f.pins) != 1 || f.pins[0] != "pol1:on" {
		t.Fatalf("入向应绑定 pol1: %v", f.pins)
	}
	if len(f.pouts) != 1 || f.pouts[0] != "pol2:on" {
		t.Fatalf("出向应绑定 pol2: %v", f.pouts)
	}
	// 只改出向（pol2 → pol1）→ 不动入向；出向先解绑旧、再绑新
	if err := p.ApplyInterface(context.Background(), model.InterfaceConfig{
		Name: "ens192", IngressPolicy: "pol1", EgressPolicy: "pol1"}); err != nil {
		t.Fatalf("改出向: %v", err)
	}
	if len(f.pins) != 1 {
		t.Fatalf("只改出向不应动入向: %v", f.pins)
	}
	if len(f.pouts) != 3 || f.pouts[1] != "pol2:off" || f.pouts[2] != "pol1:on" {
		t.Fatalf("出向应先解绑 pol2 再绑 pol1: %v", f.pouts)
	}
	// 清掉入向、保留出向（出向已是 pol1，不应再有任何出向动作）
	if err := p.ApplyInterface(context.Background(), model.InterfaceConfig{
		Name: "ens192", EgressPolicy: "pol1"}); err != nil {
		t.Fatalf("清入向: %v", err)
	}
	if len(f.pins) != 2 || f.pins[1] != "pol1:off" {
		t.Fatalf("入向应解绑: %v", f.pins)
	}
	if len(f.pouts) != 3 || f.pouts[2] != "pol1:on" {
		t.Fatalf("出向已是 pol1，清入向不应再动出向: %v", f.pouts)
	}
	// DeleteQos 把 pol1 的两向绑定都解掉（当前只剩出向）
	if err := p.DeleteQos(context.Background(), "pol1"); err != nil {
		t.Fatalf("DeleteQos: %v", err)
	}
	if len(f.pouts) != 4 || f.pouts[3] != "pol1:off" {
		t.Fatalf("DeleteQos 应解绑出向: %v", f.pouts)
	}
	if _, ok := f.policers["pol1"]; ok {
		t.Fatalf("policer pol1 应删除: %v", f.policers)
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

// ---------- 决策 #335：交换机 DHCP 中继（VPP dhcp proxy） ----------

// proxyCall 一次 ProxySet 调用的实参快照（断言消息字段与方向用）。
type proxyCall struct {
	rx, srvVrf uint32
	isAdd      bool
	server     string
	src        string
}

type fakeDhcp struct {
	calls []proxyCall
	err   error
}

func (f *fakeDhcp) ProxySet(rxVrfID, serverVrfID uint32, isAdd bool, server, src string) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, proxyCall{rx: rxVrfID, srvVrf: serverVrfID, isAdd: isAdd, server: server, src: src})
	return nil
}

func (f *fakeDhcp) Close() {}

func TestDhcpRelayApplyAndClear(t *testing.T) {
	f := &fakeDhcp{}
	p := NewDhcpProvider(f)
	vs := model.VirtualSwitch{Name: "vs-a", Type: "l2",
		Gateway:         &model.VSGateway{Addresses: []string{"192.168.100.1/24", "2001:db8:100::1/64"}},
		DhcpRelayServer: "192.168.100.2"}

	// apply：表 id 取网关专属 VRF（vr-<名>），源地址自动取 BVI 的第一个 IPv4 网关地址（跳过 v6）
	if err := p.SyncRelay(context.Background(), vs); err != nil {
		t.Fatalf("SyncRelay(apply): %v", err)
	}
	want := TableID(GatewayVRFName("vs-a"))
	if len(f.calls) != 1 {
		t.Fatalf("应恰好下发一次 proxy，实际 %v", f.calls)
	}
	c := f.calls[0]
	if c.rx != want || c.srvVrf != want || !c.isAdd || c.server != "192.168.100.2" || c.src != "192.168.100.1" {
		t.Fatalf("proxy 字段不符: %+v（want rx=srvVrf=%d）", c, want)
	}

	// 声明未变：幂等跳过（不再下发）
	if err := p.SyncRelay(context.Background(), vs); err != nil {
		t.Fatalf("SyncRelay(幂等): %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("声明未变不应重复下发，实际 %v", f.calls)
	}

	// 清 relay：按登记值发 IsAdd=false
	vs.DhcpRelayServer = ""
	if err := p.SyncRelay(context.Background(), vs); err != nil {
		t.Fatalf("SyncRelay(clear): %v", err)
	}
	if len(f.calls) != 2 || f.calls[1].isAdd || f.calls[1].server != "192.168.100.2" || f.calls[1].src != "192.168.100.1" {
		t.Fatalf("清 relay 应按登记值撤销: %v", f.calls)
	}
	// 已无登记：再次清是空操作（幂等）
	if err := p.SyncRelay(context.Background(), vs); err != nil {
		t.Fatalf("SyncRelay(再清): %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("无登记时清 relay 不应下发: %v", f.calls)
	}
}

func TestDhcpRelayChangeServerAndGatewayDomain(t *testing.T) {
	f := &fakeDhcp{}
	p := NewDhcpProvider(f)
	vs := model.VirtualSwitch{Name: "vs-a", Type: "l2",
		Gateway:         &model.VSGateway{Addresses: []string{"192.168.100.1/24"}},
		DhcpRelayServer: "192.168.100.2"}
	if err := p.SyncRelay(context.Background(), vs); err != nil {
		t.Fatalf("初次 apply: %v", err)
	}

	// 改 server：同域重发 IsAdd=true（覆盖）
	vs.DhcpRelayServer = "10.0.0.99"
	if err := p.SyncRelay(context.Background(), vs); err != nil {
		t.Fatalf("改 server: %v", err)
	}
	if len(f.calls) != 2 || !f.calls[1].isAdd || f.calls[1].server != "10.0.0.99" {
		t.Fatalf("改 server 应重发 proxy: %v", f.calls)
	}

	// 网关换域（gateway.vrf）：先撤旧域 proxy，再下发新域
	vs.Gateway = &model.VSGateway{Vrf: "vs-mgmt", Addresses: []string{"10.10.0.1/24"}}
	if err := p.SyncRelay(context.Background(), vs); err != nil {
		t.Fatalf("换网关域: %v", err)
	}
	if len(f.calls) != 4 {
		t.Fatalf("换域应先撤旧再下发新，实际 %v", f.calls)
	}
	oldID, newID := TableID(GatewayVRFName("vs-a")), TableID("vs-mgmt")
	if f.calls[2].isAdd || f.calls[2].rx != oldID || f.calls[2].server != "10.0.0.99" {
		t.Fatalf("第 3 步应撤旧域: %+v", f.calls[2])
	}
	if !f.calls[3].isAdd || f.calls[3].rx != newID || f.calls[3].src != "10.10.0.1" || f.calls[3].server != "10.0.0.99" {
		t.Fatalf("第 4 步应在新域下发: %+v", f.calls[3])
	}
}

func TestDhcpRelayDeleteAndErrors(t *testing.T) {
	f := &fakeDhcp{}
	p := NewDhcpProvider(f)
	vs := model.VirtualSwitch{Name: "vs-a", Type: "l2",
		Gateway:         &model.VSGateway{Addresses: []string{"192.168.100.1/24"}},
		DhcpRelayServer: "192.168.100.2"}
	if err := p.SyncRelay(context.Background(), vs); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// 删交换机连带撤 proxy（IsAdd=false，按登记值）；无登记再删是空操作
	if err := p.DeleteRelay(context.Background(), "vs-a"); err != nil {
		t.Fatalf("DeleteRelay: %v", err)
	}
	if len(f.calls) != 2 || f.calls[1].isAdd || f.calls[1].server != "192.168.100.2" {
		t.Fatalf("删交换机应撤 proxy: %v", f.calls)
	}
	if err := p.DeleteRelay(context.Background(), "vs-a"); err != nil {
		t.Fatalf("DeleteRelay(无登记): %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("无登记时删除不应下发: %v", f.calls)
	}

	// 底座错误上抛（与 QoS/SPAN 删除同一口径）
	f2 := &fakeDhcp{err: errors.New("boom")}
	p2 := NewDhcpProvider(f2)
	if err := p2.SyncRelay(context.Background(), vs); err == nil {
		t.Fatal("下发错误应上抛")
	}
	// 无网关的交换机声明了 relay（防御：提交校验已挡，编排层仍不静默）
	if err := p2.SyncRelay(context.Background(),
		model.VirtualSwitch{Name: "vs-b", Type: "l2", DhcpRelayServer: "10.0.0.1"}); err == nil ||
		!strings.Contains(err.Error(), "gateway ip") {
		t.Fatalf("无网关应报错并指向 gateway ip: %v", err)
	}
}

func TestDhcpRelayRecoveryReplay(t *testing.T) {
	f := newRecoveryFixture()
	vs := l2Switch("vs-a", "ens192")
	vs.Gateway = &model.VSGateway{Addresses: []string{"192.168.100.1/24"}}
	vs.DhcpRelayServer = "192.168.100.2"
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{vs}}

	// 首次收敛即重放 relay（登记为空）
	errs := f.net.EnsureConsistent(context.Background(), cfg)
	if len(errs) != 0 {
		t.Fatalf("应收敛成功，实际: %v", errs)
	}
	want := TableID(GatewayVRFName("vs-a"))
	calls := f.dhcp.calls
	if len(calls) != 1 || !calls[0].isAdd || calls[0].rx != want ||
		calls[0].server != "192.168.100.2" || calls[0].src != "192.168.100.1" {
		t.Fatalf("恢复重放应含 relay proxy: %v", calls)
	}

	// 模拟 VPP 重启：登记清空后重放同一条 proxy 消息（幂等重发）
	if errs := f.net.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("重复收敛应成功: %v", errs)
	}
	calls = f.dhcp.calls
	if len(calls) != 2 || !calls[1].isAdd || calls[1].rx != want {
		t.Fatalf("重放后应再次下发 relay: %v", calls)
	}

	// 下发失败 → 独立记源（virtual-switches/<名>/dhcp-relay）且不阻塞其余对象
	f.dhcp.err = errors.New("boom")
	f.dhcp.calls = nil
	errs = f.net.EnsureConsistent(context.Background(), cfg)
	if len(errs) == 0 {
		t.Fatal("relay 下发失败应记未收敛项")
	}
	found := false
	for _, a := range f.alarms.List(AlarmActive) {
		if a.Source == "virtual-switches/vs-a/dhcp-relay" && a.Code == AlarmUnconverged {
			found = true
		}
	}
	if !found {
		t.Fatalf("失败项应以 dhcp-relay 独立记源进告警，实际: %+v", f.alarms.List(AlarmActive))
	}
}

func TestDeleteBridgeDomainRemovesRelay(t *testing.T) {
	f := newRecoveryFixture()
	vs := l2Switch("vs-a", "ens192")
	vs.Gateway = &model.VSGateway{Addresses: []string{"192.168.100.1/24"}}
	vs.DhcpRelayServer = "192.168.100.2"
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{vs}}
	if errs := f.net.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("收敛: %v", errs)
	}
	f.dhcp.calls = nil

	// 删交换机：先撤 proxy（IsAdd=false），再拆网关与 BD
	if err := f.net.DeleteBridgeDomain(context.Background(), "vs-a"); err != nil {
		t.Fatalf("DeleteBridgeDomain: %v", err)
	}
	if len(f.dhcp.calls) != 1 || f.dhcp.calls[0].isAdd || f.dhcp.calls[0].server != "192.168.100.2" {
		t.Fatalf("删交换机应连带撤 proxy: %v", f.dhcp.calls)
	}
	if f.l2.bds[BDID("vs-a")] {
		t.Fatal("BD 应已删除")
	}
}

// ---------- 决策 #337：MAC 学习上限（learn-limit）下发/清配置/幂等 ----------

func TestLearnLimitApplyResetIdempotent(t *testing.T) {
	f := newFakeL2()
	p := NewL2Provider(f)
	vs := model.VirtualSwitch{Name: "vs-l2", Type: "l2", LearnLimit: 8192,
		Ports: []model.VSwitchPort{{Seq: 1, Interface: "ens192"}}}
	bd := BDID("vs-l2")

	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("ApplyBridgeDomain(apply): %v", err)
	}
	if f.learn[bd] != 8192 {
		t.Fatalf("应下发学习上限 8192，实际 %v（calls=%v）", f.learn, f.calls)
	}

	// 声明未变：幂等（不重发 learn 消息；端口重挂会记 attach，故只数 learn 消息）
	learnCalls := func() int {
		n := 0
		for _, c := range f.calls {
			if strings.HasPrefix(c, "learn:") {
				n++
			}
		}
		return n
	}
	n := learnCalls()
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("ApplyBridgeDomain(幂等): %v", err)
	}
	if learnCalls() != n {
		t.Fatalf("声明未变不应重发 learn 消息（实际 %v）", f.calls)
	}

	// 改值：重发新上限
	vs.LearnLimit = 4096
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("ApplyBridgeDomain(改值): %v", err)
	}
	if f.learn[bd] != 4096 {
		t.Fatalf("改值应重发 4096，实际 %v", f.learn)
	}

	// 清配置：恢复 VPP 默认（不设限）
	vs.LearnLimit = 0
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("ApplyBridgeDomain(清配置): %v", err)
	}
	if f.learn[bd] != vppDefaultLearnLimit {
		t.Fatalf("清配置应恢复默认 %d，实际 %d", vppDefaultLearnLimit, f.learn[bd])
	}

	// 已无登记：再清是空操作
	n = learnCalls()
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("ApplyBridgeDomain(再清): %v", err)
	}
	if learnCalls() != n {
		t.Fatalf("无登记时再清不应下发: %v", f.calls)
	}
}
