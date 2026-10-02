package system

// 大页池「在用 vs 实际持有」的读视图与回收（决策 #329 起步、#346 扩，FR-SYS-002 /
// FR-CMP-004 / FR-OPS-010）。
//
// 由来：`show system hugepages` / REST / 控制台资源池页会看到「声明 N、内核实际 M（M>N）」
// 的无主占用，产品此前**没有回收路径**（只能重启或手工写 sysctl）。成因已由决策
// #199/#201 消掉（vpp 包自带的 vm.nr_hugepages 撑大机制被 dpkg-divert 接管 + 90 号
// sysctl 钉声明值），但历史遗留 / 手工设置 / 早期版本升上来的机器仍可能带着多余页——
// 本文件给它一条**有界、诚实**的运行时收敛路径。
//
// 决策 #346 补上 #329 的口径盲区（round124/125 真机）：
//   · #329 把「在用 = 内核实际 − 空闲」当作「有持有者的页」，但二者并不相等——内核收缩池
//     （例如开机 `hugepages=4` 预分配、随后按声明收敛到 2）可能留下「已分配却没有任何
//     进程/inode 引用」的页：统计上算「在用」，却谁都用不了。round124 搭 DNS 现场时 1G 池
//     正是被这样一页占着（`nr=2`、`free=0`，唯一持有者只有 sem-vm 的 guest RAM），于是
//     新 VM `Cannot allocate memory` 起不来，而读视图四列全「一致」+「无需回收」，**极易
//     被误读成「环境受限」**。round125 实验证实该页可回收（`echo 1 > nr_hugepages` 能释放它）。
//
// 口径：
//   · 数字如实呈现：**声明**（配置唯一真源）/ **内核实际**（sysfs nr_hugepages）/
//     **在用**（nr − free）/ **实际持有**（遍历 `/proc/*/smaps` 的 hugetlb 映射、按 inode
//     去重、按 KernelPageSize 折算）/ **无主占用**（= 在用 − 实际持有，≥ 0）。取不到内核值就
//     如实说取不到（actual/free/in_use = -1、state=unreadable）；取不到持有值就 held/orphan = -1
//     并给 note，**都不编造**。
//   · 判占用者**不能按 maps 路径过滤**（round125 教训：既漏匿名 hugetlb、又把共享映射当成独立
//     分配）；本实现用 smaps 逐映射行的**内核 hugetlb 标记（VmFlags 含 `ht`）**识别，用
//     `KernelPageSize` 折算到对应页尺寸池，用映射头行的 `dev:inode` 去重——同一页被多进程共享
//     映射（如 vhost-user 同时出现在 qemu 与 VPP 的映射里）只计一次。也**不用**
//     `/proc/<pid>/status` 的 `HugetlbPages`（它是全尺寸总量、不分页尺寸）。
//   · 回收：① 空闲的多余页（#329）= min(实际−声明, 空闲)；② 无主占用页（#346）。无主页的回收
//     机制是「把此尺寸的 nr_hugepages 先写到**实际持有值**（内核只释放空闲页，被引用的页有
//     引用计数、释放不了）→ 回读 → 再写回**声明值** → 回读」，故运行中的 VM/VPP 不受影响。
//   · 有界：每池**至多两次写 + 两次回读**（不重试不循环）；写 sysfs 返回成功不等于池已收敛，
//     必须回读确认。红线：**在用且被引用的页一律不动**；只收敛到**已声明**值——不改声明值
//     （改声明是 `set resource-pools hugepages page-size <size> count <n>`，需 reboot 生效）。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// HugepageSizes 产品托管/展示的两个页尺寸（顺序固定，输出稳定；读不到的池也如实列出）。
var HugepageSizes = []string{"1G", "2M"}

// HugepageSurplusAlarmCode 大页池「实际高于声明且收敛不掉」的告警码（决策 #329）。
// 由启动/60s 巡检按内核实况重建：收敛后自动消警、跨 nfvisd 重启仍可见。
const HugepageSurplusAlarmCode = "HUGEPAGE_POOL_SURPLUS"

