package api

// 决策 #467（收口 round24 登记 R24-A）：`show virtual-switches` 与 REST/Web 三面同源——
//   - 列表 = 运行态 BD 行（逐字不变，含「未在配置中（运行态存在）」行）∪ committed 里
//     type=l3 的交换机行（BD-ID/Learn/Flood/Ports 列「—」、Note 列「l3（VRF）」）；
//   - `<L3名>` 详情 = 与 `show vrfs <名>` **同一实现、同一输出**（L3 交换机的读视图即其 VRF）；
//   - `<L3名> ports|statistics` = 如实说明「L3（VRF）无 L2 成员端口，三层接口见 show vrfs <名>」；
//     `<L3名> mac-table` = 如实说明「仅 L2 交换机支持」。
//
// 关键约束：L2 行的渲染逐字不变（语义套件 cli_bdnames/cli_bdrows 按「首列数字」解析 BD 行，
// L3 行首列是「—」不参与解析）。本文件先红后绿：L3 行缺失、详情报「不存在」、ports/mac-table
// 未给如实说明。

import (
	"strings"
	"testing"
)

// l3Fixture：vs-lan（L2）+ vs-wan（L3，l3-interface ens224 带地址），committed 就绪。
func l3Fixture(t *testing.T) *cliExecutor {
	t.Helper()
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set interfaces ens224 mtu 9000",
		"set virtual-switches vs-lan type l2",
		"set virtual-switches vs-wan type l3",
		"set virtual-switches vs-wan l3-interface ens224 ip address 192.168.98.1/24",
		"commit",
		"exit",
	)
	return x
}

// 列表：运行态 BD 行逐字不变 + L3 行格式（BD-ID 等列「—」、Note「l3（VRF）」），
// BD 行在前、L3 行随后；L3 行首列非数字（语义套件解析器判据不受影响）。
func TestShowVSwitchListIncludesCommittedL3(t *testing.T) {
	x := l3Fixture(t)
	x.setVppState(fakeVppState{bds: []BridgeDomainState{
		// vs-lan 在配置里（note 为空）；phantom 不在配置里（orphan 行必须照旧出现）
		{ID: 42, Name: "vs-lan", Learn: true, Flood: true, Ports: []BridgeDomainPort{{Name: "ens192", SwIfIndex: 1}}},
		{ID: 77, Name: "phantom", Learn: true, Flood: true},
	}})

	out := x.Execute("admin", aaaClassSU, "ssh", "show virtual-switches").Output

	// 表头逐字不变（决策 #495：`Ports` → `Ports(rt)`，明示该列是**运行态**成员数，
	// 与 `show virtual-switches <n> ports` 的派生并集不是同一口径）。
	if !strings.Contains(out, "BD-ID      Name                   Learn  Flood  Ports(rt) Note\n") {
		t.Fatalf("表头应逐字不变:\n%s", out)
	}
	// L2 行逐字不变（含 orphan 行）
	for _, want := range []string{
		"42         vs-lan                 up     up     1         \n",
		"77         phantom                up     up     0         未在配置中（运行态存在）\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("L2/运行态行应逐字不变，缺 %q:\n%s", want, out)
		}
	}
	// L3 行：BD-ID/Learn/Flood/Ports 列「—」、Note 列「l3（VRF）」
	wantL3 := "—          vs-wan                 —      —      —         l3（VRF）\n"
	if !strings.Contains(out, wantL3) {
		t.Fatalf("列表应含 L3 行 %q:\n%s", wantL3, out)
	}
	// 顺序：BD 行在前、L3 行随后
	if i, j := strings.Index(out, "vs-lan"), strings.Index(out, "vs-wan"); !(i >= 0 && j > i) {
		t.Fatalf("L3 行应排在 BD 行之后（vs-lan at %d, vs-wan at %d）:\n%s", i, j, out)
	}
	// 语义套件判据：L3 行首列不得是数字（cli_bdnames/cli_bdrows 只看 `^[0-9]+` 的行）
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "vs-wan") {
			continue
		}
		fs := strings.Fields(line)
		if len(fs) == 0 || fs[0] != "—" {
			t.Fatalf("L3 行首列应为「—」（不得数字，否则破坏 BD 解析器）: %q", line)
		}
	}
	// 结构化输出（`| display json`）同样并入 L3 行
	tree, ok := x.structured.(map[string]any)
	if !ok {
		t.Fatalf("structured 应为对象: %#v", x.structured)
	}
	items, _ := tree["virtual_switches"].([]any)
	if len(items) != 3 {
		t.Fatalf("结构化列表应含 3 行（2 BD + 1 L3），实得 %d: %#v", len(items), items)
	}
	found := false
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["name"] == "vs-wan" {
			found = true
		}
	}
	if !found {
		t.Fatalf("结构化列表应含 vs-wan: %#v", items)
	}
}

