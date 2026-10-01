package system

// 大页池「无主占用」的读视图与回收（决策 #329，FR-SYS-002 / FR-CMP-004 / FR-OPS-010）。
//
// 由来：`show system hugepages` / REST / 控制台资源池页会看到「声明 N、内核实际 M（M>N）」
// 的无主占用，产品此前**没有回收路径**（只能重启或手工写 sysctl）。成因已由决策
// #199/#201 消掉（vpp 包自带的 vm.nr_hugepages 撑大机制被 dpkg-divert 接管 + 90 号
// sysctl 钉声明值），但历史遗留 / 手工设置 / 早期版本升上来的机器仍可能带着多余页——
// 本文件给它一条**有界、诚实**的运行时收敛路径。
//
// 口径（决策 #329）：
//   · 三方数字如实呈现：**声明**（配置唯一真源）/ **内核实际**（sysfs nr_hugepages）/
//     **在用**（nr - free）；取不到就如实说取不到（actual=-1、state=unreadable），不编造。
//   · 只回收**空闲**的多余页：target = actual - min(actual-declared, free)，**在用页一律不动**
//     （绝不抽走 VPP/VNF 正在用的大页）。
//   · 有界：每个池每轮至多一次写 + 一次**回读**；写 sysfs 返回成功不等于池已收敛，
//     必须回读内核实际值确认（写后实际 != target 即如实报 verify_failed）。
//   · 实际 <= 声明：不动作（不增长，也不缩到声明以下）。
//   · 本决策只做「收敛到**已声明**值」——不改声明值（改声明是
//     `set resource-pools hugepages page-size <size> count <n>`，需 reboot 生效）。

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// HugepageSizes 产品托管/展示的两个页尺寸（顺序固定，输出稳定；读不到的池也如实列出）。
var HugepageSizes = []string{"1G", "2M"}

// HugepageSurplusAlarmCode 大页池「实际高于声明且收敛不掉」的告警码（决策 #329）。
// 由启动/60s 巡检按内核实况重建：收敛后自动消警、跨 nfvisd 重启仍可见。
const HugepageSurplusAlarmCode = "HUGEPAGE_POOL_SURPLUS"

// 池读视图状态（HugepagePoolView.State）。
const (
	HugepageStateOK         = "ok"         // 实际 <= 声明（无多余）
	HugepageStateSurplus    = "surplus"    // 实际 > 声明，且有多余**空闲**页可回收
	HugepageStateInUse      = "in_use"     // 实际 > 声明，多余页全/部分在用（回收不掉或只能回收一部分）
	HugepageStateUnmanaged  = "unmanaged"  // 声明值 <= 0：产品不托管该池
	HugepageStateUnreadable = "unreadable" // 内核未提供该页尺寸池（sysfs 不可读）
)

// 回收动作（HugepagePoolResult.Action）。
const (
	HugepageActionNone         = "none"          // 无需回收（实际 <= 声明 或 未托管）
	HugepageActionReclaimed    = "reclaimed"     // 已回收并回读确认收敛到声明
	HugepageActionPartial      = "partial"       // 回收了空闲多余页，但仍有在用多余页（未收敛）
	HugepageActionBlocked      = "blocked"       // 有多余页但全部在用，一页都回收不了
	HugepageActionUnmanaged    = "unmanaged"     // 未声明该池，不动作
	HugepageActionUnreadable   = "unreadable"    // 内核未提供该池，不动作
	HugepageActionVerifyFailed = "verify_failed" // 写入失败 / 回读不一致（**不谎称收敛**）
)

// HugepagePageKB 页尺寸 → kB（sysfs 目录名用）；未知尺寸 ok=false。
func HugepagePageKB(pageSize string) (int, bool) {
	switch pageSize {
	case "1G":
		return 1048576, true
	case "2M":
		return 2048, true
	}
	return 0, false
}

// HugepageSysfsRel 某页尺寸 nr_hugepages 的 sysfs 路径（绝对形式；root 前缀见 ReadHugepagePool）。
func HugepageSysfsRel(pageSize string) (string, bool) {
	kb, ok := HugepagePageKB(pageSize)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("/sys/kernel/mm/hugepages/hugepages-%dkB/nr_hugepages", kb), true
}

func hugepageFreeRel(pageSize string) (string, bool) {
	kb, ok := HugepagePageKB(pageSize)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("/sys/kernel/mm/hugepages/hugepages-%dkB/free_hugepages", kb), true
}