// HugepageOrphanAlarmCode 大页池「存在无主占用页」的告警码（决策 #346）：在用 > 实际持有，
// 即分配了却没有任何进程/inode 引用。与 #329 的 HUGEPAGE_POOL_SURPLUS 同一对账位置与口径
// （启动/60s 巡检按内核实况重建、收敛后自动消解、跨 nfvisd 重启仍可见），但有**独立的 scope**。
const HugepageOrphanAlarmCode = "HUGEPAGE_POOL_ORPHAN"

// 池读视图状态（HugepagePoolView.State；枚举顺序按 openapi）。
const (
	HugepageStateOK         = "ok"         // 实际 <= 声明（无多余）
	HugepageStateSurplus    = "surplus"    // 实际 > 声明，且有多余**空闲**页可回收
	HugepageStateInUse      = "in_use"     // 实际 > 声明，多余页全/部分在用（有持有者，回收不掉或只能回收一部分）
	HugepageStateOrphan     = "orphan"     // 存在**无主占用页**（在用 > 实际持有；决策 #346）
	HugepageStateUnmanaged  = "unmanaged"  // 声明值 <= 0：产品不托管该池
	HugepageStateUnreadable = "unreadable" // 内核未提供该页尺寸池（sysfs 不可读）
)

// 回收动作（HugepagePoolResult.Action）。
const (
	HugepageActionNone         = "none"          // 无需回收（实际 <= 声明 或 未托管）
	HugepageActionReclaimed    = "reclaimed"     // 已回收并回读确认收敛到声明
	HugepageActionPartial      = "partial"       // 回收了可回收页，但仍有在用多余页（未收敛）
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

// hugepageSizeForKB 页大小 kB → 产品托管的页尺寸名（1G/2M）；其它尺寸返回 ""（不属于本产品池）。
func hugepageSizeForKB(kb int) string {
	switch kb {
	case 1048576:
		return "1G"
	case 2048:
		return "2M"
	}
	return ""
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

// ---------- 实际持有（决策 #346）：遍历 /proc/*/smaps 的 hugetlb 映射 ----------

// smapsHeaderRe 匹配 smaps 的映射头行：
//
//	起始地址-结束地址 权限 偏移 设备 dev:inode [路径]
//
// 例：`7f8e00000000-7f8e40000000 rw-s 00000000 00:0d 42 /dev/hugepages/...`
var smapsHeaderRe = regexp.MustCompile(`^([0-9a-f]+)-([0-9a-f]+) (\S+) ([0-9a-f]+) (\S+) (\d+)(?:\s+(.*))?$`)

// HugepageHeldPages 遍历 `/proc/<pid>/smaps` 的 hugetlb 映射，汇总每个托管页尺寸「实际持有」
// 的页数（map 恒含 "1G"/"2M" 两键）。ok=false 表示持有值取不到（/proc 不可读 / 无任何进程的
// smaps 可读）——调用方应回 -1 并给 note，**不编造 0**。
//
// 识别与折算（round125 方法教训：判占用者不能按 maps 路径过滤）：
//
//	· 判「是不是 hugetlb 映射」用内核自己的标记：该映射的 `VmFlags` 含 `ht`（HugeTLB）——
//	  这既覆盖文件型 hugetlbfs，也覆盖匿名 hugetlb，不依赖路径。
//	· 页尺寸按该映射的 `KernelPageSize` 折算，归属到同尺寸的池；不属于 1G/2M 的忽略。
//	· 同一页被多进程共享映射（如 vhost-user 同时出现在 qemu 与 VPP 的映射里）：用映射头行的
//	  `dev:inode` 去重，只计一次。
func HugepageHeldPages(root string) (map[string]int, bool) {
	procDir := join(root, "/proc")
	ents, err := os.ReadDir(procDir)
	if err != nil {
		return nil, false
	}
	counts := map[string]int{"1G": 0, "2M": 0}
	seen := map[string]bool{} // dev:inode → 已计（跨进程共享映射只计一次）
	readAny := false
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // 非 pid 目录（mm/sys/…）
		}
		b, err := os.ReadFile(filepath.Join(procDir, e.Name(), "smaps"))
		if err != nil {
			continue // 进程已退出 / 无权限：跳过（不因单个进程读不到就把整个池判为取不到）
		}
		readAny = true
		scanSmapsHeld(b, counts, seen)
	}
	if !readAny {
		// 一个进程的 smaps 都读不到（非 Linux / 无权限）：如实报取不到，不编造 0。
		return nil, false
	}
	return counts, true
}

