package system

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ---------- 纯函数：回收计划（只回收空闲的多余页，#329） ----------

func TestPlanHugepageReclaim(t *testing.T) {
	cases := []struct {
		name         string
		declared     int
		actual, free int
		wantReclaim  int
		wantTarget   int
		wantBlocked  int
	}{
		{"有空闲的多余页 → 回收空闲那部分", 2, 4, 2, 2, 2, 0},
		{"有空闲但不足（部分在用）", 2, 4, 1, 1, 3, 1},
		{"多余页全在用 → 一页都回收不了", 2, 4, 0, 0, 4, 2},
		{"实际等于声明 → 不动作", 2, 2, 2, 0, 2, 0},
		{"实际少于声明 → 不动作（不增长也不缩）", 2, 1, 1, 0, 1, 0},
		{"未声明（0）→ 不动作", 0, 4, 4, 0, 4, 0},
		{"未声明（-1，不托管）→ 不动作", -1, 4, 4, 0, 4, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := PlanHugepageReclaim("1G", c.declared, c.actual, c.free)
			if p.Reclaimable != c.wantReclaim {
				t.Errorf("Reclaimable = %d，期望 %d", p.Reclaimable, c.wantReclaim)
			}
			if p.Target != c.wantTarget {
				t.Errorf("Target = %d，期望 %d", p.Target, c.wantTarget)
			}
			if p.Blocked != c.wantBlocked {
				t.Errorf("Blocked = %d，期望 %d", p.Blocked, c.wantBlocked)
			}
			if len(p.Reasons) == 0 {
				t.Error("Reasons 不该为空（每个池都要给出判定依据）")
			}
			// 可回收页绝不能越过在用页：目标 >= 在用页数（红线：在用页一律不动）。
			inUse := c.actual - c.free
			if p.Target < inUse {
				t.Errorf("Target %d < 在用页 %d：会抽走在用的大页（红线）", p.Target, inUse)
			}
		})
	}
}

// 计划**不含无主占用页**（决策 #346 撤回无主页回收）：即便「在用 > 持有」，只要没有空闲的
// 多余页（实际 <= 声明），计划就不产生任何写（Reclaimable=0、Target=Actual）。
func TestPlanHugepageReclaim_IgnoresOrphan(t *testing.T) {
	// round125/dev34 形态：实际 == 声明、空闲 <= 0 → 不动作（无主页不在计划内）。
	for _, c := range []struct {
		declared, actual, free int
	}{
		{2, 2, 0},     // 1G：实际 2、空闲 0（在用 2）
		{768, 768, 0}, // 2M：实际 == 声明、空闲 0
		{768, 768, 235},
	} {
		p := PlanHugepageReclaim("1G", c.declared, c.actual, c.free)
		if p.Reclaimable != 0 || p.Target != c.actual {
			t.Errorf("declared=%d actual=%d free=%d：无空闲多余页时不该有召回计划，实得 %+v", c.declared, c.actual, c.free, p)
		}
	}
}

