package network

// M3-3：L2 编排（FR-NET-010~012/015/016）。
//
// 映射（附录 B）：虚拟交换机 type=l2 → bridge domain；端口 → sw_interface_set_l2_bridge；
// cross-connect → sw_interface_set_l2_xconnect；access/trunk VLAN → create_subif；
// MAC 学习表 → l2_fib_table_details。
//
// 底座调用藏在 L2Client 接口后（govpp 实现见 l2_govpp.go），单测用假实现。
// BD ID 由交换机名确定性派生（附录 A #31），保证恢复收敛可重放。
//
// 成员摘除：govpp v0.13.0 的 sw_interface_details 无 bd_id 字段，无法直接 dump
// BD 成员，故本 provider 维护进程内挂接登记表用于「删端口后同步」；跨进程的完整
// 收敛由 M3-8 恢复收敛负责。

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"

	"github.com/xzjt/nfvis/internal/model"
)

// L2 端口类型（对应 VPP L2_API_PORT_TYPE_*）。
type L2PortType uint32

const (
	L2PortNormal L2PortType = 0
	L2PortBVI    L2PortType = 1
	L2PortUUFwd  L2PortType = 2
)

// MACEntry 一条 L2 FIB 表项（客户端原始形态）。
type MACEntry struct {
	MAC       string `json:"mac"`
	SwIfIndex uint32 `json:"sw_if_index"`
	Static    bool   `json:"static,omitempty"`
	BVI       bool   `json:"bvi,omitempty"`
}

// SwIfInfo sw_if_index → 接口名与外包 VLAN（子接口）。
type SwIfInfo struct {
	Name        string
	OuterVlanID uint16
}

// MACTableEntry MAC 学习表对外形态（契约 /virtual-switches/{name}/mac-table）。
type MACTableEntry struct {
	MAC  string `json:"mac"`
	Port string `json:"port"`
	VLAN int    `json:"vlan"`
}

// CreateSubifReq 创建 VLAN 子接口（VPP create_subif）。
type CreateSubifReq struct {
	ParentSwIfIndex uint32
	SubID           uint32
	OuterVlanID     uint16
	OneTag          bool
	ExactMatch      bool
	DefaultSub      bool
}

// VlanTagRewriteReq VLAN tag rewrite（VPP l2_interface_vlan_tag_rewrite）。
type VlanTagRewriteReq struct {
	SwIfIndex uint32
	Op        uint32 // 0=disabled 1=push1 2=push2 3=pop1 4=pop2 5=translate1 6=translate2
	PushDot1Q uint32
	Tag1      uint32
}

// L2Client VPP L2 binary API 的最小能力集（govpp 适配/单测假实现）。
type L2Client interface {
	SwInterfaceIndex(ifname string) (uint32, bool, error)
	SwInterfaceNames() (map[uint32]SwIfInfo, error)
	BridgeDomainAddDel(bdID uint32, add, learn bool, tag string) error
	SwInterfaceSetL2Bridge(swIfIndex, bdID uint32, portType L2PortType, shg uint8, enable bool) error
	SwInterfaceSetL2Xconnect(swIfIndex, bdID uint32, enable bool) error
	CreateSubif(req CreateSubifReq) (uint32, error)
	L2InterfaceVlanTagRewrite(req VlanTagRewriteReq) error
	MACTable(bdID uint32) ([]MACEntry, error)
	Close()
}

// BDID 由交换机名确定性派生 BD ID（24 位有效范围，0 保留）。
// 同一名字恒等，便于恢复收敛重放且不依赖配置顺序（附录 A #31）。
func BDID(name string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	id := h.Sum32() & 0xFFFFFF
	if id == 0 {
		id = 1
	}
	return id
}

// attachment 一条已挂接记录（用于成员同步）。
type attachment struct {
	xconnect bool
}

// L2Provider 实现 L2 虚拟交换机的编排。
type L2Provider struct {
	// client 每次操作获取一个 L2Client（govpp 侧按需开 API channel；测试注入假实现）。
	client func() (L2Client, error)

	mu       sync.Mutex
	attached map[uint32]map[uint32]attachment // bdID → swIfIndex → 挂接方式
}

// NewL2Provider 以固定客户端构造（测试/单连接场景）。
func NewL2Provider(c L2Client) *L2Provider {
	return &L2Provider{client: func() (L2Client, error) { return c, nil }, attached: map[uint32]map[uint32]attachment{}}
}

