package api

// 决策 #302（首装接口可见性，收口 round81 F1）：内核侧未接管的物理口进 `show interfaces`
// 读视图与单口视图——首装（VPP 未接管任何口、配置未声明任何接口）时操作者也能看见网卡，
// 且未接管口**不编造 VPP 侧事实**（状态/计数列如实为 -，statistics 明确说明无数据面统计）。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// freshInstallKit 首装现场：VPP 未接入（未接管任何口）、配置为空，内核有三块网卡。
func freshInstallKit(t *testing.T) *cliExecutor {
	t.Helper()
	x, _ := newCLIKit(t)
	x.setPorts(fakePorts{
		vppErr: errors.New("VPP 未接入"),
		kernel: []string{"ens160", "ens192", "ens224"},
	})
	return x
}

// TestShowInterfacesFreshInstallListsKernelPorts：F1 原始场景——首装 `show interfaces`
// 不再是「（无接口）」，内核网卡以行的形式出现并标注「未接管」；结构化输出同源。
func TestShowInterfacesFreshInstallListsKernelPorts(t *testing.T) {
	x := freshInstallKit(t)
	res := x.Execute("admin", "super-user", "ssh", "show interfaces")
	out := res.Output
	for _, want := range []string{"ens160", "ens192", "ens224", "未接管"} {
		if !strings.Contains(out, want) {
			t.Fatalf("首装清单应含内核口 %q 并标注「未接管」: %q", want, out)
		}
	}
	if strings.Contains(out, "（无接口）") {
		t.Fatalf("内核口在列时不得再回空态: %q", out)
	}
	if !strings.Contains(out, "Interface") || !strings.Contains(out, "备注") {
		t.Fatalf("清单应以表格行的形式作答（而非仅一句错误/说明）: %q", out)
	}
	// VPP 运行态不可用的说明照旧（诚实口径）：内核行的 - 是「不编造数据面事实」，
	// 声明/VPP 行的 - 由该行解释。
	if !strings.Contains(out, "VPP 运行态不可用") {
		t.Fatalf("应说明 VPP 运行态不可用: %q", out)
	}
	// 结构化输出与文本同源：每个未接管口一行，source 标注一致
	items, ok := x.structured.(map[string]any)["interfaces"].([]any)
	if !ok || len(items) != 3 {
		t.Fatalf("结构化输出应有 3 个接口条目: %v", x.structured)
	}
	for _, it := range items {
		e := it.(map[string]any)
		if e["source"] != "未接管" {
			t.Fatalf("未接管口的 source 应为「未接管」: %v", e)
		}
	}
}

// TestShowInterfacesPhysicalEquivalentWithKernelPorts：`show interfaces physical` 与裸写法
// 同一实现（决策 #155 口径不因 #302 改变）。
func TestShowInterfacesPhysicalEquivalentWithKernelPorts(t *testing.T) {
	x := freshInstallKit(t)
	bare := x.Execute("admin", "super-user", "ssh", "show interfaces").Output
	phys := x.Execute("admin", "super-user", "ssh", "show interfaces physical").Output
	if bare != phys {
		t.Fatalf("裸写法与 physical 应逐字相同\n  裸: %q\n  physical: %q", bare, phys)
	}
}

// TestShowInterfacesMixedSourcesLabels：已声明/已在 VPP 的口按既有口径展示，
// 不被内核口的新标注覆盖——内核口剔除由 untakenKernelIfnames 保证（见其表驱动用例），
// 这里验混合现场：声明口照旧、内核口标「未接管」。
func TestShowInterfacesMixedSourcesLabels(t *testing.T) {
	x, engine := newCLIKit(t)
	x.setPorts(fakePorts{
		vpp:    []string{"ens224"},
		kernel: []string{"ens160", "ens192"},
	})
	// 声明 ens192（内核侧存在、未接管）：按既有「已声明未生效」口径（VPP 运行态里没有）
	sess := config.Session{User: "system", Source: "console"}
	if err := engine.Edit(sess); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	cfg, _ := engine.Committed()
	cfg.Interfaces = append(cfg.Interfaces, model.InterfaceConfig{Name: "ens192"})
	if err := engine.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	if _, err := engine.Commit(context.Background(), sess, config.CommitOpts{}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	_ = engine.Release(sess)

	out := x.Execute("admin", "super-user", "ssh", "show interfaces").Output
	for _, want := range []string{"ens160", "ens192", "ens224", "未接管", "已声明未生效", "未声明"} {
		if !strings.Contains(out, want) {
			t.Fatalf("混合现场清单应含 %q: %q", want, out)
		}
	}
}

// TestShowOneInterfaceKernelView：未接管口的 detail 回内核事实视图（驱动/MAC/速率/状态取
// sysfs 事实，经注入的假源核对），注明「未被 VPP 接管」；真未知名仍报既有错误。
func TestShowOneInterfaceKernelView(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setPorts(fakePorts{
		vppErr: errors.New("VPP 未接入"),
		kernel: []string{"ens160"},
		facts: []network.KernelIfFacts{{
			Name: "ens160", AdminUp: true, LinkKnown: true, SpeedMbps: 10000,
			MAC: "00:50:56:aa:bb:cc", Driver: "vmxnet3", MTU: 1500,
		}},
	})
	out := x.Execute("admin", "super-user", "ssh", "show interfaces ens160").Output
	for _, want := range []string{
		"（接口 ens160 未被 VPP 接管，以下为内核侧视图）",
		"vmxnet3", "mac: 00:50:56:aa:bb:cc", "10G", "1500",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("内核视图应含 %q: %q", want, out)
		}
	}
	if strings.HasPrefix(out, "%") || strings.Contains(out, "\n%") {
		t.Fatalf("内核视图是正常作答，不得以错误行呈现: %q", out)
	}
	// detail 子形态与裸写法同源
	detail := x.Execute("admin", "super-user", "ssh", "show interfaces ens160 detail").Output
	if detail != out {
		t.Fatalf("detail 应与裸写法逐字相同\n  detail: %q\n  裸: %q", detail, out)
	}
	// 三侧都不在的名字仍报既有错误（判「不在清单」需要清单查询成功——决策 #154，
	// 故这里给可用的 VPP 清单而不是让它退化）
	x2, _ := newCLIKit(t)
	x2.setPorts(fakePorts{vpp: []string{"ens224"}, kernel: []string{"ens160"}})
	unk := x2.Execute("admin", "super-user", "ssh", "show interfaces nope0").Output
	if !strings.Contains(unk, "也不在 VPP 接口清单中") {
		t.Fatalf("真未知名仍应报「不在清单」: %q", unk)
	}
}

// TestShowOneInterfaceKernelStatistics：未接管口的 statistics 如实说明无数据面统计，
// 不冒充「连接未就绪」。
func TestShowOneInterfaceKernelStatistics(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setPorts(fakePorts{kernel: []string{"ens160"}})
	out := x.Execute("admin", "super-user", "ssh", "show interfaces ens160 statistics").Output
	if !strings.Contains(out, "未被 VPP 接管") || !strings.Contains(out, "无数据面统计") {
		t.Fatalf("未接管口的 statistics 应如实说明: %q", out)
	}
}
