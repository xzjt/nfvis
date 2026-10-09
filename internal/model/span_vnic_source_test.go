package model

import (
	"strings"
	"testing"
)

// 决策 #425：SPAN 的 vNIC 源——VPP 数据面**放行**（下发给编排层按确定性 vhost-user 口名
// 解析后镜像），内核数据面**提交期拒绝**（宿主 tap 名不可靠，落在 model 校验层，不落下发期）。
//
// 本文件是这两条口径的守护：
//   - VPP（缺省/显式）下，源为**已声明**的 VNF vNIC 必须能通过校验（否则又会退回
//     「提交期放行/拒绝」两处漂移的老问题——round3 走查 R3-4 的根因就是三处口径不齐）。
//   - 未声明的 vNIC 仍被拒（不把不存在的口放进去）。
//   - 内核数据面下同一份配置在**提交期**即被拒，文案点名替代（物理口/bond）与切换路径。

// spanVnicConfig 一份最小可提交配置：一个已声明 vhost-user vNIC 的 VNF + 一个物理口
// （分析口）+ 一条源为该 vNIC 的镜像会话。dataplane 由调用方给。
func spanVnicConfig(dataplane string) Config {
	c := Config{
		Interfaces: []InterfaceConfig{{Name: "ens224"}},
		VirtualMachineFunctions: []VMFunction{{
			Name: "fw-vm", Image: "base.qcow2",
			VCPU:   VMCpu{Count: 1},
			Memory: VMMemory{SizeMB: 512},
			Interfaces: []VnfInterface{
				{Name: "eth0", Type: "vhost-user"},
			},
		}},
		PortMirroring: []PortMirroring{{
			Name:     "span1",
			Source:   PMSource{Vnf: "fw-vm", VnfInterface: "eth0", Direction: "both"},
			Analyzer: "ens224",
		}},
	}
	if dataplane != "" {
		c.System = &SystemConfig{DataPlane: dataplane}
	}
	return c
}

// VPP 数据面（缺省与显式）下，源为已声明的 VNF vNIC 必须放行——实现落在编排层
// （internal/orchestrator/network 的 ApplySpan 解析 vhost 口名后走既有 span 原语）。
func TestSpanVnicSourceAllowedOnVPP(t *testing.T) {
	for _, dp := range []string{"", DataPlaneVPP} {
		if errs := Validate(spanVnicConfig(dp)); len(errs) != 0 {
			t.Fatalf("dataplane=%q：VPP 数据面应放行 vNIC 源，实得：\n%s", dp, errText(errs))
		}
	}
}

// 未声明的 vNIC（或 vNIC 名写错）仍在提交期被拒：不把解析不出来的口放进数据面。
func TestSpanVnicSourceUnknownVnicRejected(t *testing.T) {
	c := spanVnicConfig(DataPlaneVPP)
	c.PortMirroring[0].Source = PMSource{Vnf: "fw-vm", VnfInterface: "eth1", Direction: "both"}
	errs := Validate(c)
	if len(errs) == 0 {
		t.Fatal("未声明的 vNIC 作镜像源应被拒绝")
	}
	if got := errText(errs); !strings.Contains(got, "port-mirroring[span1].source") || !strings.Contains(got, "不存在") {
		t.Fatalf("应指向镜像源并说明 vNIC 不存在，实得：\n%s", got)
	}
}

// 源为空/两者都写仍被拒（kinds != 1），不受本次改动影响。
func TestSpanSourceMustBeExactlyOneKind(t *testing.T) {
	for name, src := range map[string]PMSource{
		"都空":   {Direction: "both"},
		"两者都写": {Interface: "ens224", Vnf: "fw-vm", VnfInterface: "eth0", Direction: "both"},
	} {
		c := spanVnicConfig(DataPlaneVPP)
		c.PortMirroring[0].Source = src
		if errs := Validate(c); len(errs) == 0 {
			t.Fatalf("%s：应被拒绝", name)
		}
	}
}

// 内核数据面：源为 VNF vNIC **提交期拒绝**（不落下发期），文案给替代与切换路径。
func TestSpanVnicSourceRejectedOnKernelDataPlane(t *testing.T) {
	errs := Validate(spanVnicConfig(DataPlaneKernel))
	if len(errs) == 0 {
		t.Fatal("内核数据面下 vNIC 源应提交期拒绝")
	}
	got := errText(errs)
	for _, want := range []string{"port_mirroring[span1]", "VNF 虚拟网卡", "物理口或 bond", "set system dataplane vpp"} {
		if !strings.Contains(got, want) {
			t.Fatalf("内核侧拒绝文案应含 %q（可照做），实得：\n%s", want, got)
		}
	}
	// 物理口源在内核数据面下仍合法（同一份配置只改源）——拒绝的是 vNIC 形态，不是镜像本身。
	c := spanVnicConfig(DataPlaneKernel)
	c.PortMirroring[0].Source = PMSource{Interface: "ens224", Direction: "both"}
	c.Interfaces = append(c.Interfaces, InterfaceConfig{Name: "ens192"})
	c.PortMirroring[0].Analyzer = "ens192"
	if errs := Validate(c); len(errs) != 0 {
		t.Fatalf("内核数据面下物理口源应被接受，实得：\n%s", errText(errs))
	}
}
