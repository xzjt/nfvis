package netkernel

// 内核数据面下的容器 vNIC 接入（v3 决策 #441；FR-NET-022 的内核侧落地）。
//
// VPP 侧形态（决策 #79 起）= memif 共享内存端点（产品建 VPP 侧 endpoint 并把 socket 挂进
// 容器，容器内跑 memif 客户端）；内核侧**没有 memif**、也不引入第三方守护（零外部依赖），
// 故自研为 **veth 对**：
//
//	宿主端（`nfvisct` + 8 位十六进制，见 orchestrator.ContainerVethNames 单一真源）
//	    由**网络编排**创建（本文件：建对 + 置 up）——**先建接口、后入域**，与 VPP 侧
//	    「先建 memif 接口、bridge-domain 段按名把端口挂进 BD」同一架构：enslave 到该 vNIC
//	    声明的交换机内核 bridge 由 **bridge-domain 段**完成（l2.go 的 memberLinkName 把容器
//	    端口映射到宿主端名，成员处理负责 master/up/VLAN）；15s 巡检按声明确保桥归属（入对桥）。
//	容器端（`nfviscp` + 同一 8 位十六进制）
//	    由**容器编排**在容器 start 成功后移入其网络命名空间（容器内名字＝声明的 vNIC 名、
//	    置 up、可选 MAC）。本文件提供实现（AttachContainerVeth），调用时机由容器编排掌握
//	    ——只有它知道 PID 与容器生命周期。
//
// VLAN：与 VPP 侧**同口径**——vNIC 级的 `vlan` 字段不在接入路径单独处理（VPP 的 memif 路径
// 同样只建接口 + 设 MAC + up），VLAN 由交换机/端口声明经 bridge-domain 段既有的成员处理落地
// （l2.go 的 applyPortVlans：access/native/trunk）。
//
// 为什么分成两半：容器端只有容器编排知道 PID/生命周期；宿主端与 bridge 是数据面职责——
// 与「VM 的宿主 tap 由 libvirt 建、产品只管 bridge」同构。
//
// 生命周期（关键：容器 netns 会在 restart 时重建）：
//   - create（提交编排的 vnf-if 段）：只建宿主端（veth 对 + 置 up；入桥由随后的 bridge-domain
//     段完成——这正是「先建接口、后入域」的依赖序，同一提交里新建交换机也能收敛）；
//   - start/restart 成功后：AttachContainerVeth —— **restart 会重建 netns，必须重新 attach**，
//     否则容器网络静默失效；
//   - stop：**宿主端随声明留驻**（容器端随 netns 消失，下次 start 重新 attach）——不删，
//     免去每轮启停重建内核对象；delete/声明消失：Delete 清宿主端（veth 成对同生共死，不留孤儿）；
//   - **nfvisd 重启**：宿主端是内核对象，**按名复用、绝不重建**（重建会打断运行中容器的
//     网络）——恢复重放（recovery.go）只做「按名核对 + 缺失补建」，容器端在容器自己的
//     netns 里不受进程重启影响（但容器 restart 后要重新 attach）；
//   - 15s 巡检：ReconcileContainerVeth 对账（声明里缺宿主端的补建、桥归属纠正、无声明对应的
//     产品宿主端清掉）。
//
// 归属判据：名字是哈希派生的、**反查不出**是哪个容器的哪个 vNIC，故本文件维护一份**进程内
// 簿记**（owner/vNIC → 名字，每次 Ensure 按声明登记）：读视图过滤（BridgeDomains）与巡检
// 优先用它，簿记尚未建立（nfvisd 刚起/从未收敛）时按**严格前缀**兜底（前缀 + 8 位小写
// 十六进制，与内置 DHCP tap 的 isProductDHCPTapName 同族的字符集校验，不误伤用户接口）。
//
// 与 apply 路径的硬约束：本文件（及其调用链）**绝不回读配置发动机**——事实来自调用点给的
// 参数（VnfPort / VnfInterface / cfg 快照）。提交期发动机锁由本次提交自己持有，apply 路径上
// `p.config()` 重入即**自死锁**（真机 SIGQUIT 全栈实证过同型事故，见 provider.go 的
// configSnapshot 注释）。只有「容器编排只给了属主名」的回收路径（DeleteContainerVeths）用
// 进程内快照 p.configSnapshot()（装配/恢复时刷新，允许的口径）补全名单。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

