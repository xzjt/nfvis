package network

// M3-3/M3-4：把 L2/L3 编排接入 orchestrator.NetworkProvider（其余沿用基础实现）。

import (
	"context"
	"fmt"
	"sync"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/state"
)

// InterfaceCounterReader 接口 rx 计数读物（决策 #337 判据③的成员口风暴判定）。
// 复用 #326 的运行态读数路径 `state.Runtime.InterfaceCounters`（stats segment），
// 不新造 VPP 查询；ok=false 表示读数不可用（stats 未接入/接口不在数据面），
// 调用方据此如实报错、放弃该轮判定（不误报）。
type InterfaceCounterReader interface {
	InterfaceCounters(ctx context.Context, ifname string) (state.InterfaceCounters, bool)
}

// L2Network 在基础 NetworkProvider 上覆盖 L2 虚拟交换机与 L3/VRF 编排。
type L2Network struct {
	orchestrator.NetworkProvider // 其余方法（ACL/NAT/SPAN/QoS）沿用基础实现
	l2                           *L2Provider
	l3                           *L3Provider
	svc                          *ServicesProvider
	acl                          *AclProvider
	nat                          *NatProvider
	bond                         *BondProvider
	lldp                         *LldpProvider
	dhcp                         *DhcpProvider          // 交换机 DHCP 中继（决策 #335，可空——未注入即无 relay 编排）
	dhcpServer                   *DHCPServerProvider    // 域内 DHCP 服务器（决策 #359，可空——未注入即无 server 编排）
	dns                          *DNSProxyProvider      // 数据面 DNS 代理（决策 #345，可空）
	vhost                        *VhostUserProvider     // M4-4：VNF vNIC 接入
	memif                        *MemifProvider         // M4-7：容器 vNIC 接入
	vhostDir                     string                 // vhost-user socket 目录（恢复收敛重放用）
	memifDir                     string                 // memif socket 目录
	alarms                       *AlarmStore            // 恢复收敛失败项落点（M3-8，可空）
	loop                         *loopDetector          // 采样式 L2 环路检测状态（决策 #337，进程内）
	counters                     InterfaceCounterReader // 成员口 rx 计数读物（判据③，可空——未注入即跳过该判据）
	sriov                        *SRIOVProvider         // PF 的 VF 数量（声明式 vf-count，决策 #70）
	// runtimeMu 串行化「进程内登记失效」与「NAT 下发」：失效清的是 NAT inside/outside 的解析
	// 来源（L3 侧登记），若与一次 ApplyNAT 交错，那次下发会按「空 inside」算期望集——
	// 少下发特性，甚至把既有 inside 特性当成配置里已删的项删掉。
	runtimeMu sync.Mutex
}

// NewL2Network 以基础 Provider 与 L2 编排器构造装饰器。
func NewL2Network(base orchestrator.NetworkProvider, l2 *L2Provider) *L2Network {
	if base == nil {
		base = orchestrator.NewNoopNetwork()
	}
	return &L2Network{NetworkProvider: base, l2: l2, loop: newLoopDetector(),
		vhostDir: orchestrator.DefaultVhostDir, memifDir: orchestrator.DefaultMemifDir}
}

// SetSocketDirs 设置 vNIC socket 目录（须与 applier/compute/container 一致，恢复收敛重放用）。
func (n *L2Network) SetSocketDirs(vhostDir, memifDir string) {
	if vhostDir != "" {
		n.vhostDir = vhostDir
	}
	if memifDir != "" {
		n.memifDir = memifDir
	}
}

// SetL3 追加 L3/VRF 编排（BVI 网关随 L2 交换机一并处理）。
func (n *L2Network) SetL3(l3 *L3Provider) {
	n.l3 = l3
	// NAT outside 转发域来源（决策 #52）；SetNAT 亦会注入，二者顺序无关。
	if n.nat != nil {
		n.nat.SetOutsideResolver(l3.TableOfIface)
	}
}

// SetServices 追加 SPAN/QoS/接口编排（M3-5）。
func (n *L2Network) SetServices(svc *ServicesProvider) { n.svc = svc }

