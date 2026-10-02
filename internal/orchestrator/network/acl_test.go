package network

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"go.fd.io/govpp/api"

	"github.com/xzjt/nfvis/internal/model"
)

// ---------- M3-5（二）：ACL 规则与绑定（假 ACLClient） ----------

type fakeAcl struct {
	ifaces   map[string]uint32
	nextIdx  uint32
	added    []string
	replaced []uint32
	setCalls [][3]uint32 // swIf, in, out
	del      []uint32
	err      error
	// tags/idxTag 模拟 VPP 里现存的 ACL（ACLTags 残渣对账用，决策 #321）。
	tags   map[string]uint32
	idxTag map[uint32]string
	// 伴随 macip ACL（决策 #341）：创建/替换/绑定调用与失败注入。
	macipCreated  []string
	macipReplaced []uint32
	macipBind     [][3]uint32 // swIf, aclIdx, isAdd(1/0)
	macipErr      error
	macipExists   bool // 是否让 add 返回「已存在」错误
	// setErr 只作用于 ACLInterfaceSet（决策 #342：绑定成功后单独注入解绑失败）。
	setErr error
}

func newFakeAcl() *fakeAcl {
	return &fakeAcl{ifaces: map[string]uint32{"ens192": 1, "ens224": 2, "bvi0": 3}, nextIdx: 99,
		tags: map[string]uint32{}, idxTag: map[uint32]string{}}
}

// putACL 直接注入一个「数据面已存在」的 ACL（模拟补偿失败留下的残渣）。
func (f *fakeAcl) putACL(tag string) uint32 {
	f.nextIdx++
	f.tags[tag] = f.nextIdx
	f.idxTag[f.nextIdx] = tag
	return f.nextIdx
}

func (f *fakeAcl) Close() {}

func (f *fakeAcl) ACLTags() ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]string, 0, len(f.tags))
	for t := range f.tags {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeAcl) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	idx, ok := f.ifaces[ifname]
	return idx, ok, nil
}

func (f *fakeAcl) ACLAddReplace(index uint32, tag string, rules []ACLRuleSpec) (uint32, error) {
	if f.err != nil {
		return 0, f.err
	}
	if index == aclIndexNew {
		f.nextIdx++
		f.added = append(f.added, tag)
		f.tags[tag] = f.nextIdx
		f.idxTag[f.nextIdx] = tag
		return f.nextIdx, nil
	}
	f.replaced = append(f.replaced, index)
	f.tags[tag] = index
	f.idxTag[index] = tag
	return index, nil
}

func (f *fakeAcl) ACLDel(index uint32) error {
	if f.err != nil {
		return f.err
	}
	f.del = append(f.del, index)
	if tag, ok := f.idxTag[index]; ok {
		delete(f.tags, tag)
		delete(f.idxTag, index)
	}
	return nil
}

func (f *fakeAcl) ACLInterfaceSet(swIfIndex, inAcl, outAcl uint32, inSet, outSet bool) error {
	if f.err != nil {
		return f.err
	}
	if f.setErr != nil {
		return f.setErr
	}
	if !inSet {
		inAcl = 0
	}
	if !outSet {
		outAcl = 0
	}
	f.setCalls = append(f.setCalls, [3]uint32{swIfIndex, inAcl, outAcl})
	return nil
}

// MacipACLAddReplace 模拟伴随 macip ACL 的创建/替换（决策 #341）。
// fakeAcl **不**实现 MacipIndexLookup：这样 reset 后重放走「新建」分支（与 VPP 重启后实况一致）。
func (f *fakeAcl) MacipACLAddReplace(index uint32, tag string) (uint32, error) {
	if f.err != nil {
		return 0, f.err
	}
	if f.macipErr != nil {
		return 0, f.macipErr
	}
	if index == aclIndexNew {
		f.nextIdx++
		f.macipCreated = append(f.macipCreated, tag)
		return f.nextIdx, nil
	}
	f.macipReplaced = append(f.macipReplaced, index)
	return index, nil
}

