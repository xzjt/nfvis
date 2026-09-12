package model

import (
	"strings"
	"testing"
)

// 资源池账本测试（FR-CMP-002/003/004、FR-CFG-011⑥⑨⑪、FR-SYS-010）。

func ledgerBase() Config {
	c := validBase() // 已含 1G x32 大页池 + 隔离核 4-7 + vpp(main 4, workers 5,6)
	c.ResourcePools = &ResourcePool{
		Hugepages: []HPool{{PageSize: "1G", Count: 32}},
		CPU:       &CPUSetup{IsolatedCores: []int{4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}},
	}
	c.Vpp = &VppConfig{
		CPU:    &VppCPU{MainCore: 4, CorelistWorkers: "5,6"},
		Memory: &VppMemory{HugepagePreference: "1G"},
	}
	c.VirtualMachineFunctions = nil
	c.ContainerFunctions = nil
	return c
}

func vmOf(name string, vcpu int, sizeMB int) VMFunction {
	return VMFunction{
		Name: name, Image: "img",
		VCPU:   VMCpu{Count: vcpu},
		Memory: VMMemory{SizeMB: sizeMB, HugepageSize: "1G"},
	}
}

func TestLedgerAllocateHappyPath(t *testing.T) {
	c := ledgerBase()
	c.VirtualMachineFunctions = []VMFunction{vmOf("fw-vm", 4, 8192)}
	l := NewPoolLedger(c)
	errs := l.Allocate(c)
	if len(errs) != 0 {
		t.Fatalf("应分配成功: %v", errs)
	}
	// VPP 保留 main(4)+workers(5,6)；VM 从剩余升序取 4 个：7,8,9,10（FR-SYS-010 互斥）
	if cores := l.CPU.VMCores["fw-vm"]; !equalInts(cores, []int{7, 8, 9, 10}) {
		t.Fatalf("fw-vm 应绑核 [7 8 9 10]（VPP 保留 4,5,6），实际 %v", cores)
	}
	if !equalInts(l.CPU.VppReserved, []int{4, 5, 6}) {
		t.Fatalf("VPP 保留核应为 [4 5 6]，实际 %v", l.CPU.VppReserved)
	}
	// 大页：8192MB / 1G = 8 页（原型同口径）
	if hp := l.Hugepages["1G"]; hp.Allocated != 8 || hp.Free != 24 {
		t.Fatalf("1G 池应 allocated=8 free=24，实际 %+v", hp)
	}
	if !equalInts(l.CPU.Free, []int{11, 12, 13, 14, 15}) {
		t.Fatalf("空闲核应为 [11..15]，实际 %v", l.CPU.Free)
	}
}

func TestLedgerSecondVMAndNumaAffinity(t *testing.T) {
	c := ledgerBase()
	c.ResourcePools.CPU.Numa = []NumaNode{
		{Node: 0, Cores: []int{4, 5, 6, 7, 8, 9, 10, 11}},
		{Node: 1, Cores: []int{12, 13, 14, 15}},
	}
	vm1 := vmOf("fw-vm", 4, 8192)
	vm2 := vmOf("probe-vm", 2, 4096)
	vm2.Memory.NumaNode = intPtr(1)
	c.VirtualMachineFunctions = []VMFunction{vm1, vm2}

	l := NewPoolLedger(c)
	if errs := l.Allocate(c); len(errs) != 0 {
		t.Fatalf("应分配成功: %v", errs)
	}
	// probe-vm 声明 NUMA 1：只从 12-15 取（VPP 不占 NUMA1）
	if cores := l.CPU.VMCores["probe-vm"]; !equalInts(cores, []int{12, 13}) {
		t.Fatalf("probe-vm 应从 NUMA1 绑核 [12 13]，实际 %v", cores)
	}
	// 4096MB / 1G = 4 页
	if hp := l.Hugepages["1G"]; hp.Allocated != 12 || hp.Free != 20 {
		t.Fatalf("1G 池应 allocated=12 free=20，实际 %+v", hp)
	}
}

func TestLedgerHugepageShortfallDetail(t *testing.T) {
	c := ledgerBase()
	c.ResourcePools.Hugepages = []HPool{{PageSize: "1G", Count: 8}}
	c.VirtualMachineFunctions = []VMFunction{vmOf("fw-vm", 2, 8192), vmOf("probe-vm", 2, 4096)}
	errs := NewPoolLedger(c).Allocate(c)
	found := false
	for _, e := range errs {
		// FR-CFG-011⑨：缺口明细（需要/空闲），OpenAPI Error.detail 示例同口径
		if strings.Contains(e.Message, "需要 4") && strings.Contains(e.Message, "空闲 0") &&
			strings.Contains(e.Path, "probe-vm") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应给出大页缺口明细，实际: %v", errs)
	}
}

