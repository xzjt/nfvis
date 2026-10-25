package schema

// BUG-21（v3.0.29 外部测试报告，`docs/REPORT-v3.0.29.md` §7）：命令树与解析器契约不一致——
// `secondary` / `prefer` 被建成 `server` 下的**独立关键字兄弟**（没有 `Opt` 标记），
// 于是树枚举出 `set system dns server secondary <ip>` 这个独立叶子，而解析器只认规范序
// `set system dns server <ip> secondary <ip2>`；按树生成的脚本在真机报
// 「%% 语句不完整：system dns server 之后需要先给 <ip> 取值，再跟 secondary」。
//
// 契约口径（判据全部取自树本身，不手抄清单）：
//  1. **可选修饰必须带 `Opt`**：命令全表写 `<ip> [secondary <ip>]`、`<ip|host> [prefer]`，
//     方括号就是 `Opt`（与 `system syslog host` 的 `P(<ip>)` + `Opt(K(port|facility|severity))`
//     同一先例）。不带 `Opt` 的修饰会被**当成独立叶子**，按树生成必然缺主参数。
//  2. **修饰必须排在必需（主）参数之后**：`server` 下先主参数、再修饰。
//  3. **按树拼出的完整语句形态里，修饰只能作主参数之后的尾巴**（取/不取都算），
//     不得出现「修饰紧跟父关键字」的形态——那正是 BUG-21。
//  4. **规范序仍可解析**：`{"system","dns","server","8.8.8.8","secondary","1.1.1.1"}` 等四条。
//
// 本文件**只钉树**：解析器与别名按决策不动（`internal/api` 不在本次改动范围）。因此
// `secondary-first` 的报错文案保持不变——那条由 api 侧既有守护
// `TestScalarRequiredBeforeSiblingKeyword`（cliexec_login_test.go）继续钉住。

import (
	"strings"
	"testing"
)

// bug21Compose 按「命令生成器」口径枚举一个节点子树下一条完整语句的全部形态
// （token 以空格连接）：
//   - 必需孩子按声明序出现（各孩子多形态时取笛卡尔积）；
//   - 可选孩子（`Opt`，文档里的 `[...]`）**只作为必需内容之后的尾巴**取/不取，
//     绝不单独成句。
//
// 这个口径与报告 §2.3「补全宿主参数」的做法一致：`ping <host> [count <n>]` 的
// `count <n>` 只有在补上 `<host>` 之后才是一条能执行的命令。
func bug21Compose(n *Node) []string {
	if len(n.Children) == 0 {
		return []string{""}
	}
	seqs := [][]string{{""}}
	appendPart := func(seqs [][]string, c *Node) [][]string {
		tails := bug21Compose(c)
		out := make([][]string, 0, len(seqs)*len(tails))
		for _, s := range seqs {
			for _, tail := range tails {
				part := c.Name
				if tail != "" {
					part += " " + tail
				}
				out = append(out, append(append([]string{}, s...), part))
			}
		}
		return out
	}
	join := func(parts []string) string {
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p != "" {
				out = append(out, p)
			}
		}
		return strings.Join(out, " ")
	}
	for _, c := range n.Children {
		if c.Optional {
			continue // 可选修饰留到必需孩子之后处理（顺序即契约）
		}
		seqs = appendPart(seqs, c)
	}
	for _, c := range n.Children {
		if !c.Optional {
			continue
		}
		withMod := appendPart(seqs, c)
		seqs = append(append([][]string{}, seqs...), withMod...)
	}
	out := make([]string, 0, len(seqs))
	for _, s := range seqs {
		out = append(out, join(s))
	}
	return out
}

// bug21Child 取同名关键字直接子节点（无则 nil）。
func bug21Child(n *Node, name string) *Node {
	for _, c := range n.Children {
		if c.Kind == Keyword && c.Name == name {
			return c
		}
	}
	return nil
}

// bug21Index 子节点在声明序中的位置（-1 = 不存在）。
func bug21Index(n *Node, name string) int {
	for i, c := range n.Children {
		if c.Kind == Keyword && c.Name == name {
			return i
		}
	}
	return -1
}

// bug21MainParamIndex 首个**必需**参数节点（主参数）的位置（-1 = 无）。
// 同时覆盖 P（ntp 的 `<ip|host>`，实例参数风格）与 SP/SPA（dns 的 `<ip>`，标量参数）。
func bug21MainParamIndex(n *Node) int {
	for i, c := range n.Children {
		if c.Kind == Param && !c.Optional {
			return i
		}
	}
	return -1
}