func (f *fakeAcl) MacipACLInterfaceAddDel(swIfIndex, aclIndex uint32, isAdd bool) error {
	if f.err != nil {
		return f.err
	}
	if f.macipErr != nil {
		return f.macipErr
	}
	if f.macipExists {
		f.macipExists = false
		return api.VPPApiError(vppValueExist)
	}
	add := uint32(0)
	if isAdd {
		add = 1
	}
	f.macipBind = append(f.macipBind, [3]uint32{swIfIndex, aclIndex, add})
	return nil
}

func TestBuildACLRules(t *testing.T) {
	acl := model.Acl{Name: "web", Rules: []model.AclRule{
		{Seq: 10, Action: "permit", Source: "any", Destination: "10.0.0.0/24",
			Protocol: "tcp", SourcePort: "1024-65535", DestinationPort: "80"},
		{Seq: 20, Action: "deny", Source: "192.168.1.0/24", Destination: "any", Protocol: "udp", DestinationPort: "53"},
		{Seq: 30, Action: "permit", Source: "any", Destination: "any", Protocol: "any"},
	}}
	rules, err := BuildACLRules(acl)
	if err != nil {
		t.Fatalf("BuildACLRules: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("规则数: %d", len(rules))
	}
	r0 := rules[0]
	if !r0.Permit || r0.Src != "0.0.0.0/0" || r0.Dst != "10.0.0.0/24" || r0.Proto != 6 ||
		r0.SPortFrom != 1024 || r0.SPortTo != 65535 || r0.DPortFrom != 80 || r0.DPortTo != 80 {
		t.Fatalf("规则 0 转换不符: %+v", r0)
	}
	if rules[1].Permit || rules[1].Proto != 17 || rules[1].DPortFrom != 53 {
		t.Fatalf("规则 1 转换不符: %+v", rules[1])
	}
	if rules[2].Proto != 0 || rules[2].SPortFrom != 0 || rules[2].SPortTo != 65535 {
		t.Fatalf("规则 2（any）转换不符: %+v", rules[2])
	}
	// 非法端口
	if _, err := BuildACLRules(model.Acl{Name: "x", Rules: []model.AclRule{{Action: "permit", SourcePort: "70000"}}}); err == nil {
		t.Fatalf("非法端口应报错")
	}
	if _, err := BuildACLRules(model.Acl{Name: "x", Rules: []model.AclRule{{Action: "permit", DestinationPort: "100-50"}}}); err == nil {
		t.Fatalf("倒置端口范围应报错")
	}
}

func TestAclApplyReplaceAndDeleteUnbind(t *testing.T) {
	f := newFakeAcl()
	p := NewAclProvider(f)
	acl := model.Acl{Name: "web", Rules: []model.AclRule{{Seq: 10, Action: "permit", Source: "any", Destination: "any"}}}
	if err := p.ApplyACL(context.Background(), acl); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	if err := p.Bind(context.Background(), "ens192", "web", ""); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if len(f.setCalls) != 1 || f.setCalls[0] != [3]uint32{1, 100, 0} {
		t.Fatalf("绑定调用不符: %v", f.setCalls)
	}
	// 同名再下发 → replace（带 index），不新增
	if err := p.ApplyACL(context.Background(), acl); err != nil {
		t.Fatalf("重复 ApplyACL: %v", err)
	}
	if len(f.added) != 1 || len(f.replaced) != 1 || f.replaced[0] != 100 {
		t.Fatalf("重复下发应 replace: added=%v replaced=%v", f.added, f.replaced)
	}
	// 删除 → 解绑（in 清 0）后删 ACL
	if err := p.DeleteACL(context.Background(), "web"); err != nil {
		t.Fatalf("DeleteACL: %v", err)
	}
	if len(f.del) != 1 || f.del[0] != 100 {
		t.Fatalf("应删除 ACL: %v", f.del)
	}
	last := f.setCalls[len(f.setCalls)-1]
	if last != [3]uint32{1, 0, 0} {
		t.Fatalf("删除后应解绑: %v", f.setCalls)
	}
	// 删除不存在的 ACL 无害
	if err := p.DeleteACL(context.Background(), "nope"); err != nil {
		t.Fatalf("删除不存在应无害: %v", err)
	}
}

func TestAclBindErrors(t *testing.T) {
	f := newFakeAcl()
	p := NewAclProvider(f)
	// 未下发即绑定 → 报错
	if err := p.Bind(context.Background(), "ens192", "missing", ""); err == nil ||
		!strings.Contains(err.Error(), "尚未下发") {
		t.Fatalf("未下发应报错: %v", err)
	}
	// 接口不存在
	if err := p.Bind(context.Background(), "ens999", "", "x"); err == nil {
		t.Fatalf("接口缺失应报错")
	}
	// 空绑定为无操作
	if err := p.Bind(context.Background(), "ens192", "", ""); err != nil {
		t.Fatalf("空绑定应无操作: %v", err)
	}
	// 客户端错误
	fe := newFakeAcl()
	fe.err = errors.New("boom")
	pe := NewAclProvider(fe)
	if err := pe.ApplyACL(context.Background(), model.Acl{Name: "a"}); err == nil {
		t.Fatalf("下发错误应上抛")
	}
}

// L2 端口绑定：ApplyBridgeDomain 后 acl-in/acl-out 落到端口。
func TestL2BindsPortACL(t *testing.T) {
	l2f := newFakeL2()
	aclf := newFakeAcl()
	acl := NewAclProvider(aclf)
	if err := acl.ApplyACL(context.Background(), model.Acl{Name: "in-acl"}); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	p := NewL2Provider(l2f)
	p.SetACL(acl)
	vs := l2vs("vs-acl", model.VSwitchPort{Seq: 0, Interface: "ens192", AclIn: "in-acl"})
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("ApplyBridgeDomain: %v", err)
	}
	if len(aclf.setCalls) != 1 || aclf.setCalls[0] != [3]uint32{1, 100, 0} {
		t.Fatalf("端口 ACL 绑定不符: %v", aclf.setCalls)
	}
}

