package network

// M3-5（三）：NAT44 源转换与静态映射（VPP nat44_ei 插件，§4.3）。
//
// ApplyNAT 为声明式全量收敛：地址池（nat44_ei_add_del_address_range）、静态映射
// （nat44_ei_add_del_static_mapping）、接口 inside/outside 特性
// （nat44_ei_interface_add_del_feature），按登记表增删补齐。
// 地址池随 **inside（租户）转发域** 下发与删除，登记里连 VRF 一起记住（见 natPool）。
//
// 内外口来源：rule.action.interface → outside；rule.virtual_switch 的 L3 交换机成员接口
// → inside（经注入的 L3Provider.AttachedIfaces 解析，见 l3.go 的登记口径）。两处解析都是
// **派生查询**，其登记可能被 InvalidateRuntimeState 清掉，故：插件开关按配置声明判定
// （空解析不得关闭插件），且恢复收敛在 ApplyNAT 之前按配置重建登记（recovery.go）。

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/xzjt/nfvis/internal/model"
)

// NATSession 一条 NAT44 会话（运行态）。
type NATSession struct {
	InsideIP    string `json:"inside_ip"`
	InsidePort  int    `json:"inside_port"`
	OutsideIP   string `json:"outside_ip"`
	OutsidePort int    `json:"outside_port"`
	Protocol    int    `json:"protocol"`
	Bytes       uint64 `json:"bytes"`
	Packets     uint32 `json:"packets"`
}

// NatClient VPP nat44_ei binary API 的最小能力集。
type NatClient interface {
	SwInterfaceIndex(ifname string) (uint32, bool, error)
	NATAddressRange(add bool, first, last string, vrfID uint32) error
	NATAddressVRFs() (map[string]uint32, error)
	NATFeature(swIfIndex uint32, inside, add bool) error
	NATEnable(enable bool, insideVRF, outsideVRF uint32) error
	NATInterfaceAddr(add bool, swIfIndex uint32) error
	NATStatic(add bool, inside, outside string) error
	NATSessions() ([]NATSession, error)
	Close()
}

// natPool 一个地址池的登记：地址范围 + 下发时所用的转发域（VRF tableID）。
//
// VRF 必须随池一起记住（round84 缺陷 A / R84-24）：
//   - **下发**：nat44_ei_add_del_address_range 的 vrf_id 会被 VPP 折算成该池地址的 FIB 索引
//     （nat44_ei_add_address: ap->fib_index = fib_table_find_or_create_and_lock(IP4, vrf_id)），
//     而 in2out 慢路径分配端口时只认「fib_index == 入接口所在表」的池地址
//     （nat44_ei_alloc_default_cb 的 a->fib_index == rx_fib_index），或 fib_index == ~0 的
//     「tenant VRF independent」地址（那是 nat44_ei_add_del_interface_addr 的形态）。
//     故池必须落在 **inside（租户）转发域**——即规则 virtual-switch 的那张表：
//     落在 outside 表或默认表时与入接口的表对不上，包进了 NAT 却分配不出端口
//     （`show errors` 见 nat44-ei-in2out-slowpath out of ports、`show nat44 ei addresses`
//     里该地址 0 busy ports、会话数恒为 0），全程无报错。
//   - **换域**：VPP 的地址池按地址唯一（重复下发报 -81，与 VRF 无关），改域必须先用**登记时**
//     的旧域删掉再按新域下发，故登记不能只存地址范围。
type natPool struct {
	first, last string
	vrf         uint32
}

// NatProvider NAT44 编排。
type NatProvider struct {
	client       func() (NatClient, error)
	insideIfaces func(vsName string) []uint32       // 注入：交换机成员 sw_if_index（可空）
	outsideTable func(ifname string) (uint32, bool) // 注入：接口所属 VRF tableID（决策 #52）

	mu         sync.Mutex
	pools      map[string]natPool // 池名 → {范围, 下发时所用 VRF}
	statics    map[string]string  // insideIP → outsideIP
	features   map[uint32]string  // swIfIndex → inside|outside
	ifaddr     map[uint32]bool    // 使用接口地址做 NAT 的外口
	enabled    bool               // nat44_ex 插件特性是否已启用（会话查询前置）
	insideVRF  uint32             // 决策 #52：生效的 inside/outside 转发域
	outsideVRF uint32
}

// NewNatProvider 以固定客户端构造（测试）。
func NewNatProvider(c NatClient) *NatProvider {
	return &NatProvider{client: func() (NatClient, error) { return c, nil },
		pools: map[string]natPool{}, statics: map[string]string{},
		features: map[uint32]string{}, ifaddr: map[uint32]bool{}}
}

