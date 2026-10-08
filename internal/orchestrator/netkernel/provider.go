package netkernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// Provider Linux 内核网络数据面的 orchestrator.NetworkProvider 实现。
//
// 并发安全：内部以 mu 串行化所有下发（提交编排本就串行，恢复巡检与提交可能并发）。
// 与 VPP 实现不同，本实现**不做进程内登记**——内核是唯一事实源，读视图直接查内核
// （`ip`/`bridge`/`nft`），故 nfvisd 重启后无需重放即自洽。
type Provider struct {
	run Runner
	mu  sync.Mutex

	// cfgMu/cfg 最近一次收敛（或装配时载入）的配置快照：无参读视图（VPPIfnames）用它判定
	// 哪些内核设备是产品自持的（宿主机上 virbr0/docker0 同样是 bridge，按设备类型选会误判）。
	//
	// cfgSrc 是可选来源（SetConfigSource 注入）：装配处接 `engine.Committed` 后，config() 每次
	// 取**当前** committed，提交后的读视图不再有陈旧窗口；没有来源时用 cfg 快照。
	cfgMu  sync.RWMutex
	cfg    model.Config
	cfgSrc func() (model.Config, error)

	// 族管理器（惰性构造；内核是事实源，管理器本身不持有产品状态）。
	acl     *aclManager
	qos     *qosManager
	span    *spanManager
	storm   *stormManager
	portSec *portSecManager

	// alarms 告警落点（R2-6）：内核数据面此前没有任何告警——恢复未收敛项与物理口链路
	// 只进 journal，`show alarms`/Web 总览/诊断包/`/events` 全查不到，与 #191/#321/#333
	// 建立的「未收敛项必须事后可查」纪律冲突。装配期注入，未注入即只返回错误（测试/工具场景）。
	alarms *network.AlarmStore
}

// 告警作用域与码（内核数据面）。
//
// scope 取值必须与 VPP 侧**逐字一致**（network 包的 recoveryScope/ifLinkScope 是私有常量）：
// 两种实现不会同时在场，但 /alarms 的读视图与对账清警都按 scope 收敛，名字不同会各说各话。
// alarmScopeForwarding 是本侧独有的独立 scope——recovery 的 Sync 只收敛自己作用域的活动项，
// 混用会让宿主 FORWARD 策略告警在下一次恢复收敛时被误消（它跟「按配置重放」无关）。
const (
	alarmScopeRecovery   = "recovery"
	alarmScopeIfaceLink  = "interface-link"
	alarmScopeForwarding = "forwarding"

	// AlarmForwardPolicyDrop 宿主 FORWARD 链策略为 DROP（R2-2）：同 hook 的 base chain 相互独立，
	// 本产品自建链的 accept **不能**豁免它，内核数据面下数据面设备之间的转发会被整体丢掉。
	AlarmForwardPolicyDrop = "FORWARD_POLICY_DROP"
	// forwardPolicySource 该告警的固定 source（它描述的是宿主策略本身，不随某个对象变化）。
	forwardPolicySource = "host-forward-policy"
)

// SetAlarms 注入告警表（恢复收敛未收敛项、物理口链路、转发前置条件的落点；可空）。
func (p *Provider) SetAlarms(a *network.AlarmStore) { p.alarms = a }

// 编译期断言：本包必须完整实现 NetworkProvider（缺方法即编译失败，不靠运行时才发现）。
var _ orchestrator.NetworkProvider = (*Provider)(nil)

// SetConfig 记录配置快照（装配处与每次恢复收敛时调用）。
func (p *Provider) SetConfig(cfg model.Config) {
	p.cfgMu.Lock()
	p.cfg = cfg
	p.cfgMu.Unlock()
}

// SetConfigSource 注入「当前 committed 配置」的来源（可空；见 config()）。
func (p *Provider) SetConfigSource(src func() (model.Config, error)) {
	p.cfgMu.Lock()
	p.cfgSrc = src
	p.cfgMu.Unlock()
}

