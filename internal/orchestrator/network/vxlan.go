package network

// VXLAN overlay（决策 #383，FR-NET-019）：v1 只做**单播 remote、IPv4 下垫层、L2 成员**。
//
// 与 #359 的 DHCP tap 同族教训：**恢复重放不靠进程内登记、不靠接口名**——VPP 用 instance
// 生成接口名（vxlan_tunnelN），产品不依赖其名；存量匹配按 `vxlan_tunnel_dump` 的
// (vni, src, dst, dst_port) 元组（本文件 VxlanTupleKey）。
//
// 生命周期口径（与 QoS/SPAN/DHCP 同族）：
//   - 随提交事务收敛：ApplyVxlan 按声明与**已下发登记**比较——元组变化先按旧元组撤、再建新
//     （决策 #380 的教训：只 add 不撤会留旧条目）；接口置 up；声明了 virtual-switch 就
//     `sw_interface_set_l2_bridge` 入该 L2 交换机的 BD；
//   - 删隧道：DeleteVxlan 先摘 BD 归属再按元组撤条目（配置里被整条删掉的隧道由提交编排发）；
//   - **恢复重放必须含它**：VPP 重启后隧道全失，recovery.go 按配置逐条重放（ApplyVxlan 的
//     元组匹配路径幂等，不重复建）。
//
// 如实边界（v1 不做）：组播/BUM 复制、ARP/ND 代理与 Bypass、VXLAN-GPE、IPv6 下垫层、
// dst-port 以外的封装参数、隧道作 L3 接口、跨 VRF 建隧。instance 由产品分配并在**本机
// 隧道集合内唯一**（按 dump 的已用集合取最小可用值；VPP 用 instance 生成接口名，冲突即建不出）。

import (
	"context"
	"fmt"
	"sync"

	"github.com/xzjt/nfvis/internal/model"
)

// VxlanTunnelInfo 数据面一条 VXLAN 隧道条目（vxlan_tunnel_dump 的一条）。
type VxlanTunnelInfo struct {
	Instance  uint32
	Vni       uint32
	Src       string
	Dst       string
	SrcPort   uint16
	DstPort   uint16
	SwIfIndex uint32
}

// VxlanState 读视图里一条**实际存在**的隧道（按元组键索引，见 VxlanStates）。
type VxlanState struct {
	Instance  uint32
	Vni       uint32
	Src       string
	Dst       string
	DstPort   uint16
	SwIfIndex uint32
}

// VxlanClient VPP vxlan plugin binary API 的最小能力集（govpp 适配/单测假实现）。
type VxlanClient interface {
	// TunnelAddDel 建/撤隧道（is_add=false 按元组撤）。返回 VPP 侧 sw_if_index。
	TunnelAddDel(isAdd bool, instance uint32, vni uint32, src, dst string, dstPort uint16) (uint32, error)
	// TunnelDump 列出 VPP 里实际的隧道——恢复重放/读视图/instance 分配的唯一事实源。
	TunnelDump() ([]VxlanTunnelInfo, error)
	// SetInterfaceUp 置接口 up（VPP 新建隧道默认 down）。
	SetInterfaceUp(swIfIndex uint32) error
	// SetL2Bridge 把接口加入/移出 bridge-domain（enable=false 即移除）。
	SetL2Bridge(swIfIndex, bdID uint32, enable bool) error
	Close()
}

// vxlanApplied 一条已下发隧道的登记（撤销变更/删除时按原值发 is_add=false）。
type vxlanApplied struct {
	instance  uint32
	vni       uint32
	src       string
	dst       string
	dstPort   uint16
	vs        string // 已加入的交换机（空 = 未入 BD）
	swIfIndex uint32
}

// tuple 登记/声明的元组键（与 VxlanTupleKey 同源）。
func (a vxlanApplied) tuple() string { return VxlanTupleKey(a.vni, a.src, a.dst, a.dstPort) }

// VxlanProvider VXLAN 隧道编排。
type VxlanProvider struct {
	client func() (VxlanClient, error)

	mu      sync.Mutex
	applied map[string]vxlanApplied // 隧道名 → 已下发登记
}

// NewVxlanProvider 以固定客户端构造（测试）。
func NewVxlanProvider(c VxlanClient) *VxlanProvider {
	return &VxlanProvider{client: func() (VxlanClient, error) { return c, nil }, applied: map[string]vxlanApplied{}}
}

// NewVxlanProviderFunc 以客户端工厂构造（连接可重连）。
func NewVxlanProviderFunc(f func() (VxlanClient, error)) *VxlanProvider {
	return &VxlanProvider{client: f, applied: map[string]vxlanApplied{}}
}

// reset 清空进程内登记（恢复收敛前调用）：登记只反映「本进程下发过什么」，
// VPP 重启后隧道已不在，按配置重放会按元组匹配重建并重建登记。
func (p *VxlanProvider) reset() {
	p.mu.Lock()
	p.applied = map[string]vxlanApplied{}
	p.mu.Unlock()
}

