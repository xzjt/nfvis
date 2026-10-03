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
	"log"
	"net"
	"sort"
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
	Proto     uint8 // 6/17；icmp 随规则家族 1(v4)/58(v6)；0=any
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
	// ACLTags 列出 VPP 里全部 ACL 的 tag（acl_dump 全量）。残渣对账用（决策 #321）：
	// 「tag 不在配置里」即提交补偿失败留下的 ACL 残渣——与残留表（#192）同一份对账视野，
	// 不靠进程内记忆，故跨 nfvisd 重启仍可见。
	ACLTags() ([]string, error)
	// MacipACLAddReplace 创建/替换伴随的「放行全部非 IP 帧」macip ACL（决策 #341）：
	// index=aclIndexNew 新建，否则替换；返回实际索引。
	//
	// 为何需要它：VPP acl plugin 在启用 IP ACL 的接口上对**非 IP L2 帧**走 macip 白名单路径，
	// 未配 macip ACL 即丢弃——产品唯一可用的 ACL 形态（L3 接口 acl-in）一绑上就把域内 ARP 丢了。
	// 这条 macip ACL（permit 任意 ip/mac，mask 0=不比较 MAC）是该路径的唯一载体，只作用于非 IP 帧，
	// IPv4/IPv6 仍走已绑的 IP ACL，故 IP 过滤语义不受影响。
	MacipACLAddReplace(index uint32, tag string) (uint32, error)
	// MacipACLInterfaceAddDel 把 macip ACL 绑定/解绑到接口（isAdd=true 绑、false 解）。
	MacipACLInterfaceAddDel(swIfIndex, aclIndex uint32, isAdd bool) error
	Close()
}

// macipACLTag 伴随 macip ACL 的稳定 tag（VPP string[64]）。
// 它是**另一类对象**（macip_acl_dump，不进 acl_dump），故不会被 ACL 残渣对账（决策 #321）误报。
const macipACLTag = "nfvis-nonip-permit"

// AclProvider ACL 规则与接口绑定编排。
type AclProvider struct {
	client func() (ACLClient, error)

	mu      sync.Mutex
	index   map[string]uint32  // ACL 名 → VPP acl index
	bound   map[uint32]aclPair // sw_if_index → 绑定的 in/out ACL 名
	byIface map[string]uint32  // 接口名 → sw_if_index（解绑用）
	// 伴随 macip ACL（决策 #341）：进程内的 index 登记与已绑接口集合。
	// 与 index 同属「进程内登记、reset 清空、重放按需重建」的同一风格。
	macipIdx   uint32          // 伴随 macip ACL 的 VPP 索引
	macipKnown bool            // 是否已创建/反查登记
	macipBound map[uint32]bool // sw_if_index → 是否已绑伴随 macip ACL
}

type aclPair struct{ in, out string }

// ACLIndexLookup 可选的「按 tag 反查已存在 ACL 索引」能力（恢复收敛用）：
// VPP 侧可能已有同名 ACL（nfvisd 重启/配置重放），据此走 replace 而非新建重复项。
type ACLIndexLookup interface {
	ACLIndexByTag(tag string) (uint32, bool, error)
}

// MacipIndexLookup 可选的「按 tag 反查已存在 macip ACL 索引」能力（决策 #341，恢复收敛用）：
// nfvisd 重启后进程内登记为空，但 VPP 侧可能已有伴随 macip ACL，据此复用而非重复创建。
type MacipIndexLookup interface {
	MacipACLIndexByTag(tag string) (uint32, bool, error)
}

// reset 清空进程内登记表（恢复收敛前调用，索引改由 acl_dump 按 tag 反查）。
// 伴随 macip 登记一并清空（决策 #341）：避免 nfvisd 重启/重放时用陈旧 index 去绑。
func (p *AclProvider) reset() {
	p.mu.Lock()
	p.index = map[string]uint32{}
	p.bound = map[uint32]aclPair{}
	p.byIface = map[string]uint32{}
	p.macipIdx, p.macipKnown = 0, false
	p.macipBound = map[uint32]bool{}
	p.mu.Unlock()
}

// NewAclProvider 以固定客户端构造（测试）。
func NewAclProvider(c ACLClient) *AclProvider {
	return &AclProvider{client: func() (ACLClient, error) { return c, nil },
		index: map[string]uint32{}, bound: map[uint32]aclPair{}, byIface: map[string]uint32{},
		macipBound: map[uint32]bool{}}
}