// scanSmapsHeld 解析一份 smaps 内容，把其中 hugetlb 映射的页数累加进 counts（去重键放进 seen）。
func scanSmapsHeld(data []byte, counts map[string]int, seen map[string]bool) {
	var (
		devInode string // 当前映射头行的 dev:inode
		inBlock  bool
		isHuge   bool // 当前映射是否 hugetlb（VmFlags 含 ht）
		pageKB   int  // KernelPageSize
		sizeKB   int  // Size
	)
	flush := func() {
		if !inBlock || !isHuge || pageKB <= 0 || sizeKB <= 0 {
			return
		}
		size := hugepageSizeForKB(pageKB)
		if size == "" {
			return // 不是本产品托管的页尺寸
		}
		if seen[devInode] {
			return // 同一页被多进程共享映射：只计一次
		}
		seen[devInode] = true
		counts[size] += sizeKB / pageKB
	}
	for _, raw := range strings.Split(string(data), "\n") {
		ln := strings.TrimRight(raw, "\r")
		if m := smapsHeaderRe.FindStringSubmatch(ln); m != nil {
			flush()
			devInode = m[5] + ":" + m[6] // dev:inode
			inBlock, isHuge, pageKB, sizeKB = true, false, 0, 0
			continue
		}
		if !inBlock {
			continue
		}
		switch {
		case strings.HasPrefix(ln, "Size:"):
			sizeKB = smapsKBNum(ln)
		case strings.HasPrefix(ln, "KernelPageSize:"):
			pageKB = smapsKBNum(ln)
		case strings.HasPrefix(ln, "VmFlags:"):
			if smapsHasVmFlag(ln, "ht") {
				isHuge = true
			}
		}
	}
	flush()
}

// smapsKBNum 取 smaps 行里的 kB 数值（如 "KernelPageSize:  2048 kB" → 2048）。
func smapsKBNum(line string) int {
	f := strings.Fields(line)
	if len(f) < 2 {
		return 0
	}
	n, err := strconv.Atoi(f[1])
	if err != nil {
		return 0
	}
	return n
}

// smapsHasVmFlag 判断 VmFlags 行是否含某标志（如 "ht" = HugeTLB）。
func smapsHasVmFlag(line, flag string) bool {
	for _, t := range strings.Fields(strings.TrimPrefix(line, "VmFlags:")) {
		if t == flag {
			return true
		}
	}
	return false
}

// HugepagePoolView 单个大页池的读视图（数字 + 可回收）。
type HugepagePoolView struct {
	PageSize    string `json:"page_size"`
	Managed     bool   `json:"managed"`     // 配置是否声明该池（声明 > 0）
	Declared    int    `json:"declared"`    // 声明页数（配置唯一真源；<=0 = 未声明/不托管）
	Actual      int    `json:"actual"`      // 内核实际页数（sysfs nr_hugepages；不可读时 -1）
	Free        int    `json:"free"`        // 内核空闲页数（不可读时 -1）
	InUse       int    `json:"in_use"`      // 在用页数 = actual - free（不可读时 -1）
	Held        int    `json:"held"`        // 实际持有页数（进程/inode 引用汇总；取不到时 -1）
	Orphan      int    `json:"orphan"`      // 无主占用页数 = in_use - held（>=0；取不到时 -1）
	Reclaimable int    `json:"reclaimable"` // 可回收页数 = 空闲多余页 + 无主占用页
	State       string `json:"state"`
	Note        string `json:"note,omitempty"` // 判定依据 / 取不到的原因（不编造）
}

