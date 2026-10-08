package api

// R2-18：内核数据面下各条 show/request 的文案必须点名真实数据面（Linux 内核网络），
// 不再把「VPP 未接入（编排器未装配）」「在 VPP 中不存在」「无 VPP 运行态」这类 VPP 话术
// 套在内核现场——那会把操作者引向查 VPP 装配/连接这条根本不存在的路径。
//
// 数据面按**装配事实**注入（fakeVppCtl 自报 Mode=kernel，committed 保持缺省 vpp），
// 顺带覆盖 R2-20 的「装配优先」口径。VPP 侧文案的逐字不变由既有用例守住
// （cli_kernel_iface_show_test.go、cli_diag_test.go、cli_compute_test.go 等）。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
	"github.com/xzjt/nfvis/internal/state"
)

// kernelDPKit 装配为内核数据面（VppController 如实自报 kernel）的 CLI 现场。
func kernelDPKit(t *testing.T) *cliExecutor {
	t.Helper()
	x, _ := newCLIKit(t)
	x.setVppCtl(fakeVppCtl{st: VppStatus{
		Mode:      model.DataPlaneKernel,
		LastError: "当前数据面为 Linux 内核网络，未使用 VPP；如需 VPP 请改回数据面并重启服务",
	}})
	x.setRuntime(nil, state.New(nil))
	return x
}

// 删除成功文案：内核数据面下不把「VPP 端口级联清理」说成发生过的事。
func TestCLIDeleteKernelDataplaneText(t *testing.T) {
	// VNF：内核下快照随域定义删除、vNIC 的宿主 tap 由 libvirt 随域回收
	x := kernelDPKit(t)
	seedVMConfig(t, x)
	out := x.Execute("admin", aaaClassSU, "ssh", "request virtual-machine-functions fw-vm delete --yes").Output
	if strings.Contains(out, "%%") {
		t.Fatalf("内核数据面下删除 VNF 应成功：%s", out)
	}
	for _, want := range []string{"当前数据面为 Linux 内核网络", "libvirt 随域回收", "无 VPP 端口需清理"} {
		if !strings.Contains(out, want) {
			t.Fatalf("删除文案应含 %q：%s", want, out)
		}
	}
	if strings.Contains(out, "运行态 vNIC/VPP 端口/快照级联清理") {
		t.Fatalf("内核数据面下不得沿用 VPP 的级联清理文案：%s", out)
	}

	// 容器：内核数据面下容器 vNIC 的 memif 接入提交期即被拒绝，现场无 memif/VPP 端口可清
	x2 := kernelDPKit(t)
	seedContainerConfig(t, x2)
	out2 := x2.Execute("admin", aaaClassSU, "ssh", "request container-functions sbc-ct1 delete --yes").Output
	if strings.Contains(out2, "%%") {
		t.Fatalf("内核数据面下删除容器应成功：%s", out2)
	}
	for _, want := range []string{"当前数据面为 Linux 内核网络", "memif 接入不受支持", "无 VPP 端口需清理"} {
		if !strings.Contains(out2, want) {
			t.Fatalf("容器删除文案应含 %q：%s", want, out2)
		}
	}
	if strings.Contains(out2, "运行态 memif/VPP 端口级联清理") {
		t.Fatalf("内核数据面下不得沿用 memif/VPP 级联清理文案：%s", out2)
	}
}

