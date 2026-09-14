package api

import (
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

// 标量数组字段的 CLI 语句落地（dns_servers / kernel params / bond members）。
// 由缺陷驱动：这三类字段此前被遍历当作标量写入 → JSON 类型不符或静默无变化
// （DNS 配置无效、bond 成员无法配置）。见 reviews 2026-09-13 第六轮。
func TestCLIArrayScalarStatements(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set system dns server 1.1.1.1",
		"set system dns server 8.8.8.8",
		"set system kernel params intel_iommu=on",
		"set system kernel params iommu=pt",
		"set bonds bond1 members ens224",
		"set bonds bond1 members 2 ens192",
	)
	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读取 candidate: %v", err)
	}
	if len(cfg.System.DNSServers) != 2 || cfg.System.DNSServers[0] != "1.1.1.1" {
		t.Fatalf("dns_servers 落点异常: %v", cfg.System.DNSServers)
	}
	if cfg.System.Kernel == nil || len(cfg.System.Kernel.Params) != 2 {
		t.Fatalf("kernel params 落点异常: %+v", cfg.System.Kernel)
	}
	if len(cfg.Bonds) != 1 || len(cfg.Bonds[0].Members) != 2 ||
		cfg.Bonds[0].Members[0] != "ens224" || cfg.Bonds[0].Members[1] != "ens192" {
		t.Fatalf("bond members 落点异常: %+v", cfg.Bonds)
	}

	// 按值删除
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"delete system dns server 1.1.1.1",
		"delete system kernel params intel_iommu=on",
		"delete bonds bond1 members ens224",
	)
	cfg2, _, _ := engine.Candidate()
	if len(cfg2.System.DNSServers) != 1 || cfg2.System.DNSServers[0] != "8.8.8.8" {
		t.Fatalf("dns 按值删除异常: %v", cfg2.System.DNSServers)
	}
	if len(cfg2.System.Kernel.Params) != 1 || cfg2.System.Kernel.Params[0] != "iommu=pt" {
		t.Fatalf("params 按值删除异常: %v", cfg2.System.Kernel.Params)
	}
	if len(cfg2.Bonds[0].Members) != 1 || cfg2.Bonds[0].Members[0] != "ens192" {
		t.Fatalf("members 按值删除异常: %v", cfg2.Bonds[0].Members)
	}
}
