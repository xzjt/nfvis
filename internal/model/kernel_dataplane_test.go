package model

import (
	"slices"
	"strings"
	"testing"
)

// 内核数据面（system.dataplane = kernel）的提交期校验（v3 决策 #404）。

func errText(errs []ValidateError) string {
	var b strings.Builder
	for _, e := range errs {
		b.WriteString(e.Path)
		b.WriteString("|")
		b.WriteString(e.Message)
		b.WriteString("\n")
	}
	return b.String()
}

func TestDataPlaneValueValidation(t *testing.T) {
	bad := Config{System: &SystemConfig{DataPlane: "dpdk"}}
	if errs := Validate(bad); len(errs) == 0 {
		t.Fatalf("非法数据面取值应被拒绝")
	} else if !strings.Contains(errText(errs), "system.dataplane") {
		t.Fatalf("报错应指向 system.dataplane，得到 %s", errText(errs))
	}
	for _, ok := range []string{"", DataPlaneVPP, DataPlaneKernel} {
		if errs := Validate(Config{System: &SystemConfig{DataPlane: ok}}); len(errs) != 0 {
			t.Fatalf("取值 %q 应被接受，得到 %s", ok, errText(errs))
		}
	}
}

func TestDataPlaneModeDefaultsToVPP(t *testing.T) {
	cases := []Config{
		{},
		{System: &SystemConfig{}},
		{System: &SystemConfig{DataPlane: ""}},
		{System: &SystemConfig{DataPlane: "bogus"}}, // 非法值兜底回落 vpp，不当作 kernel
	}
	for i, c := range cases {
		if got := c.DataPlaneMode(); got != DataPlaneVPP {
			t.Fatalf("用例 %d：缺省应为 vpp，得到 %q", i, got)
		}
	}
	if got := (&Config{System: &SystemConfig{DataPlane: DataPlaneKernel}}).DataPlaneMode(); got != DataPlaneKernel {
		t.Fatalf("显式 kernel 应生效，得到 %q", got)
	}
}

func TestKernelDataPlaneRejectsUnimplementedFamilies(t *testing.T) {
	base := func() Config { return Config{System: &SystemConfig{DataPlane: DataPlaneKernel}} }

	// 每一项都是一个「内核数据面下会静默不生效」的配置，必须提交期拒绝。
	cases := map[string]func(*Config){
		"LLDP": func(c *Config) { c.Protocols = &ProtocolsConfig{LLDP: &LldpConfig{}} },
		"DHCP 服务器": func(c *Config) {
			c.VirtualSwitches = []VirtualSwitch{{Name: "vs", Type: "l2", DhcpServerPoolStart: "10.0.0.10"}}
		},
		"DNS 代理": func(c *Config) {
			c.VirtualSwitches = []VirtualSwitch{{Name: "vs", Type: "l2", DNSProxyServers: []string{"8.8.8.8"}}}
		},
		"网关 ACL": func(c *Config) {
			c.VirtualSwitches = []VirtualSwitch{{Name: "vs", Type: "l2",
				Gateway: &VSGateway{Addresses: []string{"10.0.0.1/24"}, AclIn: "acl"}}}
		},
		"端口 ACL": func(c *Config) {
			c.VirtualSwitches = []VirtualSwitch{{Name: "vs", Type: "l2",
				Ports: []VSwitchPort{{Seq: 1, Interface: "ens192", AclIn: "acl"}}}}
		},
		"容器 vNIC": func(c *Config) {
			c.VirtualSwitches = []VirtualSwitch{{Name: "vs", Type: "l2",
				Ports: []VSwitchPort{{Seq: 1, Container: "ct1"}}}}
		},
		"vNIC 作 L3 接口": func(c *Config) {
			c.VirtualMachineFunctions = []VMFunction{{Name: "vm1",
				Interfaces: []VnfInterface{{Name: "nic0", Type: "vhost-user"}}}}
			c.Vrfs = []Vrf{{Name: "vs-l3", L3Interfaces: []L3Interface{{Interface: "nic0"}}}}
		},
		"VM memif": func(c *Config) {
			c.VirtualMachineFunctions = []VMFunction{{Name: "vm1",
				Interfaces: []VnfInterface{{Name: "nic0", Type: "memif"}}}}
		},
		"镜像源为 vNIC": func(c *Config) {
			c.PortMirroring = []PortMirroring{{Name: "m", Analyzer: "ens224",
				Source: PMSource{Vnf: "vm1", VnfInterface: "nic0"}}}
		},
		"容器网卡": func(c *Config) {
			c.ContainerFunctions = []ContainerFunction{{Name: "ct1",
				Interfaces: []VnfInterface{{Name: "nic0", Type: "memif"}}}}
		},
	}
	for name, mutate := range cases {
		c := base()
		mutate(&c)
		if errs := Validate(c); len(errs) == 0 {
			t.Fatalf("%s：内核数据面下应提交期拒绝", name)
		}
	}
}

