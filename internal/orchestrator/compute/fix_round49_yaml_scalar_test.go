package compute

// round49（外部评审两条缺陷之二）：cloud-init 的 hostname 未转义 —— 合法提交会生成
// **无法解析**的 seed（中危）。
//
// 缺陷形态（已按源码逐行核实）：`buildCloudConfig` 直接拼 `hostname: <值>` 与 `fqdn: <值>`，
// `BuildMetaData` 直接拼 `local-hostname: <值>`，中间没有任何 YAML 标量转义，而 model 侧
// 当时也没有对 cloud-init 的 hostname 做格式校验（RFC1123 校验由另一组在 model/validate.go 补）。
// 于是 `set vnf … cloud-init hostname "branch: blue"` 生成的是
//
//	hostname: branch: blue          ← YAML 里「mapping values are not allowed here」
//
// cloud-init 解析 seed 直接失败：hostname 落不下去，整个 #cloud-config 段一起被丢弃
//（SSH 公钥、users 都没了），而产品侧提交成功、毫无提示——典型的「命令成功、事实没落地」。
//
// 本组用例的两条判据：
//  1. 危险值必须走**双引号标量**且内部 `"`/`\`/控制字符正确转义——且能原样解回来（round-trip）、
//     不产生额外行（换行注入）；
//  2. 合法值（RFC1123 主机名那种）输出**逐字不变**（既有测试与真机夹具不受影响）。

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/xzjt/nfvis/internal/model"
)

// dangerousHostnames 不是合法 YAML **裸标量**的值（直接破坏语法或让值变形）：
// 含冒号+空格、引号、反斜杠、制表/换行/回车、裸控制字符、非 ASCII/不可打印、前后空白、以标记字符开头。
var dangerousHostnames = []string{
	"branch: blue",    // 冒号+空格：YAML「mapping values are not allowed here」
	`a"b`,             // 双引号：裸写会截断标量
	`back\slash`,      // 反斜杠：裸写会被 YAML 当作转义起始
	"tab\there",       // 制表符：不能出现在裸标量里
	"line\nbreak",     // 换行：会把后面的内容变成新的一行（注入）
	"cr\rhere",        // 回车
	"nul\x00byte",     // 控制字符
	"esc\x1b[31m",     // 转义字符
	"中文主机名",           // 非 ASCII
	"主机\u00a0名",       // 不可打印字符（不换行空格 U+00A0）
	" leading-space",  // 前导空白：裸标量不允许
	"trailing-space ", // 尾随空白：同上
	"-",               // YAML 指示符打头
	"#comment",        // 注释符打头
	"@at",
	"host:port",
	"", // 空值（调用方一般走 VM 名分支；这里直接守 yamlScalar 本身）
}

// yamlQuotedValue 从配置文本里取出 `key: <值>` 行的值，要求它是 YAML **双引号标量**，
// 用极简解码器反转义后返回。
//
// 为什么这样断言：值没被双引号包裹时直接判失败——那正是本缺陷的形态（`hostname: branch: blue`）；
// 被引号包裹但转义有错时解码器会报错（例如内部还有裸 `"` 就说明标量会被提前截断）。
func yamlQuotedValue(t *testing.T, cfg, key string) string {
	t.Helper()
	for _, ln := range strings.Split(cfg, "\n") {
		rest, ok := strings.CutPrefix(ln, key+": ")
		if !ok {
			continue
		}
		if len(rest) < 2 || !strings.HasPrefix(rest, `"`) || !strings.HasSuffix(rest, `"`) {
			t.Fatalf("%s 的值必须以 YAML 双引号标量写出（否则危险值会破坏整份配置）：%q", key, ln)
		}
		got, err := decodeYAMLDoubleQuoted(rest[1 : len(rest)-1])
		if err != nil {
			t.Fatalf("%s 的转义无法解码：%v（原文 %q）", key, err, ln)
		}
		return got
	}
	t.Fatalf("%s 行不存在：\n%s", key, cfg)
	return ""
}

// decodeYAMLDoubleQuoted 极简 YAML 双引号标量解码器（只处理本产品会写出的转义）：
// `\\ \" \/ \n \r \t \0 \xNN \uNNNN \UNNNNNNNN`。遇到裸引号、裸控制字符或认不出的转义即报错
// ——「引号里塞了会破坏 YAML 的裸字符」必须能被这条断言抓住。
func decodeYAMLDoubleQuoted(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			return "", fmt.Errorf("值内部出现了未转义的引号（YAML 里会截断标量）")
		}
		if c < 0x20 {
			return "", fmt.Errorf("值内部出现了裸控制字符 0x%02x（双引号标量里必须转义）", c)
		}
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(s) {
			return "", fmt.Errorf("结尾处是孤立的反斜杠")
		}
		switch s[i] {
		case '\\', '"', '/':
			b.WriteByte(s[i])
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case '0':
			b.WriteByte(0)
		case 'x', 'u', 'U':
			n := map[byte]int{'x': 2, 'u': 4, 'U': 8}[s[i]]
			if i+n >= len(s) {
				return "", fmt.Errorf("\\%c 转义不完整", s[i])
			}
			v, err := strconv.ParseUint(s[i+1:i+1+n], 16, 32)
			if err != nil {
				return "", fmt.Errorf("\\%c 转义非十六进制：%v", s[i], err)
			}
			b.WriteRune(rune(v))
			i += n
		default:
			return "", fmt.Errorf("认不出的转义 \\%c", s[i])
		}
	}
	return b.String(), nil
}

