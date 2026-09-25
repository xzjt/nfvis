package api

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/schema"
)

// 《命令全表》⇄ 命令树的**反方向**守护：树里能敲的操作形态，表里必须查得到。
//
// 与 `TestCommandTableMatchesCommandTree`（文档 → 树）正好相反：那份保证「表里写的都解析得到」，
// 本份保证「树里有的都收录了」。两者缺一不可——只有正向守护时，**产品新增/改名一个操作命令**
// （树里改了、表里没改）不会被任何测试看见：操作者能敲、`?` 也补得出来，却在这份命令参考里查不到。
//
// 覆盖面边界（**故意如此**，不是遗漏）：
//   - 只对**操作模式**树（`schema.OperRoot()`）的叶子形态逐条断言，族分 `show` / `request` /
//     其余操作（other）/ 管道四类；
//   - **配置模式（set/delete）不做逐条覆盖断言**：《命令全表》§2 是**策展合并口径**
//     （`set … interfaces <vnic> mac <mac>` 用 `…` 省略前导路径、`set vpp dpdk dev rx-queues\|… <n>`
//     用 `\|…` 省略同类项、`set system api tls cert-file <p> key-file <p>` 把两个 token 压在一格），
//     逐条机械比对会大面积假红——那些省略恰恰是给人读的。配置模式的漂移由
//     `TestCommandTableMatchesCommandTree`（正向）与 `cli_mapping_test.go` 的契约语句清单兜。
//   - 「管道」族的树侧形态数为 **0**：`schema.PipeKeywords` 是独立变量、不在 `OperRoot()` 里
//     （管道是前端对 show 输出的后处理，不是命令树节点）。该族的断言因此是**空断言**，
//     保留它只为把「四族」这件事写实；管道行的收录由正向守护与本测试的家底自检覆盖。
//
// 判据（**形态 token 数 + 逐 token 通配**，不用「前缀匹配即算」这种无判别力的松判据）：
//   - 树侧每条叶子形态：根到叶子，关键字照写，Param/Value 节点写其占位符（`<ifname>`/`<n>`/`<state>`…）；
//     可选节点（`Opt`）**取与不取两种形态都算**（`show vpp runtime [thread <id>]` 一行覆盖两条）；
//     枚举规则与「参数组层级」清单见 `coverLeafShapes` 与 `coverArgGroupParents` 的注释；
//   - 文档侧每行展开成若干候选：`<…>` 是通配、`a\|b` 是多选（展开成多条）、`[…\]` 是可选组
//     （取/不取都算）——展开按语法而不是按空白切词（`<deb\|url>` 里的 `|` 属取值多选，不参与关键字展开）；
//   - 命中要求**两边 token 数相同**，且每个位置：树是关键字则文档该位置必须是同名关键字；
//     树是 Param/Value 则文档该位置可以是关键字或占位符（占位符 ⇄ 参数节点互相通配）。
//
// 报出来的缺口有两种成因，**别只看文档一边**：① 表里确实漏了（补一行）；② 树与执行器漂移
// （树里有的形态执行器其实不认，或反之）——那种要改树/执行器，补文档只会把漂移抄进参考表。
// 2026-09-25 首轮跑出的三条（`show system configuration candidate`、
// `request images delete <name>`、`request images download … url <url>`（无 sha256））都属第 ② 类：
// 前两条树里写的写法执行器不认，第三条执行器**强制** sha256 而树里标了 `Opt`。
// **已就地收口（决策 #153 段内补充）**：一律改树——删掉 `show system configuration candidate`
// 这个重复且无实现的节点、`request images delete` 改成 `delete name <name>`、去掉 sha256 的 `Opt`；
// 守护见 `internal/api/cli_tree_drift_test.go`（树 ⇄ 执行器 ⇄ 校验层三方同源）。
// 缺口重现时：先判成因，再动断言——别为了变绿放宽判据。
//
// 自检（防解析器失效后静默全绿，任一不达标即 `t.Fatalf`）：文档命令行数下限、树侧叶子形态数下限、
// 「参数组层级」清单不腐化（路径仍在树里、确有多个孩子）；另有「管道族的树侧形态数必须为 0」一条断言。
func TestCommandTableCoversOperTree(t *testing.T) {
	rows, tableRows, pipeRows := coverParseDoc(t)

	shapes := coverLeafShapes(t)

	// 家底自检：解析器失效（表格式变了、树读不到）时必须吵醒人，而不是安静地 0 条全绿。
	if tableRows < coverMinDocRows {
		t.Fatalf("只解析到 %d 条《命令全表》命令行（下限 %d）——文档解析器可能已失效（表格式变了？）",
			tableRows, coverMinDocRows)
	}
	if len(shapes) < coverMinLeafShapes {
		t.Fatalf("树里只枚举到 %d 条叶子形态（下限 %d）——形态枚举器可能已失效（OperRoot 变了？）",
			len(shapes), coverMinLeafShapes)
	}

	famShapes, famDocs, famCovered := map[string]int{}, map[string]int{}, map[string]int{}
	var gaps []string
	for _, sh := range shapes {
		famShapes[sh.fam]++
		if sh.fam == "config" {
			continue // 配置模式不逐条断言，理由见文件头注释
		}
		if hit := coverFind(sh, rows); hit != nil {
			famCovered[sh.fam]++
			famDocs[hit.fam]++ // 命中它的文档行属于哪一族（跨族命中要能看见）
			continue
		}
		gaps = append(gaps, fmt.Sprintf("族 %s：%s（需要 class %s）→ %s",
			sh.fam, sh.render(), sh.terminal.RequiredClass(), coverSectionHint(sh.fam)))
	}
	for _, g := range gaps {
		t.Errorf("命令树里的操作形态未被《命令全表》收录：%s\n"+
			"    树里的来源：internal/schema/tree_oper.go（本守护遍历 OperRoot() 得到；操作者能敲、`?`/Tab 也补得出来）\n"+
			"    两种成因取其一：① 参考表确实漏了这行 → 按上面的建议补；② 该形态在树里其实敲不通/执行器不认\n"+
			"    （树与执行器漂移）→ 改树或执行器，别把漂移抄进参考表。", g)
	}

	t.Logf("《命令全表》覆盖操作树：文档命令行 %d 条（通用管道 %d 条）/ 登记不参与判定 %d 条（管道、`…` 简写、`?`/Tab）/ "+
		"树侧叶子形态 %d 条", tableRows, pipeRows, coverExpandFailures(rows), len(shapes))
	for _, fam := range []string{"show", "request", "other", "pipe"} {
		t.Logf("  族 %s：树侧 %d 条，被覆盖 %d 条，命中它的文档行 %d 条",
			fam, famShapes[fam], famCovered[fam], famDocs[fam])
	}
	t.Logf("  族 config（配置模式）：不在本守护的断言范围（策展合并口径，逐条会假红——理由见文件头注释）")
	if famShapes["pipe"] != 0 {
		t.Errorf("管道族在操作树里出现了 %d 条形态——`PipeKeywords` 是独立变量、不在 OperRoot() 里，"+
			"真出现说明树结构变了，本守护的分族需要重新核对", famShapes["pipe"])
	}
}