// SetACL 追加 ACL 编排（M3-5 二），并把索引查询注入 L2/L3 以绑定端口/接口。
func (n *L2Network) SetACL(a *AclProvider) {
	n.acl = a
	if n.l2 != nil {
		n.l2.SetACL(a)
	}
	if n.l3 != nil {
		n.l3.SetACL(a)
	}
}

// SetNAT 追加 NAT44 编排（M3-5 三），并把 L2 挂接表注入为 inside 接口来源。
func (n *L2Network) SetNAT(p *NatProvider) {
	n.nat = p
	if p != nil && n.l3 != nil {
		p.SetInsideResolver(n.l3.AttachedIfaces) // NAT 仅作用于 L3 交换机
		p.SetOutsideResolver(n.l3.TableOfIface)  // outside 转发域来自出接口所属 VRF（决策 #52）
	}
}

// SetBond 追加 bond 编排（M3-6）。
func (n *L2Network) SetBond(p *BondProvider) { n.bond = p }

// SetDhcp 追加交换机 DHCP 中继编排（决策 #335；未注入时 relay 语句在提交校验层仍可配，
// 但数据面无下发路径——恢复收敛会如实记未收敛项，正常装配总是注入）。
func (n *L2Network) SetDhcp(p *DhcpProvider) { n.dhcp = p }

// SetDHCPServer 追加域内 DHCP 服务器编排（决策 #359；未注入时 dhcp-server 语句在提交校验层
// 仍可配，但数据面无下发路径——恢复收敛会如实记未收敛项，正常装配总是注入）。
// 注入的同时接上两样共享设施：
//   - 「sw_if_index → 所属交换机」反查（L3Provider 的 ForwardDomainOf，BVI 已登记为转发域）：
//     punt 单播路径按上行 desc.sw_if_index 派发到对应交换机的服务器；
//   - 告警表（池耗尽告警 DHCP_POOL_EXHAUSTED 的落点；L2Network.SetAlarms 之后再注入也无妨，
//     本方法只把已知的两样接过去，告警表单独经 p.SetAlarms 注入）。
func (n *L2Network) SetDHCPServer(p *DHCPServerProvider) {
	n.dhcpServer = p
	if p == nil {
		return
	}
	p.SetSwitchResolver(func(idx uint32) (string, bool) {
		if n.l3 == nil {
			return "", false
		}
		return n.l3.ForwardDomainOf(idx)
	})
	if n.alarms != nil {
		p.SetAlarms(n.alarms)
	}
}

// SetDNSProxy 追加数据面 DNS 代理编排（决策 #345；未注入时上游语句在提交校验层仍可配，
// 但数据面无下发路径）。注入的同时把「sw_if_index → 所属交换机」的反查来源接上（L3Provider），
// 供转发器按上行包来源选按域上游——未接 L3 时反查恒 false（一律回落全局）。
func (n *L2Network) SetDNSProxy(p *DNSProxyProvider) {
	n.dns = p
	if p != nil {
		p.SetSwitchResolver(func(idx uint32) (string, bool) {
			if n.l3 == nil {
				return "", false
			}
			return n.l3.ForwardDomainOf(idx)
		})
	}
}

// ApplyDNSProxy 收敛数据面 DNS 代理声明（决策 #345）。调用时机：提交编排把它作为交换机/VRF
// 之后的伴随操作（全局或任一交换机非空 ⇒ 注册 punt + 起转发器；全空 ⇒ 注销）；恢复收敛的重放
// 走 recovery.go 的独立记源。未注入 provider 时为空操作（noop/无 VPP 路径）。
func (n *L2Network) ApplyDNSProxy(ctx context.Context, want orchestrator.DNSProxyUpstreams) error {
	if n.dns == nil {
		return nil
	}
	return n.dns.Sync(ctx, want)
}

// SetLldp 追加 LLDP 编排（M3-6）。
func (n *L2Network) SetLldp(p *LldpProvider) { n.lldp = p }

// SetVhostUser 追加 VNF vNIC（vhost-user）接入编排（M4-4）。
func (n *L2Network) SetVhostUser(p *VhostUserProvider) { n.vhost = p }

// SetMemif 追加容器 vNIC（memif）接入编排（M4-7）。
func (n *L2Network) SetMemif(p *MemifProvider) { n.memif = p }

