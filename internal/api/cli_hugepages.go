package api

// 大页池读视图与回收（决策 #329 起步、#346 扩，FR-SYS-002 / FR-CMP-004 / FR-OPS-010）。
//
// 三面同源：
//   · CLI  `show system hugepages`          与 `request system hugepages reclaim`
//   · REST `GET /system/hugepages`          与 `POST /system/hugepages:reclaim`
//   · Web  控制台「资源池」页（#/system/pools，取同一端点 + 回收按钮）
//
// 数字口径（与 internal/system/hugepages.go 的纯函数同源）：
//
//	声明值   = committed 的 resource-pools 声明（产品唯一真源；deriveKernelDesired 派生）
//	内核实际 = sysfs nr_hugepages
//	在用值   = 内核实际 - 空闲页（**不一定都有持有者**——见下）
//	实际持有 = 遍历 /proc/*/smaps 的 hugetlb 映射、按 inode 去重、按 KernelPageSize 折算
//	无主占用 = 在用 - 实际持有（≥0）：分配了却无进程/inode 引用的页（决策 #346）
//	可回收   = 空闲的多余页（#329）+ 无主占用页（#346）
//
// ⚠️ **在用 ≠ 有持有者**（决策 #346 更正 #329 的措辞）：内核收缩池（如开机 hugepages=4 后按
// 声明收敛）可能留下「谁都持有不到」的页——它算「在用」却没人能用，正是 round124 新 VM
// 起不来的成因。取不到内核值/持有值时如实显示「取不到」，**不编造**。
//
// 回收：既回收空闲的多余页（#329），也回收无主占用页（#346，机制：先把 nr_hugepages 收敛到
// 实际持有值、再升回声明值——内核只释放空闲页，被引用的页释放不了）。**在用且被引用的页一律
// 不动**；只收敛到**已声明**值——不改声明值；改声明是
// `set resource-pools hugepages page-size <size> count <n>`（需 reboot 生效）。

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
	ksys "github.com/xzjt/nfvis/internal/system"
)

// hugepageRoot 读/写 sysfs 的根（真实系统 "/"；测试注入临时目录以免动真机）。
func hugepageRoot(root string) string {
	if root == "" {
		return "/"
	}
	return root
}

// hugepageDeclared 由 committed 配置派生声明值（页尺寸 → 页数；<=0 = 未声明/不托管）。
func hugepageDeclared(cfg model.Config) map[string]int {
	d, err := deriveKernelDesired(cfg)
	if err != nil {
		return map[string]int{}
	}
	return map[string]int{"1G": d.Hugepages1G, "2M": d.Hugepages2M}
}

// hugepagePoolsView 大页池读视图（CLI `show system hugepages` 与 `GET /system/hugepages`
// **同一实现**）。pools 恒为两个页尺寸；reclaimable 为合计可回收页数。
func hugepagePoolsView(root string, cfg model.Config) map[string]any {
	pools := ksys.HugepagePoolViews(hugepageRoot(root), hugepageDeclared(cfg))
	total := 0
	for _, p := range pools {
		total += p.Reclaimable
	}
	return map[string]any{"pools": pools, "reclaimable": total}
}

// hugepageOccupants 返回某页尺寸池「谁在占用」的**可查证据**（决策 #329 的诚实性要求：
// 无法回收时必须说清谁在占用与该怎么做）。证据源：已生效配置的账本、VPP 页尺寸偏好、
// 内核 hugetlbfs 挂载点。查不到具体持有者时如实说明（内核只给总量与空闲数）。
func hugepageOccupants(cfg model.Config, pageSize string) []string {
	var out []string

	// ① 配置账本：哪些 VNF 声明用该页尺寸的大页（声明即占用意图）。
	type vmUse struct {
		name  string
		pages int
	}
	var uses []vmUse
	for _, vm := range cfg.VirtualMachineFunctions {
		if vm.Memory.SizeMB <= 0 || strings.EqualFold(vm.Memory.Backing, "normal") {
			continue // backing=normal 用普通内存，不占大页池
		}
		ps := vm.Memory.HugepageSize
		if ps == "" && cfg.ResourcePools != nil && len(cfg.ResourcePools.Hugepages) > 0 {
			ps = cfg.ResourcePools.Hugepages[0].PageSize // 缺省取资源池主池（与账本同口径）
		}
		if ps != pageSize {
			continue
		}
		uses = append(uses, vmUse{vm.Name, hugepagePagesForMB(vm.Memory.SizeMB, pageSize)})
	}
	sort.Slice(uses, func(i, j int) bool { return uses[i].name < uses[j].name })
	for _, u := range uses {
		out = append(out, fmt.Sprintf("virtual-machine-functions[%s] 声明使用 %s 大页 %d 页", u.name, pageSize, u.pages))
	}

	// ② VPP 数据面：页尺寸偏好（未声明时 VPP 默认 2M）。
	vppPref := "2M"
	if cfg.Vpp != nil && cfg.Vpp.Memory != nil && cfg.Vpp.Memory.HugepagePreference != "" {
		vppPref = cfg.Vpp.Memory.HugepagePreference
	}
	if pageSize == vppPref {
		out = append(out, fmt.Sprintf("VPP 数据面（default-hugepage-size / main-heap-page-size 取 %s）", pageSize))
	}

	// ③ 内核侧：hugetlbfs 挂载（/proc/mounts，按 pagesize= 匹配）。
	out = append(out, hugetlbfsMounts(pageSize)...)

	if len(out) == 0 {
		out = append(out, "未能从配置/VPP/挂载点确定具体持有者（内核只给池总量与空闲数，不编造）")
	}
	return out
}