// ACL 索引 0 合法：绑定不能被当成"未设置"。
func TestAclIndexZero(t *testing.T) {
	f := newFakeAcl()
	f.nextIdx = ^uint32(0) // 首次创建返回索引 0
	p := NewAclProvider(f)
	if err := p.ApplyACL(context.Background(), model.Acl{Name: "acl0",
		Rules: []model.AclRule{{Seq: 1, Action: "permit", Source: "any", Destination: "any"}}}); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	if idx, ok := p.lookup("acl0"); !ok || idx != 0 {
		t.Fatalf("索引 0 应登记为已下发: %d %v", idx, ok)
	}
	if err := p.Bind(context.Background(), "ens192", "acl0", ""); err != nil {
		t.Fatalf("绑定索引 0 的 ACL 不应报未下发: %v", err)
	}
	if len(f.setCalls) != 1 || f.setCalls[0] != [3]uint32{1, 0, 0} {
		t.Fatalf("应绑入向 ACL 索引 0: %v", f.setCalls)
	}
}

// ---------- 决策 #341：绑 L3 接口 ACL 时自动伴随 macip（放行非 IP/ARP） ----------

// ① 绑 IPv4 ACL → 发出 macip create + bind 消息。
func TestMacipCompanionOnBind(t *testing.T) {
	f := newFakeAcl()
	p := NewAclProvider(f)
	if err := p.ApplyACL(context.Background(), model.Acl{Name: "web",
		Rules: []model.AclRule{{Seq: 10, Action: "permit", Source: "any", Destination: "any"}}}); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	if err := p.BindIndex(f, 1, "web", ""); err != nil {
		t.Fatalf("BindIndex: %v", err)
	}
	if len(f.macipCreated) != 1 || f.macipCreated[0] != macipACLTag {
		t.Fatalf("应创建一条伴随 macip ACL: %v", f.macipCreated)
	}
	if len(f.macipBind) != 1 || f.macipBind[0][0] != 1 || f.macipBind[0][2] != 1 {
		t.Fatalf("应把 macip ACL 绑到接口 1: %v", f.macipBind)
	}
	// IP ACL 绑定语义不受影响：仍是同一次 set（if=1,in=100）
	if len(f.setCalls) != 1 || f.setCalls[0] != [3]uint32{1, 100, 0} {
		t.Fatalf("IP ACL 绑定不应变化: %v", f.setCalls)
	}
}

