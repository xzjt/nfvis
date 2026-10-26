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
// **仅 VPP 数据面**有 VPP 保留核：内核数据面下 VPP 不运行，该集合为空（见 NewPoolLedger）。
type CPULedger struct {
	Isolated    []int            // 隔离核全集（升序）
	VppReserved []int            // VPP main-core + corelist-workers（升序）
	VMCores     map[string][]int // VM 名 -> 分配核（升序）
	Free        []int            // 未分配核（升序）
}

// vppReservedCores 由配置推导「VPP 线程核」集合（main-core + corelist-workers）。
//
// **只收显式声明**：`main-core` 的 0 语义是「未显式指定」（network/startup.go 的
// `MainCore > 0` 才写 main-core、向导的「输入 - 表示不配置 VPP CPU」、config 引擎的 vpp
// 空段判定三处一致），此时产品**不向 startup.conf 写任何主核**，VPP 的主线程因而不被钉核
// ——扣哪一枚都没有依据。
//
// ⚠️ 如实边界（round49 定裁）：**不**把核 0 记为缺省主核占用。`docs/` 里没有 VPP 主线程缺省
// 落点的依据，而「未指定就扣核 0」会改动真实机器上的 `vpp-reserved`/`free` 读数（`show
// resource-pools`），并让「隔离核池含 0」的现有配置凭空少一枚可分配核——本仓库既有的期望
// 恰恰相反（orchestrator/compute 的收敛夹具就是 isolated=[0,1,2,3] + 无 vpp 段 + 需要 4 枚核，
// 期望分得满）。故按「无依据不扣」处理：未声明就不算在用者；若日后取得 VPP 缺省主核落点的
// 实证，落点即在此函数（补一条 `reserved[0] = true`）。
func vppReservedCores(cfg Config) []int {
	if cfg.Vpp == nil || cfg.Vpp.CPU == nil {
		return nil
	}
	reserved := map[int]bool{}
	if cfg.Vpp.CPU.MainCore > 0 {
		reserved[cfg.Vpp.CPU.MainCore] = true
	}
	if cores, err := parseCoreList(cfg.Vpp.CPU.CorelistWorkers); err == nil {
		for _, c := range cores {
			reserved[c] = true
		}
	}
	out := make([]int, 0, len(reserved))
	for c := range reserved {
		out = append(out, c)
	}
	sort.Ints(out)
	return out
}

// NewPoolLedger 按配置构建账本并扣减 VPP 保留核。
//
// VPP 线程核只在 **VPP 数据面**下是在用者：内核数据面下 VPP 不运行（vpp 段配置按设计保留，
// 但不下发、VPP 也被停掉），把它记成在用者会让「空闲/可用」读数凭空少核——操作者按该读数
// 规划 VNF 会白白放弃隔离核（同一条口径红线：不得只报声明值，也不得扣不存在的占用者）。
// 故内核数据面下 VppReserved 恒为空。
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
	if cfg.DataPlaneMode() != DataPlaneKernel {
		l.CPU.VppReserved = vppReservedCores(cfg)
	}
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
		out.HugepageSize = vmHugepageSize(cfg, vm)
	}
	return out
}

// AssignedCores 取该 VM 在**资源池账本**里分配到的绑核（与 `show resource-pools` 的「已分配」、
// 以及域 XML 的 `vcpupin` **同源**）。账本有配额缺口时已分配部分仍有效——读视图只报「分到了哪些核」，
// 未分到返回 nil（调用方据此省略该字段，不编造）。
func AssignedCores(cfg Config, vmName string) []int {
	l := NewPoolLedger(cfg)
	_ = l.Allocate(cfg)
	return slices.Clone(l.CPU.VMCores[vmName])
}

// EffectiveVM 返回该 VM 的**有效视图**（决策 #461，收口 round12/13 观察①）：配置里**未显式声明**、
// 但产品语义上有**确定缺省**的项补成有效值——免得界面在运行态明明有事实时显示「—」。
//
//	vCPU 绑定（pin）：缺省 true（FR-CMP-002 绑核是缺省行为）
//	内存后端（backing）：缺省 hugepage（FR-CMP-019：只有显式 normal 才用普通内存）
//	大页页尺寸（hugepage_size）：缺省取资源池主池（与账本/编排同源，见 vmHugepageSize）
//
// **只补有确定缺省的项**：`numa_node` 未声明＝无 NUMA 亲和，界面「—」如实，不做兜底。
// 不改行为（pin 的缺省在账本/域生成里本就是 true），只让读视图与之一致。
func EffectiveVM(cfg Config, vm VMFunction) VMFunction {
	out := vm
	if out.VCPU.Pin == nil {
		t := true
		out.VCPU.Pin = &t
	}
	if !usesNormalMemory(out) {
		if out.Memory.Backing == "" {
			out.Memory.Backing = "hugepage"
		}
		if out.Memory.HugepageSize == "" {
			out.Memory.HugepageSize = vmHugepageSize(cfg, out)
		}
	}
	return out
}

// vmHugepageSize 解析某台 VM **实际生效**的大页页尺寸：显式声明优先，否则取资源池主池
// （首个页池）；两者都没有时为空。**单源**——资源账本（Allocate）、分配结果（AllocationFor）
// 与提交期校验（validate.go，决策 #458）共用，避免三处各写一份而漂移。
func vmHugepageSize(cfg Config, vm VMFunction) string {
	if vm.Memory.HugepageSize != "" {
		return vm.Memory.HugepageSize
	}
	if cfg.ResourcePools != nil && len(cfg.ResourcePools.Hugepages) > 0 {
		return cfg.ResourcePools.Hugepages[0].PageSize
	}
	return ""
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
			pageSize := vmHugepageSize(cfg, vm)
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
		// 归因可读化（决策 #463，收口 round12/13 观察②）：账本按 **VM 名序**依次分配绑核，故
		// 「用户刚扩大的那台」未必是报错这台（真机：把 vnf-a 扩到 2 vcpu，报的是 vnf-b 分不到）。
		// Path 仍指向真正分不到的那台（那是正确的事实），文案补**分配顺序**与**照做路径**，
		// 免得操作者跑去查一台自己没动过的 VM。
		return nil, []ValidateError{{
			Path: vmPath + ".vcpu.count",
			Message: fmt.Sprintf("%s隔离核不足：需要 %d，可用 %d（绑核按 VM 名序依次分配：本台排在后面才分不到；"+
				"若你刚调整的是别的 VM，请缩小它的 vcpu 或扩充隔离核池 set resource-pools cpu isolated-cores …）",
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

// hugepageSizeMB 页尺寸 → MB（1G=1024、2M=2）；未知尺寸返回 0。**单源**——页数换算与
// 提交期「内存必须是页尺寸整数倍」校验（决策 #458）共用。
func hugepageSizeMB(pageSize string) int {
	switch pageSize {
	case "2M":
		return 2
	case "1G":
		return 1024
	default:
		return 0
	}
}

// pagesNeeded 由内存 MB 与页大小换算页数（向上取整）。
func pagesNeeded(sizeMB int, pageSize string) int {
	mb := hugepageSizeMB(pageSize)
	if mb <= 0 {
		return 0
	}
	return (sizeMB + mb - 1) / mb
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