// NewNatProviderFunc 以客户端工厂构造（连接可重连）。
func NewNatProviderFunc(f func() (NatClient, error)) *NatProvider {
	return &NatProvider{client: f, pools: map[string]natPool{},
		statics: map[string]string{}, features: map[uint32]string{}, ifaddr: map[uint32]bool{}}
}

// SetInsideResolver 注入 L3 交换机（VRF）成员接口解析（inside 接口来源）。
func (p *NatProvider) SetInsideResolver(fn func(vsName string) []uint32) { p.insideIfaces = fn }

// SetOutsideResolver 注入「接口 → 所属 VRF tableID」解析（outside 转发域来源，决策 #52）。
func (p *NatProvider) SetOutsideResolver(fn func(ifname string) (uint32, bool)) { p.outsideTable = fn }

// reset 清空进程内登记表（恢复收敛前调用，使 ApplyNAT 全量重放）。
func (p *NatProvider) reset() {
	p.mu.Lock()
	p.pools = map[string]natPool{}
	p.statics = map[string]string{}
	p.features = map[uint32]string{}
	p.ifaddr = map[uint32]bool{}
	p.enabled = false
	p.insideVRF, p.outsideVRF = 0, 0
	p.mu.Unlock()
}

// ForgetIface 摘除某接口的 NAT 登记（接口已从数据面消失时调用；幂等）。
//
// 只摘登记、不触碰 VPP：接口都没了，其上的 inside/outside 特性与「用接口地址做外部地址」
// 也随之消失；残留登记会让下一次 ApplyNAT 去删一个已不存在的接口（VPP 报 -6 No such entry），
// 而一次无害的重复删除会把整批 apply 打回滚（round84 缺陷 B）。重新声明后 ApplyNAT 会按
// 期望集重建（add 方向的 -81 按成功处理）。
func (p *NatProvider) ForgetIface(swIfIndex uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.features, swIfIndex)
	delete(p.ifaddr, swIfIndex)
}