// ② 重复绑 → 幂等（不重复 create、不重复 bind）。
func TestMacipCompanionIdempotent(t *testing.T) {
	f := newFakeAcl()
	p := NewAclProvider(f)
	if err := p.ApplyACL(context.Background(), model.Acl{Name: "web"}); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := p.BindIndex(f, 1, "web", ""); err != nil {
			t.Fatalf("BindIndex #%d: %v", i, err)
		}
	}
	if len(f.macipCreated) != 1 {
		t.Fatalf("重复绑不得重复创建 macip ACL: %v", f.macipCreated)
	}
	if len(f.macipBind) != 1 {
		t.Fatalf("重复绑不得重复 bind: %v", f.macipBind)
	}
	// 另一接口复用同一条 macip ACL（只 create 一次，bind 两次）
	if err := p.BindIndex(f, 2, "web", ""); err != nil {
		t.Fatalf("BindIndex 接口2: %v", err)
	}
	if len(f.macipCreated) != 1 || len(f.macipBind) != 2 {
		t.Fatalf("另一接口应复用 macip ACL 并各自 bind: created=%v bind=%v", f.macipCreated, f.macipBind)
	}
}

// ③ 解绑（空绑定）→ macip 一并解绑。
func TestMacipCompanionUnbind(t *testing.T) {
	f := newFakeAcl()
	p := NewAclProvider(f)
	if err := p.ApplyACL(context.Background(), model.Acl{Name: "web"}); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	if err := p.BindIndex(f, 1, "web", ""); err != nil {
		t.Fatalf("BindIndex: %v", err)
	}
	if err := p.BindIndex(f, 1, "", ""); err != nil {
		t.Fatalf("解绑: %v", err)
	}
	last := f.macipBind[len(f.macipBind)-1]
	if last[0] != 1 || last[2] != 0 {
		t.Fatalf("解绑应发出 macip unbind(接口1): %v", f.macipBind)
	}
	// 解绑后 IP ACL 也被清空
	if got := f.setCalls[len(f.setCalls)-1]; got != [3]uint32{1, 0, 0} {
		t.Fatalf("解绑应清 IP ACL 绑定: %v", f.setCalls)
	}
	// 无登记再解绑 → 空操作（幂等）
	n := len(f.macipBind)
	if err := p.BindIndex(f, 1, "", ""); err != nil {
		t.Fatalf("重复解绑应无害: %v", err)
	}
	if len(f.macipBind) != n {
		t.Fatalf("重复解绑不得再发消息: %v", f.macipBind)
	}
}

// ④ reset() 后重放 → 重建（清掉陈旧的 macip 登记，重新 create+bind）。
func TestMacipCompanionRebuiltAfterReset(t *testing.T) {
	f := newFakeAcl()
	p := NewAclProvider(f)
	if err := p.ApplyACL(context.Background(), model.Acl{Name: "web"}); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	if err := p.BindIndex(f, 1, "web", ""); err != nil {
		t.Fatalf("BindIndex: %v", err)
	}
	if len(f.macipCreated) != 1 {
		t.Fatalf("首次应创建: %v", f.macipCreated)
	}
	p.reset() // 恢复收敛开头：登记全清
	// reset 清了 index 登记，但 ApplyACL 不在本测试重放；BindIndex 依赖 index 登记，故先重放 ACL。
	if err := p.ApplyACL(context.Background(), model.Acl{Name: "web"}); err != nil {
		t.Fatalf("重放 ApplyACL: %v", err)
	}
	if err := p.BindIndex(f, 1, "web", ""); err != nil {
		t.Fatalf("reset 后重放 BindIndex: %v", err)
	}
	if len(f.macipCreated) != 2 {
		t.Fatalf("reset 后应重建 macip ACL（VPP 侧已随重启消失）: %v", f.macipCreated)
	}
	if len(f.macipBind) != 2 || f.macipBind[1][2] != 1 {
		t.Fatalf("reset 后应再次 bind: %v", f.macipBind)
	}
}