// 内核数据面下 DHCP 中继**不再**提交期拒绝：内核侧由 nfvisd 内的用户态中继实例承担
// （收 bridge 上的 DHCP 请求 → 源地址重写为 BVI 的 v4 网关地址 → 单播 server:67，giaddr=0；
// 应答按「请求期 xid → 客户端 MAC」登记表回注以太帧）。红-绿：把 validate.go 的内核侧拒绝
// 加回来，本用例即失败。
func TestKernelDataPlaneAcceptsDHCPRelay(t *testing.T) {
	ok := Config{
		System: &SystemConfig{DataPlane: DataPlaneKernel},
		VirtualSwitches: []VirtualSwitch{{Name: "vs-r", Type: "l2",
			Gateway:         &VSGateway{Addresses: []string{"192.168.99.1/24"}},
			DhcpRelayServer: "192.168.99.10"}},
	}
	if errs := Validate(ok); len(errs) != 0 {
		t.Fatalf("内核数据面下已配 v4 网关的 L2 交换机应可配 DHCP 中继，得到：%s", errText(errs))
	}
	// 两数据面同一语句：VPP 侧同样接受（既有行为不变）。
	vpp := ok
	vpp.System = &SystemConfig{DataPlane: DataPlaneVPP}
	if errs := Validate(vpp); len(errs) != 0 {
		t.Fatalf("VPP 数据面下该配置应照常接受，得到：%s", errText(errs))
	}
	// 通用约束两数据面共用（与数据面无关，保留）：无 v4 网关的中继仍被拒绝。
	noV4 := Config{
		System: &SystemConfig{DataPlane: DataPlaneKernel},
		VirtualSwitches: []VirtualSwitch{{Name: "vs-r", Type: "l2",
			Gateway:         &VSGateway{Addresses: []string{"2001:db8::1/64"}},
			DhcpRelayServer: "192.168.99.10"}},
	}
	if errs := Validate(noV4); len(errs) == 0 {
		t.Fatalf("网关没有 IPv4 地址时应拒绝 DHCP 中继（中继源地址取 BVI 的 IPv4 地址）")
	}
	// 与 dhcp-server 的互斥校验保留（两者争抢 UDP/67 的处理权）。
	conflict := Config{
		System: &SystemConfig{DataPlane: DataPlaneKernel},
		VirtualSwitches: []VirtualSwitch{{Name: "vs-r", Type: "l2",
			Gateway:             &VSGateway{Addresses: []string{"192.168.99.1/24"}},
			DhcpRelayServer:     "192.168.99.10",
			DhcpServerPoolStart: "192.168.99.100",
			DhcpServerPoolEnd:   "192.168.99.120"}},
	}
	if errs := Validate(conflict); len(errs) == 0 {
		t.Fatalf("同一交换机同时配 DHCP 中继与 DHCP 服务器仍应被拒绝（争抢 UDP/67）")
	}
}

// 决策 #435：内核数据面下 learn-limit **不再**提交期拒绝——内核 bridge 没有「学习条数上限」
// 原语，产品把它实现为「fdb 计数 + 阈值告警」（BRIDGE_FDB_LIMIT_REACHED），读视图如实注明
// 「阈值告警、非强制上限」。红-绿：把 validate.go 的内核侧拒绝加回来，本用例即失败。
func TestKernelDataPlaneAcceptsLearnLimit(t *testing.T) {
	ok := Config{
		System:          &SystemConfig{DataPlane: DataPlaneKernel},
		VirtualSwitches: []VirtualSwitch{{Name: "vs-ll", Type: "l2", LearnLimit: 100}},
	}
	if errs := Validate(ok); len(errs) != 0 {
		t.Fatalf("内核数据面下 learn-limit 应被接受，得到：\n%s", errText(errs))
	}
	// 取值域校验两数据面共用：越界仍拒绝（保留 VPP 侧 1-16777216 的通用校验）。
	bad := Config{
		System:          &SystemConfig{DataPlane: DataPlaneKernel},
		VirtualSwitches: []VirtualSwitch{{Name: "vs-ll", Type: "l2", LearnLimit: 16777217}},
	}
	if errs := Validate(bad); len(errs) == 0 {
		t.Fatalf("越界 learn-limit 应被拒绝")
	}
}

func TestKernelDataPlaneAcceptsCoreFamilies(t *testing.T) {
	enabled := true
	c := Config{
		System: &SystemConfig{DataPlane: DataPlaneKernel},
		Interfaces: []InterfaceConfig{
			{Name: "ens192", MTU: 9000, Enabled: &enabled},
			{Name: "ens224"}, {Name: "ens256"},
			{Name: "ens257"}, {Name: "ens258"},
		},
		Bonds: []Bond{{Name: "bond0", Members: []string{"ens224", "ens256"}}},
		VirtualSwitches: []VirtualSwitch{{
			Name: "vs-lan", Type: "l2", VlanAccess: 100,
			Gateway: &VSGateway{Addresses: []string{"192.168.99.1/24"}},
		}},
		Vrfs: []Vrf{{
			Name:         "vs-l3",
			L3Interfaces: []L3Interface{{Interface: "bond0", Addresses: []string{"10.0.0.1/24"}}},
			Routes:       []Route{{Prefix: "0.0.0.0/0", NextHop: "10.0.0.254"}},
		}},
		Nat:         &NatConfig{SourcePools: []NatSourcePool{{Name: "p", AddressRange: "203.0.113.1 to 203.0.113.1"}}},
		Acls:        []Acl{{Name: "acl1", Rules: []AclRule{{Seq: 10, Source: "any", Destination: "any", Action: "deny"}}}},
		QosPolicies: []QosPolicy{{Name: "pol1", Cir: 8000, Cbs: 1000}},
		PortMirroring: []PortMirroring{{Name: "span1", Analyzer: "ens258",
			Source: PMSource{Interface: "ens257", Direction: "both"}}},
		VxlanTunnels: []VxlanTunnel{{Name: "vx1", Vni: 100, Local: "10.0.0.1", Remote: "10.0.0.2"}},
	}
	if errs := Validate(c); len(errs) != 0 {
		t.Fatalf("核心族在内核数据面下应被接受，得到：\n%s", errText(errs))
	}
}