// SetSRIOV 注入 SR-IOV VF 数量编排（声明式 interfaces[].sriov.vf_count）。
func (n *L2Network) SetSRIOV(p *SRIOVProvider) { n.sriov = p }

// SetCounters 注入接口 rx 计数读物（决策 #337 判据③的成员口风暴判定）。
// 未注入时该判据静默跳过（与未注入告警表同口径）。
func (n *L2Network) SetCounters(r InterfaceCounterReader) { n.counters = r }

// InvalidateRuntimeState 让状态型子编排器的进程内登记失效（VPP 连接（重）建立时调用）。
//
// 语义（round84 收尾后固化，只此一条）：**它只让下一次收敛做全量重放**，不触碰 VPP、不改配置。
// 带外 `systemctl restart vpp` 会清空 VPP 侧配置，而进程内登记仍在：ApplyNAT 会认为
// 「已下发」而跳过重放（inside 特性、地址池、接口地址全都不在下发的 VPP 里），NAT 静默
// 失效，必须重启 nfvisd 才恢复（round84 R84-21）。失效后随后的恢复收敛做一次全量重放。
//
// 「失效后派生查询（AttachedIfaces/TableOfIface）可能返回空集」这件事必须安全，靠两条不变式：
//  1. 失效与 ApplyNAT 由 runtimeMu 互斥，且先清消费方（NAT）再清来源方（L3）：任何一次
//     ApplyNAT 要么看到完整的旧登记、要么看到完整的空登记，不会拿到「inside 空、特性却在」
//     的中间态；
//  2. ApplyNAT 的插件开关按**配置声明**判定（nat.go），空解析只会少下发几条特性，绝不关闭插件；
//     恢复收敛还会在 VRF 落地之后、ApplyNAT 之前按配置重建这三张表（l3.go RegisterL3Interfaces
//     + SetVnfTable），故一次完整收敛之后派生查询必然答得出配置声明的接口。
//
// 为何是「清空 + 重建」而不是「保留这些登记、只清 NAT 特性/池缓存」：登记里的 sw_if_index 在
// VPP 重启后会变（接口重新枚举），保留旧索引会让 NAT 去操作已不存在的口（VPP 报 -6 No such
// entry）、把整批 apply 打回滚——那正是本轮在修的 round84 缺陷 B 的形态。清空后按配置重建，
// 拿到的一定是当前运行态的索引。
//
// 与「只补齐不摘除」（附录 A #35）不冲突：失效只清进程内登记，重放只发 add、不发 del；
// 重放里 add 方向的「已存在」按成功处理（见 nat_govpp.go），故失效 + 重放可安全重复执行。
func (n *L2Network) InvalidateRuntimeState() {
	if n == nil {
		return
	}
	n.resetProviders()
}

// VnfPortIfaceName 由 vNIC 端口派生其在 VPP 中的确定性接口名（vhost-user 与 memif 各自
// 规则在 orchestrator/model 侧唯一真源，此处只做选择，供接入/删除/恢复收敛共用）。
func VnfPortIfaceName(port orchestrator.VnfPort) string {
	if port.Type == "memif" {
		return orchestrator.MemifIfaceName(port.VM, port.Interface)
	}
	return orchestrator.VnfIfaceName(port.VM, port.Interface)
}

// vnfPortProviderReady 该类型的 vNIC 接入编排是否已装配。未装配 = 本部署不管这类 vNIC
// （如未接入 VPP 的部署：该类型的接口也不会存在），与 ApplyVnfInterface 的早退口径一致——
// 恢复收敛的登记重建对这类端口同样跳过，不制造无意义的接口缺失告警。
//
// sriov-vf 恒为 false：VF 直通不过 VPP，VPP 侧没有该 vNIC 的接口，把它的确定性名拿去
// 解析/置表只会得到一条永不收敛的告警（配置上它仍可声明 virtual-switch，仅作登记）。
func (n *L2Network) vnfPortProviderReady(portType string) bool {
	switch portType {
	case "memif":
		return n.memif != nil
	case "vhost-user":
		return n.vhost != nil
	default:
		return false
	}
}

