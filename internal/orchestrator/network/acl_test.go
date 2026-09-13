package network

import (
	"context"
	"errors"
	"strings"
	"testing"

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
}

func newFakeAcl() *fakeAcl {
	return &fakeAcl{ifaces: map[string]uint32{"ens192": 1, "ens224": 2, "bvi0": 3}, nextIdx: 99}
}

func (f *fakeAcl) Close() {}

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
		return f.nextIdx, nil
	}
	f.replaced = append(f.replaced, index)
	return index, nil
}

func (f *fakeAcl) ACLDel(index uint32) error {
	if f.err != nil {
		return f.err
	}
	f.del = append(f.del, index)
	return nil
}

func (f *fakeAcl) ACLInterfaceSet(swIfIndex, inAcl, outAcl uint32, inSet, outSet bool) error {
	if f.err != nil {
		return f.err
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