func TestKernelDataPlaneLinkNameLength(t *testing.T) {
	c := Config{
		System:          &SystemConfig{DataPlane: DataPlaneKernel},
		VirtualSwitches: []VirtualSwitch{{Name: "this-name-is-way-too-long", Type: "l2"}},
	}
	errs := Validate(c)
	if len(errs) == 0 {
		t.Fatalf("超过内核接口名上限的对象名应被拒绝")
	}
	if !strings.Contains(errText(errs), "15") {
		t.Fatalf("报错应说明长度上限，得到 %s", errText(errs))
	}
	// 同一名字在 VPP 数据面下不受该限制（VPP 侧没有内核接口名约束）。
	ok := Config{System: &SystemConfig{DataPlane: DataPlaneVPP},
		VirtualSwitches: []VirtualSwitch{{Name: "this-name-is-way-too-long", Type: "l2"}}}
	if errs := Validate(ok); len(errs) != 0 {
		t.Fatalf("VPP 数据面不应受内核接口名长度限制，得到 %s", errText(errs))
	}
}

// 内核数据面下 `vhost-user` 型 vNIC 落成 virtio + 宿主 tap + vhost-net（无共享内存对端），
// 故「vhost-user 必须大页」这条约束不成立：backing normal 应放行。
// 真机走查暴露：想用普通内存建 VNF 会被这条挡住，而它本不需要大页（被迫声明资源池 + 重启）。
func TestKernelDataPlaneAllowsNormalMemoryWithVhostUserNic(t *testing.T) {
	mk := func(mode string) Config {
		return Config{
			System:          &SystemConfig{DataPlane: mode},
			VirtualSwitches: []VirtualSwitch{{Name: "vs-lan", Type: "l2"}},
			VirtualMachineFunctions: []VMFunction{{
				Name: "vnf1", Image: "alpine.qcow2",
				VCPU:   VMCpu{Count: 1},
				Memory: VMMemory{SizeMB: 256, Backing: "normal"},
				Interfaces: []VnfInterface{{
					Name: "nic0", Type: "vhost-user", VirtualSwitch: "vs-lan",
				}},
			}},
		}
	}
	if errs := Validate(mk(DataPlaneKernel)); len(errs) != 0 {
		t.Fatalf("内核数据面下 backing normal + vhost-user 型 vNIC 应放行，得到：\n%s", errText(errs))
	}
	// VPP 数据面下约束照旧（共享内存形态必须大页）。
	errs := Validate(mk(DataPlaneVPP))
	if len(errs) == 0 {
		t.Fatal("VPP 数据面下 backing normal + vhost-user 应被拒（FR-CFG-011①）")
	}
	if !strings.Contains(errText(errs), "hugepage") {
		t.Fatalf("报错应指向大页要求，得到：%s", errText(errs))
	}
}

// 跨转发域的 NAT（inside 交换机派生的 VRF ≠ 出接口所属 VRF）：内核侧 nft 规则不区分转发域、
// 也没有跨表 leaking（规则在场、一个包不命中），故提交期拒绝并点名两侧转发域与替代。
// 红-绿：修复前同一配置在内核数据面下零报错（「规则在场、一个包不通」）。
func TestKernelDataPlaneRejectsNATCrossVRF(t *testing.T) {
	base := func() Config {
		return Config{
			System:          &SystemConfig{DataPlane: DataPlaneKernel},
			Interfaces:      []InterfaceConfig{{Name: "ens192"}, {Name: "ens224"}},
			VirtualSwitches: []VirtualSwitch{{Name: "vs-in", Type: "l3"}, {Name: "vs-out", Type: "l3"}},
			Vrfs: []Vrf{
				{Name: "vs-in", L3Interfaces: []L3Interface{{Interface: "ens192", Addresses: []string{"10.0.0.1/24"}}}},
				{Name: "vs-out", L3Interfaces: []L3Interface{{Interface: "ens224", Addresses: []string{"203.0.113.1/24"}}}},
			},
			Nat: &NatConfig{Rules: []NatRule{{Seq: 10, MatchSource: "10.0.0.0/24", VirtualSwitch: "vs-in",
				Action: NatAction{Interface: "ens224"}}}},
		}
	}
	// 跨域：inside=vs-in，出接口 ens224 属 vs-out ⇒ 拒绝，文案须点名两域与机理。
	errs := Validate(base())
	if len(errs) == 0 {
		t.Fatalf("跨转发域的 NAT 规则应提交期拒绝（内核侧规则不会命中任何包）")
	}
	txt := errText(errs)
	if !strings.Contains(txt, "跨转发域") || !strings.Contains(txt, "vs-in") || !strings.Contains(txt, "vs-out") {
		t.Fatalf("报错应点名两侧转发域与机理，得到：\n%s", txt)
	}
	if !strings.Contains(txt, "set system dataplane vpp") {
		t.Fatalf("报错应给出替代路径（切回 VPP），得到：\n%s", txt)
	}
	// 同域：出接口换成 vs-in 自己的 l3 接口 ⇒ 放行。
	same := base()
	same.Nat.Rules[0].Action.Interface = "ens192"
	if errs := Validate(same); len(errs) != 0 {
		t.Fatalf("同转发域的 NAT 应放行，得到：\n%s", errText(errs))
	}
	// VPP 数据面不受该限制（跨 VRF 是其受支持形态）。
	vpp := base()
	vpp.System = &SystemConfig{DataPlane: DataPlaneVPP}
	if errs := Validate(vpp); len(errs) != 0 {
		t.Fatalf("VPP 数据面下跨转发域 NAT 应放行，得到：\n%s", errText(errs))
	}
}