// 详情：`show virtual-switches <L3名>`（含 detail）与 `show vrfs <名>` 逐字一致。
// 运行态就位（vs-lan 的 BD 在场）——正是 round24 现场：L3 名此前被错报「在 VPP 中不存在」。
func TestShowVSwitchL3DetailSameAsShowVrfs(t *testing.T) {
	x := l3Fixture(t)
	x.setVppState(fakeVppState{bds: []BridgeDomainState{
		{ID: 42, Name: "vs-lan", Learn: true, Flood: true},
	}})

	want := x.Execute("admin", aaaClassSU, "ssh", "show vrfs vs-wan").Output
	if strings.Contains(want, "%%") || !strings.Contains(want, "vs-wan") {
		t.Fatalf("show vrfs vs-wan 应是 VRF 详情: %q", want)
	}
	if !strings.Contains(want, "192.168.98.1") {
		t.Fatalf("对照输出应含 l3-interface 地址（确认比的是真详情）: %q", want)
	}
	for _, cmd := range []string{"show virtual-switches vs-wan", "show virtual-switches vs-wan detail"} {
		got := x.Execute("admin", aaaClassSU, "ssh", cmd).Output
		if got != want {
			t.Fatalf("%s 应与 show vrfs vs-wan 逐字一致:\n  实得: %q\n  期望: %q", cmd, got, want)
		}
	}
}

// L3 名的 ports/statistics/mac-table 如实说明；L2 各路径（含配置里无 BD 的 L2 名）不回归。
func TestShowVSwitchL3SubviewsHonestText(t *testing.T) {
	x := l3Fixture(t)

	for _, sub := range []string{"ports", "statistics"} {
		out := x.Execute("admin", aaaClassSU, "ssh", "show virtual-switches vs-wan "+sub).Output
		for _, want := range []string{"L3（VRF）", "无 L2 成员端口", "show vrfs vs-wan"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s 应如实说明（含 %q）:\n%s", sub, want, out)
			}
		}
		if strings.Contains(out, "Port ") || strings.Contains(out, "RxPkts") {
			t.Fatalf("%s 不得给 L3 交换机打印 L2 端口表:\n%s", sub, out)
		}
	}

	mac := x.Execute("admin", aaaClassSU, "ssh", "show virtual-switches vs-wan mac-table").Output
	if !strings.Contains(mac, "L3（VRF）") || !strings.Contains(mac, "仅 L2 交换机支持") {
		t.Fatalf("mac-table 应如实说明「仅 L2 交换机支持」:\n%s", mac)
	}

	// L2 详情不受影响：运行态 BD 详情（bd-id）照旧
	x.setVppState(fakeVppState{bds: []BridgeDomainState{
		{ID: 42, Name: "vs-lan", Learn: true, Flood: true},
	}})
	l2out := x.Execute("admin", aaaClassSU, "ssh", "show virtual-switches vs-lan").Output
	if !strings.Contains(l2out, "bd-id") || !strings.Contains(l2out, "vs-lan") || strings.Contains(l2out, "%%") {
		t.Fatalf("L2 详情应仍为运行态 BD 视图:\n%s", l2out)
	}

	// 配置里是 L2、运行态无 BD 的名字：仍报「不存在」（只有 type=l3 才改走 VRF 视图）
	x2, _ := newCLIKit(t)
	run(t, x2, "admin", aaaClassSU, "ssh",
		"configure", "set virtual-switches vs-cfg type l2", "commit", "exit")
	x2.setVppState(fakeVppState{bds: []BridgeDomainState{{ID: 7, Name: "other", Learn: true, Flood: true}}})
	got := x2.Execute("admin", aaaClassSU, "ssh", "show virtual-switches vs-cfg").Output
	if !strings.Contains(got, "不存在") {
		t.Fatalf("L2 且运行态无 BD 的名字应保持既有「不存在」文案:\n%s", got)
	}
}

// 决策 #495（收口 R41-1）：列表这一列的**口径**必须在输出里写明——它是**运行态**成员数，
// 与 `show virtual-switches <n> ports` 的派生并集不是同一口径。此前同族三个视图读出三个
// 数字而没有任何一处说明，操作者无从判断哪个才是「实际有几个口」。
//
// 红-绿：把列名改回 `Ports`、或删掉表尾口径注，本用例即精确报红。
func TestShowVSwitchListStatesPortColumnCaliber(t *testing.T) {
	x := l3Fixture(t)
	x.setVppState(fakeVppState{bds: []BridgeDomainState{
		{ID: 42, Name: "vs-lan", Learn: true, Flood: true,
			Ports: []BridgeDomainPort{{Name: "ens192", SwIfIndex: 1}}},
	}})

	out := x.Execute("admin", aaaClassSU, "ssh", "show virtual-switches").Output
	// 列名带 (rt) 后缀，明示运行态口径。**按表头行匹配**（`Ports(rt)` 在表尾口径注里也
	// 出现一次，只查子串的话改回表头也不会报红——本守护必须钉住「表头本身」）。
	if !strings.Contains(out, "Ports(rt) Note\n") {
		t.Fatalf("列表表头应标出运行态口径（Ports(rt)）:\n%s", out)
	}
	// 表尾口径注：点明运行态数 ≠ 成员读视图，并指向正确的读法
	for _, want := range []string{"运行态", "show virtual-switches", "ports", "口径不同"} {
		if !strings.Contains(out, want) {
			t.Fatalf("表尾口径注应含 %q:\n%s", want, out)
		}
	}
}