// config 当前配置：**优先读来源**（每次取实时值），失败回落最近一次快照。
//
// 读视图（BridgeDomains/VPPIfnames/产品自持设备判定）按配置声明枚举对象，而提交路径不经过本
// provider（Apply* 只拿到单个对象）⇒ 只靠快照的话，提交后的读视图会滞留旧快照（真机
// 3.0.5~dev1：新建 vs-lan 提交成功、`show virtual-switches` 恒空，15s 巡检也不刷新）。
// 未注入来源（单测/工具）或来源读失败时回落快照——不把「读不到配置」显示成「没有配置」。
func (p *Provider) config() model.Config {
	p.cfgMu.RLock()
	src := p.cfgSrc
	p.cfgMu.RUnlock()
	if src != nil {
		if cfg, err := src(); err == nil {
			return cfg
		}
	}
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	return p.cfg
}

// New 以给定 Runner 构造内核数据面 Provider（run 为 nil 时用真实宿主命令）。
func New(run Runner) *Provider {
	if run == nil {
		run = NewExecRunner()
	}
	return &Provider{run: run}
}

func (p *Provider) ip(ctx context.Context, args ...string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.run.Run(ctx, "ip", args...)
}

// bridge 是 VLAN 表的操作入口（**不是** `ip vlan`——`ip vlan` 不存在，真机实测
// `ip vlan del` 直接报 `Object "vlan" is unknown`）。`bridge vlan add/del` 本身幂等
// （重复 add 返回 0、删不存在的 vid 也返回 0），故这里只需透传错误。
func (p *Provider) bridge(ctx context.Context, args ...string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.run.Run(ctx, "bridge", args...)
}

// bridgeIdem 执行一条 bridge 命令：已存在/不存在都按成功处理（真机实测两者都返回 0，
// 这里只是对老版本 iproute2 的差异留冗余）。
func (p *Provider) bridgeIdem(ctx context.Context, args ...string) error {
	out, err := p.bridge(ctx, args...)
	if err == nil || alreadyExists(out, err) || notFound(out, err) {
		return nil
	}
	return fmt.Errorf("bridge %s: %w（%s）", joinArgs(args), err, trimOut(out))
}

func (p *Provider) nft(ctx context.Context, args ...string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.run.Run(ctx, "nft", args...)
}

// ipIdem 执行一条「对象可能已存在」的 ip 命令：已存在按成功处理（幂等）。
func (p *Provider) ipIdem(ctx context.Context, args ...string) error {
	out, err := p.ip(ctx, args...)
	if err == nil || alreadyExists(out, err) {
		return nil
	}
	return fmt.Errorf("ip %s: %w（%s）", joinArgs(args), err, trimOut(out))
}

