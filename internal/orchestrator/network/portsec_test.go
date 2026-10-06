package network

// 决策 #389：端口安全白名单的规则构造与 provider 行为（假 client，纯本机可跑）。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// fakePortSecClient 记录调用序并按脚本返回预设结果。
type fakePortSecClient struct {
	calls      []string
	byTag      map[string]uint32 // tag → index（预置的在场面）
	ruleCounts map[string]int
	bound      map[uint32]uint32 // swIfIndex → aclIndex（绑定实况）
	nextIndex  uint32
	lastRules  []MacipRuleSpec
	failBind   error
}

func newFakePortSec() *fakePortSecClient {
	return &fakePortSecClient{byTag: map[string]uint32{}, ruleCounts: map[string]int{}, bound: map[uint32]uint32{}, nextIndex: 7}
}

func (f *fakePortSecClient) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	f.calls = append(f.calls, "idx:"+ifname)
	if ifname == "ens224" {
		return 4, true, nil
	}
	return 0, false, nil
}

func (f *fakePortSecClient) MacipACLAddReplaceRules(index uint32, tag string, rules []MacipRuleSpec) (uint32, error) {
	f.calls = append(f.calls, "addreplace:"+tag)
	f.lastRules = rules
	if index == ^uint32(0) {
		index = f.nextIndex
		f.nextIndex++
	}
	f.byTag[tag] = index
	f.ruleCounts[tag] = len(rules)
	return index, nil
}

func (f *fakePortSecClient) MacipACLInterfaceAddDel(swIfIndex, aclIndex uint32, isAdd bool) error {
	if isAdd && f.failBind != nil {
		return f.failBind
	}
	f.calls = append(f.calls, "bind")
	if isAdd {
		f.bound[swIfIndex] = aclIndex
	} else {
		delete(f.bound, swIfIndex)
	}
	return nil
}

func (f *fakePortSecClient) MacipACLByTag(tag string) (uint32, int, bool, error) {
	idx, ok := f.byTag[tag]
	return idx, f.ruleCounts[tag], ok, nil
}

func (f *fakePortSecClient) MacipBoundACL(swIfIndex uint32) (uint32, bool, error) {
	idx, ok := f.bound[swIfIndex]
	return idx, ok, nil
}

func (f *fakePortSecClient) Close() {}

// TestBuildPortSecRulesShape：每条白名单 MAC 两条 permit（v4/v6 any + MAC 精确）+
// 末尾显式 deny-all（v4/v6 各一）——**显式 deny 是白名单语义的必要条件**
// （真机实证：macip 无匹配默认＝放行）。
func TestBuildPortSecRulesShape(t *testing.T) {
	rules := BuildPortSecRules([]string{"b0:b0:00:00:00:01", "b0:b0:00:00:00:02"})
	if len(rules) != 6 {
		t.Fatalf("规则数应为 2×2+2=6，实际 %d", len(rules))
	}
	for i, mac := range []string{"b0:b0:00:00:00:01", "b0:b0:00:00:00:02"} {
		p4, p6 := rules[2*i], rules[2*i+1]
		if !p4.Permit || p4.SrcPrefix != "0.0.0.0/0" || p4.SrcMAC != mac || p4.SrcMACMask != "ff:ff:ff:ff:ff:ff" {
			t.Fatalf("第 %d 条应为 v4 permit（任意 IP + MAC 精确）：%+v", 2*i, p4)
		}
		if !p6.Permit || p6.SrcPrefix != "::/0" || p6.SrcMAC != mac {
			t.Fatalf("第 %d 条应为 v6 permit：%+v", 2*i+1, p6)
		}
	}
	d4, d6 := rules[4], rules[5]
	if d4.Permit || d6.Permit {
		t.Fatal("末尾必须是显式 deny-all（无匹配默认放行 ⇒ 不带 deny 就没有白名单语义）")
	}
	if d4.SrcMACMask != "00:00:00:00:00:00" || d6.SrcMACMask != "00:00:00:00:00:00" {
		t.Fatal("deny-all 的 MAC 掩码必须为全零（不比较 MAC）")
	}
}

// TestPortSecApplyAndTeardown：白名单下发＝整体替换 + 绑定；清空＝解绑；
// 幂等重跑零 VPP 调用；恢复重放（登记为空）按 tag 反查复用索引、不重复创建。
func TestPortSecApplyAndTeardown(t *testing.T) {
	f := newFakePortSec()
	p := NewPortSecProvider(f)
	iface := model.InterfaceConfig{Name: "ens224", PortSecurity: []model.PortSecMAC{"b0:b0:00:00:00:01"}}

	if err := p.ApplyInterface(context.Background(), iface); err != nil {
		t.Fatalf("首次下发: %v", err)
	}
	if len(f.calls) == 0 || !strings.HasPrefix(f.calls[len(f.calls)-1], "bind") {
		t.Fatalf("调用序应以 bind 收尾: %v", f.calls)
	}
	idx := f.byTag[PortSecTag("ens224")]
	if idx == 0 || f.bound[4] != idx {
		t.Fatalf("应已绑定：idx=%d bound=%v", idx, f.bound)
	}
	// 幂等：同配置重跑零 VPP 调用。
	before := len(f.calls)
	if err := p.ApplyInterface(context.Background(), iface); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != before {
		t.Fatalf("幂等重跑不应有 VPP 调用: %v", f.calls[before:])
	}
	// 恢复重放：登记清空后按 tag 反查复用同一索引（不新建）。
	p.reset()
	f.calls = nil
	if err := p.ApplyInterface(context.Background(), iface); err != nil {
		t.Fatal(err)
	}
	if got := f.byTag[PortSecTag("ens224")]; got != idx {
		t.Fatalf("重放应复用索引 %d，实际 %d", idx, got)
	}
	if f.nextIndex != 8 {
		t.Fatalf("重放不应新建 ACL（nextIndex 应为 8，实际 %d）", f.nextIndex)
	}
	// 清空＝解绑。
	if err := p.ApplyInterface(context.Background(), model.InterfaceConfig{Name: "ens224"}); err != nil {
		t.Fatal(err)
	}
	if _, bound := f.bound[4]; bound {
		t.Fatal("清空白名单后应解绑")
	}
}

