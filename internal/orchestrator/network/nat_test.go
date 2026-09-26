package network

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// ---------- M3-5（三）：NAT44 收敛（假 NatClient） ----------

type fakeNat struct {
	ifaces  map[string]uint32
	ranges  []string // "add/del first-last@vrf"
	feats   []string // "add/del idx inside|outside"
	static  []string
	enables []string // "enable/disable insideVRF/outsideVRF"
	err     error
}

func newFakeNat() *fakeNat {
	return &fakeNat{ifaces: map[string]uint32{"ens192": 1, "ens224": 2}}
}

func (f *fakeNat) Close() {}

func (f *fakeNat) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	idx, ok := f.ifaces[ifname]
	return idx, ok, nil
}

func (f *fakeNat) NATAddressRange(add bool, first, last string, vrfID uint32) error {
	if f.err != nil {
		return f.err
	}
	op := "del"
	if add {
		op = "add"
	}
	f.ranges = append(f.ranges, fmt.Sprintf("%s:%s-%s@%d", op, first, last, vrfID))
	return nil
}

func (f *fakeNat) NATFeature(swIfIndex uint32, inside, add bool) error {
	if f.err != nil {
		return f.err
	}
	op, dir := "del", "outside"
	if add {
		op = "add"
	}
	if inside {
		dir = "inside"
	}
	f.feats = append(f.feats, op+":"+string(rune('0'+swIfIndex))+":"+dir)
	return nil
}

func (f *fakeNat) NATEnable(enable bool, insideVRF, outsideVRF uint32) error {
	if f.err != nil {
		return f.err
	}
	op := "disable"
	if enable {
		op = "enable"
	}
	f.enables = append(f.enables, fmt.Sprintf("%s:%d/%d", op, insideVRF, outsideVRF))
	return nil
}

func (f *fakeNat) NATInterfaceAddr(add bool, swIfIndex uint32) error {
	if f.err != nil {
		return f.err
	}
	op := "del"
	if add {
		op = "add"
	}
	f.feats = append(f.feats, op+":"+string(rune('0'+swIfIndex))+":ifaddr")
	return nil
}

func (f *fakeNat) NATStatic(add bool, inside, outside string) error {
	if f.err != nil {
		return f.err
	}
	op := "del"
	if add {
		op = "add"
	}
	f.static = append(f.static, op+":"+inside+"->"+outside)
	return nil
}

func (f *fakeNat) NATSessions() ([]NATSession, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []NATSession{{InsideIP: "10.0.0.5", OutsideIP: "203.0.113.1", Packets: 3}}, nil
}

func natFixture() model.NatConfig {
	return model.NatConfig{
		SourcePools: []model.NatSourcePool{{Name: "pool1", AddressRange: "203.0.113.10 to 203.0.113.20"}},
		Rules: []model.NatRule{{Seq: 10, MatchSource: "10.0.0.0/24", VirtualSwitch: "vs-l3",
			Action: model.NatAction{SourcePool: "pool1", Interface: "ens224"}}},
		Static: []model.NatStatic{{InsideIP: "10.0.0.5", OutsideIP: "203.0.113.1"}},
	}
}

