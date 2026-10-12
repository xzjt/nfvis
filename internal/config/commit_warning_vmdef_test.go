package config

// 决策 #470（收口 round24 R24-D）：运行中负载「形态/硬件变更需 stop+start」的可发现性。
//
// 语义本身不改（restart 是 ACPI 重启、**沿用旧定义**；start 对已运行域是幂等空操作）——
// 变更在**下次启动**生效，要让运行中的负载进入新形态必须 stop+start。缺的是提交输出的
// 照做路径，本文件覆盖两类警告：
//   ① vmDefinitionWarnings：**既有** VM 的运行域定义相关声明（vNIC/磁盘/内存/CPU/PCI 直通/
//      cloud-init）变更时的提交警告（点名变更维度）；
//   ② dataPlaneWarnings 扩展：数据面切换且配置含 VNF/容器时，在既有「需重启服务」警告后
//      补「运行中负载仍使用旧数据面形态（网络将不通）」警告。
// 负向控制（不得误报）：新增/删除 VM 不出、仅改 description/image 之类非运行域字段不出、
// 无 VNF/容器时数据面切换不追加。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// vmDefChangedHint 运行域变更警告的判别子串（不含决策号；只出现在本类警告里）。
const vmDefChangedHint = "restart 沿用旧定义"

// dpLoadRebuildText 数据面切换后的负载重建警告全文（逐字比对用）。
const dpLoadRebuildText = "警告: 运行中的 VNF/容器仍使用旧数据面形态（网络将不通）：" +
	"需对其 stop+start（容器 restart）按新数据面重建"

// vmDefCommitCfg 生成「基线 + 资源池 + 指定 VM 列表」的候选配置。
// 含 VM 的提交要过资源池账本校验，故带上 poolsFor()（与既有 VM 用例同夹子）。
func vmDefCommitCfg(vms ...model.VMFunction) model.Config {
	cfg := baseCommitted()
	cfg.ResourcePools = poolsFor()
	cfg.VirtualMachineFunctions = vms
	return cfg
}

// vmDefCommitVM 一台通过校验的最小 VM（image/vcpu/memory 必填项齐全）。
func vmDefCommitVM() model.VMFunction {
	return model.VMFunction{
		Name: "vm-a", Image: "ubuntu22-vm",
		VCPU:   model.VMCpu{Count: 2},
		Memory: model.VMMemory{SizeMB: 8192, HugepageSize: "1G"},
	}
}

// commitConsole 以 console 会话提交一份候选；校验失败直接 Fatal 并打出逐条错误。
func commitConsole(t *testing.T, k *engineKit, cfg model.Config) CommitResult {
	t.Helper()
	sess := Session{User: "admin", Source: "console"}
	k.edit(t, "admin", "console")
	if err := k.engine.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	res, err := k.engine.Commit(context.Background(), sess, CommitOpts{})
	if err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			t.Fatalf("Commit: %v: %+v", err, ve.Errors)
		}
		t.Fatalf("Commit: %v", err)
	}
	return res
}