const (
	// ctVethHostPrefix / ctVethPeerPrefix 与 orchestrator.ContainerVethNames 的命名前缀
	// **同一真源**（单源在 orchestrator；此处只作本包内的短名，便于读/过滤/核对身份）。
	// 名字总长 = 前缀 + 8 位十六进制 = 15 = IFNAMSIZ-1（与内置 DHCP tap 同长）。
	ctVethHostPrefix = orchestrator.ContainerVethHostPrefix
	ctVethPeerPrefix = orchestrator.ContainerVethPeerPrefix
)

// isProductContainerVethName 是否是产品自持的容器 veth 名（宿主端或容器端：前缀 + 8 位小写
// 十六进制，共 15 字符）。严格到字符集（与 isProductDHCPTapName 同族）：删/复用/读视图过滤
// 都要按它核对身份，放宽会把用户自己的接口认成产品内置设备。
func isProductContainerVethName(name string) bool {
	return vethNameWithPrefix(name, ctVethHostPrefix) || vethNameWithPrefix(name, ctVethPeerPrefix)
}

// isProductContainerVethHostEnd 是否产品自持的**宿主端**名（前缀 nfvisct）：bridge 成员过滤与
// 残留对账只认宿主端——容器端不进 bridge（它在容器 netns 里由容器自己用）。
func isProductContainerVethHostEnd(name string) bool {
	return vethNameWithPrefix(name, ctVethHostPrefix)
}

