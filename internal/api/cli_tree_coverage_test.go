package api

// cli_tree_coverage_test.go —— 命令树 ⇄ 覆盖表的**全树**守护（关闭「代表清单」空洞）。
//
// 由来：既有的 TestContractCLICommandsAreClassified 只校验**手工维护的** contractCLICommands
// 代表清单——它红了只说明「代表清单里的命令都归类了」，**判不出树里还有没被收进代表清单的命令**，
// 于是「0 缺口」严格讲只是「已归类集合内的 0 缺口」。`show system metrics history` 就是漏网实例：
// 树里有（tree_oper.go 的 metrics→history）、端点在（server.go 注册时第三个参数就是它服务的
// CLI 命令名），但 cliRESTCoverage 与 contractCLICommands 两张表都没收它。
//
// 本守护**遍历命令树**：把每一片「可成句的叶子形态」逐条喂给 classify()，未归类即红并逐条点名。
// 它不替代那两张表，而是给它们补上「树的全部形态都已被归类」这条**穷尽性**断言。
//
// 口径（「一条命令」= 操作者真能敲出来的一条形态）：
//   - 遍历 schema.OperRoot()（操作树）与 schema.ConfigRoot()（配置树）。
//   - 关键字节点照写其名；Param/Value 节点写其占位符（`<ifname>`/`<n>`/`<duration>`…）——
//     与 cliFormMatch 的 `<...>` 通配口径一致。
//   - **容器节点**（其孩子是互斥子命令，如 `show system` 下的 `uptime|cpu|memory|…`）自身不是
//     一条命令，只作前缀；只有「**无孩子**」或「**孩子全带 Opt**（整层可省，如
//     `show vpp runtime [thread <id>]`、`show system metrics history [name <metric>]`）」的节点成句。
//   - **参数组层级**（孩子是一条命令的多个参数而非子命令，清单复用既有 coverArgGroupParents）：
//     非可选孩子必须都取、可选孩子取/不取都算（复用既有 coverForms 口径）——少了必填参数
//     （如 `request images download name <n>`）不是一条能用的命令，不该被当成操作形态。
//   - RequireSub 的实例参数（如 `set system login user <name>`）**不能单独成句**，跳过。
//   - 配置树的**路径族动词**（`set`/`delete`/`show`/`annotate`/`edit`/`run`）：路径是同一份语句
//     语法（ConfigPathTree）挂到这些动词上的，**不逐路径展开**（展开是 1000+ 条、且全是同一份
//     candidate API / CLI 本地导航，与《命令全表》§2.4/§4 的「行」口径一致）——按动词粒度断言：
//     `set <path>`/`delete <path>` 由 candidate API 前缀规则覆盖、`show` 走覆盖表、
//     `annotate`/`edit`/`run` 走例外表。语句语法本身另由 configStatementForms() 单独枚举、
//     逐条断言落 candidate API（非空断言）。
//
// 匹配**复用既有 cliFormMatch/optGroupRe**（`<…>` 通配任意单 token、key 里的 `[...]` 匹配时剥除），
// 不另立一套。

import (
	"sort"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/schema"
)

// treeForm 命令树里的一片可成句叶子形态。
type treeForm struct {
	form string       // 形态串（token 以空格连接；Param/Value 用其占位符）
	node *schema.Node // 末节点（报错时给 RequiredClass）
}

// coverTokNames 把 coverForms 产出的 token 序列转成名字列表。
func coverTokNames(seq []coverTok) []string {
	out := make([]string, 0, len(seq))
	for _, t := range seq {
		out = append(out, t.name)
	}
	return out
}

// collectNode 从节点 n（prefix 已含 n 自身）出发，收集它及子树的可成句叶子形态。
// 与 coverLeafShapes 的走法同源（含「整层可省」与「参数组」两种口径），只是参数化了根。
func collectNode(n *schema.Node, prefix []string, path string, argGroups map[string]bool, out *[]treeForm) {
	add := func(toks []string) {
		if len(toks) > 0 {
			*out = append(*out, treeForm{form: strings.Join(toks, " "), node: n})
		}
	}
	if argGroups[path] {
		for _, seq := range coverForms(n) {
			add(append(append([]string{}, prefix...), coverTokNames(seq)...))
		}
		return
	}
	if len(n.Children) == 0 {
		add(prefix) // 无孩子：本节点自身就是一条命令
		return
	}
	if coverAllOptional(n) {
		add(prefix) // 整层可省：本节点自身成句
	}
	for _, c := range n.Children {
		if c.RequireSub {
			continue // 不能单独成句的实例参数
		}
		next := append(append([]string{}, prefix...), c.Name)
		if len(c.Children) == 0 {
			*out = append(*out, treeForm{form: strings.Join(next, " "), node: c})
			continue
		}
		childPath := c.Name
		if path != "" {
			childPath = path + " " + c.Name
		}
		collectNode(c, next, childPath, argGroups, out)
	}
}

// treeForms 枚举一棵树的全部可成句叶子形态。
func treeForms(root *schema.Node, argGroups map[string]bool) []treeForm {
	var out []treeForm
	for _, c := range root.Children {
		collectNode(c, []string{c.Name}, c.Name, argGroups, &out)
	}
	return out
}

// configPathVerbs 配置树里「携带语句路径」的动词 → 该动词按命令名粒度登记的形态。
// 路径是 ConfigPathTree 挂到这些动词上的同一份语法，不逐路径展开（见文件头口径）。
var configPathVerbs = map[string]string{
	"set":      "set <path>",
	"delete":   "delete <path>",
	"show":     "show", // 覆盖表 key 就是裸 show（配置模式读 candidate）
	"annotate": "annotate",
	"edit":     "edit",
	"run":      "run",
}