// R2-11 的判据单测：两侧都为「默认表/未归属 VRF」（空串）视为一致，只有两侧都具名且不同才算跨域。
func TestKernelNATDomainCriterion(t *testing.T) {
	cases := []struct {
		inside, outside string
		cross           bool
	}{
		{"", "", false},
		{"vs-in", "", false},
		{"", "vs-out", false},
		{"vs-in", "vs-in", false},
		{"vs-in", "vs-out", true},
	}
	for _, tc := range cases {
		if got := kernelNatDomainsCross(tc.inside, tc.outside); got != tc.cross {
			t.Fatalf("kernelNatDomainsCross(%q,%q) = %v，期望 %v", tc.inside, tc.outside, got, tc.cross)
		}
	}
}

// cross-connect（无学习点对点直通）内核侧没有对应物：静默按普通 bridge 处理会改变语义
// （学习/泛洪/VLAN），故提交期拒绝并指向 L2 交换机 + 端口。红-绿：修复前零报错。
func TestKernelDataPlaneRejectsCrossConnect(t *testing.T) {
	mk := func(mode string) Config {
		return Config{
			System:     &SystemConfig{DataPlane: mode},
			Interfaces: []InterfaceConfig{{Name: "ens192"}, {Name: "ens224"}},
			VirtualSwitches: []VirtualSwitch{{Name: "vs-cc", Type: "l2", CrossConnect: true,
				Ports: []VSwitchPort{{Seq: 1, Interface: "ens192"}, {Seq: 2, Interface: "ens224"}}}},
		}
	}
	errs := Validate(mk(DataPlaneKernel))
	if len(errs) == 0 {
		t.Fatalf("内核数据面下 cross-connect 应提交期拒绝")
	}
	txt := errText(errs)
	if !strings.Contains(txt, "virtual_switches[vs-cc].cross_connect") || !strings.Contains(txt, "cross-connect") {
		t.Fatalf("报错应点名该交换机的 cross_connect，得到：\n%s", txt)
	}
	if !strings.Contains(txt, "L2 交换机") {
		t.Fatalf("报错应给出替代（L2 交换机 + 端口），得到：\n%s", txt)
	}
	if errs := Validate(mk(DataPlaneVPP)); len(errs) != 0 {
		t.Fatalf("VPP 数据面下 cross-connect 应放行，得到：\n%s", errText(errs))
	}
}

// 静态路由的多下一跳（ECMP）：内核侧未实现（`via a,b` 会被 iproute2 当非法参数），
// 提交期拒绝并给「拆成多条单跳路由」的替代。红-绿：修复前零报错（失败落到下发期）。
func TestKernelDataPlaneRejectsRouteECMP(t *testing.T) {
	mk := func(mode string) Config {
		return Config{
			System:     &SystemConfig{DataPlane: mode},
			Interfaces: []InterfaceConfig{{Name: "ens192"}},
			Vrfs: []Vrf{{Name: "vs-l3", L3Interfaces: []L3Interface{{Interface: "ens192"}},
				Routes: []Route{{Prefix: "0.0.0.0/0", NextHop: "10.0.0.254,10.0.0.253"}}}},
		}
	}
	errs := Validate(mk(DataPlaneKernel))
	if len(errs) == 0 {
		t.Fatalf("内核数据面下多下一跳（ECMP）应提交期拒绝")
	}
	txt := errText(errs)
	if !strings.Contains(txt, "ECMP") || !strings.Contains(txt, "vrfs[vs-l3].routes[0.0.0.0/0].next_hop") {
		t.Fatalf("报错应点名 ECMP 与该路由字段，得到：\n%s", txt)
	}
	if errs := Validate(mk(DataPlaneVPP)); len(errs) != 0 {
		t.Fatalf("VPP 数据面下 ECMP 应放行（#381 已支持），得到：\n%s", errText(errs))
	}
	// 单跳不受影响（两个数据面都放行）。
	one := mk(DataPlaneKernel)
	one.Vrfs[0].Routes[0].NextHop = "10.0.0.254"
	if errs := Validate(one); len(errs) != 0 {
		t.Fatalf("单跳路由应放行，得到：\n%s", errText(errs))
	}
}