func TestNatApplyConvergence(t *testing.T) {
	f := newFakeNat()
	p := NewNatProvider(f)
	p.SetInsideResolver(func(vs string) []uint32 {
		if vs == "vs-l3" {
			return []uint32{1} // ens192 inside
		}
		return nil
	})
	if err := p.ApplyNAT(context.Background(), natFixture()); err != nil {
		t.Fatalf("ApplyNAT: %v", err)
	}
	if len(f.ranges) != 1 || f.ranges[0] != "add:203.0.113.10-203.0.113.20@0" {
		t.Fatalf("地址池: %v", f.ranges)
	}
	if len(f.static) != 1 || f.static[0] != "add:10.0.0.5->203.0.113.1" {
		t.Fatalf("静态映射: %v", f.static)
	}
	joined := strings.Join(f.feats, ",")
	// 规则给了 source-pool：外部地址由池承担，不再附加接口地址（决策 #52）。
	if len(f.feats) != 2 || !strings.Contains(joined, "add:1:inside") ||
		!strings.Contains(joined, "add:2:outside") {
		t.Fatalf("接口特性: %v", f.feats)
	}
	// 插件启用须带 inside 转发域（由 virtual-switch 派生），outside 为默认表（决策 #52）。
	wantEnable := fmt.Sprintf("enable:%d/0", TableID("vs-l3"))
	if len(f.enables) != 1 || f.enables[0] != wantEnable {
		t.Fatalf("插件启用转发域应为 %s: %v", wantEnable, f.enables)
	}

	// 幂等重放：无重复增删，也不重复启用插件
	f.ranges, f.feats, f.static, f.enables = nil, nil, nil, nil
	if err := p.ApplyNAT(context.Background(), natFixture()); err != nil {
		t.Fatalf("重复 ApplyNAT: %v", err)
	}
	if len(f.ranges)+len(f.feats)+len(f.static)+len(f.enables) != 0 {
		t.Fatalf("幂等重放不应有操作: ranges=%v feats=%v static=%v enables=%v", f.ranges, f.feats, f.static, f.enables)
	}

	// 收敛删除：清空配置 → 全部移除
	if err := p.ApplyNAT(context.Background(), model.NatConfig{}); err != nil {
		t.Fatalf("清空 ApplyNAT: %v", err)
	}
	if len(f.ranges) != 1 || !strings.HasPrefix(f.ranges[0], "del:") {
		t.Fatalf("应删除地址池: %v", f.ranges)
	}
	if len(f.static) != 1 || !strings.HasPrefix(f.static[0], "del:") {
		t.Fatalf("应删除静态映射: %v", f.static)
	}
	if len(f.feats) != 2 {
		t.Fatalf("应移除全部接口特性: %v", f.feats)
	}
	if len(f.enables) != 1 || !strings.HasPrefix(f.enables[0], "disable:") {
		t.Fatalf("应关闭 NAT44 插件: %v", f.enables)
	}
}

// 规则只给出接口（无 source-pool）时以出接口地址作外部地址（决策 #52）。
func TestNatInterfaceAddrWhenNoPool(t *testing.T) {
	f := newFakeNat()
	p := NewNatProvider(f)
	p.SetInsideResolver(func(string) []uint32 { return []uint32{1} })
	cfg := model.NatConfig{Rules: []model.NatRule{{Seq: 1, MatchSource: "10.0.0.0/24",
		VirtualSwitch: "vs-l3", Action: model.NatAction{Interface: "ens224"}}}}
	if err := p.ApplyNAT(context.Background(), cfg); err != nil {
		t.Fatalf("ApplyNAT: %v", err)
	}
	joined := strings.Join(f.feats, ",")
	if !strings.Contains(joined, "add:2:ifaddr") {
		t.Fatalf("无池时应使用出接口地址: %v", f.feats)
	}
}

