package model

// round49（外部走查）：CPU 账本与数据面脱节。
//
// 缺陷形态：NewPoolLedger 无条件把 `vpp.cpu.main-core` 与 `corelist-workers` 记为**在用者**，
// checkVpp 也无条件要求这些核落在隔离池内——而内核数据面下 VPP 根本不运行（vpp 段配置按
// 设计保留，但不下发、VPP 也被停掉，见 checkKernelDataPlane 与决策 #426）。后果有两面：
//   - 读数：内核数据面下 `show resource-pools` 的「空闲核」凭空少几枚（口径红线：可用读数
//     必须扣掉**全部**在用者，但也不得扣不存在的占用者）；
//   - 校验：明明有空闲核却报「隔离核不足」/「核不在隔离核池内」，把 VPP 时代的调参变成
//     内核数据面下的提交拦路石。
//
// 另一半是 main-core 的 0 语义：产品三处都把 `0` 当「未显式指定」（startup.go 的
// `MainCore > 0` 才写 main-core、engine 的空段判定、向导的「输入 - 表示不配置 VPP CPU」），
// 而未指定时 VPP 仍要占一个核（缺省主核＝核 0），账本据此保留核 0。

import "testing"

// TestLedgerKernelDataPlaneKeepsVppCoresAvailable 内核数据面下 VPP 线程核不占用隔离池：
// 三枚隔离核全部可分配给 VNF，vpp-reserved 为空。
func TestLedgerKernelDataPlaneKeepsVppCoresAvailable(t *testing.T) {
	c := ledgerBase()
	c.System = &SystemConfig{DataPlane: DataPlaneKernel}
	c.ResourcePools.CPU.IsolatedCores = []int{4, 5, 6}
	c.VirtualMachineFunctions = []VMFunction{vmOf("fw-vm", 3, 1024)}

	l := NewPoolLedger(c)
	if errs := l.Allocate(c); len(errs) != 0 {
		t.Fatalf("内核数据面下 VPP 不运行，其线程核应从可分配额度里释放: %v", errs)
	}
	if len(l.CPU.VppReserved) != 0 {
		t.Fatalf("内核数据面下 vpp-reserved 应为空，实际 %v", l.CPU.VppReserved)
	}
	if cores := l.CPU.VMCores["fw-vm"]; !equalInts(cores, []int{4, 5, 6}) {
		t.Fatalf("三枚隔离核都应可分配给 VNF，实际 %v", cores)
	}
}

// VPP 数据面下行为不变：VPP 线程核照旧先扣（FR-SYS-010）。
func TestLedgerVppDataPlaneStillReservesVppCores(t *testing.T) {
	c := ledgerBase()
	c.System = &SystemConfig{DataPlane: DataPlaneVPP}
	c.ResourcePools.CPU.IsolatedCores = []int{4, 5, 6}
	c.VirtualMachineFunctions = []VMFunction{vmOf("fw-vm", 3, 1024)}

	errs := NewPoolLedger(c).Allocate(c)
	if len(errs) == 0 {
		t.Fatal("VPP 数据面下 main-core(4)+workers(5,6) 必须先从额度里扣掉，3 vcpu 的 VNF 应报缺口")
	}
}

