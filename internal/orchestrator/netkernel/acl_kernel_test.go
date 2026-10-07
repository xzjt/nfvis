package netkernel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// ---------- 命名 ----------

func TestACLChainNameSanitizesAndAvoidsCollision(t *testing.T) {
	if got := aclChainName("web"); got != "acl_web" {
		t.Fatalf("常规名应原样派生，得到 %q", got)
	}
	if aclChainName("web") != aclChainName("web") {
		t.Fatalf("同一输入必须得到同一链名（确定性）")
	}
	// `.`/`-` 不在 nft 可移植标识符里，替换为 `_` 后必须靠后缀区分，否则撞链。
	a, b, c := aclChainName("a.b"), aclChainName("a-b"), aclChainName("a_b")
	if a == b || a == c || b == c {
		t.Fatalf("替换过字符的名字不应撞链：%q / %q / %q", a, b, c)
	}
	if aclBindChainName("ens192") != "bind_ens192" {
		t.Fatalf("绑定链名不符")
	}
}

// ---------- 规则翻译 ----------

func TestACLRuleArgsTranslation(t *testing.T) {
	cases := []struct {
		name string
		rule model.AclRule
		want []string
	}{
		{"v4 tcp 双端口", model.AclRule{Seq: 1, Source: "10.0.0.0/8", Destination: "10.1.1.1",
			Protocol: "tcp", SourcePort: "1024-65535", DestinationPort: "80", Action: "permit"},
			[]string{"ip", "saddr", "10.0.0.0/8", "ip", "daddr", "10.1.1.1",
				"meta", "l4proto", "tcp", "tcp", "sport", "1024-65535", "tcp", "dport", "80", "accept"}},
		{"v4 udp 拒绝", model.AclRule{Seq: 1, Protocol: "udp", DestinationPort: "53", Action: "deny"},
			[]string{"meta", "l4proto", "udp", "udp", "dport", "53", "drop"}},
		{"v4 icmp", model.AclRule{Seq: 1, Protocol: "icmp", Action: "permit"},
			[]string{"ip", "protocol", "icmp", "accept"}},
		{"v6 icmp", model.AclRule{Seq: 1, Source: "2001:db8::/32", Protocol: "icmp", Action: "deny"},
			[]string{"ip6", "saddr", "2001:db8::/32", "ip6", "nexthdr", "ipv6-icmp", "drop"}},
		{"any 侧跟随 v6", model.AclRule{Seq: 1, Destination: "2001:db8::1",
			Protocol: "tcp", DestinationPort: "443", Action: "permit"},
			[]string{"ip6", "daddr", "2001:db8::1", "meta", "l4proto", "tcp", "tcp", "dport", "443", "accept"}},
		{"协议不限但写端口", model.AclRule{Seq: 1, DestinationPort: "8080", Action: "permit"},
			[]string{"meta", "l4proto", "{", "tcp,", "udp", "}", "th", "dport", "8080", "accept"}},
		{"any/any", model.AclRule{Seq: 1, Action: "deny"}, []string{"drop"}},
		{"any 字面量", model.AclRule{Seq: 1, Source: "any", Destination: "any", Protocol: "any", Action: "permit"},
			[]string{"accept"}},
	}
	for _, c := range cases {
		got, err := aclRuleArgs(c.rule)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%s: 期望 %v，得到 %v", c.name, c.want, got)
		}
	}
}

func TestACLRuleArgsRejectsMixedFamilyAndBadAction(t *testing.T) {
	if _, err := aclRuleArgs(model.AclRule{Seq: 1, Source: "10.0.0.0/8", Destination: "2001:db8::1", Action: "permit"}); err == nil {
		t.Fatalf("两侧显式不同地址族应报错")
	}
	if _, err := aclRuleArgs(model.AclRule{Seq: 1, Action: "allow"}); err == nil {
		t.Fatalf("非法 action 应报错")
	}
}

// ---------- Apply ----------

func TestACLApplyEmitsRulesInSeqOrderAndImplicitDeny(t *testing.T) {
	f := &fakeRunner{}
	m := newACLManager(f)
	acl := model.Acl{Name: "web", Rules: []model.AclRule{
		{Seq: 20, Protocol: "tcp", DestinationPort: "443", Action: "permit"},
		{Seq: 10, Protocol: "tcp", DestinationPort: "80", Action: "permit"},
	}}
	if err := m.Apply(context.Background(), acl); err != nil {
		t.Fatal(err)
	}
	joined := f.joined()
	for _, want := range []string{
		"nft add table netdev nfvis-acl",
		"nft add chain netdev nfvis-acl acl_web",
		"nft flush chain netdev nfvis-acl acl_web",
		"nft add rule netdev nfvis-acl acl_web meta l4proto tcp tcp dport 80 accept",
		"nft add rule netdev nfvis-acl acl_web meta l4proto tcp tcp dport 443 accept",
		"nft add rule netdev nfvis-acl acl_web drop",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺少命令 %q；实际：\n%s", want, joined)
		}
	}
	// Seq 顺序即匹配顺序：Seq=10 必须先于 Seq=20 下发。
	i80, i443 := strings.Index(joined, "tcp dport 80"), strings.Index(joined, "tcp dport 443")
	if i80 < 0 || i443 < 0 || i80 > i443 {
		t.Fatalf("规则未按 Seq 顺序下发：\n%s", joined)
	}
}

