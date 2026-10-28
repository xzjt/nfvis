package netkernel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// ApplyBridgeDomain 收敛一台 L2 虚拟交换机为内核 bridge。
//
// 映射：交换机名 → bridge 接口名；成员口 `ip link set master`；access/trunk/native 由
// `bridge vlan` 表达（仅在声明了 VLAN 时打开 vlan_filtering，避免无谓改变默认转发）。
// 网关（BVI 等价物）地址直接落在 bridge 上，bridge 再按网关 VRF 入表。
//
// type=l3 的交换机不建 bridge（其 VRF 设备由 ApplyVRF 收敛），如实返回成功。
func (p *Provider) ApplyBridgeDomain(ctx context.Context, vs model.VirtualSwitch) error {
	if vs.Type == "l3" {
		return nil
	}
	br := LinkName(vs.Name)
	if err := p.ipIdem(ctx, "link", "add", "name", br, "type", "bridge"); err != nil {
		return err
	}
	if err := p.ipReq(ctx, "link", "set", "dev", br, "type", "bridge", "vlan_filtering", vlanFilteringValue(vs)); err != nil {
		return err
	}
	if vs.Description != "" {
		if err := p.ipReq(ctx, "link", "set", "dev", br, "alias", vs.Description); err != nil {
			return err
		}
	}

	// 成员口：声明集合为准，同时释放已不在声明里的旧成员（ApplyBridgeDomain 是全量语义）。
	declared := map[string]bool{}
	for _, port := range vs.Ports {
		name, ok := memberLinkName(port)
		if !ok {
			// VM vNIC 成员由 libvirt 的 bridge 接入自行 enslave（见 ApplyVnfInterface）；
			// 容器 vNIC 的宿主端由 vnf-if 段先建，此处与普通成员口同一处理（见 memberLinkName）。
			continue
		}
		declared[name] = true
		if err := p.applyBridgeMember(ctx, br, name, vs, port); err != nil {
			return err
		}
	}
	for _, cur := range p.bridgeMembers(ctx, br) {
		if declared[cur] {
			continue
		}
		// 只摘**本产品 enslave 的成员**：libvirt 自建的 VM tap（vnetN，`info_kind=tun`）与
		// ApplyVxlan 自管的隧道口（vxlan）不由本方法 enslave，一律不碰——旧实现按内核实况
		// 全量摘除，任何一次交换机重放（服务重启/升级/给同台交换机加端口）都会把运行中 VM 的
		// tap 从 bridge 摘掉：VM 仍在跑、产品零报错，宿主到 guest 的 L2 静默断流（R2-1，P0）。
		// 物理口/子接口没有 info_kind（不是虚拟设备），属正常可释放成员。
		row, ok := p.linkDetail(ctx, cur)
		if !ok {
			// 读不到设备详情：不猜、不摘（保守）——摘错的代价是运行中 VM 的 L2 静默断流，
			// 留下的陈旧成员至少是读视图可见的（且该口转作其它角色时归属会被自然改走）。
			continue
		}
		if row.LinkInfo != nil && (row.LinkInfo.InfoKind == "tun" || row.LinkInfo.InfoKind == "vxlan") {
			continue
		}
		if err := p.ipBest(ctx, "link", "set", "dev", cur, "nomaster"); err != nil {
			return err
		}
	}

	if vs.Gateway != nil && len(vs.Gateway.Addresses) > 0 {
		if err := p.applyGateway(ctx, vs, br); err != nil {
			return err
		}
		// 地址**收缩**（两个减到一个）也要收敛：applyGateway 只做 add/replace，旧地址不会
		// 自己消失（round49 外部走查）。两步合起来即「声明集合为准」的全量语义。
		if err := p.pruneGatewayAddresses(ctx, br, vs.Gateway.Addresses); err != nil {
			return err
		}
	} else if err := p.revokeGateway(ctx, vs, br); err != nil {
		// 网关声明消失（交换机保留）：清地址、退归属、回收派生的网关 VRF 设备。
		// 此前这里什么都不做——配置说没有网关，内核里 bridge 仍带着网关地址、仍挂在
		// `vr-<交换机>` 上（域内主机继续把已删网关当可用，全程零报错）。
		return err
	}
	return p.ensureLinkUp(ctx, br)
}

// vlanFilteringValue 是否需要打开 bridge 的 VLAN 过滤。
func vlanFilteringValue(vs model.VirtualSwitch) string {
	if vs.VlanAccess > 0 {
		return "1"
	}
	for _, port := range vs.Ports {
		if port.NativeVlan > 0 || len(port.TrunkVlans) > 0 {
			return "1"
		}
	}
	return "0"
}