// outside 转发域取自出接口所属 VRF（决策 #52）。
func TestNatOutsideVRF(t *testing.T) {
	f := newFakeNat()
	p := NewNatProvider(f)
	p.SetInsideResolver(func(string) []uint32 { return []uint32{1} })
	p.SetOutsideResolver(func(ifname string) (uint32, bool) {
		if ifname == "ens224" {
			return TableID("wan"), true
		}
		return 0, false
	})
	cfg := model.NatConfig{Rules: []model.NatRule{{Seq: 1, MatchSource: "10.0.0.0/24",
		VirtualSwitch: "vs-l3", Action: model.NatAction{Interface: "ens224"}}}}
	if err := p.ApplyNAT(context.Background(), cfg); err != nil {
		t.Fatalf("ApplyNAT: %v", err)
	}
	want := fmt.Sprintf("enable:%d/%d", TableID("vs-l3"), TableID("wan"))
	if len(f.enables) != 1 || f.enables[0] != want {
		t.Fatalf("outside 转发域应为 wan(%d): %v", TableID("wan"), f.enables)
	}
	// 出接口不属于同一 VRF 时防御性报错
	f2 := newFakeNat()
	p2 := NewNatProvider(f2)
	p2.SetInsideResolver(func(string) []uint32 { return nil })
	p2.SetOutsideResolver(func(ifname string) (uint32, bool) {
		if ifname == "ens224" {
			return 100, true
		}
		return 200, true
	})
	cfg2 := model.NatConfig{Rules: []model.NatRule{
		{Seq: 1, MatchSource: "10.0.0.0/24", VirtualSwitch: "vs-l3", Action: model.NatAction{Interface: "ens224"}},
		{Seq: 2, MatchSource: "10.1.0.0/24", VirtualSwitch: "vs-l3", Action: model.NatAction{Interface: "ens192"}},
	}}
	if err := p2.ApplyNAT(context.Background(), cfg2); err == nil ||
		!strings.Contains(err.Error(), "单一 outside VRF") {
		t.Fatalf("多 outside 转发域应报错: %v", err)
	}
}

// 地址池必须落在 outside 转发域里（vrf_id），且删除方向用**登记时**的那个 VRF——
// 池进了默认表就跟 outside 不在同一张表：包进了 NAT 却分配不出端口，全程无报错（round84 缺陷 A）。
func TestNatPoolUsesOutsideVRF(t *testing.T) {
	natf := newFakeNat()
	p := NewNatProvider(natf)
	p.SetInsideResolver(func(string) []uint32 { return []uint32{1} })
	wanTable := TableID("vs-wan")
	p.SetOutsideResolver(func(ifname string) (uint32, bool) {
		if ifname == "ens224" {
			return wanTable, true
		}
		return 0, false
	})
	cfg := model.NatConfig{
		SourcePools: []model.NatSourcePool{{Name: "pool-a", AddressRange: "192.168.155.62"}},
		Rules: []model.NatRule{{Seq: 10, MatchSource: "192.168.200.0/24", VirtualSwitch: "vs-nat",
			Action: model.NatAction{SourcePool: "pool-a", Interface: "ens224"}}},
	}
	ctx := context.Background()
	if err := p.ApplyNAT(ctx, cfg); err != nil {
		t.Fatalf("ApplyNAT: %v", err)
	}
	wantAdd := fmt.Sprintf("add:192.168.155.62-192.168.155.62@%d", wanTable)
	if len(natf.ranges) != 1 || natf.ranges[0] != wantAdd {
		t.Fatalf("地址池应带 outside 的 VRF %d: %v", wanTable, natf.ranges)
	}

	// 出接口换到另一张表：旧池必须用**旧 VRF** 删（用新 VRF 删不掉，VPP 里会残留）
	otherTable := TableID("vs-other")
	p.SetOutsideResolver(func(ifname string) (uint32, bool) {
		if ifname == "ens224" {
			return otherTable, true
		}
		return 0, false
	})
	natf.ranges = nil
	if err := p.ApplyNAT(ctx, cfg); err != nil {
		t.Fatalf("换转发域 ApplyNAT: %v", err)
	}
	wantOps := []string{
		fmt.Sprintf("del:192.168.155.62-192.168.155.62@%d", wanTable),
		fmt.Sprintf("add:192.168.155.62-192.168.155.62@%d", otherTable),
	}
	if len(natf.ranges) != 2 || natf.ranges[0] != wantOps[0] || natf.ranges[1] != wantOps[1] {
		t.Fatalf("删除须用登记时的 VRF、下发用新 VRF，实际 %v（期望 %v）", natf.ranges, wantOps)
	}
}