func TestHugepagePoolViewFor(t *testing.T) {
	// 多余页（有可回收）但都有持有者（在用 == 持有）
	v := HugepagePoolViewFor("1G", 2, 4, 2, true, 2, true, 0, true)
	if v.State != HugepageStateSurplus || v.Reclaimable != 2 || v.InUse != 2 || v.Held != 2 || v.Orphan != 0 || !v.Managed {
		t.Fatalf("surplus 视图不符：%+v", v)
	}
	// 多余页全在用且有持有者
	v = HugepagePoolViewFor("1G", 2, 4, 0, true, 4, true, 0, true)
	if v.State != HugepageStateInUse || v.Reclaimable != 0 || v.Orphan != 0 {
		t.Fatalf("in_use 视图不符：%+v", v)
	}
	// 一致
	if v = HugepagePoolViewFor("1G", 4, 4, 1, true, 3, true, 0, true); v.State != HugepageStateOK {
		t.Fatalf("ok 视图不符：%+v", v)
	}
	// 未托管
	if v = HugepagePoolViewFor("2M", -1, 768, 100, true, 668, true, 0, true); v.State != HugepageStateUnmanaged || v.Managed {
		t.Fatalf("unmanaged 视图不符：%+v", v)
	}
	// 取不到内核值：actual/free/in_use 一律 -1，不编造 0
	v = HugepagePoolViewFor("1G", 2, 0, 0, false, 0, false, 0, false)
	if v.State != HugepageStateUnreadable || v.Actual != -1 || v.Free != -1 || v.InUse != -1 {
		t.Fatalf("unreadable 视图不符（应回 -1 而非 0）：%+v", v)
	}
	// 无主占用（决策 #346）：在用 2、持有 1 → orphan 1，state=orphan（即便实际 == 声明）。
	// ⚠️ Reclaimable 仍为 0——无主页**不可回收**，不计入。
	v = HugepagePoolViewFor("1G", 2, 2, 0, true, 1, true, 0, true)
	if v.State != HugepageStateOrphan || v.Orphan != 1 || v.Held != 1 || v.Reclaimable != 0 {
		t.Fatalf("orphan 视图不符（Reclaimable 应为 0，无主页不可回收）：%+v", v)
	}
	if !strings.Contains(v.Note, "释放不了") || !strings.Contains(v.Note, "预留") {
		t.Fatalf("orphan 的 note 应说明预留页与产品侧释放不了：%q", v.Note)
	}
	// dev34 形态（2M）：声明 768 / 实际 768 / 空闲 235 / 持有 23 → 在用 533、无主 510、不可回收。
	v = HugepagePoolViewFor("2M", 768, 768, 235, true, 23, true, 0, true)
	if v.State != HugepageStateOrphan || v.Orphan != 510 || v.InUse != 533 || v.Reclaimable != 0 {
		t.Fatalf("2M dev34 形态视图不符：%+v", v)
	}
	// 持有值取不到：held/orphan 一律 -1，note 说明，不编造 0。
	v = HugepagePoolViewFor("1G", 2, 2, 0, true, 0, false, 0, false)
	if v.Held != -1 || v.Orphan != -1 {
		t.Fatalf("持有值取不到时应回 -1 而非 0：%+v", v)
	}
	if !strings.Contains(v.Note, "实际持有值取不到") {
		t.Fatalf("持有值取不到时 note 应如实说明：%q", v.Note)
	}
}

