package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

// 决策 #84：`show virtual-switches` 与 `show interfaces physical` 必须反映**运行态**，
// 而不是 committed 配置。缺陷复现（真机）：VPP 中存在但未写入配置的 BD 不显示；
// 接口在 VPP 中已 down 而 CLI 仍显示 up（该列取自配置的 enabled）。

// fakeVppState 假的 VPP 运行态快照。
type fakeVppState struct {
	bds   []BridgeDomainState
	ifs   map[string]InterfaceState
	bdErr error
	ifErr error
}

func (f fakeVppState) BridgeDomains() ([]BridgeDomainState, error) { return f.bds, f.bdErr }

func (f fakeVppState) InterfaceStates() (map[string]InterfaceState, error) {
	return f.ifs, f.ifErr
}

// 列表取自运行态：**配置里没有、VPP 里有的** BD 必须出现，并标注；反之不出现。
func TestShowVSwitchListIsRuntimeNotConfig(t *testing.T) {
	x, _ := newCLIKit(t)
	// 配置里只声明 vs-cfg；VPP 里是 vs-rt（+ 一个配置中没有的 phantom）
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "set virtual-switches vs-cfg type l2", "commit", "exit")
	x.setVppState(fakeVppState{bds: []BridgeDomainState{
		{ID: 4242, Name: "phantom", Learn: true, Flood: true, Ports: []BridgeDomainPort{{Name: "ens192", SwIfIndex: 1}}},
	}})

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-switches").Output
	if !strings.Contains(out, "phantom") {
		t.Fatalf("运行态存在的 BD 应被列出: %q", out)
	}
	if !strings.Contains(out, "未在配置中") {
		t.Fatalf("应标注「未在配置中（运行态存在）」: %q", out)
	}
	if strings.Contains(out, "vs-cfg") {
		t.Fatalf("配置里有但 VPP 中没有的交换机不应出现（该视图已改为运行态）: %q", out)
	}
	if !strings.Contains(out, "Learn") || !strings.Contains(out, "4242") {
		t.Fatalf("应含状态与 BD-ID 列: %q", out)
	}
}

// ports/statistics 必须给「成员端口及状态/计数」，而不是同一份配置 dump。
func TestShowVSwitchPortsHasStateAndCounters(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "set virtual-switches vs-rt type l2", "commit", "exit")
	x.setVppState(fakeVppState{
		bds: []BridgeDomainState{{ID: 7, Name: "vs-rt", Learn: true, Flood: true,
			Ports: []BridgeDomainPort{{Name: "ens192", SwIfIndex: 1}, {Name: "ens224", SwIfIndex: 2, Shg: 3}}}},
		ifs: map[string]InterfaceState{
			"ens192": {AdminUp: true, LinkUp: true, LinkSpeed: 10_000_000, DevType: "vmxnet3"},
			"ens224": {AdminUp: true, LinkUp: false},
		},
	})
	for _, sub := range []string{"ports", "statistics"} {
		out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-switches vs-rt "+sub).Output
		for _, want := range []string{"Port", "Admin", "Link", "RxPkts", "TxPkts", "ens192", "ens224"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s 应含 %q（契约: 成员端口及状态/计数）\n%s", sub, want, out)
			}
		}
		if strings.Contains(out, "type l2") {
			t.Fatalf("%s 不应回落到配置 dump:\n%s", sub, out)
		}
	}
}

// 运行态不可用：明确报错，**不得**退回配置视图（那正是缺陷来源）。
func TestShowVSwitchRuntimeUnavailableIsExplicit(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "set virtual-switches vs-cfg type l2", "commit", "exit")
	x.setVppState(fakeVppState{bdErr: errors.New("VPP socket 不可用")})

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-switches").Output
	if !strings.Contains(out, "运行态不可用") {
		t.Fatalf("应明确说明运行态不可用: %q", out)
	}
	if strings.Contains(out, "vs-cfg") {
		t.Fatalf("不得退回配置视图: %q", out)
	}
}

// 接口链接状态取自 VPP（而非配置的 enabled）：VPP 说 down 就必须显示 down。
func TestShowInterfacesPhysicalLinkStateFromVPP(t *testing.T) {
	x, _ := newCLIKit(t)
	// 配置里 ens224 未 disable（即 enabled），若取自配置则应显示 up
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "set interfaces ens224 description cfg-up", "commit", "exit")
	x.setVppState(fakeVppState{ifs: map[string]InterfaceState{
		"ens224": {AdminUp: false, LinkUp: false, LinkSpeed: 10_000_000, DevType: "vmxnet3"},
	}})

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show interfaces physical").Output
	if !strings.Contains(out, "Speed") || !strings.Contains(out, "Driver") || !strings.Contains(out, "Link") {
		t.Fatalf("表头应含 Link/Speed/Driver（契约 §1.1 明列）:\n%s", out)
	}
	row := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "ens224") {
			row = l
		}
	}
	if row == "" {
		t.Fatalf("应有 ens224 行:\n%s", out)
	}
	// 配置里 ens224 是 enabled（若取自配置则 Admin 列会是 up），VPP 里 admin/link 均 down
	fs := strings.Fields(row)
	if len(fs) < 3 || fs[1] != "down" || fs[2] != "down" {
		t.Fatalf("Admin/Link 两列都应取自 VPP 运行态（down/down），实际 %v —— 取自配置则会是 up", fs)
	}
	if !strings.Contains(row, "10G") || !strings.Contains(row, "vmxnet3") {
		t.Fatalf("速率/驱动应取自运行态: %q", row)
	}
}

// 速率格式化。
func TestFmtSpeed(t *testing.T) {
	cases := map[uint32]string{0: "-", 1000: "1M", 100_000: "100M", 10_000_000: "10G", 100: "100K"}
	for in, want := range cases {
		if got := fmtSpeed(in); got != want {
			t.Fatalf("fmtSpeed(%d) = %q，期望 %q", in, got, want)
		}
	}
}
