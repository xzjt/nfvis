package model

import "testing"

// 决策 #461（收口 round12/13 观察①）：读视图给**有效值**——配置未显式声明、但产品语义上有
// 确定缺省的项（pin 缺省 true / backing 缺省 hugepage / 页尺寸缺省取资源池主池）补成有效值，
// 免得界面在运行态明明有事实时显示「—」。只补有确定缺省的项（numa_node 不兜底）。
func TestEffectiveVMFillsDefaults(t *testing.T) {
	base := func(vm VMFunction) Config {
		return Config{
			ResourcePools: &ResourcePool{
				Hugepages: []HPool{{PageSize: "1G", Count: 8}},
				CPU:       &CPUSetup{IsolatedCores: []int{4, 5, 6, 7}},
			},
			VirtualMachineFunctions: []VMFunction{vm},
		}
	}

	// 全部未显式声明 ⇒ 三个字段都给有效值。
	cfg := base(VMFunction{Name: "fw-vm", Image: "base.qcow2", VCPU: VMCpu{Count: 2}, Memory: VMMemory{SizeMB: 2048}})
	eff := EffectiveVM(cfg, cfg.VirtualMachineFunctions[0])
	if eff.VCPU.Pin == nil || !*eff.VCPU.Pin {
		t.Fatalf("pin 未声明时有效值应为 true：%+v", eff.VCPU)
	}
	if eff.Memory.Backing != "hugepage" {
		t.Fatalf("backing 未声明时有效值应为 hugepage：%q", eff.Memory.Backing)
	}
	if eff.Memory.HugepageSize != "1G" {
		t.Fatalf("页尺寸未声明时应取资源池主池 1G：%q", eff.Memory.HugepageSize)
	}
	if cfg.VirtualMachineFunctions[0].VCPU.Pin != nil || cfg.VirtualMachineFunctions[0].Memory.Backing != "" {
		t.Fatal("EffectiveVM 不得改动入参（返回副本）")
	}

	// 显式值一律保留（含 pin=false 这种与缺省相反的显式值）。
	off := false
	cfg2 := base(VMFunction{Name: "fw-vm", Image: "x", VCPU: VMCpu{Count: 1, Pin: &off},
		Memory: VMMemory{SizeMB: 2048, HugepageSize: "2M"}})
	eff2 := EffectiveVM(cfg2, cfg2.VirtualMachineFunctions[0])
	if eff2.VCPU.Pin == nil || *eff2.VCPU.Pin {
		t.Fatalf("显式 pin=false 必须保留：%+v", eff2.VCPU)
	}
	if eff2.Memory.HugepageSize != "2M" {
		t.Fatalf("显式页尺寸必须保留：%q", eff2.Memory.HugepageSize)
	}

	// backing=normal：普通内存不用大页 ⇒ 页尺寸**不**兜底（否则界面会谎报用了 1G 页）。
	cfg3 := base(VMFunction{Name: "fw-vm", Image: "x", VCPU: VMCpu{Count: 1},
		Memory: VMMemory{SizeMB: 512, Backing: "normal"}})
	eff3 := EffectiveVM(cfg3, cfg3.VirtualMachineFunctions[0])
	if eff3.Memory.Backing != "normal" {
		t.Fatalf("显式 backing=normal 应保留：%q", eff3.Memory.Backing)
	}
	if eff3.Memory.HugepageSize != "" {
		t.Fatalf("backing=normal 不应兜底页尺寸：%q", eff3.Memory.HugepageSize)
	}
}

// AssignedCores：运行态绑核取自**资源池账本**（与 show resource-pools 的「已分配」同源）；
// 账本没分到（未声明该 VM / pin=false）时返回 nil——不编造。
func TestAssignedCoresFromLedger(t *testing.T) {
	cfg := Config{
		ResourcePools: &ResourcePool{
			Hugepages: []HPool{{PageSize: "1G", Count: 8}},
			CPU:       &CPUSetup{IsolatedCores: []int{4, 5, 6, 7}},
		},
		VirtualMachineFunctions: []VMFunction{{Name: "fw-vm", Image: "x", VCPU: VMCpu{Count: 2}, Memory: VMMemory{SizeMB: 2048}}},
	}
	if got := AssignedCores(cfg, "fw-vm"); len(got) != 2 {
		t.Fatalf("应按账本分到 2 核：%v", got)
	}
	if got := AssignedCores(cfg, "不存在"); got != nil {
		t.Fatalf("未声明的 VM 应返回 nil：%v", got)
	}

	// pin=false ⇒ 账本不分配绑核 ⇒ 读视图不报（界面「—」如实）。
	off := false
	cfg.VirtualMachineFunctions[0].VCPU.Pin = &off
	if got := AssignedCores(cfg, "fw-vm"); got != nil {
		t.Fatalf("pin=false 时不应有绑核：%v", got)
	}
}