// 决策 #353：1G 池「数据面固定占用」进读视图（字段 + 说明）。
func TestHugepagePoolViewForDataplane(t *testing.T) {
	// 1G 池且实测数据面占用 >=1 → note 追加可用性说明（Y = 空闲页数）。
	v := HugepagePoolViewFor("1G", 2, 2, 0, true, 2, true, 1, true)
	if v.HeldByDataplane != 1 {
		t.Fatalf("HeldByDataplane = %d，期望 1（实测 comm=vpp 持有）：%+v", v.HeldByDataplane, v)
	}
	if !strings.Contains(v.Note, "数据面（VPP 主堆）固定占用 1 页") ||
		!strings.Contains(v.Note, "VNF 可起页数 = 空闲页数（当前 0）") {
		t.Fatalf("1G 数据面占用说明不符：%q", v.Note)
	}
	// 与既有说明用「；」拼接（1G 无主占用 + 数据面占用并存）。
	v = HugepagePoolViewFor("1G", 2, 2, 0, true, 1, true, 1, true)
	if !strings.Contains(v.Note, "无主占用") || !strings.Contains(v.Note, "；数据面（VPP 主堆）固定占用 1 页") {
		t.Fatalf("数据面说明应与既有说明以「；」拼接：%q", v.Note)
	}
	// dp=0（实测没有数据面占用）：字段为 0，不追加说明——没有占用就不编造。
	v = HugepagePoolViewFor("1G", 2, 2, 1, true, 1, true, 0, true)
	if v.HeldByDataplane != 0 || strings.Contains(v.Note, "数据面（VPP 主堆）") {
		t.Fatalf("dp=0 时不该追加数据面说明：%+v", v)
	}
	// dp 取不到：字段回 -1（不编造 0），不追加说明。
	v = HugepagePoolViewFor("1G", 2, 2, 1, true, 1, true, 0, false)
	if v.HeldByDataplane != -1 || strings.Contains(v.Note, "数据面（VPP 主堆）") {
		t.Fatalf("dp 取不到时应回 -1 且不追加说明：%+v", v)
	}
	// 内核池不可读时仍如实给出数据面占用，空闲写「取不到」（不编造 0）。
	v = HugepagePoolViewFor("1G", 2, 0, 0, false, 0, false, 1, true)
	if v.HeldByDataplane != 1 || !strings.Contains(v.Note, "VNF 可起页数 = 空闲页数（当前 取不到）") {
		t.Fatalf("池不可读时数据面说明应如实写空闲取不到：%+v", v)
	}
	// 非 1G 池即使 dp >= 1 也不追加（本决策只针对 1G 的数据面固定占用）。
	v = HugepagePoolViewFor("2M", 768, 768, 100, true, 668, true, 668, true)
	if v.HeldByDataplane != 668 || strings.Contains(v.Note, "数据面（VPP 主堆）") {
		t.Fatalf("2M 池不该出现 1G 数据面固定占用说明：%+v", v)
	}
}

// ---------- 对账回收（临时 sysfs 根） ----------

// writePool 在临时根下造出某页尺寸池的 sysfs 文件。
func writePool(t *testing.T, root, pageSize string, nr, free int) {
	t.Helper()
	kb, ok := HugepagePageKB(pageSize)
	if !ok {
		t.Fatalf("未知页尺寸 %s", pageSize)
	}
	dir := filepath.Join(root, "sys", "kernel", "mm", "hugepages", fmt.Sprintf("hugepages-%dkB", kb))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nr_hugepages"), []byte(strconv.Itoa(nr)), 0o644); err != nil {
		t.Fatalf("写 nr: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "free_hugepages"), []byte(strconv.Itoa(free)), 0o644); err != nil {
		t.Fatalf("写 free: %v", err)
	}
}

// poolBySize 从对账结果里按页尺寸取池（HugepageSizes = 1G,2M，不能假定下标）。
func poolBySize(t *testing.T, res HugepageReconcileResult, size string) HugepagePoolResult {
	t.Helper()
	for _, p := range res.Pools {
		if p.PageSize == size {
			return p
		}
	}
	t.Fatalf("结果里没有 %s 池：%+v", size, res.Pools)
	return HugepagePoolResult{}
}

func TestReadHugepagePool(t *testing.T) {
	root := t.TempDir()
	writePool(t, root, "1G", 4, 2)
	nr, free, ok := ReadHugepagePool(root, "1G")
	if !ok || nr != 4 || free != 2 {
		t.Fatalf("读 1G 池 = (%d,%d,%v)，期望 (4,2,true)", nr, free, ok)
	}
	if _, _, ok := ReadHugepagePool(root, "2M"); ok {
		t.Fatal("未造 2M 池却读到了——不可读必须返回 ok=false，不编造 0")
	}
}