// vNIC 接入 type=l3 的交换机：内核侧 L3 交换机不建 bridge，域定义的 tap 无处可挂
// （提交期全绿、起 VM 时才失败）⇒ 提交期拒绝。红-绿：修复前零报错。
func TestKernelDataPlaneRejectsVnicOnL3Switch(t *testing.T) {
	mk := func(mode string) Config {
		return Config{
			System:          &SystemConfig{DataPlane: mode},
			VirtualSwitches: []VirtualSwitch{{Name: "vs-l3", Type: "l3"}},
			Vrfs:            []Vrf{{Name: "vs-l3", L3Interfaces: []L3Interface{{Interface: "ens192"}}}},
			Interfaces:      []InterfaceConfig{{Name: "ens192"}},
			VirtualMachineFunctions: []VMFunction{{
				Name: "vm1", Image: "alpine.qcow2",
				VCPU: VMCpu{Count: 1}, Memory: VMMemory{SizeMB: 512},
				Interfaces: []VnfInterface{{Name: "nic0", Type: "vhost-user", VirtualSwitch: "vs-l3"}},
			}},
		}
	}
	errs := Validate(mk(DataPlaneKernel))
	if len(errs) == 0 {
		t.Fatalf("内核数据面下 vNIC 接入 type=l3 交换机应提交期拒绝")
	}
	txt := errText(errs)
	if !strings.Contains(txt, "virtual_machine_functions[vm1].interfaces[nic0].virtual_switch") {
		t.Fatalf("报错应点名该 vNIC 的 virtual_switch，得到：\n%s", txt)
	}
	if !strings.Contains(txt, "vs-l3") || !strings.Contains(txt, "L2 交换机") {
		t.Fatalf("报错应点名交换机并给替代（已配网关的 L2 交换机），得到：\n%s", txt)
	}
	if errs := Validate(mk(DataPlaneVPP)); len(errs) != 0 {
		t.Fatalf("VPP 数据面下该形态应放行（vNIC 作 L3 接口进同名 VRF），得到：\n%s", errText(errs))
	}
}

// vlan 子接口的派生名（`<接口名>.<vid>`）直接用作内核设备名，超过 IFNAMSIZ-1 时下发必失败：
// 提交期拒绝（点名对象与折算后的长度）。红-绿：修复前零报错（失败落到 ip link add）。
func TestKernelDataPlaneRejectsLongVlanSubifName(t *testing.T) {
	mk := func(mode, iface string) Config {
		return Config{
			System:          &SystemConfig{DataPlane: mode},
			Interfaces:      []InterfaceConfig{{Name: iface}},
			VirtualSwitches: []VirtualSwitch{{Name: "vs-l3", Type: "l3"}},
			Vrfs: []Vrf{{Name: "vs-l3", L3Interfaces: []L3Interface{{Interface: iface, Vlan: 100,
				Addresses: []string{"10.0.0.1/24"}}}}},
		}
	}
	// ens192.100 = 10 字符 ⇒ 放行；派生名折算后的长度（15 + 1 + 3）超限 ⇒ 拒绝。
	if errs := Validate(mk(DataPlaneKernel, "ens192")); len(errs) != 0 {
		t.Fatalf("短接口名的 vlan 子接口应放行，得到：\n%s", errText(errs))
	}
	errs := Validate(mk(DataPlaneKernel, "this-name-is-way-too-long"))
	if len(errs) == 0 {
		t.Fatalf("派生接口名超过 15 字符的 vlan 子接口应提交期拒绝")
	}
	txt := errText(errs)
	if !strings.Contains(txt, "this-name-is-way-too-long") || !strings.Contains(txt, "15") {
		t.Fatalf("报错应点名对象与内核接口名上限，得到：\n%s", txt)
	}
	if !strings.Contains(txt, "vlan 子接口") {
		t.Fatalf("报错应说明是 vlan 子接口的派生名，得到：\n%s", txt)
	}
	// VPP 数据面没有内核接口名约束（同名对象在 VPP 侧照常）。
	if errs := Validate(mk(DataPlaneVPP, "this-name-is-way-too-long")); len(errs) != 0 {
		t.Fatalf("VPP 数据面不应受内核接口名长度限制，得到：\n%s", errText(errs))
	}
}