// main-core 未显式指定（0）＝ VPP 用缺省主核，产品**不向 startup.conf 写主核**（startup.go
// 的 `MainCore > 0` 才写）⇒ 不钉核、也就没有可扣的核：账本不凭空占一枚核
// （见 resources.vppReservedCores 的如实边界；`docs/` 无 VPP 主线程缺省落点的依据）。
func TestLedgerMainCoreUnsetReservesNothing(t *testing.T) {
	c := ledgerBase()
	c.Vpp = &VppConfig{CPU: &VppCPU{}} // 未声明 main-core，也没有 workers
	c.ResourcePools.CPU.IsolatedCores = []int{0, 1, 2, 3}

	l := NewPoolLedger(c)
	if len(l.CPU.VppReserved) != 0 {
		t.Fatalf("未显式声明 VPP 线程核时不应保留任何核（不猜缺省主核），实际 vpp-reserved=%v", l.CPU.VppReserved)
	}
	if !equalInts(l.CPU.Free, []int{0, 1, 2, 3}) {
		t.Fatalf("空闲应等于隔离池全集，实际 %v", l.CPU.Free)
	}
	// 隔离池含 0 的既有形态照旧可分配满（与 orchestrator/compute 的收敛夹具同口径）
	c.VirtualMachineFunctions = []VMFunction{vmOf("fw-vm", 4, 1024)}
	if errs := l.Allocate(c); len(errs) != 0 {
		t.Fatalf("隔离池 0-3 应可分配给 4 vcpu 的 VNF: %v", errs)
	}
}

// 内核数据面下同样不保留（VPP 不运行；此案与「未声明」结果相同，作为显式守护）。
func TestLedgerKernelDataPlaneReservesNoCores(t *testing.T) {
	c := ledgerBase()
	c.System = &SystemConfig{DataPlane: DataPlaneKernel}
	c.Vpp = &VppConfig{CPU: &VppCPU{}}
	c.ResourcePools.CPU.IsolatedCores = []int{0, 1, 2, 3}

	l := NewPoolLedger(c)
	if len(l.CPU.VppReserved) != 0 || !equalInts(l.CPU.Free, []int{0, 1, 2, 3}) {
		t.Fatalf("内核数据面下不应保留任何 VPP 核，实际 vpp-reserved=%v free=%v", l.CPU.VppReserved, l.CPU.Free)
	}
}

// main-core 显式指定时按声明值保留（不与缺省核 0 叠加）。
func TestLedgerExplicitMainCoreNotDoubled(t *testing.T) {
	c := ledgerBase()
	c.Vpp = &VppConfig{CPU: &VppCPU{MainCore: 3}}
	c.ResourcePools.CPU.IsolatedCores = []int{0, 1, 2, 3}

	l := NewPoolLedger(c)
	if !equalInts(l.CPU.VppReserved, []int{3}) {
		t.Fatalf("显式 main-core 3 时只保留 3，实际 %v", l.CPU.VppReserved)
	}
}

// 内核数据面下「VPP 线程核须在隔离池内」不生效（该段不下发）；配置**自洽**的检查照旧。
func TestValidateKernelDataPlaneSkipsVppCoreIsolation(t *testing.T) {
	c := validBase()
	c.System.DataPlane = DataPlaneKernel
	c.Vpp.CPU = &VppCPU{MainCore: 9, CorelistWorkers: "9"} // 9 不在隔离池 4-7 内
	mustNoErr(t, Validate(c))

	// 反向（VPP 数据面）：同一条配置必须被拒（现状不变）
	c.System.DataPlane = DataPlaneVPP
	mustErrContaining(t, Validate(c), "vpp.cpu.main-core", "不在隔离核池内")
	mustErrContaining(t, Validate(c), "vpp.cpu.corelist_workers", "不在隔离核池内")

	// 内核数据面下**语法**仍要自洽：核列表非法照旧拒绝
	c.System.DataPlane = DataPlaneKernel
	c.Vpp.CPU = &VppCPU{MainCore: 4, CorelistWorkers: "x-y"}
	mustErrContaining(t, Validate(c), "vpp.cpu.corelist_workers", "不合法")
}

// 核号为负是**永远不自洽**的配置（CLI 叶子是 uint，REST/配置导入可绕过）：两数据面都拒。
func TestValidateNegativeMainCoreRejected(t *testing.T) {
	for _, mode := range []string{DataPlaneVPP, DataPlaneKernel} {
		c := validBase()
		c.System.DataPlane = mode
		c.Vpp.CPU = &VppCPU{MainCore: -1}
		mustErrContaining(t, Validate(c), "vpp.cpu.main-core", "不能为负")
	}
}
