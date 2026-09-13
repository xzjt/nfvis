package network

// M3-5（二）：ACL 编排（FR-NET 端口/网关/L3 接口的 acl-in/acl-out，VPP acl plugin）。
//
// 规则：acl_add_replace（按名字登记 index，重放走 replace）；删除 acl_del 并解绑引用。
// 绑定：acl_interface_set_acl_list（in/out 各一），由 L2/L3 provider 在挂接端口、
// 配置 L3 接口、建立 BVI 时调用 Bind。
//
// 命中计数：VPP ACL 计数器在 stats segment（非 binary API），运行态经 /vpp/*（M3-7）
// 由 stats 客户端读取；本轮先落配置态与绑定。

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/xzjt/nfvis/internal/model"
)

// aclIndexNew VPP acl_add_replace 的新建索引哨兵（~0）。
const aclIndexNew = ^uint32(0)

// ACLRuleSpec 与底座解耦的规则形态（便于单测转换逻辑）。
type ACLRuleSpec struct {
	Permit    bool
	Src       string // ip-prefix 或 any
	Dst       string
	Proto     uint8 // 6/17/1；0=any
	SPortFrom uint16
	SPortTo   uint16
	DPortFrom uint16
	DPortTo   uint16
}

// ACLClient VPP acl plugin binary API 的最小能力集。
type ACLClient interface {
	SwInterfaceIndex(ifname string) (uint32, bool, error)
	ACLAddReplace(index uint32, tag string, rules []ACLRuleSpec) (uint32, error)
	ACLDel(index uint32) error
	ACLInterfaceSet(swIfIndex, inAcl, outAcl uint32, inSet, outSet bool) error
	Close()
}

// AclProvider ACL 规则与接口绑定编排。
type AclProvider struct {
	client func() (ACLClient, error)

	mu      sync.Mutex
	index   map[string]uint32  // ACL 名 → VPP acl index
	bound   map[uint32]aclPair // sw_if_index → 绑定的 in/out ACL 名
	byIface map[string]uint32  // 接口名 → sw_if_index（解绑用）
}

type aclPair struct{ in, out string }

// NewAclProvider 以固定客户端构造（测试）。
func NewAclProvider(c ACLClient) *AclProvider {
	return &AclProvider{client: func() (ACLClient, error) { return c, nil },
		index: map[string]uint32{}, bound: map[uint32]aclPair{}, byIface: map[string]uint32{}}
}

// NewAclProviderFunc 以客户端工厂构造（连接可重连）。
func NewAclProviderFunc(f func() (ACLClient, error)) *AclProvider {
	return &AclProvider{client: f, index: map[string]uint32{},
		bound: map[uint32]aclPair{}, byIface: map[string]uint32{}}
}

// ApplyACL 下发/更新 ACL 规则（同名走 replace）。
func (p *AclProvider) ApplyACL(ctx context.Context, acl model.Acl) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	rules, err := BuildACLRules(acl)
	if err != nil {
		return fmt.Errorf("ACL %s 规则: %w", acl.Name, err)
	}
	p.mu.Lock()
	idx, known := p.index[acl.Name]
	p.mu.Unlock()
	if !known {
		idx = aclIndexNew // VPP：~0 表示新建（0 是合法 ACL 索引，不能当"未下发"）
	}
	newIdx, err := c.ACLAddReplace(idx, acl.Name, rules)
	if err != nil {
		return fmt.Errorf("下发 ACL %s: %w", acl.Name, err)
	}
	p.mu.Lock()
	p.index[acl.Name] = newIdx
	p.mu.Unlock()
	return nil
}