// ping / clear 的「诊断运行时未接入」兜底文案：内核下点名数据面，VPP 下逐字不变。
func TestCLIDiagWiringGapKernelDataplaneText(t *testing.T) {
	x := kernelDPKit(t) // setRuntime(nil, …)：x.diag == nil，命中兜底分支
	ping := x.Execute("admin", aaaClassSU, "ssh", "ping 192.0.2.1").Output
	if !strings.Contains(ping, "当前数据面为 Linux 内核网络") || !strings.Contains(ping, "宿主网络栈") {
		t.Fatalf("内核下 ping 兜底应点名数据面：%s", ping)
	}
	if strings.Contains(ping, "VPP 未接入") {
		t.Fatalf("内核下 ping 兜底不得写「VPP 未接入」：%s", ping)
	}
	clr := x.Execute("admin", aaaClassSU, "ssh", "clear interfaces statistics").Output
	if !strings.Contains(clr, "当前数据面为 Linux 内核网络") {
		t.Fatalf("内核下 clear 兜底应点名数据面：%s", clr)
	}
	if strings.Contains(clr, "VPP 未接入") {
		t.Fatalf("内核下 clear 兜底不得写「VPP 未接入」：%s", clr)
	}

	// VPP 数据面（缺省 committed）：两条兜底文案逐字不变
	x2, _ := newCLIKit(t)
	if got := x2.Execute("admin", aaaClassSU, "ssh", "ping 192.0.2.1").Output; got != "%% ping 不可用（VPP 未接入）\n" {
		t.Fatalf("VPP 侧 ping 兜底文案应逐字不变，得到 %q", got)
	}
	if got := x2.Execute("admin", aaaClassSU, "ssh", "clear interfaces statistics").Output; got != "%% clear 不可用（VPP 未接入）\n" {
		t.Fatalf("VPP 侧 clear 兜底文案应逐字不变，得到 %q", got)
	}
}

// show vpp 概览 / threads / memory：点名数据面，不给 threads: 0 这类假读数。
func TestCLIShowVppKernelDataplaneHonest(t *testing.T) {
	x := kernelDPKit(t)

	out := x.Execute("admin", aaaClassSU, "ssh", "show vpp").Output
	for _, want := range []string{
		"dataplane: kernel",
		"threads: 不适用（当前数据面为 Linux 内核网络，无 VPP 运行态读数）",
		"memory: 运行态不可用（当前数据面为 Linux 内核网络，无 VPP 运行态读数）",
		"buffers: 运行态不可用（当前数据面为 Linux 内核网络，无 VPP 运行态读数）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("内核数据面下 show vpp 应含 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "threads: 0") {
		t.Fatalf("内核数据面下 threads: 0 是假读数：\n%s", out)
	}

	th := x.Execute("admin", aaaClassSU, "ssh", "show vpp threads").Output
	if !strings.Contains(th, "当前数据面为 Linux 内核网络") {
		t.Fatalf("show vpp threads 应点名数据面（不是「（无线程运行态）」）：%s", th)
	}
	mem := x.Execute("admin", aaaClassSU, "ssh", "show vpp memory").Output
	if !strings.Contains(mem, "当前数据面为 Linux 内核网络") {
		t.Fatalf("show vpp memory 应点名数据面：%s", mem)
	}
}

// 抓包三形态（show vpp capture / request vpp trace start|export）在内核数据面下
// 不再报「VPP 未接入（编排器未装配）」，而是点名数据面与替代路径。
func TestCLICaptureKernelDataplaneText(t *testing.T) {
	x := kernelDPKit(t)
	for _, line := range []string{
		"show vpp capture",
		"request vpp trace start interface ens192",
		"request vpp trace export",
		"request vpp restart",
	} {
		got := x.Execute("admin", aaaClassSU, "ssh", line).Output
		if !strings.Contains(got, "Linux 内核网络") {
			t.Fatalf("%s 在内核数据面下应点名数据面：%s", line, got)
		}
		if strings.Contains(got, "编排器未装配") {
			t.Fatalf("%s 内核数据面下不得再报「编排器未装配」：%s", line, got)
		}
	}
}

