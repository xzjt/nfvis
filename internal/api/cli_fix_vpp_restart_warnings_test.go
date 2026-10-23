package api

// 修 BUG-15（v3.0.25 复测报告）：`request vpp restart` 成功 ≠ 所有声明口都进了数据面。
//
// 背景：决策 #496 把「某个 `vpp dpdk dev` 解析不出 PCI」从**整份失败**改成**跳过该条目**，
// 生成期告警由 `Applier.Warnings()` 留出读口——但那个读口**没有任何调用者**，事实只落
// journal。操作者跑完重启看到「成功」就以为口都在，正是那条修复本要避免的「静默丢口」。
//
// 本用例钉住：有生成期告警时**必须上屏**（点名是哪个口、怎么救），无告警时成功文案逐字不变。

import (
	"context"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

// 有生成期告警：成功文案之后必须逐条列出，且带照做路径。
func TestCLIRequestVPPRestartSurfacesGenerationWarnings(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setVPPRestart(func(context.Context) error { return nil })
	x.setVPPRestartWarnings(func() []string {
		return []string{
			`配置中的 vpp dpdk dev ens224 未能解析 PCI 地址（读取 /sys/class/net/ens224/device/uevent: ` +
				`no such file or directory），已从 startup.conf 中跳过：该口不会出现在数据面。` +
				`如仍需给 VPP 用，请重新接管后重启数据面（request interfaces ens224 bind-dpdk --yes ` +
				`然后 request vpp restart）；否则删除该声明（delete vpp dpdk dev ens224）以免每次生成都跳过。`,
		}
	})

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request vpp restart").Output
	if !strings.Contains(out, "生成期告警") {
		t.Fatalf("有跳过口时必须上屏提示（只落日志＝静默丢口），实际输出:\n%s", out)
	}
	for _, want := range []string{"ens224", "跳过", "bind-dpdk", "delete vpp dpdk dev"} {
		if !strings.Contains(out, want) {
			t.Fatalf("告警文案应含 %q（点名是哪个口 + 怎么救），实际输出:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "binary API 已连通") {
		t.Fatalf("仍应如实说明重启本身成功，实际输出:\n%s", out)
	}
}

// 无生成期告警：成功文案必须与修复前**逐字一致**（不给正常路径加噪）。
func TestCLIRequestVPPRestartCleanPathUnchanged(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setVPPRestart(func(context.Context) error { return nil })
	x.setVPPRestartWarnings(func() []string { return nil })

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request vpp restart").Output
	if strings.Contains(out, "生成期告警") {
		t.Fatalf("无告警时不得出现告警块，实际输出:\n%s", out)
	}
	if !strings.Contains(out, "已按 committed 配置重启 VPP（binary API 已连通）并触发恢复收敛。") {
		t.Fatalf("无告警时成功文案应逐字不变，实际输出:\n%s", out)
	}
}

// 未注入读口（内核数据面/未装配 Applier）：不报、不 panic（可选接口语义）。
func TestCLIRequestVPPRestartWithoutWarningReader(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setVPPRestart(func(context.Context) error { return nil })

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request vpp restart").Output
	if strings.Contains(out, "生成期告警") {
		t.Fatalf("未注入读口时不得凭空报告警，实际输出:\n%s", out)
	}
	if !strings.Contains(out, "binary API 已连通") {
		t.Fatalf("应正常报成功，实际输出:\n%s", out)
	}
}