// ---------- 树侧：枚举操作模式树的叶子形态 ----------

// coverMinDocRows / coverMinLeafShapes 家底下限：低于它说明解析器或枚举器失效。
// 现状（2026-09-25 首轮 263 条文档行 / 147 条叶子形态；三条漂移收口后为 145 条），
// 下限留余量但不失判别力（解析器一失效就会掉到 0 或个位数）。
const (
	coverMinDocRows    = 240
	coverMinLeafShapes = 60
)

// coverTok 树侧一个 token：`literal=true` 是关键字（文档该位置必须同名），
// 否则是 Param/Value 节点（文档该位置可以是关键字或占位符）。
type coverTok struct {
	literal bool
	name    string
}

// coverShape 一条「操作者能敲出来的」叶子形态。
type coverShape struct {
	toks     []coverTok
	fam      string
	terminal *schema.Node // 末节点（报错时给 RequiredClass）
}

func (s coverShape) render() string {
	parts := make([]string, 0, len(s.toks))
	for _, t := range s.toks {
		parts = append(parts, t.name)
	}
	return strings.Join(parts, " ")
}

// coverLeafShapes 枚举操作树（OperRoot）的全部叶子形态，去重后按字典序返回。
//
// 走法（按树结构枚举，不模拟 `schema.Match` 的「无子树 Param/Value 回退父层」）：
//   - 一般层级（子节点是**互斥子命令**，如 `show system uptime|cpu|…`）：每个孩子各自走到叶子，
//     兄弟不必一起出现；
//   - **整层可省**（该层孩子全带 `Opt`，如 `show vpp runtime [thread <id>]`、`help [command]`）：
//     父节点自身也成句，故额外产生一条「停在父层」的形态；
//   - **参数组层级**（`coverArgGroupParents`，孩子是**一条命令的多个参数**而非子命令）：
//     非可选孩子必须一起出现（`request images upload name <n> type <t> file <path>`），
//     可选孩子取/不取都算——少了 `name` 或 `file` 都不是一条可用的命令，不该被当成「操作形态」。
//
// 刻意**不**追求穷尽（宁可少查，不要假红）：例如 `show vrfs <name>` 这类「子命令组的关键字
// 自身也成句」的形态不枚举（树里没有「关键字可单独成句」的标记，无法与
// `request images upload`（不可单独成句）机械区分）；它们由 `TestCommandTableMatchesCommandTree`
// 的正向守护兜。反过来，本守护只断言「枚举到的形态必须收录」。
func coverLeafShapes(t *testing.T) []coverShape {
	t.Helper()
	root := schema.OperRoot()
	coverArgGroupList(t, root) // 自检参数组清单不腐化（见该函数注释）
	byText := map[string]coverShape{}
	add := func(toks []coverTok, term *schema.Node) {
		if len(toks) == 0 {
			return
		}
		sh := coverShape{toks: toks, fam: coverFamilyOf(toks), terminal: term}
		if _, dup := byText[sh.render()]; !dup {
			byText[sh.render()] = sh
		}
	}
	var walk func(n *schema.Node, prefix []coverTok, path string)
	walk = func(n *schema.Node, prefix []coverTok, path string) {
		if coverArgGroupParents[path] {
			for _, seq := range coverForms(n) {
				add(append(append([]coverTok{}, prefix...), seq...), n)
			}
			return
		}
		if coverAllOptional(n) && len(prefix) > 0 {
			add(prefix, n) // 整层可省：父节点自身成句
		}
		for _, c := range n.Children {
			tk := coverTok{name: c.Name}
			if c.Kind == schema.Keyword {
				tk = coverTok{literal: true, name: c.Name}
			}
			next := append(append([]coverTok{}, prefix...), tk)
			if len(c.Children) == 0 {
				add(next, c)
				continue
			}
			childPath := c.Name
			if path != "" {
				childPath = path + " " + c.Name
			}
			walk(c, next, childPath)
		}
	}
	walk(root, nil, "")

	out := make([]coverShape, 0, len(byText))
	for _, sh := range byText {
		out = append(out, sh)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].render() < out[j].render() })
	return out
}

