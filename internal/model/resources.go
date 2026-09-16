package model

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// 资源池账本（FR-CMP-002/003/004）。
//
// 声明式语义：账本从配置推导——committed 配置即账本，VNF 删除后其占用随
// 配置移除自动归还（FR-CMP-003），无需独立记账状态。事务引擎在 commit 阶段
// 对 candidate 跑 Allocate（FR-CFG-011⑨⑪ 配额校验），M2 的
// show resource-pools / GET /resource-pools 直接读账本展示 总量/已分配/空闲。
//
// 分配顺序确定（按 VM 名升序、核号升序），保证 diff 与审计可复现。

// PoolLedger 资源池账本。
type PoolLedger struct {
	Hugepages map[string]*HugepagePoolUsage // 页大小 -> 池用量
	CPU       CPULedger
}

// HugepagePoolUsage 单个页大小池的用量。
type HugepagePoolUsage struct {
	PageSize  string
	Total     int
	Allocated int
	Free      int
}

// CPULedger 隔离核账本。VppReserved 先扣（FR-SYS-010：账本先扣 VPP 保留核，
// 剩余才是 VNF 可分配额度，show resource-pools 单独展示 vpp-reserved）。
type CPULedger struct {
	Isolated    []int            // 隔离核全集（升序）
	VppReserved []int            // VPP main-core + corelist-workers（升序）
	VMCores     map[string][]int // VM 名 -> 分配核（升序）
	Free        []int            // 未分配核（升序）
}

// NewPoolLedger 按配置构建账本并扣减 VPP 保留核。
func NewPoolLedger(cfg Config) *PoolLedger {
	l := &PoolLedger{
		Hugepages: map[string]*HugepagePoolUsage{},
		CPU:       CPULedger{VMCores: map[string][]int{}},
	}
	if cfg.ResourcePools != nil {
		for _, hp := range cfg.ResourcePools.Hugepages {
			l.Hugepages[hp.PageSize] = &HugepagePoolUsage{PageSize: hp.PageSize, Total: hp.Count, Free: hp.Count}
		}
		if cfg.ResourcePools.CPU != nil {
			l.CPU.Isolated = sortedCopy(cfg.ResourcePools.CPU.IsolatedCores)
		}
	}
	reserved := map[int]bool{}
	if cfg.Vpp != nil && cfg.Vpp.CPU != nil {
		if cfg.Vpp.CPU.MainCore != 0 {
			reserved[cfg.Vpp.CPU.MainCore] = true
		}
		if cores, err := parseCoreList(cfg.Vpp.CPU.CorelistWorkers); err == nil {
			for _, c := range cores {
				reserved[c] = true
			}
		}
	}
	for c := range reserved {
		l.CPU.VppReserved = append(l.CPU.VppReserved, c)
	}
	sort.Ints(l.CPU.VppReserved)
	l.CPU.Free = minusInts(l.CPU.Isolated, l.CPU.VppReserved)
	return l
}

// CheckResources 资源配额校验（FR-CFG-011⑥⑨⑪、FR-CMP-002/004）：
// 对 candidate 做全量分配演练，配额不足时逐条给出缺口明细。
func CheckResources(cfg Config) []ValidateError {
	return NewPoolLedger(cfg).Allocate(cfg)
}

// AllocatedResources 单台 VM 的资源分配结果（账本产出，供编排层组装 domain）。
type AllocatedResources struct {
	Cores        []int  // 绑核 cpuset（升序）；空 = 不绑核
	HugepageSize string // 已解析的大页页大小（backing=hugepage 时非空）
}

// AllocationFor 从配置确定性重算某台 VM 的资源分配（FR-CMP-002）：
// 先扣 VPP 保留核后取绑核；大页页大小缺省取资源池首个页池。
// 配置非法（配额不足）时返回空分配，由 commit 阶段校验拦截。
func AllocationFor(cfg Config, vm VMFunction) AllocatedResources {
	l := NewPoolLedger(cfg)
	if errs := l.Allocate(cfg); len(errs) > 0 {
		return AllocatedResources{}
	}
	out := AllocatedResources{Cores: l.CPU.VMCores[vm.Name]}
	if !usesNormalMemory(vm) {
		out.HugepageSize = vm.Memory.HugepageSize
		if out.HugepageSize == "" && cfg.ResourcePools != nil && len(cfg.ResourcePools.Hugepages) > 0 {
			out.HugepageSize = cfg.ResourcePools.Hugepages[0].PageSize
		}
	}
	return out
}