// bug21ModifierProblems 返回「修饰 vs 主参数」契约的全部违规项（空 = 合规）。
// 单独抽出成纯函数，便于 `TestDNSTreeGuardHasDiscriminatingPower` 用**改前的树形态**
// 反证判据真的会红（而不是恒绿的摆设）。
func bug21ModifierProblems(srv *Node, prefix, modifier, mainPlaceholder string) []string {
	var bad []string
	mod := bug21Child(srv, modifier)
	if mod == nil {
		return []string{prefix + "：修饰关键字 " + modifier + " 不存在（它是 `[...]` 可选修饰）"}
	}
	if !mod.Optional {
		bad = append(bad, prefix+"：`"+modifier+"` 未标 Opt——不带 Opt 时它被当成**独立叶子**，"+
			"按树生成的命令必然缺主参数（BUG-21 形态）")
	}
	mi, pi := bug21Index(srv, modifier), bug21MainParamIndex(srv)
	if pi < 0 {
		bad = append(bad, prefix+"：缺少必需的主参数节点（应先给主参数、再跟修饰）")
	} else if mi < pi {
		bad = append(bad, prefix+"：`"+modifier+"` 声明在主参数之前（须先给主参数、再跟修饰）")
	}

	shapes := bug21Compose(srv)
	plain, withMod := mainPlaceholder, mainPlaceholder+" "+modifier
	sawPlain, sawWithMod := false, false
	for _, sh := range shapes {
		if sh == modifier || strings.HasPrefix(sh, modifier+" ") {
			bad = append(bad, prefix+"：枚举出独立形态 `"+prefix+" "+sh+"`——修饰紧跟父关键字、缺主参数；"+
				"修饰必须带 Opt，只作主参数之后的尾巴")
		}
		if sh == plain {
			sawPlain = true
		}
		if strings.HasPrefix(sh, withMod) {
			sawWithMod = true
		}
	}
	if !sawPlain {
		bad = append(bad, prefix+"：未枚举出主参数形态 `"+prefix+" "+plain+"`（实得 "+strings.Join(shapes, " / ")+"）")
	}
	if !sawWithMod {
		bad = append(bad, prefix+"：未枚举出带修饰形态 `"+prefix+" "+withMod+" <…>`（实得 "+strings.Join(shapes, " / ")+"）")
	}
	return bad
}

func bug21Check(t *testing.T, label string, srv *Node, prefix, modifier, mainPlaceholder string) {
	t.Helper()
	for _, p := range bug21ModifierProblems(srv, prefix, modifier, mainPlaceholder) {
		t.Errorf("[%s] %s", label, p)
	}
}

// bug21ServerNode 取 `<path>` 指向的 `server` 节点。
func bug21ServerNode(t *testing.T, path []string) *Node {
	t.Helper()
	n, _, err := Match(ConfigPathTree(), path)
	if err != nil {
		t.Fatalf("%v 应在树里: %v", path, err)
	}
	if n.Name != "server" {
		t.Fatalf("路径 %v 应停在 server 节点上，实得 %q", path, n.Name)
	}
	return n
}

// TestDNSTreeSecondaryIsOptionalModifier：`system dns server <ip> [secondary <ip>]`。
// 改前红：`secondary` 无 Opt ⇒ 枚举只有 `<ip> secondary <ip>` 一种，主参数被吞进修饰里。
func TestDNSTreeSecondaryIsOptionalModifier(t *testing.T) {
	srv := bug21ServerNode(t, []string{"system", "dns", "server"})
	bug21Check(t, "system dns server", srv, "system dns server", "secondary", "<ip>")
}

// TestNTPTreePreferIsOptionalModifier：`system ntp server <ip|host> [prefer]`。
// 改前红：`prefer` 无 Opt ⇒ 枚举出 `system ntp server prefer`，而执行器会把 `prefer`
// 当成一台**名字叫 prefer 的服务器**接受（静默写错配置，不是报错）。
func TestNTPTreePreferIsOptionalModifier(t *testing.T) {
	srv := bug21ServerNode(t, []string{"system", "ntp", "server"})
	bug21Check(t, "system ntp server", srv, "system ntp server", "prefer", "<ip|host>")
}

// TestDNSProxyTreeSecondaryIsOptionalModifier：同族两处（全局上游 + 交换机级按域上游）。
// 它们靠别名侥幸可用（别名表里 `server secondary *` 与 `server *` 都在），但树仍广告
// 独立叶子；契约一致性要求「别名能用的形态」与「树广告的形态」是同一个。
func TestDNSProxyTreeSecondaryIsOptionalModifier(t *testing.T) {
	cases := []struct {
		label  string
		path   []string
		prefix string
	}{
		{"system dns proxy server", []string{"system", "dns", "proxy", "server"}, "system dns proxy server"},
		{"virtual-switches dns proxy server",
			[]string{"virtual-switches", "vs-x", "dns", "proxy", "server"},
			"virtual-switches vs-x dns proxy server"},
	}
	for _, c := range cases {
		bug21Check(t, c.label, bug21ServerNode(t, c.path), c.prefix, "secondary", "<ip>")
	}
}