// coverForms 参数组层级的一条命令有多少种完整写法：**非可选孩子必须都取**（顺序照树），
// 可选孩子取与不取都产生一种（`request system software add <deb> [sha256 <hex>]`
// 因此是「取 sha256」与「不取 sha256」两种）。
func coverForms(n *schema.Node) [][]coverTok {
	seqs := [][]coverTok{{}}
	for _, c := range n.Children {
		tk := coverTok{name: c.Name}
		if c.Kind == schema.Keyword {
			tk = coverTok{literal: true, name: c.Name}
		}
		tails := [][]coverTok{{}}
		if len(c.Children) > 0 {
			tails = coverForms(c)
		}
		variants := make([][]coverTok, 0, len(tails)+1)
		if c.Optional {
			variants = append(variants, nil)
		}
		for _, tail := range tails {
			variants = append(variants, append([]coverTok{tk}, tail...))
		}
		next := make([][]coverTok, 0, len(seqs)*len(variants))
		for _, s := range seqs {
			for _, v := range variants {
				next = append(next, append(append([]coverTok{}, s...), v...))
			}
		}
		seqs = next
	}
	return seqs
}

// coverAllOptional 该节点的孩子是否全带 `Opt`（`[...]`）——是则节点自身也能成句。
func coverAllOptional(n *schema.Node) bool {
	for _, c := range n.Children {
		if !c.Optional {
			return false
		}
	}
	return len(n.Children) > 0
}