func TestACLApplyRejectsEmptyName(t *testing.T) {
	if err := newACLManager(&fakeRunner{}).Apply(context.Background(), model.Acl{}); err == nil {
		t.Fatalf("空 ACL 名应报错")
	}
}

// ---------- Bind ----------

func TestACLBindInstallsNonIPPermitAndJump(t *testing.T) {
	f := &fakeRunner{}
	m := newACLManager(f)
	if err := m.Bind(context.Background(), "ens192", model.Acl{Name: "web"}); err != nil {
		t.Fatal(err)
	}
	joined := f.joined()
	for _, want := range []string{
		"nft add chain netdev nfvis-acl bind_ens192 { type filter hook ingress device ens192 priority filter ; }",
		"nft add rule netdev nfvis-acl bind_ens192 meta protocol != { ip, ip6 } accept",
		`nft add rule netdev nfvis-acl bind_ens192 jump acl_web comment "web"`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺少命令 %q；实际：\n%s", want, joined)
		}
	}
}

func TestACLBindRequiresAppliedACL(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "nft list chain netdev nfvis-acl acl_web",
		out:    "No such file or directory",
		err:    errors.New("exit status 1"),
	}}}
	err := newACLManager(f).Bind(context.Background(), "ens192", model.Acl{Name: "web"})
	if err == nil || !strings.Contains(err.Error(), "尚未下发") {
		t.Fatalf("未下发的 ACL 应报错并点明顺序，得到 %v", err)
	}
}

// ---------- Unbind ----------

func TestACLUnbindDeletesRulesByHandle(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "nft -a list chain netdev nfvis-acl bind_ens192",
		out: "table netdev nfvis-acl {\n" +
			"\tchain bind_ens192 { # handle 4\n" +
			"\t\ttype filter hook ingress device \"ens192\" priority filter; policy accept;\n" +
			"\t\tmeta protocol != { ip, ip6 } accept # handle 6\n" +
			"\t\tjump acl_web comment \"web\" # handle 7\n" +
			"\t}\n}\n",
	}}}
	if err := newACLManager(f).Unbind(context.Background(), "ens192"); err != nil {
		t.Fatal(err)
	}
	joined := f.joined()
	// 链声明行的 handle 4 不能被当成规则删（否则会删到链句柄）。
	if strings.Contains(joined, "handle 4") {
		t.Fatalf("不应删除链句柄；实际：\n%s", joined)
	}
	for _, want := range []string{
		"nft delete rule netdev nfvis-acl bind_ens192 handle 6",
		"nft delete rule netdev nfvis-acl bind_ens192 handle 7",
		"nft delete chain netdev nfvis-acl bind_ens192",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺少命令 %q；实际：\n%s", want, joined)
		}
	}
}

func TestACLUnbindToleratesMissingChain(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "nft -a list chain netdev nfvis-acl bind_ens192",
		out:    "No such file or directory",
		err:    errors.New("exit status 1"),
	}}}
	if err := newACLManager(f).Unbind(context.Background(), "ens192"); err != nil {
		t.Fatalf("绑定链不存在应按已达成，得到 %v", err)
	}
}

// ---------- Bound ----------

func TestACLBoundParsesCommentFromLiveRuleset(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "nft list chain netdev nfvis-acl bind_ens192",
		out: "table netdev nfvis-acl {\n" +
			"\tchain bind_ens192 {\n" +
			"\t\ttype filter hook ingress device \"ens192\" priority filter; policy accept;\n" +
			"\t\tmeta protocol != { ip, ip6 } accept\n" +
			"\t\tjump acl_web comment \"web\"\n" +
			"\t}\n}\n",
	}}}
	got, ok := newACLManager(f).Bound(context.Background(), "ens192")
	if !ok || got != "web" {
		t.Fatalf("Bound 应报绑定 web，得到 %q/%v", got, ok)
	}
}

func TestACLBoundNotBound(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "nft list chain netdev nfvis-acl bind_ens192",
		out:    "No such file or directory",
		err:    errors.New("exit status 1"),
	}}}
	if got, ok := newACLManager(f).Bound(context.Background(), "ens192"); ok || got != "" {
		t.Fatalf("未绑定应报 (%q,false)，得到 %q/%v", "", got, ok)
	}
}

// ---------- Delete ----------

func TestACLDeleteRefusesWhenStillBound(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "nft list table netdev nfvis-acl",
		out: "table netdev nfvis-acl {\n" +
			"\tchain acl_web {\n\t\tdrop\n\t}\n" +
			"\tchain bind_ens192 {\n\t\tjump acl_web comment \"web\"\n\t}\n}\n",
	}}}
	err := newACLManager(f).Delete(context.Background(), "web")
	if err == nil || !strings.Contains(err.Error(), "ens192") {
		t.Fatalf("仍被绑定时删除应报错并点名接口，得到 %v", err)
	}
	if f.has("nft delete chain netdev nfvis-acl acl_web") {
		t.Fatalf("被拒绝时不应删除链；实际：\n%s", f.joined())
	}
}

func TestACLDeleteRemovesChainWhenUnbound(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "nft list table netdev nfvis-acl",
		out:    "table netdev nfvis-acl {\n\tchain acl_web {\n\t\tdrop\n\t}\n}\n",
	}}}
	if err := newACLManager(f).Delete(context.Background(), "web"); err != nil {
		t.Fatal(err)
	}
	if !f.has("nft delete chain netdev nfvis-acl acl_web") {
		t.Fatalf("未绑定时应删除链；实际：\n%s", f.joined())
	}
}
