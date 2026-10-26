package system

// round49（外部评审两条缺陷之一）：`software rollback` 可能选到**比当前更新**或**预发布**的版本。
//
// 缺陷形态（已按源码逐行核实）：
//   - `Rollback` 遍历归档后固定取「最高且非当前」，**不排除高于当前版本**的包
//     （当前不在归档里时同样如此）——当前 `3.0.30` 而归档里有 `3.0.31` 时，
//     它会把 `3.0.31` 装上去：**回退成了升级**；
//   - 归档排序用 `compareVersion`，它把 `~` 当普通分隔符（`strings.FieldsFunc` 按 `~` 切段），
//     于是 `3.0.30~dev1` 因段数更多排在 `3.0.30` 之后——Debian 语义里 `~` 排在**任何**字符之前
//     （含段末），故 `3.0.30~dev1 < 3.0.30`；非数字段还一律按字典序（`3.0.9` 与 `3.0.31` 的
//     数字段被当字符串比）。
//
// 契约（docs/NFViS-openapi.yaml 的 `/system/software:rollback`）写的是「回退到**上一版本**」，
// 即版本序上严格低于当前版本的最大者；没有任何更低版本时必须如实报错，而不是往上装。
//
// 真机形态（round47/48 走查与发版的版本序列）：3.0.29 → 3.0.30~dev1 → 3.0.30 → 3.0.31。

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// TestDebVersionOrderingDebianSemantics 版本比较必须符合 Debian 语义
// （dpkg 的 `--compare-versions`；`[epoch:]upstream[-revision]`）。
//
// 断言目标是对外的 CompareDebVersion（契约点）。红-绿记录：本表在修复前跑在旧实现上
// （当时只有未导出的 compareVersion）即红——跨段/epoch/预发布/数字段共 6 组结论相反。
func TestDebVersionOrderingDebianSemantics(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		// 真实形态：预发布版（~devN）排在正式版**之前**。
		{"3.0.30~dev1", "3.0.30", -1},
		{"3.0.30", "3.0.30~dev1", 1},
		{"3.0.30~dev1", "3.0.30~dev2", -1},
		{"3.0.30~dev10", "3.0.30~dev9", 1}, // 数字段按数值，不是字典序
		{"3.0.30~dev1", "3.0.29", 1},
		// `~` 排在段末（即「短的那一侧」）之前：3.0.30~ < 3.0.30 < 3.0.30.1
		{"3.0.30", "3.0.30.1", -1},
		{"3.0.30~", "3.0.30", -1},
		// 数字段按**数值**比较（字典序会得出 "9" > "31" 的反结论）。
		{"3.0.9", "3.0.31", -1},
		{"1.0.10", "1.0.9", 1},
		{"2.0.0", "1.9.9", 1},
		{"1.0.0", "1.0.0", 0},
		{"3.0.31", "3.0.31", 0},
		// epoch 优先于其余全部（含上游版本大小）。
		{"1:1.0", "2.0", 1},
		{"2.0", "1:1.0", -1},
		{"1:1.0", "1:1.0", 0},
		{"0:2.0", "2.0", 0}, // epoch 0 与「无 epoch」等价
		// 有 debian_revision 的大于没有的；两侧都有时按 revision 比。
		{"3.0.30-1", "3.0.30", 1},
		{"3.0.30", "3.0.30-1", -1},
		{"1.0~rc1-1", "1.0-1", -1},
		{"3.0.30-2", "3.0.30-10", -1},
		// 段末（空）排在 '.' 之前：1.0 < 1.0.0
		{"1.0", "1.0.0", -1},
		{"1.0.0", "1.0", 1},
		// 数字段与字母段交错：同一位置上数字段小于字母段（order('1')=0 < order('a')=97）；
		// 但两条数字段先按数值/长度比——`1.0.10 > 1.0.1a`（"10" 比 "1" 长），这是 dpkg 的行为。
		{"1.0.1", "1.0.a", -1},
		{"1.0.a", "1.0.1", 1},
		{"1.0.10", "1.0.1a", 1},
		{"1.0.1a", "1.0.10", -1},
		// 段末（NUL）排在字母之后：1.0.1a < 1.0.1（dpkg 的著名怪癖，故预发布要用 `~`）。
		{"1.0.1a", "1.0.1", -1},
		// 字母段按 ASCII 序（dpkg 不折叠大小写）。
		{"1.0.1a", "1.0.1b", -1},
		{"1.0.1A", "1.0.1a", -1},
	}
	for _, c := range cases {
		got := CompareDebVersion(c.a, c.b)
		if sign(got) != c.want {
			t.Errorf("CompareDebVersion(%q,%q)=%d（符号 %d），期望 %d", c.a, c.b, got, sign(got), c.want)
		}
		// 反对称性：反向比较必须是相反符号。
		if back := sign(CompareDebVersion(c.b, c.a)); back != -c.want {
			t.Errorf("CompareDebVersion(%q,%q)=%d，与反向比较不对称", c.b, c.a, back)
		}
	}
}