// TestYAMLScalarEscapesDangerousValues：危险值一律双引号化、可原样解回、且不产生额外行。
func TestYAMLScalarEscapesDangerousValues(t *testing.T) {
	for _, v := range dangerousHostnames {
		got := yamlScalar(v)
		if v != "" && got == v {
			t.Fatalf("危险值 %q 未被转义（原样输出会破坏 YAML）", v)
		}
		if len(got) < 2 || !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
			t.Fatalf("危险值 %q 应输出为双引号标量，实际 %q", v, got)
		}
		back, err := decodeYAMLDoubleQuoted(got[1 : len(got)-1])
		if err != nil {
			t.Fatalf("危险值 %q 的转义无法解码：%v（输出 %q）", v, err, got)
		}
		if back != v {
			t.Fatalf("危险值 %q 应能原样解回，实际解出 %q（输出 %q）", v, back, got)
		}
		if strings.ContainsAny(got, "\n\r") {
			t.Fatalf("渲染结果不得含真实换行/回车（会注入新行）：%q", got)
		}
	}
}

// TestYAMLScalarQuotesYAMLNonStringLiterals：语法上没问题、但裸写会被 YAML **解析成非字符串**
// 的值也必须加引号（纯数字 → 整数；true/yes/on/null/~ → 布尔与空值）。cloud-init 的
// `hostname`/`local-hostname` 是字符串字段，拿到整数/布尔即校验失败——同一份文件里的
// netplanScalar 早就为此加引号（同一缺陷族，判据同源）。
func TestYAMLScalarQuotesYAMLNonStringLiterals(t *testing.T) {
	for _, v := range []string{"9", "123", "0", "true", "True", "false", "yes", "Yes", "no", "on", "off", "null", "NULL", "~"} {
		got := yamlScalar(v)
		if len(got) < 2 || !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
			t.Fatalf("%q 裸写会被 YAML 解析成非字符串，必须加引号，实际输出 %q", v, got)
		}
		if back, err := decodeYAMLDoubleQuoted(got[1 : len(got)-1]); err != nil || back != v {
			t.Fatalf("%q 加引号后应原样解回：得 %q / %v", v, back, err)
		}
	}
}

// TestYAMLScalarKeepsSafeValuesVerbatim：合法（RFC1123 主机名形状）值逐字输出、不加多余引号
// ——既有测试与真机夹具（fw / fw-vm / probe-vm / web.example.com）的输出形态不许变。
func TestYAMLScalarKeepsSafeValuesVerbatim(t *testing.T) {
	for _, v := range []string{"fw", "fw-1", "web.example.com", "a", "A1-b2.c3", "3.0.30", "vnf-a", "cli-vm"} {
		if got := yamlScalar(v); got != v {
			t.Fatalf("合法值 %q 应逐字输出，实际 %q", v, got)
		}
	}
}

// TestBuildUserDataEscapesHostname：走真实入口（BuildUserData → buildCloudConfig）时，
// 危险 hostname 生成的配置必须可解析、值可原样取回，且行数与安全值一致（无换行注入）。
func TestBuildUserDataEscapesHostname(t *testing.T) {
	safeLines := countLines(BuildUserData(model.VMFunction{Name: "fw-vm", CloudInit: &model.CloudInit{
		Hostname: "safe-host", SSHKeys: []string{"ssh-ed25519 AAA test@host"},
	}}))
	for _, h := range dangerousHostnames {
		if h == "" {
			continue // 空 hostname 走 VM 名分支（另有用例覆盖）
		}
		cfg := BuildUserData(model.VMFunction{Name: "fw-vm", CloudInit: &model.CloudInit{
			Hostname: h, SSHKeys: []string{"ssh-ed25519 AAA test@host"},
		}})
		if got := yamlQuotedValue(t, cfg, "hostname"); got != h {
			t.Fatalf("hostname 应原样取回：输入 %q 取回 %q\n%s", h, got, cfg)
		}
		if got := yamlQuotedValue(t, cfg, "fqdn"); got != h {
			t.Fatalf("fqdn 应原样取回：输入 %q 取回 %q\n%s", h, got, cfg)
		}
		if n := countLines(cfg); n != safeLines {
			t.Fatalf("宿主名 %q 注入了额外行（%d 行 vs 安全值 %d 行）：\n%s", h, n, safeLines, cfg)
		}
	}
}

