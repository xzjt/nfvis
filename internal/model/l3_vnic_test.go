package model

// 已声明的 vNIC 可作 L3 接口（l3-interface）——round84 证据 §14 的断点。
//
// 由来：guest 的网关地址必须落在 guest 自己的口上。此前校验层把 l3-interface 限死在
// 物理口/bond/VLAN 子接口，于是网关只能配在别的接口上，而 VPP 只为「接收接口自己拥有的
// 地址」作答 ARP（同 VRF 另一接口的地址不代答）——实测 guest 100% Destination Host
// Unreachable、NAT inside 也永远不含该 vNIC。
//
// 判据只认配置里确实声明过的 vNIC（名字由 ifacename.go 的规则派生，与编排层同源）：
// 未声明/不存在/类型不产生 VPP 接口的名字、以及 tap 之类仍旧拒绝。

import (
	"strings"
	"testing"
)

// vnicL3Config 在 validBase 上追加一个承载 vNIC 地址的 VRF（网关落在 vNIC 自己的口上）。
func vnicL3Config(iface string) Config { return vnicL3ConfigFor(validBase(), iface) }

// vnicL3ConfigFor 与 vnicL3Config 同形，但在给定配置上追加 VRF（超长名用例需要先改名）。
func vnicL3ConfigFor(c Config, iface string) Config {
	c.Vrfs = append(c.Vrfs, Vrf{Name: "vs-nat",
		L3Interfaces: []L3Interface{{Interface: iface, Addresses: []string{"192.168.200.1/24"}}}})
	return c
}

func TestValidateL3InterfaceAcceptsDeclaredVnic(t *testing.T) {
	// VM 的 vhost-user vNIC：validBase 的 fw-vm/eth0。
	mustNoErr(t, Validate(vnicL3Config(VnfIfaceName("fw-vm", "eth0"))))
	mustNoErr(t, Validate(vnicL3Config("vh-fw-vm-eth0"))) // 字面量：规则若漂移本行即失败

	// 容器 memif vNIC：validBase 的 sbc-ct1/eth0。
	mustNoErr(t, Validate(vnicL3Config(MemifIfaceName("sbc-ct1", "eth0"))))
	mustNoErr(t, Validate(vnicL3Config("mf-sbc-ct1-eth0")))

	// 超长名的哈希回退路径：放行的必须是**同一规则**算出的名字，
	// 校验层若另抄一份「vh-<vm>-<vnic>」拼接，这里会因 >63 而被拒。
	long := validBase()
	long.VirtualMachineFunctions[0].Name = strings.Repeat("a", 60)
	mustNoErr(t, Validate(vnicL3ConfigFor(long, VnfIfaceName(strings.Repeat("a", 60), "eth0"))))
}

func TestValidateL3InterfaceRejectsUndeclaredVnic(t *testing.T) {
	// sriov-vf 的 vNIC 不进 VPP（无 vh- 口），其名字不得当 L3 接口。
	sriov := validBase()
	sriov.Interfaces = append(sriov.Interfaces, InterfaceConfig{Name: "ens2f2"})
	sriov.VirtualMachineFunctions[0].Interfaces = append(sriov.VirtualMachineFunctions[0].Interfaces,
		VnfInterface{Name: "eth2", Type: "sriov-vf", Sriov: &SriovBind{PhysicalInterface: "ens2f2", VFID: 0}})

	cases := []struct {
		name string
		cfg  func() Config
	}{
		{"VM 已存在但 vNIC 未声明", func() Config { return vnicL3Config("vh-fw-vm-eth1") }},
		{"VM 不存在", func() Config { return vnicL3Config("vh-ghost-vm-eth0") }},
		{"容器 vNIC 未声明", func() Config { return vnicL3Config("mf-sbc-ct1-eth1") }},
		{"形如哈希名的随机名字", func() Config { return vnicL3Config("vh-0123abcd") }},
		{"sriov-vf vNIC（无 VPP 侧接口）", func() Config { return vnicL3ConfigFor(sriov, "vh-fw-vm-eth2") }},
		{"tap（M5 证据里的 tap 作 inside 仍不可用）", func() Config { return vnicL3Config("tap0") }},
		{"未声明的物理口", func() Config { return vnicL3Config("ens9f9") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustErrContaining(t, Validate(tc.cfg()), "l3_interfaces", "不存在或不是物理口")
		})
	}
}

// 物理口/bond/VLAN 子接口的既有行为不得回退。
func TestValidateL3InterfaceKeepsPhysicalAndBond(t *testing.T) {
	// validBase 的 vs-mgmt 就是 ens2f0.100（交换机端口 ens2f0 的 VLAN 子接口）。
	mustNoErr(t, Validate(validBase()))

	// bond 仍可作 L3 接口。
	c := validBase()
	c.Interfaces = append(c.Interfaces, InterfaceConfig{Name: "ens2f1"})
	c.Bonds = []Bond{{Name: "bond1", Members: []string{"ens2f1"}}}
	c.Vrfs = append(c.Vrfs, Vrf{Name: "vs-wan",
		L3Interfaces: []L3Interface{{Interface: "bond1", Addresses: []string{"203.0.113.1/24"}}}})
	mustNoErr(t, Validate(c))
}