// ApplyVnfInterface 建立 vNIC 接入（FR-NET-020/021/023）：
//   - vhost-user：VPP 建 server socket 接口并命名，交换机端口随后按名挂接；
//     若 vNIC 指向 L3 交换机（port.VRF 非空），再将该接口置入对应 VRF 表；
//   - sriov-vf：不经 VPP（VF 直通，hostdev 由 compute 组装）；
//   - memif：容器 vNIC，由容器编排在 M4-7 处理。
func (n *L2Network) ApplyVnfInterface(ctx context.Context, port orchestrator.VnfPort) error {
	switch port.Type {
	case "vhost-user":
		if n.vhost == nil {
			return nil // 未接入 VPP：由 noop/基础实现决定（保持 M1 语义不报错）
		}
		if err := n.vhost.Apply(ctx, port); err != nil {
			return err
		}
		if port.VRF != "" && n.l3 != nil {
			return n.l3.SetVnfTable(ctx, port.VRF, VnfPortIfaceName(port))
		}
		return nil
	case "memif":
		if n.memif == nil {
			return nil
		}
		if err := n.memif.Apply(ctx, port); err != nil {
			return err
		}
		if port.VRF != "" && n.l3 != nil {
			return n.l3.SetVnfTable(ctx, port.VRF, VnfPortIfaceName(port))
		}
		return nil
	case "sriov-vf":
		return nil
	default:
		return fmt.Errorf("vNIC %s/%s 类型 %q 不受支持", port.VM, port.Interface, port.Type)
	}
}

// DeleteVnfInterface 删除 vNIC 接入（VM 删除/vNIC 移除/迁移时同步 VPP 侧）。
func (n *L2Network) DeleteVnfInterface(ctx context.Context, owner, ifaceName string) error {
	// vhost-user 与 memif 接口按各自确定性命名删除；未接入的一侧为 nil 时跳过（幂等）。
	if n.vhost != nil {
		if err := n.vhost.Delete(ctx, owner, ifaceName); err != nil {
			return err
		}
	}
	if n.memif != nil {
		if err := n.memif.Delete(ctx, owner, ifaceName); err != nil {
			return err
		}
	}
	// VRF/NAT inside 登记随接口一并摘除：接口已从数据面消失，残留的 sw_if_index 会让
	// NAT inside 指向不存在的口（下一次 ApplyNAT 直接失败，且会去删一个已消失的接口）。
	// 两个确定性名都试，与删除同样幂等；NAT 侧同一索引的登记一并摘掉。
	if n.l3 != nil {
		for _, name := range []string{
			orchestrator.VnfIfaceName(owner, ifaceName),
			orchestrator.MemifIfaceName(owner, ifaceName),
		} {
			if idx, ok := n.l3.ForgetVnfIface(name); ok && n.nat != nil {
				n.nat.ForgetIface(idx)
			}
		}
	}
	return nil
}

// VnfPortLinkState 查询 vNIC 接口链路状态（FR-NET-023；供告警/运行态）。
func (n *L2Network) VnfPortLinkState(ctx context.Context, vmName, ifaceName string) (exists, up bool, err error) {
	if n.vhost == nil {
		return false, false, nil
	}
	return n.vhost.LinkState(ctx, vmName, ifaceName)
}

func (n *L2Network) ApplyBond(ctx context.Context, bond model.Bond) error {
	if n.bond == nil {
		return nil
	}
	return n.bond.ApplyBond(ctx, bond)
}

func (n *L2Network) DeleteBond(ctx context.Context, name string) error {
	if n.bond == nil {
		return nil
	}
	return n.bond.DeleteBond(ctx, name)
}

func (n *L2Network) ApplyLLDP(ctx context.Context, lldpCfg *model.LldpConfig) error {
	if n.lldp == nil {
		return nil
	}
	return n.lldp.ApplyLLDP(ctx, lldpCfg)
}

// LldpNeighbors 供 /protocols/lldp/neighbors 运行态（M3-6）。
func (n *L2Network) LldpNeighbors(ctx context.Context) ([]LldpNeighbor, error) {
	if n.lldp == nil {
		return nil, nil
	}
	return n.lldp.Neighbors(ctx)
}

