// 守护：**给操作者看的文本**里不得出现内部引用（FR-xxx / §x / 决策 #nn / 附录 A #nn）。
//
// 由来（2026-09-15，决策 #86）：CLI 帮助与报错里长期混着需求编号，例如
//
//	`commit   提交 candidate（FR-CFG-002/003）`
//	`%% …（FR-NET-012）`
//
// —— 操作者看到的是「FR-CFG-002/003」，既无信息量也读不通。需求可追溯（AGENTS 规则 2）
// 靠**代码注释与设计类 docs/**，不靠给操作者看的文本。
//
// 扩展（2026-09-16，决策 #87）：#86 只扫 `.go` 字符串字面量，于是**非 Go 文件里的泄漏全部漏网**，
// 而它们恰恰是安装/运维时直接打在终端上的——`dpkg -i` 会打印 `deploy/debian/postinst` 的
// `log` 输出（如 `已重启 nfvis.service 以加载新版本（FR-OPS-001）`），随包安装的
// `docs/NFViS-用户手册.md` 是操作者手里的说明书。故判据从「文件后缀」改为
// 「**这段文本会到达操作者吗**」，逐类文件决定扫哪部分：
//
//	.go                    字符串字面量（注释豁免）
//	.sh                    非注释行（`#` 注释豁免，引号感知）
//	.service               非注释行（行首 `#`/`;` 豁免；systemd 无内联注释语法）
//	Makefile / *.mk        非注释行（构建期 echo 同样打在操作者终端）
//	deploy/ 下的一切        非注释行——`postinst`/`prerm`/`postrm` 按 dpkg 约定**没有后缀**，
//	                       而 `dpkg -i` 时它们的输出直接打在操作者终端
//	docs/NFViS-用户手册.md  全文（整份都是操作者读物，随 deb 装到 /usr/share/doc/nfvis/）
//
// 设计/契约/验收类 docs（规格书、命令树设计、openapi description、验收检查表、命令全表、
// M5-验收记录）**有意不扫**：那些引用就是需求可追溯的落点，清掉会削弱验收证据链。
//
// 同时拦住「剥掉引用后留下的标点残渣」——这是 #86 清理时真实踩到的形态：
//
//	（FR-CFG-011⑪）→（⑪）   FR-NET-003/FR-NET-001 → /001
//	（§4：仅 super-user）→（：仅 super-user）
package archtest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 内部引用本身（Go 与脚本口径：`§x` 在源码里指契约/规格书，操作者解析不了，故一律禁）
var refPatterns = []*regexp.Regexp{
	regexp.MustCompile(`FR-[A-Z]+-[0-9]+`),
	regexp.MustCompile(`§[0-9]`),
	regexp.MustCompile(`决策 #[0-9]+`),
	regexp.MustCompile(`附录 A #[0-9]+`),
}

// docRefPatterns **操作者读物（用户手册）**的口径：只禁「操作者手里无从解析」的标识。
// 特意**不含 `§x`**——手册里的 `见 §4.3`/`按 §3.2` 是指向**这份手册自身**（或随包同装的
// 命令全表）的导航，正是 #86 把 `docs/` 引用保留下来的那一条理由；在手册里它有用、能点得动。
// 而 `FR-xxx`（需求编号）与 `决策 #nn`/`附录 A #nn`（决策编号）对操作者没有任何可查的索引，
// 是纯噪声，故一律禁。
var docRefPatterns = []*regexp.Regexp{
	regexp.MustCompile(`FR-[A-Z]+-[0-9]+`),
	regexp.MustCompile(`决策 #[0-9]+`),
	regexp.MustCompile(`附录 A #[0-9]+`),
}

// residueCommon 剥掉引用后留下的**标点残渣**（「清了引用、留下垃圾」）。所有文件类都判：
// `（FR-CMP-015；…——决策 #75）` 这类擦掉后留下的 `（；…）` 对谁都是垃圾。
var residueCommon = []*regexp.Regexp{
	regexp.MustCompile(`（[:；，、]`),
	regexp.MustCompile(`[：；，、]）`),
	regexp.MustCompile(`（\s*）`),
	regexp.MustCompile(`，，|：：|；；`),
}

// residueGoScript 仅 Go/脚本判：这两种形态在**源码与脚本输出**里必是残渣，
// 但在 Markdown 里都是**合法书写**，故对操作者读物禁用——
//   - 圈号 `①-⑳`：手册用它们作步骤编号（`**④ 快照**`），不是 `（FR-CFG-011⑪）` 的残留；
//   - `/#nn`：`/001` 是 `FR-NET-003/FR-NET-001` 的残留，而目录里 `[系统要求](#1-系统要求)` 是锚点。
var residueGoScript = []*regexp.Regexp{
	regexp.MustCompile(`[\x{2460}-\x{2473}]`), // ①②…⑳
	regexp.MustCompile(`/#[0-9]`),
}