// vethNameWithPrefix 前缀匹配 + 余下必须是 8 位小写十六进制，且总长 = 前缀 + 8（15 字符；
// 严格字符集，不猜大小写）。
func vethNameWithPrefix(name, prefix string) bool {
	if len(name) != len(prefix)+8 {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if name[i] != prefix[i] {
			return false
		}
	}
	for _, c := range name[len(prefix):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isProductContainerVethPort 读视图里的端口名是否是产品自持的容器 vNIC 宿主端：**优先编排
// 簿记**（该桥口是产品为哪个容器的哪个 vNIC 建的，只有产品知道），簿记尚未建立（管理器未装配）
// 时按**严格前缀**兜底（见 api_surface.go 的 BridgeDomains）。
func (p *Provider) isProductContainerVethPort(name string) bool {
	if p.containerVethHostEnds()[name] {
		return true
	}
	return isProductContainerVethHostEnd(name)
}

// ---------- 规格（由声明派生） ----------

// ctVethSpec 一个容器 vNIC 宿主端的规格（声明派生；两端名字全部来自
// orchestrator.ContainerVethNames 单一真源，不在本包另写一份派生）。
type ctVethSpec struct {
	owner string // 容器名
	iface string // vNIC 名（也是容器内的接口名）
	host  string // 宿主端名（产品创建；入桥由 bridge-domain 段/巡检负责）
	peer  string // 容器端名（容器编排在 start 后移入容器 netns）
	vs    string // 接入的交换机名（空 = 未接入任何交换机：只建对、不入桥）
}

// ctVethSpecOf 由（属主、vNIC、接入交换机）派生规格（名字走单一真源）。
func ctVethSpecOf(owner, iface, vs string) ctVethSpec {
	host, peer := orchestrator.ContainerVethNames(owner, iface)
	return ctVethSpec{owner: owner, iface: iface, host: host, peer: peer, vs: vs}
}

// containerVethSpecsOf 从配置派生全部容器 vNIC 宿主端规格（按容器名、vNIC 名升序——与
// VnfPortsOf 同一排序口径，保证恢复重放与巡检的命令序列确定、可复现）。
func containerVethSpecsOf(cfg model.Config) []ctVethSpec {
	var out []ctVethSpec
	for _, ct := range cfg.ContainerFunctions {
		for _, nic := range ct.Interfaces {
			if nic.Type != "memif" {
				continue // 容器 vNIC 恒为 memif（校验保证；与 VnfPortsOf 同口径跳过）
			}
			out = append(out, ctVethSpecOf(ct.Name, nic.Name, nic.VirtualSwitch))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].owner != out[j].owner {
			return out[i].owner < out[j].owner
		}
		return out[i].iface < out[j].iface
	})
	return out
}

// ---------- 底座（真实现见 container_veth_{linux,other}.go） ----------

// ctVethIO 一对 veth 的内核操作面（单测注入内存实现，见 container_veth_test.go）。
type ctVethIO interface {
	// EnsurePair 确保 veth 对存在：**按名复用、绝不重建**（重建会打断运行中容器的网络）。
	EnsurePair(host, peer string) error
	// SetMaster 把宿主端 enslave 到交换机内核 bridge。
	SetMaster(name, bridge string) error
	// SetUp 置宿主端管理员 up。
	SetUp(name string) error
	// Attach 把容器端接进 pid 的网络命名空间：改名 niceName、可选设 MAC、置 up。
	Attach(peer string, pid int, niceName, mac string) error
	// Delete 删除宿主端（veth 成对：删一端即整对消失）；设备不存在按已达成（幂等）。
	Delete(name string) error
	// Exists 设备是否存在（写后回读确认与残留对账用）。
	Exists(name string) (bool, error)
}

// ctVethLayer 打开 veth 操作面的底座（真实现见 container_veth_{linux,other}.go；与 DHCP 的
// dhcpTapLayer / LLDP 的 lldpLayer 同一注入口径）。
type ctVethLayer interface {
	Open() (ctVethIO, error)
}

// ---------- 管理器（进程内簿记 + 下发编排） ----------

type ctVethKey struct{ owner, iface string }

type ctVethNames struct{ host, peer string }

// ctVethManager 各容器 vNIC 宿主端的编排者（决策 #441）。本身**不持有**内核状态（内核是事实
// 源），只维护「owner/vNIC → 名字」的簿记，供读视图过滤与回收路径使用。
type ctVethManager struct {
	p     *Provider
	layer ctVethLayer

	mu    sync.Mutex
	hosts map[ctVethKey]ctVethNames
}

func newCtVethManager(p *Provider, layer ctVethLayer) *ctVethManager {
	return &ctVethManager{p: p, layer: layer, hosts: map[ctVethKey]ctVethNames{}}
}

// open 打开底座（真实现无连接/fd 可持有，故每次操作取一次；打开失败如实报错，不静默）。
func (m *ctVethManager) open() (ctVethIO, error) {
	io, err := m.layer.Open()
	if err != nil {
		return nil, fmt.Errorf("容器 vNIC 宿主端的 veth 底座不可用: %w", err)
	}
	if io == nil {
		return nil, fmt.Errorf("容器 vNIC 宿主端的 veth 底座返回空实现（装配缺陷，请上报）")
	}
	return io, nil
}

// remember 登记一个宿主端的归属（簿记；Ensure 时按声明登记）。
func (m *ctVethManager) remember(spec ctVethSpec) {
	m.mu.Lock()
	m.hosts[ctVethKey{owner: spec.owner, iface: spec.iface}] = ctVethNames{host: spec.host, peer: spec.peer}
	m.mu.Unlock()
}

// forget 忘掉某个宿主端名对应的条目（删除成功 / 巡检清残渣后）。
func (m *ctVethManager) forget(host string) {
	m.mu.Lock()
	for k, n := range m.hosts {
		if n.host == host {
			delete(m.hosts, k)
		}
	}
	m.mu.Unlock()
}

// retain 只保留仍在声明里的条目（巡检时刷新：已不声明的条目不留，防读视图过滤用到陈旧名字）。
func (m *ctVethManager) retain(keys map[ctVethKey]bool) {
	m.mu.Lock()
	for k := range m.hosts {
		if !keys[k] {
			delete(m.hosts, k)
		}
	}
	m.mu.Unlock()
}

// bookkeptHosts 簿记里的宿主端名集合（副本；读视图过滤用）。
func (m *ctVethManager) bookkeptHosts() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]bool, len(m.hosts))
	for _, n := range m.hosts {
		out[n.host] = true
	}
	return out
}

