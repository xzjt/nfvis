package model

import (
	"strings"
	"testing"
)

// FR-NET-021：VF 直通占用物理口时，其 PF 端口禁止进 bridge domain / L3 接口。
func TestVFSriovPFInDataPathRejected(t *testing.T) {
	c := validBase()
	c.Interfaces = append(c.Interfaces, InterfaceConfig{Name: "ens192"})
	c.VirtualSwitches = append(c.VirtualSwitches, VirtualSwitch{
		Name: "vs1", Type: "l2",
		Ports: []VSwitchPort{{Seq: 0, Interface: "ens192"}},
	})
	c.VirtualMachineFunctions = append(c.VirtualMachineFunctions, VMFunction{
		Name: "fw-vm", Image: "img", VCPU: VMCpu{Count: 1},
		Memory: VMMemory{SizeMB: 1024, HugepageSize: "1G"},
		Interfaces: []VnfInterface{{
			Name: "eth1", Type: "sriov-vf",
			Sriov: &SriovBind{PhysicalInterface: "ens192", VFID: 1},
		}},
	})
	errs := Validate(c)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "FR-NET-021") && strings.Contains(e.Path, "sriov.physical_interface") {
			found = true
		}
	}
	if !found {
		t.Fatalf("PF 进 BD 时应报 FR-NET-021，实际: %v", errs)
	}

	// 移除全部 BD 端口引用后应通过。
	for i := range c.VirtualSwitches {
		c.VirtualSwitches[i].Ports = nil
	}
	for _, e := range Validate(c) {
		if strings.Contains(e.Message, "FR-NET-021") {
			t.Fatalf("未引用 PF 时不应报 FR-NET-021: %v", e)
		}
	}
}