// 多条规则的 inside 转发域不一致必须报错（VPP NAT44 单实例，决策 #52）。
func TestNatRejectsMultipleInsideVRF(t *testing.T) {
	f := newFakeNat()
	p := NewNatProvider(f)
	p.SetInsideResolver(func(string) []uint32 { return nil })
	cfg := model.NatConfig{Rules: []model.NatRule{
		{Seq: 1, MatchSource: "10.0.0.0/24", VirtualSwitch: "vs-a", Action: model.NatAction{Interface: "ens224"}},
		{Seq: 2, MatchSource: "10.1.0.0/24", VirtualSwitch: "vs-b", Action: model.NatAction{Interface: "ens224"}},
	}}
	if err := p.ApplyNAT(context.Background(), cfg); err == nil ||
		!strings.Contains(err.Error(), "单一 inside 转发域") {
		t.Fatalf("多 inside 转发域应报错: %v", err)
	}
}

func TestNatErrors(t *testing.T) {
	// 未定义源池
	f := newFakeNat()
	cfg := model.NatConfig{Rules: []model.NatRule{{Seq: 1, Action: model.NatAction{SourcePool: "nope"}}}}
	if err := NewNatProvider(f).ApplyNAT(context.Background(), cfg); err == nil ||
		!strings.Contains(err.Error(), "未定义") {
		t.Fatalf("未定义源池应报错: %v", err)
	}
	// 同一接口内外冲突
	f2 := newFakeNat()
	p2 := NewNatProvider(f2)
	p2.SetInsideResolver(func(string) []uint32 { return []uint32{2} })
	cfg2 := model.NatConfig{Rules: []model.NatRule{{Seq: 1, VirtualSwitch: "vs", Action: model.NatAction{Interface: "ens224"}}}}
	if err := p2.ApplyNAT(context.Background(), cfg2); err == nil || !strings.Contains(err.Error(), "内外口") {
		t.Fatalf("内外口冲突应报错: %v", err)
	}
	// 外口不存在
	cfg3 := model.NatConfig{Rules: []model.NatRule{{Seq: 1, Action: model.NatAction{Interface: "ens999"}}}}
	if err := NewNatProvider(newFakeNat()).ApplyNAT(context.Background(), cfg3); err == nil {
		t.Fatalf("外口缺失应报错")
	}
	// 出接口未指定：必须显式报错（此前静默跳过 → NAT 完全不生效，排查成本极高）
	cfg4 := model.NatConfig{
		SourcePools: []model.NatSourcePool{{Name: "pool1", AddressRange: "192.168.155.220 to 192.168.155.225"}},
		Rules: []model.NatRule{{Seq: 1, MatchSource: "10.0.0.0/24",
			VirtualSwitch: "vs-l3", Action: model.NatAction{SourcePool: "pool1"}}}}
	f4 := newFakeNat()
	if err := NewNatProvider(f4).ApplyNAT(context.Background(), cfg4); err == nil ||
		!strings.Contains(err.Error(), "未指定出接口") {
		t.Fatalf("source-pool 未给出接口时应报错: %v", err)
	}
	// virtual-switch 未指定：inside 无从解析
	cfg5 := model.NatConfig{Rules: []model.NatRule{{Seq: 1, Action: model.NatAction{Interface: "ens224"}}}}
	if err := NewNatProvider(newFakeNat()).ApplyNAT(context.Background(), cfg5); err == nil ||
		!strings.Contains(err.Error(), "未指定 virtual-switch") {
		t.Fatalf("未指定 virtual-switch 时应报错: %v", err)
	}
	// 客户端错误
	fe := newFakeNat()
	fe.err = errors.New("boom")
	if err := NewNatProvider(fe).ApplyNAT(context.Background(), natFixture()); err == nil {
		t.Fatalf("客户端错误应上抛")
	}
	// 地址范围格式
	if _, _, err := parseAddressRange("bad range here"); err == nil {
		t.Fatalf("非法地址范围应报错")
	}
	if a, b, err := parseAddressRange("10.0.0.1"); err != nil || a != b {
		t.Fatalf("单地址应起止相同: %v %v %v", a, b, err)
	}
}

