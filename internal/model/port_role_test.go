package model

// 数据面角色互斥：一个网口在 {bond 成员, 交换机端口, L3 接口, 镜像源/分析口} 中只能出现一次。
//
// 由来（round84 干净快照走查）：同一物理口可同时被三条声明绑定且产品全静默接受——实测
// ens192 既作 bond0 成员又作 vs-l3 的 l3-interface 时，该口入向被 bond-input 吃光、不进
// bridge-domain，BD 转发整体失效（learned=0）并连锁到 guest 拿不到 DHCP，配置侧零报错。
// 手册 §8.9 早已声明该约束（成员口须未被虚拟交换机引用），此处把它落到 commit 校验。

import (
	"strings"
	"testing"
)

func TestValidatePortRoleExclusivityRejects(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		path    string // 报错落点（后声明的那个角色）
		msgPart string // 期望点名的冲突对象
	}{
		{
			"bond 成员 + 交换机端口 同口",
			func(c *Config) {
				// validBase 里 ens2f0 已是 vs-app 的端口；bond0 后声明 → 在端口处报冲突
				c.Bonds = []Bond{{Name: "bond0", Members: []string{"ens2f0"}}}
			},
			"virtual-switches[vs-app].ports[1].interface",
			"bond0 的成员口",
		},
		{
			"bond 成员 + 交换机端口 同口（交换机侧后加的口）",
			func(c *Config) {
				c.Bonds = []Bond{{Name: "bond0", Members: []string{"ens2f0"}}}
				c.VirtualSwitches[0].Ports = append(c.VirtualSwitches[0].Ports,
					VSwitchPort{Seq: 2, Interface: "ens2f0"})
			},
			"virtual-switches[vs-app].ports[2].interface",
			"bond0 的成员口",
		},
		{
			"bond 成员 + l3-interface 同口",
			func(c *Config) {
				c.Bonds = []Bond{{Name: "bond0", Members: []string{"ens2f0"}}}
				c.Vrfs = append(c.Vrfs, Vrf{Name: "vs-l3",
					L3Interfaces: []L3Interface{{Interface: "ens2f0", Addresses: []string{"192.168.155.10/24"}}}})
			},
			"vrfs[vs-l3].l3_interfaces[ens2f0].interface",
			"bond0 的成员口",
		},
		{
			"交换机端口 + 镜像源口 同口",
			func(c *Config) {
				c.PortMirroring = []PortMirroring{{Name: "span1",
					Source: PMSource{Interface: "ens2f0", Direction: "both"}, Analyzer: "ens192"}}
			},
			"port-mirroring[span1].source.interface",
			"虚拟交换机 vs-app 的端口",
		},
		{
			"L3 接口 + 镜像分析口 同口",
			func(c *Config) {
				c.Interfaces = append(c.Interfaces, InterfaceConfig{Name: "ens2f1"})
				c.Vrfs = append(c.Vrfs, Vrf{Name: "vs-l3",
					L3Interfaces: []L3Interface{{Interface: "ens2f1", Addresses: []string{"192.168.155.10/24"}}}})
				c.PortMirroring = []PortMirroring{{Name: "span1",
					Source: PMSource{Interface: "ens2f0", Direction: "both"}, Analyzer: "ens2f1"}}
			},
			"port-mirroring[span1].analyzer",
			"VRF vs-l3 的 L3 接口",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validBase()
			tc.mutate(&cfg)
			errs := Validate(cfg)
			mustErrContaining(t, errs, tc.path, "角色冲突")
			mustErrContaining(t, errs, tc.path, tc.msgPart)
		})
	}
}

// 管理口隔离（FR-NET-002）这条既有规则不得因新检查而回退：管理口仍被数据面引用时，
// 原有「不得用于数据面」报错必须照旧出现（新检查可另行叠加角色冲突，但不许把它顶掉）。
func TestValidatePortRoleExclusivityKeepsManagementIsolation(t *testing.T) {
	c := withMgmtIface(validBase(), "ens2f0") // ens2f0 已是 vs-app 的端口
	mustErrContaining(t, Validate(c), "virtual-switches[0].ports[0].interface", "不得用于数据面")

	c2 := withMgmtIface(validBase(), "ens2f0")
	c2.Bonds = []Bond{{Name: "bond0", Members: []string{"ens2f0"}}}
	mustErrContaining(t, Validate(c2), "bonds[0].members[0]", "不得用于数据面")
}

