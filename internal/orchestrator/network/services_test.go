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

	// 故障注入钩子（决策 #363 单测）：按方法/按调用粒度置错，nil 即不注入。
	// 调用一律「先记录、后判错」，故失败尝试也在 pins/pouts 里可见（可断言确实下发了）。
	pinErr        func(idx uint32, name string, apply bool) error
	poutErr       func(idx uint32, name string, apply bool) error
	policerAddErr func(name string, add bool) error
	spanOffErr    func(from, to uint32) error
	// 与 pins/pouts 一一对应的接口索引，用于区分绑同一策略名的不同接口（map 遍历顺序不定）。
	pinIdx  []uint32
	poutIdx []uint32
	// PolicerAddDel 调用记录（name:add|del），用于断言重试真的重删了 policer。
	addDel []string
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
	if f.spanOffErr != nil {
		return f.spanOffErr(from, to)
	}
	return nil
}

func (f *fakeSvc) PolicerAddDel(name string, cirKbps uint32, cb uint64, add bool) (uint32, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.addDel = append(f.addDel, name+":"+map[bool]string{true: "add", false: "del"}[add])
	if f.policerAddErr != nil {
		if err := f.policerAddErr(name, add); err != nil {
			return 0, err
		}
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
	f.pinIdx = append(f.pinIdx, swIfIndex)
	if f.pinErr != nil {
		return f.pinErr(swIfIndex, name, apply)
	}
	return nil
}

func (f *fakeSvc) PolicerOutput(swIfIndex uint32, name string, apply bool) error {
	if f.err != nil {
		return f.err
	}
	f.pouts = append(f.pouts, name+":"+map[bool]string{true: "on", false: "off"}[apply])
	f.poutIdx = append(f.poutIdx, swIfIndex)
	if f.poutErr != nil {
		return f.poutErr(swIfIndex, name, apply)
	}
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

// ---------- 决策 #363：QoS/SPAN 绑定的逐步骤登记（登记=最后成功下发态） ----------

// svcRegs 读某接口的入/出向绑定登记快照（与 Provider 同包，锁内读）。
func svcRegs(p *ServicesProvider, ifname string) (in, out string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bound[ifname], p.boundEgress[ifname]
}

// svcPolicer 读 policer 是否仍在登记表里。
func svcPolicer(p *ServicesProvider, name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.policer[name]
}

// svcSpanTracked 读 SPAN 会话是否仍在登记表里。
func svcSpanTracked(p *ServicesProvider, name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.spans[name]
	return ok
}

// countSeq 数 list 中等于 want 的元素个数。
func countSeq(list []string, want string) int {
	n := 0
	for _, s := range list {
		if s == want {
			n++
		}
	}
	return n
}

// TestApplyInterfaceBindFailKeepsLastState 绑新失败：旧策略已解绑成功 ⇒ 该向登记为空
// （最后成功态）；重试必须**真的再次下发**新策略，而不是因登记已被改成就绪值而静默跳过。
func TestApplyInterfaceBindFailKeepsLastState(t *testing.T) {
	f := newFakeSvc()
	p := NewServicesProvider(f)
	ctx := context.Background()
	if err := p.ApplyInterface(ctx, model.InterfaceConfig{Name: "ens192", IngressPolicy: "old"}); err != nil {
		t.Fatalf("初次绑定: %v", err)
	}
	f.pinErr = func(_ uint32, name string, apply bool) error {
		if name == "new" && apply {
			return errors.New("vpp down")
		}
		return nil
	}
	if err := p.ApplyInterface(ctx, model.InterfaceConfig{Name: "ens192", IngressPolicy: "new"}); err == nil {
		t.Fatal("绑新失败应上抛")
	}
	if in, _ := svcRegs(p, "ens192"); in != "" {
		t.Fatalf("旧策略已解绑成功，入向登记应为空（最后成功态），实际 %q", in)
	}
	if len(f.pins) != 3 || f.pins[0] != "old:on" || f.pins[1] != "old:off" || f.pins[2] != "new:on" {
		t.Fatalf("应依次：绑 old、解绑 old、下发 new（失败尝试也留痕）: %v", f.pins)
	}
	// 重试：登记为空 ⇒ 必须再次下发 new（旧实现登记已是 new，会静默跳过）
	f.pinErr = nil
	if err := p.ApplyInterface(ctx, model.InterfaceConfig{Name: "ens192", IngressPolicy: "new"}); err != nil {
		t.Fatalf("重试绑定: %v", err)
	}
	if in, _ := svcRegs(p, "ens192"); in != "new" {
		t.Fatalf("重试成功后登记应为 new，实际 %q", in)
	}
	if n := countSeq(f.pins, "new:on"); n != 2 {
		t.Fatalf("失败尝试与重试成功后各下发一次 new:on（共 2 次），实际 %d: %v", n, f.pins)
	}
}

// TestApplyInterfaceUnbindFailKeepsOldRegistration 解绑旧失败：该向登记保持旧值，
// 并且不再往下绑新（数据面仍是旧策略，登记如实反映）。
func TestApplyInterfaceUnbindFailKeepsOldRegistration(t *testing.T) {
	f := newFakeSvc()
	p := NewServicesProvider(f)
	ctx := context.Background()
	if err := p.ApplyInterface(ctx, model.InterfaceConfig{Name: "ens192", IngressPolicy: "old"}); err != nil {
		t.Fatalf("初次绑定: %v", err)
	}
	f.pinErr = func(_ uint32, _ string, apply bool) error {
		if !apply {
			return errors.New("vpp down")
		}
		return nil
	}
	if err := p.ApplyInterface(ctx, model.InterfaceConfig{Name: "ens192", IngressPolicy: "new"}); err == nil {
		t.Fatal("解绑旧失败应上抛")
	}
	if in, _ := svcRegs(p, "ens192"); in != "old" {
		t.Fatalf("解绑失败，登记应保持旧值 old，实际 %q", in)
	}
	if n := countSeq(f.pins, "new:on"); n != 0 {
		t.Fatalf("解绑未成功不应绑新: %v", f.pins)
	}
}

// TestApplyInterfaceDirectionsIndependent 两向互不影响：入向已成功推进为 pin2，
// 出向解绑失败 ⇒ 出向登记停在旧值 pout（#331 的独立性在失败路径同样成立）。
func TestApplyInterfaceDirectionsIndependent(t *testing.T) {
	f := newFakeSvc()
	p := NewServicesProvider(f)
	ctx := context.Background()
	if err := p.ApplyInterface(ctx, model.InterfaceConfig{
		Name: "ens192", IngressPolicy: "pin", EgressPolicy: "pout"}); err != nil {
		t.Fatalf("初次绑定: %v", err)
	}
	f.poutErr = func(_ uint32, _ string, apply bool) error {
		if !apply {
			return errors.New("vpp down")
		}
		return nil
	}
	if err := p.ApplyInterface(ctx, model.InterfaceConfig{
		Name: "ens192", IngressPolicy: "pin2", EgressPolicy: "pout2"}); err == nil {
		t.Fatal("出向解绑失败应上抛")
	}
	if in, out := svcRegs(p, "ens192"); in != "pin2" || out != "pout" {
		t.Fatalf("入向应推进为 pin2、出向应保持 pout，实际 %q/%q", in, out)
	}
}

// TestApplyInterfaceCompensationRebindsOld 补偿路径：一次 apply 中入向已换成新策略
// （登记已推进）、出向失败返回 ⇒ 补偿 ApplyInterface(旧值) 必须按登记差把入向解绑新、
// 绑回旧（VPP 调用确实发生），否则登记与数据面会停在「半新」。
func TestApplyInterfaceCompensationRebindsOld(t *testing.T) {
	f := newFakeSvc()
	p := NewServicesProvider(f)
	ctx := context.Background()
	if err := p.ApplyInterface(ctx, model.InterfaceConfig{Name: "ens192", IngressPolicy: "pol1"}); err != nil {
		t.Fatalf("初次绑定: %v", err)
	}
	f.poutErr = func(_ uint32, name string, apply bool) error {
		if name == "bad" && apply {
			return errors.New("vpp down")
		}
		return nil
	}
	if err := p.ApplyInterface(ctx, model.InterfaceConfig{
		Name: "ens192", IngressPolicy: "pol2", EgressPolicy: "bad"}); err == nil {
		t.Fatal("出向绑新失败应上抛")
	}
	if in, out := svcRegs(p, "ens192"); in != "pol2" || out != "" {
		t.Fatalf("失败后登记应为入向 pol2、出向空，实际 %q/%q", in, out)
	}
	// 补偿：把接口拉回旧声明（只有入向 pol1），登记差驱动下发
	if err := p.ApplyInterface(ctx, model.InterfaceConfig{Name: "ens192", IngressPolicy: "pol1"}); err != nil {
		t.Fatalf("补偿 ApplyInterface(旧值): %v", err)
	}
	if in, _ := svcRegs(p, "ens192"); in != "pol1" {
		t.Fatalf("补偿后登记应回 pol1，实际 %q", in)
	}
	if n := countSeq(f.pins, "pol2:off"); n != 1 {
		t.Fatalf("补偿应先解绑 pol2: %v", f.pins)
	}
	if n := countSeq(f.pins, "pol1:on"); n != 2 { // 初次绑定 + 补偿绑回
		t.Fatalf("补偿应把 pol1 绑回（共 2 次 pol1:on），实际: %v", f.pins)
	}
}

// TestDeleteQosUnbindFailKeepsPerIfaceRegistry 两个口绑同一策略：第二个口解绑失败 ⇒
// 已成功解绑的口登记摘除、失败口保留；policer 登记保留；重试只重做失败口。
// 注意 bound 是 map、遍历顺序不定，故按调用记录（pinIdx）动态确定哪个口是「第二个」。
func TestDeleteQosUnbindFailKeepsPerIfaceRegistry(t *testing.T) {
	f := newFakeSvc()
	p := NewServicesProvider(f)
	ctx := context.Background()
	if err := p.ApplyQos(ctx, model.QosPolicy{Name: "pol1", Cir: 1000000}); err != nil {
		t.Fatalf("ApplyQos: %v", err)
	}
	for _, name := range []string{"ens192", "ens224"} {
		if err := p.ApplyInterface(ctx, model.InterfaceConfig{Name: name, IngressPolicy: "pol1"}); err != nil {
			t.Fatalf("绑定 %s: %v", name, err)
		}
	}
	f.pins, f.pinIdx = nil, nil // 只看删除阶段的解绑调用
	offCalls := 0
	f.pinErr = func(_ uint32, _ string, apply bool) error {
		if !apply {
			offCalls++
			if offCalls == 2 { // 让第二个被遍历到的口解绑失败
				return errors.New("vpp down")
			}
		}
		return nil
	}
	if err := p.DeleteQos(ctx, "pol1"); err == nil {
		t.Fatal("解绑失败应上抛")
	}
	if len(f.pins) != 2 || f.pins[0] != "pol1:off" || f.pins[1] != "pol1:off" {
		t.Fatalf("两个口应各尝试解绑一次: %v", f.pins)
	}
	failedIdx := f.pinIdx[1] // 第二个尝试的即失败口
	var failedName string
	for name, idx := range f.ifaces {
		if idx == failedIdx {
			failedName = name
		}
	}
	if failedName == "" {
		t.Fatalf("找不到解绑失败的接口: idx=%d", failedIdx)
	}
	for _, name := range []string{"ens192", "ens224"} {
		in, _ := svcRegs(p, name)
		if name == failedName {
			if in != "pol1" {
				t.Fatalf("解绑失败的口 %s 登记应保留 pol1，实际 %q", name, in)
			}
		} else if in != "" {
			t.Fatalf("已解绑成功的口 %s 登记应摘除，实际 %q", name, in)
		}
	}
	if !svcPolicer(p, "pol1") || len(f.policers) != 1 {
		t.Fatalf("解绑未全部完成，policer 登记与数据面实例都应保留: %v", f.policers)
	}
	// 重试：只重做失败口，全部成功后才删 policer
	f.pinErr = nil
	if err := p.DeleteQos(ctx, "pol1"); err != nil {
		t.Fatalf("重试 DeleteQos: %v", err)
	}
	if len(f.pins) != 3 || f.pinIdx[2] != failedIdx {
		t.Fatalf("重试应只重做失败口（idx %d），实际 %v / %v", failedIdx, f.pins, f.pinIdx)
	}
	if in, _ := svcRegs(p, failedName); in != "" {
		t.Fatalf("重试成功后登记应摘除，实际 %q", in)
	}
	if svcPolicer(p, "pol1") {
		t.Fatal("全部解绑成功后应摘 policer 登记")
	}
	if _, ok := f.policers["pol1"]; ok {
		t.Fatal("policer 应从数据面删除")
	}
	if n := countSeq(f.addDel, "pol1:del"); n != 1 {
		t.Fatalf("解绑未全部完成前不应删 policer（del 应恰好 1 次）: %v", f.addDel)
	}
}

// TestDeleteQosPolicerDeleteFailKeepsRegistry 解绑都成功、PolicerAddDel(del) 失败 ⇒
// 接口登记已摘、policer 登记保留；重试只重删 policer，不再发解绑。
func TestDeleteQosPolicerDeleteFailKeepsRegistry(t *testing.T) {
	f := newFakeSvc()
	p := NewServicesProvider(f)
	ctx := context.Background()
	if err := p.ApplyQos(ctx, model.QosPolicy{Name: "pol1", Cir: 1000000}); err != nil {
		t.Fatalf("ApplyQos: %v", err)
	}
	if err := p.ApplyInterface(ctx, model.InterfaceConfig{Name: "ens192", IngressPolicy: "pol1"}); err != nil {
		t.Fatalf("绑定: %v", err)
	}
	f.policerAddErr = func(_ string, add bool) error {
		if !add {
			return errors.New("vpp down")
		}
		return nil
	}
	if err := p.DeleteQos(ctx, "pol1"); err == nil {
		t.Fatal("删 policer 失败应上抛")
	}
	if in, _ := svcRegs(p, "ens192"); in != "" {
		t.Fatalf("解绑已成功，接口登记应摘除，实际 %q", in)
	}
	if !svcPolicer(p, "pol1") {
		t.Fatal("policer 删除失败，登记应保留（重试可再删）")
	}
	if _, ok := f.policers["pol1"]; !ok {
		t.Fatal("PolicerAddDel 失败时数据面 policer 不应被算作已删")
	}
	pinN := len(f.pins)
	f.policerAddErr = nil
	if err := p.DeleteQos(ctx, "pol1"); err != nil {
		t.Fatalf("重试 DeleteQos: %v", err)
	}
	if len(f.pins) != pinN {
		t.Fatalf("接口登记已摘，重试不应再发解绑: %v", f.pins)
	}
	if svcPolicer(p, "pol1") {
		t.Fatal("重试成功后 policer 登记应摘除")
	}
	if n := countSeq(f.addDel, "pol1:del"); n != 2 {
		t.Fatalf("重试应重删 policer（共 2 次 del 调用）: %v", f.addDel)
	}
}

// TestDeleteSpanFailKeepsRegistry SpanDisable 失败 ⇒ 登记保留、重试再关；成功才摘登记。
func TestDeleteSpanFailKeepsRegistry(t *testing.T) {
	f := newFakeSvc()
	p := NewServicesProvider(f)
	ctx := context.Background()
	pm := model.PortMirroring{Name: "pm1",
		Source: model.PMSource{Interface: "ens192", Direction: "ingress"}, Analyzer: "ens256"}
	if err := p.ApplySpan(ctx, pm); err != nil {
		t.Fatalf("ApplySpan: %v", err)
	}
	f.spanOffErr = func(_, _ uint32) error { return errors.New("vpp down") }
	if err := p.DeleteSpan(ctx, "pm1"); err == nil {
		t.Fatal("SpanDisable 失败应上抛")
	}
	if !svcSpanTracked(p, "pm1") {
		t.Fatal("关闭失败，登记应保留（重试可再关）")
	}
	if len(f.spanOff) != 1 {
		t.Fatalf("应已尝试关闭一次: %v", f.spanOff)
	}
	f.spanOffErr = nil
	if err := p.DeleteSpan(ctx, "pm1"); err != nil {
		t.Fatalf("重试 DeleteSpan: %v", err)
	}
	if svcSpanTracked(p, "pm1") {
		t.Fatal("重试成功后登记应摘除")
	}
	if len(f.spanOff) != 2 || f.spanOff[1] != 1 {
		t.Fatalf("重试应再次下发关闭（源口 1）: %v", f.spanOff)
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
	calls   []proxyCall
	err     error
	dump    []ProxyEntry
	dumpErr error
}

func (f *fakeDhcp) ProxySet(rxVrfID, serverVrfID uint32, isAdd bool, server, src string) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, proxyCall{rx: rxVrfID, srvVrf: serverVrfID, isAdd: isAdd, server: server, src: src})
	return nil
}

func (f *fakeDhcp) ProxyDump() ([]ProxyEntry, error) {
	if f.dumpErr != nil {
		return nil, f.dumpErr
	}
	return f.dump, nil
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
	tableA := TableID(GatewayVRFName("vs-a"))

	// 改 server（同域）：必须先撤旧（IsAdd=false 旧 server）后加新（决策 #380/R140-1；
	// 旧实现只 add ⇒ VPP 里两条残留、删除只撤最新一条）。
	vs.DhcpRelayServer = "10.0.0.99"
	if err := p.SyncRelay(context.Background(), vs); err != nil {
		t.Fatalf("改 server: %v", err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("改 server 应先撤旧后加新，实际 %v", f.calls)
	}
	if f.calls[1].isAdd || f.calls[1].rx != tableA || f.calls[1].server != "192.168.100.2" || f.calls[1].src != "192.168.100.1" {
		t.Fatalf("改 server 第 1 步应按旧值撤旧: %+v", f.calls[1])
	}
	if !f.calls[2].isAdd || f.calls[2].rx != tableA || f.calls[2].server != "10.0.0.99" || f.calls[2].src != "192.168.100.1" {
		t.Fatalf("改 server 第 2 步应下发新值: %+v", f.calls[2])
	}
	// 声明未变：幂等重跑不产生任何调用。
	if err := p.SyncRelay(context.Background(), vs); err != nil {
		t.Fatalf("改 server 后幂等重跑: %v", err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("声明未变不应重复下发/撤销: %v", f.calls)
	}

	// 网关换域（gateway.vrf）：先撤旧域 proxy，再下发新域
	vs.Gateway = &model.VSGateway{Vrf: "vs-mgmt", Addresses: []string{"10.10.0.1/24"}}
	if err := p.SyncRelay(context.Background(), vs); err != nil {
		t.Fatalf("换网关域: %v", err)
	}
	if len(f.calls) != 5 {
		t.Fatalf("换域应先撤旧再下发新，实际 %v", f.calls)
	}
	newID := TableID("vs-mgmt")
	if f.calls[3].isAdd || f.calls[3].rx != tableA || f.calls[3].server != "10.0.0.99" {
		t.Fatalf("换域第 1 步应撤旧域: %+v", f.calls[3])
	}
	if !f.calls[4].isAdd || f.calls[4].rx != newID || f.calls[4].src != "10.10.0.1" || f.calls[4].server != "10.0.0.99" {
		t.Fatalf("换域第 2 步应在新域下发: %+v", f.calls[4])
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

// TestDhcpRelayReconcileProxy 决策 #380 + #394②：对账 VPP 实际 proxy 与配置声明——**双向**：
// 清未声明/陈旧条目，并**补**声明了却缺失的条目。
func TestDhcpRelayReconcileProxy(t *testing.T) {
	tableA := TableID(GatewayVRFName("vs-a"))
	tableB := TableID(GatewayVRFName("vs-b"))
	vsA := model.VirtualSwitch{Name: "vs-a", Type: "l2",
		Gateway:         &model.VSGateway{Addresses: []string{"192.168.100.1/24"}},
		DhcpRelayServer: "192.168.100.2"}
	vsB := model.VirtualSwitch{Name: "vs-b", Type: "l2",
		Gateway:         &model.VSGateway{Addresses: []string{"192.168.200.1/24"}},
		DhcpRelayServer: "192.168.200.20"}
	// vs-c 声明了 relay 但网关缺失（relayTargetOf 报错）⇒ 无法判定其表，应跳过、不当成「未声明」误删。
	vsC := model.VirtualSwitch{Name: "vs-c", Type: "l2", DhcpRelayServer: "10.0.0.1"}

	f := &fakeDhcp{dump: []ProxyEntry{
		// ① 未声明表（残留）：应清除
		{RxVrfID: 99999, Src: "10.9.9.1", Servers: []ProxyServer{{VrfID: 99999, Server: "10.9.9.2"}}},
		// ② 声明表（vs-a）上的陈旧 server：与声明不符，应清除；且声明的 server 缺失，应补发
		{RxVrfID: tableA, Src: "192.168.100.1", Servers: []ProxyServer{{VrfID: tableA, Server: "10.0.0.99"}}},
		// ③ 声明表（vs-b）上与声明一致的 server：不动
		{RxVrfID: tableB, Src: "192.168.200.1", Servers: []ProxyServer{{VrfID: tableB, Server: "192.168.200.20"}}},
	}}
	p := NewDhcpProvider(f)
	if err := p.ReconcileProxy([]model.VirtualSwitch{vsA, vsB, vsC}); err != nil {
		t.Fatalf("对账应成功: %v", err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("应补①声明缺失 1 条 + 清②/③两条陈旧，实际 %v", f.calls)
	}
	adds := map[string]proxyCall{}
	dels := map[string]proxyCall{}
	for _, c := range f.calls {
		if c.isAdd {
			adds[c.server] = c
		} else {
			dels[c.server] = c
		}
	}
	// 补：vs-a 声明的 server 缺失 ⇒ 按声明字段下发（rx=server=表 id、src 取网关 v4）。
	if c, ok := adds["192.168.100.2"]; !ok || c.rx != tableA || c.srvVrf != tableA || c.src != "192.168.100.1" {
		t.Fatalf("声明缺失的 proxy 应被补发: %+v", f.calls)
	}
	if len(adds) != 1 {
		t.Fatalf("应只补发 1 条（vs-b 已一致、vs-c 不可判定）: %+v", f.calls)
	}
	// 清：未声明表条目 + 声明表上的陈旧 server。
	if c, ok := dels["10.9.9.2"]; !ok || c.rx != 99999 || c.srvVrf != 99999 || c.src != "10.9.9.1" {
		t.Fatalf("未声明表条目应被清除: %+v", f.calls)
	}
	if c, ok := dels["10.0.0.99"]; !ok || c.rx != tableA || c.srvVrf != tableA || c.src != "192.168.100.1" {
		t.Fatalf("声明表的陈旧 server 应被清除: %+v", f.calls)
	}
	if _, ok := dels["192.168.200.20"]; ok {
		t.Fatalf("与声明一致的 server 不应被清除: %+v", f.calls)
	}
}

// TestDhcpRelayReconcileProxyBoundaries 决策 #380 边界：dump 失败跳过不误撤；清除失败如实报错。
func TestDhcpRelayReconcileProxyBoundaries(t *testing.T) {
	// dump 失败 ⇒ 返回错误且零删除
	f := &fakeDhcp{dumpErr: errors.New("dump boom")}
	p := NewDhcpProvider(f)
	if err := p.ReconcileProxy(nil); err == nil {
		t.Fatal("dump 失败应返回错误")
	}
	if len(f.calls) != 0 {
		t.Fatalf("dump 失败时不得删除: %v", f.calls)
	}

	// ProxySet(false) 失败 ⇒ 如实返回错误
	f2 := &fakeDhcp{err: errors.New("proxy boom"), dump: []ProxyEntry{
		{RxVrfID: 99999, Src: "10.9.9.1", Servers: []ProxyServer{{VrfID: 99999, Server: "10.9.9.2"}}},
	}}
	p2 := NewDhcpProvider(f2)
	if err := p2.ReconcileProxy(nil); err == nil {
		t.Fatal("清除失败应如实返回错误")
	}
}

// TestL2NetworkReconcileProxy 决策 #380/#394②：L2Network 从 cfg 取「声明了 relay」的集合转发给
// provider——双向对账（补声明缺失 + 清未声明）。
func TestL2NetworkReconcileProxy(t *testing.T) {
	f := &fakeDhcp{dump: []ProxyEntry{
		{RxVrfID: 99999, Src: "10.9.9.1", Servers: []ProxyServer{{VrfID: 99999, Server: "10.9.9.2"}}},
	}}
	n := NewL2Network(nil, nil)
	n.SetDhcp(NewDhcpProvider(f))
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{{
		Name: "vs-a", Type: "l2",
		Gateway:         &model.VSGateway{Addresses: []string{"192.168.100.1/24"}},
		DhcpRelayServer: "192.168.100.2",
	}}}
	if errs := n.ReconcileProxy(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("对账应成功: %v", errs)
	}
	// 声明了 relay 但数据面没有 ⇒ 补发；数据面上未声明的表 ⇒ 清除。
	tableA := TableID(GatewayVRFName("vs-a"))
	var added, removed bool
	for _, c := range f.calls {
		switch {
		case c.isAdd && c.rx == tableA && c.server == "192.168.100.2":
			added = true
		case !c.isAdd && c.rx == 99999:
			removed = true
		}
	}
	if !added || !removed {
		t.Fatalf("应补声明缺失并清未声明表条目，实际 %v", f.calls)
	}

	// 未注入 provider ⇒ 空操作（不 panic）
	nn := NewL2Network(nil, nil)
	if errs := nn.ReconcileProxy(context.Background(), cfg); errs != nil {
		t.Fatalf("未注入 provider 应返回 nil: %v", errs)
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

// 决策 #390③：接口元素的接口级 teardown——解绑该口入/出向 QoS policer，只动绑定、
// 不碰 MTU/状态（接口即将随数据面重启消失）。无绑定 = 空操作（幂等）。
func TestServicesTeardownInterface(t *testing.T) {
	f := newFakeSvc()
	p := NewServicesProvider(f)
	ctx := context.Background()
	if err := p.ApplyQos(ctx, model.QosPolicy{Name: "pin", Cir: 1000000}); err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyQos(ctx, model.QosPolicy{Name: "pout", Cir: 2000000}); err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyInterface(ctx, model.InterfaceConfig{
		Name: "ens192", MTU: 9000, IngressPolicy: "pin", EgressPolicy: "pout"}); err != nil {
		t.Fatal(err)
	}
	if in, out := svcRegs(p, "ens192"); in != "pin" || out != "pout" {
		t.Fatalf("前置：应已登记两向绑定: in=%q out=%q", in, out)
	}
	f.pins, f.pouts = nil, nil
	f.mtu = map[uint32]uint32{}

	if err := p.TeardownInterface(ctx, "ens192"); err != nil {
		t.Fatalf("TeardownInterface: %v", err)
	}
	if len(f.pins) != 1 || f.pins[0] != "pin:off" {
		t.Fatalf("入向应解绑 pin: %v", f.pins)
	}
	if len(f.pouts) != 1 || f.pouts[0] != "pout:off" {
		t.Fatalf("出向应解绑 pout: %v", f.pouts)
	}
	if in, out := svcRegs(p, "ens192"); in != "" || out != "" {
		t.Fatalf("解绑成功后登记应清空: in=%q out=%q", in, out)
	}
	// 只动绑定：不得重设 MTU / 状态。
	if _, ok := f.mtu[1]; ok {
		t.Fatalf("teardown 不应设置 MTU: %v", f.mtu)
	}

	// 幂等：无绑定登记时零调用。
	f.pins, f.pouts = nil, nil
	if err := p.TeardownInterface(ctx, "ens192"); err != nil {
		t.Fatal(err)
	}
	if len(f.pins) != 0 || len(f.pouts) != 0 {
		t.Fatalf("无绑定应零调用: pins=%v pouts=%v", f.pins, f.pouts)
	}
}
