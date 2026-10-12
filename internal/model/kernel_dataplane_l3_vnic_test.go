package model

// 内核数据面下 l3-interface 指向 **vNIC 派生名**的提交期拒绝（决策 #469；收口 round24 登记的
// R24-C，与 R13-C 同族缺口）。
//
// 由来：`vh-<vm>-<nic>` 是 #172 文档化的 NAT-inside 形态，VPP 侧完全受支持（见
// l3_vnic_test.go）；而内核侧根本没有该设备名。内核校验原有的短名比对（`vnics[li.Interface]`，
// nic0/nic1）对文档化的**派生名**写法永不命中——提交期放行后，内核侧恢复收敛如实报
// IFACE_MISSING/UNCONVERGED 告警，且带该 vNIC 的 VM 在域组装时失败（start 必失败）。
//
// 红-绿：修复前内核侧 vh-/mf-/哈希形态三类派生名均零报错（第一个用例的三条子用例失败）；
// VPP 侧同配置放行、以及「无可归因声明的 vh- 名走既有存在性检查」为边界守护。

import (
	"strings"
	"testing"
)

// derivedL3Config 构造最小配置：声明 VM vm1/nic0（vhost-user）与容器 ct1/nic0（memif），
// 并把 iface 作为三层接口挂进 VRF vs-nat。mode 显式给 vpp|kernel（空值不算 kernel，
// 且本文件的用例都要两态对照）。
func derivedL3Config(mode, iface string) Config {
	return Config{
		System: &SystemConfig{DataPlane: mode},
		VirtualMachineFunctions: []VMFunction{{
			Name: "vm1", Image: "alpine.qcow2",
			VCPU: VMCpu{Count: 1}, Memory: VMMemory{SizeMB: 512},
			Interfaces: []VnfInterface{{Name: "nic0", Type: "vhost-user"}},
		}},
		ContainerFunctions: []ContainerFunction{{
			Name: "ct1", Image: "alpine:3.20",
			Interfaces: []VnfInterface{{Name: "nic0", Type: "memif"}},
		}},
		Vrfs: []Vrf{{Name: "vs-nat",
			L3Interfaces: []L3Interface{{Interface: iface, Addresses: []string{"192.168.200.1/24"}}}}},
	}
}

func TestKernelDataPlaneRejectsVnicDerivedNameL3Interface(t *testing.T) {
	longVM := strings.Repeat("a", 60)
	hashed := VnfIfaceName(longVM, "nic0")
	if hashed == "vh-"+longVM+"-nic0" {
		t.Fatalf("前提不成立：VM 名 %d 字符时 VnfIfaceName 应走哈希回退，得到 %q", len(longVM), hashed)
	}

	cases := []struct {
		name      string
		iface     string
		vmName    string // 需要长名 VM 的用例改主 VM 名（派生名必须可归因才命中）
		wantOwner string // 派生名命中的归属（"<VM|容器>/<vNIC>"）
		shortName bool   // 短名命中：既有文案，不做派生名要点断言
	}{
		{name: "VM 派生名", iface: "vh-vm1-nic0", wantOwner: "vm1/nic0"},
		{name: "容器派生名", iface: "mf-ct1-nic0", wantOwner: "ct1/nic0"},
		{name: "超长哈希派生名", iface: hashed, vmName: longVM, wantOwner: longVM + "/nic0"},
		{name: "vNIC 短名（既有行为保持）", iface: "nic0", shortName: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := derivedL3Config(DataPlaneKernel, tc.iface)
			if tc.vmName != "" {
				c.VirtualMachineFunctions[0].Name = tc.vmName
			}
			errs := Validate(c)
			if len(errs) == 0 {
				t.Fatalf("内核数据面下 l3-interface %q 应提交期拒绝", tc.iface)
			}
			txt := errText(errs)
			if !strings.Contains(txt, "vrfs[vs-nat].l3_interfaces["+tc.iface+"]") {
				t.Fatalf("报错应定位到该 l3-interface，得到：\n%s", txt)
			}
			if tc.shortName {
				return // 短名走既有文案（「不能直接作为三层接口」），不要求派生名要点。
			}
			for _, want := range []string{
				"派生名", "vNIC " + tc.wantOwner, "内核侧不存在该设备", "物理口/bond",
				"set system dataplane vpp",
			} {
				if !strings.Contains(txt, want) {
					t.Fatalf("报错应含 %q，得到：\n%s", want, txt)
				}
			}
		})
	}
}

// VPP 侧同配置必须放行（#172 文档化形态完全保留；本检查只挂在内核块里）。
func TestVPPDataPlaneKeepsVnicDerivedNameL3Interface(t *testing.T) {
	for _, iface := range []string{"vh-vm1-nic0", "mf-ct1-nic0"} {
		if errs := Validate(derivedL3Config(DataPlaneVPP, iface)); len(errs) != 0 {
			t.Fatalf("VPP 数据面下 l3-interface %q 应放行，得到：\n%s", iface, errText(errs))
		}
	}
}

// 只挡**可归因**的派生名：没有任何 vNIC 声明能派生出的 vh-/mf- 名不被本检查点名
// （判据不做前缀猜测），仍走既有的「不存在或不是物理口…」检查——如实边界。
func TestKernelDataPlaneDerivedNameCheckOnlyForDeclaredVnics(t *testing.T) {
	errs := Validate(derivedL3Config(DataPlaneKernel, "vh-ghost-nic9"))
	if len(errs) == 0 {
		t.Fatal("未被任何 vNIC 声明的 vh- 名应被既有存在性检查拒绝")
	}
	txt := errText(errs)
	if !strings.Contains(txt, "不存在") {
		t.Fatalf("应走既有「不存在或不是物理口…」检查，得到：\n%s", txt)
	}
	if strings.Contains(txt, "派生名") {
		t.Fatalf("无可归因声明的 vh- 名不应被派生名检查点名，得到：\n%s", txt)
	}
}