// ReadHugepagePool 读某页尺寸池 (nr, free, ok)；sysfs 不可读时 ok=false（**不编造 0**）。
func ReadHugepagePool(root, pageSize string) (int, int, bool) {
	nrRel, ok := HugepageSysfsRel(pageSize)
	if !ok {
		return 0, 0, false
	}
	frRel, _ := hugepageFreeRel(pageSize)
	nrB, e1 := os.ReadFile(join(root, nrRel))
	frB, e2 := os.ReadFile(join(root, frRel))
	if e1 != nil || e2 != nil {
		return 0, 0, false
	}
	nr, er1 := strconv.Atoi(strings.TrimSpace(string(nrB)))
	fr, er2 := strconv.Atoi(strings.TrimSpace(string(frB)))
	if er1 != nil || er2 != nil {
		return 0, 0, false
	}
	if fr > nr { // 防御：free 不应大于 nr
		fr = nr
	}
	if fr < 0 {
		fr = 0
	}
	return nr, fr, true
}

// HugepagePoolView 单个大页池的读视图（三方数字 + 可回收）。
type HugepagePoolView struct {
	PageSize    string `json:"page_size"`
	Managed     bool   `json:"managed"`     // 配置是否声明该池（声明 > 0）
	Declared    int    `json:"declared"`    // 声明页数（配置唯一真源；<=0 = 未声明/不托管）
	Actual      int    `json:"actual"`      // 内核实际页数（sysfs nr_hugepages；不可读时 -1）
	Free        int    `json:"free"`        // 内核空闲页数（不可读时 -1）
	InUse       int    `json:"in_use"`      // 在用页数 = actual - free（不可读时 -1）
	Reclaimable int    `json:"reclaimable"` // 可回收的空闲多余页数
	State       string `json:"state"`
	Note        string `json:"note,omitempty"` // 判定依据 / 取不到的原因（不编造）
}

// HugepagePoolViewFor 纯函数：由声明/实际/空闲（+ 是否可读）产出读视图。
func HugepagePoolViewFor(pageSize string, declared, actual, free int, readable bool) HugepagePoolView {
	v := HugepagePoolView{PageSize: pageSize, Declared: declared}
	if !readable {
		v.Actual, v.Free, v.InUse = -1, -1, -1
		v.State = HugepageStateUnreadable
		v.Note = "内核未提供该页尺寸的池（sysfs 不可读）——取不到实际值，不编造"
		return v
	}
	v.Actual, v.Free = actual, free
	v.InUse = actual - free
	v.Managed = declared > 0
	switch {
	case declared <= 0:
		v.State = HugepageStateUnmanaged
		v.Note = "配置未声明该页尺寸的池，产品不托管（不回收）"
	case actual <= declared:
		v.State = HugepageStateOK
	default:
		surplus := actual - declared
		p := PlanHugepageReclaim(pageSize, declared, actual, free)
		v.Reclaimable = p.Reclaimable
		if p.Reclaimable == 0 {
			v.State = HugepageStateInUse
			v.Note = fmt.Sprintf("实际 %d 高于声明 %d：多出的 %d 页全部在用，无可回收的空闲页", actual, declared, surplus)
		} else if p.Blocked > 0 {
			v.State = HugepageStateInUse
			v.Note = fmt.Sprintf("实际 %d 高于声明 %d：可回收空闲 %d 页，另有 %d 页在用（不可回收）",
				actual, declared, p.Reclaimable, p.Blocked)
		} else {
			v.State = HugepageStateSurplus
			v.Note = fmt.Sprintf("实际 %d 高于声明 %d：有 %d 页空闲可回收", actual, declared, p.Reclaimable)
		}
	}
	return v
}

// HugepagePoolViews 读视图全集：两个页尺寸**恒列出**（读不到的池 state=unreadable 并说明），
// 使契约声明的数组形状稳定、客户端不会因机器差异取到空数组。
func HugepagePoolViews(root string, declared map[string]int) []HugepagePoolView {
	out := make([]HugepagePoolView, 0, len(HugepageSizes))
	for _, size := range HugepageSizes {
		nr, free, ok := ReadHugepagePool(root, size)
		out = append(out, HugepagePoolViewFor(size, declared[size], nr, free, ok))
	}
	return out
}

// HugepagePlan 单个大页池的回收计划（纯函数输出，便于直接单测）。
type HugepagePlan struct {
	PageSize    string
	Declared    int
	Actual      int
	Free        int
	InUse       int
	Reclaimable int // 本次可回收页数（= 空闲的多余页）
	Target      int // 回收后目标页数（Reclaimable=0 时 = Actual）
	Blocked     int // 多余但在用、不可回收的页数
	Reasons     []string
}