func TestReconcileHugepages_ReclaimsFreeSurplus(t *testing.T) {
	root := t.TempDir()
	writePool(t, root, "1G", 4, 2) // 声明 2 → 多余 2 页且空闲
	writePool(t, root, "2M", 768, 700)

	res := ReconcileHugepages(root, map[string]int{"1G": 2, "2M": 768}, SysfsHugepageSetter{Root: root}, nil)

	if res.Reclaimed != 2 {
		t.Fatalf("回收页数 = %d，期望 2", res.Reclaimed)
	}
	bySize := map[string]HugepagePoolResult{}
	for _, p := range res.Pools {
		bySize[p.PageSize] = p
	}
	p1 := bySize["1G"]
	if p1.Action != HugepageActionReclaimed || p1.ActualBefore != 4 || p1.ActualAfter != 2 {
		t.Fatalf("1G 结果不符：%+v", p1)
	}
	if p2 := bySize["2M"]; p2.Action != HugepageActionNone {
		t.Fatalf("2M 应为无需回收，实得 %+v", p2)
	}
	// 回读（独立读 sysfs）确认真的降到了声明值。
	if nr, _, _ := ReadHugepagePool(root, "1G"); nr != 2 {
		t.Fatalf("回读 1G nr = %d，期望 2", nr)
	}
	if len(res.Unconverged()) != 0 {
		t.Fatalf("回收后不该有未收敛项：%+v", res.Unconverged())
	}
}

func TestReconcileHugepages_BlockedByInUse(t *testing.T) {
	root := t.TempDir()
	writePool(t, root, "1G", 4, 0) // 多余 2 页但全在用

	res := ReconcileHugepages(root, map[string]int{"1G": 2}, SysfsHugepageSetter{Root: root},
		func(string, int) []string {
			return []string{"virtual-machine-functions[vnf-a] 声明使用 1G 大页 2 页"}
		})

	p := res.Pools[0]
	if p.Action != HugepageActionBlocked || p.Reclaimed != 0 {
		t.Fatalf("在用挡住时应为 blocked 且不回收：%+v", p)
	}
	if len(p.Blockers) == 0 || !strings.Contains(p.Blockers[0], "vnf-a") {
		t.Fatalf("应给出谁在占用的证据：%+v", p.Blockers)
	}
	// 未写 sysfs：在用页一律不动。
	if nr, _, _ := ReadHugepagePool(root, "1G"); nr != 4 {
		t.Fatalf("被挡住时不该改池大小，实得 nr=%d", nr)
	}
	if len(res.Unconverged()) == 0 {
		t.Fatal("被挡住时应报未收敛（供告警呈现）")
	}
}

// 写成功但回读**未变**（内核只释放空闲页；目标页不在空闲链表上）→ 绝不报成功（无假绿）。
func TestReconcileHugepages_ReadbackUnchangedNotReclaimed(t *testing.T) {
	root := t.TempDir()
	writePool(t, root, "2M", 768, 235) // 计划里看似有多余页（此处故意让计划 > 0 以走到写路径）
	// 用一个「写了个寂寞」的落地器模拟内核未按请求变化（写后回读不变）。
	set := &frozenSetter{root: root}

	// 声明低于实际、且有空闲 → plan.Reclaimable > 0，会尝试写。
	res := ReconcileHugepages(root, map[string]int{"2M": 512}, set, nil)
	p := poolBySize(t, res, "2M")
	if p.Action != HugepageActionReclaimed && p.Action != HugepageActionVerifyFailed {
		t.Fatalf("结果应为 reclaimed 或 verify_failed，实得 %+v", p)
	}
	// 回读未变（仍 768）→ 必须 verify_failed 且不计入已回收（不谎称收敛）。
	if p.ActualAfter != 768 {
		t.Fatalf("回读未变时应如实为 768，实得 %d", p.ActualAfter)
	}
	if p.Action != HugepageActionVerifyFailed || p.Error == "" {
		t.Fatalf("回读未变必须报 verify_failed 且带原因：%+v", p)
	}
	if p.Reclaimed != 0 || res.Reclaimed != 0 {
		t.Fatalf("回读未变不该计入已回收：%+v", p)
	}
}

