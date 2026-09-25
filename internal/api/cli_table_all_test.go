package api

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/schema"
)

// 《CLI 命令全表》⇄ 命令树的**全族**守护：表里每一条命令行都必须能在树里解析到，
// 且表里有 class 列时（`request` 族与「其余操作命令」两张表有）必须与 `RequiredClass()` 一致。
//
// 为什么在 `TestRequestClassMatchesCommandTable` 之外再要一份：那份只守 `request` 族
// （class 列最要紧的一族，`Su()`/`Op()` 标记都挂在那里），而「文档 ⇄ 代码树」的漂移不止那一族——
// `show` 族的省略写法（`show interfaces <ifname> detail` 的 `physical` 可省）、
// 配置模式的关键字路径、其余操作命令在树里的位置，各自的漂移都不会被任何守护看见。
// 表是操作者手里的命令参考：表里写了、树里没有 = `?`/Tab 补不出来，`cli_bridge`
// 的前置校验也过不去（`cliexec.go` 用 `schema.Match` 校验），所以两侧必须同源。
//
// 与既有守护的分工：`tokensOfTableCommand` 遇到关键字级多选（`a|b`）即放弃（返回 ok=false，交人工），
// 本守护把多选**机械展开成多条备选**，每条都要解析到、且 class 一致——判据更严，跳过数因此从
// 「多选行全跳」降到只剩「文档简写」几行。既有函数与既有测试的行为都没动。
//
// 形态约定（照实处理；见《命令全表》§0 与各表表头）：
//   - 行形如 `| \`命令\` | 说明 | [权限] | 落点 | 实测 |`；**只有 request 族与「其余操作命令」
//     两张表有「权限」列**（按表头判定，不看某一行恰好有几个单元格——漏写的行会静默失去守护），
//     其余表 4 列（show）/3 列（通用管道），无权限列的行只断言「能否解析到」；
//   - 关键字级多选写成 `a\|b`（两侧无空格，故按 " | " 切单元格是安全的）；
//   - 一行可能用 ` / ` 并列多条命令（如 edit <path> / up / top / exit 写在同一格），逐条判；
//   - 可选组 `[...]`（可嵌套）**两种形态都试**（剥掉 / 给出），`<...>` 占位符替换为探针取值；
//   - 非命令行（通用管道的 `\| match`、`?`、`…` 简写、`show | display set` 的管道段、
//     `edit <path>` 这类占位符代表整段子语法的简写）识别并跳过，**逐条 t.Logf 登记**且设上限。
//
// 判据边界（如实登记，别把本守护当成万能的）：`schema.Match` 是**路径匹配**——不做取值枚举校验，
// 且关键字位置匹配不上时会用 `firstParam()`/`singleValue()` 兜底（值叶子消耗 token 后回退一层）。
// 因此本守护能抓到的是「文档写了、树里根本没有这条路径」（关键字位置报 `未知命令`），
// 抓不到「位置该是关键字、树里却是取值」这类结构错位（会被兜底吃掉）。
// 另外 `show` 族的表没有权限列，故只判路径不判 class；class 一致性由 `request`/「其余操作」两张表守。
func TestCommandTableMatchesCommandTree(t *testing.T) {
	b, err := os.ReadFile("../../docs/NFViS-CLI命令全表.md")
	if err != nil {
		t.Fatalf("读取命令全表: %v", err)
	}
	docClasses := map[string]schema.Class{"R": schema.ClassReadOnly, "O": schema.ClassOperator, "S": schema.ClassSuperUser}

	var rows, cmds, checked, classChecked, skipped, fellBackToGroups int
	family := map[string]int{}
	skipReasons := map[string]int{}

	tableCells, tableHasClass := 0, false
	table := "<未知表>"
	inScope := false
	for i, line := range strings.Split(string(b), "\n") {
		lineNo := i + 1
		if strings.HasPrefix(line, "## ") {
			// §1 操作模式、§2 配置模式是命令行；§0 阅读约定、§3 统计、§4 已知限制不是。
			inScope = strings.HasPrefix(line, "## 1.") || strings.HasPrefix(line, "## 2.")
			table = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			continue
		}
		if !inScope {
			continue
		}
		if strings.HasPrefix(line, "| ") && !strings.HasPrefix(line, "| `") {
			// 表头（`| 命令 | 说明 | …`、通用管道的 `| 管道 | 说明 | 实测 |`）：列数随表变，
			// 权限列的有无按表头判定。
			cells := splitTableCells(line)
			if len(cells) >= 2 && cells[1] == "说明" {
				tableCells = len(cells)
				tableHasClass = len(cells) >= 3 && cells[2] == "权限"
			}
			continue
		}
		if !strings.HasPrefix(line, "| `") {
			continue
		}

		rows++
		cells := splitTableCells(line)
		if tableCells > 0 && len(cells) != tableCells {
			t.Errorf("第 %d 行只切出 %d 个单元格，本表表头是 %d 列——列缺失（少写的权限/落点/实测列让这行核对不了）：%s\n    所属表：%s",
				lineNo, len(cells), tableCells, line, table)
		}
		docClass := ""
		if tableHasClass && len(cells) > 2 {
			docClass = strings.TrimSpace(cells[2])
			if _, ok := docClasses[docClass]; !ok {
				t.Errorf("第 %d 行的 class 列是 %q（应为 R/O/S）：%s", lineNo, docClass, line)
				docClass = ""
			}
		}

		for _, cmd := range splitTableCommands(cells[0]) {
			cmds++
			family[tableFamily(cmd)]++

			if reason := tableNonCommand(cmd); reason != "" {
				skipped++
				skipReasons[reason]++
				t.Logf("跳过（%s）：第 %d 行 %s", reason, lineNo, cmd)
				continue
			}
			alts, reason := tableTokensOf(cmd, false)
			if reason != "" {
				skipped++
				skipReasons[reason]++
				t.Logf("跳过（%s）：第 %d 行 %s", reason, lineNo, cmd)
				continue
			}

			root := tableRoot(cmd)
			node, matched, shorthand, fails := matchTableAlts(root, alts)
			if !matched {
				// 可选组剥掉后解析不了，再试「把可选组都给出」的形态：表里的 `[<seq>]`
				// 与树里的 `Opt(PT("<seq>"))` 是同一件事，两种形态都存在，任一种成立即算解析到。
				if kept, kreason := tableTokensOf(cmd, true); kreason == "" {
					if knode, kmatched, _, _ := matchTableAlts(root, kept); kmatched {
						node, matched, fellBackToGroups = knode, true, fellBackToGroups+1
					}
				}
			}
			switch {
			case matched:
				checked++
				if docClass != "" {
					classChecked++
					if got := node.RequiredClass().String(); got != docClass {
						t.Errorf("第 %d 行的命令 %q：《命令全表》写 %s，命令树算出来是 %s——两侧必须同源（要么补 Su()/Op()，要么改表并说明理由）\n    原文：%s",
							lineNo, cmd, docClass, got, line)
					}
				}
			case shorthand != "":
				// 占位符代表的是一段子语法（`edit <path>`、`run <oper-command>`），
				// 单个探针 token 机械展开不了——登记后跳过，不当「解析不到」。
				skipped++
				skipReasons[shorthandSkipReason]++
				t.Logf("跳过（%s）：第 %d 行 %s —— %s", shorthandSkipReason, lineNo, cmd, shorthand)
			default:
				if len(fails) == 0 { // 不该发生（备选至少一条，且上面已排掉「简写」）：报出来别 panic
					t.Errorf("第 %d 行的命令 %q 既没解析到、也没给出失败原因——解析器自身有问题", lineNo, cmd)
					continue
				}
				f := fails[0]
				t.Errorf("第 %d 行的命令 %q 在命令树里解析不到：%v\n    文档 token（可选组已剥掉）%v，失败在第 %d 个：%s\n    原文：%s",
					lineNo, cmd, f.err, f.toks, f.depth+1, f.toks[f.depth], line)
			}
		}
	}

	t.Logf("命令全表全族核对：解析行 %d 行 / 命令 %d 条（解析到 %d 条，跳过 %d 条，class 核对 %d 次，靠「可选组给出」形态解析到 %d 条）",
		rows, cmds, checked, skipped, classChecked, fellBackToGroups)
	for _, fam := range []string{"show", "request", "其余操作", "配置模式"} {
		t.Logf("  族 %s：%d 条", fam, family[fam])
	}
	t.Logf("  （族按命令首 token 归类：§2.1 的裸 `show` 与 `show | display set` 因此计在 show 族；`?`/Tab 与管道段也在内）")
	reasons := make([]string, 0, len(skipReasons))
	for r := range skipReasons {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		t.Logf("  跳过原因「%s」：%d 条", r, skipReasons[r])
	}

	// 三层防「解析器失效后静默全绿」：行数下限、实际核对数下限、跳过数上限。
	if rows < 240 {
		t.Errorf("只读到 %d 行命令——解析器可能失效（表格式变了？）", rows)
	}
	if checked < 200 {
		t.Errorf("只真正核对了 %d 条命令——判据可能被跳过规则吃掉了", checked)
	}
	if skipped > 40 {
		t.Errorf("跳过 %d 条（上限 40）——跳过太多说明解析器跟不上表里的写法，本守护会失去判别力", skipped)
	}
}