// 提交主路径：运行域定义相关声明变更（vNIC/内存）出警告并点名维度；
// 新增/删除 VM、仅改 description/image 不出。
func TestEngineCommitVmDefinitionWarning(t *testing.T) {
	// ① 新增 VM：不出运行域变更警告（没有「运行中沿用旧定义」一说）。
	t.Run("新增 VM 不出", func(t *testing.T) {
		k := newEngineKit(t)
		res := commitConsole(t, k, vmDefCommitCfg(vmDefCommitVM()))
		if warningsContain(res.Warnings, vmDefChangedHint) {
			t.Fatalf("新增 VM 不应出运行域变更警告: %+v", res.Warnings)
		}
	})

	// ② 给既有 VM 新增 vNIC ⇒ 出警告、点名「vNIC 声明」、写明 stop+start 照做路径。
	t.Run("改 vNIC 出并点名", func(t *testing.T) {
		k := newEngineKit(t)
		base := commitConsole(t, k, vmDefCommitCfg(vmDefCommitVM()))
		if warningsContain(base.Warnings, vmDefChangedHint) {
			t.Fatalf("前置（首次声明 VM）不应出警告: %+v", base.Warnings)
		}
		vm := vmDefCommitVM()
		vm.Interfaces = []model.VnfInterface{{Name: "eth0", Type: "vhost-user"}}
		res := commitConsole(t, k, vmDefCommitCfg(vm))
		if !warningsContain(res.Warnings, "VM vm-a 的 vNIC 声明 已变更") {
			t.Fatalf("改 vNIC 应出警告并点名「vNIC 声明」: %+v", res.Warnings)
		}
		if !warningsContain(res.Warnings, "需 stop+start") {
			t.Fatalf("警告应写明 stop+start 照做路径: %+v", res.Warnings)
		}
	})

	// ③ 改内存 ⇒ 点名「内存」。
	t.Run("改内存出", func(t *testing.T) {
		k := newEngineKit(t)
		commitConsole(t, k, vmDefCommitCfg(vmDefCommitVM()))
		vm := vmDefCommitVM()
		vm.Memory.SizeMB = 16384
		res := commitConsole(t, k, vmDefCommitCfg(vm))
		if !warningsContain(res.Warnings, "VM vm-a 的 内存 已变更") {
			t.Fatalf("改内存应出警告并点名「内存」: %+v", res.Warnings)
		}
	})

	// ④ 仅改 description / image（非运行域字段）⇒ 不出。
	t.Run("仅改非运行域字段不出", func(t *testing.T) {
		k := newEngineKit(t)
		commitConsole(t, k, vmDefCommitCfg(vmDefCommitVM()))
		vm := vmDefCommitVM()
		vm.Description = "注释变更"
		res := commitConsole(t, k, vmDefCommitCfg(vm))
		if warningsContain(res.Warnings, vmDefChangedHint) {
			t.Fatalf("仅改 description 不应出警告: %+v", res.Warnings)
		}
		vm.Image = "other-img"
		res = commitConsole(t, k, vmDefCommitCfg(vm))
		if warningsContain(res.Warnings, vmDefChangedHint) {
			t.Fatalf("仅改 image 不应出警告: %+v", res.Warnings)
		}
	})

	// ⑤ 删除 VM ⇒ 不出（它已不在 new 的 VM 集合里）。
	t.Run("删除 VM 不出", func(t *testing.T) {
		k := newEngineKit(t)
		commitConsole(t, k, vmDefCommitCfg(vmDefCommitVM()))
		res := commitConsole(t, k, vmDefCommitCfg())
		if warningsContain(res.Warnings, vmDefChangedHint) {
			t.Fatalf("删除 VM 不应出运行域变更警告: %+v", res.Warnings)
		}
	})
}

// 提交主路径：数据面切换的负载重建警告（带 VNF 出；不带不出）。
func TestEngineCommitDataPlaneSwitchLoadWarning(t *testing.T) {
	// ① 带 VNF 切数据面：既有「需重启服务」与新「负载需重建」两条都在。
	k := newEngineKit(t)
	cfg := vmDefCommitCfg(vmDefCommitVM())
	cfg.System.DataPlane = model.DataPlaneKernel
	res := commitConsole(t, k, cfg)
	if !warningsContain(res.Warnings, "需重启服务") {
		t.Fatalf("数据面切换应保留既有「需重启服务」警告: %+v", res.Warnings)
	}
	if !warningsContain(res.Warnings, dpLoadRebuildText) {
		t.Fatalf("带 VNF 切换应追加负载重建警告: %+v", res.Warnings)
	}

	// ② 不带 VNF/容器切换：只有「需重启服务」，不追加负载重建警告。
	k2 := newEngineKit(t)
	cfg2 := baseCommitted()
	cfg2.System.DataPlane = model.DataPlaneKernel
	res2 := commitConsole(t, k2, cfg2)
	if !warningsContain(res2.Warnings, "需重启服务") {
		t.Fatalf("数据面切换应提示需重启服务: %+v", res2.Warnings)
	}
	if warningsContain(res2.Warnings, "仍使用旧数据面形态") {
		t.Fatalf("无 VNF/容器时不应追加负载重建警告: %+v", res2.Warnings)
	}
}

// 直接调用 dataPlaneWarnings：负载（VM/容器）在场才追加，同实现切换零警告。
func TestDataPlaneWarningsRebuildAfterSwitch(t *testing.T) {
	vpp := model.Config{}
	kernel := model.Config{System: &model.SystemConfig{DataPlane: model.DataPlaneKernel}}

	// 无负载：仅「需重启服务」一条。
	ws := dataPlaneWarnings(vpp, kernel)
	if len(ws) != 1 || !warningsContain(ws, "需重启服务") {
		t.Fatalf("无负载切换应仅「需重启服务」: %+v", ws)
	}

	// 含 VM：追加重建警告（全文比对）。
	withVM := kernel
	withVM.VirtualMachineFunctions = []model.VMFunction{{Name: "vm-a"}}
	ws2 := dataPlaneWarnings(vpp, withVM)
	if !warningsContain(ws2, dpLoadRebuildText) {
		t.Fatalf("含 VNF 切换应追加负载重建警告: %+v", ws2)
	}

	// 含容器：同样追加。
	withCT := kernel
	withCT.ContainerFunctions = []model.ContainerFunction{{Name: "ct-a"}}
	if !warningsContain(dataPlaneWarnings(vpp, withCT), dpLoadRebuildText) {
		t.Fatalf("含容器切换应追加负载重建警告: %+v", dataPlaneWarnings(vpp, withCT))
	}

	// 未切换（同一实现）：零警告（load 在场也不出）。
	if ws4 := dataPlaneWarnings(withVM, withVM); len(ws4) != 0 {
		t.Fatalf("同实现切换不应有警告: %+v", ws4)
	}
}

