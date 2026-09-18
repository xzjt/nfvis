package api

// CLI 管道过滤（FR-CLI-005，命令树 §1.1 通用管道）：
// match/except/begin <re>、count、last <n> 为文本过滤；
// display json/xml 由执行器的结构化快照渲染（服务端解析，客户端只做展示）。
// 管道自左向右依次生效，可与命令解析正交组合（如 | match x | count）。

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// pipeSpec 一段管道。
type pipeSpec struct {
	kind string // match | except | count | last | begin | display-json | display-xml
	arg  string // 正则或数字
}

// splitPipes 以词边界 "|" 拆分命令与管道段（不在引号内）。
func splitPipes(line string) (string, []pipeSpec, error) {
	segments := splitUnquoted(line, '|')
	cmd := strings.TrimSpace(segments[0])
	var pipes []pipeSpec
	for _, seg := range segments[1:] {
		fields := strings.Fields(seg)
		if len(fields) == 0 {
			return "", nil, fmt.Errorf("管道段为空")
		}
		switch fields[0] {
		case "match", "except", "begin":
			if len(fields) != 2 {
				return "", nil, fmt.Errorf("%s 需要一个正则参数", fields[0])
			}
			if _, err := regexp.Compile(fields[1]); err != nil {
				return "", nil, fmt.Errorf("正则 %q 不合法: %v", fields[1], err)
			}
			pipes = append(pipes, pipeSpec{kind: fields[0], arg: fields[1]})
		case "compare":
			// 契约 §3 的两种写法：`show | compare`（candidate ⇄ committed）与
			// `show configuration | compare rollback <n>`（committed ⇄ 第 n 个历史快照）。
			// 能力早在引擎里（CompareCandidate/Compare），此前只是**没接线**（发现 #4）：
			// 操作者在 commit 前看不到自己改了什么——事务模型的核心动作缺失。
			switch {
			case len(fields) == 1:
				pipes = append(pipes, pipeSpec{kind: "compare"})
			case len(fields) == 3 && fields[1] == "rollback":
				n, err := strconv.Atoi(fields[2])
				if err != nil || n < 1 {
					return "", nil, fmt.Errorf("rollback 需要一个正整数快照序号")
				}
				pipes = append(pipes, pipeSpec{kind: "compare", arg: fields[2]})
			default:
				return "", nil, fmt.Errorf("compare 的用法：| compare 或 | compare rollback <n>")
			}
		case "count":
			pipes = append(pipes, pipeSpec{kind: "count"})
		case "last":
			if len(fields) != 2 {
				return "", nil, fmt.Errorf("last 需要一个行数参数")
			}
			if _, err := strconv.Atoi(fields[1]); err != nil {
				return "", nil, fmt.Errorf("last 行数须为整数")
			}
			pipes = append(pipes, pipeSpec{kind: "last", arg: fields[1]})
		case "display":
			if len(fields) == 2 && fields[1] == "set" {
				// 契约 §3 曾声明「配置模式 show | display set 以 set 语句展开」——未实现，
				// 已按附录 A #84 更正契约（不再是承诺）。此处给出替代路径而不谎报支持。
				return "", nil, fmt.Errorf("| display set 未实现：" +
					"导出配置用 save <file>，查看配置用 show configuration，结构化用 | display json")
			}
			if len(fields) != 2 || (fields[1] != "json" && fields[1] != "xml") {
				return "", nil, fmt.Errorf("display 仅支持 json|xml")
			}
			pipes = append(pipes, pipeSpec{kind: "display-" + fields[1]})
		default:
			return "", nil, fmt.Errorf("未知管道: %s", fields[0])
		}
	}
	return cmd, pipes, nil
}

// splitUnquoted 按竖线拆分，忽略双引号内的 |。
func splitUnquoted(line string, sep rune) []string {
	var segs []string
	var cur strings.Builder
	inQuote := false
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == sep && !inQuote:
			segs = append(segs, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(segs, cur.String())
}

// applyPipes 自左向右应用管道；display json/xml 需要执行器的结构化快照。
func (x *cliExecutor) applyPipes(text string, pipes []pipeSpec) string {
	for _, p := range pipes {
		switch p.kind {
		case "match", "except", "begin":
			text = filterLines(text, p)
		case "count":
			n := len(nonEmptyLines(text))
			text = fmt.Sprintf("计数: %d\n", n)
		case "last":
			n, _ := strconv.Atoi(p.arg)
			lines := nonEmptyLines(text)
			if n > len(lines) {
				n = len(lines)
			}
			if len(lines) == 0 {
				text = ""
			} else {
				text = strings.Join(lines[len(lines)-n:], "\n") + "\n"
			}
		case "compare":
			var (
				out string
				err error
			)
			if p.arg == "" {
				out, err = x.engine.CompareCandidate()
			} else {
				n, _ := strconv.Atoi(p.arg)
				out, err = x.engine.Compare(n)
			}
			if err != nil {
				return "%% " + err.Error() + "\n"
			}
			text = out
		case "display-json":
			if x.structured == nil {
				return "%% 该命令不支持 display json（仅配置 show 族可用）\n"
			}
			b, err := json.MarshalIndent(x.structured, "", "  ")
			if err != nil {
				return "%% 结构化输出失败: " + err.Error() + "\n"
			}
			text = string(b) + "\n"
		case "display-xml":
			if x.structured == nil {
				return "%% 该命令不支持 display xml（仅配置 show 族可用）\n"
			}
			text = renderXML(x.structured, "configuration", 0)
		}
	}
	return text
}

func nonEmptyLines(text string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func filterLines(text string, p pipeSpec) string {
	re, err := regexp.Compile(p.arg)
	if err != nil {
		return "%% 正则不合法: " + err.Error() + "\n"
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	var kept []string
	started := false
	for _, l := range lines {
		switch p.kind {
		case "match":
			if re.MatchString(l) {
				kept = append(kept, l)
			}
		case "except":
			if !re.MatchString(l) {
				kept = append(kept, l)
			}
		case "begin":
			if started || re.MatchString(l) {
				started = true
				kept = append(kept, l)
			}
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n") + "\n"
}

// renderXML 配置 JSON 树的简单 XML 渲染（display xml）。
func renderXML(v any, key string, depth int) string {
	pad := strings.Repeat("  ", depth)
	switch x := v.(type) {
	case map[string]any:
		var b strings.Builder
		fmt.Fprintf(&b, "%s<%s>\n", pad, xmlKey(key))
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			b.WriteString(renderXML(x[k], k, depth+1))
		}
		fmt.Fprintf(&b, "%s</%s>\n", pad, xmlKey(key))
		return b.String()
	case []any:
		var b strings.Builder
		for _, e := range x {
			b.WriteString(renderXML(e, singular(key), depth))
		}
		return b.String()
	default:
		return fmt.Sprintf("%s<%s>%s</%s>\n", pad, xmlKey(key), xmlEscape(scalarStringOf(v)), xmlKey(key))
	}
}

// singular 数组元素标签去复数（近似：去尾部 s）。
func singular(k string) string {
	if strings.HasSuffix(k, "s") && len(k) > 1 {
		return k[:len(k)-1]
	}
	return k
}

func xmlKey(k string) string {
	out := strings.ReplaceAll(k, "-", "_")
	if out == "" {
		return "item"
	}
	return out
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}