// textScope 描述一个文件「扫哪部分」以及「判哪些形态」。
type textScope struct {
	// strip 返回行内**会被操作者看到**的部分；nil 表示整行（全文扫描）。
	strip func(string) string
	// onlyQuoted 仅 Go 模式为真：只看含字符串字面量的行。
	onlyQuoted bool
	// patterns 该文件类适用的引用 + 残渣形态。
	patterns []*regexp.Regexp
}

// operatorDocs **随包发布且是操作者读物**的文件：整份文本都算「给操作者看的话」，无注释可豁免。
// 注意判据是「操作者读物」，不是「在 docs/ 下」——设计/契约/验收类文档不在其中（见包注释）。
var operatorDocs = map[string]bool{
	"docs/NFViS-用户手册.md": true,
}

// textScope 描述一个文件「扫哪部分」。
// scopeFor 按「文本会不会到达操作者」决定扫描范围；第二个返回值为 false 表示该文件不扫。
// 判据顺序：显式读物 → 后缀 → 无后缀的安装/脚本材料。
func scopeFor(rel string, data []byte) (textScope, bool) {
	base := filepath.Base(rel)
	switch {
	case operatorDocs[rel]:
		// 操作者读物：全文扫，但只判「操作者无从解析」的编号（`§x` 是文档内导航，放行）。
		return textScope{patterns: docPatterns()}, true
	case strings.HasSuffix(base, ".go"):
		if strings.HasSuffix(base, "_test.go") {
			return textScope{}, false
		}
		return textScope{strip: codePart, onlyQuoted: true, patterns: goScriptPatterns()}, true
	case strings.HasSuffix(base, ".sh"):
		return textScope{strip: hashCommentPart, patterns: goScriptPatterns()}, true
	case strings.HasSuffix(base, ".service"):
		return textScope{strip: unitCommentPart, patterns: goScriptPatterns()}, true
	case base == "Makefile", base == "makefile", strings.HasSuffix(base, ".mk"):
		return textScope{strip: hashCommentPart, patterns: goScriptPatterns()}, true
	}
	// 无后缀的文件：**`deploy/` 是随包安装的材料**（`postinst`/`prerm`/`postrm` 三个
	// 维护者脚本按 dpkg 约定没有后缀，`dpkg -i` 时它们的输出直接打在操作者终端），
	// 以及任何以 shell shebang 开头的脚本——都按 shell 口径扫。
	if strings.HasPrefix(rel, "deploy/") || hasShellShebang(data) {
		return textScope{strip: hashCommentPart, patterns: goScriptPatterns()}, true
	}
	return textScope{}, false
}

// goScriptPatterns 源码/脚本口径：引用全禁 + 残渣全判。
func goScriptPatterns() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(refPatterns)+len(residueCommon)+len(residueGoScript))
	out = append(out, refPatterns...)
	out = append(out, residueCommon...)
	out = append(out, residueGoScript...)
	return out
}

// docPatterns 操作者读物口径：只禁内部编号 + 标点残渣（见 docRefPatterns / residueGoScript 的注释）。
func docPatterns() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(docRefPatterns)+len(residueCommon))
	out = append(out, docRefPatterns...)
	out = append(out, residueCommon...)
	return out
}

// hasShellShebang：首行是 `#!…sh…`（sh/bash/dash/ash）即认为是 shell 脚本。
func hasShellShebang(data []byte) bool {
	line := string(data)
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	return strings.HasPrefix(line, "#!") && strings.Contains(line, "sh")
}

// codePart 去掉行尾注释，返回代码部分（正确处理字符串/字符字面量里的 // 与转义）。
func codePart(line string) string {
	var inStr, inRune bool
	for i := 0; i < len(line); i++ {
		switch {
		case inStr:
			if line[i] == '\\' {
				i++
			} else if line[i] == '"' {
				inStr = false
			}
		case inRune:
			if line[i] == '\\' {
				i++
			} else if line[i] == '\'' {
				inRune = false
			}
		case line[i] == '"':
			inStr = true
		case line[i] == '\'':
			inRune = true
		case line[i] == '/' && i+1 < len(line) && line[i+1] == '/':
			return line[:i]
		}
	}
	return line
}

// hashCommentPart 返回 shell / Makefile 行里注释之前的部分。
// `#` 只有在**行首或前一个字符是空白**且**不在引号内**时才开始注释：
// `echo "a#b"` 里的 `#` 是字面量，`x=a#b` 里的 `#` 也是（shell 不做词内注释），
// 而 `x=1  # FR-xxx` 是注释——豁免它正是本规则要的（注释是需求编号的合法落点）。
func hashCommentPart(line string) string {
	var inSingle, inDouble bool
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case inDouble:
			if c == '\\' {
				i++ // 转义：跳过下一个字符（`\"` 不闭合引号）
			} else if c == '"' {
				inDouble = false
			}
		case c == '\\':
			i++ // 引号外的转义（`\#` 是字面量 `#`）
		case c == '\'':
			inSingle = true
		case c == '"':
			inDouble = true
		case c == '#':
			if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' {
				return line[:i]
			}
		}
	}
	return line
}

