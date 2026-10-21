package api

// R41-2（决策 #495）：内核数据面下 `show nat` 因「NAT 会话表」不受支持而**整条命令失败**。
//
// 现场（v3 round41，内核数据面、已配 NAT）：`show nat` 先打印配置段（池/规则），随后把
// 提供者的能力哨兵（netkernel.ErrUnsupported，经 %w 包装）当成执行故障原样带 `%%` 返回——
// 脚本模式（cli.OutputFailed）按行首 `%` 判失败，`-c`/`-f` 脚本在第 1 行中止、退出码 1。
// 而**同族** `show vxlan tunnels` 是行内降级（配置字段照给、运行态给原因、退出码 0）；
// REST 侧也早已按同一判据区分：`GET /nat` 200、`GET /nat/sessions` **501**（能力不受支持
// ≠ 服务异常，见 rest_501_test.go）。
//
// 本文件把 CLI 的这一面钉住：能力哨兵 ⇒ 行内降级（配置段 + 一行说明、不带 `%`）；
// 非能力哨兵（真实执行故障）⇒ 仍 `%%` 报失败（不得因为降级把真故障吞掉）。

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/cli"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/netkernel"
)

// kernelNatKit 内核数据面 + 一份**内核侧合法**的 NAT 配置（源池 + 一条规则）。
//
// 规则的 inside/outside 必须同属一个转发域（内核侧 NAT 不区分转发域，跨域提交期就拒绝），
// 故 inside 交换机 nl3 同时持有两个三层口：内网 10.5.0.0/24 与出接口 203.0.113.1/24。
func kernelNatKit(t *testing.T) *cliExecutor {
	t.Helper()
	x, _ := newCLIKit(t)
	x.setVppCtl(fakeVppCtl{st: VppStatus{Mode: model.DataPlaneKernel}})
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set system dataplane kernel",
		"set interfaces ens5f0 description inside",
		"set interfaces ens6f0 description uplink",
		"set virtual-switches nl3 type l3",
		"set virtual-switches nl3 l3-interface ens5f0 ip address 10.5.0.1/24",
		"set virtual-switches nl3 l3-interface ens6f0 ip address 203.0.113.1/24",
		"set nat source-pool pool-a address-range 203.0.113.10 to 203.0.113.20",
		"set nat rules 10 match source 10.5.0.0/24 virtual-switch nl3 action source-pool pool-a interface ens6f0",
		"commit",
		"exit")
	return x
}

// natUnsupportedErr 与内核提供者**逐字同形**的能力哨兵（netkernel 的 unsupported()）。
func natUnsupportedErr() error {
	return fmt.Errorf("%w：%s", netkernel.ErrUnsupported, "NAT 会话表")
}

// TestCLIShowNatUnsupportedIsInlineDegradation：能力不受支持 ⇒ 配置段照给 + 一行说明、
// 命令判成功（与 `show vxlan tunnels`、REST 501 同口径）。
func TestCLIShowNatUnsupportedIsInlineDegradation(t *testing.T) {
	x := kernelNatKit(t)
	x.setNetRuntime(nil, nil, nil, &fakeNatSessions{err: natUnsupportedErr()}, nil)

	for _, cmd := range []string{"show nat", "show nat sessions"} {
		res := x.Execute("admin", aaa.ClassSuperUser, "ssh", cmd)
		out := res.Output

		// ① 不得被 CLI 的失败判据判失败（脚本模式与向导共用 cli.OutputFailed：
		// 行首单个/双个 % 与行首「校验失败」算失败）。
		if cli.OutputFailed(out) {
			t.Fatalf("%s：能力不受支持不得判失败（脚本模式会中止、退出码 1）：%q", cmd, out)
		}
		if strings.Contains(out, "%%") {
			t.Fatalf("%s：降级说明不得带 %%%% 前缀：%q", cmd, out)
		}

		// ② 配置段照常渲染（池/规则都在，与 REST GET /nat 的 200 一侧同源）。
		for _, want := range []string{
			"source-pool pool-a address-range 203.0.113.10 to 203.0.113.20",
			"rules 10 match source 10.5.0.0/24 virtual-switch nl3",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s：应照常渲染配置段 %q：%q", cmd, want, out)
			}
		}

		// ③ 一行说明要点明：运行态（会话表）不可用、原因（该能力在当前数据面不受支持）、
		// 出路（切回 VPP 数据面后可用）。原因直接取提供者哨兵原文（不加工）。
		for _, want := range []string{"运行态不可用", "不受支持", "NAT 会话表", "切回 VPP 数据面后可用"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s：降级说明应含 %q：%q", cmd, want, out)
			}
		}
		if strings.Contains(out, "无 NAT 会话") {
			t.Fatalf("%s：运行态不可用 ≠ 无会话（不得报成空表）：%q", cmd, out)
		}
	}
}