// memberLinkName 端口声明对应的内核成员口名；ok=false 表示该端口不由本方法 enslave。
func memberLinkName(port model.VSwitchPort) (string, bool) {
	switch {
	case port.Interface != "":
		return LinkName(port.Interface), true
	case port.Vnf != "":
		return "", false // libvirt 自建 tap 并挂 bridge
	case port.Container != "":
		// 容器 vNIC：内核侧是**产品自持的 veth 对**（决策 #441）——宿主端由网络编排在提交编排的
		// vnf-if 段**先建**（ApplyContainerVeth：建对 + 置 up；**先建接口、后入域**，与 VPP 侧
		// 「先建 memif 接口、BD 段按名把端口挂进 BD」同一架构），本段作为 bridge-domain 的成员
		// 处理负责把它 enslave 到该交换机的内核 bridge、置 up，并随成员一起落 VLAN
		// （applyPortVlans：交换机/端口声明，与 VPP 侧同口径——vNIC 级的 vlan 字段不单独处理）。
		// 名字走 orchestrator.ContainerVethNames 单一真源（与 ApplyContainerVeth、容器端接入、
		// 读视图过滤、15s 巡检同一份派生）。
		host, _ := orchestrator.ContainerVethNames(port.Container, port.ContainerInterface)
		return host, true
	default:
		return "", false
	}
}

// applyBridgeMember 把一个成员口收敛进内核 bridge：入域（master）→ 置 up → 按声明落 VLAN 规则。
//
// 调用方：ApplyBridgeDomain 的成员循环（全量语义；name 由 memberLinkName 解析）与
// ApplyInterface 的域归属收敛（决策 #449：`request interfaces <n> enable` 后按配置快照
// 把口接回桥）。两处必须是**同一份**语义（enslave 与置 up 的顺序、VLAN 规则形态），
// 复制第二份必然漂移——抽成这个单成员助手。
func (p *Provider) applyBridgeMember(ctx context.Context, br, name string, vs model.VirtualSwitch, spec model.VSwitchPort) error {
	if err := p.ipReq(ctx, "link", "set", "dev", name, "master", br); err != nil {
		return err
	}
	if err := p.ensureLinkUp(ctx, name); err != nil {
		return err
	}
	return p.applyPortVlans(ctx, name, vs, spec)
}

