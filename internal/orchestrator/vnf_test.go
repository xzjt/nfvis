package orchestrator

import (
	"strings"
	"testing"
)

func TestVnfSocketPath(t *testing.T) {
	if got := VnfSocketPath("/run/nfvis/vhost", "fw-vm", "eth0"); got != "/run/nfvis/vhost/fw-vm-eth0.sock" {
		t.Fatalf("socket 路径: %s", got)
	}
}

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
	if got != VnfIfaceName(longVM, longIface) {
		t.Fatal("哈希名非确定")
	}
	if got == VnfIfaceName(longVM, longIface+"x") {
		t.Fatal("不同 vNIC 不应生成同名")
	}
}

func TestVnfPortTag(t *testing.T) {
	if got := VnfPortTag("fw-vm", "eth0"); got != "nfvis:vnf:fw-vm:eth0" {
		t.Fatalf("tag: %s", got)
	}
}