// 直接调用 vmDefinitionWarnings：六个变更维度逐一命中、多维度按固定顺序、
// 多 VM 按名排序、无关字段与增删 VM 不命中、无变更零输出。
func TestVmDefinitionWarningsDimensionsAndOrder(t *testing.T) {
	with := func(mut func(*model.VMFunction)) model.Config {
		vm := vmDefCommitVM()
		mut(&vm)
		return model.Config{VirtualMachineFunctions: []model.VMFunction{vm}}
	}
	base := model.Config{VirtualMachineFunctions: []model.VMFunction{vmDefCommitVM()}}

	cases := []struct {
		name string
		mut  func(*model.VMFunction)
		dim  string
	}{
		{"vNIC", func(vm *model.VMFunction) {
			vm.Interfaces = []model.VnfInterface{{Name: "eth0", Type: "vhost-user"}}
		}, "vNIC 声明"},
		{"磁盘", func(vm *model.VMFunction) {
			vm.Disks = []model.VMDisk{{Name: "data0", SizeGB: 10}}
		}, "磁盘"},
		{"内存", func(vm *model.VMFunction) { vm.Memory.SizeMB = 16384 }, "内存"},
		{"CPU", func(vm *model.VMFunction) { vm.VCPU.Count = 4 }, "CPU"},
		{"PCI 直通设备", func(vm *model.VMFunction) {
			vm.PCIDevices = []string{"0000:03:00.0"}
		}, "PCI 直通设备"},
		{"cloud-init", func(vm *model.VMFunction) {
			vm.CloudInit = &model.CloudInit{UserData: "#cloud-config"}
		}, "cloud-init"},
	}
	for _, tc := range cases {
		ws := vmDefinitionWarnings(base, with(tc.mut))
		if len(ws) != 1 || !contains(ws[0], tc.dim) {
			t.Fatalf("%s 变更应出 1 条点名「%s」的警告: %+v", tc.name, tc.dim, ws)
		}
	}

	// 全文比对：维度拼接顺序固定（vNIC 声明 → 内存）、VM 名与照做路径逐字。
	both := with(func(vm *model.VMFunction) {
		vm.Interfaces = []model.VnfInterface{{Name: "eth0", Type: "vhost-user"}}
		vm.Memory.SizeMB = 16384
	})
	want := "警告: VM vm-a 的 vNIC 声明、内存 已变更：变更在下次启动时生效；" +
		"VM 运行中时 restart 沿用旧定义，需 stop+start"
	ws := vmDefinitionWarnings(base, both)
	if len(ws) != 1 || ws[0] != want {
		t.Fatalf("双维度警告全文不符:\n got: %+v\nwant: %s", ws, want)
	}

	// 多 VM：按 VM 名排序输出；未变更的 VM 不出现。
	mk := func(name string, mem int) model.VMFunction {
		vm := vmDefCommitVM()
		vm.Name = name
		vm.Memory.SizeMB = mem
		return vm
	}
	old := model.Config{VirtualMachineFunctions: []model.VMFunction{mk("vm-b", 8192), mk("vm-a", 8192), mk("vm-c", 8192)}}
	next := model.Config{VirtualMachineFunctions: []model.VMFunction{
		mk("vm-c", 8192), // 未变
		mk("vm-b", 16384),
		mk("vm-a", 16384),
	}}
	got := vmDefinitionWarnings(old, next)
	if len(got) != 2 || !contains(got[0], "VM vm-a") || !contains(got[1], "VM vm-b") {
		t.Fatalf("应按 VM 名排序只报变更的 VM: %+v", got)
	}

	// 新增/删除 VM、无关字段变更、无变更：零输出。
	added := model.Config{VirtualMachineFunctions: []model.VMFunction{mk("vm-a", 8192), mk("vm-new", 8192)}}
	if got := vmDefinitionWarnings(base, added); len(got) != 0 {
		t.Fatalf("新增 VM 不应出: %+v", got)
	}
	removed := model.Config{}
	if got := vmDefinitionWarnings(base, removed); len(got) != 0 {
		t.Fatalf("删除 VM 不应出: %+v", got)
	}
	desc := with(func(vm *model.VMFunction) { vm.Description = "x"; vm.Image = "other" })
	if got := vmDefinitionWarnings(base, desc); len(got) != 0 {
		t.Fatalf("仅改 description/image 不应出: %+v", got)
	}
	if got := vmDefinitionWarnings(base, base); len(got) != 0 {
		t.Fatalf("无变更不应出: %+v", got)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