func TestNatSessions(t *testing.T) {
	p := NewNatProvider(newFakeNat())
	if err := p.ApplyNAT(context.Background(), natFixture()); err != nil {
		t.Fatalf("ApplyNAT: %v", err)
	}
	rows, err := p.Sessions(context.Background())
	if err != nil || len(rows) != 1 || rows[0].OutsideIP != "203.0.113.1" {
		t.Fatalf("Sessions: %v %+v", err, rows)
	}
}

// 插件未启用时会话查询直接返回空（不应答 VPP，避免阻塞）。
func TestNatSessionsDisabled(t *testing.T) {
	rows, err := NewNatProvider(newFakeNat()).Sessions(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("未启用应返回空: %v %+v", err, rows)
	}
}

// round84 目标终态回归：VNF 侧声明 virtual-switch（L3 交换机）的 vNIC 置入该 VRF 后，
// NAT inside 必须包含该 vNIC——否则其流量拿不到 nat44-ei-in2out 特性，
// 而配置、show nat 全都正常（实测 inside 只有物理口，guest 100% 不通）。
func TestNatInsideIncludesVnfNic(t *testing.T) {
	l3f := newFakeL3()
	vh := orchestrator.VnfIfaceName("vnf-a", "eth0")
	l3f.ifaces[vh] = 5 // vhost-user 口已由 vhost 编排建出（VPP 侧同名可见）
	natf := newFakeNat()

	net := NewL2Network(orchestrator.NewNoopNetwork(), NewL2Provider(newFakeL2()))
	net.SetL3(NewL3Provider(l3f))
	net.SetVhostUser(vhostProvider(newFakeVhost()))
	net.SetNAT(NewNatProvider(natf))

	ctx := context.Background()
	if err := net.ApplyVnfInterface(ctx, orchestrator.VnfPort{VM: "vnf-a", Interface: "eth0",
		Type: "vhost-user", VirtualSwitch: "vs-nat", VRF: "vs-nat",
		Socket: "/run/nfvis/vhost/vnf-a-eth0.sock"}); err != nil {
		t.Fatalf("ApplyVnfInterface: %v", err)
	}
	cfg := model.NatConfig{Rules: []model.NatRule{{Seq: 10, MatchSource: "192.168.200.0/24",
		VirtualSwitch: "vs-nat", Action: model.NatAction{Interface: "ens224"}}}}
	if err := net.ApplyNAT(ctx, cfg); err != nil {
		t.Fatalf("ApplyNAT: %v", err)
	}
	joined := strings.Join(natf.feats, ",")
	if !strings.Contains(joined, "add:5:inside") || !strings.Contains(joined, "add:2:outside") {
		t.Fatalf("vNIC 必须作为 NAT inside 下发: %v", natf.feats)
	}

	// vNIC 删除后摘除登记：不得让 inside 指向已消失的接口
	if err := net.DeleteVnfInterface(ctx, "vnf-a", "eth0"); err != nil {
		t.Fatalf("DeleteVnfInterface: %v", err)
	}
	if got := net.l3.AttachedIfaces("vs-nat"); len(got) != 0 {
		t.Fatalf("删除 vNIC 后不得残留 inside 登记: %v", got)
	}
	// NAT 侧的同一索引登记也随接口一并摘除：否则下一次 ApplyNAT 会去删一个已消失的接口
	// （VPP 报 -6 No such entry），而一次无害的重复删除会把整批 apply 打回滚。
	natf.feats = nil
	if err := net.ApplyNAT(ctx, cfg); err != nil {
		t.Fatalf("vNIC 删除后 ApplyNAT: %v", err)
	}
	for _, op := range natf.feats {
		if strings.Contains(op, ":5:") {
			t.Fatalf("不得再对该 vNIC 的旧索引下发任何 NAT 操作: %v", natf.feats)
		}
	}
}