// VxlanTupleKey 隧道/运行态条目的元组键（vni|src|dst|dst_port）——恢复重放与读视图
// 都按它匹配存量，**不依赖进程内登记、不依赖 VPP 接口名**（接口名由 instance 生成）。
func VxlanTupleKey(vni uint32, src, dst string, dstPort uint16) string {
	return fmt.Sprintf("%d|%s|%s|%d", vni, src, dst, dstPort)
}

// AllocateVxlanInstance 在已用 instance 集合内取最小可用值（从 0 起找第一个未占用者）。
// 纯函数，便于单测钉住「本机隧道集合内唯一」的分配口径。
func AllocateVxlanInstance(used map[uint32]bool) uint32 {
	for i := uint32(0); ; i++ {
		if !used[i] {
			return i
		}
	}
}

// vxlanFindTuple 在 dump 结果里按元组找存量（找不到返回 ok=false）。
func vxlanFindTuple(infos []VxlanTunnelInfo, vni uint32, src, dst string, dstPort uint16) (VxlanTunnelInfo, bool) {
	for _, i := range infos {
		if i.Vni == vni && i.Src == src && i.Dst == dst && i.DstPort == dstPort {
			return i, true
		}
	}
	return VxlanTunnelInfo{}, false
}

// ApplyVxlan 把一条隧道声明收敛到 VPP（幂等）：
//   - 登记里的元组与本次不同 ⇒ **先按旧元组撤、再建新**（决策 #380 的教训）；
//   - 元组在 VPP 里已存在 ⇒ 复用其 instance/sw_if_index（不重复建）；
//   - 否则分配本机最小可用 instance 建隧；
//   - 接口置 up；声明了 virtual-switch 就入该 L2 交换机的 BD（旧归属不同时先摘旧）。
func (p *VxlanProvider) ApplyVxlan(ctx context.Context, t model.VxlanTunnel) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()

	dstPort := uint16(t.EffectiveDstPort())
	p.mu.Lock()
	old, had := p.applied[t.Name]
	p.mu.Unlock()

	infos, err := c.TunnelDump()
	if err != nil {
		return fmt.Errorf("读取 VXLAN 隧道运行态: %w", err)
	}

	// 变更撤旧：元组变化（改 local/remote/vni/dst-port）时旧的必须先撤——只 add 不撤会让
	// 旧条目残留在 VPP 里（该元组继续可达），此后按名字删除也只是撤最新一条。
	// 旧条目已不在 VPP（带外删除/重启后登记尚在）＝已达成，不撤不发（VPP 对「删不存在的
	// 隧道」的返回值不保证为 0，不以错误码猜测）。
	changed := had && old.tuple() != VxlanTupleKey(uint32(t.Vni), t.Local, t.Remote, dstPort)
	if changed {
		if _, foundOld := vxlanFindTuple(infos, old.vni, old.src, old.dst, old.dstPort); foundOld {
			if _, err := c.TunnelAddDel(false, old.instance, old.vni, old.src, old.dst, old.dstPort); err != nil {
				return fmt.Errorf("撤销隧道 %s 的原条目（vni %d, %s→%s）: %w", t.Name, old.vni, old.src, old.dst, err)
			}
		}
	}

	info, found := vxlanFindTuple(infos, uint32(t.Vni), t.Local, t.Remote, dstPort)
	swIfIndex := info.SwIfIndex
	instance := info.Instance
	if !found {
		switch {
		case had: // 元组已变（刚撤旧）或 VPP 里尚未建：沿用旧 instance，接口名稳定
			instance = old.instance
		default:
			used := make(map[uint32]bool, len(infos))
			for _, i := range infos {
				used[i.Instance] = true
			}
			instance = AllocateVxlanInstance(used)
		}
		if swIfIndex, err = c.TunnelAddDel(true, instance, uint32(t.Vni), t.Local, t.Remote, dstPort); err != nil {
			return fmt.Errorf("建 VXLAN 隧道 %s（vni %d, %s→%s）: %w", t.Name, t.Vni, t.Local, t.Remote, err)
		}
	}
	if err := c.SetInterfaceUp(swIfIndex); err != nil {
		return fmt.Errorf("置 VXLAN 隧道 %s 的接口 up: %w", t.Name, err)
	}
	// 登记＝「最后一个**成功下发**的状态」，两步推进（同 #363 的登记语义）：
	// ① 隧道条目已建立/复用且已置 up ⇒ 先记元组与接口（BD 归属暂记可推断的现状）；
	// ② 入/移 BD 成功 ⇒ 再把归属更新为目标值。
	// 这样两步之间失败时（如入 BD 报错），登记里已是**新元组**——提交编排的补偿
	// （ApplyVxlan(old)）因此能按登记撤掉刚建的新隧道，不留残渣。
	vsBefore := ""
	if had && !changed {
		vsBefore = old.vs // 元组未变：接口仍在原交换机（换域时下面先摘旧）
	}
	p.mu.Lock()
	p.applied[t.Name] = vxlanApplied{
		instance: instance, vni: uint32(t.Vni), src: t.Local, dst: t.Remote,
		dstPort: dstPort, vs: vsBefore, swIfIndex: swIfIndex,
	}
	p.mu.Unlock()

	// BD 归属：元组未变而交换机变了（接口还在）时先摘旧成员；接口已随撤旧消失时无需摘。
	if had && !changed && old.vs != "" && old.vs != t.VirtualSwitch && old.swIfIndex != 0 {
		if err := c.SetL2Bridge(old.swIfIndex, BDID(old.vs), false); err != nil && !isMissingIfaceErr(err) {
			return fmt.Errorf("把隧道 %s 移出原交换机 %s: %w", t.Name, old.vs, err)
		}
	}
	if t.VirtualSwitch != "" {
		if err := c.SetL2Bridge(swIfIndex, BDID(t.VirtualSwitch), true); err != nil {
			return fmt.Errorf("把隧道 %s 加入交换机 %s 的 BD: %w", t.Name, t.VirtualSwitch, err)
		}
	}

	// ② 归属已按声明收敛：更新登记（记录失败时不更新，登记保持「条目已在、归属未知」，
	// 撤销路径对「接口已不在」一律容忍）。
	p.mu.Lock()
	if rec, ok := p.applied[t.Name]; ok {
		rec.vs = t.VirtualSwitch
		rec.swIfIndex = swIfIndex
		p.applied[t.Name] = rec
	}
	p.mu.Unlock()
	return nil
}

