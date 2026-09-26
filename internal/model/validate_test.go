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
