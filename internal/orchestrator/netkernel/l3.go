package netkernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// 域 VRF 表内兜底路由的路由规格：类型关键字 + 缺省前缀 + 最大档度量（决策 #446）。
//
// 内核的查找链是「l3mdev-table → main → default」：域 VRF 表未命中会**回落宿主 main 表的默认
// 路由**（round10 真机实证：BVI 域内的 VNF 朝域外发包，带 guest 私网源地址的原始报文从管理口
// ens160 出去；对照 VPP 数据面的同流量为丢弃）。在产品创建的**每个转发域** VRF 表内补上
// `unreachable default` 兜底（每个地址族各一条）：域内到未知目的地立即判不可达
// （ICMP unreachable / EHOSTUNREACH），与 VPP 的「FIB 无路即弃」语义对齐；显式路由
// （connected/静态/L3 接口）按最长前缀优先，行为不变。宿主 main 表与 ip rule 一概不动
// （管理面自身出向不经域表）。
//
// **v4/v6 两族都要**：BVI 网关与 l3-interface 都支持 IPv6 地址（`gateway ip <v6-prefix>` /
// `interfaces <if> address <v6>`），域内 v6 流量存在**同一条** main 表回落路径；契约说的是
// 「每个转发域 VRF 表」，未限定地址族。两族只是 `ip` 的族参数不同（v4 无、v6 `-6`），
// 下发/读回/回收走同一套结构（vrfFallbackFamilies）。
//
// 度量取接近 u32 上限的值（两族 FIB 都是**度量小的优先**：v4 的别名表按优先级升序、
// v6 的 fib6 叶子链按 fib6_metric 升序）：域内**显式声明的默认路由**（任何 distance）因此都
// 优先于兜底。两条都是必需的——若兜底用度量 0，同前缀下它会**影子化**显式默认路由（度量小的
// 先进先匹配）；而度量相同还会与显式默认路由落在同一优先级上互相替换（v4 的 replace 按
// 「前缀 + dscp + 度量」定位别名、v6 的 fib6_add_rt2node 同样要求度量相等才 replace），
// 把操作者的配置改掉。更具体的路由（connected/子网静态）走最长前缀，与度量无关。
//
// ⚠️ 如实边界：宿主在内核层面禁用了 IPv6（如 `ipv6.disable=1`，AF_INET6 的路由操作整体
// 不可用）时，v6 兜底下发会失败并如实报在提交错误里（错误带完整命令）。产品本就要求 v6
// 转发可用（EnsureForwarding 把 net.ipv6.conf.all.forwarding=1 当必需前置，见 R2-10 口径），
// 故未为此加「静默跳过 v6」的分支。
const (
	vrfFallbackType   = "unreachable"
	vrfFallbackPrefix = "default"
	vrfFallbackMetric = "4278198272"
)

// vrfFallbackFamily 兜底路由的地址族：v4/v6 共用同一套下发/读回/回收结构，差别只在 `ip`
// 的族选择参数。
type vrfFallbackFamily struct {
	label string   // 族名（错误文案用）
	args  []string // `ip` 的族选择参数（v4 无、v6 `-6`）
}

// vrfFallbackFamilies 兜底覆盖的地址族与收敛顺序（v4 先、v6 后；固定序让命令序列确定、可断言）。
var vrfFallbackFamilies = []vrfFallbackFamily{
	{label: "IPv4"},
	{label: "IPv6", args: []string{"-6"}},
}

// ipArgs 族参数 + 其余 `ip` 参数（返回**新**切片，不与族参数的底层数组共享）。
func (f vrfFallbackFamily) ipArgs(rest ...string) []string {
	return append(append([]string{}, f.args...), rest...)
}

// ensureVRF 确保内核 VRF 设备存在且已 up（路由表号由名字确定性派生），并在表内补域兜底路由
// （决策 #446：L3 交换机 VRF 与 L2 网关域共用本创建点，两处都由此覆盖）。
func (p *Provider) ensureVRF(ctx context.Context, vrfName string) error {
	if err := p.ipIdem(ctx, "link", "add", "name", vrfName, "type", "vrf", "table", fmt.Sprint(VRFTableID(vrfName))); err != nil {
		return err
	}
	if err := p.ipReq(ctx, "link", "set", "dev", vrfName, "up"); err != nil {
		return err
	}
	return p.ensureVRFFallback(ctx, vrfName)
}

// ensureVRFFallback 确保域 VRF 表内**两族**的兜底路由都在位；已在位则跳过（恢复重放/每次提交
// 都会走这条）。
func (p *Provider) ensureVRFFallback(ctx context.Context, vrfName string) error {
	for _, fam := range vrfFallbackFamilies {
		if p.vrfFallbackPresent(ctx, vrfName, fam) {
			continue
		}
		if err := p.ipReq(ctx, vrfFallbackArgs("replace", vrfName, fam)...); err != nil {
			return fmt.Errorf("域兜底路由（%s）: %w", fam.label, err)
		}
	}
	return nil
}

