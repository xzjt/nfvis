package system

// 大页池「在用 vs 实际持有」的读视图与回收（决策 #329 起步、#346 扩，FR-SYS-002 /
// FR-CMP-004 / FR-OPS-010）。
//
// 由来：`show system hugepages` / REST / 控制台资源池页会看到「声明 N、内核实际 M（M>N）」
// 的多余页，产品此前**没有回收路径**（只能重启或手工写 sysctl）。成因已由决策
// #199/#201 消掉（vpp 包自带的 vm.nr_hugepages 撑大机制被 dpkg-divert 接管 + 90 号
// sysctl 钉声明值），但历史遗留 / 手工设置 / 早期版本升上来的机器仍可能带着多余页——
// 本文件给它一条**有界、诚实**的运行时收敛路径（回收范围见下）。
//
// 决策 #346 给 #329 补上「实际持有 / 无主占用」两列（round124/125 真机），但真机实测
// （dev34，round126 更正，见 `docs/evidence/v2-round12{4,5}-*.txt`）证明**无主占用页不能
// 靠写 nr_hugepages 回收**：
//   · #329 把「在用 = 内核实际 − 空闲」当作「有持有者的页」，但二者并不相等——读视图因此增两列
//     **实际持有**（按 `/proc/*/smaps` 的 hugetlb 映射、按 inode 去重、按 KernelPageSize 折算）
//     与 **无主占用**（= 在用 − 实际持有，≥ 0），并新增 `state=orphan` 与 `HUGEPAGE_POOL_ORPHAN`
//     告警——**看得见确有效**。
//   · ⚠️ 但「无主占用页」**不是**「内核释放得掉的页」：这类页多为**被进程预留（reserve）但尚未
//     fault 的大页**（例如数据面 DPDK 的预留）——它们**不在空闲链表上**（所以算「在用」）、
//     也**不在任何 smaps 的 ht 映射里**（所以 held 看不到），写 `nr_hugepages` **释放不了**它们
//     （实测 `nr` 不变）。⇒ **本决策只保留「可见性」**（两列 + state=orphan + 告警），
//     **不尝试回收无主占用页**：reclaim 只回收「内核实际 > 声明 **且空闲**」的多余页（#329 原范围）。
//     能释放预留页的是**预留者本身**（如停掉数据面 DPDK / 释放预留），不是产品侧内核 sysfs。
//
// 口径：
//   · 数字如实呈现：**声明**（配置唯一真源）/ **内核实际**（sysfs nr_hugepages）/
//     **在用**（nr − free）/ **实际持有**（遍历 `/proc/*/smaps` 的 hugetlb 映射、按 inode
//     去重、按 KernelPageSize 折算）/ **无主占用**（= 在用 − 实际持有，≥ 0）。取不到内核值就
//     如实说取不到（actual/free/in_use = -1、state=unreadable）；取不到持有值就 held/orphan = -1
//     并给 note，**都不编造**。
//   · 数据面归属（决策 #353，收口 #347）：持有汇总再按进程 `/proc/<pid>/comm` 拆出
//     **数据面占用**（comm 以 vpp 开头的进程提交的页，实测；真机 `vpp_main`）——VPP 主堆固定占 1 个 1G 页且无配置键
//     可释放，「声明 2 却只能起 1 个 VNF」的困惑正源于此。1G 池该值 ≥1 时读视图的 note 追加
//     可用性说明（VNF 可起页数 = 空闲页数）。**纯呈现，不改回收/对账/告警任何语义**。
//   · 判占用者**不能按 maps 路径过滤**（round125 教训：既漏匿名 hugetlb、又把共享映射当成独立
//     分配）；本实现用 smaps 逐映射行的**内核 hugetlb 标记（VmFlags 含 `ht`）**识别，用
//     `KernelPageSize` 折算到对应页尺寸池，用映射头行的 `dev:inode` 去重——同一页被多进程共享
//     映射（如 vhost-user 同时出现在 qemu 与 VPP 的映射里）只计一次。也**不用**
//     `/proc/<pid>/status` 的 `HugetlbPages`（它是全尺寸总量、不分页尺寸）。
//   · 回收：只回收**空闲的多余页**（#329）= min(实际−声明, 空闲)。写 sysfs `nr_hugepages` 只能
//     释放**空闲**页——被进程引用的页、被预留未 fault 的页都释放不了，故**在用页一律不动**。
//   · 有界：每池**至多一次写 + 一次回读**（不重试不循环）；写 sysfs 返回成功不等于池已收敛，
//     必须**回读**内核实际值确认；**无变化即如实报「无可回收的空闲多余页」**，绝不报未发生的成功。
//   · 红线：**在用页一律不动**；只收敛到**已声明**值——不改声明值（改声明是
//     `set resource-pools hugepages page-size <size> count <n>`，需 reboot 生效）。

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
// 即分配了却没有任何进程/inode 引用（多为被进程预留但未使用的大页）。与 #329 的
// HUGEPAGE_POOL_SURPLUS 同一对账位置与口径（启动/60s 巡检按内核实况重建、收敛后自动消解、
// 跨 nfvisd 重启仍可见），但有**独立的 scope**。**只作可见性告警**——产品侧回收不动这类页。
const HugepageOrphanAlarmCode = "HUGEPAGE_POOL_ORPHAN"

