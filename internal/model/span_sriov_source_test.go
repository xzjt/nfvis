package model

// 决策 #430①：SPAN 的镜像源为 **sriov-vf 型** vNIC 时**提交期拒绝**（两数据面一致）。
//
// 由来：vNIC 源在 VPP 侧的实现（决策 #425）落地后如实登记——sriov-vf 是 PCI 直通，
// 不经过 VPP/内核桥，作为镜像源永远收不到流量；校验层此前接受、只在下发期报错。
// 能前置判定的不留给下发期：本文件是这条口径的守护。
//   - sriov-vf 源 → 提交期拒绝，文案点名 sriov-vf 与两条替代（vhost-user 型 vNIC /
//     物理口或 bond）；
//   - vhost-user 型 vNIC 源 → 放行（实现路径不变）；
//   - 物理口源 → 放行。

import (
	"strings"
	"testing"
)

// spanSriovVnicConfig 一份最小可提交配置：一个声明了 **sriov-vf 型 vNIC** 的 VNF
// （PF 为 ens192）+ 一个物理分析口（ens224）+ 一条源为该 vNIC 的镜像会话。
func spanSriovVnicConfig(dataplane string) Config {
	c := Config{
		Interfaces: []InterfaceConfig{{Name: "ens192"}, {Name: "ens224"}},
		VirtualMachineFunctions: []VMFunction{{
			Name: "fw-vm", Image: "base.qcow2",
			VCPU:   VMCpu{Count: 1},
			Memory: VMMemory{SizeMB: 512},
			Interfaces: []VnfInterface{{
				Name: "eth1", Type: "sriov-vf",
				Sriov: &SriovBind{PhysicalInterface: "ens192", VFID: 0},
			}},
		}},
		PortMirroring: []PortMirroring{{
			Name:     "span1",
			Source:   PMSource{Vnf: "fw-vm", VnfInterface: "eth1", Direction: "both"},
			Analyzer: "ens224",
		}},
	}
	if dataplane != "" {
		c.System = &SystemConfig{DataPlane: dataplane}
	}
	return c
}

// 决策 #430①：镜像源为 **sriov-vf 型** vNIC 时**提交期拒绝**（两数据面一致）——PCI 直通
// 的 vNIC 不经过数据面交换机，作为镜像源永远收不到流量；能前置判定的不留给下发期。
// 文案点名 sriov-vf 与两条替代（vhost-user 型 vNIC / 物理口或 bond）。
func TestSpanSriovVfSourceRejected(t *testing.T) {
	for _, dp := range []string{"", DataPlaneVPP, DataPlaneKernel} {
		errs := Validate(spanSriovVnicConfig(dp))
		if len(errs) == 0 {
			t.Fatalf("dataplane=%q：sriov-vf 源应提交期拒绝", dp)
		}
		got := errText(errs)
		for _, want := range []string{"port-mirroring[span1].source", "sriov-vf", "vhost-user", "物理口/bond"} {
			if !strings.Contains(got, want) {
				t.Fatalf("dataplane=%q：拒绝文案应含 %q（可照做），实得：\n%s", dp, want, got)
			}
		}
	}
}

// 决策 #430①：源类型不是 sriov-vf 时不受影响——vhost-user 型 vNIC 源与物理口源均放行
// （同一份配置只改源类型/形态）。
func TestSpanSriovVfSourceAlternativesAllowed(t *testing.T) {
	// vhost-user 型 vNIC 源（VPP 数据面）放行
	if errs := Validate(spanVnicConfig(DataPlaneVPP)); len(errs) != 0 {
		t.Fatalf("vhost-user 源应放行，实得：\n%s", errText(errs))
	}
	// 物理口源放行（VPP 数据面）
	c := spanVnicConfig(DataPlaneVPP)
	c.PortMirroring[0].Source = PMSource{Interface: "ens224", Direction: "both"}
	c.Interfaces = append(c.Interfaces, InterfaceConfig{Name: "ens192"})
	c.PortMirroring[0].Analyzer = "ens192"
	if errs := Validate(c); len(errs) != 0 {
		t.Fatalf("物理口源应放行，实得：\n%s", errText(errs))
	}
}