// 派生设备名互撞：内核设备名全局唯一，两个对象（或对象与派生的网关 VRF 名）撞进同一个名字时
// 提交期拒绝——否则先下发的占住设备、后下发的直接失败，配置与数据面从此错位。
//
// 已知可达形态：交换机 lan 的网关 VRF 派生名是 `vr-lan`，而一个**名叫 vr-lan** 的交换机
// （/bond/隧道/L3 交换机）内核设备名也是 `vr-lan`。
//
// 红-绿：修复前同一配置在内核数据面下零报错（失败落到 `ip link add`，报错只有设备名）。
func TestKernelDataPlaneRejectsDerivedDeviceNameCollision(t *testing.T) {
	// 交换机 lan（带网关 ⇒ 会建派生 VRF `vr-lan`）+ 交换机 vr-lan（设备名也是 vr-lan）。
	mk := func(mode string, order ...string) Config {
		c := Config{System: &SystemConfig{DataPlane: mode}}
		gw := VirtualSwitch{Name: "lan", Type: "l2",
			Gateway: &VSGateway{Addresses: []string{"192.168.99.1/24"}}}
		other := VirtualSwitch{Name: "vr-lan", Type: "l2"}
		if len(order) > 0 && order[0] == "reversed" {
			c.VirtualSwitches = []VirtualSwitch{other, gw}
		} else {
			c.VirtualSwitches = []VirtualSwitch{gw, other}
		}
		return c
	}

	errs := Validate(mk(DataPlaneKernel))
	if len(errs) == 0 {
		t.Fatalf("派生网关 VRF 名与同名交换机的设备名撞名，应提交期拒绝")
	}
	txt := errText(errs)
	// 文案必须点名**两个配置对象**（只报设备名操作者对不上是哪个声明），并给替代（改名）。
	for _, want := range []string{"lan", "vr-lan", "内核设备名", "改名"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("报错应含 %q，得到：\n%s", want, txt)
		}
	}
	if !strings.Contains(txt, "交换机 \"lan\" 的网关 VRF") || !strings.Contains(txt, "交换机 \"vr-lan\"") {
		t.Fatalf("报错应点名两个配置对象与派生来源，得到：\n%s", txt)
	}
	if !strings.Contains(txt, "virtual_switches[vr-lan]") {
		t.Fatalf("报错定位应指向冲突对象，得到：\n%s", txt)
	}
	// 声明顺序不影响判定（两类对象在同一集合里比对）。
	if errs := Validate(mk(DataPlaneKernel, "reversed")); len(errs) == 0 {
		t.Fatalf("调换声明顺序同样应被拒绝")
	}
	// VPP 数据面没有内核设备名这回事：同名对象照常共存。
	if errs := Validate(mk(DataPlaneVPP)); len(errs) != 0 {
		t.Fatalf("VPP 数据面不应受派生设备名撞名限制，得到：\n%s", errText(errs))
	}

	// 没有网关（不建派生 VRF）时不撞：lan 是 bridge、vr-lan 也是 bridge，两个名字互不相同。
	noGW := mk(DataPlaneKernel)
	noGW.VirtualSwitches[0].Gateway = nil
	if errs := Validate(noGW); len(errs) != 0 {
		t.Fatalf("未配网关的交换机不建派生 VRF，不应报撞名，得到：\n%s", errText(errs))
	}
	// 只有 IPv6 网关地址同样会建派生 VRF（判据是"声明了网关地址"而非地址族）。
	v6gw := mk(DataPlaneKernel)
	v6gw.VirtualSwitches[0].Gateway = &VSGateway{Addresses: []string{"2001:db8::1/64"}}
	if errs := Validate(v6gw); len(errs) == 0 {
		t.Fatalf("IPv6 网关同样会建派生 VRF，撞名应被拒绝")
	}
	// 跨类撞名（交换机 × bond 同名 ⇒ 同一内核设备名）同样被拦下。
	cross := Config{
		System:          &SystemConfig{DataPlane: DataPlaneKernel},
		Interfaces:      []InterfaceConfig{{Name: "ens192"}, {Name: "ens224"}},
		VirtualSwitches: []VirtualSwitch{{Name: "zx", Type: "l2"}},
		Bonds:           []Bond{{Name: "zx", Members: []string{"ens192", "ens224"}}},
	}
	if errs := Validate(cross); len(errs) == 0 {
		t.Fatalf("交换机与 bond 同名会派生同一个内核设备名，应提交期拒绝")
	} else {
		txt = errText(errs)
		if !strings.Contains(txt, "交换机 \"zx\"") || !strings.Contains(txt, "bond \"zx\"") {
			t.Fatalf("跨类撞名的报错应点名两个对象（含类型），得到：\n%s", txt)
		}
	}
	if errs := Validate(func() Config { c := cross; c.System = &SystemConfig{DataPlane: DataPlaneVPP}; return c }()); len(errs) != 0 {
		t.Fatalf("VPP 数据面下跨类同名不受内核设备名约束（既有口径），得到：\n%s", errText(errs))
	}

	// l3 交换机与同名 vrfs 条目是**同一个对象**（附录 B 映射）：不能自撞。
	self := Config{
		System:          &SystemConfig{DataPlane: DataPlaneKernel},
		VirtualSwitches: []VirtualSwitch{{Name: "vs-l3", Type: "l3"}},
		Vrfs:            []Vrf{{Name: "vs-l3"}},
	}
	if errs := Validate(self); len(errs) != 0 {
		t.Fatalf("L3 交换机与承载它的同名 vrfs 条目不应报撞名，得到：\n%s", errText(errs))
	}
}

