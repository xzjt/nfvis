package system

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ---------- 纯函数：回收计划 ----------

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
			// 这些用例都让「实际持有 = 在用」（无无主占用页），以锁住 #329 的原口径不回归。
			p := PlanHugepageReclaim("1G", c.declared, c.actual, c.free, c.actual-c.free, true)
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

func TestHugepagePoolViewFor(t *testing.T) {
	// 多余页（有可回收）但都有持有者（在用 == 持有）
	v := HugepagePoolViewFor("1G", 2, 4, 2, true, 2, true)
	if v.State != HugepageStateSurplus || v.Reclaimable != 2 || v.InUse != 2 || v.Held != 2 || v.Orphan != 0 || !v.Managed {
		t.Fatalf("surplus 视图不符：%+v", v)
	}
	// 多余页全在用且有持有者
	v = HugepagePoolViewFor("1G", 2, 4, 0, true, 4, true)
	if v.State != HugepageStateInUse || v.Reclaimable != 0 || v.Orphan != 0 {
		t.Fatalf("in_use 视图不符：%+v", v)
	}
	// 一致
	if v = HugepagePoolViewFor("1G", 4, 4, 1, true, 3, true); v.State != HugepageStateOK {
		t.Fatalf("ok 视图不符：%+v", v)
	}
	// 未托管
	if v = HugepagePoolViewFor("2M", -1, 768, 100, true, 668, true); v.State != HugepageStateUnmanaged || v.Managed {
		t.Fatalf("unmanaged 视图不符：%+v", v)
	}
	// 取不到内核值：actual/free/in_use 一律 -1，不编造 0
	v = HugepagePoolViewFor("1G", 2, 0, 0, false, 0, false)
	if v.State != HugepageStateUnreadable || v.Actual != -1 || v.Free != -1 || v.InUse != -1 {
		t.Fatalf("unreadable 视图不符（应回 -1 而非 0）：%+v", v)
	}
	// 无主占用（决策 #346）：在用 2、持有 1 → orphan 1，state=orphan（即便实际 == 声明）。
	v = HugepagePoolViewFor("1G", 2, 2, 0, true, 1, true)
	if v.State != HugepageStateOrphan || v.Orphan != 1 || v.Held != 1 || v.Reclaimable != 1 {
		t.Fatalf("orphan 视图不符：%+v", v)
	}
	// 持有值取不到：held/orphan 一律 -1，note 说明，不编造 0。
	v = HugepagePoolViewFor("1G", 2, 2, 0, true, 0, false)
	if v.Held != -1 || v.Orphan != -1 {
		t.Fatalf("持有值取不到时应回 -1 而非 0：%+v", v)
	}
	if !strings.Contains(v.Note, "实际持有值取不到") {
		t.Fatalf("持有值取不到时 note 应如实说明：%q", v.Note)
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

// ---------- 回收计划：含无主占用页（决策 #346） ----------

func TestPlanHugepageReclaim_Orphan(t *testing.T) {
	// 无主占用（实际 == 声明，但在用 2、持有 1）：两步写 [1, 2]，收敛目标回到声明 2。
	p := PlanHugepageReclaim("1G", 2, 2, 0, 1, true)
	if p.Orphan != 1 || p.Reclaimable != 1 {
		t.Fatalf("orphan 计划不符：%+v", p)
	}
	if len(p.Writes) != 2 || p.Writes[0] != 1 || p.Writes[1] != 2 {
		t.Fatalf("无主页回收应为有界两步写 [1,2]，实得 %v", p.Writes)
	}
	if p.Target != 2 {
		t.Fatalf("收敛目标应回到声明值 2，实得 %d", p.Target)
	}

	// 空闲多余页 + 无主占用页：一起回收（Reclaimable = 空闲多余 + 无主）。
	p = PlanHugepageReclaim("1G", 2, 4, 2, 1, true) // in_use=2, held=1 → orphan=1；空闲多余=2
	if p.Orphan != 1 || p.FreeSurplus != 2 || p.Reclaimable != 3 {
		t.Fatalf("orphan+surplus 计划不符：%+v", p)
	}

	// 持有 > 声明（被引用的页降不下去）：收敛目标 = 实际持有，仍有在用多余页（blocked）。
	p = PlanHugepageReclaim("1G", 2, 4, 0, 3, true) // in_use=4, held=3 → orphan=1；blocked=2
	if p.Orphan != 1 || p.Target != 3 || p.Blocked != 2 {
		t.Fatalf("持有>声明的计划不符：%+v", p)
	}

	// 持有值取不到：不回收无主页（orphan 记 0，理由如实说明取不到）。
	p = PlanHugepageReclaim("1G", 2, 2, 0, 0, false)
	if p.Orphan != 0 || len(p.Writes) != 0 {
		t.Fatalf("持有值取不到时不该回收无主页：%+v", p)
	}
	if !strings.Contains(strings.Join(p.Reasons, "；"), "取不到") {
		t.Fatalf("应如实说明持有值取不到：%v", p.Reasons)
	}
}

// ---------- 对账回收：无主占用页（决策 #346） ----------

// kernelSetter 模拟内核 sysfs 语义：写 nr_hugepages **只释放空闲页**——被进程引用的页（held）
// 有引用计数、释放不了，故池大小不会低于 held；free = 池大小 - held。写入次数可查（验有界）。
type kernelSetter struct {
	t      *testing.T
	root   string
	held   map[string]int
	writes []int
}

func (k *kernelSetter) SetPoolPages(pageSize string, target int) error {
	k.writes = append(k.writes, target)
	held := k.held[pageSize]
	nr := target
	if nr < held {
		nr = held // 被引用的页释放不了
	}
	free := nr - held
	if free < 0 {
		free = 0
	}
	writePool(k.t, k.root, pageSize, nr, free)
	return nil
}

func TestReconcileHugepages_ReclaimsOrphan(t *testing.T) {
	root := t.TempDir()
	writePool(t, root, "1G", 2, 0) // 实际 2、空闲 0：在用 2
	// 进程只持有 1 页（1G）；另一页无主占用。
	writeSmaps(t, root, "555",
		smapsBlock("7f0000000000", "7f0040000000", "00:0d", "42", 1*1048576, 1048576, true, "/dev/hugepages/1-sem-vm/pc.ram"))
	set := &kernelSetter{t: t, root: root, held: map[string]int{"1G": 1}}

	res := ReconcileHugepages(root, map[string]int{"1G": 2}, set, nil)
	p := res.Pools[0]
	if p.PageSize != "1G" {
		t.Fatalf("pools[0] 应为 1G：%+v", p)
	}
	if p.Orphan != 1 || p.Held != 1 {
		t.Fatalf("进入对账时应观测到无主占用 1 页：%+v", p)
	}
	if p.Action != HugepageActionReclaimed || p.Reclaimed != 1 {
		t.Fatalf("无主页应被回收并收敛到声明：%+v", p)
	}
	if len(set.writes) != 2 {
		t.Fatalf("无主占用回收应为有界两步写，实得 %d 次：%v", len(set.writes), set.writes)
	}
	// 独立事实源：回读内核，池仍为声明值 2，且腾出一页空闲（无主页已被释放）。
	if nr, free, _ := ReadHugepagePool(root, "1G"); nr != 2 || free != 1 {
		t.Fatalf("回读 = nr %d free %d，期望 nr 2 free 1（无主页被回收成空闲页）", nr, free)
	}
	if res.Reclaimed != 1 {
		t.Fatalf("合计回收 = %d，期望 1", res.Reclaimed)
	}
	if len(res.Orphaned()) != 1 {
		t.Fatalf("Orphaned 应列出该池（供建 ORPHAN 告警）：%+v", res.Orphaned())
	}
}

func TestReconcileHugepages_OrphanButHeldAboveDeclared(t *testing.T) {
	root := t.TempDir()
	writePool(t, root, "1G", 4, 0) // 实际 4、空闲 0：在用 4
	// 持有 3 页（3 页被引用）、1 页无主占用；声明 2。
	writeSmaps(t, root, "556",
		smapsBlock("7f0000000000", "7f00c0000000", "00:0d", "99", 3*1048576, 1048576, true, "/dev/hugepages/big"))
	set := &kernelSetter{t: t, root: root, held: map[string]int{"1G": 3}}

	res := ReconcileHugepages(root, map[string]int{"1G": 2}, set, nil)
	p := res.Pools[0]
	if p.Reclaimed != 1 || p.Action != HugepageActionPartial {
		t.Fatalf("应部分回收（收掉无主页、被引用页留住）：%+v", p)
	}
	// 独立事实源：被引用的 3 页留住（内核不动在用且被引用的页）。
	if nr, _, _ := ReadHugepagePool(root, "1G"); nr != 3 {
		t.Fatalf("被引用的 3 页应留住，实际应为 3，实得 %d", nr)
	}
}
