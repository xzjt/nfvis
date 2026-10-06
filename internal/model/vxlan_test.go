package model

// VXLAN 隧道校验表（决策 #383）：合法通过；越界/同址/重复/L3 交换机/被引用的交换机被删
// 各拒绝并给出可照做的下一步。

import (
	"strings"
	"testing"
)

func vxlanBase() Config {
	return Config{
		VirtualSwitches: []VirtualSwitch{{Name: "vs-l2", Type: "l2"}, {Name: "vs-l3", Type: "l3"}},
		Vrfs:            []Vrf{{Name: "vs-l3"}},
		VxlanTunnels: []VxlanTunnel{
			{Name: "tun-a", Vni: 100, Local: "10.99.0.1", Remote: "10.99.0.2", VirtualSwitch: "vs-l2"},
			{Name: "tun-b", Vni: 200, Local: "10.99.0.1", Remote: "10.99.0.3", DstPort: 5789},
		},
	}
}

func TestValidateVxlanPass(t *testing.T) {
	mustNoErr(t, Validate(vxlanBase()))
	// 缺省端口（0 = 4789）与显式 4789 等价、都合法
	c := vxlanBase()
	c.VxlanTunnels = append(c.VxlanTunnels, VxlanTunnel{Name: "tun-c", Vni: 300, Local: "10.99.1.1", Remote: "10.99.1.2", DstPort: 4789})
	mustNoErr(t, Validate(c))
}

func TestValidateVxlanVniRange(t *testing.T) {
	for _, vni := range []int{0, -1, 16777216} {
		c := vxlanBase()
		c.VxlanTunnels = []VxlanTunnel{{Name: "tun-a", Vni: vni, Local: "10.99.0.1", Remote: "10.99.0.2"}}
		mustErrContaining(t, Validate(c), "tun-a].vni", "VNI")
	}
	// 边界值合法
	for _, vni := range []int{1, 16777215} {
		c := vxlanBase()
		c.VxlanTunnels = []VxlanTunnel{{Name: "tun-a", Vni: vni, Local: "10.99.0.1", Remote: "10.99.0.2"}}
		mustNoErr(t, Validate(c))
	}
}

func TestValidateVxlanAddressFamilyAndEquality(t *testing.T) {
	c := vxlanBase()
	c.VxlanTunnels = []VxlanTunnel{{Name: "tun-a", Vni: 100, Local: "2001:db8::1", Remote: "10.99.0.2"}}
	mustErrContaining(t, Validate(c), "tun-a].local", "IPv4")

	c = vxlanBase()
	c.VxlanTunnels = []VxlanTunnel{{Name: "tun-a", Vni: 100, Local: "10.99.0.1", Remote: "10.99.0.1"}}
	mustErrContaining(t, Validate(c), "tun-a].remote", "不能相同")

	c = vxlanBase()
	c.VxlanTunnels = []VxlanTunnel{{Name: "tun-a", Vni: 100, Local: "10.99.0.999", Remote: "10.99.0.2"}}
	mustErrContaining(t, Validate(c), "tun-a].local", "IPv4")
}

func TestValidateVxlanDstPortRange(t *testing.T) {
	for _, port := range []int{-1, 65536} {
		c := vxlanBase()
		c.VxlanTunnels = []VxlanTunnel{{Name: "tun-a", Vni: 100, Local: "10.99.0.1", Remote: "10.99.0.2", DstPort: port}}
		mustErrContaining(t, Validate(c), "tun-a].dst_port", "端口")
	}
	for _, port := range []int{1, 65535} {
		c := vxlanBase()
		c.VxlanTunnels = []VxlanTunnel{{Name: "tun-a", Vni: 100, Local: "10.99.0.1", Remote: "10.99.0.2", DstPort: port}}
		mustNoErr(t, Validate(c))
	}
}

func TestValidateVxlanDuplicates(t *testing.T) {
	// name 重复
	c := vxlanBase()
	c.VxlanTunnels = []VxlanTunnel{
		{Name: "tun-a", Vni: 100, Local: "10.99.0.1", Remote: "10.99.0.2"},
		{Name: "tun-a", Vni: 200, Local: "10.99.0.1", Remote: "10.99.0.3"},
	}
	mustErrContaining(t, Validate(c), "tun-a", "重复")

	// vni 重复（元组不同也拒绝：VPP 侧同一 VNI 只能有一条隧道）
	c = vxlanBase()
	c.VxlanTunnels = []VxlanTunnel{
		{Name: "tun-a", Vni: 100, Local: "10.99.0.1", Remote: "10.99.0.2"},
		{Name: "tun-b", Vni: 100, Local: "10.99.0.1", Remote: "10.99.0.3"},
	}
	mustErrContaining(t, Validate(c), "tun-b].vni", "VNI 100")

	// (vni, local, remote) 元组重复（name 不同也拒绝）
	c = vxlanBase()
	c.VxlanTunnels = []VxlanTunnel{
		{Name: "tun-a", Vni: 100, Local: "10.99.0.1", Remote: "10.99.0.2"},
		{Name: "tun-b", Vni: 100, Local: "10.99.0.1", Remote: "10.99.0.2", DstPort: 4789},
	}
	mustErrContaining(t, Validate(c), "tun-b", "元组")
}