// TestCompareVersionWrapperMatchesDebian 旧的 compareVersion 保留为薄包装：语义必须与
// CompareDebVersion 完全一致（归档排序用它，语义漂移等于改回退判定）。
func TestCompareVersionWrapperMatchesDebian(t *testing.T) {
	for _, p := range [][2]string{
		{"3.0.30~dev1", "3.0.30"}, {"3.0.30", "3.0.31"}, {"1:1.0", "2.0"}, {"3.0.30-1", "3.0.30"},
		{"1.0", "1.0.0"}, {"1.0.10", "1.0.1a"}, {"3.0.30~dev2", "3.0.30~dev1"},
	} {
		want := sign(CompareDebVersion(p[0], p[1]))
		if got := sign(compareVersion(p[0], p[1])); got != want {
			t.Fatalf("compareVersion(%q,%q)=%d 与 CompareDebVersion=%d 不一致", p[0], p[1], got, want)
		}
	}
}

// sign 把比较结果归一到 -1/0/1（只关心序关系，不关心具体差值）。
func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// TestDebVersionSortAscending 归档排序必须按 Debian 序升序（Rollback 依赖「升序取最后一个
// 满足 < 当前的」这一点；排错序哪怕选中的版本名对不上）。
func TestDebVersionSortAscending(t *testing.T) {
	vs := []string{"3.0.31", "3.0.30", "3.0.30~dev2", "3.0.30~dev1", "3.0.29", "1:0.1"}
	sort.Slice(vs, func(i, j int) bool { return CompareDebVersion(vs[i], vs[j]) < 0 })
	want := []string{"3.0.29", "3.0.30~dev1", "3.0.30~dev2", "3.0.30", "3.0.31", "1:0.1"}
	for i := range want {
		if vs[i] != want[i] {
			t.Fatalf("升序排列错误：得 %v，期望 %v", vs, want)
		}
	}
}

// rollbackFixture 造一个「归档 + 当前版本」的夹具，并跑一次 Rollback。
// fakeRunner 的 dpkg-query 答不出状态行 ⇒ 走 unverified 旧口径（不报错），
// 因此这里能干净地断言「选了哪个包」而不被安装判定干扰。
func rollbackFixture(t *testing.T, cur string, archived ...string) (*SoftwareManager, *fakeRunner, SoftwareResult, error) {
	t.Helper()
	shrinkingSettle(t)
	dir := t.TempDir()
	for _, v := range archived {
		writeDeb(t, dir, "nfvis_"+v+"_amd64.deb")
	}
	fr := &fakeRunner{replies: map[string]string{"dpkg-query": cur + "\n"}}
	m := NewSoftwareManager(dir, fr.run, func() string { return cur })
	res, err := m.Rollback(context.Background())
	return m, fr, res, err
}

// installedPaths 返回本次 Rollback 真正 `dpkg -i` 的包路径。
func installedPaths(fr *fakeRunner) []string {
	var out []string
	for _, c := range fr.calls {
		if strings.HasPrefix(c, "dpkg -i ") {
			out = append(out, strings.TrimPrefix(c, "dpkg -i "))
		}
	}
	return out
}

// 本缺陷的**核心用例**：当前 `3.0.30~dev2`，归档含 `3.0.30~dev1` / `3.0.30` / `3.0.29`。
// 上一版（版本序上严格低于当前的最大者）是 `3.0.30~dev1`——不是 `3.0.30`（那是「更高」，
// 因为 ~dev 预发布排在正式版之前），也不是 `3.0.29`（更旧）。
// 修复前的实现取「最高且非当前」= `3.0.30` ⇒ 本用例红。
func TestSoftwareRollbackPicksPreviousNotHighest(t *testing.T) {
	_, fr, res, err := rollbackFixture(t, "3.0.30~dev2", "3.0.29", "3.0.30", "3.0.30~dev1")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if res.Previous != "3.0.30~dev2" {
		t.Fatalf("res.Previous 的语义是「当前版本」：得 %q", res.Previous)
	}
	if !strings.Contains(res.Package, "nfvis_3.0.30~dev1_amd64.deb") {
		t.Fatalf("应回退到版本序上的上一版 3.0.30~dev1（不是 3.0.30、也不是 3.0.29）：%+v", res)
	}
	if got := installedPaths(fr); len(got) != 1 || !strings.HasSuffix(got[0], "nfvis_3.0.30~dev1_amd64.deb") {
		t.Fatalf("dpkg -i 的目标应为 3.0.30~dev1：%v", got)
	}
}

