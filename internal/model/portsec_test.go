package model

// 决策 #389：端口安全白名单（per-port 允许 MAC 白名单）的校验。
// 条目：合法 MAC/大小写不敏感去重/上限 32；前置：L2 交换机静态成员、非 bond 成员；
// 互斥：与 L3 接口 ACL 的 macip 绑定槽（正向与反向两个方向）。

import (
	"strings"
	"testing"
)

func TestValidatePortSecurityEntries(t *testing.T) {
	// 未配置（空）合法。
	mustNoErr(t, Validate(validBase()))

	// 白名单挂 L2 交换机的静态成员口（validBase 的 ens2f0 是 vs-app 的端口）合法。
	c := validBase()
	c.Interfaces[0].PortSecurity = []PortSecMAC{"b0:b0:00:00:00:01", "b0:b0:00:00:00:02"}
	mustNoErr(t, Validate(c))

	// 非法 MAC：点名到条目路径。
	bad := validBase()
	bad.Interfaces[0].PortSecurity = []PortSecMAC{"b0:b0:00:00:00:01", "not-a-mac"}
	mustErrContaining(t, Validate(bad), "port_security[1]", "非法")

	// 重复（大小写不敏感）：直接构造绕过解码层归一，校验仍按归一比较。
	dup := validBase()
	dup.Interfaces[0].PortSecurity = []PortSecMAC{"B0:B0:00:00:00:01", "b0:b0:00:00:00:01"}
	mustErrContaining(t, Validate(dup), "port_security[1]", "重复")

	// 上限 32 条：33 条被拒、32 条通过。
	over := validBase()
	for i := 0; i < 33; i++ {
		over.Interfaces[0].PortSecurity = append(over.Interfaces[0].PortSecurity,
			PortSecMAC("b0:b0:00:00:00:"+string(rune('a'+i%26))+string(rune('0'+i/26))))
	}
	// 上面构造可能有重复副作用：直接按合法格式构建 33 条唯一 MAC（末字节 01..21 十六进制）。
	over.Interfaces[0].PortSecurity = nil
	for i := 1; i <= 33; i++ {
		over.Interfaces[0].PortSecurity = append(over.Interfaces[0].PortSecurity,
			PortSecMAC("b0:b0:00:00:00:"+hex2(i)))
	}
	mustErrContaining(t, Validate(over), "port_security", "最多 32 条")
	at32 := validBase()
	for i := 1; i <= 32; i++ {
		at32.Interfaces[0].PortSecurity = append(at32.Interfaces[0].PortSecurity,
			PortSecMAC("b0:b0:00:00:00:"+hex2(i)))
	}
	mustNoErr(t, Validate(at32))
}

func hex2(n int) string {
	const d = "0123456789abcdef"
	return string([]byte{d[(n>>4)&0xf], d[n&0xf]})
}

func TestValidatePortSecurityPreconditions(t *testing.T) {
	// 非 L2 成员口：白名单配在未进交换机、又非 bond 成员的声明口上 ⇒ 拒绝（文案含照做路径）。
	notMember := validBase()
	enabled := true
	notMember.Interfaces = append(notMember.Interfaces, InterfaceConfig{Name: "ens2f1", Enabled: &enabled})
	notMember.Interfaces[1].PortSecurity = []PortSecMAC{"b0:b0:00:00:00:01"}
	mustErrContaining(t, Validate(notMember), "port_security", "不是任何 L2 交换机的静态成员端口")

	// bond 成员口：白名单配在 bond 成员上 ⇒ 拒绝（无论是否另有 L2 成员问题，bond 判定必须在）。
	bonded := validBase()
	bonded.Bonds = []Bond{{Name: "bond0", Members: []string{"ens2f0"}}}
	bonded.VirtualSwitches[0].Ports = nil // 去掉端口，让 bond 判定成为唯一前置错误
	bonded.Interfaces[0].PortSecurity = []PortSecMAC{"b0:b0:00:00:00:01"}
	mustErrContaining(t, Validate(bonded), "port_security", "bond 成员口")
}

func TestValidatePortSecurityMacipSlotExclusion(t *testing.T) {
	// 正向：口已是 L3 接口且绑了 acl-in ⇒ 配白名单被拒（同一 macip 绑定槽）。
	fwd := validBase()
	fwd.Vrfs[0].L3Interfaces = []L3Interface{{Interface: "ens2f0", Addresses: []string{"10.10.0.1/24"}, AclIn: "acl-web"}}
	fwd.Interfaces[0].PortSecurity = []PortSecMAC{"b0:b0:00:00:00:01"}
	errs := Validate(fwd)
	mustErrContaining(t, errs, "port_security", "macip 绑定槽")

	// 反向：口已配白名单 ⇒ 给同口作为 l3-interface 绑 acl-in 也被拒。
	rev := validBase()
	rev.Interfaces[0].PortSecurity = []PortSecMAC{"b0:b0:00:00:00:01"}
	rev.Vrfs[0].L3Interfaces = []L3Interface{{Interface: "ens2f0", Addresses: []string{"10.10.0.1/24"}, AclIn: "acl-web"}}
	errs = Validate(rev)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "端口安全") && strings.Contains(e.Message, "acl") {
			found = true
		}
	}
	if !found {
		t.Fatalf("反向（l3-interface 绑 ACL）应被拒并点名端口安全；实际：%v", errs)
	}

	// 说明（实现口径，防回归）：同一网口本就有**角色互斥**（交换机端口 vs L3 接口不能并存），
	// 故「白名单口 + 同口 L3 ACL」在配置层面已不可达——上面的 macip 槽互斥是**纵深防御**
	// （底座确实只有一个 macip 绑定槽；将来任何放宽角色互斥的改动都必须先过这道检查）。
	// 因该不可达性，「无 acl-in 且不冲突」的反例无合法现场可构造，故意省略（不留假用例）。
}

// 决策 #401（R176-2）：同接口「端口安全白名单 × 风暴抑制」的 L2 入向分类槽互斥（portsec 侧）。
// 与 L3-ACL 的 macip 槽互斥同族：两者都绑接口唯一的 L2 入向分类槽（portsec 走 macip、storm 走
// classify），并存会静默失效并让先配者的删除撞「槽被占用」；提交期硬拒（反向在 checkInterfaces）。
func TestValidatePortSecurityStormMutex(t *testing.T) {
	both := validBase()
	both.Interfaces[0].PortSecurity = []PortSecMAC{"b0:b0:00:00:00:01"}
	both.Interfaces[0].StormControl = &StormControl{BroadcastKbps: 8000}
	errs := Validate(both)
	// 错误落在 portsec 侧字段路径，点名两者 + 机理 + 两条照做路径。
	mustErrContaining(t, errs, "interfaces[ens2f0].port_security", "风暴抑制")
	mustErrContaining(t, errs, "interfaces[ens2f0].port_security", "L2 入向分类槽")
	mustErrContaining(t, errs, "interfaces[ens2f0].port_security", "delete interfaces ens2f0 port-security")
	mustErrContaining(t, errs, "interfaces[ens2f0].port_security", "delete interfaces ens2f0 storm-control")

	// 只留白名单（无风暴抑制）：合法——互斥只针对并存。
	only := validBase()
	only.Interfaces[0].PortSecurity = []PortSecMAC{"b0:b0:00:00:00:01"}
	mustNoErr(t, Validate(only))
}