func TestReconcileHugepages_ReadbackMismatchReported(t *testing.T) {
	root := t.TempDir()
	writePool(t, root, "1G", 4, 2)
	// 假落地器：写了个寂寞（不真改文件）→ 回读 != 目标 → verify_failed（不谎称收敛）。
	noop := noopSetter{}

	res := ReconcileHugepages(root, map[string]int{"1G": 2}, noop, nil)
	p := res.Pools[0]
	if p.Action != HugepageActionVerifyFailed || p.Error == "" {
		t.Fatalf("回读不一致必须报 verify_failed 且带原因：%+v", p)
	}
	if p.Reclaimed != 0 || res.Reclaimed != 0 {
		t.Fatalf("回读不一致不该计入已回收：%+v", p)
	}
}

func TestReconcileHugepages_NoGrowthWhenActualBelowDeclared(t *testing.T) {
	root := t.TempDir()
	writePool(t, root, "1G", 1, 1) // 实际 1 < 声明 2
	res := ReconcileHugepages(root, map[string]int{"1G": 2}, SysfsHugepageSetter{Root: root}, nil)
	p := res.Pools[0]
	if p.Action != HugepageActionNone || p.Reclaimed != 0 {
		t.Fatalf("实际<声明时不动作：%+v", p)
	}
	if nr, _, _ := ReadHugepagePool(root, "1G"); nr != 1 {
		t.Fatalf("不该增长池，实得 nr=%d", nr)
	}
}

func TestReconcileHugepages_UnmanagedAndUnreadable(t *testing.T) {
	root := t.TempDir()
	writePool(t, root, "1G", 4, 2)
	// 2M 未造（不可读）；1G 未声明（0）。
	res := ReconcileHugepages(root, map[string]int{"1G": 0}, SysfsHugepageSetter{Root: root}, nil)
	if res.Pools[0].Action != HugepageActionUnmanaged {
		t.Fatalf("1G 未声明应为 unmanaged：%+v", res.Pools[0])
	}
	if res.Pools[1].Action != HugepageActionUnreadable {
		t.Fatalf("2M 不可读应为 unreadable：%+v", res.Pools[1])
	}
	if len(res.Unconverged()) != 0 {
		t.Fatalf("未托管/不可读不算未收敛：%+v", res.Unconverged())
	}
}

func TestSetPoolPages_UnknownSizeAndWrite(t *testing.T) {
	root := t.TempDir()
	writePool(t, root, "2M", 4, 4)
	set := SysfsHugepageSetter{Root: root}
	if err := set.SetPoolPages("1G", 2); err == nil {
		t.Fatal("未知页尺寸应报错")
	}
	if err := set.SetPoolPages("2M", 2); err != nil {
		t.Fatalf("写 2M 目标应成功: %v", err)
	}
	if nr, _, _ := ReadHugepagePool(root, "2M"); nr != 2 {
		t.Fatalf("回读 2M nr = %d，期望 2", nr)
	}
	if err := set.SetPoolPages("2M", -1); err == nil {
		t.Fatal("负目标应报错")
	}
}

type noopSetter struct{}

func (noopSetter) SetPoolPages(string, int) error { return nil }

// frozenSetter 模拟「写 sysfs 成功但内核未按请求变化」（例如目标页不在空闲链表上）：回读不变。
type frozenSetter struct{ root string }

func (s *frozenSetter) SetPoolPages(pageSize string, target int) error {
	// 写目标值，但随即「内核」又把它改回原样——模拟只在空闲页上生效、其余释放不了。
	nr, free, _ := ReadHugepagePool(s.root, pageSize)
	_ = target
	writePoolRaw(s.root, pageSize, nr, free)
	return nil
}

// writePoolRaw 直接按数值写池（无 *testing.T，供假落地器复用）。
func writePoolRaw(root, pageSize string, nr, free int) {
	kb, ok := HugepagePageKB(pageSize)
	if !ok {
		return
	}
	dir := filepath.Join(root, "sys", "kernel", "mm", "hugepages", fmt.Sprintf("hugepages-%dkB", kb))
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "nr_hugepages"), []byte(strconv.Itoa(nr)), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "free_hugepages"), []byte(strconv.Itoa(free)), 0o644)
}

