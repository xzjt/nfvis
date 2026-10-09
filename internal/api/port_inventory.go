package api

// 运行态端口清单（决策 #83）。
//
// `<ifname>` 的动态候选此前一律取自 committed 配置的 `interfaces[].name`，
// 与真实端口无关：既漏掉未声明的 DPDK 口（已接管的口在内核中已无 netdev），
// 又会列出根本不存在的名字。此处把「端口清单」提升为一等运行态来源，按语义分两侧：
// 数据面（VPP）与内核——由 schema 侧的不同 kind 选择（`vpp-ifnames`/`kernel-ifnames`/`ifnames`）。
//
// 底座交互藏在接口后：实现见 internal/orchestrator/network（govpp + sysfs），单测用假实现。

import (
	"fmt"
	"sort"

	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// PortInventory 端口清单来源。
type PortInventory interface {
	// VPPIfnames VPP 中的接口名（= 已被 DPDK 接管的数据面端口，已排序）。
	VPPIfnames() ([]string, error)
	// KernelIfnames 内核网卡名（物理口；已接管的口在内核中已消失，故不在其中）。
	KernelIfnames() ([]string, error)
	// KernelIfFacts 内核侧物理口事实（决策 #302：未接管口读视图的驱动/MAC/速率/状态取 sysfs，
	// 不编造 VPP 侧事实）。
	KernelIfFacts() ([]network.KernelIfFacts, error)
	// KernelIfNotInDPReason 内核数据面下，某物理口「已声明却未进数据面」的原因（决策 #431）：
	// 仍绑 vfio-pci / 被 networkd 持有为 down / 取不到（ok=false）。VPP 数据面下恒取不到。
	KernelIfNotInDPReason(name string) (network.KernelIfReason, bool)
}

// vppIfnames / kernelIfnames 候选取值：未接入或查询失败一律返回 nil
// （契约 §5.3「失败则退化为仅关键字」），不退回「已配置接口名」——那正是本决策要修的错误来源。
func (s *Server) vppIfnames() []string {
	if s.ports == nil {
		return nil
	}
	names, err := s.ports.VPPIfnames()
	if err != nil {
		return nil
	}
	return names
}

// vppIfnamesOK 同 vppIfnames 但带成功标志：判「不在 VPP 清单」必须有**成功的查询**
// （决策 #154 同取向——宁可少下结论，也不冤枉一个真实存在的口）。
func (s *Server) vppIfnamesOK() ([]string, bool) {
	if s.ports == nil {
		return nil, false
	}
	names, err := s.ports.VPPIfnames()
	if err != nil {
		return nil, false
	}
	return names, true
}

// kernelIfFacts 内核侧物理口事实：未装配或读取失败返回 nil（调用方按「取不到就不给」处理）。
func (s *Server) kernelIfFacts() []network.KernelIfFacts {
	if s.ports == nil {
		return nil
	}
	fs, err := s.ports.KernelIfFacts()
	if err != nil {
		return nil
	}
	return fs
}

// kernelIfnames / kernelIfFacts 的 cliExecutor 侧同源取值（`show interfaces` 视图用，
// 与 REST 同一个 PortInventory 注入）。
func (x *cliExecutor) kernelIfnamesSafe() []string {
	if x.ports == nil {
		return nil
	}
	names, err := x.ports.KernelIfnames()
	if err != nil {
		return nil
	}
	return names
}

func (x *cliExecutor) kernelIfFactsSafe() []network.KernelIfFacts {
	if x.ports == nil {
		return nil
	}
	fs, err := x.ports.KernelIfFacts()
	if err != nil {
		return nil
	}
	return fs
}

// kernelIfaceFacts 单口内核事实（找不到 ok=false）。
func (x *cliExecutor) kernelIfaceFacts(name string) (network.KernelIfFacts, bool) {
	for _, f := range x.kernelIfFactsSafe() {
		if f.Name == name {
			return f, true
		}
	}
	return network.KernelIfFacts{}, false
}

// kernelIfNotInDPReason 内核数据面下，某物理口「已声明却未进数据面」的**点名文案**
// （决策 #431）：仍绑 vfio-pci（附 PCI 与照做路径）/ 被 networkd 持有为 down（附人工做法）；
// 取不到原因返回 ok=false（调用方沿用既有「已声明未生效」）。文案在此处渲染（事实源只给
// 结构化的 Kind/Driver/PCI），无内部引用。
func (x *cliExecutor) kernelIfNotInDPReason(name string) (string, bool) {
	if x.ports == nil {
		return "", false
	}
	r, ok := x.ports.KernelIfNotInDPReason(name)
	if !ok {
		return "", false
	}
	switch r.Kind {
	case network.KernelIfReasonVFIO:
		drv := r.Driver
		if drv == "" {
			drv = "vfio-pci"
		}
		where := ""
		if r.PCI != "" {
			where = "（PCI " + r.PCI + "）"
		}
		return fmt.Sprintf("仍绑定在 %s 驱动上%s、内核里没有它；先交还内核：request interfaces %s unbind-dpdk --yes",
			drv, where, name), true
	case network.KernelIfReasonNetworkdDown:
		return "被 systemd-networkd 持有为 down（netplan activation-mode 为 off）；" +
			"需人工把该口改为 manual 并 netplan apply（产品不代改 netplan）", true
	}
	return "", false
}

// kernelIfaceAdminUp 内核运行态里该口是否管理态 up（不在运行态清单 → 视为未生效）。
// 决策 #431 的点名只在「已声明却没进数据面」的口上触发，用它把「已在数据面 up」的口排除。
func kernelIfaceAdminUp(states map[string]InterfaceState, name string) bool {
	st, ok := states[name]
	return ok && st.AdminUp
}

// allIfnamesDeclared `set interfaces <n>` 的候选并集（决策 #302）：
// 内核未接管 ∪ 配置已声明 ∪ VPP 运行态——首装（VPP 未接管任何口、配置未声明）也能补全到
// 内核网卡名（round81 F1）。声明名入选的依据：声明是接管流程的第一步（先声明端口后绑定），
// 「已绑定 + VPP 未起」等生命周期各态里声明名可能暂时缺席另两份清单。配置读取失败时
// 退化为内核 ∪ VPP（声明名缺席，其余照给——各来源独立退化）。
func (s *Server) allIfnamesDeclared() []string {
	out := s.allIfnames() // VPP ∪ 内核
	if cfg, err := s.engine.Committed(); err == nil {
		for _, ifc := range cfg.Interfaces {
			out = append(out, ifc.Name)
		}
	}
	return dedupeStrings(out)
}

// untakenKernelIfnames 未接管的内核物理口（决策 #302 的枚举核心，纯函数）：
// kernel − vpp − declared。过滤口径（规格书附录 A #302）：
//   - 空名与 lo/local0 防御性按名剔除（物理口判断本身在枚举处——/sys/class/net/<n>
//     有 `device` 链接才算物理口，lo/veth/docker0 等虚拟接口无 device，进不到这里）；
//   - 已被 VPP 接管（出现在 VPP 运行态清单）的口剔除——正常情况下已接管口的内核 netdev
//     已消失，两侧同时出现按 VPP 优先，属双保险；
//   - 配置已声明的口剔除（声明口按既有口径展示，不重复出两行）。
//
// 输出去重并按名排序（顺序稳定，便于比对与测试）。
func untakenKernelIfnames(kernel, vpp, declared []string) []string {
	taken := make(map[string]struct{}, len(vpp)+len(declared))
	for _, n := range vpp {
		taken[n] = struct{}{}
	}
	for _, n := range declared {
		taken[n] = struct{}{}
	}
	out := make([]string, 0, len(kernel))
	seen := make(map[string]struct{}, len(kernel))
	for _, n := range kernel {
		if n == "" || n == "lo" || n == "local0" {
			continue
		}
		if _, ok := taken[n]; ok {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (s *Server) kernelIfnames() []string {
	if s.ports == nil {
		return nil
	}
	names, err := s.ports.KernelIfnames()
	if err != nil {
		return nil
	}
	return names
}

// allIfnames VPP ∪ 内核（动作混合节点用，如 `request interfaces <n> enable|bind-dpdk`）。
func (s *Server) allIfnames() []string {
	vpp := s.vppIfnames()
	kern := s.kernelIfnames()
	out := make([]string, 0, len(vpp)+len(kern))
	out = append(out, vpp...)
	out = append(out, kern...)
	return dedupeStrings(out)
}

// dedupeStrings 去重并排序（同一名字可能两侧都报；顺序稳定便于比对）。
func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