// PlanHugepageReclaim 纯函数：由声明/实际/空闲算出回收计划。
//
//	actual <= declared      → 不动作（Target=Actual，Reclaimable=0）
//	declared <= 0            → 未托管，不动作
//	actual > declared        → Reclaimable = min(actual-declared, free)，Target = actual-Reclaimable
func PlanHugepageReclaim(pageSize string, declared, actual, free int) HugepagePlan {
	p := HugepagePlan{PageSize: pageSize, Declared: declared, Actual: actual, Free: free, Target: actual}
	p.InUse = actual - free
	if p.InUse < 0 {
		p.InUse = 0
	}
	switch {
	case declared <= 0:
		p.Reasons = append(p.Reasons, "配置未声明该页尺寸的池（不托管），不回收")
	case actual <= declared:
		p.Reasons = append(p.Reasons, fmt.Sprintf("实际 %d 不高于声明 %d，无需回收", actual, declared))
	default:
		surplus := actual - declared
		if free <= 0 {
			p.Blocked = surplus
			p.Reasons = append(p.Reasons, fmt.Sprintf("多余的 %d 页全部在用，无空闲页可回收", surplus))
			return p
		}
		p.Reclaimable = surplus
		if free < surplus {
			p.Reclaimable = free
		}
		p.Target = actual - p.Reclaimable
		p.Blocked = surplus - p.Reclaimable
		p.Reasons = append(p.Reasons, fmt.Sprintf("回收空闲的多余页 %d 页（%d → %d）", p.Reclaimable, actual, p.Target))
		if p.Blocked > 0 {
			p.Reasons = append(p.Reasons, fmt.Sprintf("另有 %d 页在用，不可回收", p.Blocked))
		}
	}
	return p
}

// HugepagePoolSetter 写大页池目标页数的能力（接口，便于单测注入假源）。
type HugepagePoolSetter interface {
	// SetPoolPages 把某页尺寸池调整为 target 页（写 sysfs nr_hugepages）。
	// 实现**只负责写**；收敛判定由调用方回读内核实际值完成（写成功 ≠ 收敛）。
	SetPoolPages(pageSize string, target int) error
}

// SysfsHugepageSetter 真实实现：写 /sys/kernel/mm/hugepages/hugepages-<kB>kB/nr_hugepages。
//
// 为什么按尺寸写 sysfs 而不是 `vm.nr_hugepages`：后者只作用于**默认尺寸**池，
// 对非默认尺寸池无效；按尺寸的 sysfs 对 1G/2M 都成立（与决策 #106 的双池口径一致）。
type SysfsHugepageSetter struct{ Root string }

// NewSysfsHugepageSetter 真实系统上的落地器（Root="/"）。
func NewSysfsHugepageSetter() SysfsHugepageSetter { return SysfsHugepageSetter{Root: "/"} }

// SetPoolPages 写目标页数。写入失败（权限/文件系统只读/内核不接受）如实返回错误。
func (s SysfsHugepageSetter) SetPoolPages(pageSize string, target int) error {
	rel, ok := HugepageSysfsRel(pageSize)
	if !ok {
		return fmt.Errorf("未知的大页页尺寸 %q", pageSize)
	}
	if target < 0 {
		return fmt.Errorf("目标页数不能为负：%d", target)
	}
	if err := os.WriteFile(join(s.Root, rel), []byte(strconv.Itoa(target)), 0o644); err != nil {
		return fmt.Errorf("写入大页池 %s 目标 %d 页失败（需 root；该内核/虚拟化环境可能不允许运行期调整）：%w",
			pageSize, target, err)
	}
	return nil
}

// HugepagePoolResult 单池的回收执行结果（结构化；CLI/REST/巡检共用）。
type HugepagePoolResult struct {
	PageSize     string   `json:"page_size"`
	Declared     int      `json:"declared"`
	ActualBefore int      `json:"actual_before"`
	ActualAfter  int      `json:"actual_after"`
	InUse        int      `json:"in_use"`
	Free         int      `json:"free"`
	Reclaimed    int      `json:"reclaimed"`
	Action       string   `json:"action"`
	Reasons      []string `json:"reasons"`
	Blockers     []string `json:"blockers,omitempty"` // 无法回收时「谁在占用」的可查证据
	Error        string   `json:"error,omitempty"`    // 写入失败/回读不一致的原因（不谎称收敛）
}

// Converged 该池当前实际是否已不高于声明（未托管/不可读的池不算未收敛）。
func (r HugepagePoolResult) Converged() bool {
	if r.Action == HugepageActionUnmanaged || r.Action == HugepageActionUnreadable {
		return true
	}
	return r.ActualAfter >= 0 && r.ActualAfter <= r.Declared
}

// HugepageReconcileResult 一次对账回收的全体结果。
type HugepageReconcileResult struct {
	Pools     []HugepagePoolResult `json:"pools"`
	Reclaimed int                  `json:"reclaimed"` // 本次实际回收的页数合计
}