func (n *L2Network) ApplyNAT(ctx context.Context, nat model.NatConfig) error {
	if n.nat == nil {
		return nil
	}
	// 与登记失效互斥（见 runtimeMu 与 InvalidateRuntimeState）：失效清的是本次下发要读的
	// inside/outside 解析来源，交错会让本次按「空转发域」算期望集。
	n.runtimeMu.Lock()
	defer n.runtimeMu.Unlock()
	return n.nat.ApplyNAT(ctx, nat)
}

// NATSessions 供 /nat 运行态展示（M3-7）。
func (n *L2Network) NATSessions(ctx context.Context) ([]NATSession, error) {
	if n.nat == nil {
		return nil, nil
	}
	return n.nat.Sessions(ctx)
}

func (n *L2Network) ApplyACL(ctx context.Context, acl model.Acl) error {
	if n.acl == nil {
		return nil
	}
	return n.acl.ApplyACL(ctx, acl)
}

func (n *L2Network) DeleteACL(ctx context.Context, name string) error {
	if n.acl == nil {
		return nil
	}
	return n.acl.DeleteACL(ctx, name)
}

func (n *L2Network) ApplyInterface(ctx context.Context, iface model.InterfaceConfig) error {
	// 声明式 VF 数量（FR-NET-004）：配置里写了就必须落实——失败即报错，
	// 不得静默无操作（决策 #70；此前 vf_count 被持久化却无人执行）。
	if iface.Sriov != nil {
		if n.sriov == nil {
			return fmt.Errorf("接口 %s 配置了 sriov.vf-count=%d 但 SR-IOV 未接入（编排器未装配）",
				iface.Name, iface.Sriov.VFCount)
		}
		if err := n.sriov.SetVFCount(ctx, iface.Name, iface.Sriov.VFCount); err != nil {
			return fmt.Errorf("接口 %s 设置 VF 数量 %d: %w", iface.Name, iface.Sriov.VFCount, err)
		}
	}
	if n.svc == nil {
		return n.NetworkProvider.ApplyInterface(ctx, iface)
	}
	return n.svc.ApplyInterface(ctx, iface)
}

func (n *L2Network) ApplySpan(ctx context.Context, pm model.PortMirroring) error {
	if n.svc == nil {
		return nil
	}
	return n.svc.ApplySpan(ctx, pm)
}

func (n *L2Network) DeleteSpan(ctx context.Context, name string) error {
	if n.svc == nil {
		return nil
	}
	return n.svc.DeleteSpan(ctx, name)
}

func (n *L2Network) ApplyQos(ctx context.Context, q model.QosPolicy) error {
	if n.svc == nil {
		return nil
	}
	return n.svc.ApplyQos(ctx, q)
}

func (n *L2Network) DeleteQos(ctx context.Context, name string) error {
	if n.svc == nil {
		return nil
	}
	return n.svc.DeleteQos(ctx, name)
}

func (n *L2Network) ApplyBridgeDomain(ctx context.Context, vs model.VirtualSwitch) error {
	if err := n.l2.ApplyBridgeDomain(ctx, vs); err != nil {
		return err
	}
	if n.l3 != nil {
		return n.l3.ApplyGateway(ctx, vs) // 有 Gateway 才动作（FR-NET-014）
	}
	return nil
}

func (n *L2Network) DeleteBridgeDomain(ctx context.Context, name string) error {
	// 决策 #359：先回收该交换机的 DHCP 服务器——内置 tap 是 BD 成员口（先解引用、后删被引用，
	// 与 #196/#342 同口径），且停用即清租约文件；未启用时幂等空操作。
	if n.dhcpServer != nil {
		if err := n.dhcpServer.Sync(ctx, model.VirtualSwitch{Name: name}); err != nil {
			return err
		}
	}
	// 决策 #335：先撤 DHCP 中继（proxy 引用该域的表与 BVI 地址），再拆网关与 BD——
	// 与「先解引用、后删被引用」的删除顺序一致。无登记时幂等空操作。
	if n.dhcp != nil {
		if err := n.dhcp.DeleteRelay(ctx, name); err != nil {
			return err
		}
	}
	// 先删 BVI 网关（其 BD 成员身份随之消失），再删 BD：BD 仍有成员时
	// VPP 拒绝删除（-120）；l2 摘除已失效成员（BVI/vhost）按已摘除处理。
	if n.l3 != nil {
		if err := n.l3.DeleteGateway(ctx, name); err != nil {
			return err
		}
	}
	return n.l2.DeleteBridgeDomain(ctx, name)
}

