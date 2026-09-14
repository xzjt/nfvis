package api

// FR-SYS-004（决策 #69）：CLI `set system syslog host … facility/severity` 此前在命令树
// 与 CLI 契约中已声明、但执行器未映射（契约与实现漂移）。本测试守护补齐后的映射与校验。

import (
	"strings"
	"testing"
)

func TestCLISyslogHostFacilitySeverity(t *testing.T) {
	x, eng := newCLIKit(t)

	// 任意顺序、可组合：facility + severity + port
	run(t, x, "admin", "super-user", "ssh",
		"configure",
		"set system syslog host 10.0.0.9 port 514 facility daemon severity warn",
		"commit",
		"exit")

	cfg, err := eng.Committed()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.System == nil || cfg.System.Syslog == nil {
		t.Fatal("syslog 段未落模型")
	}
	sc := cfg.System.Syslog
	if sc.RemoteHost != "10.0.0.9" || sc.RemotePort != 514 || sc.Facility != "daemon" || sc.Severity != "warn" {
		t.Fatalf("host/port/facility/severity 未正确落模型: %+v", sc)
	}

	// 顺序颠倒同样成立
	run(t, x, "admin", "super-user", "ssh",
		"configure",
		"set system syslog host 10.0.0.10 severity error facility local0",
		"commit",
		"exit")
	cfg, _ = eng.Committed()
	sc = cfg.System.Syslog
	if sc.RemoteHost != "10.0.0.10" || sc.Facility != "local0" || sc.Severity != "error" {
		t.Fatalf("乱序参数未正确落模型: %+v", sc)
	}

	// 按关键字删除单个选项
	run(t, x, "admin", "super-user", "ssh",
		"configure", "delete system syslog host 10.0.0.10 facility local0", "commit", "exit")
	cfg, _ = eng.Committed()
	if cfg.System.Syslog.Facility != "" {
		t.Fatalf("facility 未删除: %+v", cfg.System.Syslog)
	}
	if cfg.System.Syslog.Severity != "error" {
		t.Fatalf("severity 不应被误删: %+v", cfg.System.Syslog)
	}
}

// 非法 facility / severity / 奇偶不配对 须在校验或执行期被拒。
func TestCLISyslogHostRejectsInvalid(t *testing.T) {
	x, _ := newCLIKit(t)

	out := x.Execute("admin", "super-user", "ssh",
		"set system syslog host 10.0.0.9 facility nosuchfacility").Output
	if !strings.Contains(out, "facility") {
		t.Fatalf("非法 facility 应报错: %q", out)
	}

	out = x.Execute("admin", "super-user", "ssh",
		"set system syslog host 10.0.0.9 severity verbose").Output
	if !strings.Contains(out, "severity") {
		t.Fatalf("非法 severity 应报错: %q", out)
	}

	// 缺值：由命令树先行拒绝（facility 是带值节点），不必落到别名的成对校验。
	// 断言"被拒绝"而非具体措辞——关键是不得静默接受。
	out = x.Execute("admin", "super-user", "ssh",
		"set system syslog host 10.0.0.9 facility").Output
	if !strings.Contains(out, "%") {
		t.Fatalf("缺值应被拒绝: %q", out)
	}
}

// 删除整目标时一并清除 facility/severity（不留孤儿字段）。
func TestCLISyslogHostDeleteClearsAll(t *testing.T) {
	x, eng := newCLIKit(t)
	run(t, x, "admin", "super-user", "ssh",
		"configure",
		"set system syslog host 10.0.0.9 port 514 facility daemon severity warn",
		"commit", "exit")
	run(t, x, "admin", "super-user", "ssh",
		"configure", "delete system syslog host 10.0.0.9", "commit", "exit")

	cfg, err := eng.Committed()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.System != nil && cfg.System.Syslog != nil {
		sc := cfg.System.Syslog
		if sc.RemoteHost != "" || sc.RemotePort != 0 || sc.Facility != "" || sc.Severity != "" {
			t.Fatalf("删除目标应清除全部远程转发字段: %+v", sc)
		}
	}
}