// NewL2ProviderFunc 以客户端工厂构造（连接可能重连时使用）。
func NewL2ProviderFunc(f func() (L2Client, error)) *L2Provider {
	return &L2Provider{client: f, attached: map[uint32]map[uint32]attachment{}}
}

// ApplyBridgeDomain 把 L2 虚拟交换机收敛到 bridge domain：建 BD、挂接目标端口
// （access/trunk VLAN 经子接口），并摘除不再属于该 BD 的成员。
func (p *L2Provider) ApplyBridgeDomain(ctx context.Context, vs model.VirtualSwitch) error {
	if vs.Type != "l2" {
		return nil // L3 交换机经同名 VRF 编排（附录 B）
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	bdID := BDID(vs.Name)
	if err := c.BridgeDomainAddDel(bdID, true, true, vs.Name); err != nil {
		return fmt.Errorf("建 bridge-domain %s(id=%d): %w", vs.Name, bdID, err)
	}

	desired, err := p.desiredMembers(c, vs)
	if err != nil {
		return err
	}

	// 摘除不再需要的成员（配置删端口后同步）
	stale := p.takeStale(bdID, desired)
	sort.Slice(stale, func(i, j int) bool { return stale[i] < stale[j] })
	for _, idx := range stale {
		if err := c.SwInterfaceSetL2Bridge(idx, bdID, L2PortNormal, 0, false); err != nil {
			return fmt.Errorf("摘除成员 %d: %w", idx, err)
		}
		if err := c.SwInterfaceSetL2Xconnect(idx, 0, false); err != nil {
			return fmt.Errorf("解除 cross-connect %d: %w", idx, err)
		}
	}

	idxs := make([]int, 0, len(desired))
	for idx := range desired {
		idxs = append(idxs, int(idx))
	}
	sort.Ints(idxs)
	for _, idx := range idxs {
		if err := desired[uint32(idx)](c); err != nil {
			return err
		}
	}
	return nil
}

// DeleteBridgeDomain 删除 BD：先摘除登记成员，再删 BD（FR-NET-016）。
func (p *L2Provider) DeleteBridgeDomain(ctx context.Context, name string) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	bdID := BDID(name)
	prev := p.takeAll(bdID)
	idxs := make([]int, 0, len(prev))
	for idx := range prev {
		idxs = append(idxs, int(idx))
	}
	sort.Ints(idxs)
	for _, idx := range idxs {
		if err := c.SwInterfaceSetL2Bridge(uint32(idx), bdID, L2PortNormal, 0, false); err != nil {
			return fmt.Errorf("摘除成员 %d: %w", idx, err)
		}
		if err := c.SwInterfaceSetL2Xconnect(uint32(idx), 0, false); err != nil {
			return fmt.Errorf("解除 cross-connect %d: %w", idx, err)
		}
	}
	if err := c.BridgeDomainAddDel(bdID, false, false, name); err != nil {
		return fmt.Errorf("删 bridge-domain %s(id=%d): %w", name, bdID, err)
	}
	return nil
}

// MACTable 返回 BD 的 MAC 学习表（FR-NET-015），已解析为接口名/VLAN。
func (p *L2Provider) MACTable(ctx context.Context, name string) ([]MACTableEntry, error) {
	c, err := p.client()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	rows, err := c.MACTable(BDID(name))
	if err != nil {
		return nil, err
	}
	names, err := c.SwInterfaceNames()
	if err != nil {
		return nil, err
	}
	out := make([]MACTableEntry, 0, len(rows))
	for _, r := range rows {
		info := names[r.SwIfIndex]
		out = append(out, MACTableEntry{MAC: r.MAC, Port: info.Name, VLAN: int(info.OuterVlanID)})
	}
	return out, nil
}

// takeStale 记录本次挂接集合，返回需摘除的旧成员。
func (p *L2Provider) takeStale(bdID uint32, desired map[uint32]func(L2Client) error) []uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var stale []uint32
	if prev := p.attached[bdID]; prev != nil {
		for idx := range prev {
			if _, ok := desired[idx]; !ok {
				stale = append(stale, idx)
			}
		}
	}
	next := make(map[uint32]attachment, len(desired))
	for idx := range desired {
		next[idx] = attachment{}
	}
	p.attached[bdID] = next
	return stale
}

func (p *L2Provider) takeAll(bdID uint32) map[uint32]attachment {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := p.attached[bdID]
	delete(p.attached, bdID)
	return prev
}

