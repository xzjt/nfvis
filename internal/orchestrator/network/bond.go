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

// BondRuntime 数据面现存的 bond（撤销收敛用）。
type BondRuntime struct {
	SwIfIndex uint32
	Name      string
}

// BondClient VPP bond binary API 的最小能力集。
type BondClient interface {
	SwInterfaceIndex(ifname string) (uint32, bool, error)
	Bonds() ([]BondRuntime, error)                      // 数据面现存全部 bond
	BondMembers(bondSwIfIndex uint32) ([]uint32, error) // 某 bond 的成员 sw_if_index
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

// reset 清空进程内登记表（恢复收敛前调用，bond 存在性改按接口名反查）。
func (p *BondProvider) reset() {
	p.mu.Lock()
	p.bonds = map[string]uint32{}
	p.members = map[string][]uint32{}
	p.lacp = map[string]bool{}
	p.mu.Unlock()
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
			return fmt.Errorf("%w: bond %s 成员 %s"+ifaceMissingHint, ErrIfaceUnavailable, bond.Name, m)
		}
		memberIdx = append(memberIdx, idx)
	}

	p.mu.Lock()
	idx, exists := p.bonds[bond.Name]
	oldLacp := p.lacp[bond.Name]
	oldMembers := p.members[bond.Name]
	p.mu.Unlock()

	if !exists {
		// 恢复收敛：VPP 侧可能已存在同名 bond（登记表已清空）。模式无法反查，
		// 删除后重建，保证与配置一致。
		if stale, ok, err := c.SwInterfaceIndex(bond.Name); err != nil {
			return fmt.Errorf("查询 bond %s: %w", bond.Name, err)
		} else if ok {
			if err := c.BondDelete(stale); err != nil {
				return fmt.Errorf("清理已存在的 bond %s: %w", bond.Name, err)
			}
			oldMembers = nil
		}
	}

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

// DeleteBond 删除 bond：先摘除成员（成员退回普通口，其在 bridge-domain/L3 的归属由 VPP
// 保留），再删除 bond 接口本身。登记表未命中时按接口名反查数据面，使删除不依赖
// 「本进程创建过该 bond」（nfvisd 重启后登记表为空，配置删除仍须落到数据面）。
func (p *BondProvider) DeleteBond(ctx context.Context, name string) error {
	p.mu.Lock()
	idx, ok := p.bonds[name]
	members := append([]uint32{}, p.members[name]...)
	p.mu.Unlock()

	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()

	if !ok {
		found, exists, err := c.SwInterfaceIndex(name)
		if err != nil {
			return fmt.Errorf("查询 bond %s: %w", name, err)
		}
		if !exists {
			return nil
		}
		idx = found
	}
	// 成员一律以数据面为准（登记表可能陈旧或缺项），登记表命中的成员并入以防漏摘。
	if live, err := c.BondMembers(idx); err != nil {
		return fmt.Errorf("枚举 bond %s 成员: %w", name, err)
	} else {
		members = unionU32(members, live)
	}
	for _, m := range members {
		if err := c.BondDetachMember(m); err != nil {
			return fmt.Errorf("摘除 bond %s 成员 %d: %w", name, m, err)
		}
	}
	if err := c.BondDelete(idx); err != nil {
		return fmt.Errorf("删除 bond %s: %w", name, err)
	}
	p.forget(name)
	return nil
}

// PruneBonds 拆除**声明集之外**的 bond（apply 撤销缺口：配置已不再声明，数据面仍在）。
//
// 只创建不删除曾是本编排的缺口：`delete bonds <名>` 提交后数据面仍留 BondEthernetX 与
// 成员关系，该物理口处于「既是 bond 成员又是 bridge-domain 成员」的混淆态，且无法在原
// 位置重新声明为普通口，必须重启数据面才消失。本方法把这类残留对象一并拆掉。
//
// 每个待拆 bond 的处置顺序：
//  1. 按数据面枚举出的成员 sw_if_index 逐个 bond_detach_member——成员退回普通口，
//     其在 bridge-domain/L3 的归属由 VPP 保留，无需重放接口配置；
//  2. 再 bond_delete 删除 bond 接口本身。
//
// 声明仍在的 bond 一律跳过（其存在性与参数由 ApplyBond 对齐，不得误删）。
// 成员枚举失败时不冒进删除（状态未知），该项按未收敛上报；单对象失败不阻塞其余。
// 返回未拆除项，调用方转告警。
func (p *BondProvider) PruneBonds(ctx context.Context, declared []model.Bond) []error {
	keep := make(map[string]bool, len(declared))
	for _, b := range declared {
		keep[b.Name] = true
	}

	c, err := p.client()
	if err != nil {
		return []error{err}
	}
	defer c.Close()

	bonds, err := c.Bonds()
	if err != nil {
		return []error{fmt.Errorf("查询数据面现存 bond: %w", err)}
	}

	var errs []error
	for _, b := range bonds {
		if keep[b.Name] {
			continue
		}
		if err := removeUndeclaredBond(c, b); err != nil {
			errs = append(errs, fmt.Errorf("拆除未声明的 bond %s: %w", b.Name, err))
			continue
		}
		p.forget(b.Name)
	}
	return errs
}

// removeUndeclaredBond 拆除一个未声明的 bond：先摘成员，再删接口。
func removeUndeclaredBond(c BondClient, b BondRuntime) error {
	members, err := c.BondMembers(b.SwIfIndex)
	if err != nil {
		return fmt.Errorf("枚举成员: %w", err)
	}
	for _, m := range members {
		if err := c.BondDetachMember(m); err != nil {
			return fmt.Errorf("摘除成员 %d: %w", m, err)
		}
	}
	if err := c.BondDelete(b.SwIfIndex); err != nil {
		return fmt.Errorf("删除接口: %w", err)
	}
	return nil
}

// forget 清掉进程内登记（bond 在数据面已不存在）。
func (p *BondProvider) forget(name string) {
	p.mu.Lock()
	delete(p.bonds, name)
	delete(p.members, name)
	delete(p.lacp, name)
	p.mu.Unlock()
}

func unionU32(a, b []uint32) []uint32 {
	out := append([]uint32{}, a...)
	for _, v := range b {
		if !containsU32(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func containsU32(s []uint32, v uint32) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