// TestDNSTreeCanonicalOrderStillParses：规范序必须保持可解析（本次只改树的修饰标记，
// 不动解析器语义）。四条覆盖 dns / ntp / 两处 dns proxy。
func TestDNSTreeCanonicalOrderStillParses(t *testing.T) {
	cases := []struct {
		tokens   []string
		modifier string
	}{
		{[]string{"system", "dns", "server", "8.8.8.8", "secondary", "1.1.1.1"}, "secondary"},
		{[]string{"system", "ntp", "server", "10.0.0.1", "prefer"}, "prefer"},
		{[]string{"system", "dns", "proxy", "server", "8.8.8.8", "secondary", "8.8.4.4"}, "secondary"},
		{[]string{"virtual-switches", "vs-x", "dns", "proxy", "server", "10.0.0.53", "secondary", "10.0.0.54"}, "secondary"},
	}
	root := ConfigPathTree()
	for _, c := range cases {
		n, depth, err := Match(root, c.tokens)
		if err != nil {
			t.Errorf("规范序 %v 应可解析: %v", c.tokens, err)
			continue
		}
		if depth != len(c.tokens) {
			t.Errorf("规范序 %v 应消耗全部 %d 个 token，实得 %d（停在 %q）", c.tokens, len(c.tokens), depth, n.Name)
		}
		// 纯关键字路径必须真的经过修饰关键字（否则「解析成功」可能只是把 secondary 当成了
		// 主参数取值——那正是 BUG-21 要排除的形态）。
		_, kp, err := MatchKeywordPath(root, c.tokens)
		if err != nil {
			t.Errorf("规范序 %v 的关键字路径应可枚举: %v", c.tokens, err)
			continue
		}
		if !contains(kp, c.modifier) {
			t.Errorf("规范序 %v 的关键字路径 %v 未经过修饰关键字 %q", c.tokens, kp, c.modifier)
		}
	}
}

// TestDNSTreeModifierOfferedAfterMainParam（候选侧交叉核对）：主参数给过之后，`?`/Tab
// 才把修饰列出来——与规范序一致。
//
// 注：主参数位置仍会同时列出 `secondary`（`Candidates` 不看 `Optional`，这是全树一贯行为，
// 改动它超出本次范围）——本条只钉「给过主参数后修饰可见」这个正向契约。
func TestDNSTreeModifierOfferedAfterMainParam(t *testing.T) {
	for _, c := range []struct {
		label    string
		tokens   []string
		modifier string
	}{
		{"dns", []string{"system", "dns", "server", "8.8.8.8"}, "secondary"},
		{"ntp", []string{"system", "ntp", "server", "1.2.3.4"}, "prefer"},
		{"dns proxy", []string{"system", "dns", "proxy", "server", "8.8.8.8"}, "secondary"},
	} {
		names := candidateNames(Candidates(ConfigPathTree(), c.tokens, "", testDyn))
		if !contains(names, c.modifier) {
			t.Errorf("%s：给过主参数后应给出修饰候选 %q，实得 %v", c.label, c.modifier, names)
		}
	}
}

// TestDNSTreeGuardHasDiscriminatingPower：判据自校准——把**改前的树形态**（修饰是普通
// 兄弟、无 Opt）喂给同一组断言，必须全部报红。否则上面的用例可能只是恒绿摆设。
func TestDNSTreeGuardHasDiscriminatingPower(t *testing.T) {
	// 改前形态：secondary 与主参数平级、未标 Opt
	old := K("server", "DNS 服务器",
		SPA("<ip>", "dns_servers", "服务器地址"),
		K("secondary", "备用服务器", SPA("<ip>", "dns_servers", "地址")),
	)
	problems := bug21ModifierProblems(old, "system dns server", "secondary", "<ip>")
	if len(problems) == 0 {
		t.Fatal("改前形态（secondary 无 Opt）应被判红——判据没有区分力")
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"未标 Opt", "未枚举出主参数形态"} {
		if !strings.Contains(joined, want) {
			t.Errorf("改前形态应报 %q，实得:\n%s", want, joined)
		}
	}
	// 再验「修饰排在主参数之前」与「修饰缺失」两条判据各有区分力
	before := K("server", "DNS 服务器",
		Opt(K("secondary", "备用服务器", SPA("<ip>", "dns_servers", "地址"))),
		SPA("<ip>", "dns_servers", "服务器地址"),
	)
	if got := strings.Join(bug21ModifierProblems(before, "x", "secondary", "<ip>"), "\n"); !strings.Contains(got, "声明在主参数之前") {
		t.Errorf("修饰排在主参数之前应报红，实得: %s", got)
	}
	if got := strings.Join(bug21ModifierProblems(K("server", "无修饰"), "x", "secondary", "<ip>"), "\n"); !strings.Contains(got, "不存在") {
		t.Errorf("修饰缺失应报红，实得: %s", got)
	}
}