// TestCLIShowNatUnsupportedNoConfig：没有 NAT 配置时也要给可读输出（不是一行空输出）。
func TestCLIShowNatUnsupportedNoConfig(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setVppCtl(fakeVppCtl{st: VppStatus{Mode: model.DataPlaneKernel}})
	x.setNetRuntime(nil, nil, nil, &fakeNatSessions{err: natUnsupportedErr()}, nil)

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show nat")
	out := res.Output
	if cli.OutputFailed(out) || strings.Contains(out, "%%") {
		t.Fatalf("无 NAT 配置 + 能力不受支持同样不得判失败：%q", out)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("无 NAT 配置时不得只回一个空行")
	}
	for _, want := range []string{"无 NAT 配置", "运行态不可用", "NAT 会话表"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出应含 %q：%q", want, out)
		}
	}
}

// TestCLIShowNatRuntimeFaultStillFails 对照组：非能力哨兵的真实执行故障仍以 `%%` 报失败
// ——降级分支只认能力哨兵，不得把「查不到」一律吞成成功。
func TestCLIShowNatRuntimeFaultStillFails(t *testing.T) {
	x := kernelNatKit(t)
	x.setNetRuntime(nil, nil, nil, &fakeNatSessions{err: errors.New("VPP 会话读取失败")}, nil)

	for _, cmd := range []string{"show nat", "show nat sessions"} {
		res := x.Execute("admin", aaa.ClassSuperUser, "ssh", cmd)
		out := res.Output
		if !cli.OutputFailed(out) {
			t.Fatalf("%s：真实执行故障必须仍判失败：%q", cmd, out)
		}
		if !strings.Contains(out, "%% VPP 会话读取失败") {
			t.Fatalf("%s：真实执行故障应保留 %%%% + 原始原因：%q", cmd, out)
		}
		// 失败时配置段照旧先给（既有行为不变）
		if !strings.Contains(out, "source-pool pool-a") {
			t.Fatalf("%s：失败时也应先给配置段：%q", cmd, out)
		}
	}
}

// TestCLIShowNatUnsupportedNoteVppMode：同族哨兵若在 VPP 数据面下出现（不属于当前现场），
// 说明里**不**点「切回 VPP 数据面」——那会把操作者引向一个无效动作。
func TestCLIShowNatUnsupportedNoteVppMode(t *testing.T) {
	x := kernelNatKit(t)
	x.setVppCtl(fakeVppCtl{st: VppStatus{Mode: model.DataPlaneVPP}}) // 覆盖装配事实：VPP
	x.setNetRuntime(nil, nil, nil, &fakeNatSessions{err: natUnsupportedErr()}, nil)

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show nat").Output
	if cli.OutputFailed(out) {
		t.Fatalf("能力不受支持不得判失败：%q", out)
	}
	if !strings.Contains(out, "运行态不可用") {
		t.Fatalf("仍应如实报运行态不可用：%q", out)
	}
	if strings.Contains(out, "切回 VPP 数据面") {
		t.Fatalf("当前已是 VPP 数据面，不得再让操作者「切回 VPP」：%q", out)
	}
}
