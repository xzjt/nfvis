package network

// M3-5（三）：NAT44 源转换与静态映射（VPP nat44_ei 插件，§4.3）。
//
// ApplyNAT 为声明式全量收敛：地址池（nat44_ei_add_del_address_range）、静态映射
// （nat44_ei_add_del_static_mapping）、接口 inside/outside 特性
// （nat44_ei_interface_add_del_feature），按登记表增删补齐。
//
// 内外口来源：rule.action.interface → outside；rule.virtual_switch 的成员接口
// → inside（经注入的 L2 挂接表解析，NL2 编排在 ApplyBridgeDomain 时登记）。

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
	NATAddressRange(add bool, first, last string) error
	NATFeature(swIfIndex uint32, inside, add bool) error
	NATEnable(enable bool) error
	NATInterfaceAddr(add bool, swIfIndex uint32) error
	NATStatic(add bool, inside, outside string) error
	NATSessions() ([]NATSession, error)
	Close()
}

// NatProvider NAT44 编排。
type NatProvider struct {
	client       func() (NatClient, error)
	insideIfaces func(vsName string) []uint32 // 注入：交换机成员 sw_if_index（可空）

	mu       sync.Mutex
	pools    map[string][2]string // 池名 → {first,last}
	statics  map[string]string    // insideIP → outsideIP
	features map[uint32]string    // swIfIndex → inside|outside
	ifaddr   map[uint32]bool      // 使用接口地址做 NAT 的外口
	enabled  bool                 // nat44_ex 插件特性是否已启用（会话查询前置）
}

// NewNatProvider 以固定客户端构造（测试）。
func NewNatProvider(c NatClient) *NatProvider {
	return &NatProvider{client: func() (NatClient, error) { return c, nil },
		pools: map[string][2]string{}, statics: map[string]string{},
		features: map[uint32]string{}, ifaddr: map[uint32]bool{}}
}

// NewNatProviderFunc 以客户端工厂构造（连接可重连）。
func NewNatProviderFunc(f func() (NatClient, error)) *NatProvider {
	return &NatProvider{client: f, pools: map[string][2]string{},
		statics: map[string]string{}, features: map[uint32]string{}, ifaddr: map[uint32]bool{}}
}

// SetInsideResolver 注入 L3 交换机（VRF）成员接口解析（inside 接口来源）。
func (p *NatProvider) SetInsideResolver(fn func(vsName string) []uint32) { p.insideIfaces = fn }

// reset 清空进程内登记表（恢复收敛前调用，使 ApplyNAT 全量重放）。
func (p *NatProvider) reset() {
	p.mu.Lock()
	p.pools = map[string][2]string{}
	p.statics = map[string]string{}
	p.features = map[uint32]string{}
	p.ifaddr = map[uint32]bool{}
	p.enabled = false
	p.mu.Unlock()
}

// ApplyNAT 声明式收敛 NAT44 配置。
func (p *NatProvider) ApplyNAT(ctx context.Context, nat model.NatConfig) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()

	desiredPools := map[string][2]string{}
	for _, sp := range nat.SourcePools {
		first, last, err := parseAddressRange(sp.AddressRange)
		if err != nil {
			return fmt.Errorf("NAT 源池 %s: %w", sp.Name, err)
		}
		desiredPools[sp.Name] = [2]string{first, last}
	}
	desiredStatics := map[string]string{}
	for _, st := range nat.Static {
		desiredStatics[st.InsideIP] = st.OutsideIP
	}
	desiredFeatures, desiredIfAddr, err := p.desiredFeatures(c, nat, desiredPools)
	if err != nil {
		return err
	}
	nonEmpty := len(desiredPools) > 0 || len(desiredStatics) > 0 || len(desiredFeatures) > 0
	p.mu.Lock()
	p.enabled = nonEmpty
	p.mu.Unlock()
	if nonEmpty {
		if err := c.NATEnable(true); err != nil {
			return fmt.Errorf("启用 NAT44 EI: %w", err)
		}
	}

	// 地址池：删旧/改值/新增
	p.mu.Lock()
	oldPools := p.pools
	p.mu.Unlock()
	for name, rng := range oldPools {
		if want, ok := desiredPools[name]; !ok || want != rng {
			if err := c.NATAddressRange(false, rng[0], rng[1]); err != nil {
				return fmt.Errorf("删除 NAT 地址池 %s: %w", name, err)
			}
		}
	}
	for name, rng := range desiredPools {
		if old, ok := oldPools[name]; ok && old == rng {
			continue
		}
		if err := c.NATAddressRange(true, rng[0], rng[1]); err != nil {
			return fmt.Errorf("下发 NAT 地址池 %s: %w", name, err)
		}
	}

	// 接口特性：删旧/翻转/新增
	p.mu.Lock()
	oldFeat := p.features
	p.mu.Unlock()
	for idx, dir := range oldFeat {
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

	if !nonEmpty {
		if err := c.NATEnable(false); err != nil {
			return fmt.Errorf("关闭 NAT44 EI: %w", err)
		}
	}
	p.mu.Lock()
	p.pools, p.statics, p.features, p.ifaddr = desiredPools, desiredStatics, desiredFeatures, desiredIfAddr
	p.mu.Unlock()
	return nil
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

// desiredFeatures 汇总接口 inside/outside：rule.action.interface → outside；
// rule.virtual_switch 成员 → inside。同一接口不可同时内外。
func (p *NatProvider) desiredFeatures(c NatClient, nat model.NatConfig, pools map[string][2]string) (map[uint32]string, map[uint32]bool, error) {
	features := map[uint32]string{}
	ifaddr := map[uint32]bool{}
	if p.insideIfaces != nil {
		for _, r := range nat.Rules {
			if r.VirtualSwitch == "" {
				continue
			}
			for _, idx := range p.insideIfaces(r.VirtualSwitch) {
				features[idx] = "inside"
			}
		}
	}
	for _, r := range nat.Rules {
		if r.Action.SourcePool != "" {
			if _, ok := pools[r.Action.SourcePool]; !ok {
				return nil, nil, fmt.Errorf("NAT 规则 %d 引用未定义的源池 %s", r.Seq, r.Action.SourcePool)
			}
		}
		if r.Action.Interface == "" {
			continue
		}
		idx, ok, err := c.SwInterfaceIndex(r.Action.Interface)
		if err != nil {
			return nil, nil, fmt.Errorf("解析 NAT 外口 %s: %w", r.Action.Interface, err)
		}
		if !ok {
			return nil, nil, fmt.Errorf("%w: NAT 外口 %s（是否未由 DPDK 接管？）", ErrIfaceUnavailable, r.Action.Interface)
		}
		if features[idx] == "inside" {
			return nil, nil, fmt.Errorf("接口 %s 同时被配置为 NAT 内外口", r.Action.Interface)
		}
		features[idx] = "outside"
		ifaddr[idx] = true
	}
	return features, ifaddr, nil
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
