package model

import (
	"strings"
	"testing"
)

// 缺陷（真机 v3.0.24，BUG-05）：把已由数据面（VPP/DPDK）接管的网卡直通给 VM，commit 成功，
// `request virtual-machine-functions <vm> start` 才以 vfio `Could not open '/dev/vfio/8':
// Device or resource busy` 失败。
//
// 配置层能判定的只有**字面标识**这一维：真实配置里数据面写的是口名（`set vpp dpdk dev ens224`），
// 直通写的是 BDF（`pci-device 0000:13:00.0`），口名→BDF 的映射只在运行态（sysfs / 绑定记录）里，
// 本包（叶子包，network 反向 import 它）取不到。故这里钉住的是「同一标识同时被写成数据面端口与
// 直通设备」——操作者把 BDF 填进端口位置时的形态；报错必须点名设备、占用者与照做路径。
func TestFixPCIDeviceClaimedByDataplaneRejected(t *testing.T) {
	cases := []struct {
		name     string
		claim    func(c *Config) // 只设置本用例那一种数据面占用
		wantRole string
		wantCmd  string // 该角色对应的释放语句（报错须逐字给出，操作者才能照抄）
	}{
		{
			name: "dpdk-per-dev",
			claim: func(c *Config) {
				c.Vpp.DPDK.PerDev = []VppDevOverride{{Interface: "0000:13:00.0"}}
			},
			wantRole: "VPP/DPDK 接管",
			wantCmd:  "delete vpp dpdk dev 0000:13:00.0",
		},
		{
			name: "vswitch-port",
			claim: func(c *Config) {
				c.VirtualSwitches[0].Ports = []VSwitchPort{{Seq: 1, Interface: "0000:13:00.0"}}
			},
			wantRole: "虚拟交换机 vs-app 的端口",
			wantCmd:  "delete virtual-switches vs-app ports 1",
		},
		{
			name: "bond-member",
			claim: func(c *Config) {
				c.Bonds = []Bond{{Name: "bond0", Members: []string{"0000:13:00.0"}}}
			},
			wantRole: "bond bond0 的成员口",
			wantCmd:  "delete bonds bond0 members 0000:13:00.0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validBase()
			c.VirtualMachineFunctions[0].PCIDevices = []string{"13:00.0"} // 省略 domain 的写法
			tc.claim(&c)

			errs := Validate(c)
			got := findPCIError(errs)
			if got == nil {
				t.Fatalf("数据面已占用的 PCI 设备直通给 VM 应被拒绝，实际无相关报错: %v", errs)
			}
			if !strings.HasPrefix(got.Path, "virtual-machine-functions[fw-vm].pci_devices[0]") {
				t.Errorf("报错应指向 pci_devices[0]，实际路径 %q", got.Path)
			}
			for _, want := range []string{
				"0000:13:00.0",        // 归一后的设备地址（点名的设备）
				tc.wantRole,           // 谁占着
				tc.wantCmd,            // 照做路径①：删掉那一条占用声明（逐字可抄）
				"request vpp restart", // 照做路径②：让数据面放手
				"unbind-dpdk",         // 照做路径③：设备还在 vfio 上时交还内核
			} {
				if !strings.Contains(got.Message, want) {
					t.Errorf("报错文案缺少 %q:\n%s", want, got.Message)
				}
			}
		})
	}
}

// 无数据面占用时照常通过（不产生误报）。这是**真实配置的常态**：per_dev 按口名声明
// （`ens2f0`），而口名→BDF 的映射属运行态，配置层不应据此猜。
func TestFixPCIDeviceWithoutDataplaneClaimPasses(t *testing.T) {
	c := validBase()
	c.VirtualMachineFunctions[0].PCIDevices = []string{"0000:13:00.0"}
	if c.Vpp.DPDK.PerDev[0].Interface != "ens2f0" {
		t.Fatalf("前置：基准配置的 DPDK 端口应是按口名声明的 ens2f0")
	}
	if errs := Validate(c); len(errs) != 0 {
		t.Fatalf("无数据面占用的 PCI 设备不应报错: %v", errs)
	}
}