// vrfFallbackArgs 一族的兜底路由命令参数，与静态路由同一命令行形状（`ip [-6] route <verb>
// vrf <名> …`）。
//
// 不复用 routeArgs：兜底的路由规格是**两个词**（类型关键字 `unreachable` + 前缀 `default`），
// 而 routeArgs 把 route.Prefix 作为**单个 argv**——塞 "unreachable default" 进去在真机上会被
// iproute2 当作一个非法前缀打回（`Error: … prefix is expected rather than …`）。
// `vrf <名>` 这点与 routeArgs 同源：表号由 iproute2 从 VRF 设备读回**实际**值
// （ipvrf_get_table，两族同一条 RTM_GETLINK 路径），不另造 TID 推导、也就不会与设备实际所属
// 表不一致；replace 语义与 ApplyRoute 同口径（重复下发无害；v4/v6 的 replace 都只匹配同度量
// 的既有路由，故不会动操作者声明的默认路由）。
func vrfFallbackArgs(verb, vrfName string, fam vrfFallbackFamily) []string {
	return fam.ipArgs("route", verb, "vrf", LinkName(vrfName),
		vrfFallbackType, vrfFallbackPrefix, "metric", vrfFallbackMetric)
}

// deleteVRFFallback 回收域 VRF 表内两族的兜底路由（删除 VRF 设备前调用，不留下孤立路由）。
//
// 调用前必须确认 VRF 设备在场（见 vrfDevicePresent）：`ip route del vrf <名>` 在设备不存在时
// 于 iproute2 **解析阶段**报 Invalid VRF——不是「找不到设备」一类文案，ipBest 不容错。
//
// 回收命令带与下发相同的度量：v6 的删除路径**按度量过滤**（内核 `ip6_route_del`：
// `fc_metric && fc_metric != rt->fib6_metric → continue`），不带度量会退化成「删该前缀的第一条」
// ——可能删到操作者自己的默认路由；v4 的删除不比较度量（只为两族同形可读）。
// 另注：reject 路由不归属 VRF 设备本身（v6 绑 loopback、v4 无下一跳），设备删除**不会**连带
// 回收它——这条显式回收是「不残留孤立路由」的承重墙。
func (p *Provider) deleteVRFFallback(ctx context.Context, vrfName string) error {
	for _, fam := range vrfFallbackFamilies {
		if err := p.ipBest(ctx, vrfFallbackArgs("del", vrfName, fam)...); err != nil {
			return fmt.Errorf("域兜底路由（%s）: %w", fam.label, err)
		}
	}
	return nil
}

// vrfFallbackPresent 该族在域 VRF 表内是否已有兜底路由。
//
// 读 `ip [-6] -j route show vrf <名>`（iproute2 按该设备**实际**表号过滤；unreachable 不在
// `vrf` 选择器排除的类型里，两族共用同一打印机、同样输出 type/dst 键）。读不回来按「不在位」
// 处理：读回只为幂等跳过，收敛与否以随后的写为准——写是 replace 语义、多下一次没有副作用；
// 倒是把一次失败的读回上抛，会让普通重放整体失败。
func (p *Provider) vrfFallbackPresent(ctx context.Context, vrfName string, fam vrfFallbackFamily) bool {
	out, err := p.ip(ctx, fam.ipArgs("-j", "route", "show", "vrf", LinkName(vrfName))...)
	if err != nil {
		return false
	}
	var rows []ipRouteRow
	if json.Unmarshal([]byte(out), &rows) != nil {
		return false
	}
	for _, row := range rows {
		if !strings.EqualFold(row.Type, "unreachable") {
			continue
		}
		switch row.Dst {
		case "", "default", "0.0.0.0/0", "::/0":
			return true
		}
	}
	return false
}

// vrfDevicePresent 内核里该名字是（严格地）一台 VRF 设备。
//
// 删除路径的守卫：`ip route del vrf <名>` 在设备不存在（或同名设备不是 VRF）时，iproute2 在
// **解析阶段**就报 Invalid VRF 而 ipBest 不容错——不先确认设备在场，「删已删过的 VRF」这条
// 幂等路径会被兜底回收自己打断。
func (p *Provider) vrfDevicePresent(ctx context.Context, name string) bool {
	row, ok := p.linkDetail(ctx, name)
	if !ok || row.LinkInfo == nil {
		return false
	}
	return strings.EqualFold(row.LinkInfo.InfoKind, "vrf")
}