// DeleteVxlan 撤销一条隧道：先摘 BD 归属，再按**登记元组**撤条目；无登记（进程重启/带外
// 场景）时按配置元组撤（幂等——条目本就不在 VPP 也照常返回成功）。
func (p *VxlanProvider) DeleteVxlan(ctx context.Context, t model.VxlanTunnel) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()

	dstPort := uint16(t.EffectiveDstPort())
	p.mu.Lock()
	rec, had := p.applied[t.Name]
	delete(p.applied, t.Name)
	p.mu.Unlock()

	vni, src, dst, port := uint32(t.Vni), t.Local, t.Remote, dstPort
	if had {
		if rec.vs != "" && rec.swIfIndex != 0 {
			// 先摘 BD 归属（幂等）；接口已不在 = 已达成。
			if err := c.SetL2Bridge(rec.swIfIndex, BDID(rec.vs), false); err != nil && !isMissingIfaceErr(err) {
				return fmt.Errorf("把隧道 %s 移出交换机 %s: %w", t.Name, rec.vs, err)
			}
		}
		vni, src, dst, port = rec.vni, rec.src, rec.dst, rec.dstPort
	}
	// 撤条目：先确认它在不在（条目本就不在＝已达成；VPP 对「删不存在的隧道」的返回值
	// 不保证为 0，不以错误码猜测）。
	infos, err := c.TunnelDump()
	if err != nil {
		return fmt.Errorf("读取 VXLAN 隧道运行态: %w", err)
	}
	if _, found := vxlanFindTuple(infos, vni, src, dst, port); !found {
		return nil
	}
	if _, err := c.TunnelAddDel(false, 0, vni, src, dst, port); err != nil {
		return fmt.Errorf("删除 VXLAN 隧道 %s（vni %d, %s→%s）: %w", t.Name, vni, src, dst, err)
	}
	return nil
}

// EnsureConsistent 恢复收敛重放：对声明里的每条隧道走 ApplyVxlan 的幂等路径（按 dump
// 元组匹配存量，缺则建；不靠进程内登记/接口名）。单条失败不阻塞其余（错误逐条返回并带
// 隧道名标签，调用方记未收敛项）。
func (p *VxlanProvider) EnsureConsistent(ctx context.Context, cfg model.Config) []error {
	var errs []error
	for _, t := range cfg.VxlanTunnels {
		if err := p.ApplyVxlan(ctx, t); err != nil {
			errs = append(errs, fmt.Errorf("vxlan-tunnels/%s: %w", t.Name, err))
		}
	}
	return errs
}

// VxlanStates 读取 VPP 里实际的 VXLAN 隧道，按**元组键**（VxlanTupleKey，含 dst_port）
// 索引。读视图（CLI/REST）遍历配置声明逐条查表：命中 = 已在数据面并带 sw_if_index，
// 未命中 = 未收敛。按元组而非接口名匹配（接口名由 instance 生成、产品不依赖它）。
func (p *VxlanProvider) VxlanStates(ctx context.Context) (map[string]VxlanState, error) {
	c, err := p.client()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	infos, err := c.TunnelDump()
	if err != nil {
		return nil, err
	}
	out := make(map[string]VxlanState, len(infos))
	for _, i := range infos {
		out[VxlanTupleKey(i.Vni, i.Src, i.Dst, i.DstPort)] = VxlanState{
			Instance: i.Instance, Vni: i.Vni, Src: i.Src, Dst: i.Dst,
			DstPort: i.DstPort, SwIfIndex: i.SwIfIndex,
		}
	}
	return out, nil
}