func TestValidateVxlanVirtualSwitchRef(t *testing.T) {
	// 引用的交换机必须是 L2（L3 没有 BD）
	c := vxlanBase()
	c.VxlanTunnels = []VxlanTunnel{{Name: "tun-a", Vni: 100, Local: "10.99.0.1", Remote: "10.99.0.2", VirtualSwitch: "vs-l3"}}
	mustErrContaining(t, Validate(c), "tun-a].virtual_switch", "L3")

	// 引用的交换机不存在（含「被隧道引用的交换机不得删除」的照做路径）
	c = vxlanBase()
	c.VxlanTunnels = []VxlanTunnel{{Name: "tun-a", Vni: 100, Local: "10.99.0.1", Remote: "10.99.0.2", VirtualSwitch: "vs-gone"}}
	errs := Validate(c)
	mustErrContaining(t, errs, "tun-a].virtual_switch", "delete vxlan tunnels tun-a virtual-switch")
}

// 「被隧道引用的交换机不得删除」：把交换机从配置里删掉、隧道仍在 ⇒ 提交期拒绝，
// 且错误指向解引用语句（与既有「先解引用、后删被引用」口径一致）。
func TestValidateVxlanReferencedSwitchCannotBeDeleted(t *testing.T) {
	c := vxlanBase()                                                 // 含 vs-l2 与引用它的 tun-a
	c.VirtualSwitches = []VirtualSwitch{{Name: "vs-l3", Type: "l3"}} // 删掉 vs-l2
	errs := Validate(c)
	if len(errs) == 0 {
		t.Fatal("删除被隧道引用的交换机应被拒绝，实际通过校验")
	}
	mustErrContaining(t, errs, "tun-a].virtual_switch", "仍被本 VXLAN 隧道引用")
	mustErrContaining(t, errs, "tun-a].virtual_switch", "delete vxlan tunnels tun-a virtual-switch")
}

// 反向：先解引用（delete vxlan tunnels tun-a virtual-switch）后交换机可删——配置里隧道不再引用它。
func TestValidateVxlanAfterUnbindSwitchDelete(t *testing.T) {
	c := vxlanBase()
	c.VirtualSwitches = []VirtualSwitch{{Name: "vs-l3", Type: "l3"}}
	c.VxlanTunnels = []VxlanTunnel{{Name: "tun-a", Vni: 100, Local: "10.99.0.1", Remote: "10.99.0.2"}} // virtual-switch 已解引用
	mustNoErr(t, Validate(c))
}

// 数据面身份是接口 tag（固定宽度 64 字节、含 NUL 至多 63）——超长会被静默截断、读回对不上，
// 故按数据面上限在提交期拒绝（名字最长 = 63 − len("nfvis-vxlan:")）。
func TestValidateVxlanTagLength(t *testing.T) {
	maxName := VxlanTagMaxLen - len(VxlanTagPrefix)
	if got := (VxlanTunnel{Name: strings.Repeat("a", maxName)}).DataPlaneTag(); len(got) != VxlanTagMaxLen {
		t.Fatalf("边界名字的 tag 长度 = %d，期望 %d", len(got), VxlanTagMaxLen)
	}
	c := vxlanBase()
	c.VxlanTunnels = []VxlanTunnel{{Name: strings.Repeat("a", maxName), Vni: 100, Local: "10.99.0.1", Remote: "10.99.0.2"}}
	mustNoErr(t, Validate(c))

	c = vxlanBase()
	c.VxlanTunnels = []VxlanTunnel{{Name: strings.Repeat("a", maxName+1), Vni: 100, Local: "10.99.0.1", Remote: "10.99.0.2"}}
	mustErrContaining(t, Validate(c), "].name", "过长")
}

// DataPlaneTag 的形状（平台前缀 + 名；识别与打标两侧同源）。
func TestVxlanDataPlaneTag(t *testing.T) {
	tun := VxlanTunnel{Name: "tun-a"}
	if got, want := tun.DataPlaneTag(), "nfvis-vxlan:tun-a"; got != want {
		t.Fatalf("DataPlaneTag = %q，期望 %q", got, want)
	}
	if !strings.HasPrefix(tun.DataPlaneTag(), VxlanTagPrefix) {
		t.Fatal("tag 必须带平台前缀（运行态按前缀过滤）")
	}
}