// applyPortVlans 按声明收敛成员口的 VLAN 条目（先按实况清、再按声明加，保证删除也能收敛）。
//
// VLAN 表由 `bridge` 命令维护（**不是** `ip vlan`——后者不存在）；`bridge vlan add/del`
// 在真机上本身幂等/容错（重复 add、删不存在的 vid 都返回 0）。
func (p *Provider) applyPortVlans(ctx context.Context, port string, vs model.VirtualSwitch, spec model.VSwitchPort) error {
	want := map[int]vlanSpec{}
	if spec.NativeVlan > 0 {
		want[spec.NativeVlan] = vlanSpec{pvid: true, untagged: true}
	}
	for _, vid := range spec.TrunkVlans {
		if _, ok := want[vid]; !ok {
			want[vid] = vlanSpec{}
		}
	}
	if vs.VlanAccess > 0 {
		want[vs.VlanAccess] = vlanSpec{pvid: true, untagged: true}
	}
	for _, vid := range p.bridgePortVlans(ctx, port) {
		if _, ok := want[vid]; ok {
			continue
		}
		// 本交换机没声明任何 VLAN 时保留默认 VID 1（VLAN 过滤本就关着，动它没有意义）。
		if vid == 1 && len(want) == 0 {
			continue
		}
		if err := p.bridgeIdem(ctx, "vlan", "del", "dev", port, "vid", fmt.Sprint(vid)); err != nil {
			return err
		}
	}
	for vid, s := range want {
		args := []string{"vlan", "add", "dev", port, "vid", fmt.Sprint(vid)}
		if s.pvid {
			args = append(args, "pvid")
		}
		if s.untagged {
			args = append(args, "untagged")
		}
		if err := p.bridgeIdem(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

type vlanSpec struct{ pvid, untagged bool }

// applyGateway 下发 L2 交换机的三层网关：地址落在 bridge 上，bridge 入网关 VRF。
func (p *Provider) applyGateway(ctx context.Context, vs model.VirtualSwitch, br string) error {
	vrfName := vs.Gateway.Vrf
	if vrfName == "" {
		vrfName = GatewayVRFName(vs.Name)
	} else {
		vrfName = LinkName(vrfName)
	}
	if err := p.ensureVRF(ctx, vrfName); err != nil {
		return err
	}
	// round51 P4：网关**换目标**（派生的 `vr-<交换机>` → 显式 `gateway vrf <名>`）时收回上一位
	// 派生设备（含它的两条域兜底路由）。此前只有「网关声明消失」（revokeGateway）与「交换机
	// 被删」（DeleteBridgeDomain）两条路径回收它，换目标这条漏了 ⇒ 内核里长期留一张空 VRF
	// 与两条 unreachable default，而读视图（交换机报网关地址、VRF 列表只报已声明的交换机）
	// 里都看不到它们。在重挂 master **之前**收回：设备删除会顺带把 br 从它上面摘下来。
	if err := p.reclaimDerivedGatewayVRF(ctx, vs, br, vrfName); err != nil {
		return err
	}
	if err := p.ipReq(ctx, "link", "set", "dev", br, "master", vrfName); err != nil {
		return err
	}
	for _, cidr := range vs.Gateway.Addresses {
		if err := p.ipIdem(ctx, "addr", "replace", cidr, "dev", br); err != nil {
			return err
		}
	}
	return nil
}

// reclaimDerivedGatewayVRF 网关归属的目标变了、且**当前**归属正好是这台交换机**派生的**
// 网关 VRF（`vr-<交换机名>`）时，把它连域兜底路由一起回收（round51 P4）。
//
// 边界与 revokeGateway 逐字同口径：
//   - 目标就是派生名（无显式 vrf 声明）⇒ 无事可做；
//   - 当前归属是显式 `gateway vrf <名>` 的设备、或读不到归属 ⇒ 一律不动（那是操作者声明的
//     对象，生命周期不由本交换机决定；猜测式回收比留下一个未回收设备危险）；
//   - 设备不在场 ⇒ 跳过兜底回收（`ip route del vrf <名>` 在设备不存在时会打断幂等路径），
//     只留一条容忍式的 `ip link del`。
//
// 顺序与 revokeGateway/DeleteBridgeDomain 一致：先回收域兜底路由、再删设备（设备删除不会
// 连带回收 reject 路由）。
func (p *Provider) reclaimDerivedGatewayVRF(ctx context.Context, vs model.VirtualSwitch, br, target string) error {
	derived := GatewayVRFName(vs.Name)
	if target == derived {
		return nil
	}
	master, slaveKind, ok := p.linkMaster(ctx, br)
	if !ok || master == "" || !strings.EqualFold(slaveKind, "vrf") || master != derived {
		return nil
	}
	if p.vrfDevicePresent(ctx, derived) {
		if err := p.deleteVRFFallback(ctx, derived); err != nil {
			return err
		}
	}
	return p.ipBest(ctx, "link", "del", derived)
}

// pruneGatewayAddresses 删掉 bridge 上**声明里已没有**的网关地址（地址收缩，round49 外部走查）。
//
// applyGateway 对每个声明地址只做 `ip addr replace`（加/替换），旧地址不会自己消失：网关地址
// 从两个减到一个时，被删的那个继续留在 bridge 上，域内主机照旧把它当网关用——而配置与读视图
// 里都只有新的那一个。与 applyGateway 合起来即「声明集合为准」的全量语义（与本文件成员口、
// VLAN 条的收敛同一口径）。
//
// 比较前两边都归一（normCIDR）：内核实况回读的写法未必与配置逐字相同（`2001:0db8::1/64` 与
// `2001:db8::1/64` 是同一个地址），不归一会把自己刚替换上去的地址当成「已不声明」删掉。
func (p *Provider) pruneGatewayAddresses(ctx context.Context, br string, declared []string) error {
	want := make(map[string]bool, len(declared))
	for _, cidr := range declared {
		want[normCIDR(cidr)] = true
	}
	have, err := p.bridgeAddresses(ctx, br)
	if err != nil {
		return err
	}
	for _, cidr := range have {
		if want[normCIDR(cidr)] {
			continue
		}
		if err := p.ipBest(ctx, "addr", "del", cidr, "dev", br); err != nil {
			return fmt.Errorf("删除 %s 上已不声明的网关地址 %s: %w", br, cidr, err)
		}
	}
	return nil
}

// revokeGateway 撤销 L2 交换机的三层网关（`gateway` 声明消失，或声明仍在但地址集合清空）。
//
// 顺序：清地址（v4/v6 按实况逐条删）→ 退出网关 VRF 归属 → 回收**派生的**网关 VRF 设备
// （`vr-<交换机名>`，含域兜底路由）。与 VPP 侧 ApplyGateway 的撤销语义（同轮实现）对齐。
//
// 两处边界：
//   - **显式 `gateway vrf <名>` 的设备不动**：它是操作者在 vrf 段里声明的对象，生命周期不由
//     这台交换机决定（与 DeleteBridgeDomain 同口径）。判据是「bridge 当前挂在哪个 VRF 上」，
//     只有挂的正好是派生名才回收——派生名与操作者可能的同名对象（如名为 `vr-vs-lan` 的 L3
//     交换机，LinkName 后就是它）**不会**被误删。
//   - 读不到归属（linkMaster 答不出）时不猜：地址照清（那是声明外的事实），归属与设备保留，
//     由下一次按配置重放再收敛（猜测式 nomaster 会摘掉别的对象刚建立的归属，见 detachMasterIf）。
func (p *Provider) revokeGateway(ctx context.Context, vs model.VirtualSwitch, br string) error {
	have, err := p.bridgeAddresses(ctx, br)
	if err != nil {
		return err
	}
	for _, cidr := range have {
		if err := p.ipBest(ctx, "addr", "del", cidr, "dev", br); err != nil {
			return fmt.Errorf("撤销网关：删除 %s 上的地址 %s: %w", br, cidr, err)
		}
	}
	master, slaveKind, ok := p.linkMaster(ctx, br)
	if !ok || master == "" || !strings.EqualFold(slaveKind, "vrf") {
		return nil // 没有 VRF 归属（或读不到）：地址已清，其余不在本语句的处置范围
	}
	if err := p.ipBest(ctx, "link", "set", "dev", br, "nomaster"); err != nil {
		return err
	}
	if master != GatewayVRFName(vs.Name) {
		return nil // 显式/共享 VRF：归属已摘，设备留给它的属主
	}
	// 派生网关 VRF 随声明消失而回收：先回收域兜底路由、再删设备（与 DeleteVRF/DeleteBridgeDomain
	// 同序——设备删除不会连带回收 reject 路由）。此处设备必然在场（bridge 刚挂在它上面），
	// 故不需要 vrfDevicePresent 守卫。
	if err := p.deleteVRFFallback(ctx, master); err != nil {
		return err
	}
	return p.ipBest(ctx, "link", "del", master)
}

// bridgeAddrRow `ip -j addr show dev <名>` 的一行（本文件只取地址、前缀长与 scope）。
//
// 不复用 l3.go 的 ipAddrRow：那里刻意只要地址（删除路径上的宽容读取），而这里必须看 scope
// ——内核自动生成的 IPv6 链路本地地址（fe80::/64，scope=link）不属于网关声明，绝不能被
// 撤销/收缩当成「声明外的旧地址」删掉（删掉不会自动重生，会静默打瘸 bridge 的 v6 邻居发现）。
type bridgeAddrRow struct {
	AddrInfo []struct {
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
		Scope     string `json:"scope"`
	} `json:"addr_info"`
}

// bridgeAddresses 读回某设备当前**属于本产品管理范围**的地址（CIDR 形态，比较的是集合而非写法）：
// scope=link 的（IPv6 链路本地、IPv4 的 169.254.0.0/16）一律排除——它们是内核按设备/协议栈
// 自动生成的，不代表任何声明（见 bridgeAddrRow）。
//
// 与 deviceAddresses（宽容版：删除路径把「读不到」当「没有」）不同，本函数**读失败上抛**：
// 网关撤销/收缩若把「问不出来」当成「没有旧地址」，就会静默留下已删网关的地址——那正是
// round49 修的那类「配置与数据面不一致、全程无报错」。
func (p *Provider) bridgeAddresses(ctx context.Context, dev string) ([]string, error) {
	out, err := p.ip(ctx, "-j", "addr", "show", "dev", dev)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 的地址: %w", dev, err)
	}
	if strings.TrimSpace(out) == "" {
		// iproute2 成功时至少打印 `[]`；**空白输出**只出现在读不到现场的情形（设备不在场等）。
		// 这条容忍不会造成误删（没有任何可删的名字），而上抛会让「设备已不在」的幂等重入
		// （恢复重放、重复提交）整条失败；真失败（命令非零退出、输出不可解析）照旧上抛。
		return nil, nil
	}
	var rows []bridgeAddrRow
	if json.Unmarshal([]byte(out), &rows) != nil {
		return nil, fmt.Errorf("解析 %s 的地址失败（%s）", dev, trimOut(out))
	}
	var cidrs []string
	for _, r := range rows {
		for _, a := range r.AddrInfo {
			if a.Local == "" || strings.EqualFold(a.Scope, "link") {
				continue
			}
			cidrs = append(cidrs, fmt.Sprintf("%s/%d", a.Local, a.PrefixLen))
		}
	}
	return cidrs, nil
}

// normCIDR 归一 CIDR 文本，只用于「是不是同一个地址」的比较（解析不出来时按原样返回，不猜）。
func normCIDR(s string) string {
	if pre, err := netip.ParsePrefix(strings.TrimSpace(s)); err == nil {
		return pre.String()
	}
	return s
}

// DeleteBridgeDomain 删除内核 bridge（成员口由内核自动释放），并回收**它自己创建的**网关 VRF。
//
// 网关 VRF 由 ApplyBridgeDomain 按需创建（无显式 vrf 声明时用派生的 `vr-<交换机名>`）——
// 删交换机时必须一并回收，否则内核里长期留一张空 VRF（真机走查实测的残留：
// 删了 vs-lan，`vr-vs-lan` 还在）。显式声明的 `gateway vrf <名>` 不在此删除：
// 那是操作者自己的对象，生命周期不由本交换机决定。网关 VRF 表内的域兜底路由（决策 #446，
// v4/v6 两族）同样随设备一并回收（先删路由、再删设备）。
//
// 只对 **L2** 交换机成立：名字在内核里是一台 VRF 设备（＝L3 交换机，设备名与交换机同名）
// 时拒绝删除并指向 L3 交换机的删除路径（round51 P2，见函数体首段）。
func (p *Provider) DeleteBridgeDomain(ctx context.Context, name string) error {
	// round51 P2 的兜底闸：**本方法只删 bridge**。L3 交换机（type=l3）与它同名的那台内核设备
	// 是 ApplyVRF 建的 **VRF 设备**（`LinkName(vrf.Name)`），对这样的名字执行 `ip link del`
	// 会把 VRF 连兜底路由一起凭空删掉，而随后按 VRF 语义的清理（成员口地址/子接口/兜底路由）
	// 全部查不到对象、静默跳过 ⇒ 数据面留下了「表在、路由在、成员地址在」的残渣且无人报错。
	// 调用方（teardown 段）已按声明类型过滤，这里再核一次**内核实况**——闸门设在被删对象本身，
	// 任何调用方（含将来的新调用点）都绕不过去。读不到设备类型（不存在/读失败）照旧放行：
	// 此时没有任何设备会被这条命令删掉，幂等语义不变。
	if row, ok := p.linkDetail(ctx, LinkName(name)); ok && row.LinkInfo != nil &&
		strings.EqualFold(row.LinkInfo.InfoKind, "vrf") {
		return fmt.Errorf("内核里的 %s 是一台 VRF 设备（L3 交换机），不是 L2 bridge；"+
			"本方法只删 L2 交换机。请按删除 L3 交换机的方式回收它（delete virtual-switches %s），"+
			"那样才会连成员口地址、vlan 子接口与域兜底路由一并清理", LinkName(name), name)
	}
	// 决策 #438：先停该交换机的 DHCP 服务器——内置 tap 是 bridge 成员口、单播接收 socket 绑在
	// bridge 的 BVI 地址上（reference 先解、被引用者后删，与 #437 relay/#196/#342 的删除倒序一致）。
	// 未启用/从未收敛时幂等空操作。
	srv, hub := p.dhcpComponents()
	if err := hub.Stop(name); err != nil {
		return err
	}
	if err := srv.Sync(ctx, model.VirtualSwitch{Name: name}); err != nil {
		return err
	}
	// 决策 #437：先停该交换机的 DHCP 中继实例（它引用 bridge 的收发路径与 BVI 地址）——
	// 先解引用、后删被引用，与 #196/#342 的删除倒序一致。无实例/未声明时幂等空操作。
	if err := p.relayMgr().Stop(name); err != nil {
		return err
	}
	if err := p.ipBest(ctx, "link", "del", LinkName(name)); err != nil {
		return err
	}
	// 决策 #446：网关域 VRF 表内也有域兜底路由（ApplyBridgeDomain → applyGateway → ensureVRF），
	// 删 VRF 设备前先回收它——不把孤立路由留给内核。设备不在场（无网关/显式 gateway vrf/
	// 已删过/同名设备不是 VRF）时跳过：对不存在的 VRF 下 `ip route del` 会报 Invalid VRF，
	// 会把「删已删过的交换机」的幂等路径打断。
	gwVrf := GatewayVRFName(name)
	if p.vrfDevicePresent(ctx, gwVrf) {
		if err := p.deleteVRFFallback(ctx, gwVrf); err != nil {
			return err
		}
	}
	return p.ipBest(ctx, "link", "del", gwVrf)
}