// ApplyNAT 声明式收敛 NAT44 配置。
func (p *NatProvider) ApplyNAT(ctx context.Context, nat model.NatConfig) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()

	desiredPools := map[string]natPool{}
	for _, sp := range nat.SourcePools {
		first, last, err := parseAddressRange(sp.AddressRange)
		if err != nil {
			return fmt.Errorf("NAT 源池 %s: %w", sp.Name, err)
		}
		desiredPools[sp.Name] = natPool{first: first, last: last}
	}
	desiredStatics := map[string]string{}
	for _, st := range nat.Static {
		desiredStatics[st.InsideIP] = st.OutsideIP
	}
	desiredFeatures, desiredIfAddr, insideVRF, outsideVRF, err := p.desiredFeatures(c, nat, desiredPools)
	if err != nil {
		return err
	}
	// 地址池落在 **inside（租户）转发域**里——即规则 virtual-switch 的那张表（决策 #52 的
	// inside VRF；未归属任何 VRF → 默认表 0）。VPP 把该 vrf_id 折算成池地址的 FIB 索引，
	// in2out 慢路径只从「与入接口（inside）同一张表」的池地址里分配端口：
	// 池落在 outside 表上时两者对不上 → out of ports、会话恒为 0（round84 R84-24 真机实测：
	// 同一地址分别按 outside / inside 下发，前者 0 busy ports、后者立刻建起会话）。
	for name, pool := range desiredPools {
		pool.vrf = insideVRF
		desiredPools[name] = pool
	}
	// 插件启用/关闭按**配置声明**判定，而不是按解析结果：解析为空只说明「这一轮没有可下发的
	// 特性」（进程内登记刚失效、接口还没落地、vNIC 尚未接入……），不等于「没有 NAT 配置」。
	// 两个判据必须分开：一旦按解析结果判定，「解析来源缺失」就会走到
	// nat44_ei_plugin_enable_disable(false)，VPP 侧 `show nat44 ei interfaces`/`addresses`
	// 随即全空，而配置里规则/交换机/地址都在、日志也可能没有一条未收敛项
	//（真机实测：restart vpp / restart nfvis 之后）。此处以声明为唯一判据，解析只决定「下发哪些」。
	declared := len(nat.Rules) > 0 || len(nat.SourcePools) > 0 || len(nat.Static) > 0

	// 插件启用/关闭：仅在状态或 inside/outside 转发域变化时下发（幂等重放不重复请求）。
	p.mu.Lock()
	wasEnabled, oldInside, oldOutside := p.enabled, p.insideVRF, p.outsideVRF
	p.mu.Unlock()
	if declared {
		if wasEnabled && (oldInside != insideVRF || oldOutside != outsideVRF) {
			// VPP 不允许在启用状态下切换转发域，先关再开。
			if err := c.NATEnable(false, oldInside, oldOutside); err != nil {
				return fmt.Errorf("切换 NAT44 EI 转发域前关闭失败: %w", err)
			}
			wasEnabled = false
		}
		if !wasEnabled {
			if err := c.NATEnable(true, insideVRF, outsideVRF); err != nil {
				return fmt.Errorf("启用 NAT44 EI（inside-vrf %d outside-vrf %d）: %w", insideVRF, outsideVRF, err)
			}
		}
	} else if wasEnabled {
		if err := c.NATEnable(false, oldInside, oldOutside); err != nil {
			return fmt.Errorf("关闭 NAT44 EI: %w", err)
		}
	}
	p.mu.Lock()
	p.enabled, p.insideVRF, p.outsideVRF = declared, insideVRF, outsideVRF
	p.mu.Unlock()

	// 地址池：删旧/改值/新增。删除必须用**登记时**的 VRF（池落在哪张表只有登记知道）：
	// 换域时若省掉这一步，VPP 会以「地址已存在」拒绝新域的下发（按地址唯一，与 VRF 无关），
	// 池就永远停在旧表上——而配置与 show nat 看起来完全正常。
	p.mu.Lock()
	oldPools := p.pools
	p.mu.Unlock()
	for name, pool := range oldPools {
		if want, ok := desiredPools[name]; !ok || want != pool {
			if err := c.NATAddressRange(false, pool.first, pool.last, pool.vrf); err != nil {
				return fmt.Errorf("删除 NAT 地址池 %s（VRF %d）: %w", name, pool.vrf, err)
			}
		}
	}
	// 期望里有、登记里没有（冷启动）或与登记不同：下发前先确认 VPP 侧同一地址不在**别的**转发域里。
	// 池地址在 VPP 里按**地址**唯一（与 VRF 无关），而 add 命中「已存在」又按幂等容忍（重放安全）——
	// 于是旧版本（1.1.42 把池下发到 outside 表）或人工 vppctl 留下的同一地址副本永远纠不回来，
	// in2out 也就永远分配不出端口（R84-24 的现场正是如此）。登记为空时读回真实 VRF，只删真不一致的。
	var vppPools map[string]uint32
	vppPoolsRead := false
	for name, pool := range desiredPools {
		if old, ok := oldPools[name]; ok && old == pool {
			continue
		}
		if _, known := oldPools[name]; !known {
			if !vppPoolsRead {
				vppPoolsRead = true
				got, err := c.NATAddressVRFs()
				if err != nil {
					return fmt.Errorf("读取 NAT 地址池的转发域: %w", err)
				}
				vppPools = got
			}
			if cur, ok := vppPools[pool.first]; ok && cur != pool.vrf {
				if err := c.NATAddressRange(false, pool.first, pool.last, cur); err != nil {
					return fmt.Errorf("删除 NAT 地址池 %s 在转发域 %d 的旧副本: %w", name, cur, err)
				}
			}
		}
		if err := c.NATAddressRange(true, pool.first, pool.last, pool.vrf); err != nil {
			return fmt.Errorf("下发 NAT 地址池 %s（VRF %d）: %w", name, pool.vrf, err)
		}
	}

	// 接口特性：删旧/翻转/新增
	p.mu.Lock()
	oldFeat := p.features
	p.mu.Unlock()

	// inside 期望集不可信时的处理（见 insideUnresolved）：只补齐、不删除既有 inside 特性，
	// 并把它们留在登记里——下一轮解析健康时再按规则决定去留。空解析下删掉 inside 特性
	// 正是「NAT 被整体关掉」的另一半形态（VPP 侧 inside 口瞬间清零，而配置与 show 全正常）。
	keepInside := map[uint32]string{}
	if p.insideUnresolved(nat) {
		for idx, dir := range oldFeat {
			if _, want := desiredFeatures[idx]; !want && dir == "inside" {
				keepInside[idx] = dir
			}
		}
	}
	for idx, dir := range oldFeat {
		if _, hold := keepInside[idx]; hold {
			continue
		}
		if want, ok := desiredFeatures[idx]; !ok || want != dir {
			if err := c.NATFeature(idx, dir == "inside", false); err != nil {
				return fmt.Errorf("移除接口 %d 的 NAT %s 特性: %w", idx, dir, err)
			}
		}
	}
	for idx, dir := range desiredFeatures {
		if old, ok := oldFeat[idx]; ok && old == dir {
			continue
		}
		if err := c.NATFeature(idx, dir == "inside", true); err != nil {
			return fmt.Errorf("配置接口 %d 的 NAT %s 特性: %w", idx, dir, err)
		}
	}

	// 接口地址 NAT（action.interface：使用外口自身地址）
	p.mu.Lock()
	oldIfAddr := p.ifaddr
	p.mu.Unlock()
	for idx := range oldIfAddr {
		if !desiredIfAddr[idx] {
			if err := c.NATInterfaceAddr(false, idx); err != nil {
				return fmt.Errorf("移除接口 %d 的 NAT 接口地址: %w", idx, err)
			}
		}
	}
	for idx := range desiredIfAddr {
		if oldIfAddr[idx] {
			continue
		}
		if err := c.NATInterfaceAddr(true, idx); err != nil {
			return fmt.Errorf("配置接口 %d 使用 NAT 接口地址: %w", idx, err)
		}
	}

	// 静态映射：删旧/新增
	p.mu.Lock()
	oldStatics := p.statics
	p.mu.Unlock()
	for inside, outside := range oldStatics {
		if want, ok := desiredStatics[inside]; !ok || want != outside {
			if err := c.NATStatic(false, inside, outside); err != nil {
				return fmt.Errorf("删除 NAT 静态映射 %s→%s: %w", inside, outside, err)
			}
		}
	}
	for inside, outside := range desiredStatics {
		if old, ok := oldStatics[inside]; ok && old == outside {
			continue
		}
		if err := c.NATStatic(true, inside, outside); err != nil {
			return fmt.Errorf("下发 NAT 静态映射 %s→%s: %w", inside, outside, err)
		}
	}

	p.mu.Lock()
	p.pools, p.statics, p.ifaddr = desiredPools, desiredStatics, desiredIfAddr
	p.features = desiredFeatures
	// 本轮因解析缺失而保留的 inside 特性留在登记里：VPP 侧还在（未删），登记也必须记得，
	// 否则下一轮就没有依据把它删掉（登记丢项 = 残留永不收敛）。
	for idx, dir := range keepInside {
		p.features[idx] = dir
	}
	p.mu.Unlock()
	return nil
}