// ipReq 执行一条**必须成功**的 ip 命令：任何失败都如实上报。
//
// 用于「声明要求对象存在」的下发路径（置 MTU、enslave 成员口、加地址、下发路由）——
// 这里容错会把「口名写错/口不存在」变成静默成功，正是本仓库反复强调要避免的假成功。
// ensureLinkUp 把设备置 up 并**回读确认**——写成功 ≠ 已生效。
//
// 真机教训（干净快照走查实测）：`ip link set dev <口> up` 可能返回 0 却**不生效**，因为宿主侧
// 有策略在把它压回去（该现场是 netplan 的 `activation-mode: off` → systemd-networkd
// `ActivationPolicy=always-down`）。此时若按"命令成功"继续，后面下发路由会报
// `Nexthop has invalid gateway`——**根因被埋在下游**，操作者从错误里看不出是链路没起来。
// 故这里回读确认、有限重试，仍不生效就**如实报出**并给出可照做的原因。
func (p *Provider) ensureLinkUp(ctx context.Context, dev string) error {
	const attempts = 3
	for i := 0; i < attempts; i++ {
		if err := p.ipReq(ctx, "link", "set", "dev", dev, "up"); err != nil {
			return err
		}
		up, err := p.linkAdminUp(ctx, dev)
		if err != nil {
			return nil // 读不回来就不阻塞（读视图另有如实呈现），不把"读不到"当"没起来"
		}
		if up {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("接口 %s 置 up 未生效（重试 %d 次后内核仍为 down）；"+
		"常见原因：宿主侧链路策略把它压回 down（如 systemd-networkd 的 ActivationPolicy=always-down、"+
		"netplan 的 activation-mode: off）或虚拟网卡未连接——请先解除该策略再提交", dev, attempts)
}

// linkAdminUp 设备的管理态是否已 up（IFF_UP）。读不到返回错误（与"确认为 down"区分开）。
func (p *Provider) linkAdminUp(ctx context.Context, dev string) (bool, error) {
	out, err := p.ip(ctx, "-j", "link", "show", "dev", dev)
	if err != nil {
		return false, err
	}
	var rows []struct {
		Flags []string `json:"flags"`
	}
	if json.Unmarshal([]byte(out), &rows) != nil || len(rows) == 0 {
		return false, fmt.Errorf("解析 %s 的链路状态失败", dev)
	}
	for _, f := range rows[0].Flags {
		if strings.EqualFold(f, "UP") {
			return true, nil
		}
	}
	return false, nil
}

func (p *Provider) ipReq(ctx context.Context, args ...string) error {
	out, err := p.ip(ctx, args...)
	if err == nil {
		return nil
	}
	return fmt.Errorf("ip %s: %w（%s）", joinArgs(args), err, trimOut(out))
}

// ipBest 执行一条「对象可能已不存在」的 ip 命令：不存在按已达成处理（删除/解绑等清理路径）。
func (p *Provider) ipBest(ctx context.Context, args ...string) error {
	out, err := p.ip(ctx, args...)
	if err == nil || notFound(out, err) {
		return nil
	}
	return fmt.Errorf("ip %s: %w（%s）", joinArgs(args), err, trimOut(out))
}

// ---------- 接口与聚合 ----------

// ApplyInterface 收敛物理接口声明：MTU、管理状态、描述。
//
// 内核数据面下物理口**始终留在内核**（不交 DPDK），故这里是直接改 netdev 属性；
// 地址不在此处下发（由 L3 交换机的 l3-interface 负责），与 VPP 侧口径一致。
func (p *Provider) ApplyInterface(ctx context.Context, iface model.InterfaceConfig) error {
	if iface.Name == "" {
		return fmt.Errorf("接口名不能为空")
	}
	if iface.MTU > 0 {
		if err := p.ipReq(ctx, "link", "set", "dev", iface.Name, "mtu", fmt.Sprint(iface.MTU)); err != nil {
			return err
		}
	}
	if iface.Description != "" {
		// alias 是内核侧最接近「接口描述」的可读属性（`ip link show` 可见）。
		if err := p.ipReq(ctx, "link", "set", "dev", iface.Name, "alias", iface.Description); err != nil {
			return err
		}
	}
	// 与 VPP 侧同口径：Enabled 缺省视为启用（数据口必须显式 up 才转发）。
	if iface.Enabled == nil || *iface.Enabled {
		if err := p.ensureLinkUp(ctx, iface.Name); err != nil {
			return err
		}
	} else if err := p.ipReq(ctx, "link", "set", "dev", iface.Name, "down"); err != nil {
		return err
	}
	// 声明式 VF 数量：与数据面无关的 sysfs 能力，两种数据面共用同一实现。
	if iface.Sriov != nil {
		if err := network.NewSRIOVProvider().SetVFCount(ctx, iface.Name, iface.Sriov.VFCount); err != nil {
			return fmt.Errorf("接口 %s 设置 VF 数量 %d: %w", iface.Name, iface.Sriov.VFCount, err)
		}
	}
	// 接口级绑定族（顺序与 VPP 侧一致：限速 → 风暴抑制 → 端口安全）。
	// 都要求接口已 up，故放在接口层之后。
	if err := p.applyInterfaceQoS(ctx, iface); err != nil {
		return err
	}
	if err := p.applyInterfaceStorm(ctx, iface); err != nil {
		return err
	}
	return p.applyInterfacePortSec(ctx, iface)
}

// TeardownInterface 接口元素从配置里删除时的接口级绑定回收：限速（入/出两向）、
// 风暴抑制、端口安全。MTU/描述/管理状态不动（产品从不删物理口，改回原值不是"回收"）。
func (p *Provider) TeardownInterface(ctx context.Context, iface model.InterfaceConfig) error {
	dev := LinkName(iface.Name)
	if err := p.qosMgr().Unbind(ctx, dev, "ingress"); err != nil {
		return err
	}
	if err := p.qosMgr().Unbind(ctx, dev, "egress"); err != nil {
		return err
	}
	if err := p.stormMgr().Teardown(ctx, dev); err != nil {
		return err
	}
	return p.portSecMgr().Teardown(ctx, dev)
}

// ApplyBond 收敛内核 bonding 接口（FR-NET-017）。
//
// 映射：无 lacp 声明 → `balance-xor`（静态聚合，与 VPP 静态 bond 同义）；有 lacp →
// `802.3ad`，lacp_rate 由 interval 映射（fast=1、slow=0）。成员口按声明收敛：
// 声明里的 enslave、已从声明里删掉的释放。
//
// 属性变更（mode/xmit_hash_policy/lacp_rate）不能原地改：已在场的 bond 与声明不一致时
// **删掉重建**（与 VPP 侧「lacp 变则重建」同法）。旧实现把 `File exists` 一吞了之，
// 于是改 LACP 的提交成功、内核仍是旧模式，而 bond 没有运行态读视图可发现（R2-13①）。
func (p *Provider) ApplyBond(ctx context.Context, bond model.Bond) error {
	if bond.Name == "" {
		return fmt.Errorf("bond 名不能为空")
	}
	mode, xmit, lacpRate := "balance-xor", "layer2", ""
	opts := []string{"mode", mode}
	if bond.Lacp != nil {
		mode, xmit = "802.3ad", "layer3+4"
		lacpRate = "slow"
		if bond.Lacp.Interval == "fast" {
			lacpRate = "fast"
		}
		opts = []string{"mode", mode, "xmit_hash_policy", xmit, "lacp_rate", lacpRate}
	}
	if row, ok := p.linkDetail(ctx, bond.Name); ok && bondAttrsDiffer(row.LinkInfo, mode, xmit, lacpRate) {
		if err := p.ipBest(ctx, "link", "del", bond.Name); err != nil {
			return err
		}
	}
	args := append([]string{"link", "add", "name", bond.Name, "type", "bond"}, opts...)
	if err := p.ipIdem(ctx, args...); err != nil {
		return err
	}
	if bond.MTU > 0 {
		if err := p.ipReq(ctx, "link", "set", "dev", bond.Name, "mtu", fmt.Sprint(bond.MTU)); err != nil {
			return err
		}
	}
	if bond.Description != "" {
		if err := p.ipReq(ctx, "link", "set", "dev", bond.Name, "alias", bond.Description); err != nil {
			return err
		}
	}
	declared := map[string]bool{}
	for _, m := range bond.Members {
		declared[m] = true
		if err := p.ipReq(ctx, "link", "set", "dev", m, "master", bond.Name); err != nil {
			return err
		}
	}
	// 释放已从声明里删掉的成员：只放开**确实挂在本 bond 上的**（转作其它角色的口不动）。
	for _, cur := range p.linkMembers(ctx, bond.Name) {
		if declared[cur] {
			continue
		}
		if err := p.detachMasterIf(ctx, cur, bond.Name); err != nil {
			return err
		}
	}
	return p.ipReq(ctx, "link", "set", "dev", bond.Name, "up")
}

// bondInfoData `ip -d -j link show` 里 bond 的属性（linkinfo.info_data）。
type bondInfoData struct {
	Mode           string `json:"mode"`
	XmitHashPolicy string `json:"xmit_hash_policy"`
	LacpRate       string `json:"lacp_rate"`
}

// bondAttrsDiffer 已在场的 bond 属性是否**可证**与声明不同（可证不同才删掉重建）。
//
// 读不到的字段按「无法证明不同」处理，不重建——重建会短暂打断已运行的 LAG（成员表清空），
// 不能凭猜测反复做。lacpRate 为空（无 lacp 声明）时不比较：静态聚合不关心它。
func bondAttrsDiffer(li *ipLinkInfo, mode, xmit, lacpRate string) bool {
	if li == nil || li.InfoKind != "bond" {
		return true // 同名设备不是 bond：必须重建
	}
	if li.InfoData == nil {
		return false
	}
	var d bondInfoData
	if json.Unmarshal(li.InfoData, &d) != nil {
		return false
	}
	if d.Mode != "" && d.Mode != mode {
		return true
	}
	if d.XmitHashPolicy != "" && d.XmitHashPolicy != xmit {
		return true
	}
	if lacpRate != "" && d.LacpRate != "" && d.LacpRate != lacpRate {
		return true
	}
	return false
}

// DeleteBond 删除内核 bonding 接口（成员口由内核自动释放回原状态）。
func (p *Provider) DeleteBond(ctx context.Context, name string) error {
	return p.ipBest(ctx, "link", "del", name)
}

// ---------- VNF vNIC ----------

// ApplyVnfInterface 建立 VNF vNIC 接入。
//
// 内核数据面下 VM 侧就是 virtio 网卡：libvirt 按 `<interface type='bridge'>` 自行创建
// 宿主 tap（vhost-net 加速）并挂到交换机对应的内核 bridge，产品不需要也不应该预先创建 tap
// （预建会与 libvirt 的创建冲突）。故本方法是**如实空操作**——接入由计算编排的域定义完成，
// 交换机侧（bridge 及其成员）由 ApplyBridgeDomain 收敛。
//
// sriov-vf 不经软件交换机，同样无动作；memif 在内核数据面无对应物（提交期已拒绝）。
func (p *Provider) ApplyVnfInterface(context.Context, orchestrator.VnfPort) error { return nil }

// DeleteVnfInterface vNIC 删除：tap 随域销毁由 libvirt 回收，无产品侧对象需要撤销。
func (p *Provider) DeleteVnfInterface(context.Context, string, string) error { return nil }

// ---------- 内核数据面无对应物的族（如实报不支持） ----------

func unsupported(feature string) error {
	return fmt.Errorf("%w：%s", ErrUnsupported, feature)
}

// ---------- 未实现族：只在**确有声明**时报不支持 ----------
//
// 关键口径（真机走查抓到）：提交编排把这些族当作 bridge-domain/VRF 的**伴随操作**调用——
// 每台 L2 交换机都会走一次 `ApplyDhcpRelay`/`ApplyDHCPServer`，DNS 代理则按全局+按域上游
// 汇总后调用。**未声明时必须是空操作**，否则任何一次普通提交都会被"未实现"挡住（现场：
// 只建了一台 L2 交换机，提交却报 `dhcp-relay[vs-lan] 不受支持`）。
// 提交期校验已拒绝在内核数据面下**声明**这些族，故下面这些分支是纵深防御。

// ApplyLLDP LLDP 邻居：内核侧需 lldpd 守护进程对接，属独立立项；未声明即空操作。
func (p *Provider) ApplyLLDP(_ context.Context, lldp *model.LldpConfig) error {
	if lldp == nil {
		return nil
	}
	return unsupported("LLDP（内核数据面尚未实现，请改用 VPP 数据面）")
}

// ApplyDhcpRelay 交换机 DHCP 中继：内核侧对应 dhcrelay，属独立立项；未声明即空操作。
func (p *Provider) ApplyDhcpRelay(_ context.Context, vs model.VirtualSwitch) error {
	if vs.DhcpRelayServer == "" {
		return nil
	}
	return unsupported("DHCP 中继（内核数据面尚未实现，请改用 VPP 数据面）")
}

// ApplyDHCPServer 域内 DHCP 服务器：内核侧对应 dnsmasq/kea，属独立立项；未声明即空操作。
func (p *Provider) ApplyDHCPServer(_ context.Context, vs model.VirtualSwitch) error {
	if vs.DhcpServerPoolStart == "" && vs.DhcpServerPoolEnd == "" {
		return nil
	}
	return unsupported("DHCP 服务器（内核数据面尚未实现，请改用 VPP 数据面）")
}

// ApplyDNSProxy 数据面 DNS 代理：内核侧对应 dnsmasq，属独立立项；无任何上游即空操作。
//
// 启用判据与 VPP 侧**逐字同源**（network.DNSProxyProvider.Sync）：先丢掉空的按域条目
// （每台交换机都会带一条、未配时是空列表），再判「全局或任一域非空」。
func (p *Provider) ApplyDNSProxy(_ context.Context, want orchestrator.DNSProxyUpstreams) error {
	enabled := len(want.Global) > 0
	if !enabled {
		for _, ups := range want.PerSwitch {
			if len(ups) > 0 {
				enabled = true
				break
			}
		}
	}
	if !enabled {
		return nil
	}
	return unsupported("数据面 DNS 代理（内核数据面尚未实现，请改用 VPP 数据面）")
}