// 池读视图状态（HugepagePoolView.State；枚举顺序按 openapi）。
const (
	HugepageStateOK         = "ok"         // 实际 <= 声明（无多余）
	HugepageStateSurplus    = "surplus"    // 实际 > 声明，且有多余**空闲**页可回收
	HugepageStateInUse      = "in_use"     // 实际 > 声明，多余页全/部分在用（不可回收）
	HugepageStateOrphan     = "orphan"     // 存在**无主占用页**（在用 > 实际持有；决策 #346；产品侧不可回收）
	HugepageStateUnmanaged  = "unmanaged"  // 声明值 <= 0：产品不托管该池
	HugepageStateUnreadable = "unreadable" // 内核未提供该页尺寸池（sysfs 不可读）
)

// 回收动作（HugepagePoolResult.Action）。
const (
	HugepageActionNone         = "none"          // 无需回收（实际 <= 声明 或 未托管）；无可回收的空闲多余页
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
//
// ⚠️ 「无主占用 = 在用 − 实际持有」里那些页**不在这份汇总里**——它们恰恰是没有任何 smaps 引用的
// 页（多为被进程预留 reserve 但未 fault 的大页），故本函数**看不到**它们，这也正是它们「无主」的原因。
//
// 需要区分「这页算谁的」时用 HugepageHeldPagesDetail（决策 #353）；本函数保持既有签名与语义。
func HugepageHeldPages(root string) (map[string]int, bool) {
	total, _, ok := HugepageHeldPagesDetail(root)
	return total, ok
}

// dataplaneCommPrefix 数据面进程名前缀（`/proc/<pid>/comm` 的**主线程名**）：真机实测 VPP 为
// `vpp_main`（worker 线程另名 `vpp_wk_*`，但 `/proc/<pid>/comm` 只反映主线程）——**用前缀
// `vpp` 匹配**（round133 上机核对的实测值；勿写死 "vpp" 精确值）。决策 #353 用它把持有页
// 归属出「数据面占用」。
const dataplaneCommPrefix = "vpp"

// HugepageHeldPagesDetail 在 HugepageHeldPages 的基础上，把「数据面（comm 以 vpp 开头，实测 vpp_main）提交的页」
// 单独归属出来（决策 #353）。total 与 HugepageHeldPages 的返回值完全一致。
//
// 归属口径（round133b 上机修正——**独占归属**，勿改回「首见归属」）：
//
//	· 对每个 hugetlb 页（dev:inode）记录**全部映射它的进程**是否数据面（comm 以 "vpp" 开头，
//	  真机实测 `vpp_main`）。一页**仅由数据面进程映射、无其它进程共享**才计入 dataplane。
//	  ⚠️ 真机教训：vhost-user 会把 **VNF 的 guest RAM 大页映射进 VPP 进程**（该页同时出现在
//	  vpp_main 与 qemu 的 smaps 里）——若按「首个被扫描到的进程」归属，/proc 目录序一变
//	  （如 VNF 重启换 pid）同一现场会在 1↔2 间翻转（round133b Browser Use 复核抓到，CLI 与
//	  REST 读数互相矛盾）。guest RAM 是 VNF 的页、不是「数据面固定占用」，独占判据把它排除。
//	· comm 读不到的进程**按非数据面计**——宁少不猜；因此「无其它进程共享」若含 comm 读不到的
//	  进程，也会把该页排除在 dataplane 之外（保守、不虚报数据面占用）。
//	· 每页全局只计一次（total 与旧实现一致）。
//
// ok=false 表示一个进程的 smaps 都读不到（非 Linux / 无权限）：total/dataplane 为 nil，
// 调用方应回 -1 并给 note，**不编造 0**。
func HugepageHeldPagesDetail(root string) (total, dataplane map[string]int, ok bool) {
	procDir := join(root, "/proc")
	ents, err := os.ReadDir(procDir)
	if err != nil {
		return nil, nil, false
	}
	// 每页记录：页尺寸 + 页数 + 映射者构成（是否数据面映射过 / 是否被非数据面映射过）。
	type heldPage struct {
		size    string
		pages   int
		byDP    bool
		byOther bool
	}
	pages := map[string]*heldPage{} // dev:inode → 记录（跨进程共享映射只记一次）
	readAny := false
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // 非 pid 目录（mm/sys/…）
		}
		pidDir := filepath.Join(procDir, e.Name())
		b, err := os.ReadFile(filepath.Join(pidDir, "smaps"))
		if err != nil {
			continue // 进程已退出 / 无权限：跳过（不因单个进程读不到就把整个池判为取不到）
		}
		readAny = true
		isDP := strings.HasPrefix(readProcComm(pidDir), dataplaneCommPrefix)
		for _, pg := range scanSmapsPages(b) {
			rec, ok := pages[pg.key]
			if !ok {
				rec = &heldPage{size: pg.size, pages: pg.pages}
				pages[pg.key] = rec
			}
			if isDP {
				rec.byDP = true
			} else {
				rec.byOther = true
			}
		}
	}
	if !readAny {
		// 一个进程的 smaps 都读不到（非 Linux / 无权限）：如实报取不到，不编造 0。
		return nil, nil, false
	}
	total = map[string]int{"1G": 0, "2M": 0}
	dataplane = map[string]int{"1G": 0, "2M": 0}
	for _, rec := range pages {
		total[rec.size] += rec.pages
		if rec.byDP && !rec.byOther {
			dataplane[rec.size] += rec.pages // 独占归属：仅数据面映射的页才算「数据面固定占用」
		}
	}
	return total, dataplane, true
}