func TestLedgerCoreShortfallDetail(t *testing.T) {
	c := ledgerBase()
	c.ResourcePools.CPU.IsolatedCores = []int{4, 5, 6, 7} // VPP 占 4,5,6 后仅剩 1 核
	c.VirtualMachineFunctions = []VMFunction{vmOf("fw-vm", 2, 1024)}
	errs := NewPoolLedger(c).Allocate(c)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "需要 2") && strings.Contains(e.Message, "可用 1") &&
			strings.Contains(e.Path, "fw-vm") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应给出隔离核缺口明细（FR-CMP-002），实际: %v", errs)
	}
}

func TestLedgerNumaShortfallPrefersGlobalHint(t *testing.T) {
	c := ledgerBase()
	c.ResourcePools.CPU.Numa = []NumaNode{
		{Node: 0, Cores: []int{4, 5, 6}},
		{Node: 1, Cores: []int{7, 8}},
	}
	vm := vmOf("fw-vm", 2, 1024)
	vm.Memory.NumaNode = intPtr(1)
	c.VirtualMachineFunctions = []VMFunction{vm}
	errs := NewPoolLedger(c).Allocate(c)
	// 全局尚有 9-15 空闲，但 NUMA1 仅 7,8 已被占（vpp 4,5,6 之后 fw-vm 取 7,8），
	// 第二台同 NUMA VM 应报 NUMA 维度缺口
	vm2 := vmOf("probe-vm", 1, 1024)
	vm2.Memory.NumaNode = intPtr(1)
	c.VirtualMachineFunctions = append(c.VirtualMachineFunctions, vm2)
	errs = NewPoolLedger(c).Allocate(c)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "NUMA 1") && strings.Contains(e.Path, "probe-vm") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应报 NUMA 1 维度缺口（FR-CMP-002 缺哪个 NUMA），实际: %v", errs)
	}
}

func TestLedgerUnpinnedVMSkipsCoreAllocation(t *testing.T) {
	c := ledgerBase()
	c.ResourcePools.CPU.IsolatedCores = []int{4, 5, 6}
	vm := vmOf("fw-vm", 4, 1024)
	pin := false
	vm.VCPU.Pin = &pin
	c.VirtualMachineFunctions = []VMFunction{vm}
	l := NewPoolLedger(c)
	if errs := l.Allocate(c); len(errs) != 0 {
		t.Fatalf("pin=false 不应做绑核分配: %v", errs)
	}
	if len(l.CPU.VMCores["fw-vm"]) != 0 {
		t.Fatalf("pin=false 不应占用隔离核")
	}
}

func TestLedgerRequiresPools(t *testing.T) {
	c := ledgerBase()
	c.ResourcePools = nil
	c.VirtualMachineFunctions = []VMFunction{vmOf("fw-vm", 2, 1024)}
	errs := NewPoolLedger(c).Allocate(c)
	if len(errs) == 0 {
		t.Fatalf("无资源池时创建 VNF 应报错（FR-CMP-002）")
	}
}

func TestLedgerPoolShrinkBlockedByExistingVNF(t *testing.T) {
	// FR-CMP-004：资源池缩减前校验不被存量 VNF 占用
	c := ledgerBase()
	c.VirtualMachineFunctions = []VMFunction{vmOf("fw-vm", 2, 8192)}
	c.ResourcePools.Hugepages = []HPool{{PageSize: "1G", Count: 4}} // 存量需 8 页
	errs := NewPoolLedger(c).Allocate(c)
	if len(errs) == 0 {
		t.Fatalf("池缩减到存量之下应报错")
	}
}

func TestLedgerDefaultMainPool(t *testing.T) {
	c := ledgerBase()
	c.ResourcePools.Hugepages = []HPool{{PageSize: "2M", Count: 4096}, {PageSize: "1G", Count: 8}}
	vm := vmOf("fw-vm", 2, 1024)
	vm.Memory.HugepageSize = "" // 缺省取主池（2M：1024MB/2MB = 512 页）
	c.VirtualMachineFunctions = []VMFunction{vm}
	l := NewPoolLedger(c)
	if errs := l.Allocate(c); len(errs) != 0 {
		t.Fatalf("缺省主池应可分配: %v", errs)
	}
	if hp := l.Hugepages["2M"]; hp.Allocated != 512 {
		t.Fatalf("2M 主池应分配 512 页，实际 %d", hp.Allocated)
	}
}

func TestCheckResourcesConvenience(t *testing.T) {
	c := ledgerBase()
	c.VirtualMachineFunctions = []VMFunction{vmOf("fw-vm", 4, 8192)}
	if errs := CheckResources(c); len(errs) != 0 {
		t.Fatalf("合法配置应通过资源校验: %v", errs)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
