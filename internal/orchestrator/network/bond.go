package network

// M3-6：bond（链路聚合，FR-NET-017）编排。
//
// 模式映射（附录 A #33）：bond.lacp == nil → 静态聚合（VPP XOR 哈希）；
// lacp.mode = active|passive → VPP LACP，成员被动位按 mode 设置。
// bond 接口名固定为配置名（bond_create 后改接口名），使交换机端口/L3 接口可按名引用。
// 模式无法原地修改，变更即重建。

import (
	"context"
	"fmt"
	"sync"

	"github.com/xzjt/nfvis/internal/model"
)

// BondClient VPP bond binary API 的最小能力集。
type BondClient interface {
	SwInterfaceIndex(ifname string) (uint32, bool, error)
	BondCreate(lacp bool) (uint32, error)
	SetInterfaceName(swIfIndex uint32, name string) error
	BondAddMember(bondSwIfIndex, memberSwIfIndex uint32, passive bool) error
	BondDetachMember(memberSwIfIndex uint32) error
	BondDelete(bondSwIfIndex uint32) error
	SetState(swIfIndex uint32, up bool) error
	SetMTU(swIfIndex, mtu uint32) error
	Close()
}

// BondProvider bond 编排（进程内登记成员/mode 以支持收敛）。
type BondProvider struct {
	client func() (BondClient, error)

	mu      sync.Mutex
	bonds   map[string]uint32   // bond 名 → sw_if_index
	members map[string][]uint32 // bond 名 → 成员 sw_if_index
	lacp    map[string]bool     // bond 名 → 是否 LACP
}

// NewBondProvider 以固定客户端构造（测试）。
func NewBondProvider(c BondClient) *BondProvider {
	return &BondProvider{client: func() (BondClient, error) { return c, nil },
		bonds: map[string]uint32{}, members: map[string][]uint32{}, lacp: map[string]bool{}}
}

// NewBondProviderFunc 以客户端工厂构造（连接可重连）。
func NewBondProviderFunc(f func() (BondClient, error)) *BondProvider {
	return &BondProvider{client: f, bonds: map[string]uint32{},
		members: map[string][]uint32{}, lacp: map[string]bool{}}
}

// ApplyBond 创建/收敛 bond：模式变更或不存在则重建，随后对齐成员。
func (p *BondProvider) ApplyBond(ctx context.Context, bond model.Bond) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	if len(bond.Members) == 0 {
		return fmt.Errorf("bond %s 至少需要一个成员口", bond.Name)
	}
	wantLacp := bond.Lacp != nil
	passive := wantLacp && bond.Lacp.Mode == "passive"

	memberIdx := make([]uint32, 0, len(bond.Members))
	for _, m := range bond.Members {
		idx, ok, err := c.SwInterfaceIndex(m)
		if err != nil {
			return fmt.Errorf("解析 bond 成员 %s: %w", m, err)
		}
		if !ok {
			return fmt.Errorf("bond 成员 %s 不存在于 VPP（是否未由 DPDK 接管？）", m)
		}
		memberIdx = append(memberIdx, idx)
	}

	p.mu.Lock()
	idx, exists := p.bonds[bond.Name]
	oldLacp := p.lacp[bond.Name]
	oldMembers := p.members[bond.Name]
	p.mu.Unlock()

	if exists && oldLacp != wantLacp { // 模式不可原地改，重建
		if err := c.BondDelete(idx); err != nil {
			return fmt.Errorf("重建 bond %s（删除旧实例）: %w", bond.Name, err)
		}
		exists = false
		delete(p.bonds, bond.Name)
	}
	if !exists {
		idx, err = c.BondCreate(wantLacp)
		if err != nil {
			return fmt.Errorf("创建 bond %s: %w", bond.Name, err)
		}
		if err := c.SetInterfaceName(idx, bond.Name); err != nil {
			return fmt.Errorf("命名 bond 接口 %s: %w", bond.Name, err)
		}
		oldMembers = nil
	}

	// 先摘除不再属于该 bond 的成员
	for _, m := range oldMembers {
		if !containsU32(memberIdx, m) {
			if err := c.BondDetachMember(m); err != nil {
				return fmt.Errorf("摘除 bond %s 成员 %d: %w", bond.Name, m, err)
			}
		}
	}
	// 再添加/更新成员
	for _, m := range memberIdx {
		if containsU32(oldMembers, m) {
			continue
		}
		if err := c.BondAddMember(idx, m, passive); err != nil {
			return fmt.Errorf("添加 bond %s 成员 %d: %w", bond.Name, m, err)
		}
	}
	if bond.MTU > 0 {
		if err := c.SetMTU(idx, uint32(bond.MTU)); err != nil {
			return fmt.Errorf("设置 bond %s MTU: %w", bond.Name, err)
		}
	}
	for _, m := range memberIdx {
		_ = c.SetState(m, true) // 成员置 up；从属状态由 VPP 管理
	}
	if err := c.SetState(idx, true); err != nil {
		return fmt.Errorf("启用 bond %s: %w", bond.Name, err)
	}

	p.mu.Lock()
	p.bonds[bond.Name], p.members[bond.Name], p.lacp[bond.Name] = idx, memberIdx, wantLacp
	p.mu.Unlock()
	return nil
}

// DeleteBond 删除 bond（成员随 bond 一并释放）。
func (p *BondProvider) DeleteBond(ctx context.Context, name string) error {
	p.mu.Lock()
	idx, ok := p.bonds[name]
	delete(p.bonds, name)
	delete(p.members, name)
	delete(p.lacp, name)
	p.mu.Unlock()
	if !ok {
		return nil
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.BondDelete(idx); err != nil {
		return fmt.Errorf("删除 bond %s: %w", name, err)
	}
	return nil
}

func containsU32(s []uint32, v uint32) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