// hostsOf 某属主在簿记里的全部宿主端名（升序，确定性输出）。
func (m *ctVethManager) hostsOf(owner string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k, n := range m.hosts {
		if k.owner == owner {
			out = append(out, n.host)
		}
	}
	sort.Strings(out)
	return out
}

// has 该（属主, vNIC）是否在簿记里（归属判据之一，见 DeleteVnfInterface）。
func (m *ctVethManager) has(key ctVethKey) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.hosts[key]
	return ok
}

// sync 收敛一个容器 vNIC 的**宿主端接口**（幂等）：确保 veth 对存在、回读确认、置宿主端 up。
//
// **不含入桥与 VLAN**：那是 bridge-domain 段的成员处理（先建接口、后入域，见文件头与
// l2.go 的 memberLinkName）——本方法只保证「接口在、是 up」，提交编排里它先于交换机段执行，
// 同一次提交里新建的交换机此刻还没有内核 bridge，在这里 master 会以底座原始错误失败。
// **不 attach**：容器端要等容器 start 后由容器编排移进它的 netns（本方法不知道、也不该猜 PID）。
func (m *ctVethManager) sync(ctx context.Context, spec ctVethSpec) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	io, err := m.open()
	if err != nil {
		return err
	}
	if err := io.EnsurePair(spec.host, spec.peer); err != nil {
		return fmt.Errorf("创建 veth 对（%s ↔ %s）失败: %w", spec.host, spec.peer, err)
	}
	// 簿记在「对已存在」之后立刻登记：此后任何一步失败，回收路径都还能按它对上号。
	m.remember(spec)
	if ok, err := io.Exists(spec.host); err != nil {
		return fmt.Errorf("回读 veth 宿主端 %s 失败: %w", spec.host, err)
	} else if !ok {
		return fmt.Errorf("veth 宿主端 %s 创建后回读不到（内核里没有它）", spec.host)
	}
	if err := io.SetUp(spec.host); err != nil {
		return fmt.Errorf("置 veth 宿主端 %s up 失败: %w", spec.host, err)
	}
	return nil
}

// ensureBridge 按声明把宿主端 enslave 到该 vNIC 声明的交换机内核 bridge（幂等）。
//
// **归口说明**：日常收敛由 **bridge-domain 段**负责（先建接口、后入域：ApplyContainerVeth
// 只建对，桥段把宿主端当普通成员口处理——master/up/VLAN，见 l2.go 的 memberLinkName）。
// 本方法只在 **15s 巡检**里用：把「带外被摘出 bridge」「桥被带外重建」「提交期桥尚未存在」
// 的归属纠正回来；**不是提交路径的第二条下发路径**。
func (m *ctVethManager) ensureBridge(ctx context.Context, spec ctVethSpec) error {
	if spec.vs == "" {
		return nil // 未接入交换机：无桥可入（与 VPP 侧「建接口、不进 BD」同义）
	}
	io, err := m.open()
	if err != nil {
		return err
	}
	br := LinkName(spec.vs)
	if err := io.SetMaster(spec.host, br); err != nil {
		return fmt.Errorf("把 %s 加入交换机 %s 的内核 bridge %s 失败（该 bridge 是否已收敛？）: %w",
			spec.host, spec.vs, br, err)
	}
	return nil
}