// insideUnresolved 报告「规则声明了 inside 来源（virtual-switch），但该交换机解析不出任何成员接口」。
//
// 这是**解析来源缺失**的信号（L3 侧登记尚未重建/被失效、该交换机的口还没进数据面），
// 不是「配置里把 inside 口删了」：两者在期望集上都表现为「没有 inside」，但对 VPP 的动作截然相反
// （前者不能动、后者要删）。缺了这一层判别，一次登记缺失就会被当成配置变更，把既有 inside 特性
// 删掉——真机实测的「restart vpp/nfvis 后 NAT 整体消失」里至少包含这一环（本包单测可复现该动作）。
func (p *NatProvider) insideUnresolved(nat model.NatConfig) bool {
	for _, r := range nat.Rules {
		if r.VirtualSwitch == "" {
			continue
		}
		if p.insideIfaces == nil || len(p.insideIfaces(r.VirtualSwitch)) == 0 {
			return true
		}
	}
	return false
}

// Sessions 返回 NAT44 会话（运行态）。插件未启用时直接返回空——
// nat44_ei_user_session_dump 在插件未启用时不应答，会阻塞请求。
func (p *NatProvider) Sessions(ctx context.Context) ([]NATSession, error) {
	p.mu.Lock()
	on := p.enabled
	p.mu.Unlock()
	if !on {
		return nil, nil
	}
	c, err := p.client()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.NATSessions()
}