// HugepagePoolViewFor 纯函数：由声明/实际/空闲/持有（+ 是否可读）产出读视图。
//
// heldOK=false 表示持有值取不到（/proc 不可读）——held/orphan 回 -1 并给 note，不编造。
func HugepagePoolViewFor(pageSize string, declared, actual, free int, readable bool, held int, heldOK bool) HugepagePoolView {
	v := HugepagePoolView{PageSize: pageSize, Declared: declared}
	if !readable {
		v.Actual, v.Free, v.InUse = -1, -1, -1
		v.Held, v.Orphan = -1, -1
		if heldOK {
			v.Held = held // 内核池不可读，但进程持有值仍可读：如实给出
		}
		v.State = HugepageStateUnreadable
		v.Note = "内核未提供该页尺寸的池（sysfs 不可读）——取不到实际值，不编造"
		return v
	}
	v.Actual, v.Free = actual, free
	v.InUse = actual - free
	v.Managed = declared > 0
	if heldOK {
		v.Held = held
		orphan := v.InUse - held
		if orphan < 0 {
			orphan = 0
		}
		v.Orphan = orphan
	} else {
		v.Held, v.Orphan = -1, -1
	}
	heldNote := ""
	if !heldOK {
		heldNote = "；实际持有值取不到（/proc 不可读），无主占用无法判定"
	}

	switch {
	case declared <= 0:
		v.State = HugepageStateUnmanaged
		v.Note = "配置未声明该页尺寸的池，产品不托管（不回收）" + heldNote
	case v.Orphan > 0:
		v.State = HugepageStateOrphan
		p := PlanHugepageReclaim(pageSize, declared, actual, free, held, heldOK)
		v.Reclaimable = p.Reclaimable
		v.Note = fmt.Sprintf("在用 %d 页中实际只有 %d 页有持有者：%d 页无主占用（分配了但无进程/inode 引用），可回收",
			v.InUse, v.Held, v.Orphan)
	case actual <= declared:
		v.State = HugepageStateOK
		v.Note = heldNote
	default:
		p := PlanHugepageReclaim(pageSize, declared, actual, free, held, heldOK)
		v.Reclaimable = p.Reclaimable
		surplus := actual - declared
		if p.Reclaimable == 0 {
			v.State = HugepageStateInUse
			v.Note = fmt.Sprintf("实际 %d 高于声明 %d：多出的 %d 页全部在用（有持有者），无可回收的空闲页%s",
				actual, declared, surplus, heldNote)
		} else if p.Blocked > 0 {
			v.State = HugepageStateInUse
			v.Note = fmt.Sprintf("实际 %d 高于声明 %d：可回收 %d 页，另有 %d 页在用（不可回收）%s",
				actual, declared, p.Reclaimable, p.Blocked, heldNote)
		} else {
			v.State = HugepageStateSurplus
			v.Note = fmt.Sprintf("实际 %d 高于声明 %d：有 %d 页空闲可回收%s", actual, declared, p.Reclaimable, heldNote)
		}
	}
	return v
}