// Unconverged 仍未收敛到声明值的池（实际 > 声明：在用页挡住 / 回读不一致）。
func (r HugepageReconcileResult) Unconverged() []HugepagePoolResult {
	out := []HugepagePoolResult{}
	for _, p := range r.Pools {
		if !p.Converged() {
			out = append(out, p)
		}
	}
	return out
}

// ReconcileHugepages 对账式回收（决策 #329）：只回收「实际 > 声明 且空闲」的多余页，
// 每个池**至多一次写 + 一次回读**（有界，不重试、不循环）。
//
// declared：页尺寸 → 声明页数（<=0 / 缺失 = 不托管，不动作）。
// set：写能力（nil = 用 SysfsHugepageSetter{Root: root}）。
// blockers：无法回收/未完全收敛时取「谁在占用」的证据（可 nil）。
//
// 诚实性：写成功不等于收敛——一律回读内核实际值；回读 != 目标即报 verify_failed 并给出原因，
// 绝不把「sysctl/sysfs 写成功」当成「池已收敛」。
func ReconcileHugepages(root string, declared map[string]int, set HugepagePoolSetter,
	blockers func(pageSize string, inUse int) []string) HugepageReconcileResult {

	if set == nil {
		set = SysfsHugepageSetter{Root: root}
	}
	out := HugepageReconcileResult{Pools: []HugepagePoolResult{}}
	for _, size := range HugepageSizes {
		decl := declared[size]
		nr, free, ok := ReadHugepagePool(root, size)
		res := HugepagePoolResult{PageSize: size, Declared: decl, Reasons: []string{}}
		if !ok {
			res.ActualBefore, res.ActualAfter = -1, -1
			res.InUse, res.Free = -1, -1
			res.Action = HugepageActionUnreadable
			res.Reasons = append(res.Reasons, "内核未提供该页尺寸的池（sysfs 不可读）——不动作")
			out.Pools = append(out.Pools, res)
			continue
		}
		plan := PlanHugepageReclaim(size, decl, nr, free)
		res.ActualBefore, res.InUse, res.Free = nr, plan.InUse, free
		res.Reasons = append(res.Reasons, plan.Reasons...)

		switch {
		case decl <= 0:
			res.ActualAfter = nr
			res.Action = HugepageActionUnmanaged
		case plan.Reclaimable == 0:
			res.ActualAfter = nr
			if plan.Blocked > 0 {
				res.Action = HugepageActionBlocked
				if blockers != nil {
					res.Blockers = blockers(size, plan.InUse)
				}
			} else {
				res.Action = HugepageActionNone
			}
		default:
			if err := set.SetPoolPages(size, plan.Target); err != nil {
				res.ActualAfter = nr
				res.Action = HugepageActionVerifyFailed
				res.Error = err.Error()
				break
			}
			after, freeAfter, okAfter := ReadHugepagePool(root, size)
			if !okAfter {
				res.ActualAfter = nr
				res.Action = HugepageActionVerifyFailed
				res.Error = "写入后回读失败：sysfs 不可读，无法确认是否收敛"
				break
			}
			res.ActualAfter, res.Free = after, freeAfter
			res.InUse = after - freeAfter
			if res.InUse < 0 {
				res.InUse = 0
			}
			if after != plan.Target {
				res.Action = HugepageActionVerifyFailed
				res.Error = fmt.Sprintf("写入目标 %d 页后回读为 %d 页（内核未按请求释放：多余的空闲页可能已被并发占用）",
					plan.Target, after)
				break
			}
			res.Reclaimed = nr - after
			if res.Reclaimed < 0 {
				res.Reclaimed = 0
			}
			out.Reclaimed += res.Reclaimed
			if after > decl {
				res.Action = HugepageActionPartial
				res.Reasons = append(res.Reasons, fmt.Sprintf("已回收 %d 页，实际仍为 %d（高于声明 %d）：剩余 %d 页在用，不可回收",
					res.Reclaimed, after, decl, after-decl))
				if blockers != nil {
					res.Blockers = blockers(size, res.InUse)
				}
			} else {
				res.Action = HugepageActionReclaimed
			}
		}
		out.Pools = append(out.Pools, res)
	}
	return out
}

// SortPools 稳定排序辅助（页尺寸 1G 在前、2M 在后；调用方一般不需要）。
func SortPools(pools []HugepagePoolView) {
	order := map[string]int{}
	for i, s := range HugepageSizes {
		order[s] = i
	}
	sort.SliceStable(pools, func(i, j int) bool { return order[pools[i].PageSize] < order[pools[j].PageSize] })
}