// TestPortSecApplyBindFailureKeepsRegistrationHonest：绑定失败时登记不写成已绑定
// （登记=最后一个成功下发的状态，决策 #363 口径）——重试会重跑绑定。
func TestPortSecApplyBindFailureKeepsRegistrationHonest(t *testing.T) {
	f := newFakePortSec()
	f.failBind = errors.New("vpp busy")
	p := NewPortSecProvider(f)
	iface := model.InterfaceConfig{Name: "ens224", PortSecurity: []model.PortSecMAC{"b0:b0:00:00:00:01"}}
	if err := p.ApplyInterface(context.Background(), iface); err == nil {
		t.Fatal("绑定失败应返回错误")
	}
	// 重试（绑定恢复正常）应重新发起绑定而不是因登记跳过。
	f.failBind = nil
	if err := p.ApplyInterface(context.Background(), iface); err != nil {
		t.Fatalf("重试: %v", err)
	}
	if _, bound := f.bound[4]; !bound {
		t.Fatal("重试后应完成绑定")
	}
}

// TestPortSecTeardownOnlyOwnSlot：决策 #390②——teardown 只解自己那个槽。
// 正例：槽上正是本接口端口安全 ACL ⇒ 解绑。负例：同提交「删白名单 + 改挂 L3 acl-in」时
// #341 伴随 macip 先绑（槽上是他方 ACL）⇒ 不得解绑（否则域内非 IP/ARP 被静默丢弃，
// round171 §1.2 真机复现的失败形态）。
func TestPortSecTeardownOnlyOwnSlot(t *testing.T) {
	iface := model.InterfaceConfig{Name: "ens224", PortSecurity: []model.PortSecMAC{"b0:b0:00:00:00:01"}}

	// 正例：槽上是自己的 ACL ⇒ 解绑。
	f := newFakePortSec()
	p := NewPortSecProvider(f)
	if err := p.ApplyInterface(context.Background(), iface); err != nil {
		t.Fatal(err)
	}
	ownIdx := f.byTag[PortSecTag("ens224")]
	if f.bound[4] != ownIdx || ownIdx == 0 {
		t.Fatalf("前置：应已绑定自己的端口安全 ACL: %v", f.bound)
	}
	if err := p.ApplyInterface(context.Background(), model.InterfaceConfig{Name: "ens224"}); err != nil {
		t.Fatal(err)
	}
	if _, bound := f.bound[4]; bound {
		t.Fatalf("槽上是自己的 ACL：应解绑")
	}

	// 负例：槽被他方 macip ACL（如 #341 伴随）占用 ⇒ 不动作，且登记仍要清。
	f2 := newFakePortSec()
	p2 := NewPortSecProvider(f2)
	if err := p2.ApplyInterface(context.Background(), iface); err != nil {
		t.Fatal(err)
	}
	f2.bound[4] = 99 // 伴随 macip（他方）占用同一槽
	f2.calls = nil
	if err := p2.ApplyInterface(context.Background(), model.InterfaceConfig{Name: "ens224"}); err != nil {
		t.Fatal(err)
	}
	if got := f2.bound[4]; got != 99 {
		t.Fatalf("槽上是他方 ACL：不得解绑（应保持 99，实际 %d）", got)
	}
	for _, c := range f2.calls {
		if c == "bind" {
			t.Fatalf("槽上是他方 ACL：不得产生任何 macip 绑定/解绑调用: %v", f2.calls)
		}
	}
	// 登记应已清空（portsec 已停用）：再次空声明幂等零调用。
	before := len(f2.calls)
	if err := p2.ApplyInterface(context.Background(), model.InterfaceConfig{Name: "ens224"}); err != nil {
		t.Fatal(err)
	}
	if len(f2.calls) != before {
		t.Fatalf("登记应已清空（幂等零调用）: %v", f2.calls[before:])
	}
}

// TestPortSecDataplaneHonest：tag 不在场/绑定槽被别的 ACL 占用时如实呈现（不编造）。
func TestPortSecDataplaneHonest(t *testing.T) {
	f := newFakePortSec()
	p := NewPortSecProvider(f)
	dp, err := p.Dataplane(context.Background(), "ens224")
	if err != nil {
		t.Fatal(err)
	}
	if !dp.Available || dp.TagPresent || dp.Bound {
		t.Fatalf("未下发时应如实报不在场/未绑定：%+v", dp)
	}
	// 槽被别的 macip ACL 占用（如 #341 伴随 ACL）：Bound=false 且带占用索引。
	f.bound[4] = 99
	dp, _ = p.Dataplane(context.Background(), "ens224")
	if dp.Bound || dp.BoundIndex != 99 {
		t.Fatalf("槽被占用时应如实报未绑定 + 占用索引：%+v", dp)
	}
}