// ---------- 实际持有汇总（决策 #346） ----------

// writeSmaps 在临时根下造出 /proc/<pid>/smaps。
func writeSmaps(t *testing.T, root, pid, content string) {
	t.Helper()
	dir := filepath.Join(root, "proc", pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建 /proc/%s 目录: %v", pid, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "smaps"), []byte(content), 0o644); err != nil {
		t.Fatalf("写 /proc/%s/smaps: %v", pid, err)
	}
}

// smapsBlock 拼一个 smaps 映射块：头行（地址范围/权限/偏移/dev:inode/路径）+ Size +
// KernelPageSize + VmFlags。huge=true 时 VmFlags 带 `ht`（内核的 HugeTLB 标记）。
func smapsBlock(start, end, dev, inode string, sizeKB, pageKB int, huge bool, path string) string {
	flags := "rd wr mr mw me ac"
	if huge {
		flags = "rd wr sh mr mw me ms de ht pf io"
	}
	return fmt.Sprintf("%s-%s rw-s 00000000 %s %s %s\nSize: %d kB\nKernelPageSize: %d kB\nMMUPageSize: %d kB\nVmFlags: %s\n",
		start, end, dev, inode, path, sizeKB, pageKB, pageKB, flags)
}

func TestHugepageHeldPages(t *testing.T) {
	root := t.TempDir()
	// pid 100：1G 映射 2 页 + 2M 映射 3 页 + 一个普通 4kB 映射（不计）+ 一个 THP
	// （KernelPageSize=2M 但无 ht → 不是 hugetlb，不计）。
	pid100 := smapsBlock("7f0000000000", "7f0040000000", "00:0d", "42", 2*1048576, 1048576, true, "/dev/hugepages/1-sem-vm/pc.ram") +
		smapsBlock("7f1000000000", "7f1000600000", "00:0d", "77", 3*2048, 2048, true, "/dev/hugepages/vpp") +
		smapsBlock("7f2000000000", "7f2000001000", "08:01", "999", 4, 4, false, "/usr/lib/x.so") +
		smapsBlock("7f3000000000", "7f3000200000", "00:00", "0", 2*2048, 2048, false, "[heap]")
	// pid 200：与 pid 100 共享同一个 1G hugetlbfs 映射（同 dev:inode → 只应计一次）。
	pid200 := smapsBlock("7fa000000000", "7fa040000000", "00:0d", "42", 2*1048576, 1048576, true, "/dev/hugepages/1-sem-vm/pc.ram")
	writeSmaps(t, root, "100", pid100)
	writeSmaps(t, root, "200", pid200)

	held, ok := HugepageHeldPages(root)
	if !ok {
		t.Fatal("应能读到持有值")
	}
	if held["1G"] != 2 {
		t.Errorf("1G 实际持有 = %d，期望 2（共享映射按 inode 只计一次；普通/THP 映射不计）", held["1G"])
	}
	if held["2M"] != 3 {
		t.Errorf("2M 实际持有 = %d，期望 3", held["2M"])
	}

	// 取不到：/proc 不存在（非 Linux / 权限不足）→ ok=false，调用方应回 -1，不编造 0。
	if _, ok := HugepageHeldPages(t.TempDir()); ok {
		t.Fatal("/proc 不存在时应报「取不到」（ok=false），不编造 0")
	}
}

// ---------- 数据面归属（决策 #353）：HugepageHeldPagesDetail ----------

// writeProcComm 在临时根下造出 /proc/<pid>/comm（决策 #353：数据面按 comm=vpp 归属）。
func writeProcComm(t *testing.T, root, pid, comm string) {
	t.Helper()
	dir := filepath.Join(root, "proc", pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建 /proc/%s 目录: %v", pid, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
		t.Fatalf("写 /proc/%s/comm: %v", pid, err)
	}
}