// ApplyVRF 收敛一台 L3 虚拟交换机为内核 VRF：VRF 设备 + 各 l3-interface 的归属与地址 +
// 声明里的静态路由（与 VPP 侧同口径：只加不撤，撤销由 DeleteRoute/DeleteL3Interface 负责）。
func (p *Provider) ApplyVRF(ctx context.Context, vrf model.Vrf) error {
	if vrf.Name == "" {
		return fmt.Errorf("L3 交换机名不能为空")
	}
	vrfName := LinkName(vrf.Name)
	if err := p.ensureVRF(ctx, vrfName); err != nil {
		return err
	}
	if vrf.Description != "" {
		if err := p.ipReq(ctx, "link", "set", "dev", vrfName, "alias", vrf.Description); err != nil {
			return err
		}
	}
	for _, li := range vrf.L3Interfaces {
		if err := p.applyL3Interface(ctx, vrfName, li); err != nil {
			return err
		}
		// 三层接口的入向 ACL 绑定（声明写 `acl-in <名>`；策略对象由 ApplyACL 先行下发）。
		if err := p.bindL3IfaceACL(ctx, li); err != nil {
			return err
		}
	}
	for _, rt := range vrf.Routes {
		if err := p.ApplyRoute(ctx, vrf.Name, rt); err != nil {
			return err
		}
	}
	return nil
}

// applyL3Interface 把一条 l3-interface 挂进 VRF 并下发地址（vlan 子接口按需创建）。
func (p *Provider) applyL3Interface(ctx context.Context, vrfName string, li model.L3Interface) error {
	if li.Interface == "" {
		return fmt.Errorf("l3-interface 的接口名不能为空")
	}
	dev, err := l3DeviceNameChecked(li)
	if err != nil {
		return err
	}
	base := LinkName(li.Interface)
	// 基口必须先 up：vlan 子接口的 carrier 跟随基口，基口 down 时 `ip link set <子接口> up`
	// 会直接报 `RTNETLINK answers: Network is down`（真机实测）。三层接口本就要求可用，
	// 故这里显式置 up（幂等；这条口径写进设计文档与用户手册）。
	if err := p.ensureLinkUp(ctx, base); err != nil {
		return err
	}
	if li.Vlan > 0 {
		if err := p.ipIdem(ctx, "link", "add", "link", base, "name", dev, "type", "vlan", "id", fmt.Sprint(li.Vlan)); err != nil {
			return err
		}
	}
	if err := p.ipReq(ctx, "link", "set", "dev", dev, "master", vrfName); err != nil {
		return err
	}
	for _, cidr := range li.Addresses {
		if err := p.ipIdem(ctx, "addr", "replace", cidr, "dev", dev); err != nil {
			return err
		}
	}
	return p.ensureLinkUp(ctx, dev)
}

// l3DeviceName l3-interface 对应的内核设备名（**不校验长度**）。
//
// 供「按名解引用」类宽松路径使用（它们用同一派生口径，超限时下游命令如实失败）；
// 下发/回收路径一律用 l3DeviceNameChecked 拿如实错误（R2-21：`<基口>.<vid>` 可能超过
// 内核接口名上限 15）。超限时返回派生名的字面形态而不是空串——调用方的错误文案要能看出对象是谁。
func l3DeviceName(li model.L3Interface) string {
	dev, err := l3DeviceNameChecked(li)
	if err != nil {
		return fmt.Sprintf("%s.%d", LinkName(li.Interface), li.Vlan)
	}
	return dev
}

// l3DeviceNameChecked l3DeviceName 的校验版：派生名超限时如实返回错误。
func l3DeviceNameChecked(li model.L3Interface) (string, error) {
	if li.Vlan > 0 {
		return VlanSubifName(LinkName(li.Interface), li.Vlan)
	}
	return LinkName(li.Interface), nil
}

// ApplyRoute 下发一条静态路由到该 VRF 的表（replace 语义，幂等）。
func (p *Provider) ApplyRoute(ctx context.Context, vrfName string, route model.Route) error {
	args := routeArgs("replace", vrfName, route)
	return p.ipIdem(ctx, args...)
}

// DeleteRoute 撤销一条静态路由。
func (p *Provider) DeleteRoute(ctx context.Context, vrfName string, route model.Route) error {
	args := routeArgs("del", vrfName, route)
	return p.ipBest(ctx, args...)
}

func routeArgs(verb, vrfName string, route model.Route) []string {
	args := []string{"route", verb, "vrf", LinkName(vrfName), route.Prefix}
	if route.NextHop != "" {
		args = append(args, "via", route.NextHop)
	}
	if route.Distance > 0 {
		args = append(args, "metric", fmt.Sprint(route.Distance))
	}
	return args
}