// ApplyDhcpRelay 收敛一台交换机的 DHCP 中继声明（决策 #335）。调用时机：
// 提交编排把它作为 bridge-domain **之后**的伴随操作（proxy 的表 id 与中继源地址来自网关声明，
// 先有 BVI 地址与表才有 relay）；恢复收敛的重放走 recovery.go 交换机段的独立记源。
// 未注入 DhcpProvider 时为空操作（noop/无 VPP 路径）。
func (n *L2Network) ApplyDhcpRelay(ctx context.Context, vs model.VirtualSwitch) error {
	if n.dhcp == nil {
		return nil
	}
	return n.dhcp.SyncRelay(ctx, vs)
}

// ApplyDHCPServer 收敛一台交换机的 DHCP 服务器声明（决策 #359）。调用时机：
// 提交编排把它作为 bridge-domain（与 dhcp-relay）**之后**的伴随操作（tap 入 BD、server-id/网关
// 都来自网关声明，先有 BVI 地址与 BD 才有 server）；恢复收敛的重放走 recovery.go 的独立记源。
// 未启用（无池）的声明＝teardown（删 tap/注销注册/清租约文件）。未注入 provider 时空操作。
func (n *L2Network) ApplyDHCPServer(ctx context.Context, vs model.VirtualSwitch) error {
	if n.dhcpServer == nil {
		return nil
	}
	return n.dhcpServer.Sync(ctx, vs)
}

// DHCPServerLeases 某交换机的租约表读视图（决策 #359；供 API/CLI 运行态读物）。
// ok=false = 该交换机没有运行中的服务器（未配置/未收敛）。
func (n *L2Network) DHCPServerLeases(name string) ([]DHCPLease, bool) {
	if n.dhcpServer == nil {
		return nil, false
	}
	return n.dhcpServer.Leases(name)
}

// DHCPServerActiveLeases 生效租约数（state=active；ok=false 同 DHCPServerLeases）。
func (n *L2Network) DHCPServerActiveLeases(name string) (int, bool) {
	if n.dhcpServer == nil {
		return 0, false
	}
	return n.dhcpServer.ActiveLeases(name)
}

// DHCPTapIndexes 产品自持的内置 DHCP tap 的 sw_if_index 集合（决策 #359：端口读视图按它
// 过滤内置 tap——**不用名字匹配**，用户不可见/不可删）。
func (n *L2Network) DHCPTapIndexes() map[uint32]bool {
	if n.dhcpServer == nil {
		return nil
	}
	return n.dhcpServer.TapIndexes()
}

// ReconcileDHCPServer DHCP 服务器的 15s 巡检收敛（决策 #359；供巡检与残渣对账同块调用）：
// 对配置里启用的交换机做幂等 Sync（补齐带外丢失的 tap/注册）、到期租约回收与池耗尽告警复核。
func (n *L2Network) ReconcileDHCPServer(ctx context.Context, cfg model.Config) []error {
	if n.dhcpServer == nil {
		return nil
	}
	return n.dhcpServer.Reconcile(ctx, cfg)
}

// ReconcileProxy DHCP 中继 proxy 的巡检对账（决策 #380/R140-1；供 15s 巡检与残渣对账同块调用）：
// 从 cfg.VirtualSwitches 取「声明了 relay」的集合，交给 DhcpProvider 与 VPP 实际条目比对，
// 清除未声明/陈旧的多余 proxy 条目（不靠进程内登记，跨 nfvisd 重启仍有效）。未注入 provider 时空操作。
func (n *L2Network) ReconcileProxy(ctx context.Context, cfg model.Config) []error {
	if n.dhcp == nil {
		return nil
	}
	var declared []model.VirtualSwitch
	for _, vs := range cfg.VirtualSwitches {
		if vs.DhcpRelayServer != "" {
			declared = append(declared, vs)
		}
	}
	if err := n.dhcp.ReconcileProxy(declared); err != nil {
		return []error{err}
	}
	return nil
}