// 接口 statistics / 未知名清单：内核数据面下点名内核网络，VPP 措辞只在 VPP 下出现。
func TestCLIIfaceKernelDataplaneText(t *testing.T) {
	x := kernelDPKit(t)
	x.setPorts(fakePorts{
		vpp:    []string{"ens224"},
		kernel: []string{"ens160"},
		facts: []network.KernelIfFacts{{
			Name: "ens160", AdminUp: true, LinkKnown: true, SpeedMbps: 10000, Driver: "vmxnet3", MTU: 1500,
		}},
	})

	// 未接管内核口的 statistics：点明「当前数据面为 Linux 内核网络」+ 无数据面统计
	out := x.Execute("admin", aaaClassSU, "ssh", "show interfaces ens160 statistics").Output
	if !strings.Contains(out, "Linux 内核网络") || !strings.Contains(out, "无数据面统计") {
		t.Fatalf("内核口 statistics 应点名数据面并说明无数据面统计：%s", out)
	}
	if strings.Contains(out, "未被 VPP 接管") {
		t.Fatalf("内核数据面下不得说「未被 VPP 接管」：%s", out)
	}

	// 三侧都不在的名字：报「不在 Linux 内核网络接口清单中」
	unk := x.Execute("admin", aaaClassSU, "ssh", "show interfaces nope0").Output
	if !strings.Contains(unk, "不在 Linux 内核网络接口清单中") {
		t.Fatalf("未知接口应点名数据面清单：%s", unk)
	}

	// monitor interfaces：同口径点名数据面
	mon := x.Execute("admin", aaaClassSU, "ssh", "monitor interfaces ens160").Output
	if !strings.Contains(mon, "当前数据面为 Linux 内核网络") {
		t.Fatalf("monitor 统计暂不可用应点名数据面：%s", mon)
	}
}

// 虚拟交换机：空态与「不存在」按数据面措辞。
func TestCLIVSwitchKernelDataplaneText(t *testing.T) {
	x := kernelDPKit(t)

	// 运行态空表：不说「VPP 中无 bridge-domain」
	x.setVppState(fakeVppState{})
	out := x.Execute("admin", aaaClassSU, "ssh", "show virtual-switches").Output
	if !strings.Contains(out, "Linux 内核网络中无虚拟交换机") {
		t.Fatalf("内核空态应点名数据面：%s", out)
	}

	// 有运行态但问的名字不存在：报「在 Linux 内核网络中不存在」（列表/detail 与 ports 同口径）
	x2 := kernelDPKit(t)
	x2.setVppState(fakeVppState{bds: []BridgeDomainState{{ID: 7, Name: "vs-rt", Learn: true, Flood: true}}})
	for _, line := range []string{"show virtual-switches ghost", "show virtual-switches ghost ports"} {
		got := x2.Execute("admin", aaaClassSU, "ssh", line).Output
		if !strings.Contains(got, "在 Linux 内核网络中不存在") {
			t.Fatalf("%s 应点名数据面：%s", line, got)
		}
	}
}

// VNF 的 vNIC：内核数据面下 vhost-user 声明由 libvirt 的 vhost-net tap 承载，
// 不显示数据面里并不存在的 vhost-user socket；统计也如实说明无该读数。
func TestCLIVMKernelDataplaneText(t *testing.T) {
	x := kernelDPKit(t)
	seedVMConfig(t, x)

	out := x.Execute("admin", aaaClassSU, "ssh", "show virtual-machine-functions fw-vm interfaces").Output
	if !strings.Contains(out, "libvirt 自建 vhost-net tap") || !strings.Contains(out, "无 vhost-user socket") {
		t.Fatalf("内核数据面下 vNIC 列应如实说明承载形态：%s", out)
	}
	if strings.Contains(out, "/vhost/") {
		t.Fatalf("内核数据面下不得给出 vhost-user socket 路径：%s", out)
	}

	stat := x.Execute("admin", aaaClassSU, "ssh", "show virtual-machine-functions fw-vm statistics").Output
	if !strings.Contains(stat, "当前数据面为 Linux 内核网络") || strings.Contains(stat, "vhost-user 口未在 VPP 找到计数") {
		t.Fatalf("内核数据面下 vNIC 统计应点名数据面、不套 VPP 话术：%s", stat)
	}
}
