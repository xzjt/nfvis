package network

// VXLAN overlay（决策 #383，FR-NET-019）：v1 只做**单播 remote、IPv4 下垫层、L2 成员**。
//
// **身份＝接口 tag**（真机实证的底座缺口）：VPP 26.06 的 `vxlan_tunnel_dump` /
// `vxlan_tunnel_v2_dump` **恒空**——两版 dump 请求 CRC 与本仓 binapi 逐一吻合
// （`f9e6675e`）、details 各自 `c3916cb1`/`d3bdd4d9` 亦吻合，用 govpp 分别按 V1/V2 发 dump
// 都立即 `<stop>`、零条目，而 `vppctl show vxlan tunnel` 有条目——与 ACL 逐规则计数
// 「注册了但不更新」同类的底座缺口（不是产品请求版本不符）。故产品在隧道口上打**自己的 tag**
// （`model.VxlanTunnel.DataPlaneTag()`，`nfvis-vxlan:<name>`，`sw_interface_tag_add_del` 打标、
// `sw_interface_dump` 的 tag 字段回读）——**不依赖 vxlan dump、也不依赖 VPP 分配的接口名**
// （与 #359 内置 tap 按 HostIfName 识别同族）。
//
// 由此的**如实边界**：运行态只能证明「该名字的隧道口在数据面」；**VNI/下垫地址/端口以配置为准**
// （底座没有可回读隧道参数的清单）。读视图（CLI/REST/Web）按此口径呈现。
//
// 生命周期口径：
//   - ApplyVxlan(t, prev)：prev 是提交 diff 里的**旧声明**（nil = 新建/恢复重放）。
//     旧元组与本次不同 ⇒ 先按**旧元组**发 is_add=false 撤、再按新元组建 + 打 tag + 置 up + 入 BD；
//     同元组且 tag 已在（如 `systemctl restart nfvis` 而 VPP 未重启）⇒ **不重复建**，只按声明
//     校正 up/BD（幂等）。两处都**不依赖 dump**（旧元组来自旧配置）。
//   - DeleteVxlan(t)：t 是**被删掉的旧声明**——按它的元组撤条目、按它的 virtual-switch 摘 BD
//     归属（tag 随接口消失）。该名字的隧道口不在数据面（VPP 重启/带外删除）＝已达成。
//   - EnsureConsistent：**按 tag 判存量**——tag 在则不动、不在则按配置建（VPP 重启会连隧道
//     一起清 ⇒ 重放即重建），**不靠进程内登记、不靠接口名**。
//   - instance：26.06 无 dump 可枚举已用 instance，故按 API 的「自动分配」值（~0，与
//     `vppctl create vxlan tunnel` 同款）交给 VPP 分配——唯一性由底座保证，冲突不会发生。
//
// v1 边界（如实）：组播/BUM 复制、ARP/ND 代理与 Bypass、VXLAN-GPE、IPv6 下垫层、
// dst-port 以外的封装参数、隧道作 L3 接口、跨 VRF 建隧——均不做。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// VxlanAutoInstance 建/撤隧时交给 VPP 自动分配 instance（API 的「可选实例，缺省 ~0」；
// 与 `vppctl create vxlan tunnel` 同款）。产品不自行枚举 instance：26.06 的 vxlan dump 恒空，
// 进程内登记跨 nfvisd 重启即失效（恢复重放刻意不靠登记）。
const VxlanAutoInstance uint32 = 0xFFFFFFFF

// TaggedIface 数据面里一个被打上平台 tag 的接口。
type TaggedIface struct {
	SwIfIndex     uint32
	InterfaceName string
}

// VxlanState 读视图里一个**实际存在**的隧道口（按隧道名索引，见 VxlanStates 的如实边界）。
type VxlanState struct {
	SwIfIndex     uint32
	InterfaceName string
}

// ErrVxlanExists 建隧时数据面已存在同元组的隧道（VPP retval -75，真机实测的返回值）。
// 常见成因：带外命令建过同一条隧道。调用方据此给出可照做的下一步。
var ErrVxlanExists = errors.New("数据面已存在同元组的 VXLAN 隧道")