// desiredFeatures 汇总接口 inside/outside 与插件转发域：rule.action.interface → outside；
// rule.virtual_switch 成员 → inside。同一接口不可同时内外。
//
// 返回值 insideVRF/outsideVRF 供 nat44_ei_plugin_enable_disable 使用（决策 #52）：
// insideVRF 由 virtual-switch 确定性派生 TableID(<vs名>)（L3 交换机与同名 Vrf 条目一一对应）；
// outsideVRF 由出接口所属 VRF 派生（未归属 → 默认表 0）。多条规则的 inside/outside 不一致时
// 在校验层已被拒绝，此处再防御性报错。
func (p *NatProvider) desiredFeatures(c NatClient, nat model.NatConfig, pools map[string]natPool) (map[uint32]string, map[uint32]bool, uint32, uint32, error) {
	features := map[uint32]string{}
	ifaddr := map[uint32]bool{}
	insideVRF := uint32(0)
	insideSet := false
	outsideVRF := uint32(0)
	outsideSet := false
	for _, r := range nat.Rules {
		if r.VirtualSwitch == "" {
			continue
		}
		if !insideSet {
			insideVRF, insideSet = TableID(r.VirtualSwitch), true
		} else if insideVRF != TableID(r.VirtualSwitch) {
			return nil, nil, 0, 0, fmt.Errorf(
				"NAT 规则 %d 的 virtual-switch %q 与前一条规则不一致：V1 仅支持单一 inside 转发域",
				r.Seq, r.VirtualSwitch)
		}
		if p.insideIfaces != nil {
			for _, idx := range p.insideIfaces(r.VirtualSwitch) {
				features[idx] = "inside"
			}
		}
	}
	for _, r := range nat.Rules {
		if r.Action.SourcePool != "" {
			if _, ok := pools[r.Action.SourcePool]; !ok {
				return nil, nil, 0, 0, fmt.Errorf("NAT 规则 %d 引用未定义的源池 %s", r.Seq, r.Action.SourcePool)
			}
		}
		if r.Action.Interface == "" {
			// VPP NAT44 的 outside 必须是显式接口（inside 由 virtual-switch 成员解析）。
			// 此前该情形被静默跳过：配置 commit 成功但 NAT 完全不生效（show nat44
			// interfaces 为空、内网不通），排查成本极高。此处改为显式报错。
			return nil, nil, 0, 0, fmt.Errorf(
				"NAT 规则 %d 未指定出接口：需 action interface <ifname>（VPP NAT44 的 outside 不支持自动推断；"+
					"source-pool 仅提供地址池）", r.Seq)
		}
		if r.VirtualSwitch == "" {
			return nil, nil, 0, 0, fmt.Errorf(
				"NAT 规则 %d 未指定 virtual-switch：inside 接口取自该 L3 交换机的成员接口", r.Seq)
		}
		idx, ok, err := c.SwInterfaceIndex(r.Action.Interface)
		if err != nil {
			return nil, nil, 0, 0, fmt.Errorf("解析 NAT 外口 %s: %w", r.Action.Interface, err)
		}
		if !ok {
			return nil, nil, 0, 0, fmt.Errorf("%w: NAT 外口 %s"+ifaceMissingHint, ErrIfaceUnavailable, r.Action.Interface)
		}
		if features[idx] == "inside" {
			return nil, nil, 0, 0, fmt.Errorf("接口 %s 同时被配置为 NAT 内外口", r.Action.Interface)
		}
		features[idx] = "outside"
		// outside 转发域：出接口所属 VRF（未归属任何 VRF → 默认表 0）。
		ovrf := uint32(0)
		if p.outsideTable != nil {
			if t, ok := p.outsideTable(r.Action.Interface); ok {
				ovrf = t
			}
		}
		if !outsideSet {
			outsideVRF, outsideSet = ovrf, true
		} else if outsideVRF != ovrf {
			return nil, nil, 0, 0, fmt.Errorf(
				"NAT 规则 %d 的出接口 %s 与其它规则的 outside 转发域不一致：V1 仅支持单一 outside VRF",
				r.Seq, r.Action.Interface)
		}
		// 未提供 source-pool 的规则以出接口地址作外部地址（nat44_ei add interface address）；
		// 提供 source-pool 时由地址池承担，避免两套外部地址语义混用。
		if r.Action.SourcePool == "" {
			ifaddr[idx] = true
		}
	}
	return features, ifaddr, insideVRF, outsideVRF, nil
}

// parseAddressRange 解析 "<ip> to <ip>"。
func parseAddressRange(s string) (string, string, error) {
	parts := strings.Split(s, " to ")
	if len(parts) != 2 {
		// 单个地址：起止相同
		if strings.TrimSpace(s) != "" && !strings.Contains(s, " ") {
			a := strings.TrimSpace(s)
			return a, a, nil
		}
		return "", "", fmt.Errorf("地址范围格式应为 \"<ip> to <ip>\"，实际 %q", s)
	}
	first, last := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if first == "" || last == "" {
		return "", "", fmt.Errorf("地址范围含空地址: %q", s)
	}
	return first, last, nil
}
