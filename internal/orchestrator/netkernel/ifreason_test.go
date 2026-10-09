package netkernel

// 决策 #431：内核数据面下「已声明却未进数据面」的原因事实（vfio 残留 / networkd 持有 down）。
// 本文件验事实层：networkd 单元的判据（纯函数 + 目录扫描）与 Provider 的两条判据合流。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

func TestNetworkdUnitHoldsDown(t *testing.T) {
	cases := []struct {
		name    string
		content string
		ifname  string
		want    bool
	}{
		{"匹配且强制 down", "[Match]\nName=ens224\n\n[Link]\nActivationPolicy=always-down\n", "ens224", true},
		{"匹配但策略非 down", "[Match]\nName=ens224\n\n[Link]\nActivationPolicy=up\n", "ens224", false},
		{"名字不匹配", "[Match]\nName=ens192\n\n[Link]\nActivationPolicy=always-down\n", "ens224", false},
		{"多名字段命中其一", "[Match]\nName=ens192 ens224\n\n[Link]\nActivationPolicy=always-down\n", "ens224", true},
		{"policy 在 Link 段之外不算", "[Match]\nName=ens224\n\n[Network]\nActivationPolicy=always-down\n", "ens224", false},
		{"ActivationPolicy=down 也算强制", "[Match]\nName=ens224\n\n[Link]\nActivationPolicy=down\n", "ens224", true},
	}
	for _, tc := range cases {
		if got := networkdUnitHoldsDown(tc.content, tc.ifname); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestNetworkdHoldsDownDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "10-netplan-ens224.network"),
		[]byte("[Match]\nName=ens224\n\n[Link]\nActivationPolicy=always-down\n"), 0o644); err != nil {
		t.Fatalf("写 networkd 单元: %v", err)
	}
	old := networkdNetDir
	networkdNetDir = dir
	t.Cleanup(func() { networkdNetDir = old })

	if held, err := NetworkdHoldsDown("ens224"); err != nil || !held {
		t.Fatalf("ens224 应被判定为 networkd 持有 down：held=%v err=%v", held, err)
	}
	if held, err := NetworkdHoldsDown("ens192"); err != nil || held {
		t.Fatalf("ens192 不应命中：held=%v err=%v", held, err)
	}
}

// Provider 合流：vfio 残留优先于 networkd 判据；两条都取不到时 ok=false（不猜）。
func TestProviderKernelIfNotInDPReason(t *testing.T) {
	// networkd 目录指向空目录，隔离宿主真实 /run/systemd/network。
	dir := t.TempDir()
	old := networkdNetDir
	networkdNetDir = dir
	t.Cleanup(func() { networkdNetDir = old })

	p := New(nil)
	p.SetHeldPortProbe(func(ifname string) (string, string, bool) {
		if ifname == "ens192" {
			return "vfio-pci", "0000:0b:00.0", true
		}
		return "", "", false
	})

	r, ok := p.KernelIfNotInDPReason("ens192")
	if !ok || r.Kind != network.KernelIfReasonVFIO || r.Driver != "vfio-pci" || r.PCI != "0000:0b:00.0" {
		t.Fatalf("vfio 残留应命中：ok=%v r=%+v", ok, r)
	}

	// 未命中 heldPort，且 networkd 目录为空 → 取不到。
	if r, ok := p.KernelIfNotInDPReason("ens224"); ok {
		t.Fatalf("取不到原因时应返回 ok=false：%+v", r)
	}

	// 写入 networkd 单元后，同一口改由 networkd 判据命中。
	if err := os.WriteFile(filepath.Join(dir, "10-netplan-ens224.network"),
		[]byte("[Match]\nName=ens224\n\n[Link]\nActivationPolicy=always-down\n"), 0o644); err != nil {
		t.Fatalf("写 networkd 单元: %v", err)
	}
	r2, ok := p.KernelIfNotInDPReason("ens224")
	if !ok || r2.Kind != network.KernelIfReasonNetworkdDown {
		t.Fatalf("networkd 持有 down 应命中：ok=%v r=%+v", ok, r2)
	}

	// 空名不判定。
	if _, ok := p.KernelIfNotInDPReason("  "); ok {
		t.Fatal("空名应返回 ok=false")
	}
}