// 可达形态：把**端口名**填进 pci-device（`pci-device ens2f0`）。接口名校验不允许冒号，
// 故 BDF 进不了端口位置；反过来「端口名误填进 BDF 字段」却是随手可写的。
// 报错必须说清「这是端口名、不是 PCI 地址」、谁占着、以及照做路径（不能只说格式非法）。
func TestFixPortNameAsPCIDeviceRejected(t *testing.T) {
	c := validBase()
	c.VirtualMachineFunctions[0].PCIDevices = []string{"ens2f0"} // 该口正是 vpp.dpdk.per_dev 的端口

	errs := Validate(c)
	got := findPCIError(errs)
	if got == nil {
		t.Fatalf("把数据面端口名当 pci-device 应被拒绝，实际无相关报错: %v", errs)
	}
	for _, want := range []string{
		"ens2f0",              // 点名的标识
		"不是 PCI 地址",           // 说清性质（不是只说格式非法）
		"VPP/DPDK 接管",         // 谁占着
		"request vpp restart", // 照做路径
	} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("报错文案缺少 %q:\n%s", want, got.Message)
		}
	}

	// 反证：同名的自由口（未被任何数据面角色占用）填进 pci-device 时**不**走这条文案，
	// 仍是原有的「格式非法」——本检查不把「所有非 BDF 写法」都归为占用。
	c2 := validBase()
	c2.VirtualMachineFunctions[0].PCIDevices = []string{"ens9f9"}
	errs2 := Validate(c2)
	got2 := findPCIError(errs2)
	if got2 == nil {
		t.Fatalf("非 BDF 写法仍应报格式错误: %v", errs2)
	}
	if strings.Contains(got2.Message, "已被数据面占用") {
		t.Fatalf("未被占用的口名不应报「数据面占用」: %s", got2.Message)
	}
}

// 既有行为不回归：同 VM 内重复、跨 VM 同 BDF 仍按原判据与文案拒绝。
func TestFixPCIDeviceExistingBehaviorsPreserved(t *testing.T) {
	t.Run("same-vm-duplicate", func(t *testing.T) {
		c := validBase()
		c.VirtualMachineFunctions[0].PCIDevices = []string{"0000:03:00.0", "03:00.0"}
		if errs := Validate(c); !hasError(errs, "pci_devices[1]", "重复声明") {
			t.Fatalf("同 VM 内重复 BDF 应报「重复声明」: %v", errs)
		}
	})

	t.Run("cross-vm-conflict", func(t *testing.T) {
		c := validBase()
		c.VirtualMachineFunctions = append(c.VirtualMachineFunctions, vmWithPCI("probe-vm", "0000:03:00.0"))
		c.VirtualMachineFunctions[0].PCIDevices = []string{"03:00.0"}
		// 既有判据：报在**后声明者**（probe-vm）上，文案点名两台 VM 与归一后的设备。
		if errs := Validate(c); !hasError(errs, "virtual-machine-functions[probe-vm].pci_devices[0]", "已声明给 VM fw-vm") {
			t.Fatalf("跨 VM 冲突应点名两台 VM 与设备（既有文案）: %v", errs)
		}
	})
}

// findPCIError 取第一条指向 VM pci_devices 的报错。
func findPCIError(errs []ValidateError) *ValidateError {
	for i := range errs {
		if strings.Contains(errs[i].Path, ".pci_devices[") {
			return &errs[i]
		}
	}
	return nil
}

func hasError(errs []ValidateError, pathPart, msgPart string) bool {
	for _, e := range errs {
		if strings.Contains(e.Path, pathPart) && strings.Contains(e.Message, msgPart) {
			return true
		}
	}
	return false
}
