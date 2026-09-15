package api

// 决策 #79：登录/TLS 类语句的映射与安全行为守护。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

// TestLoginUserPasswordIsHashedNotStoredInPlaintext 是**安全**断言：
// `set system login user <n> password <pw>` 落库的必须是 PBKDF2 哈希，
// 且语句回显不得包含明文口令（否则终端输出/会话录制即泄露）。
func TestLoginUserPasswordIsHashedNotStoredInPlaintext(t *testing.T) {
	x, engine := newCLIKit(t)
	const pw = "Sup3rSecret!x"
	out := run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set system login user ops password "+pw+" class operator",
	)
	if strings.Contains(out, "%%") {
		t.Fatalf("语句应成功: %s", out)
	}
	if strings.Contains(out, pw) {
		t.Fatalf("回显不得包含明文口令:\n%s", out)
	}
	if !strings.Contains(out, "«已隐藏»") {
		t.Fatalf("回显应脱敏为占位符:\n%s", out)
	}

	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	var got string
	var cls string
	for _, u := range cfg.System.Login.Users {
		if u.Name == "ops" {
			got, cls = u.PasswordHash, u.Class
		}
	}
	if got == "" {
		t.Fatal("用户 ops 未落库")
	}
	if got == pw {
		t.Fatal("口令以明文落库——必须哈希")
	}
	if !strings.HasPrefix(got, "pbkdf2$") {
		t.Fatalf("应为 pbkdf2 哈希，实际 %q", got)
	}
	if !aaa.VerifyPassword(got, pw) {
		t.Fatal("哈希应能校验原口令")
	}
	if cls != "operator" {
		t.Fatalf("class 应为 operator，实际 %q", cls)
	}
}

// TestLoginUserPasswordPolicyEnforced 与 REST 建用户一致：先过口令策略。
func TestLoginUserPasswordPolicyEnforced(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set system login password-policy min-length 12",
	)
	// 预期失败：直接用 Execute（run 会把任何 %% 视为致命）
	out := x.Execute("admin", aaaClassSU, "ssh", "set system login user weak password short1 class operator").Output
	if !strings.Contains(out, "口令不满足策略") {
		t.Fatalf("短口令应被策略拒绝:\n%s", out)
	}
	// 已哈希值应被接受（load/克隆路径幂等，不二次哈希）
	out = run(t, x, "admin", aaaClassSU, "ssh",
		"set system login user h1 password pbkdf2$sha256$1000$c2FsdA==$aGFzaA== class operator")
	if strings.Contains(out, "%%") {
		t.Fatalf("已哈希值应直接接受:\n%s", out)
	}
}

// TestLoginClassAllowDenyAreArrays：allow/deny 是 []string，可多条、可按键删除。
func TestLoginClassAllowDenyAreArrays(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set system login class audit allow show",
		"set system login class audit allow help",
		"set system login class audit deny configure",
	)
	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	var allow, deny []string
	for _, c := range cfg.System.Login.Classes {
		if c.Name == "audit" {
			allow, deny = c.Allow, c.Deny
		}
	}
	if len(allow) != 2 || allow[0] != "show" || allow[1] != "help" {
		t.Fatalf("allow 应为 [show help]，实际 %v", allow)
	}
	if len(deny) != 1 || deny[0] != "configure" {
		t.Fatalf("deny 应为 [configure]，实际 %v", deny)
	}
	out := run(t, x, "admin", aaaClassSU, "ssh", "delete system login class audit allow show")
	if strings.Contains(out, "%%") {
		t.Fatalf("按键删除应成功: %s", out)
	}
}

// TestTLSCombinedCertAndKey：契约里一条语句同时给证书与私钥（模型是扁平字段，CLI 多一层 tls）。
func TestTLSCombinedCertAndKey(t *testing.T) {
	x, engine := newCLIKit(t)
	out := run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set system api tls cert-file /etc/nfvis/a.pem key-file /etc/nfvis/a.key",
	)
	if strings.Contains(out, "%%") {
		t.Fatalf("语句应成功: %s", out)
	}
	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.System.API.CertFile != "/etc/nfvis/a.pem" || cfg.System.API.KeyFile != "/etc/nfvis/a.key" {
		t.Fatalf("证书/私钥未落库: %+v", cfg.System.API)
	}
}

// TestCrossConnectRequiresTwoDeclaredPorts：cross-connect 引用已声明端口，
// 且必须恰为两个——否则 applier 取不到前两个端口会**静默什么都不做**（FR-NET-012）。
func TestCrossConnectRequiresTwoDeclaredPorts(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set virtual-switches xc type l2",
		"set virtual-switches xc ports 1 interface ens224",
	)
	// 缺端口 2 → 明确报错（而不是静默无效）
	out := x.Execute("admin", aaaClassSU, "ssh", "set virtual-switches xc cross-connect 1 2").Output
	if !strings.Contains(out, "未声明") {
		t.Fatalf("端口未声明应报错:\n%s", out)
	}
	run(t, x, "admin", aaaClassSU, "ssh", "set virtual-switches xc ports 2 interface ens192")
	out = run(t, x, "admin", aaaClassSU, "ssh", "set virtual-switches xc cross-connect 1 2")
	if strings.Contains(out, "%%") {
		t.Fatalf("两个端口就绪后应成功: %s", out)
	}
	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, vs := range cfg.VirtualSwitches {
		if vs.Name == "xc" {
			found = vs.CrossConnect
		}
	}
	if !found {
		t.Fatal("cross_connect 未置位")
	}
}

// TestContainerEnvIsMap：容器环境变量在模型里是 map，不是数组。
func TestContainerEnvIsMap(t *testing.T) {
	x, engine := newCLIKit(t)
	out := run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set container-functions ct image alpine:3.20",
		"set container-functions ct env A 1",
		"set container-functions ct env B 2",
	)
	if strings.Contains(out, "%%") {
		t.Fatalf("env 语句应成功: %s", out)
	}
	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cfg.ContainerFunctions {
		if c.Name == "ct" {
			if c.Env["A"] != "1" || c.Env["B"] != "2" {
				t.Fatalf("env 应为 map{A:1,B:2}，实际 %v", c.Env)
			}
			return
		}
	}
	t.Fatal("容器未落库")
}

// TestSplitFieldsQuoted：引号内空白不切分、引号不保留——SSH 公钥含空格，
// 这是 FR-CMP-016 的 CLI 注入路径能用的前提。
func TestSplitFieldsQuoted(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{`set a b`, []string{"set", "a", "b"}},
		{`set x ssh-key "ssh-ed25519 AAAA k@h"`, []string{"set", "x", "ssh-key", "ssh-ed25519 AAAA k@h"}},
		{`set x d "a  b" y`, []string{"set", "x", "d", "a  b", "y"}},
		{`set x d "a\"b"`, []string{"set", "x", "d", `a"b`}},
		{`set x d ""`, []string{"set", "x", "d", ""}},
		{`set x d "未闭合 引号`, []string{"set", "x", "d", "未闭合 引号"}},
		{`   `, nil},
	} {
		got := splitFieldsQuoted(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("%q → %q，期望 %q", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%q → %q，期望 %q", tc.in, got, tc.want)
			}
		}
	}
}