// coverArgGroupParents **参数组层级**（孩子是一条命令的多个参数，不是互斥子命令）的显式清单，
// 键是空格连接的关键字路径（Root 不出现在键里）。
//
// 为什么需要清单：树里没有区分「互斥子命令」与「顺序参数」的标记——`Optional` 只表达文档的 `[...]`，
// 而两者的差别决定叶子形态是否完整：`show system cpu` 只取一个孩子（兄弟是别的子命令），
// `request images upload name <n>` 少了 `type`/`file` 就不是一条能用的命令。
// 没有机械判据能分开这两类（都是「关键字下挂带取值的孩子」），故如实列清单；
// 新增这类多参数命令时要往这里补一条，`coverArgGroupList` 会确证清单不腐化（节点真实存在且确有多个孩子）。
var coverArgGroupParents = map[string]bool{
	"request images upload":       true,
	"request images download":     true,
	"request sriov create-vfs":    true,
	"request sriov delete-vfs":    true,
	"request vpp trace start":     true,
	"request system software add": true,
	// 「一个必填位置参数 + 若干可选开关」也是参数组：只给开关不给位置参数不是能用的命令
	// （`ping count <n>`、`monitor interfaces interval <sec>`、`traceroute vrf <name>`）。
	"ping":               true,
	"traceroute":         true,
	"monitor interfaces": true,
}

// coverArgGroupList 自检参数组清单：每条路径都必须在 OperRoot 里找得到，且该节点确有 ≥2 个孩子
// （否则清单腐化——比如命令被改名/挪位——后续会静默失去这些层级的覆盖判定）。
func coverArgGroupList(t *testing.T, root *schema.Node) {
	t.Helper()
	paths := make([]string, 0, len(coverArgGroupParents))
	for p := range coverArgGroupParents {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		n, err := schema.Find(root, strings.Fields(p)...)
		if err != nil {
			t.Fatalf("参数组清单里的路径 %q 在 OperRoot 里找不到（清单已腐化，请同步）：%v", p, err)
		}
		if len(n.Children) < 2 {
			t.Fatalf("参数组清单里的 %q 只有 %d 个孩子——「参数组」至少要两个参数；"+
				"若该命令已改成单参数，请把这条从 coverArgGroupParents 删掉", p, len(n.Children))
		}
	}
}

// coverFamilyOf 形态属于哪一族（按首 token；与文档侧 `coverFamilyOfCmd` 同一口径）。
func coverFamilyOf(toks []coverTok) string {
	if len(toks) == 0 {
		return "empty"
	}
	switch toks[0].name {
	case "show":
		return "show"
	case "request":
		return "request"
	default:
		return "other"
	}
}

// coverSectionHint 报错时告诉维护者「该往哪一节补」。
func coverSectionHint(fam string) string {
	switch fam {
	case "show":
		return "§1.1 show 表（按同族命令就近插入，缺权限列故无需 class）"
	case "request":
		return "§1.2 request 表（记得填 class 列：与树里 RequiredClass() 一致）"
	case "other":
		return "§1.3 其余操作命令表（有 class 列）"
	case "pipe":
		return "§1.1 通用管道表"
	default:
		return "§2 配置模式（本守护不断言配置模式，请人工核对）"
	}
}

// ---------- 文档侧：解析《命令全表》并展开每行的候选形态 ----------

// coverDocTok 文档侧一个 token 位置的备选：`wild=true` 是占位符（`<x>`/`<n>`/`<ifname>`），
// 否则是与树里关键字**同名**的字面量。
type coverDocTok struct {
	wild bool
	lit  string
}

// coverDocRow 文档里一条命令行（同一物理行 ` / ` 并列的算多条，行号相同）。
type coverDocRow struct {
	lineNo int
	table  string
	fam    string
	text   string
	cands  [][]coverDocTok // 展开出的候选形态；展开失败时为空
	reason string          // 展开失败的原因（非空即只计家底、不参与覆盖判定）
}

const (
	coverMaxCands  = 64 // 单条文档行展开上限（防嵌套多选组合爆炸）
	coverMaxLogged = 40 // 「登记不参与判定」的行最多逐条打印多少（避免刷屏，计数照全）
)

