package network

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// ---------- M3-5（三）：NAT44 收敛（假 NatClient） ----------

type fakeNat struct {
	ifaces map[string]uint32
	ranges []string // "add/del first-last"
	feats  []string // "add/del idx inside|outside"
	static []string
	err    error
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

func (f *fakeNat) NATAddressRange(add bool, first, last string) error {
	if f.err != nil {
		return f.err
	}
	op := "del"
	if add {
		op = "add"
	}
	f.ranges = append(f.ranges, op+":"+first+"-"+last)
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

func (f *fakeNat) NATEnable(enable bool) error {
	if f.err != nil {
		return f.err
	}
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
	if len(f.ranges) != 1 || f.ranges[0] != "add:203.0.113.10-203.0.113.20" {
		t.Fatalf("地址池: %v", f.ranges)
	}
	if len(f.static) != 1 || f.static[0] != "add:10.0.0.5->203.0.113.1" {
		t.Fatalf("静态映射: %v", f.static)
	}
	joined := strings.Join(f.feats, ",")
	if len(f.feats) != 3 || !strings.Contains(joined, "add:1:inside") ||
		!strings.Contains(joined, "add:2:outside") || !strings.Contains(joined, "add:2:ifaddr") {
		t.Fatalf("接口特性: %v", f.feats)
	}

	// 幂等重放：无重复增删
	f.ranges, f.feats, f.static = nil, nil, nil
	if err := p.ApplyNAT(context.Background(), natFixture()); err != nil {
		t.Fatalf("重复 ApplyNAT: %v", err)
	}
	if len(f.ranges)+len(f.feats)+len(f.static) != 0 {
		t.Fatalf("幂等重放不应有操作: ranges=%v feats=%v static=%v", f.ranges, f.feats, f.static)
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
	if len(f.feats) != 3 {
		t.Fatalf("应移除全部接口特性与接口地址: %v", f.feats)
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