// ⑤ 失败路径如实上抛（不吞）：macip create 失败 → BindIndex 报错；已存在按目标状态。
func TestMacipCompanionErrors(t *testing.T) {
	f := newFakeAcl()
	p := NewAclProvider(f)
	if err := p.ApplyACL(context.Background(), model.Acl{Name: "web"}); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	f.macipErr = errors.New("boom")
	if err := p.BindIndex(f, 1, "web", ""); err == nil || !strings.Contains(err.Error(), "macip") {
		t.Fatalf("macip 创建失败应上抛并指名 macip: %v", err)
	}
	// 「已存在」按目标状态：不报错且登记为已绑（再次绑定时不再发 bind）
	f2 := newFakeAcl()
	p2 := NewAclProvider(f2)
	if err := p2.ApplyACL(context.Background(), model.Acl{Name: "web"}); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	f2.macipExists = true
	if err := p2.BindIndex(f2, 1, "web", ""); err != nil {
		t.Fatalf("绑定已存在应按成功: %v", err)
	}
	if len(f2.macipBind) != 0 {
		t.Fatalf("「已存在」的 add 不应被记为成功 bind: %v", f2.macipBind)
	}
	if err := p2.BindIndex(f2, 1, "web", ""); err != nil {
		t.Fatalf("再次绑定: %v", err)
	}
	if len(f2.macipCreated) != 1 || len(f2.macipBind) != 0 {
		t.Fatalf("「已存在」后应登记为已绑，再次绑定为空操作: created=%v bind=%v", f2.macipCreated, f2.macipBind)
	}
}

// ⑥ 恢复收敛重放含伴随 macip：配置里 L3 接口带 acl_in → EnsureConsistent 重放时重建 macip 绑定。
// （VPP 重启后 macip 绑定随运行态消失，不重放即静默丢 ARP 放行——同「静默丢失」族教训。）
func TestEnsureConsistentReplaysMacipCompanion(t *testing.T) {
	f := newRecoveryFixture()
	cfg := model.Config{
		Acls: []model.Acl{{Name: "web",
			Rules: []model.AclRule{{Seq: 10, Action: "permit", Source: "any", Destination: "any"}}}},
		VirtualSwitches: []model.VirtualSwitch{{Name: "vs-l3", Type: "l3"}},
		Vrfs: []model.Vrf{{Name: "vs-l3",
			L3Interfaces: []model.L3Interface{{Interface: "ens192", AclIn: "web"}}}},
	}
	if errs := f.net.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("应收敛成功: %v", errs)
	}
	if len(f.acl.macipCreated) != 1 || len(f.acl.macipBind) != 1 {
		t.Fatalf("重放应含伴随 macip create+bind: created=%v bind=%v", f.acl.macipCreated, f.acl.macipBind)
	}
	// 再次收敛（reset 清登记，模拟 VPP 重连/重启）：必须再次重建绑定。
	if errs := f.net.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("重复收敛应成功: %v", errs)
	}
	if len(f.acl.macipCreated) != 2 || len(f.acl.macipBind) != 2 {
		t.Fatalf("reset 后重放应重建 macip: created=%v bind=%v", f.acl.macipCreated, f.acl.macipBind)
	}
}

// ---------- 决策 #342：解绑遇「接口已不存在」按已达成（不触发回滚） ----------