// dropResidue 清掉内核里「产品自持、但没有任何声明对应」的宿主端（容器/vNIC 已删除、数据面
// 切换残留）。识别＝名字形如 nfvisct+8 位十六进制且设备类型可读为 veth（读不到类型按「无法
// 证明不是产品设备」保守处理，与内置 DHCP tap 的 TapDump 同口径）；名字反查不出属主，故
// **只按声明集合**判残留——声明里有的绝不删（哪怕读不出是谁的）。
func (m *ctVethManager) dropResidue(ctx context.Context, declared map[string]bool) error {
	rows, err := kernelLinkRows(ctx, m.p.run)
	if err != nil {
		return fmt.Errorf("容器 vNIC 宿主端残留对账读取内核接口清单失败: %w", err)
	}
	var errs []error
	var io ctVethIO
	for _, r := range rows {
		if r.Ifname == "" || declared[r.Ifname] || !isProductContainerVethHostEnd(r.Ifname) {
			continue
		}
		if k := r.kind(); k != "" && k != "veth" {
			continue // 不冒认用户的同名设备（类型可读且不是 veth）
		}
		if io == nil {
			if io, err = m.open(); err != nil {
				errs = append(errs, err)
				break
			}
		}
		if err := io.Delete(r.Ifname); err != nil {
			errs = append(errs, fmt.Errorf("清除容器 vNIC 宿主端残留 %s: %w", r.Ifname, err))
			continue
		}
		m.forget(r.Ifname)
	}
	return errors.Join(errs...)
}