// Allocate 分配演练：按 VM 名升序为每台 VM 分配大页与绑核（pin 默认 true）。
// 缺口以 ValidateError 逐条返回（含需要/空闲/NUMA 明细，FR-CMP-002）；
// 通过时账本停留在分配完成状态，供 show/API 展示。
func (l *PoolLedger) Allocate(cfg Config) []ValidateError {
	var errs []ValidateError
	used := map[int]bool{}
	for _, c := range l.CPU.VppReserved {
		used[c] = true
	}

	vms := slices.Clone(cfg.VirtualMachineFunctions)
	sort.Slice(vms, func(i, j int) bool { return vms[i].Name < vms[j].Name })

	for _, vm := range vms {
		vmPath := fmt.Sprintf("virtual-machine-functions[%s]", vm.Name)

		// —— 大页内存（FR-CFG-011⑪：指定页池必须有足够余量）——
		// FR-CMP-019/决策 #39：backing=normal 用普通内存，不占用大页池。
		if vm.Memory.SizeMB > 0 && !usesNormalMemory(vm) {
			pageSize := vm.Memory.HugepageSize
			if pageSize == "" && cfg.ResourcePools != nil && len(cfg.ResourcePools.Hugepages) > 0 {
				pageSize = cfg.ResourcePools.Hugepages[0].PageSize // 缺省取资源池主池
			}
			pool := l.Hugepages[pageSize]
			if pool == nil {
				errs = append(errs, ValidateError{
					Path: vmPath + ".memory.size-mb",
					Message: fmt.Sprintf("无 %s 大页资源池，无法分配 %dMB",
						pageSizeLabel(pageSize), vm.Memory.SizeMB),
				})
			} else {
				pages := pagesNeeded(vm.Memory.SizeMB, pool.PageSize)
				if pool.Free < pages {
					errs = append(errs, ValidateError{
						Path: vmPath + ".memory.size-mb",
						Message: fmt.Sprintf("资源池大页 %s 不足：需要 %d，空闲 %d",
							pool.PageSize, pages, pool.Free),
					})
				} else {
					pool.Allocated += pages
					pool.Free -= pages
				}
			}
		}

		// —— vCPU 绑核（FR-CMP-002：核绑定 + NUMA 亲和；pin 缺省 true）——
		if vm.VCPU.Count > 0 && (vm.VCPU.Pin == nil || *vm.VCPU.Pin) {
			cores, cerrs := l.allocateCores(cfg, &vm, used)
			errs = append(errs, cerrs...)
			l.CPU.VMCores[vm.Name] = cores
		}
	}

	l.CPU.Free = minusInts(l.CPU.Isolated, keysOf(used))
	return errs
}

// allocateCores 从剩余额度分配 count 个核。VM 声明的 NUMA 节点存在对应声明时
// 限定在该节点的核内取（FR-CMP-002 缺口明细指明哪个 NUMA）。
func (l *PoolLedger) allocateCores(cfg Config, vm *VMFunction, used map[int]bool) ([]int, []ValidateError) {
	count := vm.VCPU.Count
	vmPath := fmt.Sprintf("virtual-machine-functions[%s]", vm.Name)

	candidates := minusInts(l.CPU.Isolated, keysOf(used))
	where := ""
	if vm.Memory.NumaNode != nil && cfg.ResourcePools != nil && cfg.ResourcePools.CPU != nil {
		for _, n := range cfg.ResourcePools.CPU.Numa {
			if n.Node == *vm.Memory.NumaNode {
				candidates = minusInts(sortedCopy(n.Cores), keysOf(used))
				where = fmt.Sprintf("NUMA %d ", n.Node)
				break
			}
		}
	}

	if len(candidates) < count {
		return nil, []ValidateError{{
			Path: vmPath + ".vcpu.count",
			Message: fmt.Sprintf("%s隔离核不足：需要 %d，可用 %d",
				where, count, len(candidates)),
		}}
	}
	assigned := slices.Clone(candidates[:count])
	for _, c := range assigned {
		used[c] = true
	}
	return assigned, nil
}

// usesNormalMemory 内存 backing=normal（普通内存，仅限无 vhost-user vNIC 的 VM，
// FR-CMP-019）：不占用大页池。
func usesNormalMemory(vm VMFunction) bool {
	return strings.EqualFold(vm.Memory.Backing, "normal")
}

// pagesNeeded 由内存 MB 与页大小换算页数（向上取整）。
func pagesNeeded(sizeMB int, pageSize string) int {
	switch pageSize {
	case "2M":
		return (sizeMB + 1) / 2 // 每页 2MB
	case "1G":
		return (sizeMB + 1023) / 1024 // 每页 1024MB
	default:
		return 0
	}
}

func pageSizeLabel(s string) string {
	if s == "" {
		return "（未指定）"
	}
	return s
}

func sortedCopy(xs []int) []int {
	out := slices.Clone(xs)
	sort.Ints(out)
	return out
}

func minusInts(all, remove []int) []int {
	rm := map[int]bool{}
	for _, x := range remove {
		rm[x] = true
	}
	var out []int
	for _, x := range all {
		if !rm[x] {
			out = append(out, x)
		}
	}
	return out
}

func keysOf(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