// hugetlbfsMounts 内核 hugetlbfs 挂载点里按页尺寸匹配的项（作为「有人挂着它」的证据）。
func hugetlbfsMounts(pageSize string) []string {
	kb, ok := ksys.HugepagePageKB(pageSize)
	if !ok {
		return nil
	}
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil
	}
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Fields(ln)
		if len(f) < 4 || f[2] != "hugetlbfs" {
			continue
		}
		opts := f[3]
		if strings.Contains(opts, "pagesize="+pageSize) || strings.Contains(opts, fmt.Sprintf("pagesize=%dkB", kb)) {
			out = append(out, fmt.Sprintf("%s 挂载为 hugetlbfs（pagesize=%s）", f[1], pageSize))
		}
	}
	return out
}

// hugepagePagesForMB 内存 MB → 页数（与 model 账本的换算同口径）。
func hugepagePagesForMB(sizeMB int, pageSize string) int {
	switch pageSize {
	case "2M":
		return (sizeMB + 1) / 2
	case "1G":
		return (sizeMB + 1023) / 1024
	}
	return 0
}

// renderHugepagePools `show system hugepages`：大页池数字（声明/内核实际/在用/实际持有/
// 无主占用）+ 可回收（决策 #329/#346）。
func (x *cliExecutor) renderHugepagePools() string {
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	view := hugepagePoolsView(x.hugepageRoot, cfg)
	pools, _ := view["pools"].([]ksys.HugepagePoolView)

	var b strings.Builder
	fmt.Fprintf(&b, "%-8s %-10s %-10s %-8s %-10s %-10s %-8s %-8s %s\n",
		"页尺寸", "声明", "内核实际", "在用", "实际持有", "无主占用", "空闲", "可回收", "状态")
	for _, p := range pools {
		fmt.Fprintf(&b, "%-8s %-10s %-10s %-8s %-10s %-10s %-8s %-8d %s\n",
			p.PageSize, hugepageDeclaredText(p.Declared), hugepageNum(p.Actual),
			hugepageNum(p.InUse), hugepageNum(p.Held), hugepageNum(p.Orphan),
			hugepageNum(p.Free), p.Reclaimable, hugepageStateText(p.State))
	}

	// 说明：逐池给出判定依据；无主占用时进一步给「谁在占用」与「该怎么做」。
	var notes []string
	for _, p := range pools {
		if p.Note != "" {
			notes = append(notes, fmt.Sprintf("%s：%s", p.PageSize, p.Note))
		}
	}
	if len(notes) > 0 {
		b.WriteString("\n说明：\n")
		for _, n := range notes {
			b.WriteString("  - " + n + "\n")
		}
	}
	total := 0
	if v, ok := view["reclaimable"].(int); ok {
		total = v
	}
	if total > 0 {
		for _, p := range pools {
			if p.Reclaimable > 0 {
				b.WriteString(fmt.Sprintf("  - %s 可回收 %d 页（空闲的多余页 + 无主占用页）：request system hugepages reclaim\n", p.PageSize, p.Reclaimable))
			}
		}
	} else {
		b.WriteString("  - 无需回收（无空闲的多余页、也无无主占用页）\n")
	}
	// 未声明页池的指引（本决策不改声明值；改声明需 reboot）。
	for _, p := range pools {
		if p.State == ksys.HugepageStateUnmanaged {
			b.WriteString(fmt.Sprintf("  - %s 未声明（不托管）：如需声明用 set resource-pools hugepages page-size %s count <n>（变更需 reboot）\n",
				p.PageSize, p.PageSize))
		}
	}
	// 无主占用页：说明它「谁都持有不到」及其回收机制（不编造持有者）。
	for _, p := range pools {
		if p.State == ksys.HugepageStateOrphan {
			b.WriteString(fmt.Sprintf("  - %s 有 %d 页无主占用（在用 %s 页中仅 %s 页有进程/inode 引用）：request system hugepages reclaim 可回收"+
				"（机制：先把 nr_hugepages 收敛到实际持有值、再升回声明值；内核只释放空闲页，运行中的 VM/VPP 不受影响）\n",
				p.PageSize, p.Orphan, hugepageNum(p.InUse), hugepageNum(p.Held)))
		}
	}
	// 多余页全部在用时的证据（不编造）。
	for _, p := range pools {
		if p.State == ksys.HugepageStateInUse && p.Reclaimable == 0 {
			b.WriteString(fmt.Sprintf("  - %s 多余页全部在用，占用者（可查到的证据）：\n", p.PageSize))
			for _, bk := range hugepageOccupants(cfg, p.PageSize) {
				b.WriteString("      · " + bk + "\n")
			}
			b.WriteString("    处置：停掉持页的 VNF 后再回收，或调整声明值（set resource-pools hugepages page-size " +
				p.PageSize + " count <n>，需 reboot）\n")
		}
	}
	x.structured = jsonTree(view)
	return b.String()
}

