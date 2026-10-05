package model

import (
	"fmt"
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

// 决策 #381：静态路由多下一跳（ECMP）校验。
//
// 单值写法与语义不变（既有用例覆盖）；多值逐元素校验：空元素/坏 IP/重复/混族/超 8 拒绝。
func TestValidateRouteMultiNextHop(t *testing.T) {
	setNH := func(nh string) []ValidateError {
		c := validBase()
		c.Vrfs[0].Routes[0].NextHop = nh
		return Validate(c)
	}

	// 逗号分隔的多值（同族 IPv4）通过。
	mustNoErr(t, setNH("10.10.0.254,10.10.0.253"))

	// 空元素（尾随逗号）拒绝。
	mustErrContaining(t, setNH("10.10.0.254,"), "next_hop", "为空")

	// 第 2 个不是有效 IP。
	mustErrContaining(t, setNH("10.10.0.254,10.10.0.999"), "next_hop", "不是有效 ip")

	// 重复下一跳。
	mustErrContaining(t, setNH("10.10.0.254,10.10.0.254"), "next_hop", "重复")

	// 混族（IPv4 与 IPv6 混用）。
	mustErrContaining(t, setNH("10.10.0.254,2001:db8::1"), "next_hop", "不得混用")

	// 超过 8 个。
	mustErrContaining(t, setNH("10.0.0.1,10.0.0.2,10.0.0.3,10.0.0.4,10.0.0.5,10.0.0.6,10.0.0.7,10.0.0.8,10.0.0.9"),
		"next_hop", "最多 8 个")

	// 单值坏 IP：文案与既有等价（未引入逗号时的旧形态）。
	mustErrContaining(t, setNH("10.10.0.999"), "next_hop", "必须是有效 ip")
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

	// L3 接口 ACL 引用不存在的 ACL（端口级绑定已被决策 #340 硬拒，改由 L3 接口形态覆盖本引用校验）
	c4 := validBase()
	c4.Vrfs[0].L3Interfaces[0].AclIn = "acl-ghost"
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

func TestValidateGatewayACLRejected(t *testing.T) {
	// 决策 #340：网关上绑 ACL 在提交期硬拒（真机实证 VPP 26.06 不评估 BVI/网关上的域内流量，
	// 既不拦截也不计数），文案须指向已实证生效的 L3 接口形态。
	c := validBase()
	c.VirtualSwitches[0].Gateway.AclIn = "acl-web"
	mustErrContaining(t, Validate(c), "gateway.acl_in", "l3-interface")

	c2 := validBase()
	c2.VirtualSwitches[0].Gateway.AclOut = "acl-web"
	mustErrContaining(t, Validate(c2), "gateway.acl_out", "l3-interface")

	// 决策边界：L3 接口形态（L3Interface.AclIn）保持可用，不因本决策受影响。
	c3 := validBase()
	c3.Vrfs[0].L3Interfaces[0].AclIn = "acl-web"
	mustNoErr(t, Validate(c3))

	// 网关不设 ACL 时不报错（其余网关配置照常可用）。
	c4 := validBase()
	mustNoErr(t, Validate(c4))
}

func TestValidatePortACLRejected(t *testing.T) {
	// 决策 #340 修订（round119 补验）：交换机端口级 acl-in/acl-out 与网关同口径硬拒——
	// 真机实证 VPP 26.06 不评估 L2 路径（成员端口）上的 ACL，既不拦截也不计数；
	// 文案须指向已实证生效的 L3 接口形态。
	c := validBase()
	c.VirtualSwitches[0].Ports[0].AclIn = "acl-web"
	mustErrContaining(t, Validate(c), "ports[1].acl_in", "l3-interface")

	c2 := validBase()
	c2.VirtualSwitches[0].Ports[0].AclOut = "acl-web"
	mustErrContaining(t, Validate(c2), "ports[1].acl_out", "l3-interface")

	// 端口不设 ACL 时不受影响（既有配置照常通过）。
	c3 := validBase()
	mustNoErr(t, Validate(c3))
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

func TestValidateDhcpRelay(t *testing.T) {
	// 合法：L2 交换机已配 IPv4 网关（中继源自动取 BVI v4 地址）
	c := validBase()
	c.VirtualSwitches[0].DhcpRelayServer = "192.168.100.2"
	mustNoErr(t, Validate(c))

	// server 必须是合法 IPv4 地址
	c1 := validBase()
	c1.VirtualSwitches[0].DhcpRelayServer = "not-an-ip"
	mustErrContaining(t, Validate(c1), "dhcp_relay_server", "IPv4")
	c1b := validBase()
	c1b.VirtualSwitches[0].DhcpRelayServer = "2001:db8::1"
	mustErrContaining(t, Validate(c1b), "dhcp_relay_server", "IPv4")

	// 未配网关拒绝：文案指向先 set gateway ip
	c2 := validBase()
	c2.VirtualSwitches[0].Gateway = nil
	c2.VirtualSwitches[0].DhcpRelayServer = "192.168.100.2"
	mustErrContaining(t, Validate(c2), "dhcp_relay_server", "gateway ip")

	// 网关只有 IPv6 地址同样拒绝（中继源需要 v4）
	c3 := validBase()
	c3.VirtualSwitches[0].Gateway = &VSGateway{Addresses: []string{"2001:db8:100::1/64"}}
	c3.VirtualSwitches[0].DhcpRelayServer = "192.168.100.2"
	mustErrContaining(t, Validate(c3), "dhcp_relay_server", "gateway ip")

	// type=l3 交换机没有 BVI 网关，relay 一律拒绝
	c4 := validBase()
	c4.VirtualSwitches = append(c4.VirtualSwitches, VirtualSwitch{Name: "vs-l3", Type: "l3", DhcpRelayServer: "10.0.0.1"})
	c4.Vrfs = append(c4.Vrfs, Vrf{Name: "vs-l3"})
	mustErrContaining(t, Validate(c4), "vs-l3", "BVI")
}

func TestValidateDHCPServer(t *testing.T) {
	// 合法：L2 + IPv4 BVI（192.168.100.1/24），池在子网内且不含 BVI/网络/广播地址
	enabled := func(mut func(*VirtualSwitch)) Config {
		c := validBase()
		c.VirtualSwitches[0].DhcpServerPoolStart = "192.168.100.100"
		c.VirtualSwitches[0].DhcpServerPoolEnd = "192.168.100.200"
		mut(&c.VirtualSwitches[0])
		return c
	}
	mustNoErr(t, Validate(enabled(func(*VirtualSwitch) {})))
	// 只配可选叶子、未配 pool：不启用但合法（随 pool 生效；口径见 validate.go）
	mustNoErr(t, Validate(enabled(func(s *VirtualSwitch) {
		s.DhcpServerPoolStart, s.DhcpServerPoolEnd = "", ""
		s.DhcpServerLeaseTimeSeconds = 600
		s.DhcpServerDNS = "8.8.8.8"
		s.DhcpServerDomainName = "lab.local"
	})))

	// 非法取值域：lease-time 越界、dns 非 v4、域名带空白
	mustErrContaining(t, Validate(enabled(func(s *VirtualSwitch) { s.DhcpServerLeaseTimeSeconds = 59 })),
		"dhcp_server_lease_time_seconds", "60")
	mustErrContaining(t, Validate(enabled(func(s *VirtualSwitch) { s.DhcpServerLeaseTimeSeconds = 2592001 })),
		"dhcp_server_lease_time_seconds", "2592000")
	mustErrContaining(t, Validate(enabled(func(s *VirtualSwitch) { s.DhcpServerDNS = "2001:db8::1" })),
		"dhcp_server_dns", "IPv4")
	mustErrContaining(t, Validate(enabled(func(s *VirtualSwitch) { s.DhcpServerDomainName = "a b" })),
		"dhcp_server_domain_name", "空白")

	// 池端点必须同时给
	mustErrContaining(t, Validate(enabled(func(s *VirtualSwitch) { s.DhcpServerPoolEnd = "" })),
		"dhcp_server_pool_start", "同时给出")
	mustErrContaining(t, Validate(enabled(func(s *VirtualSwitch) { s.DhcpServerPoolStart, s.DhcpServerPoolEnd = "", "192.168.100.200" })),
		"dhcp_server_pool_start", "同时给出")

	// start > end / 非法地址
	mustErrContaining(t, Validate(enabled(func(s *VirtualSwitch) {
		s.DhcpServerPoolStart, s.DhcpServerPoolEnd = "192.168.100.200", "192.168.100.100"
	})),
		"dhcp_server_pool_start", "start ≤ end")
	mustErrContaining(t, Validate(enabled(func(s *VirtualSwitch) { s.DhcpServerPoolEnd = "not-an-ip" })),
		"dhcp_server_pool_start", "合法 IPv4")

	// 不同子网
	mustErrContaining(t, Validate(enabled(func(s *VirtualSwitch) { s.DhcpServerPoolStart, s.DhcpServerPoolEnd = "192.168.101.1", "192.168.101.9" })),
		"dhcp_server_pool_start", "同一子网")

	// 含 BVI 地址 / 网络地址 / 广播地址
	mustErrContaining(t, Validate(enabled(func(s *VirtualSwitch) { s.DhcpServerPoolStart, s.DhcpServerPoolEnd = "192.168.100.1", "192.168.100.9" })),
		"dhcp_server_pool_start", "BVI 网关地址")
	mustErrContaining(t, Validate(enabled(func(s *VirtualSwitch) { s.DhcpServerPoolStart, s.DhcpServerPoolEnd = "192.168.100.0", "192.168.100.9" })),
		"dhcp_server_pool_start", "网络地址")
	mustErrContaining(t, Validate(enabled(func(s *VirtualSwitch) {
		s.DhcpServerPoolStart, s.DhcpServerPoolEnd = "192.168.100.250", "192.168.100.255"
	})),
		"dhcp_server_pool_start", "广播地址")

	// 超上限：4096 个地址合法（含两端），4097 拒绝
	mustNoErr(t, Validate(enabled(func(s *VirtualSwitch) { s.DhcpServerPoolStart, s.DhcpServerPoolEnd = "192.168.100.2", "192.168.100.2" })))
	big := validBase()
	big.VirtualSwitches[0].Gateway = &VSGateway{Addresses: []string{"10.0.0.1/19"}}
	big.VirtualSwitches[0].DhcpServerPoolStart, big.VirtualSwitches[0].DhcpServerPoolEnd = "10.0.1.1", "10.0.17.1" // 4097 个
	mustErrContaining(t, Validate(big), "dhcp_server_pool_start", "4096")
	big.VirtualSwitches[0].DhcpServerPoolEnd = "10.0.17.0" // 恰 4096 个
	mustNoErr(t, Validate(big))

	// 前置：未配网关 / 仅 IPv6 网关 / type=l3 一律拒绝
	noGW := enabled(func(s *VirtualSwitch) { s.Gateway = nil })
	mustErrContaining(t, Validate(noGW), "dhcp_server_pool_start", "gateway ip")
	v6GW := enabled(func(s *VirtualSwitch) { s.Gateway = &VSGateway{Addresses: []string{"2001:db8:100::1/64"}} })
	mustErrContaining(t, Validate(v6GW), "dhcp_server_pool_start", "gateway ip")
	l3 := validBase()
	l3.VirtualSwitches = append(l3.VirtualSwitches, VirtualSwitch{
		Name: "vs-l3", Type: "l3", DhcpServerPoolStart: "10.0.0.10", DhcpServerPoolEnd: "10.0.0.20"})
	l3.Vrfs = append(l3.Vrfs, Vrf{Name: "vs-l3"})
	mustErrContaining(t, Validate(l3), "vs-l3", "BVI")

	// 与 dhcp-relay 互斥
	mustErrContaining(t, Validate(enabled(func(s *VirtualSwitch) { s.DhcpRelayServer = "192.168.100.2" })),
		"dhcp_server_pool_start", "UDP/67")
}

// 决策 #368（收口 R142-5）：DHCP relay 与 server 的**跨交换机全局互斥**。
func TestValidateDHCPRelayServerCrossSwitchExclusive(t *testing.T) {
	// twoSwitch 返回两台 L2 交换机（各自独立子网网关）：vs-app（192.168.100.1/24）与 vs-edge（192.168.200.1/24）。
	twoSwitch := func() Config {
		c := validBase()
		c.VirtualSwitches = append(c.VirtualSwitches, VirtualSwitch{
			Name: "vs-edge", Type: "l2",
			Gateway: &VSGateway{Addresses: []string{"192.168.200.1/24"}},
		})
		return c
	}

	// ① A 域 relay + B 域 server → 拒绝，报错点名两侧交换机与全局归属机理
	c1 := twoSwitch()
	c1.VirtualSwitches[0].DhcpRelayServer = "192.168.100.2"
	c1.VirtualSwitches[1].DhcpServerPoolStart, c1.VirtualSwitches[1].DhcpServerPoolEnd = "192.168.200.10", "192.168.200.20"
	errs := Validate(c1)
	if len(errs) != 1 {
		t.Fatalf("跨交换机并存应恰好一条错误，得 %d: %v", len(errs), errs)
	}
	msg := errs[0].Error()
	for _, want := range []string{"跨交换机", "vs-app", "vs-edge", "UDP/67", "留其一"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误文案应含 %q，实得: %s", want, msg)
		}
	}

	// ② 反向（A 域 server + B 域 relay）同样拒绝
	c2 := twoSwitch()
	c2.VirtualSwitches[0].DhcpServerPoolStart, c2.VirtualSwitches[0].DhcpServerPoolEnd = "192.168.100.10", "192.168.100.20"
	c2.VirtualSwitches[1].DhcpRelayServer = "192.168.200.2"
	mustErrContaining(t, Validate(c2), "vs-edge", "跨交换机")

	// ③ 同交换机双配 → 只报既有那条（#359 文案），不重复报跨交换机
	c3 := validBase()
	c3.VirtualSwitches[0].DhcpServerPoolStart, c3.VirtualSwitches[0].DhcpServerPoolEnd = "192.168.100.100", "192.168.100.200"
	c3.VirtualSwitches[0].DhcpRelayServer = "192.168.100.2"
	errs3 := Validate(c3)
	if len(errs3) != 1 {
		t.Fatalf("同交换机双配应恰好一条错误（不重复），得 %d: %v", len(errs3), errs3)
	}
	if strings.Contains(errs3[0].Error(), "跨交换机") {
		t.Fatalf("同交换机双配不该报跨交换机文案: %s", errs3[0])
	}
	if !strings.Contains(errs3[0].Error(), "同一交换机不能同时配置") {
		t.Fatalf("同交换机双配应报既有文案: %s", errs3[0])
	}

	// ④ relay-only 多域（两台都 relay）→ 放行（per-FIB 代理可并存）
	c4 := twoSwitch()
	c4.VirtualSwitches[0].DhcpRelayServer = "192.168.100.2"
	c4.VirtualSwitches[1].DhcpRelayServer = "192.168.200.2"
	mustNoErr(t, Validate(c4))

	// ⑤ server-only 多域（两台都 server）→ 放行（共享同一 punt socket、按域 demux）
	c5 := twoSwitch()
	c5.VirtualSwitches[0].DhcpServerPoolStart, c5.VirtualSwitches[0].DhcpServerPoolEnd = "192.168.100.10", "192.168.100.20"
	c5.VirtualSwitches[1].DhcpServerPoolStart, c5.VirtualSwitches[1].DhcpServerPoolEnd = "192.168.200.10", "192.168.200.20"
	mustNoErr(t, Validate(c5))

	// ⑥ 删一侧即放行（单向撤销的可照做路径）
	c6 := twoSwitch()
	c6.VirtualSwitches[0].DhcpRelayServer = "192.168.100.2"
	c6.VirtualSwitches[1].DhcpServerPoolStart, c6.VirtualSwitches[1].DhcpServerPoolEnd = "192.168.200.10", "192.168.200.20"
	mustErrContaining(t, Validate(c6), "vs-app", "留其一")
	c6.VirtualSwitches[1].DhcpServerPoolStart, c6.VirtualSwitches[1].DhcpServerPoolEnd = "", ""
	mustNoErr(t, Validate(c6))
}

func TestDHCPServerPoolHelpers(t *testing.T) {
	// 区间与规模（纯函数与校验/数据面共用，边界逐个钉住）
	if lo, hi, ok := DHCPServerPoolRange("192.168.100.100", "192.168.100.200"); !ok || lo > hi {
		t.Fatalf("合法池应解析成功: %v %v %v", lo, hi, ok)
	}
	if _, _, ok := DHCPServerPoolRange("192.168.100.200", "192.168.100.100"); ok {
		t.Fatal("start>end 应解析失败")
	}
	if _, _, ok := DHCPServerPoolRange("not-an-ip", "192.168.100.100"); ok {
		t.Fatal("非法地址应解析失败")
	}
	if got := DHCPServerPoolSize("192.168.100.100", "192.168.100.200"); got != 101 {
		t.Fatalf("池规模应为 101，实得 %d", got)
	}
	if got := DHCPServerPoolSize("192.168.100.1", "192.168.100.1"); got != 1 {
		t.Fatalf("单地址池规模应为 1，实得 %d", got)
	}

	// 生效取值：未配置回落缺省；配置优先
	vs := VirtualSwitch{}
	if vs.DHCPServerEnabled() {
		t.Fatal("无 pool 不启用")
	}
	if vs.DHCPServerLeaseSeconds() != DefaultDHCPServerLeaseSeconds {
		t.Fatalf("缺省租约时长应为 %d", DefaultDHCPServerLeaseSeconds)
	}
	vs.DhcpServerPoolStart, vs.DhcpServerPoolEnd = "10.0.0.10", "10.0.0.20"
	vs.DhcpServerLeaseTimeSeconds = 600
	if !vs.DHCPServerEnabled() || vs.DHCPServerLeaseSeconds() != 600 {
		t.Fatalf("pool 齐备应启用且租约时长取配置值: %+v", vs)
	}

	// 网关 IPv4 取首个 v4 地址与网段（v6 在前的声明也要跳过）
	vs.Gateway = &VSGateway{Addresses: []string{"2001:db8::1/64", "192.168.5.1/24"}}
	ip, n, ok := vs.GatewayIPv4()
	if !ok || ip.String() != "192.168.5.1" || n.String() != "192.168.5.0/24" {
		t.Fatalf("GatewayIPv4 应取首个 v4 地址: %v %v %v", ip, n, ok)
	}
}

func TestValidateLearnLimit(t *testing.T) {
	// 未配置（0）合法；合法范围内的正整数合法
	mustNoErr(t, Validate(validBase()))
	c := validBase()
	c.VirtualSwitches[0].LearnLimit = 8192
	mustNoErr(t, Validate(c))
	c1 := validBase()
	c1.VirtualSwitches[0].LearnLimit = 16777216
	mustNoErr(t, Validate(c1))

	// 负数 / 0 显式（用负值与越界值覆盖边界；0 即未配置、不报错）
	neg := validBase()
	neg.VirtualSwitches[0].LearnLimit = -1
	mustErrContaining(t, Validate(neg), "learn_limit", "1-16777216")

	// 超过 VPP 上限
	over := validBase()
	over.VirtualSwitches[0].LearnLimit = 16777217
	mustErrContaining(t, Validate(over), "learn_limit", "1-16777216")

	// type=l3 交换机不允许 L2 专属配置（含 learn_limit）——避免「设了却不生效」的静默假成功
	c3 := validBase()
	c3.VirtualSwitches = append(c3.VirtualSwitches, VirtualSwitch{Name: "vs-l3", Type: "l3", LearnLimit: 100})
	c3.Vrfs = append(c3.Vrfs, Vrf{Name: "vs-l3"})
	mustErrContaining(t, Validate(c3), "vs-l3", "L2")
}

// 决策 #345：数据面 DNS 代理上游（全局 vpp.dns_proxy_servers + 按域 vs.dns_proxy_servers）
// 逐条须为合法 IP（v4/v6），空串拒绝；条数不设上限。
func TestValidateDNSProxyUpstreams(t *testing.T) {
	// 未配置（nil/空）合法
	mustNoErr(t, Validate(validBase()))

	// 全局：合法 v4/v6、可多条
	c := validBase()
	c.Vpp.DNSProxyServers = []string{"8.8.8.8", "2001:4860:4860::8888", "1.1.1.1"}
	mustNoErr(t, Validate(c))

	// 全局：非法 IP 与空串拒绝
	bad := validBase()
	bad.Vpp.DNSProxyServers = []string{"8.8.8.8", "not-an-ip"}
	mustErrContaining(t, Validate(bad), "vpp.dns_proxy_servers[1]", "有效 IP")
	empty := validBase()
	empty.Vpp.DNSProxyServers = []string{""}
	mustErrContaining(t, Validate(empty), "vpp.dns_proxy_servers[0]", "有效 IP")

	// 按域：合法 v4/v6
	vs := validBase()
	vs.VirtualSwitches[0].DNSProxyServers = []string{"10.0.0.53", "2001:db8::53"}
	mustNoErr(t, Validate(vs))

	// 按域：非法拒绝（L3 交换机也可配按域上游——转发域是其 l3-interface，不要求 BVI 网关）
	l3 := validBase()
	l3.VirtualSwitches = append(l3.VirtualSwitches, VirtualSwitch{Name: "vs-l3", Type: "l3", DNSProxyServers: []string{"2001:db8::1"}})
	l3.Vrfs = append(l3.Vrfs, Vrf{Name: "vs-l3"})
	mustNoErr(t, Validate(l3))

	badVS := validBase()
	badVS.VirtualSwitches[0].DNSProxyServers = []string{"10.0.0.999"}
	mustErrContaining(t, Validate(badVS), "dns_proxy_servers[0]", "有效 IP")

	// 条数不设上限：多条合法
	many := validBase()
	for i := 0; i < 40; i++ {
		many.Vpp.DNSProxyServers = append(many.Vpp.DNSProxyServers, fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}
	mustNoErr(t, Validate(many))
}

func TestValidateSystemLogin(t *testing.T) {
	c := validBase()
	c.System.Login = &SystemLogin{
		Users: []LoginUserConfig{
			{Name: "admin", Class: "super-user", PasswordHash: testHash},
			{Name: "netop", Class: "custom-op", PasswordHash: testHash},
		},
		Classes:        []ClassDef{{Name: "custom-op", Allow: []string{"show"}}},
		PasswordPolicy: &PasswordPolicy{MinLength: 8, LockoutThreshold: 5, LockoutMinutes: 10},
	}
	mustNoErr(t, Validate(c))

	// 用户引用不存在的 class
	c2 := validBase()
	c2.System.Login = &SystemLogin{Users: []LoginUserConfig{{Name: "x", Class: "ghost", PasswordHash: testHash}}}
	mustErrContaining(t, Validate(c2), "users[x].class", "ghost")

	// 非法用户名 / 重复 class / 策略越界
	c3 := validBase()
	c3.System.Login = &SystemLogin{
		Users:          []LoginUserConfig{{Name: "bad name", PasswordHash: testHash}},
		Classes:        []ClassDef{{Name: "c1"}, {Name: "c1"}},
		PasswordPolicy: &PasswordPolicy{MinLength: 2},
	}
	errs := Validate(c3)
	mustErrContaining(t, errs, "bad name", "名称")
	mustErrContaining(t, errs, "c1", "重复")
	mustErrContaining(t, errs, "min_length", "范围")

	// R44-1 兜底：没有口令的用户不得提交（脱敏视图回写会由引擎继承哈希，新用户无从继承）
	c4 := validBase()
	c4.System.Login = &SystemLogin{Users: []LoginUserConfig{{Name: "nopass", Class: "super-user"}}}
	mustErrContaining(t, Validate(c4), "users[0]", "nopass")
}

// testHash 形态合法的口令哈希（校验只查"有没有"，不验内容）。
const testHash = "pbkdf2$sha256$600000$c2FsdA$hYXNo"

// 决策 #152：整文档提交的「至少留一个 super-user」兜底（判定函数本身）。
func TestCheckSuperUserPresent(t *testing.T) {
	withUsers := func(us ...LoginUserConfig) Config {
		c := validBase()
		c.System.Login = &SystemLogin{Users: us}
		return c
	}
	// 有一个 super-user 即通过（不管还有多少别的账号）
	if errs := CheckSuperUserPresent(withUsers(
		LoginUserConfig{Name: "admin", Class: "super-user", PasswordHash: testHash},
		LoginUserConfig{Name: "netop", Class: "operator", PasswordHash: testHash},
	)); len(errs) != 0 {
		t.Fatalf("有 super-user 应通过，实得 %+v", errs)
	}
	// 只有 operator / read-only：拒
	for _, cls := range []string{"operator", "read-only"} {
		errs := CheckSuperUserPresent(withUsers(LoginUserConfig{Name: "u", Class: cls, PasswordHash: testHash}))
		if len(errs) != 1 || errs[0].Path != "system.login.users" {
			t.Fatalf("class=%s 应恰好一条 system.login.users 错误，实得 %+v", cls, errs)
		}
		if !strings.Contains(errs[0].Message, "super-user") {
			t.Fatalf("报错要说明缺什么: %q", errs[0].Message)
		}
	}
	// class 为空按 read-only 算（与 aaa/api 的有效 class 判据一致）⇒ 不算 super-user
	u := LoginUserConfig{Name: "u", PasswordHash: testHash}
	if EffectiveClass(u) != "read-only" {
		t.Fatalf("class 为空的有效 class 应为 read-only，实得 %q", EffectiveClass(u))
	}
	if errs := CheckSuperUserPresent(withUsers(u)); len(errs) != 1 {
		t.Fatalf("class 为空的账号不算 super-user，应被拒，实得 %+v", errs)
	}
	// 空用户表 / 无 system 段 / 无 login 段：都拒（这正是"空配置提交"的形态）
	for name, c := range map[string]Config{
		"空用户表":     withUsers(),
		"无 login":  {System: &SystemConfig{Hostname: "n1"}},
		"无 system": {},
	} {
		if errs := CheckSuperUserPresent(c); len(errs) != 1 {
			t.Fatalf("%s 应被拒，实得 %+v", name, errs)
		}
	}
	// 消息里不得出现需求编号（决策 #86/#87：给操作者看的文本不写编号）
	errs := CheckSuperUserPresent(Config{})
	if len(errs) != 1 || strings.Contains(errs[0].Error(), "FR-") {
		t.Fatalf("报错文本不得含需求编号: %+v", errs)
	}
}

// T0-1（决策 #52）：NAT44 拓扑语义校验——出接口必填且须归属某个带地址的 VRF、
// source-pool 可选、单一 inside/outside 转发域。
func TestValidateNatTopology(t *testing.T) {
	natBase := func() Config {
		c := validBase()
		// 出接口只能用**未被数据面其它角色占用**的口：validBase 的 ens2f0 是 vs-app 的
		// 交换机端口，再把它当 NAT outside 的 L3 接口会被角色互斥校验拒绝。
		c.Interfaces = append(c.Interfaces, InterfaceConfig{Name: "ens2f1"})
		c.VirtualSwitches = append(c.VirtualSwitches, VirtualSwitch{Name: "vs-l3", Type: "l3"})
		// inside：vs-l3 的 l3-interface
		c.Vrfs = append(c.Vrfs, Vrf{Name: "vs-l3",
			L3Interfaces: []L3Interface{{Interface: "ens2f0.200", Addresses: []string{"10.99.0.1/24"}}}})
		// outside：wan VRF 承载出接口地址
		c.Vrfs = append(c.Vrfs, Vrf{Name: "wan",
			L3Interfaces: []L3Interface{{Interface: "ens2f1", Addresses: []string{"203.0.113.1/24"}}}})
		c.Nat = &NatConfig{}
		return c
	}

	// 合法：source-pool + interface 同时给出（决策 #38 的 CLI 语法）
	c := natBase()
	c.Nat.SourcePools = []NatSourcePool{{Name: "pool1", AddressRange: "203.0.113.10 to 203.0.113.20"}}
	c.Nat.Rules = []NatRule{{Seq: 10, MatchSource: "10.10.0.0/24", VirtualSwitch: "vs-l3",
		Action: NatAction{SourcePool: "pool1", Interface: "ens2f1"}}}
	mustNoErr(t, Validate(c))

	// 合法：仅 interface（无池，以出接口地址作外部地址）
	c2 := natBase()
	c2.Nat.Rules = []NatRule{{Seq: 10, MatchSource: "10.10.0.0/24", VirtualSwitch: "vs-l3",
		Action: NatAction{Interface: "ens2f1"}}}
	mustNoErr(t, Validate(c2))

	// 缺出接口 → 报错（决策 #38）
	c3 := natBase()
	c3.Nat.SourcePools = []NatSourcePool{{Name: "pool1", AddressRange: "203.0.113.10 to 203.0.113.20"}}
	c3.Nat.Rules = []NatRule{{Seq: 10, MatchSource: "10.10.0.0/24", VirtualSwitch: "vs-l3",
		Action: NatAction{SourcePool: "pool1"}}}
	mustErrContaining(t, Validate(c3), "action.interface", "必须指定出接口")

	// 多条规则 inside 转发域不一致 → 报错
	c4 := natBase()
	c4.VirtualSwitches = append(c4.VirtualSwitches, VirtualSwitch{Name: "vs-l3b", Type: "l3"})
	c4.Vrfs = append(c4.Vrfs, Vrf{Name: "vs-l3b"})
	c4.Nat.Rules = []NatRule{
		{Seq: 10, MatchSource: "10.10.0.0/24", VirtualSwitch: "vs-l3", Action: NatAction{Interface: "ens2f1"}},
		{Seq: 20, MatchSource: "10.20.0.0/24", VirtualSwitch: "vs-l3b", Action: NatAction{Interface: "ens2f1"}},
	}
	mustErrContaining(t, Validate(c4), "virtual_switch", "单一 inside 转发域")

	// 出接口不归属任何 VRF → 报错（V1 outside 转发域来自 VRF）
	c5 := natBase()
	c5.Nat.Rules = []NatRule{{Seq: 10, MatchSource: "10.10.0.0/24", VirtualSwitch: "vs-l3",
		Action: NatAction{Interface: "ens9f9"}}}
	mustErrContaining(t, Validate(c5), "action.interface", "不在任何 VRF")

	// 出接口在多条规则中归属不同 VRF → 报错
	c6 := natBase()
	c6.Interfaces = append(c6.Interfaces, InterfaceConfig{Name: "ens2f2"})
	c6.Vrfs = append(c6.Vrfs, Vrf{Name: "wan2",
		L3Interfaces: []L3Interface{{Interface: "ens2f2", Addresses: []string{"203.0.114.1/24"}}}})
	c6.Nat.Rules = []NatRule{
		{Seq: 10, MatchSource: "10.10.0.0/24", VirtualSwitch: "vs-l3", Action: NatAction{Interface: "ens2f1"}},
		{Seq: 20, MatchSource: "10.30.0.0/24", VirtualSwitch: "vs-l3", Action: NatAction{Interface: "ens2f2"}},
	}
	mustErrContaining(t, Validate(c6), "action.interface", "单一 outside VRF")
}

// FR-SYS-004（决策 #69）：远程 syslog 的 facility/severity/port 须在契约枚举与范围内。
func TestValidateSyslogRemoteFields(t *testing.T) {
	base := validBase()
	base.System.Syslog = &SyslogConfig{
		RemoteHost: "10.0.0.9", RemotePort: 514, Facility: "local0", Severity: "warn", Level: "info",
	}
	mustNoErr(t, Validate(base))

	// 非法 facility
	bad := validBase()
	bad.System.Syslog = &SyslogConfig{RemoteHost: "10.0.0.9", Facility: "bogus"}
	mustErrContaining(t, Validate(bad), "system.syslog.facility", "facility")

	// 非法 severity
	bad2 := validBase()
	bad2.System.Syslog = &SyslogConfig{RemoteHost: "10.0.0.9", Severity: "verbose"}
	mustErrContaining(t, Validate(bad2), "system.syslog.severity", "severity")

	// 端口越界
	bad3 := validBase()
	bad3.System.Syslog = &SyslogConfig{RemoteHost: "10.0.0.9", RemotePort: 70000}
	mustErrContaining(t, Validate(bad3), "system.syslog.remote_port", "超出")

	// facility 大小写不敏感（规范化由转发侧完成，校验须接受）
	ok := validBase()
	ok.System.Syslog = &SyslogConfig{RemoteHost: "10.0.0.9", Facility: "LOCAL7"}
	mustNoErr(t, Validate(ok))
}

// 决策 #356：历史时序存储的采样间隔/保留天数——仅非零时校验范围，0 = 未设置（允许，用默认）。
func TestValidateMetricsHistory(t *testing.T) {
	withHist := func(iv, rd int) Config {
		c := validBase()
		c.System.Metrics = &MetricsConfig{History: &MetricsHistoryConfig{IntervalSeconds: iv, RetentionDays: rd}}
		return c
	}

	// 合法值（含边界）
	mustNoErr(t, Validate(withHist(60, 7)))
	mustNoErr(t, Validate(withHist(MetricsIntervalMinSeconds, MetricsRetentionDaysMin)))
	mustNoErr(t, Validate(withHist(MetricsIntervalMaxSeconds, MetricsRetentionDaysMax)))

	// 0 = 未设置（字段缺省），允许
	mustNoErr(t, Validate(withHist(0, 0)))

	// 低于下界：报错须含范围与越界值（保留天数的下界是 1，0 即「未设置」哨兵，
	// 故「低于下界」只能用负数覆盖——MIN-1 会等于 0 而被当作未设置）
	mustErrContaining(t, Validate(withHist(MetricsIntervalMinSeconds-1, 7)),
		"system.metrics.history.interval_seconds", "10-3600")
	mustErrContaining(t, Validate(withHist(60, -1)),
		"system.metrics.history.retention_days", "1-365")

	// 高于上界：报错须含范围
	mustErrContaining(t, Validate(withHist(MetricsIntervalMaxSeconds+1, 7)),
		"system.metrics.history.interval_seconds", "10-3600")
	mustErrContaining(t, Validate(withHist(60, MetricsRetentionDaysMax+1)),
		"system.metrics.history.retention_days", "1-365")

	// 负数同样越界
	mustErrContaining(t, Validate(withHist(-1, -1)),
		"system.metrics.history.interval_seconds", "10-3600")

	// 空壳（metrics 有、history 缺）不报错
	c := validBase()
	c.System.Metrics = &MetricsConfig{}
	mustNoErr(t, Validate(c))

	// 访问器：缺省/越界回落默认，合法值原样
	okCfg := withHist(120, 30)
	if got := okCfg.MetricsHistoryIntervalSeconds(); got != 120 {
		t.Fatalf("合法采样间隔应原样返回，实得 %d", got)
	}
	emptyCfg := Config{}
	if got := emptyCfg.MetricsHistoryIntervalSeconds(); got != MetricsIntervalDefaultSeconds {
		t.Fatalf("缺省采样间隔应回落默认 %d，实得 %d", MetricsIntervalDefaultSeconds, got)
	}
	overCfg := withHist(MetricsIntervalMaxSeconds+1, 0)
	if got := overCfg.MetricsHistoryIntervalSeconds(); got != MetricsIntervalDefaultSeconds {
		t.Fatalf("越界采样间隔应回落默认 %d，实得 %d", MetricsIntervalDefaultSeconds, got)
	}
	overRet := withHist(0, 400)
	if got := overRet.MetricsHistoryRetentionDays(); got != MetricsRetentionDaysDefault {
		t.Fatalf("越界保留天数应回落默认 %d，实得 %d", MetricsRetentionDaysDefault, got)
	}
	if got := (*Config)(nil).MetricsHistoryRetentionDays(); got != MetricsRetentionDaysDefault {
		t.Fatalf("nil 接收者应回落默认 %d，实得 %d", MetricsRetentionDaysDefault, got)
	}
}

// 决策 #352：ACL 规则只匹配单族——两侧都写显式前缀时，混族（v4 与 v6）在校验期拒绝；
// 双族过滤的正解是两条规则（各自同族，any 一侧由编排层跟随显式侧家族）。
func TestValidateAclMixedFamilyRejected(t *testing.T) {
	c := validBase()
	c.Acls = []Acl{{Name: "acl-mix", Rules: []AclRule{{
		Seq: 10, Action: "permit", Source: "10.0.0.0/8", Destination: "2001:db8::/64",
	}}}}
	mustErrContaining(t, Validate(c), "rules[10].source", "地址族不一致")

	// 反向混族同样拒绝（source v6、destination v4）
	c2 := validBase()
	c2.Acls = []Acl{{Name: "acl-mix2", Rules: []AclRule{{
		Seq: 10, Action: "deny", Source: "2001:db8::/64", Destination: "192.168.1.0/24",
	}}}}
	mustErrContaining(t, Validate(c2), "rules[10].source", "地址族不一致")

	// 同族规则（含 any 跟随形态）不受影响
	c3 := validBase()
	c3.Acls = []Acl{{Name: "acl-v6", Rules: []AclRule{
		{Seq: 10, Action: "deny", Source: "2001:db8::/64", Destination: "any"},
		{Seq: 20, Action: "permit", Source: "10.0.0.0/8", Destination: "any"},
	}}}
	mustNoErr(t, Validate(c3))
}

// 决策 #352：NAT44 下发层严格 v4（ParseIP4Address）——池地址范围、规则 match-source、
// 静态映射的 v6 在校验期拒绝（文案注明仅支持 IPv4），不再等到 commit 期报 invalid IP4 address。
func TestValidateNatV6Rejected(t *testing.T) {
	base := func() Config {
		c := validBase()
		c.Interfaces = append(c.Interfaces, InterfaceConfig{Name: "ens2f1"})
		c.VirtualSwitches = append(c.VirtualSwitches, VirtualSwitch{Name: "vs-l3", Type: "l3"})
		c.Vrfs = append(c.Vrfs, Vrf{Name: "vs-l3",
			L3Interfaces: []L3Interface{{Interface: "ens2f0.200", Addresses: []string{"10.99.0.1/24"}}}})
		c.Vrfs = append(c.Vrfs, Vrf{Name: "wan",
			L3Interfaces: []L3Interface{{Interface: "ens2f1", Addresses: []string{"203.0.113.1/24"}}}})
		c.Nat = &NatConfig{}
		return c
	}

	// 池地址范围含 v6 → 拒绝
	c := base()
	c.Nat.SourcePools = []NatSourcePool{{Name: "pool6", AddressRange: "2001:db8::10 to 2001:db8::20"}}
	c.Nat.Rules = []NatRule{{Seq: 10, MatchSource: "10.10.0.0/24", VirtualSwitch: "vs-l3",
		Action: NatAction{SourcePool: "pool6", Interface: "ens2f1"}}}
	mustErrContaining(t, Validate(c), "address_range", "仅支持 IPv4")

	// match-source v6 → 拒绝
	c2 := base()
	c2.Nat.Rules = []NatRule{{Seq: 10, MatchSource: "2001:db8::/64", VirtualSwitch: "vs-l3",
		Action: NatAction{Interface: "ens2f1"}}}
	mustErrContaining(t, Validate(c2), "match_source", "仅支持 IPv4")

	// 静态映射 v6 → 拒绝（inside / outside 各一例）
	c3 := base()
	c3.Nat.Static = []NatStatic{{InsideIP: "2001:db8::5", OutsideIP: "203.0.113.9"}}
	mustErrContaining(t, Validate(c3), "inside_ip", "仅支持 IPv4")

	c4 := base()
	c4.Nat.Static = []NatStatic{{InsideIP: "10.99.0.5", OutsideIP: "2001:db8::9"}}
	mustErrContaining(t, Validate(c4), "outside_ip", "仅支持 IPv4")

	// v4 正常值回归：三种形态同时声明都不报错
	c5 := base()
	c5.Nat.SourcePools = []NatSourcePool{{Name: "pool1", AddressRange: "203.0.113.10 to 203.0.113.20"}}
	c5.Nat.Rules = []NatRule{{Seq: 10, MatchSource: "10.10.0.0/24", VirtualSwitch: "vs-l3",
		Action: NatAction{SourcePool: "pool1", Interface: "ens2f1"}}}
	c5.Nat.Static = []NatStatic{{InsideIP: "10.99.0.5", OutsideIP: "203.0.113.9"}}
	mustNoErr(t, Validate(c5))
}