// ⑦ 解绑时 VPP 报「接口不存在」(-2) → BindIndex 返回 nil 且清登记（IP + 伴随 macip）。
func TestAclUnbindMissingIfaceTolerated(t *testing.T) {
	f := newFakeAcl()
	p := NewAclProvider(f)
	if err := p.ApplyACL(context.Background(), model.Acl{Name: "web"}); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	if err := p.BindIndex(f, 1, "web", ""); err != nil {
		t.Fatalf("BindIndex: %v", err)
	}
	p.mu.Lock()
	_, hadPair := p.bound[1]
	_, hadMacip := p.macipBound[1]
	p.mu.Unlock()
	if !hadPair || !hadMacip {
		t.Fatalf("前置：接口 1 应已登记 IP ACL 与伴随 macip")
	}
	f.setErr = api.VPPApiError(-2) // 接口已随交换机/VM 删除
	if err := p.BindIndex(f, 1, "", ""); err != nil {
		t.Fatalf("接口不存在时解绑应按已达成返回 nil: %v", err)
	}
	p.mu.Lock()
	_, hadPair = p.bound[1]
	_, hadMacip = p.macipBound[1]
	p.mu.Unlock()
	if hadPair || hadMacip {
		t.Fatalf("接口不存在解绑后登记应被清空: bound=%v macip=%v", hadPair, hadMacip)
	}
}

// ⑧ 解绑遇其它 VPP 错误 → 如实上抛（不吞、不泛化），登记保留。
func TestAclUnbindOtherErrorPropagates(t *testing.T) {
	f := newFakeAcl()
	p := NewAclProvider(f)
	if err := p.ApplyACL(context.Background(), model.Acl{Name: "web"}); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	if err := p.BindIndex(f, 1, "web", ""); err != nil {
		t.Fatalf("BindIndex: %v", err)
	}
	f.setErr = errors.New("boom")
	if err := p.BindIndex(f, 1, "", ""); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("其它错误应上抛: %v", err)
	}
	p.mu.Lock()
	_, hadPair := p.bound[1]
	p.mu.Unlock()
	if !hadPair {
		t.Fatalf("解绑未达成时不应清登记")
	}
}

// ⑨ 伴随 macip 解绑遇「接口不存在」→ 按已达成、清 macip 登记。
func TestMacipUnbindMissingIfaceTolerated(t *testing.T) {
	f := newFakeAcl()
	p := NewAclProvider(f)
	if err := p.ApplyACL(context.Background(), model.Acl{Name: "web"}); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	if err := p.BindIndex(f, 1, "web", ""); err != nil {
		t.Fatalf("BindIndex: %v", err)
	}
	f.macipErr = api.VPPApiError(-2)
	if err := p.MacipDisallowNonIP(f, 1); err != nil {
		t.Fatalf("macip 解绑遇接口不存在应按已达成: %v", err)
	}
	p.mu.Lock()
	_, hadMacip := p.macipBound[1]
	p.mu.Unlock()
	if hadMacip {
		t.Fatalf("macip 登记应被清")
	}
}

// ⑩ DeleteACL（真实 del-acl 计划操作）：接口已不存在(-2)时解绑按已达成，ACL 仍被删除且清扫登记。
func TestDeleteACLMissingIfaceTolerated(t *testing.T) {
	f := newFakeAcl()
	p := NewAclProvider(f)
	if err := p.ApplyACL(context.Background(), model.Acl{Name: "web"}); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	if err := p.BindIndex(f, 1, "web", ""); err != nil {
		t.Fatalf("BindIndex: %v", err)
	}
	f.setErr = api.VPPApiError(-2) // 接口已随交换机/VM 删除
	if err := p.DeleteACL(context.Background(), "web"); err != nil {
		t.Fatalf("接口不存在时删 ACL 应按已达成、不得中止整次删除: %v", err)
	}
	if len(f.del) != 1 {
		t.Fatalf("ACL 本身仍应被删除: %v", f.del)
	}
	p.mu.Lock()
	_, hadPair := p.bound[1]
	p.mu.Unlock()
	if hadPair {
		t.Fatalf("陈旧绑定登记应被清（此前须重启 nfvis 才能删掉）")
	}
}