// VxlanClient VPP vxlan / 接口 binary API 的最小能力集（govpp 适配/单测假实现）。
type VxlanClient interface {
	// TunnelAddDel 建/撤隧道（is_add=false 按元组撤）。返回 VPP 侧 sw_if_index。
	// 建隧时同元组已存在 ⇒ ErrVxlanExists。
	TunnelAddDel(isAdd bool, instance uint32, vni uint32, src, dst string, dstPort uint16) (uint32, error)
	// SetTag 给接口打/改 tag（数据面身份，见文件头）。
	SetTag(swIfIndex uint32, tag string) error
	// FindTagged 列出 tag 以 prefix 开头的接口（tag → {sw_if_index, 接口名}）。
	FindTagged(prefix string) (map[string]TaggedIface, error)
	// SetInterfaceUp 置接口 up（VPP 新建隧道默认 down）。
	SetInterfaceUp(swIfIndex uint32) error
	// SetL2Bridge 把接口加入/移出 bridge-domain（enable=false 即移除）。
	SetL2Bridge(swIfIndex, bdID uint32, enable bool) error
	Close()
}

// VxlanProvider VXLAN 隧道编排。
type VxlanProvider struct {
	client func() (VxlanClient, error)
}

// NewVxlanProvider 以固定客户端构造（测试）。
func NewVxlanProvider(c VxlanClient) *VxlanProvider {
	return &VxlanProvider{client: func() (VxlanClient, error) { return c, nil }}
}

// NewVxlanProviderFunc 以客户端工厂构造（连接可重连）。
func NewVxlanProviderFunc(f func() (VxlanClient, error)) *VxlanProvider {
	return &VxlanProvider{client: f}
}

// ApplyVxlan 把一条隧道声明收敛到 VPP（幂等）。
//
// prev 是提交 diff 里的旧声明（nil = 新建/恢复重放）：
//   - 旧元组与本次不同 ⇒ 先按**旧元组**撤（不依赖 dump——旧元组就在旧配置里；同 #380 的教训：
//     只 add 不撤会留下旧条目继续可达），再按新元组建；
//   - 该名字的 tag 已在数据面且元组未变 ⇒ **不重复建**（重放幂等），只按声明校正 up/BD 归属；
//   - 无 tag ⇒ 建隧（instance 交 VPP 自动分配）→ **打 tag** → 置 up → 入 BD。
func (p *VxlanProvider) ApplyVxlan(ctx context.Context, t model.VxlanTunnel, prev *model.VxlanTunnel) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()

	tag := t.DataPlaneTag()
	tagged, err := c.FindTagged(model.VxlanTagPrefix)
	if err != nil {
		return fmt.Errorf("读取隧道运行态（接口标记）: %w", err)
	}
	cur, exists := tagged[tag]

	// 变更撤旧：先按旧元组撤（旧条目不在＝已达成——tag 判存量，不靠 dump）。
	if exists && prev != nil && !prev.SameTuple(t) {
		if _, err := c.TunnelAddDel(false, VxlanAutoInstance,
			uint32(prev.Vni), prev.Local, prev.Remote, uint16(prev.EffectiveDstPort())); err != nil {
			return fmt.Errorf("撤销隧道 %s 的原条目（vni %d, %s→%s）: %w",
				t.Name, prev.Vni, prev.Local, prev.Remote, err)
		}
		exists, cur = false, TaggedIface{}
	}

	swIfIndex := cur.SwIfIndex
	if !exists {
		swIfIndex, err = c.TunnelAddDel(true, VxlanAutoInstance,
			uint32(t.Vni), t.Local, t.Remote, uint16(t.EffectiveDstPort()))
		if err != nil {
			if errors.Is(err, ErrVxlanExists) {
				return fmt.Errorf("建 VXLAN 隧道 %s（vni %d, %s→%s）: %w——数据面里已有同元组的隧道"+
					"（可能由带外命令建立）；请先清掉它（request vpp restart 会清空数据面隧道）或核对后再试",
					t.Name, t.Vni, t.Local, t.Remote, err)
			}
			return fmt.Errorf("建 VXLAN 隧道 %s（vni %d, %s→%s）: %w", t.Name, t.Vni, t.Local, t.Remote, err)
		}
		// 建隧后立刻打标：即使后续步骤（置 up/入 BD）失败，该名字的隧道口仍可被识别，
		// 删除/重放路径据此判定存量（也便于真机 vppctl 对照）。
		if err := c.SetTag(swIfIndex, tag); err != nil {
			return fmt.Errorf("给隧道 %s 的接口打平台标记 %q: %w", t.Name, tag, err)
		}
	}
	if err := c.SetInterfaceUp(swIfIndex); err != nil {
		return fmt.Errorf("置 VXLAN 隧道 %s 的接口 up: %w", t.Name, err)
	}
	// BD 归属：元组未变而交换机变了（接口还在）时先摘旧成员；接口已随撤旧消失时无需摘。
	// 运行态只证明「该名字的隧道口在」——参数以配置为准，这里只按声明校正归属（幂等）。
	if exists && prev != nil && prev.VirtualSwitch != "" && prev.VirtualSwitch != t.VirtualSwitch {
		if err := c.SetL2Bridge(swIfIndex, BDID(prev.VirtualSwitch), false); err != nil && !isMissingIfaceErr(err) {
			return fmt.Errorf("把隧道 %s 移出原交换机 %s: %w", t.Name, prev.VirtualSwitch, err)
		}
	}
	if t.VirtualSwitch != "" {
		if err := c.SetL2Bridge(swIfIndex, BDID(t.VirtualSwitch), true); err != nil {
			return fmt.Errorf("把隧道 %s 加入交换机 %s 的 BD: %w", t.Name, t.VirtualSwitch, err)
		}
	}
	return nil
}

