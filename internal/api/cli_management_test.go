package api

// FR-SYS-001 / FR-NET-002 / FR-CFG-012（决策 #71）：管理口语句必须可在 CLI 上落实。
//
// 此前 `set system management ip address …` / `gateway …` 虽有命令树节点与 CLI 契约，
// 但**无别名映射**（树路径 `management ip address` vs 模型 `management.address` 多出 `ip` 段），
// 执行时报「语句未产生配置变更」——管理口 IP 在 CLI 上根本设不了。
//
// 另：管理口字段（含新增的网卡名）变更受 FR-CFG-012 自锁保护——来自 SSH 的普通 commit
// 必须被拒（改管理网卡可能直接切断当前会话），须以 commit confirmed 提交。

import (
	"strings"
	"testing"
)

func TestCLIManagementStatements(t *testing.T) {
	x, eng := newCLIKit(t)

	// 首次配置管理口（此前无 management 段）：SSH 普通 commit 允许（初始配置豁免）
	run(t, x, "admin", "super-user", "ssh",
		"configure",
		"set system management interface ens160",
		"set system management ip address 192.168.1.10/24",
		"set system management gateway 192.168.1.1",
		"commit",
		"exit")

	cfg, err := eng.Committed()
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.System.Management
	if m == nil {
		t.Fatal("management 段未落模型")
	}
	if m.Interface != "ens160" || m.Address != "192.168.1.10/24" || m.Gateway != "192.168.1.1" {
		t.Fatalf("管理口字段未正确落模型: %+v", m)
	}

	// show interfaces management 显示真实网卡名（此前硬编码 mgmt0）
	out := x.Execute("admin", "super-user", "ssh", "show interfaces management").Output
	if !strings.Contains(out, "ens160") {
		t.Fatalf("应显示真实管理网卡名: %q", out)
	}
	if strings.Contains(out, "mgmt0") {
		t.Fatalf("不应再出现硬编码的 mgmt0: %q", out)
	}

	// 再次变更管理口（含仅改网卡名）：SSH 普通 commit 须被拒（FR-CFG-012 自锁）
	run(t, x, "admin", "super-user", "ssh", "configure", "set system management interface ens161")
	if res := x.Execute("admin", "super-user", "ssh", "commit"); !strings.Contains(res.Output, "commit confirmed") {
		t.Fatalf("改管理网卡须要求 commit confirmed: %s", res.Output)
	}
	// 以 commit confirmed 提交则允许
	if res := x.Execute("admin", "super-user", "ssh", "commit confirmed 10"); res.Output == "" {
		t.Fatalf("commit confirmed 应有输出")
	}
	cfg, _ = eng.Committed()
	if cfg.System.Management.Interface != "ens161" {
		t.Fatalf("commit confirmed 后应生效: %+v", cfg.System.Management)
	}

	// 逐项删除（同样受自锁保护 → 用 commit confirmed）
	run(t, x, "admin", "super-user", "ssh", "configure", "delete system management interface ens161")
	if res := x.Execute("admin", "super-user", "ssh", "commit confirmed 10"); res.Output == "" {
		t.Fatalf("删除后 commit confirmed 应有输出")
	}
	cfg, _ = eng.Committed()
	if cfg.System.Management.Interface != "" {
		t.Fatalf("interface 未删除: %+v", cfg.System.Management)
	}
}

// 管理网卡被数据面引用 → commit 必须失败并逐条列出（FR-NET-002）。
func TestCLIManagementIsolationEnforced(t *testing.T) {
	x, _ := newCLIKit(t)
	out := run(t, x, "admin", "super-user", "ssh",
		"configure",
		"set system management interface ens192",
		"set interfaces ens192 description data-nic",
		"commit")
	if !strings.Contains(out, "不得用于数据面") {
		t.Fatalf("commit 应拒绝管理网卡进数据面: %s", out)
	}
}
