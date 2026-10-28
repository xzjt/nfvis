package api

// M16（round51 评审）：可选修饰关键字被当成必需参数的取值，静默写出错误配置。
//
// `set system ntp server prefer` 曾被别名规则 `{"system","ntp","server","*"}` 收下，
// 建出一台**名字叫 prefer 的服务器**（模型校验只拒绝空服务器名，commit 照样通过）。
// 命令树已把 prefer 标成 `server` 下的 `Opt` 可选修饰（BUG-21），但执行器不看它，
// 于是「? 补全能列出的形态」与「能执行并写出正确配置的形态」不一致。
//
// 契约（本文件钉住）：
//  1. 修饰当取值 ⇒ **拒绝**，且文案给出规范形态（主参数占位符 + 修饰名），不得静默建元素；
//  2. 规范序 `server <ip> prefer` 仍可用，且 prefer 真的落成 true；
//  3. 带自身内容的修饰仍是别名表有意支持的形态（分步 trunk），不得误伤。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

// TestNTPModifierAsServerValueRejected 裸 flag 修饰（prefer）当取值：拒绝且给出规范写法。
func TestNTPModifierAsServerValueRejected(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "configure")

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "set system ntp server prefer")
	if !strings.Contains(res.Output, "语句不完整") {
		t.Fatalf("把可选修饰当主参数取值应被拒绝（曾静默建出名为 prefer 的服务器）：%q", res.Output)
	}
	for _, want := range []string{"system ntp server", "<ip|host>", "prefer"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("报错应含规范形态的 %q：%q", want, res.Output)
		}
	}
	if strings.Contains(res.Output, "[ok]") {
		t.Fatalf("不得回报成功：%q", res.Output)
	}
	// 候选里不得出现名为 prefer 的 NTP 服务器。
	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读 candidate: %v", err)
	}
	if cfg.System != nil {
		for _, srv := range cfg.System.Ntp {
			if srv.Server == "prefer" {
				t.Fatalf("不得建出名字叫 prefer 的服务器: %+v", cfg.System.Ntp)
			}
		}
	}
}

// TestNTPCanonicalPreferStillWorks 规范序（主参数在前）必须仍可用，且 prefer 落成 true。
func TestNTPCanonicalPreferStillWorks(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set system ntp server 10.0.0.1 prefer",
		"set system ntp server 10.0.0.2 prefer")

	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读 candidate: %v", err)
	}
	if cfg.System == nil || cfg.System.Ntp == nil {
		t.Fatalf("应落 system.ntp: %+v", cfg.System)
	}
	byName := map[string]bool{}
	for _, s := range cfg.System.Ntp {
		byName[s.Server] = s.Prefer
	}
	if !byName["10.0.0.1"] || !byName["10.0.0.2"] {
		t.Fatalf("两个服务器的 prefer 都应为 true: %+v", cfg.System.Ntp)
	}
}

// TestNTPPlainServerWithoutPreferStillWorks 不带修饰的裸地址仍可用（既有形态不回归）。
func TestNTPPlainServerWithoutPreferStillWorks(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set system ntp server 10.0.0.3")

	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读 candidate: %v", err)
	}
	found := false
	for _, s := range cfg.System.Ntp {
		if s.Server == "10.0.0.3" {
			found = true
			if s.Prefer {
				t.Errorf("未给 prefer 时不得置为首选: %+v", s)
			}
		}
	}
	if !found {
		t.Fatalf("应落 10.0.0.3: %+v", cfg.System.Ntp)
	}
}

// TestModifierGuardSparesStagedContentModifier 反向守卫：带自身内容的修饰仍是**别名表有意支持**
// 的分步形态（`ports <seq> interface trunk vlans <list>`），主参数取值位的判据不得误伤它。
func TestModifierGuardSparesStagedContentModifier(t *testing.T) {
	x, _ := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set interfaces ens192",
		"set virtual-switches vs-lan type l2",
		"set virtual-switches vs-lan ports 5 interface ens192")

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh",
		"set virtual-switches vs-lan ports 5 interface trunk vlans 300")
	if strings.HasPrefix(res.Output, "%%") {
		t.Fatalf("分步 trunk（修饰带自身内容）不得被误伤：%s", res.Output)
	}
}