// DeleteVxlan 撤销一条隧道：t 是**被删掉的旧声明**——先按它的 virtual-switch 摘 BD 归属
// （接口已随撤旧消失时无需摘），再按它的元组撤条目。该名字的隧道口不在数据面（VPP 重启/
// 带外删除/从未建成）＝已达成，不报错（幂等）。
func (p *VxlanProvider) DeleteVxlan(ctx context.Context, t model.VxlanTunnel) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	defer c.Close()

	tagged, err := c.FindTagged(model.VxlanTagPrefix)
	if err != nil {
		return fmt.Errorf("读取隧道运行态（接口标记）: %w", err)
	}
	cur, exists := tagged[t.DataPlaneTag()]
	if !exists {
		return nil
	}
	if t.VirtualSwitch != "" {
		if err := c.SetL2Bridge(cur.SwIfIndex, BDID(t.VirtualSwitch), false); err != nil && !isMissingIfaceErr(err) {
			return fmt.Errorf("把隧道 %s 移出交换机 %s: %w", t.Name, t.VirtualSwitch, err)
		}
	}
	if _, err := c.TunnelAddDel(false, VxlanAutoInstance,
		uint32(t.Vni), t.Local, t.Remote, uint16(t.EffectiveDstPort())); err != nil {
		return fmt.Errorf("删除 VXLAN 隧道 %s（vni %d, %s→%s）: %w", t.Name, t.Vni, t.Local, t.Remote, err)
	}
	return nil
}

// EnsureConsistent 恢复收敛重放：**按 tag 判存量**——tag 在则不动（不重复建）、不在则按配置
// 建（VPP 重启会连隧道一起清，重放即重建）。单条失败不阻塞其余（错误逐条返回并带隧道名标签）。
func (p *VxlanProvider) EnsureConsistent(ctx context.Context, cfg model.Config) []error {
	var errs []error
	for _, t := range cfg.VxlanTunnels {
		if err := p.ApplyVxlan(ctx, t, nil); err != nil {
			errs = append(errs, fmt.Errorf("vxlan-tunnels/%s: %w", t.Name, err))
		}
	}
	return errs
}

// VxlanStates 运行态读视图：按**隧道名**索引数据面上带平台标记的隧道口。
//
// **如实边界**：底座（VPP 26.06）的 vxlan dump 恒空，没有可回读隧道参数的清单——本视图只能
// 证明「该名字的隧道口在数据面」（值给 sw_if_index 与 VPP 接口名）；**VNI/下垫地址/端口
// 以配置为准**，不声称从数据面读到了它们。
func (p *VxlanProvider) VxlanStates(ctx context.Context) (map[string]VxlanState, error) {
	c, err := p.client()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	tagged, err := c.FindTagged(model.VxlanTagPrefix)
	if err != nil {
		return nil, err
	}
	out := make(map[string]VxlanState, len(tagged))
	for tag, ti := range tagged {
		name := strings.TrimPrefix(tag, model.VxlanTagPrefix)
		if name == "" {
			continue // 只有前缀的畸形 tag：不猜名字
		}
		out[name] = VxlanState{SwIfIndex: ti.SwIfIndex, InterfaceName: ti.InterfaceName}
	}
	return out, nil
}
