package api

// 运行态端口清单（决策 #83）。
//
// `<ifname>` 的动态候选此前一律取自 committed 配置的 `interfaces[].name`，
// 与真实端口无关：既漏掉未声明的 DPDK 口（已接管的口在内核中已无 netdev），
// 又会列出根本不存在的名字。此处把「端口清单」提升为一等运行态来源，按语义分两侧：
// 数据面（VPP）与内核——由 schema 侧的不同 kind 选择（`vpp-ifnames`/`kernel-ifnames`/`ifnames`）。
//
// 底座交互藏在接口后：实现见 internal/orchestrator/network（govpp + sysfs），单测用假实现。

import "sort"

// PortInventory 端口清单来源。
type PortInventory interface {
	// VPPIfnames VPP 中的接口名（= 已被 DPDK 接管的数据面端口，已排序）。
	VPPIfnames() ([]string, error)
	// KernelIfnames 内核网卡名（物理口；已接管的口在内核中已消失，故不在其中）。
	KernelIfnames() ([]string, error)
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