// TestBuildMetaDataEscapesLocalHostname：meta-data 的 local-hostname 同样必须转义。
func TestBuildMetaDataEscapesLocalHostname(t *testing.T) {
	safeLines := countLines(BuildMetaData(model.VMFunction{
		Name: "fw-vm", CloudInit: &model.CloudInit{Hostname: "safe-host"},
	}))
	for _, h := range dangerousHostnames {
		if h == "" {
			continue
		}
		md := BuildMetaData(model.VMFunction{Name: "fw-vm", CloudInit: &model.CloudInit{Hostname: h}})
		if got := yamlQuotedValue(t, md, "local-hostname"); got != h {
			t.Fatalf("local-hostname 应原样取回：输入 %q 取回 %q\n%s", h, got, md)
		}
		if n := countLines(md); n != safeLines {
			t.Fatalf("宿主名 %q 在 meta-data 里注入了额外行（%d 行 vs 安全值 %d 行）：\n%s", h, n, safeLines, md)
		}
	}
}

// TestBuildUserDataSafeHostnameVerbatim：合法 hostname 的历史输出形态逐字不变
// （helpers_test.go 与本机真机夹具都按这个形状比对）。
func TestBuildUserDataSafeHostnameVerbatim(t *testing.T) {
	cfg := BuildUserData(model.VMFunction{Name: "fw-vm", CloudInit: &model.CloudInit{Hostname: "web.example.com"}})
	for _, want := range []string{"\nhostname: web.example.com\n", "\nfqdn: web.example.com\n"} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("合法 hostname 应逐字输出 %q：\n%s", want, cfg)
		}
	}
	// 缺省（无 cloud-init hostname）时回落 VM 名，形态同样逐字不变。
	cfg = BuildUserData(model.VMFunction{Name: "probe-vm", CloudInit: &model.CloudInit{SSHKeys: []string{"k"}}})
	if !strings.Contains(cfg, "\nhostname: probe-vm\n") {
		t.Fatalf("无 hostname 应回落 VM 名并逐字输出：\n%s", cfg)
	}
	md := BuildMetaData(model.VMFunction{Name: "fw-vm", CloudInit: &model.CloudInit{Hostname: "fw-1"}})
	if !strings.Contains(md, "\nlocal-hostname: fw-1\n") {
		t.Fatalf("meta-data 的合法 hostname 应逐字输出：\n%s", md)
	}
	if !strings.Contains(md, "instance-id: fw-vm-") {
		t.Fatalf("instance-id 形态不变：\n%s", md)
	}
}

// TestBuildMetaDataInstanceIDDiffersByValue：instance-id 由**宿主名等语义值**派生
// （转义只影响 YAML 文本，不许让它变成常量或对危险值报错——那会让既有 VM 重启后被
// cloud-init 当成新实例、重跑 once-per-instance 的 user-data）。
func TestBuildMetaDataInstanceIDDiffersByValue(t *testing.T) {
	id := func(md string) string {
		t.Helper()
		for _, ln := range strings.Split(md, "\n") {
			if r, ok := strings.CutPrefix(ln, "instance-id: "); ok {
				return r
			}
		}
		t.Fatalf("meta-data 里应含 instance-id：\n%s", md)
		return ""
	}
	safe := id(BuildMetaData(model.VMFunction{Name: "fw-vm", CloudInit: &model.CloudInit{Hostname: "fw"}}))
	danger := id(BuildMetaData(model.VMFunction{
		Name: "fw-vm", CloudInit: &model.CloudInit{Hostname: "branch: blue"},
	}))
	for _, got := range []string{safe, danger} {
		if !strings.HasPrefix(got, "fw-vm-") || len(got) <= len("fw-vm-") {
			t.Fatalf("instance-id 应为 %q 前缀 + 摘要：%q", "fw-vm-", got)
		}
	}
	if safe == danger {
		t.Fatalf("不同宿主名应派生出不同的 instance-id：%q", safe)
	}
}

// TestYAMLScalarEscapesInvalidUTF8：非法 UTF-8 字节（不可能是合法 hostname）也必须渲染成
// **可解析的** YAML——按字节 `\xNN` 转义，输出里不得残留裸的非法字节。
// 如实边界：YAML 的 `\xNN` 是**码点**转义，故「非法字节 → \xNN」不是字节级可逆的；
// 这里只要求「结构不破、解析得出来」，不要求字节级 round-trip（那在 YAML 里做不到）。
func TestYAMLScalarEscapesInvalidUTF8(t *testing.T) {
	raw := "bad\xff\xfeutf8"
	got := yamlScalar(raw)
	if len(got) < 2 || !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
		t.Fatalf("非法 UTF-8 值也应输出为双引号标量，实际 %q", got)
	}
	body := got[1 : len(got)-1]
	if !strings.Contains(body, `\xff`) || !strings.Contains(body, `\xfe`) {
		t.Fatalf("非法字节应按 \\xNN 转义：%q", got)
	}
	if !utf8.ValidString(body) {
		t.Fatalf("转义后的标量本体应是合法 UTF-8（否则 seed 里是坏字节）：%q", got)
	}
	for i := 0; i < len(body); i++ {
		if body[i] < 0x20 {
			t.Fatalf("转义后的标量本体不得含裸控制字符（0x%02x）：%q", body[i], got)
		}
	}
}

// countLines 数配置文本的行数（末尾换行不算额外一行）。
func countLines(s string) int { return len(strings.Split(strings.TrimRight(s, "\n"), "\n")) }
