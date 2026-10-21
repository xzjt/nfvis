package model

// 决策 #495（收口 R41-3）：内核数据面下 VNF 派生端口**不再编造 VPP 逻辑名**。
//
// 由来（round41 真机）：内核数据面下 VNF 的 vNIC 由 libvirt 以 `interface type='bridge'` +
// 宿主 tap `vnetN` 承载，内核里根本没有 `vh-<vm>-<nic>` 设备；而 AttachedPortName 的 VNF
// 分支不看数据面，于是端口表同时出现 `vh-r41v-nic0`（source=vnf，按名 `ip link` 找不到）
// 与 `vnet0`（source=runtime，真实存在）两条并列——「表里有、设备没有」。
//
// 口径：与容器分支（决策 #442 修 R5-1）同路数——按数据面如实给名。内核侧**没有产品侧可
// 确定**的设备名（`vnetN` 由 libvirt 分配且随域重建变化，产品无可靠映射：见
// internal/orchestrator/netkernel/api_surface.go 的 CheckVnfPorts 注释），故返回空串，由调用方
// 退回 `<vm>/<nic>` 语义名（AttachedPortName 注释里既有的约定）。VPP 侧逐字不变。
//
// 红-绿：未改实现前，本文件的两条内核断言都拿到 `vh-…`/`mf-…`（VPP 逻辑名），消息里即写明。

import (
	"strings"
	"testing"
)

// vnfNameConfig 同一个 VNF 配置在两个数据面下的对照（dataPlane 为空 = 未配置，回落 vpp）。
func vnfNameConfig(dataPlane string) Config {
	return Config{
		System:          &SystemConfig{DataPlane: dataPlane},
		VirtualSwitches: []VirtualSwitch{{Name: "vs-a", Type: "l2"}},
		VirtualMachineFunctions: []VMFunction{{Name: "fw-vm", Image: "alpine.qcow2",
			Interfaces: []VnfInterface{{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-a"}}}},
	}
}

// AttachedPortName 的 VNF 分支按数据面给名：VPP（含缺省）⇒ vh-<vm>-<nic>；内核 ⇒ 空串。
func TestAttachedPortNameVNFKernelReturnsEmpty(t *testing.T) {
	if got := AttachedPortName(vnfNameConfig(DataPlaneVPP), PortSourceVNF, "fw-vm", "eth0"); got != "vh-fw-vm-eth0" {
		t.Fatalf("VPP 数据面下 VNF vNIC 名应逐字不变（vh-fw-vm-eth0），实得 %q", got)
	}
	// 未配置数据面 = 回落 vpp（#404 的缺省口径），同样保持 vh- 名。
	if got := AttachedPortName(vnfNameConfig(""), PortSourceVNF, "fw-vm", "eth0"); got != "vh-fw-vm-eth0" {
		t.Fatalf("未配置数据面应回落 VPP 口径（vh-fw-vm-eth0），实得 %q", got)
	}
	if got := AttachedPortName(vnfNameConfig(DataPlaneKernel), PortSourceVNF, "fw-vm", "eth0"); got != "" {
		t.Fatalf("内核数据面下 VNF vNIC 没有产品侧可确定的设备名，应返回空串（调用方退回 <vm>/<nic>），实得 %q", got)
	}
}

// 内核下 VNF 的 **memif 型** vNIC：提交期就被 checkKernelDataPlane 拒绝（"memif 接入尚未实现"，
// 见 validate.go），故本函数对它只是**防御性**分支——历史配置/绕过校验的输入同样给空串，不给出
// `mf-…` 这个内核里不存在的名字。第三个断言钉住这条前提，防止「内核哪天支持 memif 了」时本
// 用例的语义被悄悄改变。
func TestAttachedPortNameVNFMemifKernelReturnsEmpty(t *testing.T) {
	withMemif := func(mode string) Config {
		c := vnfNameConfig(mode)
		c.VirtualMachineFunctions[0].Interfaces[0].Type = "memif"
		return c
	}
	if got := AttachedPortName(withMemif(DataPlaneVPP), PortSourceVNF, "fw-vm", "eth0"); got != "mf-fw-vm-eth0" {
		t.Fatalf("VPP 侧 VNF memif 名应逐字不变（mf-fw-vm-eth0），实得 %q", got)
	}
	if got := AttachedPortName(withMemif(DataPlaneKernel), PortSourceVNF, "fw-vm", "eth0"); got != "" {
		t.Fatalf("内核数据面下 memif 型 vNIC 同样不存在，应返回空串，实得 %q", got)
	}
	rejected := false
	for _, e := range Validate(withMemif(DataPlaneKernel)) {
		if strings.Contains(e.Message, "memif") {
			rejected = true
			break
		}
	}
	if !rejected {
		t.Fatal("前提不成立：内核数据面下 VNF 的 memif vNIC 应在提交期被拒绝（checkKernelDataPlane）")
	}
}

// 派生读视图：内核下 VNF 条目的 Port 为空（身份字段保留，调用方据此退回 <vm>/<nic>）；
// VPP 下 Port 为 vh-…（逐字不变）。
func TestDerivedSwitchPortsKernelVNFPortEmpty(t *testing.T) {
	vpp := DerivedSwitchPorts(vnfNameConfig(DataPlaneVPP), "vs-a")
	if len(vpp) != 1 || vpp[0].Source != PortSourceVNF || vpp[0].Port != "vh-fw-vm-eth0" {
		t.Fatalf("VPP 侧派生条目应为 1 条且 Port=vh-fw-vm-eth0，实得 %+v", vpp)
	}

	krn := DerivedSwitchPorts(vnfNameConfig(DataPlaneKernel), "vs-a")
	if len(krn) != 1 || krn[0].Source != PortSourceVNF {
		t.Fatalf("内核侧应派生 1 条 VNF 成员，实得 %+v", krn)
	}
	if krn[0].Port != "" {
		t.Fatalf("内核侧派生条目的 Port 应为空（不编造 VPP 逻辑名），实得 %q", krn[0].Port)
	}
	if krn[0].VNF != "fw-vm" || krn[0].VNFInterface != "eth0" {
		t.Fatalf("身份字段应保留（调用方据此退回 <vm>/<nic>）：%+v", krn[0])
	}
}