// NewAclProviderFunc 以客户端工厂构造（连接可重连）。
func NewAclProviderFunc(f func() (ACLClient, error)) *AclProvider {
	return &AclProvider{client: f, index: map[string]uint32{},
		bound: map[uint32]aclPair{}, byIface: map[string]uint32{}, macipBound: map[uint32]bool{}}
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
		// 恢复收敛：VPP 侧可能已存在同名 ACL，按 tag 反查后走 replace（避免重复项）
		if lk, ok := c.(ACLIndexLookup); ok {
			got, found, err := lk.ACLIndexByTag(acl.Name)
			if err != nil {
				return fmt.Errorf("查询已存在 ACL %s: %w", acl.Name, err)
			}
			if found {
				idx, known = got, true
			}
		}
	}
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
			// 决策 #342：目标接口已不存在（VPP INVALID_SW_IF_INDEX -2）——绑定随接口一起消失，
			// 解绑属**已达成**，不能因此打断整次删除（真机 round117/119/120：整次提交回滚）。
			if isMissingIfaceErr(err) {
				log.Printf("接口 %d 已不存在，按已回收处理（ACL 解绑）", swIf)
				p.mu.Lock()
				delete(p.bound, swIf)
				delete(p.macipBound, swIf)
				p.mu.Unlock()
				continue
			}
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
		return fmt.Errorf("接口 %s 不存在于 VPP"+ifaceMissingHint, ifname)
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
//
// 决策 #341：IP ACL 非空时**自动伴随**一条 macip 白名单（放行全部非 IP 帧，含 ARP）并绑到同一接口
// ——VPP acl plugin 对启用 IP ACL 的接口上的非 IP 帧走 macip 白名单路径，不配就丢 ARP。
// aclIn 与 aclOut 都为空表示**解绑**：IP ACL 与伴随 macip 一并解绑（有登记才动 VPP，幂等）。
// IP ACL 的绑定/顺序/语义不因本次改动发生任何变化。
func (p *AclProvider) BindIndex(c ACLClient, swIfIndex uint32, aclIn, aclOut string) error {
	if aclIn == "" && aclOut == "" {
		p.mu.Lock()
		_, hadPair := p.bound[swIfIndex]
		hadMacip := p.macipBound[swIfIndex]
		p.mu.Unlock()
		if !hadPair && !hadMacip {
			return nil // 无登记即无操作（与既有「空绑定为无操作」一致）
		}
		if err := c.ACLInterfaceSet(swIfIndex, 0, 0, false, false); err != nil {
			// 决策 #342：目标接口已不存在（-2）时绑定随接口一起消失，解绑属已达成。
			if !isMissingIfaceErr(err) {
				return fmt.Errorf("解绑接口 %d 的 ACL: %w", swIfIndex, err)
			}
			log.Printf("接口 %d 已不存在，按已回收处理（ACL 解绑）", swIfIndex)
		} else if hadMacip {
			if err := p.MacipDisallowNonIP(c, swIfIndex); err != nil {
				return err
			}
		}
		p.mu.Lock()
		delete(p.bound, swIfIndex)
		delete(p.macipBound, swIfIndex)
		p.mu.Unlock()
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
	// 伴随绑定（决策 #341）：IP ACL 非空即确保该接口放行非 IP 帧（幂等）。
	if err := p.MacipAllowNonIP(c, swIfIndex); err != nil {
		return err
	}
	p.mu.Lock()
	p.bound[swIfIndex] = aclPair{in: aclIn, out: aclOut}
	p.mu.Unlock()
	return nil
}

// MacipAllowNonIP 确保接口上有一条第 #341 的伴随 macip ACL（放行全部非 IP 帧）并绑定之。
//
// 幂等：① 进程内已登记「该接口已绑」→ 空操作；② 进程内无 macip index（首次/恢复收敛）时，
// 先按 tag 反查 VPP 侧既有 macip ACL（nfvisd 重启场景复用，避免重复项），查不到才新建；
// ③ 绑定收到「已存在」按目标状态处理（跨 nfvisd 重启时 VPP 侧绑定可能仍在）。
// 失败如实上抛（不吞）——绑定不出即为提交失败，走既有补偿。
func (p *AclProvider) MacipAllowNonIP(c ACLClient, swIfIndex uint32) error {
	p.mu.Lock()
	if p.macipKnown && p.macipBound[swIfIndex] {
		p.mu.Unlock()
		return nil
	}
	known, idx := p.macipKnown, p.macipIdx
	p.mu.Unlock()

	if !known {
		if lk, ok := c.(MacipIndexLookup); ok {
			got, found, err := lk.MacipACLIndexByTag(macipACLTag)
			if err != nil {
				return fmt.Errorf("查询已存在 macip ACL: %w", err)
			}
			if found {
				known, idx = true, got
			}
		}
	}
	if !known {
		idx = aclIndexNew // ~0：新建
	}
	newIdx, err := c.MacipACLAddReplace(idx, macipACLTag)
	if err != nil {
		return fmt.Errorf("创建非 IP 放行 macip ACL: %w", err)
	}
	// 先登记 index：即便随后绑定失败，重放也能复用同一 ACL 而不重复创建。
	p.mu.Lock()
	p.macipIdx, p.macipKnown = newIdx, true
	p.mu.Unlock()

	if err := c.MacipACLInterfaceAddDel(swIfIndex, newIdx, true); err != nil {
		if !vppErrIs(err, vppValueExist) { // 已绑定 = 目标状态
			return fmt.Errorf("绑定非 IP 放行 macip ACL 到接口 %d: %w", swIfIndex, err)
		}
	}
	p.mu.Lock()
	p.macipBound[swIfIndex] = true
	p.mu.Unlock()
	return nil
}

// MacipDisallowNonIP 解绑接口上的伴随 macip ACL（幂等：无登记即无操作）。
func (p *AclProvider) MacipDisallowNonIP(c ACLClient, swIfIndex uint32) error {
	p.mu.Lock()
	if !p.macipKnown || !p.macipBound[swIfIndex] {
		p.mu.Unlock()
		return nil
	}
	idx := p.macipIdx
	p.mu.Unlock()
	if err := c.MacipACLInterfaceAddDel(swIfIndex, idx, false); err != nil {
		// 解绑方向：对象本就不在 / 已是目标状态按成功（同 natRemovalBenign 口径）；
		// 决策 #342：目标接口已不存在（-2）时绑定随接口消失，同属已达成。
		if !vppErrIs(err, vppNoSuchEntry, vppValueExist) && !isMissingIfaceErr(err) {
			return fmt.Errorf("解绑接口 %d 的非 IP 放行 macip ACL: %w", swIfIndex, err)
		}
		if isMissingIfaceErr(err) {
			log.Printf("接口 %d 已不存在，按已回收处理（ACL 解绑）", swIfIndex)
		}
	}
	p.mu.Lock()
	delete(p.macipBound, swIfIndex)
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

// ACLTagsInVPP 返回 VPP 里全部 ACL 的 tag（去重、升序）。
//
// 用途（决策 #321）：残渣对账据此找「tag 不在配置里」的 ACL——提交补偿失败留下的残渣。
// 查询失败上抛（把「问不出来」当「没有残渣」是假绿）。
func (p *AclProvider) ACLTagsInVPP() ([]string, error) {
	c, err := p.client()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	tags, err := c.ACLTags()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}

func (p *AclProvider) pairOf(swIf uint32) aclPair {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bound[swIf]
}

// BuildACLRules 把配置模型规则转换为与底座解耦的规则形态。
//
// 决策 #352 家族语义：一条规则只匹配单一地址族——任一侧显式 v6 即整条规则按 v6 下发，
// any/空一侧跟随显式侧取 `::/0`（其余 any/空维持 `0.0.0.0/0` 的既有 v4 行为）；
// `icmp` 协议随规则家族映射（v4=1 / v6=58）。混族规则在校验层（model）已拒绝，
// 此处返回同文案 error 作防御（校验拦截后理论不可达）。
func BuildACLRules(acl model.Acl) ([]ACLRuleSpec, error) {
	out := make([]ACLRuleSpec, 0, len(acl.Rules))
	for _, r := range acl.Rules {
		srcV6, dstV6 := isExplicitV6(r.Source), isExplicitV6(r.Destination)
		if srcV6 != dstV6 && !isAnyAddr(r.Source) && !isAnyAddr(r.Destination) {
			return nil, fmt.Errorf("规则 %d source/destination 地址族不一致（%s 与 %s）：一条规则仅匹配单族，双族需两条规则",
				r.Seq, r.Source, r.Destination)
		}
		v6 := srcV6 || dstV6
		spec := ACLRuleSpec{
			Permit: strings.EqualFold(r.Action, "permit"),
			Src:    prefixOrAny(r.Source, v6),
			Dst:    prefixOrAny(r.Destination, v6),
			Proto:  protoOf(r.Protocol, v6),
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

// isAnyAddr 规则地址字段是否为「any/空」（any/空一侧的家族由显式侧决定，见 #352）。
func isAnyAddr(s string) bool {
	s = strings.TrimSpace(s)
	return s == "" || strings.EqualFold(s, "any")
}

// isExplicitV6 判定规则地址字段是否为**显式** IPv6（非 any/空，且前缀的地址部分解析为 v6；
// any/空与解析失败的值都不是显式 v6）。any/空的家族由调用方按显式侧跟随（决策 #352），
// 本函数只回答「这一侧自己写明了 v6 吗」。
func isExplicitV6(s string) bool {
	if isAnyAddr(s) {
		return false
	}
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() == nil
}

// prefixOrAny 规则地址字段：any/空 → 该规则家族的全零前缀（决策 #352 家族跟随：
// 显式侧是 v6 时 any 侧取 ::/0；其余维持 0.0.0.0/0 的既有 v4 行为）；显式值原样透传。
func prefixOrAny(s string, v6 bool) string {
	if isAnyAddr(s) {
		if v6 {
			return "::/0"
		}
		return "0.0.0.0/0"
	}
	return strings.TrimSpace(s)
}

// protoOf 协议名 → IANA 编号：tcp=6、udp=17；icmp 随规则家族映射（v4=1、v6=58，
// 决策 #352）；icmp6/icmpv6 恒 58；any/未知 → 0（不比较协议）。
func protoOf(p string, v6 bool) uint8 {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "tcp":
		return 6
	case "udp":
		return 17
	case "icmp":
		if v6 {
			return 58
		}
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