// 哈希截断派生出的撞名同样要拦：13~15 字符的交换机名会让 `vr-<名>` 超过 15，走
// 「前缀 + 8 位哈希」截断；而截断结果本身是个合法（≤15 字符）的对象名——于是
// 「一台 15 字符交换机 + 一个恰好叫该派生名的交换机」是**离线可构造**的撞名形态（R2-21 家族）。
// 本用例同时钉住 model 侧对 LinkName/GatewayVRFName 的复刻确实走了截断分支（不是"短名恰好相等"）。
func TestKernelDataPlaneRejectsHashedDerivedNameCollision(t *testing.T) {
	sw := "abcdefghijklmno" // 15 字符：`vr-` 前缀后共 18 字符 ⇒ 必然走哈希截断
	dev := kernelDerivedGatewayVRFName(sw)
	if len(sw) != kernelLinkNameMax || dev == "vr-"+sw || len(dev) > kernelLinkNameMax {
		t.Fatalf("前提不成立：%q 的派生网关 VRF 名 %q 应走哈希截断且 ≤%d 字符", sw, dev, kernelLinkNameMax)
	}
	mk := func(mode, other string) Config {
		return Config{
			System: &SystemConfig{DataPlane: mode},
			VirtualSwitches: []VirtualSwitch{
				{Name: sw, Type: "l2", Gateway: &VSGateway{Addresses: []string{"192.168.99.1/24"}}},
				{Name: other, Type: "l2"},
			},
		}
	}
	// 对方名字恰好等于派生设备名 ⇒ 撞名必被拦。
	errs := Validate(mk(DataPlaneKernel, dev))
	if len(errs) == 0 {
		t.Fatalf("交换机 %q 的网关 VRF 派生名 %q 与同名交换机撞名，应提交期拒绝", sw, dev)
	}
	if txt := errText(errs); !strings.Contains(txt, dev) || !strings.Contains(txt, "内核设备名") {
		t.Fatalf("报错应点名派生设备名与机理，得到：\n%s", txt)
	}
	// 对照组：同前缀、名字差一个字符 ⇒ 哈希不同 ⇒ 派生设备名不同，必须放行（判据不能过宽）。
	other := kernelDerivedGatewayVRFName("abcdefghijklmnp")
	if other == dev {
		t.Skip("对照组派生名与实验组相同（前缀/哈希规则变化），跳过")
	}
	if errs := Validate(mk(DataPlaneKernel, other)); len(errs) != 0 {
		t.Fatalf("派生设备名不同的长名不应报撞名，得到：\n%s", errText(errs))
	}
}

// protocol icmp + 端口字段：两个数据面语义不同（VPP 按 ICMP type/code 解读，内核忽略端口
// 字段）——内核侧提交期拒绝，让用户显式选择。红-绿：修复前零报错（字段被静默忽略）。
func TestKernelDataPlaneRejectsIcmpPorts(t *testing.T) {
	mk := func(mode, proto, dport string) Config {
		return Config{
			System: &SystemConfig{DataPlane: mode},
			Acls: []Acl{{Name: "acl1", Rules: []AclRule{{Seq: 10, Source: "any", Destination: "any",
				Protocol: proto, DestinationPort: dport, Action: "permit"}}}},
		}
	}
	errs := Validate(mk(DataPlaneKernel, "icmp", "8"))
	if len(errs) == 0 {
		t.Fatalf("内核数据面下 protocol icmp + 端口字段应提交期拒绝")
	}
	txt := errText(errs)
	if !strings.Contains(txt, "acls[acl1].rules[10]") || !strings.Contains(txt, "icmp") {
		t.Fatalf("报错应点名该 ACL 规则与 icmp 语义分歧，得到：\n%s", txt)
	}
	if !strings.Contains(txt, "端口") || !strings.Contains(txt, "set system dataplane vpp") {
		t.Fatalf("报错应说明端口字段会被忽略并给出替代，得到：\n%s", txt)
	}
	// tcp 带端口不受影响（内核侧端口匹配是正常语义）；无端口的 icmp 规则也放行。
	if errs := Validate(mk(DataPlaneKernel, "tcp", "443")); len(errs) != 0 {
		t.Fatalf("tcp 带端口应放行，得到：\n%s", errText(errs))
	}
	if errs := Validate(mk(DataPlaneKernel, "icmp", "")); len(errs) != 0 {
		t.Fatalf("不带端口的 icmp 规则应放行，得到：\n%s", errText(errs))
	}
	// VPP 数据面照旧（端口即 ICMP type/code）。
	if errs := Validate(mk(DataPlaneVPP, "icmp", "8")); len(errs) != 0 {
		t.Fatalf("VPP 数据面下 icmp + 端口应放行，得到：\n%s", errText(errs))
	}
}