// 归档里同时有更低与更高的版本时，只能选更低的那个（不能往上装）。
func TestSoftwareRollbackNeverPicksNewerVersion(t *testing.T) {
	_, fr, res, err := rollbackFixture(t, "3.0.30", "3.0.29", "3.0.30~dev1", "3.0.31")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if !strings.Contains(res.Package, "nfvis_3.0.30~dev1_amd64.deb") {
		t.Fatalf("应回退到 3.0.30~dev1（严格低于 3.0.30 的最大者）：%+v", res)
	}
	for _, p := range installedPaths(fr) {
		if strings.Contains(p, "3.0.31") {
			t.Fatalf("绝不安装高于当前版本的包：%v", p)
		}
	}
}

// 归档里只有更高的版本（当前版本不在归档中）⇒ 必须报错，且**什么都不装**。
// 这正是「回退成了升级」的现场（归档里留着一个更高版本的包即复现）。
func TestSoftwareRollbackRefusesWhenOnlyNewerArchived(t *testing.T) {
	_, fr, res, err := rollbackFixture(t, "3.0.30", "3.0.31")
	if err == nil {
		t.Fatalf("归档中没有低于当前版本的包时不得回退（修复前会把 3.0.31 装上去）：%+v", res)
	}
	if !strings.Contains(err.Error(), "无可回退") {
		t.Fatalf("错误文案应说明无可回退：%v", err)
	}
	// 归档中可见的版本必须如实列出（操作者据此知道「为什么没有可回退的」）。
	for _, want := range []string{"3.0.31", "3.0.30"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误文案应列出归档可见版本与当前版本（缺 %q）：%v", want, err)
		}
	}
	if got := installedPaths(fr); len(got) != 0 {
		t.Fatalf("没有任何更低版本时不得执行 dpkg -i：%v", got)
	}
	if res.Previous != "3.0.30" {
		t.Fatalf("报错路径上 res.Previous 仍应是当前版本：%+v", res)
	}
}

// 「预发布版被当成上一版」的现场：当前 3.0.29，归档里只有**更高**的 3.0.30 与 3.0.30~dev1。
// 旧比较器把 `~dev1` 排在正式版之前 ⇒ 「最高且非当前」选中预发布版并把它装上去；
// 正确语义下这两个包都高于当前版本 ⇒ 必须报错、什么都不装。
func TestSoftwareRollbackRefusesPrereleaseAboveCurrent(t *testing.T) {
	_, fr, res, err := rollbackFixture(t, "3.0.29", "3.0.30~dev1", "3.0.30")
	if err == nil {
		t.Fatalf("归档中只有高于当前版本的包（含预发布版）时不得回退：%+v", res)
	}
	if !strings.Contains(err.Error(), "无可回退") {
		t.Fatalf("错误文案应说明无可回退：%v", err)
	}
	if got := installedPaths(fr); len(got) != 0 {
		t.Fatalf("不得把高于当前版本的预发布版装上去：%v", got)
	}
}

// 当前版本不在归档中时，仍按版本大小判断（而不是「找不到当前版本就放弃」）。
func TestSoftwareRollbackWorksWhenCurrentNotArchived(t *testing.T) {
	_, _, res, err := rollbackFixture(t, "3.0.30~dev3", "3.0.29", "3.0.30~dev1")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if !strings.Contains(res.Package, "nfvis_3.0.30~dev1_amd64.deb") {
		t.Fatalf("当前版本不在归档中时应选归档里低于它的最大者（3.0.30~dev1）：%+v", res)
	}
}

// 归档里全是预发布版时，正式版仍是「更高的版本」，不能被当作回退目标。
func TestSoftwareRollbackPrefersLowerPrerelease(t *testing.T) {
	_, _, res, err := rollbackFixture(t, "3.0.30~dev3", "3.0.30~dev1", "3.0.30~dev2")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if !strings.Contains(res.Package, "nfvis_3.0.30~dev2_amd64.deb") {
		t.Fatalf("应选 3.0.30~dev2（严格低于 dev3 的最大者）：%+v", res)
	}
}
