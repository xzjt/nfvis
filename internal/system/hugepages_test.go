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

func TestHugepagePoolViewFor(t *testing.T) {
	// 无主占用（有可回收）
	v := HugepagePoolViewFor("1G", 2, 4, 2, true)
	if v.State != HugepageStateSurplus || v.Reclaimable != 2 || v.InUse != 2 || !v.Managed {
		t.Fatalf("surplus 视图不符：%+v", v)
	}
	// 多余页全在用
	v = HugepagePoolViewFor("1G", 2, 4, 0, true)
	if v.State != HugepageStateInUse || v.Reclaimable != 0 {
		t.Fatalf("in_use 视图不符：%+v", v)
	}
	// 一致
	if v = HugepagePoolViewFor("1G", 4, 4, 1, true); v.State != HugepageStateOK {
		t.Fatalf("ok 视图不符：%+v", v)
	}
	// 未托管
	if v = HugepagePoolViewFor("2M", -1, 768, 100, true); v.State != HugepageStateUnmanaged || v.Managed {
		t.Fatalf("unmanaged 视图不符：%+v", v)
	}
	// 取不到内核值：actual/free/in_use 一律 -1，不编造 0
	v = HugepagePoolViewFor("1G", 2, 0, 0, false)
	if v.State != HugepageStateUnreadable || v.Actual != -1 || v.Free != -1 || v.InUse != -1 {
		t.Fatalf("unreadable 视图不符（应回 -1 而非 0）：%+v", v)
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
