package cliparse

import (
	"reflect"
	"testing"
)

// TestSplitFieldsQuotes 引号语义：引号内空白/换行不切、引号剥除、`\"`/`\\` 转义。
func TestSplitFieldsQuotes(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"set system hostname fw", []string{"set", "system", "hostname", "fw"}},
		{`set x y "a b c"`, []string{"set", "x", "y", "a b c"}},
		// SSH 公钥必含空格（决策 #79）：引号包住即一个 token。
		{`set cloud-init ssh-key "ssh-ed25519 AAAA you@host"`,
			[]string{"set", "cloud-init", "ssh-key", "ssh-ed25519 AAAA you@host"}},
		// 引号内的 `|` 与 `#` 是普通字符（不是管道/注释）。
		{`set x y "a|b"`, []string{"set", "x", "y", "a|b"}},
		{`set x y "#!/bin/sh"`, []string{"set", "x", "y", "#!/bin/sh"}},
		// 转义：`\"` 是字面量引号、`\\` 是字面量反斜杠。
		{`set x y "a\"b"`, []string{"set", "x", "y", `a"b`}},
		{`set x y "a\\b"`, []string{"set", "x", "y", `a\b`}},
		// 引号跨行：值内保留换行（整段取值）。
		{"set x y \"a\nb\"", []string{"set", "x", "y", "a\nb"}},
		// 未闭合引号：宽容地按到末尾为一 token（上层据此可报错，不静默丢字）。
		{`set x y "a b`, []string{"set", "x", "y", "a b"}},
		// 空串与纯空白。
		{"", nil},
		{"   ", nil},
	}
	for _, c := range cases {
		if got := SplitFields(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("SplitFields(%q) = %#v，期望 %#v", c.in, got, c.want)
		}
	}
}

// TestSplitFieldsOffsets 位置追踪与分词**同源**（决策 #377/E8）：tokens 与 SplitFields 逐字一致，
// starts[i] 指向第 i 个 token 的**内容起点**（引用 token = 开引号之后；未引用/转义 = 首字符）。
func TestSplitFieldsOffsets(t *testing.T) {
	cases := []struct {
		in     string
		starts []int
	}{
		{"set x y fw", []int{0, 4, 6, 8}},
		{`set x y "a b"`, []int{0, 4, 6, 9}}, // 开引号后一位
		{`set x y "a\"b`, []int{0, 4, 6, 9}}, // 引用 token 内含转义，内容仍自开引号后
		{`set x y a\"b`, []int{0, 4, 6, 8}},  // 未引用 token 含转义 ⇒ 自首字符
	}
	for _, c := range cases {
		toks, starts := SplitFieldsOffsets(c.in)
		if !reflect.DeepEqual(toks, SplitFields(c.in)) {
			t.Fatalf("SplitFieldsOffsets(%q) tokens=%#v 与 SplitFields 不一致", c.in, toks)
		}
		if !reflect.DeepEqual(starts, c.starts) {
			t.Fatalf("SplitFieldsOffsets(%q) starts=%#v，期望 %#v", c.in, starts, c.starts)
		}
	}
}

// TestSplitUnquotedPipe 管道切分只在**未引用**位置发生（决策 #155 补充三 / #313）。
func TestSplitUnquotedPipe(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"show version | match VPP", []string{"show version ", " match VPP"}},
		// 引号内的 `|` 不算分隔，且引号原样保留给下游分词器。
		{`set x y "a|b" | count`, []string{`set x y "a|b" `, " count"}},
		// 转义引号不闭合引号域：`"a\"|b"` 整段属于一个引号域。
		{`set x y "a\"|b" | count`, []string{`set x y "a\"|b" `, " count"}},
		// 多个未引用管道。
		{"show x | match a | last 3", []string{"show x ", " match a ", " last 3"}},
		// 没有管道：整段返回。
		{"show version", []string{"show version"}},
	}
	for _, c := range cases {
		if got := SplitUnquoted(c.in, '|'); !reflect.DeepEqual(got, c.want) {
			t.Errorf("SplitUnquoted(%q) = %#v，期望 %#v", c.in, got, c.want)
		}
	}
}

// TestOpenQuote 引号闭合判定（含转义）。
func TestOpenQuote(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"show version", false},
		{`set x y "a b"`, false},
		{`set x y "a b`, true}, // 未闭合
		{`set x y "a`, true},   // 引号跨行时的首行
		{"set x y \"a\nb\"", false},
		{`set x y "a\"`, true},   // `\"` 是字面量引号，引号仍未闭合
		{`set x y "a\\"`, false}, // `\\` 是字面量反斜杠，随后的 `"` 闭合
	}
	for _, c := range cases {
		if got := OpenQuote(c.in); got != c.want {
			t.Errorf("OpenQuote(%q) = %v，期望 %v", c.in, got, c.want)
		}
	}
}

// TestSplitStatements 语句切分：跨行引号值取整段（值内保留换行），其余按行。
func TestSplitStatements(t *testing.T) {
	// 关键用例：一条 set 语句的值用引号跨两行。
	script := "configure\n" +
		"set virtual-machine-functions fw-vm cloud-init user-data \"#!/bin/sh\n" +
		"echo hi\"\n" +
		"commit"
	got := SplitStatements(script)
	want := []string{
		"configure",
		"set virtual-machine-functions fw-vm cloud-init user-data \"#!/bin/sh\necho hi\"",
		"commit",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SplitStatements 跨行引号值失败：\n got=%#v\nwant=%#v", got, want)
	}
	// 值内换行必须真的保留（不是被拼成空格）。
	if !contains(got[1], "#!/bin/sh\necho hi") {
		t.Fatalf("值内换行应保留，实得 %q", got[1])
	}

	// 空行保留为独立空语句（调用方跳过）；多语句各归各行。
	got = SplitStatements("show version\n\nshow vpp status")
	want = []string{"show version", "", "show vpp status"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("空行/多语句切分失败：%#v", got)
	}

	// 末尾未闭合引号：整段收作一条（上层报错），不丢行。
	got = SplitStatements("set x y \"a\nb")
	if len(got) != 1 || !contains(got[0], "b") {
		t.Fatalf("末尾未闭合引号应整段收作一条，实得 %#v", got)
	}
}

// TestSplitFieldsMatchesUnquotedPipeline 两条分词路径对「引号内 | 」结论一致：
// 先按未引用位置切管道、再对命令段分词，引号内的 `|` 落在同一 token 里。
func TestSplitFieldsMatchesUnquotedPipeline(t *testing.T) {
	line := `set virtual-switches vs description "a|b" | count`
	segs := SplitUnquoted(line, '|')
	cmd := segs[0]
	toks := SplitFields(cmd)
	found := false
	for _, tk := range toks {
		if tk == "a|b" {
			found = true
		}
	}
	if !found {
		t.Fatalf("引号内的 `|` 应留在同一 token，实得 %#v", toks)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