func (n *L2Network) ApplyVRF(ctx context.Context, vrf model.Vrf) error {
	if n.l3 == nil {
		return nil
	}
	return n.l3.ApplyVRF(ctx, vrf)
}

func (n *L2Network) DeleteVRF(ctx context.Context, name string) error {
	if n.l3 == nil {
		return nil
	}
	return n.l3.DeleteVRF(ctx, name)
}

// RetryDeferredVRFDeletes 复核「删表延后」的 L3 交换机（决策 #192），返回本轮**确认已清理**的
// 交换机名，并把与之相关的提交期告警（延后 + 补偿残渣）清掉。
//
// 调用时机：恢复收敛（VPP 重启后的重连）与周期巡检。cfg 是 committed 配置——表被重新声明
// 回来时该项本来就是合法存在，登记与告警一并清除；否则以「表在不在 VPP 里」为唯一判据。
func (n *L2Network) RetryDeferredVRFDeletes(ctx context.Context, cfg model.Config) []string {
	if n == nil || n.l3 == nil {
		return nil
	}
	declared := make(map[string]bool, len(cfg.Vrfs))
	for _, v := range cfg.Vrfs {
		declared[v.Name] = true
	}
	cleared := n.l3.RetryPendingDeletes(ctx, func(name string) bool { return declared[name] })
	if n.alarms != nil {
		for _, name := range cleared {
			n.alarms.Resolve(orchestrator.CommitScope, orchestrator.CommitVrfDeleteDeferred, name)
			// 同一张表的补偿残渣告警一并消掉：表没了/被重新声明，那条残渣已不成立。
			for _, desc := range []string{orchestrator.VrfOpDesc(name), orchestrator.VrfDeleteOpDesc(name)} {
				n.alarms.Resolve(orchestrator.CommitScope, orchestrator.CommitCompensationFailed, desc)
			}
		}
	}
	return cleared
}

// ApplyRoute/DeleteRoute 单条静态路由的下发与撤销：撤销由提交编排按「旧/新声明差集」下发
// （删除路径此前整条漏，见 internal/orchestrator/apply.go 的 del-route 计划操作）。
func (n *L2Network) ApplyRoute(ctx context.Context, vrfName string, r model.Route) error {
	if n.l3 == nil {
		return nil
	}
	return n.l3.ApplyRoute(ctx, vrfName, r)
}

func (n *L2Network) DeleteRoute(ctx context.Context, vrfName string, r model.Route) error {
	if n.l3 == nil {
		return nil
	}
	return n.l3.DeleteRoute(ctx, vrfName, r)
}

// UnbindL3IfaceACL 撤销一条 L3 接口的 acl-in 绑定与伴随 macip（决策 #361）。调用时机：提交编排
// 在「接口仍在声明里、acl-in 被清」时构造 l3-acl-unbind 计划操作（在本 VRF 的 ApplyVRF 之后、
// 删除段之前）。未注入 L3 编排时为空操作（noop/无 VPP 路径）。
func (n *L2Network) UnbindL3IfaceACL(ctx context.Context, vrfName string, li model.L3Interface) error {
	if n.l3 == nil {
		return nil
	}
	return n.l3.UnbindL3IfaceACL(ctx, vrfName, li)
}

// DeleteL3Interface 回收一条已从声明里删除的 L3 接口（决策 #361：清地址 → 解绑 ACL/伴随 macip
// → 移回默认表 → 摘登记）。调用时机：提交编排的 del-l3-if 计划操作。未注入 L3 编排时空操作。
func (n *L2Network) DeleteL3Interface(ctx context.Context, vrfName string, li model.L3Interface) error {
	if n.l3 == nil {
		return nil
	}
	return n.l3.DeleteL3Interface(ctx, vrfName, li)
}

// MACTable 供 /virtual-switches/{name}/mac-table 运行态查询（M3-3）。
func (n *L2Network) MACTable(ctx context.Context, name string) ([]MACTableEntry, error) {
	return n.l2.MACTable(ctx, name)
}

// Routes 供 /vrfs/{name}/routes 运行态查询（M3-4）。
func (n *L2Network) Routes(ctx context.Context, name string) ([]RouteEntry, error) {
	if n.l3 == nil {
		return nil, ErrL3Unavailable
	}
	return n.l3.Routes(ctx, name)
}
