package network

// 一个解析不出 PCI 的 `vpp dpdk dev` 条目不得让整份 startup.conf 生成失败（真机 v3.0.24）。
//
// 现场：口被交还内核驱动后仍留在 committed 配置里，此后每次 `request vpp restart` 都在生成阶段
// 报 `解析 "ens224" 的 PCI 地址失败: ... no such file or directory` —— 连同其它配置正常的口一起
// 起不来，数据面整段停摆；产品内唯一出路是删掉那一条声明或重新接管。
// 口径：跳过**那一条**，其余 `dev` 块照常产出，且跳过必须被如实、点名地说出来（不许静默消失）。

import (
	"context"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// fixedPCIResolver ens192 可解析、ens224 解析失败（模拟已交还内核的口）。
func fixedPCIResolver(ifname string) (string, error) {
	switch ifname {
	case "ens192":
		return "0000:03:00.0", nil
	case "ens224":
		return "", errNotFound("ens224")
	}
	return "", errNotFound(ifname)
}

// dpdkPerDevConfig 两个单网卡覆盖（第一个可解析、第二个解析失败）。
func dpdkPerDevConfig() *model.Config {
	return &model.Config{Vpp: &model.VppConfig{
		DPDK: &model.VppDPDK{PerDev: []model.VppDevOverride{
			{Interface: "ens192", RxQueues: 4},
			{Interface: "ens224", TxQueues: 4},
		}},
	}}
}

// 第二个条目解析失败：不报错、第一个 dev 块在、第二个不在、告警点名被跳过的口。
func TestStartupSkipsUnresolvedDPDKDev(t *testing.T) {
	out, err := GenerateStartup(dpdkPerDevConfig(), fixedPCIResolver)
	if err != nil {
		t.Fatalf("单个条目解析失败不应让整份生成失败（其余口会一起起不来）: %v", err)
	}
	if !strings.Contains(out, "dev 0000:03:00.0 {") || !strings.Contains(out, "name ens192") {
		t.Fatalf("可解析的口必须照常产出 dev 块:\n%s", out)
	}
	if strings.Contains(out, "ens224") {
		t.Fatalf("解析失败的口不应出现在 dev 段（内核里没有它，VPP 会启动失败）:\n%s", out)
	}
}

// 全部可解析：告警为空，dpdk 段逐字节等于既有产出（回归）。
func TestStartupAllDPDKDevResolvedUnchanged(t *testing.T) {
	cfg := dpdkPerDevConfig()
	cfg.Vpp.DPDK.PerDev = cfg.Vpp.DPDK.PerDev[:1]
	all := func(string) (string, error) { return "0000:03:00.0", nil }

	withWarn, warnings, err := GenerateStartupWithWarnings(cfg, all)
	if err != nil {
		t.Fatalf("GenerateStartupWithWarnings: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("全部可解析时不应有告警: %v", warnings)
	}
	// 金标准独立书写（不从实现反推）：dpdk 段只应有这一个 dev 块，缩进与键序都不得变。
	const wantDPDK = "dpdk {\n" +
		"  dev 0000:03:00.0 {\n" +
		"    name ens192\n" +
		"    num-rx-queues 4\n" +
		"  }\n" +
		"}\n\n"
	if got := dpdkSection(t, withWarn); got != wantDPDK {
		t.Fatalf("dpdk 段应与既有产出逐字节一致:\n--- want ---\n%q\n--- got ---\n%q", wantDPDK, got)
	}
	// 既有入口与新的带告警入口必须给出同一份文本。
	plain, err := GenerateStartup(cfg, all)
	if err != nil {
		t.Fatalf("GenerateStartup: %v", err)
	}
	if plain != withWarn {
		t.Fatalf("GenerateStartup 与 GenerateStartupWithWarnings 文本不一致:\n%s\n---\n%s", plain, withWarn)
	}
}

// dpdkSection 截出 dpdk 段（含段尾空行），用于逐字节比对。
func dpdkSection(t *testing.T, conf string) string {
	t.Helper()
	i := strings.Index(conf, "dpdk {\n")
	if i < 0 {
		t.Fatalf("生成文本没有 dpdk 段:\n%s", conf)
	}
	rest := conf[i:]
	j := strings.Index(rest, "\n}\n\n")
	if j < 0 {
		t.Fatalf("dpdk 段未闭合:\n%s", conf)
	}
	return rest[:j+len("\n}\n\n")]
}

// 无解析器 + 存在单网卡覆盖：仍是错误（既有契约不变）。
func TestStartupUnresolvedDPDKDevWithoutResolverStillErrors(t *testing.T) {
	if _, err := GenerateStartup(dpdkPerDevConfig(), nil); err == nil || !strings.Contains(err.Error(), "PCI") {
		t.Fatalf("无解析器时应报错且点名 PCI: %v", err)
	}
	_, _, err := GenerateStartupWithWarnings(dpdkPerDevConfig(), nil)
	if err == nil || !strings.Contains(err.Error(), "PCI") {
		t.Fatalf("WithWarnings 的无解析器契约应与 GenerateStartup 一致: %v", err)
	}
}

// Applier.Apply 的混合现场：startup.conf 照写、可解析的口在内、Warn 回调收到点名告警。
func TestApplierApplyKeepsGoingWhenOneDPDKDevUnresolved(t *testing.T) {
	var written string
	var warns []string
	applier := &Applier{
		Path: "/tmp/startup.conf",
		PCI:  fixedPCIResolver,
		Write: func(path string, data []byte) error {
			written = string(data)
			return nil
		},
		Warn:           func(msg string) { warns = append(warns, msg) },
		Restarter:      restarterFunc(func(context.Context) error { return nil }),
		RestartOnApply: false,
	}
	cfg := dpdkPerDevConfig()
	if _, err := applier.Apply(context.Background(), cfg); err != nil {
		t.Fatalf("一个口解析失败不应中止落地（否则数据面整体停摆）: %v", err)
	}
	if !strings.Contains(written, "dev 0000:03:00.0 {") || strings.Contains(written, "ens224") {
		t.Fatalf("startup.conf 应含可解析的口、不含解析失败的口:\n%s", written)
	}
	if len(warns) == 0 || !strings.Contains(strings.Join(warns, "\n"), "ens224") {
		t.Fatalf("Warn 回调必须点名被跳过的口: %v", warns)
	}
	got := strings.Join(applier.Warnings(), "\n")
	if !strings.Contains(got, "ens224") || !strings.Contains(got, "bind-dpdk") {
		t.Fatalf("Applier 应保留最近一次被跳过条目的说明（供 CLI/REST 取用）: %q", got)
	}
	if !strings.Contains(got, "delete vpp dpdk dev ens224") {
		t.Fatalf("说明必须给出可照做的出路（重新接管或删声明）: %q", got)
	}
}
