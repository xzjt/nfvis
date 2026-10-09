package netkernel

import (
	"context"
	"errors"
	"os"
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
		// 两侧皆 any（未写地址）⇒ 规则按 v4 落：补 `ip saddr 0.0.0.0/0`（手册「地址家族」段与
		// VPP 侧同口径；不补会同时命中 v6，见 aclRuleArgs 注释）。
		{"两侧 any + udp 拒绝", model.AclRule{Seq: 1, Protocol: "udp", DestinationPort: "53", Action: "deny"},
			[]string{"ip", "saddr", "0.0.0.0/0", "meta", "l4proto", "udp", "udp", "dport", "53", "drop"}},
		{"v4 icmp", model.AclRule{Seq: 1, Protocol: "icmp", Action: "permit"},
			[]string{"ip", "protocol", "icmp", "accept"}},
		{"v6 icmp", model.AclRule{Seq: 1, Source: "2001:db8::/32", Protocol: "icmp", Action: "deny"},
			[]string{"ip6", "saddr", "2001:db8::/32", "ip6", "nexthdr", "ipv6-icmp", "drop"}},
		{"any 侧跟随 v6", model.AclRule{Seq: 1, Destination: "2001:db8::1",
			Protocol: "tcp", DestinationPort: "443", Action: "permit"},
			[]string{"ip6", "daddr", "2001:db8::1", "meta", "l4proto", "tcp", "tcp", "dport", "443", "accept"}},
		{"协议不限但写端口", model.AclRule{Seq: 1, DestinationPort: "8080", Action: "permit"},
			[]string{"ip", "saddr", "0.0.0.0/0", "meta", "l4proto", "{", "tcp,", "udp", "}", "th", "dport", "8080", "accept"}},
		{"any/any", model.AclRule{Seq: 1, Action: "deny"}, []string{"ip", "saddr", "0.0.0.0/0", "drop"}},
		{"any 字面量", model.AclRule{Seq: 1, Source: "any", Destination: "any", Protocol: "any", Action: "permit"},
			[]string{"ip", "saddr", "0.0.0.0/0", "accept"}},
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

// 两侧皆 any 的规则必须显式落 v4（安全面，R2-4）：nft 规则体没有家族限定时会**同时命中 v4 与
// v6**——`rule 10 permit any/any` 把 v6 一并放行，排在其后的 `deny <v6 前缀>` 永不达（seq 小的
// 先命中），v6 策略被整体绕过。手册与 VPP 侧的口径是「any 跟随对侧家族；两侧都 any 按 v4」。
//
// 红-绿：修复前 any/any 的规则体是裸 `accept`/`meta l4proto tcp`（零家族限定 ⇒ 用例红）。
func TestACLRuleArgsPinsAnyAnyToV4(t *testing.T) {
	// ① any/any + permit：出现 `ip`（且不出现 `ip6`）。
	got, err := aclRuleArgs(model.AclRule{Seq: 1, Source: "any", Destination: "any", Action: "permit"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "ip saddr 0.0.0.0/0 accept" {
		t.Fatalf("any/any permit 应显式落 v4（ip saddr 0.0.0.0/0），得到 %v", got)
	}
	aclAssertFamilyV4(t, "any/any permit", got)

	// ② any/any + protocol tcp（未写地址、未写端口）：同样要有 v4 限定，且不出现 ip6。
	got, err = aclRuleArgs(model.AclRule{Seq: 1, Protocol: "tcp", Action: "permit"})
	if err != nil {
		t.Fatal(err)
	}
	if !aclArgsHaveFamily(got) || got[0] != "ip" {
		t.Fatalf("any/any + tcp 应显式落 v4，得到 %v", got)
	}
	aclAssertFamilyV4(t, "any/any tcp", got)
	// 写了端口/协议不限的两种写法同样要落 v4（同一判据覆盖三条分支）。
	for _, r := range []model.AclRule{
		{Seq: 1, Protocol: "udp", DestinationPort: "53", Action: "permit"},
		{Seq: 1, DestinationPort: "8080", Action: "permit"},
	} {
		got, err = aclRuleArgs(r)
		if err != nil {
			t.Fatal(err)
		}
		aclAssertFamilyV4(t, "any/any 带端口", got)
	}
	// icmp 分支自带的 `ip protocol icmp` 就是 v4 限定，不重复补（也不得落成 v6）。
	got, err = aclRuleArgs(model.AclRule{Seq: 1, Protocol: "icmp", Action: "deny"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "ip protocol icmp drop" {
		t.Fatalf("any/any + icmp 应保持单一 v4 限定（不重复补、不落 v6），得到 %v", got)
	}

	// ③ 一侧显式 v6 的既有语义不回归：any 跟随 v6、整条写成 ip6。
	got, err = aclRuleArgs(model.AclRule{Seq: 1, Destination: "2001:db8::1", Protocol: "tcp", Action: "permit"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "ip6 daddr 2001:db8::1 meta l4proto tcp accept" {
		t.Fatalf("一侧显式 v6 应整条按 v6（any 跟随对侧家族），得到 %v", got)
	}
	// ④ 显式 v4+v4 不变：不额外补全 v4 任意地址。
	got, err = aclRuleArgs(model.AclRule{Seq: 1, Source: "10.0.0.0/8", Destination: "10.1.1.1", Action: "permit"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "ip saddr 10.0.0.0/8 ip daddr 10.1.1.1 accept" {
		t.Fatalf("显式 v4 规则应保持原样（不追加 0.0.0.0/0），得到 %v", got)
	}
}

// aclAssertFamilyV4 断言一组规则参数显式落在 v4：出现独立关键字 `ip`、且不出现 `ip6`。
func aclAssertFamilyV4(t *testing.T, name string, args []string) {
	t.Helper()
	for _, a := range args {
		if a == "ip6" {
			t.Fatalf("%s：规则体不应出现 ip6 限定（会同时/错误命中 v6），得到 %v", name, args)
		}
	}
	if !aclArgsHaveFamily(args) || args[0] != "ip" {
		t.Fatalf("%s：规则体应以 `ip` 限定开头（两侧 any 按 v4），得到 %v", name, args)
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

// batchCaptureRunner 在真实调用 `nft -f <path>` 的**那一刻**读取该文件内容并留存：
// 实现用 defer 删除批量文件，调用结束后文件已不存在，故只能在 Run 里取内容。
// 同时记录 -f 的文件路径，供「调用后文件确实被删除」的断言使用。
type batchCaptureRunner struct {
	fakeRunner
	batches []string
	paths   []string
	readErr error
}

func (r *batchCaptureRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "nft" && len(args) >= 2 && args[0] == "-f" {
		r.paths = append(r.paths, args[1])
		data, err := os.ReadFile(args[1])
		if err != nil {
			r.readErr = err
		} else {
			r.batches = append(r.batches, string(data))
		}
	}
	return r.fakeRunner.Run(ctx, name, args...)
}

// singleBatch 断言恰好读到一份批量内容并返回它（读不到/多份即失败）。
func (r *batchCaptureRunner) singleBatch(t *testing.T) string {
	t.Helper()
	if r.readErr != nil {
		t.Fatalf("调用 nft -f 时批量文件应存在且可读，读取失败：%v", r.readErr)
	}
	if len(r.batches) != 1 {
		t.Fatalf("应恰好读到一份批量文件内容，实得 %d；调用：\n%s", len(r.batches), r.joined())
	}
	return r.batches[0]
}

func TestACLApplyEmitsRulesInSeqOrderAndImplicitDeny(t *testing.T) {
	f := &batchCaptureRunner{}
	m := newACLManager(f)
	acl := model.Acl{Name: "web", Rules: []model.AclRule{
		{Seq: 20, Protocol: "tcp", DestinationPort: "443", Action: "permit"},
		{Seq: 10, Protocol: "tcp", DestinationPort: "80", Action: "permit"},
	}}
	if err := m.Apply(context.Background(), acl); err != nil {
		t.Fatal(err)
	}
	// 表/链的确保仍是独立命令（幂等，不涉及「链为空」窗口）。
	joined := f.joined()
	for _, want := range []string{
		"nft add table netdev nfvis-acl",
		"nft add chain netdev nfvis-acl acl_web",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺少命令 %q；实际：\n%s", want, joined)
		}
	}
	// flush + 全部规则 + 链尾 drop 在**一次** nft -f 的批量文件里，按 Seq 顺序。
	content := f.singleBatch(t)
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	want := []string{
		"flush chain netdev nfvis-acl acl_web",
		// 两条规则未写地址（两侧 any）⇒ 各补一条 v4 限定（R2-4：不补会同时命中 v6）。
		"add rule netdev nfvis-acl acl_web ip saddr 0.0.0.0/0 meta l4proto tcp tcp dport 80 accept",
		"add rule netdev nfvis-acl acl_web ip saddr 0.0.0.0/0 meta l4proto tcp tcp dport 443 accept",
		// 链尾无条件 drop：规则本身没有匹配条件（"全匹配"的兜底），不需要（也不应）加地址限定。
		"add rule netdev nfvis-acl acl_web drop",
	}
	if len(lines) != len(want) {
		t.Fatalf("批量应恰好 %d 行（flush + 规则 + 兜底 drop），实得 %d：\n%s", len(want), len(lines), content)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("批量第 %d 行不符：\n期望 %q\n实得 %q\n完整内容：\n%s", i+1, want[i], lines[i], content)
		}
	}
	if !strings.HasSuffix(content, "\n") {
		t.Fatalf("批量文件应以换行结尾（每行一条命令）：\n%q", content)
	}
}

// TestACLApplyIsSingleAtomicBatch 断言 Apply 的「清空 + 重建 + 兜底 drop」只经**一次**
// `nft -f` 批量下发，不再有独立的 `nft flush chain` / `nft add rule` 调用（那正是
// 「flush 与首条规则之间链为空」的 fail-open 窗口来源，决策 #434）。
//
// 红-绿：把 Apply 改回逐条路径 ⇒「应恰好一次 nft -f，实得 0」判红。
func TestACLApplyIsSingleAtomicBatch(t *testing.T) {
	f := &batchCaptureRunner{}
	m := newACLManager(f)
	acl := model.Acl{Name: "web", Rules: []model.AclRule{
		{Seq: 10, Protocol: "tcp", DestinationPort: "80", Action: "permit"},
		{Seq: 20, Protocol: "icmp", Action: "deny"},
	}}
	if err := m.Apply(context.Background(), acl); err != nil {
		t.Fatal(err)
	}
	// 恰好一次 nft -f。
	var fcount int
	for _, c := range f.calls {
		if strings.HasPrefix(c, "nft -f ") {
			fcount++
		}
	}
	if fcount != 1 {
		t.Fatalf("应恰好一次 nft -f，实得 %d；调用：\n%s", fcount, f.joined())
	}
	// 不得再有独立的 flush / add rule 调用。
	for _, c := range f.calls {
		if strings.HasPrefix(c, "nft flush chain ") {
			t.Fatalf("不应再有独立的 flush 调用（fail-open 窗口来源）；调用：\n%s", f.joined())
		}
		if strings.HasPrefix(c, "nft add rule ") {
			t.Fatalf("不应再有独立的 add rule 调用（fail-open 窗口来源）；调用：\n%s", f.joined())
		}
	}
	// 批量内容与逐条路径逐字等价。
	content := f.singleBatch(t)
	if !strings.HasPrefix(content, "flush chain netdev nfvis-acl acl_web\n") {
		t.Fatalf("批量应以 flush 开头：\n%s", content)
	}
	if !strings.HasSuffix(content, "add rule netdev nfvis-acl acl_web drop\n") {
		t.Fatalf("批量应以链尾无条件 drop 结尾：\n%s", content)
	}
}

// TestACLApplyRemovesBatchFile 断言批量临时文件在 Apply 返回后已被删除（不留残渣）。
func TestACLApplyRemovesBatchFile(t *testing.T) {
	f := &batchCaptureRunner{}
	m := newACLManager(f)
	if err := m.Apply(context.Background(), model.Acl{Name: "web", Rules: []model.AclRule{
		{Seq: 10, Action: "deny"},
	}}); err != nil {
		t.Fatal(err)
	}
	if len(f.paths) != 1 {
		t.Fatalf("应记录一个批量文件路径，实得 %d", len(f.paths))
	}
	if _, err := os.Stat(f.paths[0]); !os.IsNotExist(err) {
		t.Fatalf("批量临时文件 %s 应在 Apply 后删除，Stat 得到 err=%v", f.paths[0], err)
	}
	if !strings.Contains(f.paths[0], "nfvis-acl-") {
		t.Fatalf("批量临时文件名应带 nfvis-acl- 前缀便于排查，实得 %q", f.paths[0])
	}
}

// TestACLApplyBatchFailureDoesNotFallBack 断言 `nft -f` 失败时如实返回错误，**不回退**逐条路径
// （宁可拒绝，也不静默引入窗口）。
func TestACLApplyBatchFailureDoesNotFallBack(t *testing.T) {
	f := &batchCaptureRunner{fakeRunner: fakeRunner{replies: []fakeReply{{
		prefix: "nft -f ",
		out:    "Error: Could not process rule",
		err:    errors.New("exit status 1"),
	}}}}
	err := newACLManager(f).Apply(context.Background(), model.Acl{Name: "web", Rules: []model.AclRule{
		{Seq: 10, Action: "deny"},
	}})
	if err == nil {
		t.Fatalf("nft -f 失败应如实返回错误")
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "nft flush chain ") || strings.HasPrefix(c, "nft add rule ") {
			t.Fatalf("失败后不得回退逐条路径；调用：\n%s", f.joined())
		}
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