// 归属口径：comm=vpp 的进程提交的页计入 dataplane；共享页（同 dev:inode）全局只计一次、
// 归属取首个提交进程；comm 读不到的进程按非数据面计（宁少不猜）。
func TestHugepageHeldPagesDetailDataplaneAttribution(t *testing.T) {
	root := t.TempDir()
	// pid 100（comm=vpp）：1G inode 42 共 2 页 + 2M inode 77 共 3 页 → 全部计入数据面。
	writeSmaps(t, root, "100",
		smapsBlock("7f0000000000", "7f0040000000", "00:0d", "42", 2*1048576, 1048576, true, "/dev/hugepages/vpp-heap")+
			smapsBlock("7f1000000000", "7f1000600000", "00:0d", "77", 3*2048, 2048, true, "/dev/hugepages/vpp-buf"))
	writeProcComm(t, root, "100", "vpp_main")
	// pid 200（qemu）：与 vpp 共享 inode 42 的 2 页（应去重不计，归属仍是首个提交者 vpp）
	// + 自己的 1G inode 55 共 1 页（非数据面）。
	writeSmaps(t, root, "200",
		smapsBlock("7fa000000000", "7fa040000000", "00:0d", "42", 2*1048576, 1048576, true, "/dev/hugepages/vpp-heap")+
			smapsBlock("7fb000000000", "7fb040000000", "00:0d", "55", 1*1048576, 1048576, true, "/dev/hugepages/1-sem-vm/pc.ram"))
	writeProcComm(t, root, "200", "qemu-system-x86")
	// pid 300（不写 comm，模拟读不到）：按非数据面计，但页数仍进 total。
	writeSmaps(t, root, "300",
		smapsBlock("7fc000000000", "7fc040000000", "00:0d", "66", 1*1048576, 1048576, true, "/dev/hugepages/1-sem-vm2/pc.ram"))

	total, dp, ok := HugepageHeldPagesDetail(root)
	if !ok {
		t.Fatal("应能读到持有值")
	}
	// 1G = 2（vpp）+ 1（qemu 自有）+ 1（comm 读不到）= 4；共享 inode 42 的三次出现只计 vpp 那次。
	if total["1G"] != 4 {
		t.Errorf("total[1G] = %d，期望 4（共享页按 inode 去重）", total["1G"])
	}
	if total["2M"] != 3 {
		t.Errorf("total[2M] = %d，期望 3", total["2M"])
	}
	// 数据面只算 comm=vpp 的提交：1G 2 页、2M 3 页；qemu/读不到 comm 的进程不计。
	if dp["1G"] != 2 || dp["2M"] != 3 {
		t.Errorf("dataplane = %+v，期望 1G=2 2M=3（仅 comm=vpp 的进程）", dp)
	}
	// 与既有签名同源：HugepageHeldPages 的汇总必须与 Detail 的 total 一致（保留旧调用方不动）。
	held, ok2 := HugepageHeldPages(root)
	if !ok2 || held["1G"] != total["1G"] || held["2M"] != total["2M"] {
		t.Errorf("HugepageHeldPages 与 Detail 不一致：held=%+v total=%+v ok=%v", held, total, ok2)
	}
	// 取不到：/proc 不存在 → ok=false 且 total/dataplane 为 nil（调用方回 -1，不编造 0）。
	if total, dp, ok := HugepageHeldPagesDetail(t.TempDir()); ok || total != nil || dp != nil {
		t.Errorf("/proc 不存在时应报取不到（total/dataplane 为 nil）：ok=%v total=%v dp=%v", ok, total, dp)
	}
}

