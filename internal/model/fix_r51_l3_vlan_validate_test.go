package model

// round51 跟随项：L3 接口的 VLAN 子接口字段校验（每条都先红后绿）。
//
// 覆盖：
//   - `L3Interface.Vlan` 此前零范围校验 —— REST/load 可写 99999，下发层按 uint16 截断成另一个
//     VID，提交却报成功；
//   - 接口名的带点写法（`ens224.100`，下发层按它拆 vlan 子接口）此前只判「父口存在」——
//     点后不是合法 VID 时提交放行、下发找不到接口，失败还被归到 DPDK 绑定上；
//   - 同一子接口的两种写法（`ens224` + `vlan 100` 与 `ens224.100`）此前不查重。

import (
	"strings"
	"testing"
)

// fixR51L3Config 在 validBase 上造一条可控的 l3-interface 列表：追加一个未被任何角色占用的
// 物理口 ens2f1（validBase 的 ens2f0 已是 vs-app 的交换机端口，直接拿它当 L3 接口会踩端口
// 角色互斥，把别的报错混进来），其余校验环境保持不变。
func fixR51L3Config(lis ...L3Interface) Config {
	c := validBase()
	c.Interfaces = append(c.Interfaces, InterfaceConfig{Name: "ens2f1"})
	c.Vrfs[0].L3Interfaces = lis
	return c
}

// 1) Vlan 字段的范围：0 = 不建子接口（合法），其余须 1..4094。
func TestFixR51L3VlanRangeValidated(t *testing.T) {
	for _, bad := range []int{-1, -100, 4095, 65536, 99999} {
		c := fixR51L3Config(L3Interface{Interface: "ens2f1", Vlan: bad, Addresses: []string{"10.10.0.1/24"}})
		mustErrContaining(t, Validate(c), ".vlan", "必须在 1-4094")
	}
	// 负控：0（不建子接口）与边界内的 1/4094 照旧合法。
	for _, ok := range []int{0, 1, 4094} {
		c := fixR51L3Config(L3Interface{Interface: "ens2f1", Vlan: ok, Addresses: []string{"10.10.0.1/24"}})
		mustNoErr(t, Validate(c))
	}
}

// 2) 接口名的带点写法：点后必须是 1..4094 的 VID 且前缀非空，否则拒绝（合法写法不受影响）。
func TestFixR51L3DottedNameValidated(t *testing.T) {
	for _, name := range []string{"ens2f1.", ".100", "ens2f1.0", "ens2f1.4095", "ens2f1.99999", "ens2f1.v100", "ens2f1.100.200"} {
		c := fixR51L3Config(L3Interface{Interface: name, Addresses: []string{"10.10.0.1/24"}})
		mustErrContaining(t, Validate(c), "l3_interfaces[", "带点但不是合法的 vlan 子接口写法")
	}
	// 负控：合法写法（带点的 vlan 子接口、与不带点的父口）照旧放行。
	mustNoErr(t, Validate(fixR51L3Config(L3Interface{Interface: "ens2f1.100", Addresses: []string{"10.10.0.1/24"}})))
	mustNoErr(t, Validate(fixR51L3Config(L3Interface{Interface: "ens2f1", Addresses: []string{"10.10.0.1/24"}})))
}