const shorthandSkipReason = "文档简写（占位符代表一段子语法）"

// splitTableCells 按 " | " 切单元格（命令里的多选写成 `\|`、两侧无空格，故这样切是安全的），
// 并去掉首单元格的 "| " 前缀与末单元格的结尾 "|"。
func splitTableCells(line string) []string {
	parts := strings.Split(strings.TrimSpace(line), " | ")
	if len(parts) == 0 {
		return nil
	}
	parts[0] = strings.TrimPrefix(parts[0], "| ")
	parts[len(parts)-1] = strings.TrimSpace(strings.TrimSuffix(parts[len(parts)-1], "|"))
	return parts
}

// splitTableCommands 拆出单元格里的命令（a / b 形态并列多条，各自带反引号），逐条去掉反引号。
func splitTableCommands(cell string) []string {
	var out []string
	for _, p := range strings.Split(cell, " / ") {
		if c := strings.Trim(strings.TrimSpace(p), "`"); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// tableNonCommand 识别**不是命令树内容**的写法，返回跳过原因（空表示是候选命令）。
func tableNonCommand(cmd string) string {
	switch {
	case strings.HasPrefix(cmd, `\|`) || strings.HasPrefix(cmd, "|"):
		return "管道段（通用管道的 `\\| …`，不属于命令树）"
	case cmd == "?" || cmd == "Tab":
		return "交互按键（按键即时，不是命令行）"
	case strings.Contains(cmd, "…"):
		return "文档简写（… 省略了路径或子命令）"
	}
	return ""
}

// configFirstTokens 配置模式（`ConfigRoot()`）的顶层固定命令。
// `show`/`exit` 两棵树都有：本守护归操作树——《命令全表》§1.1 的 show 行就是操作模式；
// §2.1 的裸 `show`/`exit` 在配置树里，但这里只判「能不能解析到」，两棵树都成立故结论不变。
var configFirstTokens = map[string]bool{
	"set": true, "delete": true, "annotate": true, "edit": true, "up": true, "top": true,
	"commit": true, "rollback": true, "load": true, "save": true, "run": true, "discard": true,
}

func tableRoot(cmd string) *schema.Node {
	if f := strings.Fields(cmd); len(f) > 0 && configFirstTokens[f[0]] {
		return schema.ConfigRoot()
	}
	return schema.OperRoot()
}

func tableFamily(cmd string) string {
	f := strings.Fields(cmd)
	switch {
	case len(f) == 0:
		return "其他"
	case f[0] == "show":
		return "show"
	case f[0] == "request":
		return "request"
	case configFirstTokens[f[0]]:
		return "配置模式"
	default:
		return "其余操作"
	}
}

// tableAlt 一条可机械展开的 token 序列。
type tableAlt struct {
	toks []string // 交给 schema.Match 的 token（占位符已换探针、多选已展开）
	src  []string // 对应位置在文档里的原文（报错时给人看的是这一份）
}

// maxTableAlts 多选展开的备选上限（现状最多 4：`rx-queues|tx-queues|rx-descriptors|tx-descriptors`）。
const maxTableAlts = 8

// tableTokensOf 把文档命令文本转成备选 token 序列：
//   - 占位符 `<x>`、`<x|y>` → 探针取值（与操作者敲命令的形态一致；占位符里的 `|` 是取值多选，
//     不参与关键字级展开）；
//   - 可选组 `[...]`（含嵌套）：keepGroups=false 整组剥掉，true 则原样保留——`[<seq>]`、
//     `[trunk vlans <l>|native <v>]` 两种形态都是操作者会敲的，调用方两种都试；
//   - 关键字级多选 `a|b` → 展开成多条备选，每条都要解析到且 class 一致；
//   - 管道分隔符 `|`（`show | display set`）→ 无法机械展开，返回原因跳过。
func tableTokensOf(cmd string, keepGroups bool) ([]tableAlt, string) {
	alts := []tableAlt{{}}
	depth := 0
	for _, raw := range strings.Fields(cmd) {
		tk, opened, closed := trimGroups(raw)
		inGroup := depth > 0 || opened > 0
		depth += opened
		if tk != "" && (keepGroups || !inGroup) {
			switch {
			case tablePlaceholderRe.MatchString(tk):
				for i := range alts {
					alts[i].toks = append(alts[i].toks, "probe")
					alts[i].src = append(alts[i].src, tk)
				}
			case tk == "|" || tk == `\|`:
				return nil, "管道段（`show | display set`，不属于命令树）"
			case strings.Contains(tk, "|"):
				opts := strings.Split(strings.ReplaceAll(tk, `\|`, "|"), "|")
				var next []tableAlt
				for _, a := range alts {
					for _, o := range opts {
						next = append(next, tableAlt{
							toks: append(append([]string{}, a.toks...), o),
							src:  append(append([]string{}, a.src...), tk),
						})
					}
				}
				alts = next
				if len(alts) > maxTableAlts {
					return nil, fmt.Sprintf("多选展开超过 %d 条备选", maxTableAlts)
				}
			default:
				for i := range alts {
					alts[i].toks = append(alts[i].toks, tk)
					alts[i].src = append(alts[i].src, tk)
				}
			}
		}
		depth -= closed
	}
	if depth != 0 {
		return nil, "可选组 [...] 括号不平衡"
	}
	if len(alts[0].toks) == 0 {
		return nil, "空命令"
	}
	return alts, ""
}

// trimGroups 剥掉 token 两侧的 `[`/`]`，返回剩下来的内容与本 token 开/闭了几层组。
func trimGroups(tk string) (rest string, opened, closed int) {
	for strings.HasPrefix(tk, "[") {
		opened++
		tk = strings.TrimPrefix(tk, "[")
	}
	for tk != "" && strings.HasSuffix(tk, "]") {
		closed++
		tk = strings.TrimSuffix(tk, "]")
	}
	return tk, opened, closed
}

// compoundPlaceholders 表里代表**一段子语法**（多个 token）而非单个取值的占位符名字。
// `edit <path>`（一段配置路径）、`annotate <path> "text"`、`run <oper-command>`（一整条操作命令）
// 都属此类，单个探针 token 展开不了：逐个登记后跳过。
// **只豁免这几个名字**——不按结构猜（「树里该位置只有关键字子节点」这种结构在
// `show interfaces <ifname> detail`（树里少了 `<ifname>` 参数）上完全一样，按结构猜会把真漂移吃掉）。
var compoundPlaceholders = map[string]bool{"path": true, "oper-command": true}

// tableFail 一条备选的失败现场（文档侧 token 可读，报错直接用这一份）。
type tableFail struct {
	toks  []string
	depth int
	err   error
}

// matchTableAlts 依次试各备选，返回首个能解析到的节点；全部失败时给出失败清单与「文档简写」判定。
func matchTableAlts(root *schema.Node, alts []tableAlt) (node *schema.Node, matched bool, shorthand string, fails []tableFail) {
	for _, alt := range alts {
		n, depth, err := schema.Match(root, alt.toks)
		if err == nil {
			return n, true, "", nil
		}
		if depth < len(alt.src) {
			if name := placeholderName(alt.src[depth]); compoundPlaceholders[name] && hasKeywordChild(n) {
				shorthand = fmt.Sprintf("占位符 %s 代表一段子语法（树里该位置是带子关键字的节点，单个探针 token 展开不了）", alt.src[depth])
				continue
			}
		}
		fails = append(fails, tableFail{toks: alt.src, depth: depth, err: err})
	}
	return nil, false, shorthand, fails
}

// placeholderName 取出占位符里的类型名（`<ip|host>` → `ip|host`；不是占位符则返回空）。
func placeholderName(tk string) string {
	if !tablePlaceholderRe.MatchString(tk) {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(tk, "<"), ">")
}

func hasKeywordChild(n *schema.Node) bool {
	if n == nil {
		return false
	}
	for _, c := range n.Children {
		if c.Kind == schema.Keyword {
			return true
		}
	}
	return false
}