// `nat static` 没有转发域字段（模型只有 inside/outside 地址），内核侧 dnat 规则无作用域
// （不区分域）——配置里存在**多于一个具名转发域**时静态映射作用域不明（同一对地址可能落在
// 任一域），提交期拒绝并点名涉及的域；单域/全默认表（≤1 个具名域）无歧义，放行。
//
// 红-绿：修复前同一配置在内核数据面下零报错（「配了就是启用、作用域不明」）。
func TestKernelDataPlaneRejectsNatStaticWithMultipleDomains(t *testing.T) {
	// 两个具名域：两台 type=l3 交换机（名字即承载 L3 配置的 VRF 设备名，落点是同名 vrfs 条目）。
	mk := func(mode string, withStatic bool) Config {
		c := Config{
			System:          &SystemConfig{DataPlane: mode},
			Interfaces:      []InterfaceConfig{{Name: "ens192"}, {Name: "ens224"}},
			VirtualSwitches: []VirtualSwitch{{Name: "vs-a", Type: "l3"}, {Name: "vs-b", Type: "l3"}},
			Vrfs: []Vrf{
				{Name: "vs-a", L3Interfaces: []L3Interface{{Interface: "ens192", Addresses: []string{"10.0.0.1/24"}}}},
				{Name: "vs-b", L3Interfaces: []L3Interface{{Interface: "ens224", Addresses: []string{"203.0.113.1/24"}}}},
			},
		}
		if withStatic {
			c.Nat = &NatConfig{Static: []NatStatic{{InsideIP: "10.0.0.5", OutsideIP: "203.0.113.9"}}}
		}
		return c
	}
	// 两个具名域 + static ⇒ 拒绝（点名 nat.static、涉及的域与替代）。
	errs := Validate(mk(DataPlaneKernel, true))
	if len(errs) == 0 {
		t.Fatalf("两个具名转发域下的 nat static 应提交期拒绝（作用域不明）")
	}
	txt := errText(errs)
	if !strings.Contains(txt, "nat.static") || !strings.Contains(txt, "转发域") {
		t.Fatalf("报错应点名 nat.static 与转发域，得到：\n%s", txt)
	}
	if !strings.Contains(txt, "vs-a") || !strings.Contains(txt, "vs-b") {
		t.Fatalf("报错应点名涉及的域（vs-a、vs-b），得到：\n%s", txt)
	}
	if !strings.Contains(txt, "单域") || !strings.Contains(txt, "set system dataplane vpp") {
		t.Fatalf("报错应给替代（改为单域使用 / 切回 VPP），得到：\n%s", txt)
	}
	// 单个具名域 ⇒ 放行（static 的作用域无歧义）。
	one := mk(DataPlaneKernel, true)
	one.VirtualSwitches = one.VirtualSwitches[:1]
	one.Vrfs = one.Vrfs[:1]
	if errs := Validate(one); len(errs) != 0 {
		t.Fatalf("单具名域下的 nat static 应放行，得到：\n%s", errText(errs))
	}
	// 零具名域（全默认表）⇒ 放行。
	none := Config{System: &SystemConfig{DataPlane: DataPlaneKernel},
		Nat: &NatConfig{Static: []NatStatic{{InsideIP: "10.0.0.5", OutsideIP: "203.0.113.9"}}}}
	if errs := Validate(none); len(errs) != 0 {
		t.Fatalf("全默认表下（零具名域）nat static 应放行，得到：\n%s", errText(errs))
	}
	// 两个具名域但没有 nat static ⇒ 放行（域数量本身不是错误，不误伤）。
	if errs := Validate(mk(DataPlaneKernel, false)); len(errs) != 0 {
		t.Fatalf("无 nat static 的多域配置应放行，得到：\n%s", errText(errs))
	}
	// VPP 数据面不受该限制（1:1 静态映射是 VPP 的原生能力）。
	if errs := Validate(mk(DataPlaneVPP, true)); len(errs) != 0 {
		t.Fatalf("VPP 数据面下多域 + nat static 应放行，得到：\n%s", errText(errs))
	}
}

// 转发域判据（纯函数）表：显式 `gateway vrf` 按该名计入；派生网关 VRF 与同名条目去重（同一
// VRF 设备名 = 同一个域）；L3 族以 `vrfs` 条目为准（与同名 l3 交换机不重复计数，裸条目也计入）；
// l2 无网关不计入；顺序按声明序（报错可复现）。
func TestKernelNamedDomainsCriterion(t *testing.T) {
	gw := func(vrf string) *VSGateway {
		return &VSGateway{Addresses: []string{"192.168.99.1/24"}, Vrf: vrf}
	}
	cases := []struct {
		name string
		cfg  Config
		want []string
	}{
		{"零域（全默认表）", Config{}, nil},
		{"l2 无网关不计", Config{VirtualSwitches: []VirtualSwitch{{Name: "lan", Type: "l2"}}}, nil},
		{"l2 有网关 ⇒ 派生的网关 VRF 计入",
			Config{VirtualSwitches: []VirtualSwitch{{Name: "lan", Type: "l2", Gateway: gw("")}}},
			[]string{"vr-lan"}},
		{"显式 gateway vrf 用它",
			Config{VirtualSwitches: []VirtualSwitch{{Name: "lan", Type: "l2", Gateway: gw("vr-custom")}}},
			[]string{"vr-custom"}},
		{"vrfs 条目与同名 l3 交换机 ⇒ 同一域（不重复计数）",
			Config{
				VirtualSwitches: []VirtualSwitch{{Name: "vs-l3", Type: "l3"}},
				Vrfs:            []Vrf{{Name: "vs-l3"}},
			},
			[]string{"vs-l3"}},
		{"只有裸 vrfs 条目（无同名 l3 交换机）也计入",
			Config{Vrfs: []Vrf{{Name: "vs-bare"}}},
			[]string{"vs-bare"}},
		{"显式名与派生名撞同 ⇒ 同一域（去重）",
			Config{VirtualSwitches: []VirtualSwitch{
				{Name: "lan", Type: "l2", Gateway: gw("")},
				{Name: "other", Type: "l2", Gateway: gw("vr-lan")},
			}},
			[]string{"vr-lan"}},
		{"vrfs 名与另一台的派生网关名同 ⇒ 同一域（去重）",
			Config{
				VirtualSwitches: []VirtualSwitch{{Name: "lan", Type: "l2", Gateway: gw("")}},
				Vrfs:            []Vrf{{Name: "vr-lan"}},
			},
			[]string{"vr-lan"}},
		{"多域按声明序（vrfs 条目 + l2 网关）",
			Config{
				VirtualSwitches: []VirtualSwitch{
					{Name: "vs-a", Type: "l3"},
					{Name: "lan", Type: "l2", Gateway: gw("")},
				},
				Vrfs: []Vrf{{Name: "vs-a"}},
			},
			[]string{"vs-a", "vr-lan"}},
	}
	for _, tc := range cases {
		if got := kernelNamedDomains(tc.cfg); !slices.Equal(got, tc.want) {
			t.Fatalf("%s：kernelNamedDomains = %v，期望 %v", tc.name, got, tc.want)
		}
	}
}