// DeleteL3Interface 回收一条已从声明里删除的 L3 接口：清地址 → 出 VRF → （vlan 子接口）删设备。
func (p *Provider) DeleteL3Interface(ctx context.Context, vrfName string, iface model.L3Interface) error {
	dev, err := l3DeviceNameChecked(iface)
	if err != nil {
		return err
	}
	// 先解引用：接口要撤，它上面的 ACL 绑定必须一起摘（否则 nft 里留着指向该口的 jump）。
	if iface.AclIn != "" {
		if err := p.aclMgr().Unbind(ctx, dev); err != nil {
			return err
		}
	}
	for _, cidr := range iface.Addresses {
		if err := p.ipBest(ctx, "addr", "del", cidr, "dev", dev); err != nil {
			return err
		}
	}
	// 只摘**本产品这次要解绑的那类归属**（VRF）：迁移提交里同一个口可能刚被挂到某台交换机
	// （plan 段序：bridge-domain 先于 l3 撤销），无条件 nomaster 会把刚挂上的 bridge 归属摘掉——
	// 配置说它是交换机端口、内核里它却不是任何 bridge 的成员（R2-14；与 #193/#196/#361 同族）。
	if err := p.detachMasterIfVRF(ctx, dev); err != nil {
		return err
	}
	if iface.Vlan > 0 {
		return p.ipBest(ctx, "link", "del", dev)
	}
	return nil
}

// detachMasterIfVRF 当前 master 是 VRF 时摘除归属；别的对象（bridge/bond）接管时保持不动。
//
// 读不到归属（设备不存在/读失败）同样不动：删除路径的「已达成」由调用方按需处理，
// 猜测式 nomaster 会摘掉别的对象刚建立的归属，比留下一个解绑动作更危险。
func (p *Provider) detachMasterIfVRF(ctx context.Context, dev string) error {
	master, slaveKind, ok := p.linkMaster(ctx, dev)
	if !ok || master == "" {
		return nil
	}
	if !strings.EqualFold(slaveKind, "vrf") {
		return nil
	}
	return p.ipBest(ctx, "link", "set", "dev", dev, "nomaster")
}

// DeleteVRF 删除一台 L3 交换机：先回收其成员口（清地址、出 VRF、删 vlan 子接口）、回收兜底
// 路由（v4/v6 两族），再删 VRF 设备。
func (p *Provider) DeleteVRF(ctx context.Context, name string) error {
	vrfName := LinkName(name)
	for _, m := range p.vrfMembers(ctx, vrfName) {
		for _, cidr := range p.deviceAddresses(ctx, m) {
			if err := p.ipBest(ctx, "addr", "del", cidr, "dev", m); err != nil {
				return err
			}
		}
		// 与 DeleteL3Interface 同口径：只摘本 VRF 的归属（成员清单来自实况，此处是冗余保护——
		// 口已被别的对象接管时不误摘）。
		if err := p.detachMasterIf(ctx, m, vrfName); err != nil {
			return err
		}
		if strings.Contains(m, ".") {
			if err := p.ipBest(ctx, "link", "del", m); err != nil {
				return err
			}
		}
	}
	// 决策 #446：先回收域兜底路由、再删 VRF 设备。放在成员口回收**之后**：成员清理中途失败
	// 提前返回时，域内仍有兜底保护（先摘兜底会开一个「兜底不在、VRF 还在」的外泄窗口）。
	// 设备不在场（已删过/同名设备不是 VRF）时跳过——对不存在的 VRF 下 `ip route del` 会在
	// iproute2 解析阶段报 Invalid VRF，打断「删已删过的 VRF」的幂等路径。
	if p.vrfDevicePresent(ctx, vrfName) {
		if err := p.deleteVRFFallback(ctx, vrfName); err != nil {
			return err
		}
	}
	return p.ipBest(ctx, "link", "del", vrfName)
}

// ipLinkRow `ip -j link show` 的一行（只取接口名）。
type ipLinkRow struct {
	Ifname string `json:"ifname"`
}

// vrfMembers 当前挂在给定 VRF 下的接口名（读失败返回 nil）。
func (p *Provider) vrfMembers(ctx context.Context, vrfName string) []string {
	return p.linkMembers(ctx, vrfName)
}

// ipAddrRow `ip -j addr show` 的一行。
type ipAddrRow struct {
	AddrInfo []struct {
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
	} `json:"addr_info"`
}

// deviceAddresses 某设备当前的地址（CIDR 形态；读失败返回 nil）。
func (p *Provider) deviceAddresses(ctx context.Context, dev string) []string {
	out, err := p.run.Run(ctx, "ip", "-j", "addr", "show", "dev", dev)
	if err != nil {
		return nil
	}
	var rows []ipAddrRow
	if json.Unmarshal([]byte(out), &rows) != nil {
		return nil
	}
	var cidrs []string
	for _, r := range rows {
		for _, a := range r.AddrInfo {
			if a.Local != "" {
				cidrs = append(cidrs, fmt.Sprintf("%s/%d", a.Local, a.PrefixLen))
			}
		}
	}
	return cidrs
}