// requestHugepagesReclaim `request system hugepages reclaim`（决策 #329）：
// 只回收**空闲**的多余页；在用页一律不动；写后回读确认。
func (x *cliExecutor) requestHugepagesReclaim(user string) string {
	if x.hugepages == nil {
		return "%% 大页池回收不可用（编排器未装配）\n"
	}
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	res := ksys.ReconcileHugepages(hugepageRoot(x.hugepageRoot), hugepageDeclared(cfg), x.hugepages,
		func(size string, inUse int) []string { return hugepageOccupants(cfg, size) })

	var b strings.Builder
	b.WriteString("大页池回收（回收空闲的多余页与无主占用页，收敛到声明值；在用且被引用的页不动）：\n")
	fmt.Fprintf(&b, "%-8s %-10s %-10s %-10s %-8s %-8s %s\n",
		"页尺寸", "声明", "回收前", "回收后", "在用", "已回收", "结果")
	for _, p := range res.Pools {
		fmt.Fprintf(&b, "%-8s %-10s %-10s %-10s %-8s %-8d %s\n",
			p.PageSize, hugepageDeclaredText(p.Declared), hugepageNum(p.ActualBefore),
			hugepageNum(p.ActualAfter), hugepageNum(p.InUse), p.Reclaimed, hugepageActionResult(p))
	}
	for _, p := range res.Pools {
		for _, r := range p.Reasons {
			b.WriteString(fmt.Sprintf("  · %s：%s\n", p.PageSize, r))
		}
		if p.Error != "" {
			b.WriteString(fmt.Sprintf("  · %s：%s\n", p.PageSize, p.Error))
		}
		for _, bk := range p.Blockers {
			b.WriteString(fmt.Sprintf("  · %s 占用者：%s\n", p.PageSize, bk))
		}
	}
	unconv := res.Unconverged()
	if len(unconv) == 0 && res.Reclaimed == 0 {
		b.WriteString("无需回收：内核实际已不高于声明值，未做任何改动。\n")
	} else if len(unconv) == 0 {
		b.WriteString(fmt.Sprintf("已回收 %d 页，各池均已收敛到声明值。\n", res.Reclaimed))
	} else {
		b.WriteString("仍有未收敛项（多为在用页挡住，本命令不动在用页）：\n")
		for _, p := range unconv {
			b.WriteString(fmt.Sprintf("  - %s：声明 %s、实际 %s、在用 %s；处置见上方占用者说明\n",
				p.PageSize, hugepageDeclaredText(p.Declared), hugepageNum(p.ActualAfter), hugepageNum(p.InUse)))
		}
	}

	detail := fmt.Sprintf("大页池回收：回收 %d 页", res.Reclaimed)
	failed := 0
	for _, p := range res.Pools {
		if p.Action == ksys.HugepageActionVerifyFailed {
			failed++
		}
	}
	if failed > 0 {
		msg := fmt.Sprintf("%d 个池回收未收敛", failed)
		x.audit(user, "system.hugepages.reclaim", detail+"（"+msg+"）", fmt.Errorf("%s", msg))
		// 写入失败/回读不一致属于「命令没做成」——如实报错（不谎称收敛）。
		b.WriteString("%% 大页池回收未收敛：\n")
		for _, p := range res.Pools {
			if p.Action == ksys.HugepageActionVerifyFailed {
				b.WriteString(fmt.Sprintf("  - %s：%s\n", p.PageSize, p.Error))
			}
		}
		return b.String()
	}
	x.audit(user, "system.hugepages.reclaim", detail, nil)
	x.structured = jsonTree(res)
	return b.String()
}

