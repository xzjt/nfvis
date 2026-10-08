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

// TestCrossConnectStatementLandsBool：cross-connect 是**开关**，语句形态与模型同源。
//
// 由来（round2 现场）：命令树曾把它声明成两个位置参数（`cross-connect <port-a> <port-b>`），
// 而模型字段是 bool（OpenAPI 同为 boolean）——值个数不匹配的写法落到通用遍历写出数组，
// 用户看到的是一句 `cannot unmarshal array into … cross_connect of type bool` 的内部报错；
// 端口身份本由该交换机 `ports` 列表承担（VPP 侧取前两个），语句里带端口号是多余的契约面。
// 现在语句是显式取值叶子（同 `set system kernel low-latency true`）：置位 true、清位 false、
// `delete … cross-connect` 清键，取值只认 true|false（其余在语句层给出可照做的报错）。
func TestCrossConnectStatementLandsBool(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh", "configure", "set virtual-switches xc type l2")
	crossConnectOf := func() bool {
		t.Helper()
		cfg, _, err := engine.Candidate()
		if err != nil {
			t.Fatal(err)
		}
		for _, vs := range cfg.VirtualSwitches {
			if vs.Name == "xc" {
				return vs.CrossConnect
			}
		}
		t.Fatal("交换机 xc 不在 candidate 里")
		return false
	}
	// 置位：不带端口号（端口由 ports 承担）
	if out := run(t, x, "admin", aaaClassSU, "ssh", "set virtual-switches xc cross-connect true"); strings.Contains(out, "%%") {
		t.Fatalf("cross-connect true 应成功: %s", out)
	}
	if !crossConnectOf() {
		t.Fatalf("语句成功但配置里 cross_connect 不为 true")
	}
	// 缺取值 / 非法取值都在**语句层**报可照做的错（不得落到 JSON 解码的内部报错）
	for _, bad := range []string{
		"set virtual-switches xc cross-connect",
		"set virtual-switches xc cross-connect 1",
		"set virtual-switches xc cross-connect 1 2", // 旧的两端口形态：取值不合法即拒
	} {
		out := x.Execute("admin", aaaClassSU, "ssh", bad).Output
		if !strings.Contains(out, "%%") {
			t.Fatalf("非法形态应报错 %q: %s", bad, out)
		}
		if strings.Contains(out, "cannot unmarshal") {
			t.Fatalf("不应把 JSON 解码内部报错抛给用户 %q: %s", bad, out)
		}
	}
	if !crossConnectOf() {
		t.Fatalf("被拒语句不得改动配置（cross_connect 应为 true）")
	}
	// 清位：false 与 delete 两种形态都落模型
	if out := run(t, x, "admin", aaaClassSU, "ssh", "set virtual-switches xc cross-connect false"); strings.Contains(out, "%%") {
		t.Fatalf("cross-connect false 应成功: %s", out)
	}
	if crossConnectOf() {
		t.Fatalf("cross-connect false 后 cross_connect 应回 false")
	}
	run(t, x, "admin", aaaClassSU, "ssh", "set virtual-switches xc cross-connect true")
	out := run(t, x, "admin", aaaClassSU, "ssh", "delete virtual-switches xc cross-connect")
	if strings.Contains(out, "%%") {
		t.Fatalf("delete … cross-connect 应成功: %s", out)
	}
	if crossConnectOf() {
		t.Fatalf("delete … cross-connect 后 cross_connect 应回 false")
	}
}

// TestCrossConnectDisplaySetRoundTrip：display set 反推与树同源——`cross-connect true`
// 能反推、能回放（回放自校验在 generateSetStmts 内；反推失败会报「display set 内部错误」）。
func TestCrossConnectDisplaySetRoundTrip(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaaClassSU, "ssh",
		"configure",
		"set virtual-switches xc type l2",
		"set virtual-switches xc cross-connect true")
	out := x.Execute("admin", aaaClassSU, "ssh", "show configuration candidate | display set").Output
	if strings.Contains(out, "内部错误") {
		t.Fatalf("display set 反推/回放失败: %s", out)
	}
	if !strings.Contains(out, "set virtual-switches xc cross-connect true") {
		t.Fatalf("display set 应反推出新形态语句:\n%s", out)
	}
	if strings.Contains(out, "cross-connect 1") || strings.Contains(out, "cross-connect 2") {
		t.Fatalf("不得再反推旧的两端口形态:\n%s", out)
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