// reconcile 15s 巡检对账（决策 #441）：
//   - 声明里每个容器 vNIC：宿主端按名确保在位（缺则建、置 up），并**入对桥**（master 由本
//     巡检兜底：提交期由 bridge-domain 段负责，带外被摘出/桥被重建时靠这里纠正）；
//     **不做 attach**——这里没有 PID，也不该猜容器在不在跑（attach 由容器编排在 start 后做）；
//   - 内核里存在、但没有任何声明对应的**产品宿主端**：删除（容器/vNIC 已删除、切换残留）；
//   - 簿记随声明刷新（已不声明的条目清掉）。
//
// 逐条如实返回错误（巡检日志 + 未收敛告警），一条失败不遮其它条。
// 不重建已存在的宿主端（绝不）：重建会打断运行中容器的网络。
func (m *ctVethManager) reconcile(ctx context.Context, specs []ctVethSpec) error {
	var errs []error
	declared := make(map[string]bool, len(specs))
	keys := make(map[ctVethKey]bool, len(specs))
	for _, spec := range specs {
		declared[spec.host] = true
		keys[ctVethKey{owner: spec.owner, iface: spec.iface}] = true
		if err := m.sync(ctx, spec); err != nil {
			errs = append(errs, fmt.Errorf("container-functions/%s/interfaces/%s: %w", spec.owner, spec.iface, err))
			continue // 接口都没收敛，入桥必然失败——只报本条，不叠第二个错
		}
		if err := m.ensureBridge(ctx, spec); err != nil {
			errs = append(errs, fmt.Errorf("container-functions/%s/interfaces/%s: %w", spec.owner, spec.iface, err))
		}
	}
	m.retain(keys)
	if err := m.dropResidue(ctx, declared); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ---------- Provider 接线 ----------

// ApplyContainerVeth 建立/收敛一个容器 vNIC 的**宿主端接口**（内核数据面）。
//
// 幂等：同名宿主端存在即复用（绝不重建）。**只建对 + 置 up**：入桥与 VLAN 由 bridge-domain
// 段的成员处理完成（先建接口、后入域，见文件头与 l2.go 的 memberLinkName）——提交编排里本段
// 先于交换机段执行，同一次提交里新建的交换机此刻还没有内核 bridge，在这里 master 会以底座
// 原始错误失败。未接入交换机（port.VirtualSwitch 为空）时只建对、不入桥（与 VPP 侧
// 「建 memif 接口、不进 bridge-domain」同义）；vNIC 级 `vlan` 同样不在这里处理（与 VPP 侧
// memif 路径同口径，VLAN 由交换机/端口声明落地）。
func (p *Provider) ApplyContainerVeth(ctx context.Context, port orchestrator.VnfPort) error {
	if port.Type != "memif" {
		return fmt.Errorf("容器 vNIC %s/%s 类型 %q 非 memif（内核侧容器接入即 veth 对）", port.VM, port.Interface, port.Type)
	}
	if port.VM == "" || port.Interface == "" {
		return fmt.Errorf("容器 vNIC 缺少属主名或 vNIC 名（装配/声明缺陷，请上报）")
	}
	return p.ctVethMgr().sync(ctx, ctVethSpecOf(port.VM, port.Interface, port.VirtualSwitch))
}

// DeleteContainerVeth 删除一个容器 vNIC 的宿主端（veth 成对：宿主端删掉即整对消失，容器端
// 随容器 netns 一起回收，不留孤儿）。**幂等**：设备不存在按已达成。
func (p *Provider) DeleteContainerVeth(ctx context.Context, owner, iface string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	spec := ctVethSpecOf(owner, iface, "")
	mgr := p.ctVethMgr()
	io, err := mgr.open()
	if err != nil {
		return err
	}
	if err := io.Delete(spec.host); err != nil {
		return fmt.Errorf("删除容器 %s 的 vNIC %s 宿主端 %s 失败: %w", owner, iface, spec.host, err)
	}
	mgr.forget(spec.host)
	return nil
}

// AttachContainerVeth 把容器 vNIC 的**容器端**接进运行中容器的网络命名空间（容器 start /
// restart 成功后由容器编排调用；restart 会重建 netns，必须重新 attach，否则容器网络静默失效）。
//
// 逐个 vNIC 幂等 ensure：先按声明确保宿主端接口在位（veth 对 + up；已在位即复用，**绝不
// 重建**），再把容器端移入 `pid` 的 netns（容器内名字＝声明的 vNIC 名、置 up、声明了 mac 时
// 设在该端）。一个 vNIC 失败**不吞**：逐条如实收集（带容器/vNIC 名）后 errors.Join，其余
// vNIC 照常收敛。
func (p *Provider) AttachContainerVeth(ctx context.Context, owner string, ifaces []model.VnfInterface, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("容器 %s 的网络命名空间目标 pid 非法（%d）：容器未在运行？"+
			"attach 只能在容器 start 成功后做", owner, pid)
	}
	mgr := p.ctVethMgr()
	io, err := mgr.open()
	if err != nil {
		return err
	}
	var errs []error
	for _, nic := range ifaces {
		if nic.Type != "memif" {
			continue // 容器 vNIC 恒为 memif（校验保证；与 VnfPortsOf 同口径跳过）
		}
		spec := ctVethSpecOf(owner, nic.Name, nic.VirtualSwitch)
		if err := mgr.sync(ctx, spec); err != nil {
			errs = append(errs, fmt.Errorf("容器 %s 的 vNIC %s: %w", owner, nic.Name, err))
			continue
		}
		if err := io.Attach(spec.peer, pid, nic.Name, nic.MAC); err != nil {
			errs = append(errs, fmt.Errorf("把容器 %s 的 vNIC %s（容器端 %s）接入其网络命名空间（pid %d）失败: %w",
				owner, nic.Name, spec.peer, pid, err))
		}
	}
	return errors.Join(errs...)
}

// DeleteContainerVeths 删除某容器的**全部**容器 vNIC 宿主端（容器删除 / 声明消失路径；
// **stop 不删**——宿主端随声明留驻，容器端随 netns 消失、下次 start 重新 attach）。
//
// 名字由哈希派生、反查不出属主，故名单取两处并集：
//   - 进程内簿记（本进程 Ensure 过的，含声明已从 committed 里删掉的对象）；
//   - 进程内配置快照（p.configSnapshot()：nfvisd 重启后簿记为空，按声明补全）。
//
// **不按前缀扫内核全删**：名字反查不出属主，全删会误伤别的容器的 veth。逐条删除并如实报错。
func (p *Provider) DeleteContainerVeths(ctx context.Context, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	names := p.containerVethNamesOfOwner(owner)
	if len(names) == 0 {
		return nil
	}
	mgr := p.ctVethMgr()
	io, err := mgr.open()
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range names {
		if err := io.Delete(name); err != nil {
			errs = append(errs, fmt.Errorf("删除容器 %s 的宿主端 %s 失败: %w", owner, name, err))
			continue
		}
		mgr.forget(name)
	}
	return errors.Join(errs...)
}

