package netkernel

import (
	"strings"
	"testing"
)

// R2-21：派生内核名的反例——两个**合法**（≤15，提交期自己校验的上限）的 13 字符交换机名
// 共享 7 字符前缀时，旧实现（截到 10 字符 + 16 位 FNV 哈希）会派生**同一个** `vr-…` 设备名
// （同一张 VRF 表）——两个交换机合并成一个，且离线可构造（FNV-1a 可逆）。
//
// 这两个名字对应的 16 位哈希值相同（低 16 位 0x85c6），是报告 §2 R2-21 描述的反例的具体化
// （报告只给了形态，未给字面名字；本用例用同一形态离线构造）。
func TestDerivedNamesDoNotCollideForLegalObjectNames(t *testing.T) {
	const (
		swA = "sw-test000221"
		swB = "sw-test001060"
	)
	if len(swA) > 15 || len(swB) > 15 {
		t.Fatalf("反例名必须是合法对象名（≤15）")
	}
	devA, devB := GatewayVRFName(swA), GatewayVRFName(swB)
	if devA == devB {
		t.Fatalf("两个合法交换机名派生出同一内核设备名 %q（旧实现：截 10 字符 + 16 位哈希）", devA)
	}
	if len(devA) > ifnameMax || len(devB) > ifnameMax {
		t.Fatalf("派生名超过内核上限：%q(%d) %q(%d)", devA, len(devA), devB, len(devB))
	}
	// 前缀不同也必须区分（哈希不同自然区分；这里同时钉住前缀预算被纳入长度计算）。
	if LinkName(swA) == LinkName(swB) {
		t.Fatalf("LinkName 也不得让两个合法名相撞")
	}
}

// 截断名保留名字前缀（便于排障）+ 全宽 32 位哈希（8 位十六进制），总长仍 ≤15。
func TestLinkNameTruncationBudget(t *testing.T) {
	long := "a-very-long-virtual-switch-name"
	got := LinkName(long)
	if len(got) > ifnameMax {
		t.Fatalf("截断后长度超限：%q（%d）", got, len(got))
	}
	if !strings.HasPrefix(got, "a-very") {
		t.Fatalf("截断名应保留名字前缀（纳入了长度预算），得到 %q", got)
	}
	if !strings.Contains(got, "-") || len(strings.Split(got, "-")[len(strings.Split(got, "-"))-1]) != 8 {
		t.Fatalf("截断名应带 8 位十六进制哈希后缀，得到 %q", got)
	}
	if LinkName(long) != got {
		t.Fatalf("同一输入必须得到同一名字（确定性）")
	}
	if LinkName("b-very-long-virtual-switch-name") == got {
		t.Fatalf("不同长名不应撞名")
	}
}

// R2-21：`VlanSubifName` 的派生名超过内核接口名上限（15）时必须**返回错误**，
// 而不是给一个内核装不上的名字（旧实现直接返回，错误要到后面的 `ip link add` 才出现、
// 且指不到「基口名太长」；提交期对派生名的校验由校验侧负责，本函数是下发/回收路径的第二道）。
func TestVlanSubifNameLengthLimit(t *testing.T) {
	ok, err := VlanSubifName("ens192", 100)
	if err != nil || ok != "ens192.100" {
		t.Fatalf("合法长度应原样返回，得到 %q, %v", ok, err)
	}
	// 15 字符基口 + ".100" = 19 —— 超出。
	long := "abcdefghijklmno"
	if _, err := VlanSubifName(long, 100); err == nil {
		t.Fatalf("超长子接口名必须报错（基口 %q + vlan）", long)
	} else if !strings.Contains(err.Error(), "15") {
		t.Fatalf("错误文案应点名内核上限，得到 %v", err)
	}
	// 边界：13 + ".10" = 16 超；12 + ".10" = 15 恰好可用。
	if _, err := VlanSubifName("abcdefghijklm", 10); err == nil {
		t.Fatalf("16 字符应报错")
	}
	exact, err := VlanSubifName("abcdefghijkl", 10)
	if err != nil || len(exact) != 15 {
		t.Fatalf("恰好 15 字符应通过，得到 %q, %v", exact, err)
	}
}

// R2-13③：`alreadyExists` 只认真实观察到的「已存在」短语——旧实现里的裸子串 "exist"
// 会把「does not exist」判成「已存在」，幂等路径于是把失败当成功。
func TestAlreadyExistsDoesNotMatchDoesNotExist(t *testing.T) {
	nf := errStub{}
	if alreadyExists(`Device "ens192" does not exist.`, nf) {
		t.Fatalf("does not exist 不能被判成 alreadyExists")
	}
	if alreadyExists(`Error: Device "ens192" does not exist.`, nf) {
		t.Fatalf("does not exist 不能被判成 alreadyExists")
	}
	// 真实观察到的「已存在」文案仍必须命中。
	for _, s := range []string{
		"RTNETLINK answers: File exists",
		"Error: Address already assigned",
		"Error: device 'bond0' already exists",
	} {
		if !alreadyExists(s, nf) {
			t.Fatalf("%q 应被判成 alreadyExists", s)
		}
	}
}

// notFound 与 alreadyExists 必须互斥（同一文案不能两边都命中）。
func TestExistMatchersAreDisjoint(t *testing.T) {
	nf := errStub{}
	for _, s := range []string{
		"RTNETLINK answers: File exists",
		"Cannot find device \"bond0\"",
		`Device "ens192" does not exist.`,
		"Error: Parent Qdisc doesn't exists.",
		"No such file or directory",
	} {
		if alreadyExists(s, nf) == notFound(s, nf) {
			t.Fatalf("文案 %q 在 alreadyExists/notFound 上判定相同（%v/%v）",
				s, alreadyExists(s, nf), notFound(s, nf))
		}
	}
}

// errStub 非 nil 的错误占位（两个匹配函数都要求 err != nil）。
type errStub struct{}

func (errStub) Error() string { return "exit status 1" }