// coverParseDoc 读《命令全表》§1/§2 的命令行，逐行展开成候选形态。
// 返回全部行、命令行数（家底）与通用管道行数。
func coverParseDoc(t *testing.T) (rows []coverDocRow, tableRows, pipeRows int) {
	t.Helper()
	b, err := os.ReadFile("../../docs/NFViS-CLI命令全表.md")
	if err != nil {
		t.Fatalf("读取命令全表: %v", err)
	}
	table := "<未知表>"
	inScope := false
	logged := 0
	for i, line := range strings.Split(string(b), "\n") {
		lineNo := i + 1
		if strings.HasPrefix(line, "## ") {
			// §1 操作模式、§2 配置模式是命令行；§0 阅读约定、§3 统计、§4 已知限制不是。
			inScope = strings.HasPrefix(line, "## 1.") || strings.HasPrefix(line, "## 2.")
			table = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			continue
		}
		if !inScope || !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := splitTableCells(line)
		if len(cells) == 0 {
			t.Errorf("第 %d 行切不出单元格：%s", lineNo, line)
			continue
		}
		for _, cmd := range splitTableCommands(cells[0]) {
			tableRows++
			row := coverDocRow{lineNo: lineNo, table: table, text: cmd, fam: coverFamilyOfCmd(cmd)}
			switch {
			case coverIsPipe(cmd):
				pipeRows++
				row.reason = "通用管道段（`\\| match` 等，不是命令树节点）"
			case tableNonCommand(cmd) != "":
				row.reason = tableNonCommand(cmd)
			default:
				row.cands, row.reason = coverExpandCmd(cmd)
			}
			rows = append(rows, row)
			if row.reason != "" {
				logged++
				if logged <= coverMaxLogged {
					t.Logf("登记（不参与覆盖判定）：第 %d 行 %s —— %s", lineNo, cmd, row.reason)
				} else if logged == coverMaxLogged+1 {
					t.Logf("（登记行超过 %d 条，其余不再逐条打印——以汇总计数为准）", coverMaxLogged)
				}
			}
		}
	}
	return rows, tableRows, pipeRows
}

// coverFamilyOfCmd 文档命令的族（与树侧 `coverFamilyOf` 同一口径，另加管道与配置模式）。
func coverFamilyOfCmd(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return "empty"
	}
	switch {
	case coverIsPipe(cmd):
		return "pipe"
	case f[0] == "show":
		return "show"
	case f[0] == "request":
		return "request"
	case configFirstTokens[f[0]]:
		return "config"
	default:
		return "other"
	}
}

// coverIsPipe 通用管道行（首 token 是 Markdown 转义后的 `\|` 或裸 `|`）。
func coverIsPipe(cmd string) bool {
	return strings.HasPrefix(cmd, `\|`) || strings.HasPrefix(cmd, "|")
}

// coverExpandFailures 统计展开失败（登记）的文档行数。
func coverExpandFailures(rows []coverDocRow) int {
	n := 0
	for _, r := range rows {
		if r.reason != "" || len(r.cands) == 0 {
			n++
		}
	}
	return n
}

// coverFind 找一条能覆盖该形态的文档行（返回命中的行；没有则 nil）。跨族命中照算，但日志里能看见。
func coverFind(sh coverShape, rows []coverDocRow) *coverDocRow {
	for i := range rows {
		for _, cand := range rows[i].cands {
			if coverShapeHit(sh.toks, cand) {
				return &rows[i]
			}
		}
	}
	return nil
}

// coverShapeHit 形态判定：**token 数相同 + 逐 token 通配**。
// 树里是关键字的位置 → 文档该位置必须是同名关键字（占位符不算命中：那意味着
// 参考表把关键字写成了取值，操作者照表敲会撞「未知命令」）；
// 树里是 Param/Value 的位置 → 文档该位置是关键字或占位符都算（占位符 ⇄ 参数节点互相通配）。
func coverShapeHit(shape []coverTok, cand []coverDocTok) bool {
	if len(shape) != len(cand) {
		return false
	}
	for i := range shape {
		if !shape[i].literal {
			continue
		}
		if cand[i].wild || cand[i].lit != shape[i].name {
			return false
		}
	}
	return true
}

// ---------- 文档文本 → 候选形态 ----------

// coverLex 文档命令文本的 token 流：`w` 单词（含 `<…>` 占位符）、`[`/`]` 组括号、
// `|` 多选分隔符（文档里写作 `\|`）。
type coverLex struct {
	kind byte
	text string
}