// containerVethNamesOfOwner 某属主的宿主端名集合：进程内簿记 ∪ 进程内配置快照里该容器的
// 声明（升序，确定性）。只读快照，不读配置发动机（apply 路径的硬约束，见文件头）。
func (p *Provider) containerVethNamesOfOwner(owner string) []string {
	set := map[string]bool{}
	p.ctVethMu.Lock()
	mgr := p.ctVeth
	p.ctVethMu.Unlock()
	if mgr != nil {
		for _, name := range mgr.hostsOf(owner) {
			set[name] = true
		}
	}
	for _, ct := range p.configSnapshot().ContainerFunctions {
		if ct.Name != owner {
			continue
		}
		for _, nic := range ct.Interfaces {
			if nic.Type != "memif" {
				continue
			}
			host, _ := orchestrator.ContainerVethNames(owner, nic.Name)
			set[host] = true
		}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ReconcileContainerVeth 内核数据面容器 vNIC 宿主端的 15s 巡检对账（决策 #441）：声明里的宿主端
// 按名确保在位（缺则建、**入对桥**、置 up）、无声明对应的产品宿主端清掉（残渣）。
//
// 入桥在巡检里兜底的意义：提交期由 bridge-domain 段的成员处理负责（先建接口、后入域），
// 带外把宿主端摘出 bridge、桥被带外重建、同一提交里桥段失败等情形由这里纠正回声明态。
//
// 「未声明且从未装配」⇒ 空操作：功能没被用过的机器不必因巡检常驻构造管理器（无对象可对账）；
// 一旦装配过（提交/恢复重放碰过它），空声明也走一轮——把带外残留的宿主端收干净。
//
// 失败**如实进未收敛项**：返回错误（15s 巡检日志）之外，按 EnsureConsistent 的同一
// scope/code/source 建/消告警——`show alarms` 事后可查；下一轮成功即自动消解。
// 本方法**不做 attach**（没有 PID，也不该猜容器在不在跑）；宿主端**随声明存在**（stop 不删，
// 容器端随 netns 消失、下次 start 重新 attach），故这里只按声明对账、不依赖容器运行态。
func (p *Provider) ReconcileContainerVeth(ctx context.Context, cfg model.Config) []error {
	specs := containerVethSpecsOf(cfg)
	p.ctVethMu.Lock()
	mgr := p.ctVeth
	p.ctVethMu.Unlock()
	if len(specs) == 0 && mgr == nil {
		return nil // 未声明且从未装配：无对象可对账，不构造管理器、不下发任何内核命令
	}
	const src = "container-veth"
	err := p.ctVethMgr().reconcile(ctx, specs)
	if err == nil {
		if p.alarms != nil {
			p.alarms.Resolve(alarmScopeRecovery, network.AlarmUnconverged, src)
		}
		return nil
	}
	if p.alarms != nil {
		p.alarms.Raise(alarmScopeRecovery, network.SeverityWarning, network.AlarmUnconverged, err.Error(), src)
	}
	return []error{err}
}

// Attach / Delete 容器编排的接入钩子（**契约名**）：容器 start/restart 成功后 attach 容器端、
// stop/delete 时回收宿主端。与 AttachContainerVeth / DeleteContainerVeths 同源，只是把
// 「容器编排只关心属主与 pid」这一层显式化（内核数据面的命名/簿记细节不外泄）。
// 装配处把本 Provider 的这一对方法接到容器编排的钩子上即可（无需按数据面分叉）。
func (p *Provider) Attach(ctx context.Context, owner string, ifaces []model.VnfInterface, pid int) error {
	return p.AttachContainerVeth(ctx, owner, ifaces, pid)
}

func (p *Provider) Delete(ctx context.Context, owner string) error {
	return p.DeleteContainerVeths(ctx, owner)
}