// HugepagePoolViews 读视图全集：两个页尺寸**恒列出**（读不到的池 state=unreadable 并说明），
// 使契约声明的数组形状稳定、客户端不会因机器差异取到空数组。持有值同样遍历 /proc 求取。
func HugepagePoolViews(root string, declared map[string]int) []HugepagePoolView {
	held, heldOK := HugepageHeldPages(root)
	out := make([]HugepagePoolView, 0, len(HugepageSizes))
	for _, size := range HugepageSizes {
		nr, free, ok := ReadHugepagePool(root, size)
		h := 0
		if heldOK {
			h = held[size]
		}
		out = append(out, HugepagePoolViewFor(size, declared[size], nr, free, ok, h, heldOK))
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
	Held        int   // 实际持有（-1 = 取不到）
	HeldOK      bool  // 持有值是否可读
	Orphan      int   // 无主占用 = InUse - Held（>=0；HeldOK=false 时 0，由 HeldOK 表意）
	FreeSurplus int   // 空闲的多余页（#329）
	Reclaimable int   // 本次可回收页数 = FreeSurplus + Orphan
	Writes      []int // 有界写序列（0/1/2 步；无主占用时先降到实际持有、再升回声明）
	Target      int   // 收敛目标内核实际值（写完后的期望 nr；Reclaimable=0 时 = Actual）
	Blocked     int   // 多余但在用（有持有者）、不可回收的页数
	Reasons     []string
}

// PlanHugepageReclaim 纯函数：由声明/实际/空闲/持有算出回收计划。有界（Writes 长度 ≤ 2）。
//
//	declared <= 0            → 未托管，不动作
//	无主占用（in_use > held）→ 写序列 [held, declared]（内核只释放空闲页；被引用的页释放不了）
//	仅空闲的多余页（#329）   → 写序列 [actual - min(actual-declared, free)]
//	无可回收页              → 不动作
func PlanHugepageReclaim(pageSize string, declared, actual, free, held int, heldOK bool) HugepagePlan {
	p := HugepagePlan{PageSize: pageSize, Declared: declared, Actual: actual, Free: free, Held: -1, HeldOK: heldOK, Target: actual}
	p.InUse = actual - free
	if p.InUse < 0 {
		p.InUse = 0
	}
	if heldOK {
		if held < 0 {
			held = 0
		}
		p.Held = held
		orphan := p.InUse - held
		if orphan < 0 {
			orphan = 0
		}
		p.Orphan = orphan
	}

	if declared <= 0 {
		p.Reasons = append(p.Reasons, "配置未声明该页尺寸的池（不托管），不回收")
		return p
	}

	// #329：空闲的多余页 = min(实际-声明, 空闲)。
	if actual > declared {
		surplus := actual - declared
		if free < surplus {
			p.FreeSurplus = free
		} else {
			p.FreeSurplus = surplus
		}
		if p.FreeSurplus < 0 {
			p.FreeSurplus = 0
		}
		p.Blocked = surplus - p.FreeSurplus
	}
	p.Reclaimable = p.FreeSurplus + p.Orphan

	if p.Reclaimable == 0 {
		switch {
		case p.Blocked > 0:
			p.Reasons = append(p.Reasons, fmt.Sprintf("多余的 %d 页全部在用（有持有者），无空闲页可回收", p.Blocked))
		case actual <= declared:
			p.Reasons = append(p.Reasons, fmt.Sprintf("实际 %d 不高于声明 %d，无需回收", actual, declared))
		default:
			p.Reasons = append(p.Reasons, "无可回收的页")
		}
		if !heldOK {
			p.Reasons = append(p.Reasons, "持有值取不到（/proc 不可读），无主占用无法判定，不做无主页回收")
		}
		return p
	}

	if p.Orphan > 0 {
		// 无主占用页（#346）：两步——先降到实际持有值（内核只释放空闲页；被引用的页有引用计数、
		// 释放不了），再升回声明值。运行中的 VM/VPP 因此不受影响。
		p.Writes = []int{p.Held, declared}
		p.Target = declared
		if p.Held > declared {
			p.Target = p.Held // 被引用的页降到声明以下降不动：收敛目标即实际持有
		}
		p.Reasons = append(p.Reasons, fmt.Sprintf("回收无主占用页 %d 页：先把内核收敛到实际持有 %d 页（内核只释放空闲页），再升回声明 %d 页",
			p.Orphan, p.Held, declared))
		if p.FreeSurplus > 0 {
			p.Reasons = append(p.Reasons, fmt.Sprintf("同时回收空闲的多余页 %d 页（%d → 声明 %d）", p.FreeSurplus, actual, declared))
		}
	} else {
		// 只有空闲的多余页（#329）：一次写。
		p.Target = actual - p.FreeSurplus
		p.Writes = []int{p.Target}
		p.Reasons = append(p.Reasons, fmt.Sprintf("回收空闲的多余页 %d 页（%d → %d）", p.FreeSurplus, actual, p.Target))
	}
	if p.Blocked > 0 {
		p.Reasons = append(p.Reasons, fmt.Sprintf("另有 %d 页在用且有持有者，不可回收", p.Blocked))
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
//
// 安全性（决策 #346）：内核的 `set_max_huge_pages` **只释放空闲页**——被进程引用的页有引用计数、
// 释放不了。因此把 nr_hugepages 收敛到「实际持有值」再升回声明值，**运行中的 VM/VPP 不受影响**。
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

	// 以下为决策 #346 的巡检用信息（不进入对外契约 schema，故不序列化）：
	Held   int `json:"-"` // 本次实际持有（-1 = 取不到）
	Orphan int `json:"-"` // 进入对账时观测到的无主占用页数（-1 = 取不到）；供巡检建/消告警
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

// Orphaned 进入本次对账时观测到「无主占用页」的池（供巡检 HUGEPAGE_POOL_ORPHAN 建/消告警）。
func (r HugepageReconcileResult) Orphaned() []HugepagePoolResult {
	out := []HugepagePoolResult{}
	for _, p := range r.Pools {
		if p.Orphan > 0 {
			out = append(out, p)
		}
	}
	return out
}

// executeHugepageWrites 执行有界的写序列（每步后回读），返回最后一次成功回读的 (nr, free)。
// 任一步写失败 / 回读失败即如实返回错误（**不谎称收敛**）。
func executeHugepageWrites(root string, set HugepagePoolSetter, pageSize string, writes []int) (int, int, error) {
	after, free := -1, -1
	for _, target := range writes {
		if err := set.SetPoolPages(pageSize, target); err != nil {
			return after, free, err
		}
		a, f, ok := ReadHugepagePool(root, pageSize)
		if !ok {
			return after, free, fmt.Errorf("写入后回读失败：sysfs 不可读，无法确认是否收敛")
		}
		after, free = a, f
	}
	return after, free, nil
}

// ReconcileHugepages 对账式回收（决策 #329 起步、#346 扩）：既回收「实际 > 声明 且空闲」的多余页
// （#329），也回收**无主占用页**（#346）。每个池**至多两次写 + 两次回读**（有界，不重试、不循环）。
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
	held, heldOK := HugepageHeldPages(root)
	out := HugepageReconcileResult{Pools: []HugepagePoolResult{}}
	for _, size := range HugepageSizes {
		decl := declared[size]
		nr, free, ok := ReadHugepagePool(root, size)
		res := HugepagePoolResult{PageSize: size, Declared: decl, Reasons: []string{}}
		h := 0
		if heldOK {
			h = held[size]
		}
		if !ok {
			res.ActualBefore, res.ActualAfter = -1, -1
			res.InUse, res.Free = -1, -1
			res.Held, res.Orphan = -1, -1
			if heldOK {
				res.Held = h
			}
			res.Action = HugepageActionUnreadable
			res.Reasons = append(res.Reasons, "内核未提供该页尺寸的池（sysfs 不可读）——不动作")
			out.Pools = append(out.Pools, res)
			continue
		}
		plan := PlanHugepageReclaim(size, decl, nr, free, h, heldOK)
		res.ActualBefore, res.InUse, res.Free = nr, plan.InUse, free
		res.Held, res.Orphan = plan.Held, plan.Orphan
		res.Reasons = append(res.Reasons, plan.Reasons...)

		switch {
		case decl <= 0:
			res.ActualAfter = nr
			res.Action = HugepageActionUnmanaged
		case len(plan.Writes) == 0:
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
			after, freeAfter, err := executeHugepageWrites(root, set, size, plan.Writes)
			if err != nil {
				if after < 0 {
					res.ActualAfter = nr
				} else {
					res.ActualAfter = after
					res.Free = freeAfter
				}
				res.Action = HugepageActionVerifyFailed
				res.Error = err.Error()
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
			res.Reclaimed = plan.Reclaimable
			out.Reclaimed += res.Reclaimed
			if after > decl {
				res.Action = HugepageActionPartial
				res.Reasons = append(res.Reasons, fmt.Sprintf("已回收 %d 页，实际仍为 %d（高于声明 %d）：剩余 %d 页在用且有持有者，不可回收",
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