// 四个角色各占一口 → 通过。
func TestValidatePortRoleExclusivityPass(t *testing.T) {
	c := validBase()
	c.Interfaces = append(c.Interfaces, InterfaceConfig{Name: "ens2f1"},
		InterfaceConfig{Name: "ens2f2"}, InterfaceConfig{Name: "ens2f3"}, InterfaceConfig{Name: "ens2f4"})
	c.Bonds = []Bond{{Name: "bond0", Members: []string{"ens2f1"}}}
	c.VirtualSwitches[0].Ports = []VSwitchPort{{Seq: 1, Interface: "ens2f2"}}
	c.Vrfs = append(c.Vrfs, Vrf{Name: "vs-l3",
		L3Interfaces: []L3Interface{{Interface: "ens2f3", Addresses: []string{"192.168.155.10/24"}}}})
	c.PortMirroring = []PortMirroring{{Name: "span1",
		Source: PMSource{Interface: "ens2f0", Direction: "both"}, Analyzer: "ens2f4"}}
	mustNoErr(t, Validate(c))

	// VLAN 子接口与父口是两个接口（与 checkManagementIsolation 同口径）：
	// ens2f0 作交换机端口、ens2f0.100 作 L3 接口 → 不构成角色冲突。
	mustNoErr(t, Validate(validBase()))
}

// 真机套件的端口分配必须始终合法（cli-fulltest-phase2.sh 的「前置对象」+ 阶段 3 的 bond 提交）：
// 阶段 2 提交 vs-l3 的 L3 接口（ens224，192.168.155.10/24，阶段 5 的 ping source 用它），
// 阶段 3 再把 ens192 收进 bond0——两段合起来也必须通过，否则此后任何 commit（阶段 4/5/6）
// 都会被 commit 校验整体拒绝。本用例是那段端口分配的回归锚点：改动套件端口前先看这里。
func TestValidateSuitePortAllocationStaysLegal(t *testing.T) {
	pre := Config{
		Interfaces: []InterfaceConfig{{Name: "ens192"}, {Name: "ens224"}},
		Acls: []Acl{{Name: "acl-test", Rules: []AclRule{{Seq: 10, Source: "any", Destination: "any",
			Protocol: "tcp", DestinationPort: "443", Action: "permit"}}}},
		QosPolicies:     []QosPolicy{{Name: "pol-test", Cir: 1000000000, Cbs: 1000000}},
		VirtualSwitches: []VirtualSwitch{{Name: "vs-l2", Type: "l2"}, {Name: "vs-l3", Type: "l3"}},
		Vrfs: []Vrf{{Name: "vs-l3", L3Interfaces: []L3Interface{
			{Interface: "ens224", Addresses: []string{"192.168.155.10/24"}}}}},
	}
	mustNoErr(t, Validate(pre)) // 阶段 2 预块提交

	after := pre
	after.Bonds = []Bond{{Name: "bond0", Members: []string{"ens192"}}}
	mustNoErr(t, Validate(after)) // 阶段 3 叠上 bond0 后仍须合法
}

// 未声明的接口被两个角色引用时，只报「不存在/不是物理口」，不叠角色冲突噪声。
func TestValidatePortRoleExclusivitySkipsUnknownIface(t *testing.T) {
	c := validBase()
	c.Interfaces = nil
	c.Vpp.DPDK = nil
	c.VirtualSwitches[0].Ports = []VSwitchPort{{Seq: 1, Interface: "ens9f9"}}
	c.PortMirroring = []PortMirroring{{Name: "span1",
		Source: PMSource{Interface: "ens9f9", Direction: "both"}, Analyzer: "ens9f9"}}
	for _, e := range Validate(c) {
		if strings.Contains(e.Message, "角色冲突") {
			t.Fatalf("未声明的接口不应产生角色冲突：%v", e)
		}
	}
}