// DeleteACL 删除 ACL 并解绑引用它的接口。
func (p *AclProvider) DeleteACL(ctx context.Context, name string) error {
	p.mu.Lock()
	idx, ok := p.index[name]
	delete(p.index, name)
	var rebind []uint32
	for swIf, pair := range p.bound {
		if pair.in == name || pair.out == name {
			if pair.in == name {
				pair.in = ""
			}
			if pair.out == name {
				pair.out = ""
			}
			p.bound[swIf] = pair
			rebind = append(rebind, swIf)
		}
	}
	p.mu.Unlock()
	if !ok {
		return nil
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	for _, swIf := range rebind {
		pair := p.pairOf(swIf)
		in, inSet := p.lookup(pair.in)
		out, outSet := p.lookup(pair.out)
		if err := c.ACLInterfaceSet(swIf, in, out, inSet && pair.in != "", outSet && pair.out != ""); err != nil {
			return fmt.Errorf("解绑接口 %d 的 ACL %s: %w", swIf, name, err)
		}
	}
	if err := c.ACLDel(idx); err != nil {
		return fmt.Errorf("删除 ACL %s: %w", name, err)
	}
	return nil
}

// Bind 按接口名把 acl-in/acl-out 绑定到接口（名字为空表示不绑）。
func (p *AclProvider) Bind(ctx context.Context, ifname, aclIn, aclOut string) error {
	if aclIn == "" && aclOut == "" {
		return nil
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	idx, ok, err := c.SwInterfaceIndex(ifname)
	if err != nil {
		return fmt.Errorf("解析接口 %s: %w", ifname, err)
	}
	if !ok {
		return fmt.Errorf("接口 %s 不存在于 VPP（是否未由 DPDK 接管？）", ifname)
	}
	if err := p.BindIndex(c, idx, aclIn, aclOut); err != nil {
		return err
	}
	p.mu.Lock()
	p.byIface[ifname] = idx
	p.mu.Unlock()
	return nil
}

// BindIndex 按 sw_if_index 绑定（VLAN 子接口/BVI 等无配置名场景）。
func (p *AclProvider) BindIndex(c ACLClient, swIfIndex uint32, aclIn, aclOut string) error {
	if aclIn == "" && aclOut == "" {
		return nil
	}
	in, inSet := p.lookup(aclIn)
	if aclIn != "" && !inSet {
		return fmt.Errorf("ACL %s 尚未下发（apply 顺序应为 ACL 先于 L2/L3）", aclIn)
	}
	out, outSet := p.lookup(aclOut)
	if aclOut != "" && !outSet {
		return fmt.Errorf("ACL %s 尚未下发（apply 顺序应为 ACL 先于 L2/L3）", aclOut)
	}
	if err := c.ACLInterfaceSet(swIfIndex, in, out, inSet && aclIn != "", outSet && aclOut != ""); err != nil {
		return fmt.Errorf("绑定接口 %d 的 ACL: %w", swIfIndex, err)
	}
	p.mu.Lock()
	p.bound[swIfIndex] = aclPair{in: aclIn, out: aclOut}
	p.mu.Unlock()
	return nil
}

// lookup 返回 ACL 名对应的 VPP 索引与是否已下发（索引 0 合法，故需 ok 区分）。
func (p *AclProvider) lookup(name string) (uint32, bool) {
	if name == "" {
		return 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	idx, ok := p.index[name]
	return idx, ok
}

func (p *AclProvider) pairOf(swIf uint32) aclPair {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bound[swIf]
}

// BuildACLRules 把配置模型规则转换为与底座解耦的规则形态。
func BuildACLRules(acl model.Acl) ([]ACLRuleSpec, error) {
	out := make([]ACLRuleSpec, 0, len(acl.Rules))
	for _, r := range acl.Rules {
		spec := ACLRuleSpec{
			Permit: strings.EqualFold(r.Action, "permit"),
			Src:    prefixOrAny(r.Source),
			Dst:    prefixOrAny(r.Destination),
			Proto:  protoOf(r.Protocol),
		}
		var err error
		if spec.SPortFrom, spec.SPortTo, err = parsePortRange(r.SourcePort); err != nil {
			return nil, fmt.Errorf("规则 %d 源端口 %q: %w", r.Seq, r.SourcePort, err)
		}
		if spec.DPortFrom, spec.DPortTo, err = parsePortRange(r.DestinationPort); err != nil {
			return nil, fmt.Errorf("规则 %d 目的端口 %q: %w", r.Seq, r.DestinationPort, err)
		}
		out = append(out, spec)
	}
	return out, nil
}

func prefixOrAny(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "any") {
		return "0.0.0.0/0"
	}
	return s
}

func protoOf(p string) uint8 {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "tcp":
		return 6
	case "udp":
		return 17
	case "icmp":
		return 1
	case "icmp6", "icmpv6":
		return 58
	default:
		return 0 // any
	}
}

// parsePortRange 解析 "80" / "1024-65535"；空表示任意（0-65535）。
func parsePortRange(s string) (uint16, uint16, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "any") {
		return 0, 65535, nil
	}
	lo, hi := s, s
	if i := strings.IndexByte(s, '-'); i >= 0 {
		lo, hi = strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
	}
	a, err := strconv.Atoi(lo)
	if err != nil || a < 0 || a > 65535 {
		return 0, 0, fmt.Errorf("非法端口 %q", lo)
	}
	b, err := strconv.Atoi(hi)
	if err != nil || b < a || b > 65535 {
		return 0, 0, fmt.Errorf("非法端口范围 %q", s)
	}
	return uint16(a), uint16(b), nil
}
