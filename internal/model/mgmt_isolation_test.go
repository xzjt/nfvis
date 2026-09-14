package model

// FR-NET-002 / FR-SEC-001（决策 #71）：管理网卡须与数据面隔离。
// 指定 system.management.interface 后，commit 必须拒绝该网卡出现在任何数据面引用中。
//
// 基线夹具 validBase() 的数据面引用是 ens2f0；本测试用 ens160 作为管理网卡，
// 逐项把它塞进各类数据面引用来验证被拒绝，且不破坏夹具既有引用。

import "testing"

func withMgmtIface(c Config, ifname string) Config {
	c.System.Management.Interface = ifname
	return c
}

func TestValidateManagementIsolationPass(t *testing.T) {
	// 管理网卡未被任何数据面引用 → 通过
	mustNoErr(t, Validate(withMgmtIface(validBase(), "ens160")))

	// 未指定管理网卡 → 无从判定，保持既有行为
	mustNoErr(t, Validate(validBase()))
}

func TestValidateManagementIsolationRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		path   string
	}{
		{
			"管理网卡被声明为业务网卡（会被 VPP 接管）",
			func(c *Config) { c.Interfaces = append(c.Interfaces, InterfaceConfig{Name: "ens160"}) },
			"interfaces[1].name",
		},
		{
			"管理网卡作为 DPDK 设备参数覆盖对象",
			func(c *Config) {
				c.Vpp.DPDK.PerDev = append(c.Vpp.DPDK.PerDev, VppDevOverride{Interface: "ens160"})
			},
			"vpp.dpdk.per_dev[1].interface",
		},
		{
			"管理网卡作为交换机端口",
			func(c *Config) {
				c.VirtualSwitches[0].Ports = append(c.VirtualSwitches[0].Ports, VSwitchPort{Seq: 2, Interface: "ens160"})
			},
			"virtual-switches[0].ports[1].interface",
		},
		{
			"管理网卡作为 bond 成员",
			func(c *Config) { c.Bonds = []Bond{{Name: "bond0", Members: []string{"ens2f0", "ens160"}}} },
			"bonds[0].members[1]",
		},
		{
			"管理网卡作为 L3 接口",
			func(c *Config) {
				c.Vrfs = append(c.Vrfs, Vrf{Name: "vs-l3",
					L3Interfaces: []L3Interface{{Interface: "ens160", Addresses: []string{"10.20.0.1/24"}}}})
			},
			"vrfs[1].l3_interfaces[0].interface",
		},
		{
			"管理网卡作为镜像源端口",
			func(c *Config) {
				c.PortMirroring = []PortMirroring{{Name: "span1",
					Source: PMSource{Interface: "ens160", Direction: "both"}, Analyzer: "ens2f0"}}
			},
			"port-mirroring[0].source.interface",
		},
		{
			"管理网卡作为镜像分析端口",
			func(c *Config) {
				c.PortMirroring = []PortMirroring{{Name: "span1",
					Source: PMSource{Interface: "ens2f0", Direction: "both"}, Analyzer: "ens160"}}
			},
			"port-mirroring[0].analyzer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := withMgmtIface(validBase(), "ens160")
			tc.mutate(&cfg)
			mustErrContaining(t, Validate(cfg), tc.path, "不得用于数据面")
		})
	}
}

// 管理网卡名非法 → 拒绝。
func TestValidateManagementIfaceName(t *testing.T) {
	cfg := validBase()
	cfg.System.Management.Interface = "bad name!"
	mustErrContaining(t, Validate(cfg), "system.management.interface", "非法")
}

// FR-SYS-006（决策 #71）：并发连接上限不能为负（0 = 不限）。
func TestValidateMaxSessionsNonNegative(t *testing.T) {
	ok := validBase()
	ok.System.API = &APIConfig{MaxSessions: 0}
	mustNoErr(t, Validate(ok))

	ok2 := validBase()
	ok2.System.API = &APIConfig{MaxSessions: 64}
	mustNoErr(t, Validate(ok2))

	bad := validBase()
	bad.System.API = &APIConfig{MaxSessions: -1}
	mustErrContaining(t, Validate(bad), "system.api.max_sessions", "不能为负")
}