// coverLexCmd 按语法切词（不是按空白切）：`<…>` 整段是一个 token（其内部的 `\|` 属取值多选，
// 不参与关键字展开），`[\|]` 独立成 token。`[\|]` 与 `<…>` 都不含空格，故扫到分界符即停。
func coverLexCmd(cmd string) []coverLex {
	var out []coverLex
	for i := 0; i < len(cmd); {
		switch ch := cmd[i]; {
		case ch == ' ' || ch == '\t':
			i++
		case ch == '[':
			out = append(out, coverLex{kind: '['})
			i++
		case ch == ']':
			out = append(out, coverLex{kind: ']'})
			i++
		case ch == '|':
			out = append(out, coverLex{kind: '|'})
			i++
		case ch == '\\' && i+1 < len(cmd) && cmd[i+1] == '|':
			out = append(out, coverLex{kind: '|'})
			i += 2
		case ch == '<':
			j := strings.IndexByte(cmd[i:], '>')
			if j < 0 {
				j = len(cmd) - i - 1
			}
			out = append(out, coverLex{kind: 'w', text: cmd[i : i+j+1]})
			i += j + 1
		default:
			j := i
			for j < len(cmd) && !strings.ContainsRune(" \t[]|\\", rune(cmd[j])) {
				j++
			}
			if j == i { // 孤立的 `\` 之类：别死循环
				j++
			}
			out = append(out, coverLex{kind: 'w', text: cmd[i:j]})
			i = j
		}
	}
	return out
}

// coverItem 一层里的一个「位置」：普通 token，或 `[…]` 可选组（组内自带备选序列）。
type coverItem struct {
	word     string
	wild     bool
	optional bool
	sub      [][]coverItem // optional 组内部的备选（组内的 `|` 各切一条）
}

// coverParseLevel 解析一层：读到 `]`（或串尾）为止；同层的 `|` 把该层切成多个备选序列
// （`[active\|all]` 两条、`[trunk vlans <l>\|native <v>]` 是「trunk vlans <l>」与「native <v>」两条）。
// 返回下一个待读位置（停在 `]` 上，由调用方消耗）。
func coverParseLevel(ts []coverLex, i int) ([][]coverItem, int) {
	var alts [][]coverItem
	var cur []coverItem
	for i < len(ts) {
		switch ts[i].kind {
		case ']':
			return append(alts, cur), i
		case '|':
			alts = append(alts, cur)
			cur = nil
			i++
		case '[':
			sub, next := coverParseLevel(ts, i+1)
			cur = append(cur, coverItem{optional: true, sub: sub})
			i = next
			if i < len(ts) && ts[i].kind == ']' {
				i++
			}
		default:
			cur = append(cur, coverItem{word: ts[i].text, wild: tablePlaceholderRe.MatchString(ts[i].text)})
			i++
		}
	}
	return append(alts, cur), i
}

// coverExpandCmd 把一条文档命令行展开成候选形态集合（可达上限即如实登记、不硬截断成"命中"）。
func coverExpandCmd(cmd string) ([][]coverDocTok, string) {
	lex := coverLexCmd(cmd)
	depth := 0
	for _, t := range lex {
		switch t.kind {
		case '[':
			depth++
		case ']':
			depth--
		}
	}
	if depth != 0 {
		return nil, fmt.Sprintf("可选组 `[...]` 括号不平衡（%d 层未闭合）", depth)
	}
	alts, _ := coverParseLevel(lex, 0)
	var out [][]coverDocTok
	for _, seq := range alts {
		coverExpandSeq(seq, nil, &out)
		if len(out) > coverMaxCands {
			return nil, fmt.Sprintf("展开超过 %d 条候选（多选/可选组嵌套过多）", coverMaxCands)
		}
	}
	if len(out) == 0 {
		return nil, "展开后没有候选形态（空命令？）"
	}
	return out, ""
}

// coverExpandSeq 把一个 item 序列展开成候选 token 序列：可选组取与不取都产生候选。
func coverExpandSeq(items []coverItem, prefix []coverDocTok, out *[][]coverDocTok) {
	if len(*out) > coverMaxCands {
		return
	}
	if len(items) == 0 {
		*out = append(*out, append([]coverDocTok{}, prefix...))
		return
	}
	it, rest := items[0], items[1:]
	if it.optional {
		coverExpandSeq(rest, prefix, out) // 不取
		for _, sub := range it.sub {
			joined := make([]coverItem, 0, len(sub)+len(rest))
			joined = append(joined, sub...)
			joined = append(joined, rest...)
			coverExpandSeq(joined, prefix, out)
		}
		return
	}
	coverExpandSeq(rest, append(append([]coverDocTok{}, prefix...),
		coverDocTok{wild: it.wild, lit: it.word}), out)
}
