package model

// vNIC 接口名规则（ifacename.go）的真源测试：编排层（internal/orchestrator）的
// VnfIfaceName/MemifIfaceName 只是转发，规则本身在此守护。
//
// 名字是**跨版本兼容事实**——VPP 侧接口按名解析（交换机端口、NAT inside、反查收敛），
// 规则一变既有安装的接口就成孤儿，故哈希回退路径以定值（golden）钉住。

import (
	"strings"
	"testing"
)

func TestVnfIfaceName(t *testing.T) {
	if got := VnfIfaceName("fw-vm", "eth0"); got != "vh-fw-vm-eth0" {
		t.Fatalf("接口名: %s", got)
	}
	// 超长（>63）→ 确定性哈希名，且不截断碰撞。
	longVM := strings.Repeat("a", 60)
	longIface := strings.Repeat("b", 60)
	got := VnfIfaceName(longVM, longIface)
	if len(got) > 63 || !strings.HasPrefix(got, "vh-") {
		t.Fatalf("超长应回退哈希名且 ≤63: %q(%d)", got, len(got))
	}
	if got != "vh-9dddf0b2" {
		t.Fatalf("哈希名已变（既有安装的 VPP 接口会成孤儿）: %s", got)
	}
	if got != VnfIfaceName(longVM, longIface) {
		t.Fatal("哈希名非确定")
	}
	if got == VnfIfaceName(longVM, longIface+"x") {
		t.Fatal("不同 vNIC 不应生成同名")
	}
}

func TestMemifIfaceName(t *testing.T) {
	if got := MemifIfaceName("ct-a", "eth0"); got != "mf-ct-a-eth0" {
		t.Fatalf("接口名: %s", got)
	}
	// memif 的哈希键前缀与 vhost-user 不同（"memif/"），超长名同样定值钉住。
	longVM := strings.Repeat("a", 60)
	longIface := strings.Repeat("b", 60)
	got := MemifIfaceName(longVM, longIface)
	if len(got) > 63 || !strings.HasPrefix(got, "mf-") {
		t.Fatalf("超长应回退哈希名且 ≤63: %q(%d)", got, len(got))
	}
	if got != "mf-fcab83db" {
		t.Fatalf("哈希名已变（既有安装的 VPP 接口会成孤儿）: %s", got)
	}
	if got != MemifIfaceName(longVM, longIface) {
		t.Fatal("哈希名非确定")
	}
}
