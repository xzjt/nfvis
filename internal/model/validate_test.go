package model

import (
	"strings"
	"testing"
)

func mustNoErr(t *testing.T, errs []ValidateError) {
	t.Helper()
	if len(errs) != 0 {
		t.Fatalf("期望校验通过，实际 %d 个错误: %v", len(errs), errs)
	}
}

func mustErrContaining(t *testing.T, errs []ValidateError, pathPart, msgPart string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e.Path, pathPart) && strings.Contains(e.Message, msgPart) {
			return
		}
	}
	t.Fatalf("期望包含 path~%q message~%q 的错误，实际: %v", pathPart, msgPart, errs)
}

func validBase() Config {
	enabled := true
	return Config{
		System: &SystemConfig{
			Hostname:   "nfvis-node1",
			Management: &MgmtConfig{Address: "192.168.1.10/24", Gateway: "192.168.1.1"},
		},
		Interfaces: []InterfaceConfig{{Name: "ens2f0", MTU: 9000, Enabled: &enabled}},
		ResourcePools: &ResourcePool{
			Hugepages: []HPool{{PageSize: "1G", Count: 32}},
			CPU:       &CPUSetup{IsolatedCores: []int{4, 5, 6, 7}},
		},
		Vpp: &VppConfig{
			CPU:    &VppCPU{MainCore: 4, CorelistWorkers: "5,6"},
			Memory: &VppMemory{HugepagePreference: "1G"},
			DPDK:   &VppDPDK{PerDev: []VppDevOverride{{Interface: "ens2f0", RxQueues: 4}}},
		},
		VirtualSwitches: []VirtualSwitch{{
			Name: "vs-app", Type: "l2", VlanAccess: 100,
			Gateway: &VSGateway{Addresses: []string{"192.168.100.1/24"}},
			Ports:   []VSwitchPort{{Seq: 1, Interface: "ens2f0"}},
		}},
		Vrfs: []Vrf{{Name: "vs-mgmt", L3Interfaces: []L3Interface{{Interface: "ens2f0.100", Addresses: []string{"10.10.0.1/24"}}}, Routes: []Route{{Prefix: "0.0.0.0/0", NextHop: "10.10.0.254"}}}},
		Acls: []Acl{{Name: "acl-web", Rules: []AclRule{{Seq: 10, Direction: "ingress", Source: "any", Destination: "any", Protocol: "tcp", Action: "permit"}}}},
		VirtualMachineFunctions: []VMFunction{{
			Name: "fw-vm", Image: "ubuntu22-vm",
			VCPU:   VMCpu{Count: 4},
			Memory: VMMemory{SizeMB: 8192, HugepageSize: "1G"},
			Interfaces: []VnfInterface{
				{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-app", MAC: "52:54:00:aa:00:01"},
			},
		}},
		ContainerFunctions: []ContainerFunction{{
			Name: "sbc-ct1", Image: "alpine-ct",
			Interfaces: []VnfInterface{{Name: "eth0", Type: "memif", VirtualSwitch: "vs-app"}},
		}},
	}
}

func TestValidatePass(t *testing.T) {
	mustNoErr(t, Validate(validBase()))
	mustNoErr(t, Validate(Config{})) // 空配置合法（增量编辑的中间态）
}

func TestValidateNameSyntaxAndDuplicates(t *testing.T) {
	c := validBase()
	c.VirtualMachineFunctions = append(c.VirtualMachineFunctions, VMFunction{Name: "bad name!", Image: "x", VCPU: VMCpu{Count: 1}, Memory: VMMemory{SizeMB: 1}})
	c.VirtualSwitches = append(c.VirtualSwitches, VirtualSwitch{Name: "vs-app", Type: "l2"})
	errs := Validate(c)
	mustErrContaining(t, errs, "bad name!", "名称")
	mustErrContaining(t, errs, "vs-app", "重复")

	long := strings.Repeat("a", 65)
	c2 := Config{VirtualMachineFunctions: []VMFunction{{Name: long, Image: "x", VCPU: VMCpu{Count: 1}, Memory: VMMemory{SizeMB: 1}}}}
	mustErrContaining(t, Validate(c2), long, "名称")
}

func TestValidateEnumAndFormat(t *testing.T) {
	c := validBase()
	c.VirtualSwitches[0].Type = "l4"
	c.VirtualMachineFunctions[0].Memory.Backing = "swap"
	c.VirtualMachineFunctions[0].Interfaces[0].MAC = "not-a-mac"
	c.QosPolicies = []QosPolicy{{Name: "p", Cir: -1, Cbs: 0}}
	c.System.Management.Address = "192.168.1.10" // 缺掩码
	c.Vrfs[0].Routes[0].NextHop = "10.10.0.999"
	errs := Validate(c)
	mustErrContaining(t, errs, "vs-app", "type")
	mustErrContaining(t, errs, "backing", "backing")
	mustErrContaining(t, errs, "eth0", "MAC")
	mustErrContaining(t, errs, "p", "cir")
	mustErrContaining(t, errs, "management", "ip-prefix")
	mustErrContaining(t, errs, "next_hop", "ip")

	// vlan 越界
	c2 := validBase()
	c2.VirtualSwitches[0].VlanAccess = 4095
	mustErrContaining(t, Validate(c2), "vlan_access", "vlan")

	// lacp 枚举
	c3 := validBase()
	c3.Bonds = []Bond{{Name: "bond0", Members: []string{"ens2f0"}, Lacp: &Lacp{Mode: "eager"}}}
	mustErrContaining(t, Validate(c3), "bond0", "lacp")
}

func TestValidateVMRequiredFields(t *testing.T) {
	c := validBase()
	c.VirtualMachineFunctions[0].Image = ""
	c.VirtualMachineFunctions[0].VCPU.Count = 0
	c.VirtualMachineFunctions[0].Memory.SizeMB = 0
	errs := Validate(c)
	mustErrContaining(t, errs, "image", "必填")
	mustErrContaining(t, errs, "vcpu", "必填")
	mustErrContaining(t, errs, "size_mb", "必填")
}

func TestValidateReferences(t *testing.T) {
	// vNIC 引用不存在的交换机
	c := validBase()
	c.VirtualMachineFunctions[0].Interfaces[0].VirtualSwitch = "vs-ghost"
	mustErrContaining(t, Validate(c), "eth0", "虚拟交换机")

	// 端口引用不存在的物理口
	c2 := validBase()
	c2.VirtualSwitches[0].Ports[0].Interface = "ens9f9"
	mustErrContaining(t, Validate(c2), "ports", "ens9f9")

	// 交换机端口引用不存在的 VNF
	c3 := validBase()
	c3.VirtualSwitches[0].Ports = append(c3.VirtualSwitches[0].Ports, VSwitchPort{Seq: 2, Vnf: "ghost-vm", VnfInterface: "eth0"})
	mustErrContaining(t, Validate(c3), "ports[2]", "ghost-vm")

	// 端口 ACL 引用不存在的 ACL
	c4 := validBase()
	c4.VirtualSwitches[0].Ports[0].AclIn = "acl-ghost"
	mustErrContaining(t, Validate(c4), "acl_in", "acl-ghost")

	// 网关显式 VRF 引用不存在
	c5 := validBase()
	c5.VirtualSwitches[0].Gateway.Vrf = "vr-ghost"
	mustErrContaining(t, Validate(c5), "gateway.vrf", "vr-ghost")

	// QoS 绑定引用不存在的策略
	c6 := validBase()
	c6.Interfaces[0].IngressPolicy = "pol-ghost"
	mustErrContaining(t, Validate(c6), "ingress_policy", "pol-ghost")
}

func TestValidateFRConfig011Rules(t *testing.T) {
	// ① vhost-user 必须 hugepage backing（FR-CFG-011①）
	c := validBase()
	c.VirtualMachineFunctions[0].Memory.Backing = "normal"
	mustErrContaining(t, Validate(c), "fw-vm", "vhost-user")

	// ③ MAC 不得重复（FR-CFG-011③）
	c2 := validBase()
	c2.VirtualMachineFunctions[0].Interfaces = append(c2.VirtualMachineFunctions[0].Interfaces,
		VnfInterface{Name: "eth1", Type: "vhost-user", VirtualSwitch: "vs-app", MAC: "52:54:00:aa:00:01"})
	mustErrContaining(t, Validate(c2), "eth1", "MAC")

	// ③ 跨 VNF MAC 不得重复（FR-CFG-011③：全部 VNF 共享命名空间）
	c2b := validBase()
	c2b.ContainerFunctions = append(c2b.ContainerFunctions, ContainerFunction{
		Name: "sbc-ct2", Image: "alpine-ct",
		Interfaces: []VnfInterface{{Name: "eth0", Type: "memif", VirtualSwitch: "vs-app", MAC: "52:54:00:aa:00:01"}},
	})
	mustErrContaining(t, Validate(c2b), "sbc-ct2", "重复")

	// ④ native VLAN 与 trunk 允许列表冲突（FR-CFG-011④）
	c2c := validBase()
	c2c.VirtualSwitches[0].Ports[0].TrunkVlans = []int{100, 200}
	c2c.VirtualSwitches[0].Ports[0].NativeVlan = 200
	mustErrContaining(t, Validate(c2c), "native", "冲突")

	// ④ 交换机 access VLAN 须在 trunk 允许列表内（FR-CFG-011④）
	c2d := validBase()
	c2d.VirtualSwitches[0].Ports[0].TrunkVlans = []int{200}
	mustErrContaining(t, Validate(c2d), "ports[1]", "trunk 允许列表")

	// ⑧ dpdk dev 覆盖必须是 DPDK 物理口（FR-CFG-011⑧）
	c3 := validBase()
	c3.Vpp.DPDK.PerDev = []VppDevOverride{{Interface: "bond0", RxQueues: 2}}
	c3.Bonds = []Bond{{Name: "bond0", Members: []string{"ens2f0"}}}
	mustErrContaining(t, Validate(c3), "bond0", "物理口")

	// ② 地址不得与既有 L3 接口冲突/重叠（FR-CFG-011②）
	c4 := validBase()
	c4.VirtualSwitches[0].Gateway = &VSGateway{Addresses: []string{"10.10.0.77/24"}}
	c4.Vrfs[0].L3Interfaces[0].Addresses = []string{"10.10.0.1/24"}
	mustErrContaining(t, Validate(c4), "gateway", "重叠")
}

func TestValidateFRSys010Rules(t *testing.T) {
	// vpp 核必须在隔离核池内（FR-SYS-010）
	c := validBase()
	c.Vpp.CPU.CorelistWorkers = "5,9"
	mustErrContaining(t, Validate(c), "corelist_workers", "隔离核")

	// hugepage-preference 必须与资源池页大小一致（FR-SYS-010）
	c2 := validBase()
	c2.Vpp.Memory.HugepagePreference = "2M"
	mustErrContaining(t, Validate(c2), "hugepage_preference", "一致")

	// workers_per_numa 与 corelist_workers 互斥
	c3 := validBase()
	c3.Vpp.CPU.WorkersPerNuma = 2
	mustErrContaining(t, Validate(c3), "workers_per_numa", "互斥")

	// VM 指定页大小必须有对应资源池（FR-CFG-011⑪ 池存在性部分）
	c4 := validBase()
	c4.VirtualMachineFunctions[0].Memory.HugepageSize = "2M"
	mustErrContaining(t, Validate(c4), "hugepage_size", "资源池")
}

func TestValidateL3SwitchMapping(t *testing.T) {
	// type=l3 的交换机必须有同名 Vrf 条目承载 L3 数据（附录 B 映射）
	c := validBase()
	c.VirtualSwitches = append(c.VirtualSwitches, VirtualSwitch{Name: "vs-l3", Type: "l3"})
	mustErrContaining(t, Validate(c), "vs-l3", "VRF")

	// 补上同名 Vrf 后通过
	c.Vrfs = append(c.Vrfs, Vrf{Name: "vs-l3"})
	mustNoErr(t, Validate(c))

	// l3 交换机不得携带 L2 专属配置
	c2 := validBase()
	c2.VirtualSwitches = append(c2.VirtualSwitches, VirtualSwitch{Name: "vs-l3", Type: "l3", VlanAccess: 10})
	c2.Vrfs = append(c2.Vrfs, Vrf{Name: "vs-l3"})
	mustErrContaining(t, Validate(c2), "vs-l3", "L2")
}