// 读视图集成：1G 池的数据面占用与说明由 HugepagePoolViews 一次遍历给出；2M 不出现该说明。
func TestHugepagePoolViewsDataplaneField(t *testing.T) {
	root := t.TempDir()
	writePool(t, root, "1G", 2, 0) // 实际 2、空闲 0
	writePool(t, root, "2M", 768, 700)
	writeSmaps(t, root, "100",
		smapsBlock("7f0000000000", "7f0040000000", "00:0d", "42", 1*1048576, 1048576, true, "/dev/hugepages/vpp-heap")+
			smapsBlock("7f1000000000", "7f1000600000", "00:0d", "77", 3*2048, 2048, true, "/dev/hugepages/vpp-buf"))
	writeProcComm(t, root, "100", "vpp_main")

	views := HugepagePoolViews(root, map[string]int{"1G": 2, "2M": 768})
	bySize := map[string]HugepagePoolView{}
	for _, v := range views {
		bySize[v.PageSize] = v
	}
	p1 := bySize["1G"]
	if p1.HeldByDataplane != 1 || !strings.Contains(p1.Note, "VNF 可起页数 = 空闲页数（当前 0）") {
		t.Fatalf("1G 视图应含数据面占用与可用性说明：%+v", p1)
	}
	p2 := bySize["2M"]
	if p2.HeldByDataplane != 3 || strings.Contains(p2.Note, "数据面（VPP 主堆）") {
		t.Fatalf("2M 视图应如实给出数据面占用、但不出现 1G 固定占用说明：%+v", p2)
	}
}

// ---------- 对账回收：无主占用页**不在**回收范围（决策 #346 真机实测撤回） ----------

// dev34 形态（2M）：声明 768 / 实际 768 / 空闲 235；进程只持有 23 页 → 无主占用 510 页。
// reclaim 只回收空闲的多余页（此处实际 <= 声明，无多余页）→ 不动作、不写、如实报无可回收；
// 无主页只作可见性（Orphan/Held 观测 + Orphaned() 供告警），**绝不报成功**。
func TestReconcileHugepages_OrphanNotReclaimed(t *testing.T) {
	root := t.TempDir()
	writePool(t, root, "2M", 768, 235)
	// 进程持有 23 页 2M（ha 映射）。
	writeSmaps(t, root, "555",
		smapsBlock("7f0000000000", "7f0002e00000", "00:0d", "42", 23*2048, 2048, true, "/dev/hugepages/vpp-heap"))

	set := &countingSetter{}
	res := ReconcileHugepages(root, map[string]int{"2M": 768}, set, nil)
	p := poolBySize(t, res, "2M")
	if p.PageSize != "2M" {
		t.Fatalf("应取到 2M 池：%+v", p)
	}
	if p.Held != 23 || p.Orphan != 510 {
		t.Fatalf("进入对账时应观测到持有 23 / 无主 510：%+v", p)
	}
	if p.Action != HugepageActionNone || p.Reclaimed != 0 || res.Reclaimed != 0 {
		t.Fatalf("实际 == 声明（无空闲多余页）时不该回收、更不该报成功：%+v", p)
	}
	if len(set.writes) != 0 {
		t.Fatalf("无空闲多余页时不该写 sysfs，实得 %v", set.writes)
	}
	// 独立事实源：池与空闲数完全没变（无主页写 nr 也释放不了）。
	if nr, free, _ := ReadHugepagePool(root, "2M"); nr != 768 || free != 235 {
		t.Fatalf("回读 = nr %d free %d，期望 nr 768 free 235（无变化）", nr, free)
	}
	// 无主页仍应如实呈现，供 ORPHAN 告警。
	if len(res.Orphaned()) != 1 || res.Orphaned()[0].Orphan != 510 {
		t.Fatalf("Orphaned 应列出该池与 510 页无主（供建 ORPHAN 告警）：%+v", res.Orphaned())
	}
}

// countingSetter 记录写次数（不真改文件）。
type countingSetter struct{ writes []int }

func (s *countingSetter) SetPoolPages(pageSize string, target int) error {
	s.writes = append(s.writes, target)
	return nil
}