// readProcComm 读 `/proc/<pid>/comm`（进程名）；读不到返回 ""（按非数据面计——不猜）。
func readProcComm(pidDir string) string {
	b, err := os.ReadFile(filepath.Join(pidDir, "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// heldPageRef 一份 smaps 里的一个托管 hugetlb 页（按 dev:inode 标识）。
type heldPageRef struct {
	key   string // dev:inode（跨进程共享映射的同一页同键）
	size  string // "1G" / "2M"
	pages int    // 该映射的页数（sizeKB / KernelPageSize）
}

// scanSmapsPages 解析一份 smaps 内容，返回其中**托管尺寸**的 hugetlb 映射（逐页记录，不去重）。
// 去重与归属由调用方（HugepageHeldPagesDetail）按 dev:inode 汇总——独占归属需要看到**所有**
// 映射者，故本函数不做 seen 过滤（决策 #353，round133b 修正）。
func scanSmapsPages(data []byte) []heldPageRef {
	var (
		out      []heldPageRef
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
		out = append(out, heldPageRef{key: devInode, size: size, pages: sizeKB / pageKB})
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
	return out
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
	PageSize string `json:"page_size"`
	Managed  bool   `json:"managed"`  // 配置是否声明该池（声明 > 0）
	Declared int    `json:"declared"` // 声明页数（配置唯一真源；<=0 = 未声明/不托管）
	Actual   int    `json:"actual"`   // 内核实际页数（sysfs nr_hugepages；不可读时 -1）
	Free     int    `json:"free"`     // 内核空闲页数（不可读时 -1）
	InUse    int    `json:"in_use"`   // 在用页数 = actual - free（不可读时 -1）
	Held     int    `json:"held"`     // 实际持有页数（进程/inode 引用汇总；取不到时 -1）
	// HeldByDataplane 其中数据面（comm 以 vpp 开头，即 VPP 主堆/缓冲）提交的页数（实测；取不到时 -1）。
	// 决策 #353：VPP 主堆固定占 1 个 1G 页且无配置键可释放——这列回答「池里的页算谁的」。
	HeldByDataplane int    `json:"held_by_dataplane"`
	Orphan          int    `json:"orphan"`      // 无主占用页数 = in_use - held（>=0；取不到时 -1）
	Reclaimable     int    `json:"reclaimable"` // 可回收的**空闲多余页**数（无主占用页不可回收，故不计入）
	State           string `json:"state"`
	Note            string `json:"note,omitempty"` // 判定依据 / 取不到的原因（不编造）
}

// HugepagePoolViewFor 纯函数：由声明/实际/空闲/持有/数据面占用（+ 是否可读）产出读视图。
//
// heldOK=false 表示持有值取不到（/proc 不可读）——held/orphan 回 -1 并给 note，不编造。
// dataplane/dataplaneOK 为「数据面（comm 以 vpp 开头）提交的页数」及其可取性（决策 #353）——取不到时
// HeldByDataplane 回 -1；1G 池且实测占用 >=1 时 note 追加可用性说明（VNF 可起页数 = 空闲页数）。
// 注意：无主占用（orphan）**只作可见性呈现**，不计入 Reclaimable（产品侧回收不动这类页）。
func HugepagePoolViewFor(pageSize string, declared, actual, free int, readable bool,
	held int, heldOK bool, dataplane int, dataplaneOK bool) HugepagePoolView {

	v := HugepagePoolView{PageSize: pageSize, Declared: declared}
	v.HeldByDataplane = -1
	if dataplaneOK {
		v.HeldByDataplane = dataplane
	}
	if !readable {
		v.Actual, v.Free, v.InUse = -1, -1, -1
		v.Held, v.Orphan = -1, -1
		if heldOK {
			v.Held = held // 内核池不可读，但进程持有值仍可读：如实给出
		}
		v.State = HugepageStateUnreadable
		v.Note = joinHugepageNote("内核未提供该页尺寸的池（sysfs 不可读）——取不到实际值，不编造",
			hugepageDataplaneNote(pageSize, v.HeldByDataplane, v.Free))
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
		v.Reclaimable = PlanHugepageReclaim(pageSize, declared, actual, free).Reclaimable
		v.Note = fmt.Sprintf("在用 %d 页中仅 %d 页有进程/inode 引用：%d 页无主占用（多为被进程预留但尚未使用的大页，例如数据面 DPDK 预留）。"+
			"它们不在空闲链表上，产品侧写 nr_hugepages 释放不了、reclaim 不会动它们——需从预留者一侧释放（如停/重启数据面或释放预留）%s",
			v.InUse, v.Held, v.Orphan, heldNote)
	case actual <= declared:
		v.State = HugepageStateOK
		v.Note = heldNote
	default:
		p := PlanHugepageReclaim(pageSize, declared, actual, free)
		v.Reclaimable = p.Reclaimable
		surplus := actual - declared
		if p.Reclaimable == 0 {
			v.State = HugepageStateInUse
			v.Note = fmt.Sprintf("实际 %d 高于声明 %d：多出的 %d 页全部在用（不在空闲链表上，可能含被进程预留的大页），无可回收的空闲页%s",
				actual, declared, surplus, heldNote)
		} else if p.Blocked > 0 {
			v.State = HugepageStateInUse
			v.Note = fmt.Sprintf("实际 %d 高于声明 %d：可回收空闲 %d 页，另有 %d 页在用（不可回收）%s",
				actual, declared, p.Reclaimable, p.Blocked, heldNote)
		} else {
			v.State = HugepageStateSurplus
			v.Note = fmt.Sprintf("实际 %d 高于声明 %d：有 %d 页空闲可回收%s", actual, declared, p.Reclaimable, heldNote)
		}
	}
	v.Note = joinHugepageNote(v.Note, hugepageDataplaneNote(pageSize, v.HeldByDataplane, free))
	return v
}

// hugepageDataplaneNote 1G 池且实测有数据面占用时的可用性说明（决策 #353）。非 1G 池、
// 占用值取不到（-1）、占用为 0 时返回空串——不编造；空闲值取不到时如实写「取不到」。
func hugepageDataplaneNote(pageSize string, dataplane, free int) string {
	if pageSize != "1G" || dataplane < 1 {
		return ""
	}
	freeText := "取不到"
	if free >= 0 {
		freeText = strconv.Itoa(free)
	}
	return fmt.Sprintf("数据面（VPP 主堆）固定占用 %d 页（实测；不可配置释放）——VNF 可起页数 = 空闲页数（当前 %s）",
		dataplane, freeText)
}

// joinHugepageNote 用「；」拼接读视图说明（空串跳过；既有说明在前、追加说明在后）。
func joinHugepageNote(base, extra string) string {
	switch {
	case extra == "":
		return base
	case base == "":
		return extra
	}
	return base + "；" + extra
}

// HugepagePoolViews 读视图全集：两个页尺寸**恒列出**（读不到的池 state=unreadable 并说明），
// 使契约声明的数组形状稳定、客户端不会因机器差异取到空数组。持有值与数据面归属同样遍历 /proc 求取
// （决策 #353；两者同一次遍历，取不到时一律回 -1）。
func HugepagePoolViews(root string, declared map[string]int) []HugepagePoolView {
	held, dataplane, heldOK := HugepageHeldPagesDetail(root)
	out := make([]HugepagePoolView, 0, len(HugepageSizes))
	for _, size := range HugepageSizes {
		nr, free, ok := ReadHugepagePool(root, size)
		h, d := 0, 0
		if heldOK {
			h, d = held[size], dataplane[size]
		}
		out = append(out, HugepagePoolViewFor(size, declared[size], nr, free, ok, h, heldOK, d, heldOK))
	}
	return out
}

// HugepagePlan 单个大页池的回收计划（纯函数输出，便于直接单测）。
//
// 计划**只覆盖「空闲的多余页」**（#329）：无主占用页不可回收（见文件顶部说明），故不在计划内。
type HugepagePlan struct {
	PageSize    string
	Declared    int
	Actual      int
	Free        int
	InUse       int
	Reclaimable int // 本次可回收页数 = 空闲的多余页 = min(实际-声明, 空闲)
	Target      int // 回收后目标内核实际值（Reclaimable=0 时 = Actual）
	Blocked     int // 多余但在用（不在空闲链表上）、不可回收的页数
	Reasons     []string
}

// PlanHugepageReclaim 纯函数：由声明/实际/空闲算出回收计划（只回收空闲的多余页，#329）。
//
//	actual <= declared      → 不动作（Target=Actual，Reclaimable=0）
//	declared <= 0            → 未托管，不动作
//	actual > declared        → Reclaimable = min(actual-declared, free)，Target = actual - Reclaimable
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
			p.Reasons = append(p.Reasons, fmt.Sprintf("多余的 %d 页全部在用（不在空闲链表上，可能含被进程预留的大页），无空闲页可回收", surplus))
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
			p.Reasons = append(p.Reasons, fmt.Sprintf("另有 %d 页在用（不可回收）", p.Blocked))
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
//
// 能力边界（决策 #346 真机实测）：内核的 `set_max_huge_pages` **只能释放空闲页**——被进程引用
// 的页（有引用计数）与被进程**预留但未 fault** 的页（不在空闲链表上）都**释放不了**。因此把
// `nr_hugepages` 写小只会腾出真正的空闲页；写入成功不代表池变小，必须回读核验。
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

// Unconverged 仍未收敛到声明值的池（实际 > 声明：在用/预留页挡住 / 回读不一致）。
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

// ReconcileHugepages 对账式回收（决策 #329）：只回收「实际 > 声明 **且空闲**」的多余页，
// 每个池**至多一次写 + 一次回读**（有界，不重试、不循环）。**无主占用页不在回收范围**
// （决策 #346：这类页多为被进程预留但未 fault 的大页，写 nr_hugepages 释放不了），
// 只在结果里如实呈现（供 HUGEPAGE_POOL_ORPHAN 告警）。
//
// declared：页尺寸 → 声明页数（<=0 / 缺失 = 不托管，不动作）。
// set：写能力（nil = 用 SysfsHugepageSetter{Root: root}）。
// blockers：无法回收/未完全收敛时取「谁在占用」的证据（可 nil）。
//
// 诚实性：写成功不等于收敛——一律**回读**内核实际值；回读 != 目标即报 verify_failed 并给出原因；
// 回读无变化（目标未达成）即如实报，绝不把「sysctl/sysfs 写成功」当成「池已收敛」。
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
		if !ok {
			res.ActualBefore, res.ActualAfter = -1, -1
			res.InUse, res.Free = -1, -1
			res.Held, res.Orphan = -1, -1
			if heldOK {
				res.Held = held[size]
			}
			res.Action = HugepageActionUnreadable
			res.Reasons = append(res.Reasons, "内核未提供该页尺寸的池（sysfs 不可读）——不动作")
			out.Pools = append(out.Pools, res)
			continue
		}
		// held/orphan 观测值（决策 #346 可见性；不参与回收）。
		res.Held = -1
		if heldOK {
			res.Held = held[size]
			orphan := nr - free - held[size]
			if orphan < 0 {
				orphan = 0
			}
			res.Orphan = orphan
		} else {
			res.Orphan = -1
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
				res.Error = fmt.Sprintf("写入目标 %d 页后回读为 %d 页（内核只释放空闲页：目标页可能不在空闲链表上/已被并发占用）",
					plan.Target, after)
				break
			}
			res.Reclaimed = nr - after // 基于回读的**实际**观测（不是计划值）
			if res.Reclaimed < 0 {
				res.Reclaimed = 0
			}
			out.Reclaimed += res.Reclaimed
			if after > decl {
				res.Action = HugepageActionPartial
				res.Reasons = append(res.Reasons, fmt.Sprintf("已回收 %d 页，实际仍为 %d（高于声明 %d）：剩余 %d 页在用或在预留中，不可回收",
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