// round84 收尾回归：派生查询（inside/outside 解析）在登记失效后可能返回空集，
// 此时**不得**把「解析不出来」当成「配置里没有 inside」：
//   - 插件不能被关掉（配置声明了 NAT）；
//   - VPP 侧既有 inside 特性不能被删（删掉就是真机实测的「NAT 被整体关掉」形态）；
//   - 等解析恢复健康，登记里保留的 inside 特性照常比对（不重复下发、也不遗留）。
//
// 反向也要守住：操作者真的清空了 NAT 配置时，插件必须关闭（不能因为怕关错就永不关）。
func TestApplyNATKeepsInsideWhenResolutionLost(t *testing.T) {
	f := newFakeNat()
	p := NewNatProvider(f)
	ctx := context.Background()
	inside := []uint32{1, 5}
	p.SetInsideResolver(func(string) []uint32 { return inside })
	p.SetOutsideResolver(func(string) (uint32, bool) { return TableID("vs-wan"), true })
	cfg := model.NatConfig{
		SourcePools: []model.NatSourcePool{{Name: "pool-a", AddressRange: "192.168.155.62"}},
		Rules: []model.NatRule{{Seq: 10, MatchSource: "192.168.200.0/24", VirtualSwitch: "vs-nat",
			Action: model.NatAction{SourcePool: "pool-a", Interface: "ens224"}}},
	}

	// 前置：正常运行态，inside 特性已下发
	if err := p.ApplyNAT(ctx, cfg); err != nil {
		t.Fatalf("前置 ApplyNAT: %v", err)
	}
	if joined := strings.Join(f.feats, ","); !strings.Contains(joined, "add:1:inside") ||
		!strings.Contains(joined, "add:5:inside") {
		t.Fatalf("前置应下发 inside 特性: %v", f.feats)
	}

	// 登记失效/尚未重建：解析突然答不出 inside（配置一个字没改）
	f.feats, f.enables, f.ranges = nil, nil, nil
	inside = nil
	if err := p.ApplyNAT(ctx, cfg); err != nil {
		t.Fatalf("解析为空时 ApplyNAT 不应失败: %v", err)
	}
	for _, op := range f.feats {
		if strings.HasPrefix(op, "del:") {
			t.Fatalf("空解析不得删掉既有 inside 特性: %v", f.feats)
		}
	}
	for _, op := range f.enables {
		if strings.HasPrefix(op, "disable") {
			t.Fatalf("有 NAT 配置时不得关闭插件: %v", f.enables)
		}
	}
	if got, err := p.Sessions(ctx); err != nil || len(got) == 0 {
		t.Fatalf("插件应仍处于启用态: %v %v", got, err)
	}

	// 解析恢复健康：登记里保留的 inside 特性被正确比对——既不重复下发
	inside = []uint32{1, 5}
	f.feats, f.enables, f.ranges = nil, nil, nil
	if err := p.ApplyNAT(ctx, cfg); err != nil {
		t.Fatalf("解析恢复后 ApplyNAT: %v", err)
	}
	if len(f.feats) != 0 || len(f.enables) != 0 || len(f.ranges) != 0 {
		t.Fatalf("解析恢复后应认定全部在位、无任何下发: feats=%v enables=%v ranges=%v",
			f.feats, f.enables, f.ranges)
	}

	// 反向：配置真的清空（操作者删掉全部规则/池/静态映射）→ 插件必须关闭
	f.enables = nil
	if err := p.ApplyNAT(ctx, model.NatConfig{}); err != nil {
		t.Fatalf("清空配置的 ApplyNAT: %v", err)
	}
	if len(f.enables) != 1 || !strings.HasPrefix(f.enables[0], "disable") {
		t.Fatalf("配置清空后必须关闭插件: %v", f.enables)
	}
}
