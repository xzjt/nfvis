package netkernel

import (
	"context"
	"fmt"
	"sync"

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
	cfgMu sync.RWMutex
	cfg   model.Config

	// 族管理器（惰性构造；内核是事实源，管理器本身不持有产品状态）。
	acl     *aclManager
	qos     *qosManager
	span    *spanManager
	storm   *stormManager
	portSec *portSecManager
}

// 编译期断言：本包必须完整实现 NetworkProvider（缺方法即编译失败，不靠运行时才发现）。
var _ orchestrator.NetworkProvider = (*Provider)(nil)

// SetConfig 记录配置快照（装配处与每次恢复收敛时调用）。
func (p *Provider) SetConfig(cfg model.Config) {
	p.cfgMu.Lock()
	p.cfg = cfg
	p.cfgMu.Unlock()
}

// config 当前配置快照。
func (p *Provider) config() model.Config {
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
	if iface.Enabled != nil {
		state := "up"
		if !*iface.Enabled {
			state = "down"
		}
		if err := p.ipReq(ctx, "link", "set", "dev", iface.Name, state); err != nil {
			return err
		}
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
// `802.3ad`，lacp_rate 由 interval 映射（fast=1、slow=0）。成员口按声明 enslave。
func (p *Provider) ApplyBond(ctx context.Context, bond model.Bond) error {
	if bond.Name == "" {
		return fmt.Errorf("bond 名不能为空")
	}
	mode := "balance-xor"
	opts := []string{"mode", mode}
	if bond.Lacp != nil {
		mode = "802.3ad"
		opts = []string{"mode", mode, "xmit_hash_policy", "layer3+4"}
		if bond.Lacp.Interval == "fast" {
			opts = append(opts, "lacp_rate", "fast")
		} else {
			opts = append(opts, "lacp_rate", "slow")
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
	for _, m := range bond.Members {
		if err := p.ipReq(ctx, "link", "set", "dev", m, "master", bond.Name); err != nil {
			return err
		}
	}
	return p.ipReq(ctx, "link", "set", "dev", bond.Name, "up")
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

// ApplyLLDP LLDP 邻居：内核侧需 lldpd 守护进程对接，属独立立项，本期如实报不支持。
func (p *Provider) ApplyLLDP(context.Context, *model.LldpConfig) error {
	return unsupported("LLDP（内核数据面尚未实现，请改用 VPP 数据面）")
}

// ApplyDhcpRelay 交换机 DHCP 中继：内核侧对应 dhcrelay，属独立立项，本期如实报不支持。
func (p *Provider) ApplyDhcpRelay(context.Context, model.VirtualSwitch) error {
	return unsupported("DHCP 中继（内核数据面尚未实现，请改用 VPP 数据面）")
}

// ApplyDHCPServer 域内 DHCP 服务器：内核侧对应 dnsmasq/kea，属独立立项，本期如实报不支持。
func (p *Provider) ApplyDHCPServer(context.Context, model.VirtualSwitch) error {
	return unsupported("DHCP 服务器（内核数据面尚未实现，请改用 VPP 数据面）")
}

// ApplyDNSProxy 数据面 DNS 代理：内核侧对应 dnsmasq，属独立立项，本期如实报不支持。
func (p *Provider) ApplyDNSProxy(context.Context, orchestrator.DNSProxyUpstreams) error {
	return unsupported("数据面 DNS 代理（内核数据面尚未实现，请改用 VPP 数据面）")
}