// desiredMembers 构造目标成员集合：sw_if_index → 挂接动作。
// cross-connect 交换机只挂前两个端口（点对点，FR-NET-012）。
func (p *L2Provider) desiredMembers(c L2Client, vs model.VirtualSwitch) (map[uint32]func(L2Client) error, error) {
	attached := map[uint32]func(L2Client) error{}
	bdID := BDID(vs.Name)
	if vs.CrossConnect {
		var idxs []uint32
		for _, port := range vs.Ports {
			idx, err := p.portIndex(c, vs, port)
			if err != nil {
				return nil, err
			}
			idxs = append(idxs, idx)
		}
		if len(idxs) > 2 {
			return nil, fmt.Errorf("cross-connect 交换机 %s 仅支持两个端口，实际 %d", vs.Name, len(idxs))
		}
		if len(idxs) == 2 {
			a, b := idxs[0], idxs[1]
			attached[a] = func(c L2Client) error { return c.SwInterfaceSetL2Xconnect(a, b, true) }
			attached[b] = func(c L2Client) error { return c.SwInterfaceSetL2Xconnect(b, a, true) }
		}
		return attached, nil
	}
	for _, port := range vs.Ports {
		if port.Vnf != "" || port.Container != "" {
			return nil, fmt.Errorf("端口 %s 引用 VNF/容器接口，属 M4（当前仅支持物理口/bond）", portKey(port))
		}
		portIdx, err := p.portIndex(c, vs, port)
		if err != nil {
			return nil, err
		}
		switch {
		case vs.VlanAccess > 0: // access：每个端口建 VLAN 子接口后挂接（FR-NET-011）
			sub, err := c.CreateSubif(CreateSubifReq{
				ParentSwIfIndex: portIdx, SubID: uint32(vs.VlanAccess),
				OuterVlanID: uint16(vs.VlanAccess), OneTag: true,
			})
			if err != nil {
				return nil, fmt.Errorf("创建 access 子接口 %s vlan %d: %w", portKey(port), vs.VlanAccess, err)
			}
			attached[sub] = func(c L2Client) error {
				return c.SwInterfaceSetL2Bridge(sub, bdID, L2PortNormal, 0, true)
			}
		case len(port.TrunkVlans) > 0: // trunk：tagged VID 建子接口，native 挂物理口
			for _, vid := range port.TrunkVlans {
				sub, err := c.CreateSubif(CreateSubifReq{
					ParentSwIfIndex: portIdx, SubID: uint32(vid),
					OuterVlanID: uint16(vid), OneTag: true, ExactMatch: true,
				})
				if err != nil {
					return nil, fmt.Errorf("创建 trunk 子接口 %s vlan %d: %w", portKey(port), vid, err)
				}
				attached[sub] = func(c L2Client) error {
					return c.SwInterfaceSetL2Bridge(sub, bdID, L2PortNormal, 0, true)
				}
			}
			if port.NativeVlan > 0 {
				attached[portIdx] = func(c L2Client) error {
					return c.SwInterfaceSetL2Bridge(portIdx, bdID, L2PortNormal, 0, true)
				}
			}
		default:
			attached[portIdx] = func(c L2Client) error {
				return c.SwInterfaceSetL2Bridge(portIdx, bdID, L2PortNormal, 0, true)
			}
		}
	}
	return attached, nil
}

func (p *L2Provider) portIndex(c L2Client, vs model.VirtualSwitch, port model.VSwitchPort) (uint32, error) {
	if port.Interface == "" {
		return 0, fmt.Errorf("交换机 %s 端口 %d 未指定接口", vs.Name, port.Seq)
	}
	idx, ok, err := c.SwInterfaceIndex(port.Interface)
	if err != nil {
		return 0, fmt.Errorf("解析接口 %s 的 sw_if_index: %w", port.Interface, err)
	}
	if !ok {
		return 0, fmt.Errorf("接口 %s 不存在于 VPP（是否未由 DPDK 接管？）", port.Interface)
	}
	return idx, nil
}

func portKey(port model.VSwitchPort) string {
	if port.Interface != "" {
		return port.Interface
	}
	if port.Vnf != "" {
		return port.Vnf + ":" + port.VnfInterface
	}
	if port.Container != "" {
		return port.Container + ":" + port.ContainerInterface
	}
	return fmt.Sprintf("#%d", port.Seq)
}

// ErrL2Unavailable 未连接 VPP 时 L2 客户端不可用。
var ErrL2Unavailable = errors.New("VPP 未连接，L2 客户端不可用")