// configForms 配置树的可成句形态：路径族动词按动词粒度收一条，其余走通用走法。
func configForms() []treeForm {
	var out []treeForm
	for _, c := range schema.ConfigRoot().Children {
		if v, ok := configPathVerbs[c.Name]; ok {
			out = append(out, treeForm{form: v, node: c})
			continue
		}
		collectNode(c, []string{c.Name}, c.Name, nil, &out)
	}
	return out
}

// configStatementForms 配置语句路径树（ConfigPathTree）的全部叶子形态。
// 它们统一由 candidate API 架构性覆盖——本列表用于逐条断言这一点（非空断言）。
func configStatementForms() []treeForm {
	return treeForms(schema.ConfigPathTree(), nil)
}

// dedupForms 按形态串去重（同一形态可能从多条路径到达），返回排序后的列表。
func dedupForms(in []treeForm) []treeForm {
	byForm := map[string]treeForm{}
	for _, f := range in {
		if _, ok := byForm[f.form]; !ok {
			byForm[f.form] = f
		}
	}
	out := make([]treeForm, 0, len(byForm))
	for _, f := range byForm {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].form < out[j].form })
	return out
}

// 家底下限：低于它说明枚举器失效（OperRoot/ConfigRoot 变了），而非「真的没有形态」。
const (
	treeMinOperForms        = 120
	treeMinConfigStatements = 60
)

// TestCLITreeLeavesAreClassified 遍历命令树，逐条断言每片可成句叶子形态已被归类到
// cliRESTCoverage ∪ cliRESTExceptions ∪ cliRESTGaps 之一（或 set/delete 前缀规则）。
// 未归类即逐条点名失败——新增命令忘记归类、或树里长出无人收编的形态，都会在这里现形。
func TestCLITreeLeavesAreClassified(t *testing.T) {
	oper := dedupForms(treeForms(schema.OperRoot(), coverArgGroupParents))
	cfg := dedupForms(configForms())

	if len(oper) < treeMinOperForms {
		t.Fatalf("操作树只枚举到 %d 片形态（下限 %d）——枚举器可能已失效（OperRoot 变了？）", len(oper), treeMinOperForms)
	}

	var unclassified []treeForm
	check := func(f treeForm, cmd string) {
		if bucket, _ := classify(cmd); bucket == "" {
			unclassified = append(unclassified, treeForm{form: cmd, node: f.node})
		}
	}
	for _, f := range oper {
		check(f, f.form)
	}
	for _, f := range cfg {
		check(f, f.form)
	}
	for _, f := range unclassified {
		t.Errorf("命令树形态 %q（需要 class %s）未归类：请补入 cliRESTCoverage（有对应 REST 端点）/"+
			"cliRESTExceptions（REPL 交互或 CLI 侧渲染）/cliRESTGaps（真缺口）之一——"+
			"否则 Web 控制台的功能覆盖会悄悄出现空洞", f.form, f.node.RequiredClass())
	}

	// 配置语句语法：逐条断言整族由 candidate API 覆盖（非空断言，防止语句树被清空后静默全绿）。
	stats := dedupForms(configStatementForms())
	if len(stats) < treeMinConfigStatements {
		t.Fatalf("配置语句树只枚举到 %d 条语句（下限 %d）——枚举器可能已失效（ConfigPathTree 变了？）",
			len(stats), treeMinConfigStatements)
	}
	badStmt := 0
	for _, s := range stats {
		if bucket, _ := classify("set " + s.form); bucket != "A" {
			badStmt++
			t.Errorf("配置语句 %q 未落到 candidate API 覆盖（classify 得 %q）——配置语句整族应由 "+
				"PUT/DELETE /configuration/candidate + POST /configuration/commit 架构性覆盖", s.form, bucket)
		}
	}

	t.Logf("命令树遍历：操作树 %d 片形态 / 配置树 %d 片形态 / 配置语句 %d 条；本次未归类 %d 条",
		len(oper), len(cfg), len(stats), len(unclassified))
	if badStmt == 0 {
		t.Logf("配置语句 %d 条全部落 candidate API 覆盖", len(stats))
	}
}

// TestCLITreeCoverageClassifySelfCheck 枚举器自检：形态渲染与 cliFormMatch 口径一致
// （Param/Value 占位符可被 key 的 `<...>` 通配命中），并给出几片已知形态做锚点——
// 防止枚举器「静默全绿」。
func TestCLITreeCoverageClassifySelfCheck(t *testing.T) {
	oper := dedupForms(treeForms(schema.OperRoot(), coverArgGroupParents))
	forms := map[string]bool{}
	for _, f := range oper {
		forms[f.form] = true
	}
	// 锚点：这几片必须在枚举结果里（覆盖本次修复的漏网点与几类代表性形态）。
	for _, want := range []string{
		"show system metrics history",
		"show system metrics history name <metric>",
		"show interfaces <ifname> detail",
		"request container-functions <name> exec <command>",
		"ping ipv6 <host>",
		"wizard",
		"help <command>",
	} {
		if !forms[want] {
			t.Errorf("枚举器漏掉了应出现的形态 %q——形态枚举口径可能已漂移", want)
		}
	}
	// 占位符 ⇄ 通配：形态里的 `<host>` 应能被 key 里的 `<x>` 命中（同 token 数）。
	if n := cliFormMatch("ping ipv6 <host>", "ping ipv6 <x>"); n == 0 {
		t.Errorf("形态与 key 的占位符通配口径不一致：ping ipv6 <host> vs ping ipv6 <x> 未命中")
	}
	// 容器节点不成句：`show system` 本身不该作为命令形态出现。
	if forms["show system"] {
		t.Errorf("`show system` 是容器节点（其孩子是互斥子命令），不该被当成一条命令")
	}
}