func hugepageNum(v int) string {
	if v < 0 {
		return "取不到"
	}
	return fmt.Sprintf("%d", v)
}

func hugepageDeclaredText(v int) string {
	if v <= 0 {
		return "未声明"
	}
	return fmt.Sprintf("%d", v)
}

func hugepageStateText(state string) string {
	switch state {
	case ksys.HugepageStateOK:
		return "一致"
	case ksys.HugepageStateSurplus:
		return "有可回收的空闲多余页"
	case ksys.HugepageStateInUse:
		return "多余页在用（回收不了/只能回收一部分）"
	case ksys.HugepageStateOrphan:
		return "有无主占用页（可回收）"
	case ksys.HugepageStateUnmanaged:
		return "未声明（不托管）"
	case ksys.HugepageStateUnreadable:
		return "取不到内核实际值"
	}
	return state
}

func hugepageActionResult(p ksys.HugepagePoolResult) string {
	switch p.Action {
	case ksys.HugepageActionReclaimed:
		return "已回收（回读确认）"
	case ksys.HugepageActionPartial:
		return "部分回收（仍有在用多余页）"
	case ksys.HugepageActionBlocked:
		return "无法回收（多余页全在用）"
	case ksys.HugepageActionNone:
		return "无需回收"
	case ksys.HugepageActionUnmanaged:
		return "未声明（不托管）"
	case ksys.HugepageActionUnreadable:
		return "取不到内核实际值"
	case ksys.HugepageActionVerifyFailed:
		return "失败（写入/回读不一致）"
	}
	return p.Action
}

// HugepageReconcile 按 committed 配置对账回收大页池（决策 #329）。
//
// 供**既有巡检**（启动 / 60s 系统巡检）调用——CLI `request system hugepages reclaim`、
// REST `POST /system/hugepages:reclaim` 与巡检三处共用同一份派生（声明值）与同一份
// 回收实现（ksys.ReconcileHugepages），不存在"巡检一套、命令一套"。
// 调用方负责记日志与维护告警（本函数只做对账 + 回收，不新造定时器）。
func HugepageReconcile(root string, cfg model.Config, set ksys.HugepagePoolSetter) ksys.HugepageReconcileResult {
	return ksys.ReconcileHugepages(hugepageRoot(root), hugepageDeclared(cfg), set,
		func(size string, inUse int) []string { return hugepageOccupants(cfg, size) })
}

// handleGetHugepages GET /api/v1/system/hugepages：大页池三方数字（声明/内核实际/在用）
// 与可回收页数（决策 #329）。与 CLI `show system hugepages` **同一实现**（共用
// hugepagePoolsView），不出现「两条路径两套行为」。
func (s *Server) handleGetHugepages(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hugepagePoolsView(s.hugepageRoot, cfg))
}

// handleHugepagesReclaim POST /system/hugepages:reclaim（决策 #329，super-user）。
//
// 与 CLI `request system hugepages reclaim` 共用 ksys.ReconcileHugepages——只回收**空闲**的
// 多余页（在用页一律不动），写后回读确认；写入失败/回读不一致时 400（如实报错，不谎称收敛）。
func (s *Server) handleHugepagesReclaim(w http.ResponseWriter, r *http.Request) {
	if s.hugepage == nil {
		writeError(w, http.StatusServiceUnavailable, "RUNTIME_UNAVAILABLE", errRuntimeUnavailable, nil)
		return
	}
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	res := ksys.ReconcileHugepages(hugepageRoot(s.hugepageRoot), hugepageDeclared(cfg), s.hugepage,
		func(size string, inUse int) []string { return hugepageOccupants(cfg, size) })
	user := "api"
	if info, ok := Identity(r); ok {
		user = info.User
	}
	detail := fmt.Sprintf("大页池回收：回收 %d 页", res.Reclaimed)
	for _, p := range res.Pools {
		if p.Action == ksys.HugepageActionVerifyFailed {
			s.engine.Audit(user, "system.hugepages.reclaim", detail+": "+p.PageSize+" "+p.Error, "failure")
			writeError(w, http.StatusBadRequest, "VALIDATION_FAILED",
				fmt.Sprintf("大页池 %s 回收未收敛：%s", p.PageSize, p.Error), nil)
			return
		}
	}
	s.engine.Audit(user, "system.hugepages.reclaim", detail, "success")
	writeJSON(w, http.StatusOK, res)
}