// 3) 查重按**子接口身份**（`<接口>.<vlan>`）：同一子接口的两种写法是重复；同一口的不同 vlan 不是。
func TestFixR51L3DuplicateKeyedOnIdentity(t *testing.T) {
	// (a) 两种写法（`ens2f1` + vlan 100 与 `ens2f1.100`）是同一子接口 → 仍拒绝，且**只报一条**。
	// 两种次序都覆盖（先写字段式、或先写带点式）。
	for _, lis := range [][]L3Interface{
		{{Interface: "ens2f1", Vlan: 100, Addresses: []string{"10.10.0.1/24"}}, {Interface: "ens2f1.100", Addresses: []string{"10.10.1.1/24"}}},
		{{Interface: "ens2f1.100", Addresses: []string{"10.10.1.1/24"}}, {Interface: "ens2f1", Vlan: 100, Addresses: []string{"10.10.0.1/24"}}},
	} {
		errs := Validate(fixR51L3Config(lis...))
		// 判据一：有且只有一条「重复定义」（身份查重折叠两种写法 ⇒ 同一对不重复报）。
		if n := fixR51CountDup(errs); n != 1 {
			t.Fatalf("同一子接口的两种写法应恰好报一条重复定义，实得 %d 条: %v", n, errs)
		}
		// 判据二：点名的是**身份**（规范的 `<父口>.<vlan>` 名）。
		mustErrContaining(t, errs, "l3_interfaces[ens2f1.100]", "重复定义")
	}

	// (b) 完全同名（平铺写两遍同一条声明）→ 仍拒绝一次。
	same := []L3Interface{
		{Interface: "ens2f1", Vlan: 100, Addresses: []string{"10.10.0.1/24"}},
		{Interface: "ens2f1", Vlan: 100, Addresses: []string{"10.10.1.1/24"}},
	}
	if n := fixR51CountDup(Validate(fixR51L3Config(same...))); n != 1 {
		t.Fatalf("平铺重名应恰好报一条重复定义，实得 %d 条", n)
	}
	plain := []L3Interface{
		{Interface: "ens2f1", Addresses: []string{"10.10.0.1/24"}},
		{Interface: "ens2f1", Addresses: []string{"10.10.1.1/24"}},
	}
	if n := fixR51CountDup(Validate(fixR51L3Config(plain...))); n != 1 {
		t.Fatalf("无 vlan 的重名应恰好报一条重复定义，实得 %d 条", n)
	}
}

// [本轮放开] 同一物理口上的**不同 vlan 子接口**是两个子接口（此前被按字面查重误拒）。
func TestFixR51L3SamePortDifferentVlanAllowed(t *testing.T) {
	// 字段式（新 `vlan <v>` 语句能表达的写法）：vlan 100 与 vlan 200 各一个。
	mustNoErr(t, Validate(fixR51L3Config(
		L3Interface{Interface: "ens2f1", Vlan: 100, Addresses: []string{"10.10.0.1/24"}},
		L3Interface{Interface: "ens2f1", Vlan: 200, Addresses: []string{"10.10.1.1/24"}},
	)))
	// 带点式（既有写法，一直是合法的）：回归锚点。
	mustNoErr(t, Validate(fixR51L3Config(
		L3Interface{Interface: "ens2f1.100", Addresses: []string{"10.10.0.1/24"}},
		L3Interface{Interface: "ens2f1.200", Addresses: []string{"10.10.1.1/24"}},
	)))
	// 混合形态（一条字段式 + 一条带点式、vlan 不同）同样是两个子接口。
	mustNoErr(t, Validate(fixR51L3Config(
		L3Interface{Interface: "ens2f1", Vlan: 100, Addresses: []string{"10.10.0.1/24"}},
		L3Interface{Interface: "ens2f1.200", Addresses: []string{"10.10.1.1/24"}},
	)))

	// 角色互斥同口径：子接口与父口是两个接口（既有口径——带点写法一直是这么算的，
	// 见 port_role_test 的「ens2f0 作交换机端口、ens2f0.100 作 L3 接口 → 不冲突」）。
	// 叶式声明（`ens2f0` + vlan 100）只占子接口身份，故不与 vs-app 的 ens2f0 端口冲突。
	c := validBase()
	c.Vrfs[0].L3Interfaces = []L3Interface{{Interface: "ens2f0", Vlan: 100, Addresses: []string{"10.10.0.1/24"}}}
	mustNoErr(t, Validate(c))
}

// fixR51CountDup 数「重复定义」类报错（判「恰好报一条」用）。
func fixR51CountDup(errs []ValidateError) int {
	n := 0
	for _, e := range errs {
		if strings.Contains(e.Message, "重复定义") {
			n++
		}
	}
	return n
}
