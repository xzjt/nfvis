package model

import "testing"

// 决策 #326：交换机成员端口读视图的派生与来源标注。
func TestDerivedSwitchPortsSourcesAndDedup(t *testing.T) {
	cfg := Config{
		VirtualSwitches: []VirtualSwitch{{
			Name: "vs-x", Type: "l2",
			Ports: []VSwitchPort{
				{Seq: 1, Interface: "ens224", TrunkVlans: []int{100}},
				// 显式声明的 vNIC 成员：与 VNF 侧声明同名同 nic → 只出这一条（source=config）
				{Seq: 2, Vnf: "fw", VnfInterface: "eth0"},
			},
		}},
		VirtualMachineFunctions: []VMFunction{
			{Name: "fw", Interfaces: []VnfInterface{
				{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-x"}, // 与静态 ports 重复 → 去重
				{Name: "eth1", Type: "vhost-user", VirtualSwitch: "vs-x"}, // 派生
			}},
			{Name: "lb", Interfaces: []VnfInterface{
				{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-other"}, // 别的交换机 → 不出
			}},
		},
		ContainerFunctions: []ContainerFunction{
			{Name: "ct", Interfaces: []VnfInterface{
				{Name: "m0", Type: "memif", VirtualSwitch: "vs-x"}, // 派生
			}},
		},
	}
	got := DerivedSwitchPorts(cfg, "vs-x")
	if len(got) != 4 {
		t.Fatalf("应派生 4 条（config×2 + vnf×1 + container×1），实得 %d: %+v", len(got), got)
	}
	if got[0].Source != PortSourceConfig || got[0].Interface != "ens224" {
		t.Fatalf("第 0 条应为静态口: %+v", got[0])
	}
	if got[1].Source != PortSourceConfig || got[1].VNF != "fw" || got[1].VNFInterface != "eth0" {
		t.Fatalf("第 1 条应为显式声明的 vNIC 成员: %+v", got[1])
	}
	if got[2].Source != PortSourceVNF || got[2].VNF != "fw" || got[2].VNFInterface != "eth1" {
		t.Fatalf("第 2 条应为 VNF 派生（eth1）: %+v", got[2])
	}
	if got[3].Source != PortSourceContainer || got[3].Container != "ct" || got[3].ContainerInterface != "m0" {
		t.Fatalf("第 3 条应为容器派生: %+v", got[3])
	}
	// 去重：显式声明的 eth0 不得再作为派生条目出现。
	for _, p := range got {
		if p.Source != PortSourceConfig && p.VNF == "fw" && p.VNFInterface == "eth0" {
			t.Fatalf("已在静态 ports 声明的 vNIC 不应重复派生: %+v", p)
		}
	}
}

// 派生按 (名, vNIC) 升序，输出确定；VNF 增删后读视图随之变化。
func TestDerivedSwitchPortsDeterministic(t *testing.T) {
	cfg := Config{VirtualMachineFunctions: []VMFunction{
		{Name: "b", Interfaces: []VnfInterface{{Name: "eth1", VirtualSwitch: "vs"}}},
		{Name: "a", Interfaces: []VnfInterface{
			{Name: "eth1", VirtualSwitch: "vs"}, {Name: "eth0", VirtualSwitch: "vs"}}},
	}}
	got := DerivedSwitchPorts(cfg, "vs")
	want := [][2]string{{"a", "eth0"}, {"a", "eth1"}, {"b", "eth1"}}
	if len(got) != len(want) {
		t.Fatalf("应 %d 条，实得 %d", len(want), len(got))
	}
	for i, w := range want {
		if got[i].VNF != w[0] || got[i].VNFInterface != w[1] {
			t.Fatalf("第 %d 条应为 %v，实得 %+v", i, w, got[i])
		}
	}
	// 删掉 a/eth0 后不再出现（派生的准据是配置，不是缓存）。a 的 interfaces 为 [eth1, eth0]。
	cfg.VirtualMachineFunctions[1].Interfaces = cfg.VirtualMachineFunctions[1].Interfaces[:1]
	got = DerivedSwitchPorts(cfg, "vs")
	for _, p := range got {
		if p.VNF == "a" && p.VNFInterface == "eth0" {
			t.Fatalf("删除声明后不应再派生: %+v", p)
		}
	}
}

// 交换机未在配置中声明时：仅返回挂在它名下的派生条目（调用方据此给说明，而非丢弃）。
func TestDerivedSwitchPortsUndeclaredSwitch(t *testing.T) {
	cfg := Config{VirtualMachineFunctions: []VMFunction{
		{Name: "fw", Interfaces: []VnfInterface{{Name: "eth0", VirtualSwitch: "vs-l3"}}},
	}}
	got := DerivedSwitchPorts(cfg, "vs-l3")
	if len(got) != 1 || got[0].Source != PortSourceVNF {
		t.Fatalf("未声明的交换机也应按 VNF 声明派生出条目: %+v", got)
	}
	if n := DerivedSwitchPorts(cfg, "no-such"); len(n) != 0 {
		t.Fatalf("无任何声明的交换机应得空并集: %+v", n)
	}
}

// AttachedPortName：vhost-user→vh-<vm>-<nic>、memif→mf-<owner>-<nic>；sriov-vf 无法确定→空。
// 决策 #442（收口 R5-1）：**内核数据面**下容器 vNIC 的实际设备是 veth 宿主端（nfvisct…）——
// 端口视图按数据面如实给名（VPP 侧与 VM 侧逐字不变）。
func TestAttachedPortName(t *testing.T) {
	cfg := Config{
		VirtualMachineFunctions: []VMFunction{{Name: "fw", Interfaces: []VnfInterface{
			{Name: "eth0", Type: "vhost-user"}, {Name: "vf0", Type: "sriov-vf"},
		}}},
	}
	if got := AttachedPortName(cfg, PortSourceVNF, "fw", "eth0"); got != "vh-fw-eth0" {
		t.Fatalf("vhost-user 名应为 vh-fw-eth0，实得 %q", got)
	}
	if got := AttachedPortName(cfg, PortSourceVNF, "fw", "vf0"); got != "" {
		t.Fatalf("sriov-vf 无法确定 VPP 名，应为空，实得 %q", got)
	}
	if got := AttachedPortName(cfg, PortSourceContainer, "ct", "m0"); got != "mf-ct-m0" {
		t.Fatalf("VPP 侧 memif 名应为 mf-ct-m0，实得 %q", got)
	}

	// 内核数据面：容器 ⇒ veth 宿主端名（15 字符、前缀 nfvisct）；且**逐字等于**编排序的
	// ContainerVethNames 宿主端（读视图与编排同一份规则——单一真源）。
	kernel := cfg
	kernel.System = &SystemConfig{DataPlane: DataPlaneKernel}
	hostWant, peerWant := ContainerVethNames("ct", "m0")
	if len(hostWant) != 15 || hostWant[:7] != "nfvisct" || peerWant[:7] != "nfviscp" {
		t.Fatalf("veth 命名形如 <前缀>+8hex（15 字符），实得 host=%q peer=%q", hostWant, peerWant)
	}
	if got := AttachedPortName(kernel, PortSourceContainer, "ct", "m0"); got != hostWant {
		t.Fatalf("内核侧容器端口名应为 veth 宿主端 %q，实得 %q", hostWant, got)
	}
	if got := AttachedPortName(kernel, PortSourceContainer, "ct", "m0"); got == peerWant {
		t.Fatal("内核侧端口名应为宿主端，不是容器端")
	}
	// 内核数据面下 VM 侧 vNIC 名逐字不变（vhost-user 走 virtio+tap，无产品接口名；仍给 vh- 名）。
	if got := AttachedPortName(kernel, PortSourceVNF, "fw", "eth0"); got != "vh-fw-eth0" {
		t.Fatalf("内核侧 vhost-user 名应逐字不变（vh-fw-eth0），实得 %q", got)
	}
}

// VNICAttachedTo：删除派生端口的指引判据。
func TestVNICAttachedTo(t *testing.T) {
	cfg := Config{
		VirtualMachineFunctions: []VMFunction{{Name: "fw", Interfaces: []VnfInterface{
			{Name: "eth0", VirtualSwitch: "vs-x"},
		}}},
		ContainerFunctions: []ContainerFunction{{Name: "ct", Interfaces: []VnfInterface{
			{Name: "m0", VirtualSwitch: "vs-x"},
		}}},
	}
	cases := []struct {
		kind, owner, nic, vs string
		want                 bool
	}{
		{PortSourceVNF, "fw", "eth0", "vs-x", true},
		{PortSourceVNF, "fw", "", "vs-x", true}, // 未给 nic：该 VM 有任一 nic 挂本交换机即真
		{PortSourceVNF, "fw", "eth0", "vs-y", false},
		{PortSourceVNF, "nope", "eth0", "vs-x", false},
		{PortSourceContainer, "ct", "m0", "vs-x", true},
		{PortSourceContainer, "ct", "m0", "vs-y", false},
	}
	for _, c := range cases {
		if got := VNICAttachedTo(cfg, c.kind, c.owner, c.nic, c.vs); got != c.want {
			t.Errorf("VNICAttachedTo(%s,%s,%s,%s)=%v，期望 %v", c.kind, c.owner, c.nic, c.vs, got, c.want)
		}
	}
}