// unitCommentPart 返回 systemd 单元行里注释之前的部分。
// systemd **只认行首**（允许前导空白）的 `#`/`;` 为注释，**没有内联注释语法**——
// `Description=a # b` 里的 `#` 是字面量，故不能照搬 shell 的剥法。
func unitCommentPart(line string) string {
	t := strings.TrimLeft(line, " \t")
	if strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
		return ""
	}
	return line
}

// TestUserVisibleTextHasNoInternalRefs 操作者可见文本不得含内部引用或剥离残渣。
func TestUserVisibleTextHasNoInternalRefs(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "build", "prototype", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scope, ok := scopeFor(rel, data)
		if !ok {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			text := line
			if scope.strip != nil {
				text = scope.strip(line)
			}
			if scope.onlyQuoted && !strings.Contains(text, `"`) {
				continue
			}
			for _, re := range scope.patterns {
				if m := re.FindString(text); m != "" {
					offenders = append(offenders,
						rel+":"+itoa(i+1)+": ["+m+"] "+strings.TrimSpace(text))
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历源码: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("操作者可见文本含内部引用/残渣 %d 处（需求可追溯请写在注释与设计类 docs/ 里）：\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// TestGuardCoversNonGoOperatorText 守护**自身**的自检：范围表里的每类文件都必须真的被扫到，
// 否则「扩展了守护」只是注释里的一句话——文件改名/新增读物时最容易静默失守。
func TestGuardCoversNonGoOperatorText(t *testing.T) {
	cases := []struct {
		rel      string
		data     string
		wantScan bool
	}{
		// dpkg 维护者脚本按约定**没有后缀**，deploy/ 前缀是它们被扫到的唯一依据
		{"deploy/debian/postinst", "#!/bin/sh\n", true},
		{"deploy/debian/postinst.sh", "#!/bin/sh\n", true},
		{"deploy/nfvis.service", "[Unit]\n", true},
		{"contrib/scripts/cli-pty-smoke.sh", "#!/usr/bin/env bash\n", true},
		{"Makefile", "deb:\n", true},
		{"docs/NFViS-用户手册.md", "# 手册\n", true},
		{"internal/api/cli_help.go", "package api\n", true},
		{"internal/api/cli_help_test.go", "package api\n", false},
		// 无后缀但在别处的 shell 脚本：按 shebang 认出来
		{"contrib/hooks/pre-commit", "#!/bin/sh\necho hi\n", true},
		{"contrib/data/notes", "no shebang here\n", false},
		// 设计/契约/验收类文档有意不扫（引用即追溯落点）
		{"docs/NFViS-系统产品需求与目标架构规格书.md", "# 规格书\n", false},
		{"docs/NFViS-CLI命令全表.md", "# 全表\n", false},
		{"docs/V1-验收检查表.md", "# 检查表\n", false},
		{"docs/M5-验收记录.md", "# 记录\n", false},
	}
	for _, c := range cases {
		if _, got := scopeFor(c.rel, []byte(c.data)); got != c.wantScan {
			t.Errorf("scopeFor(%q) = %v，期望 %v", c.rel, got, c.wantScan)
		}
	}

	// 注释豁免的两条关键语义：`#` 在引号内/词中**不是**注释（否则 `echo "a#b"` 会被误剥），
	// 行首/空白后的 `#` 是注释（否则注释里的编号会误报）。
	for _, c := range []struct{ in, want string }{
		{`log "提示（FR-OPS-041）"`, `log "提示（FR-OPS-041）"`},
		{`log "提示（FR-OPS-041）" # 注释里的 FR-OPS-013 不算`, `log "提示（FR-OPS-041）" `},
		{`echo "a#b"`, `echo "a#b"`},
		{`x=a#b`, `x=a#b`},
		{`x=1  # FR-XXX-000`, `x=1  `},
		{`# FR-XXX-000 整行注释`, ``},
		{`echo "\# FR-XXX-000"`, `echo "\# FR-XXX-000"`},
	} {
		if got := hashCommentPart(c.in); got != c.want {
			t.Errorf("hashCommentPart(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
	// systemd 无内联注释：`Description=` 里的 `#` 必须当字面量留着（否则会漏掉真实引用）。
	if got := unitCommentPart(`Description=a # FR-XXX-000`); got != `Description=a # FR-XXX-000` {
		t.Errorf("unitCommentPart 应保留内联 # ，得到 %q", got)
	}
	if got := unitCommentPart("\t; FR-XXX-000"); got != "" {
		t.Errorf("unitCommentPart 应豁免行首 ; 注释，得到 %q", got)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
