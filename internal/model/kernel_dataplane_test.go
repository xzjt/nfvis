package model

import (
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
		"DHCP 中继": func(c *Config) {
			c.VirtualSwitches = []VirtualSwitch{{Name: "vs", Type: "l2", DhcpRelayServer: "10.0.0.1"}}
		},
		"DHCP 服务器": func(c *Config) {
			c.VirtualSwitches = []VirtualSwitch{{Name: "vs", Type: "l2", DhcpServerPoolStart: "10.0.0.10"}}
		},
		"DNS 代理": func(c *Config) {
			c.VirtualSwitches = []VirtualSwitch{{Name: "vs", Type: "l2", DNSProxyServers: []string{"8.8.8.8"}}}
		},
		"学习上限": func(c *Config) {
			c.VirtualSwitches = []VirtualSwitch{{Name: "vs", Type: "l2", LearnLimit: 100}}
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
