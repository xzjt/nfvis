package api

// CLI 命令执行器（api/cli_bridge 的守护进程侧，骨架 §2 internal/api/cli_bridge.go）。
//
// 语义：nfvis-cli 本地用 schema 包做 ?/Tab 补全（§3.3 编译期共享，不依赖守护进程
// 存活），命令行经 POST /cli/execute 转发到此执行——命令树与执行器必须同源
//（AGENTS.md 常见错误第 1 条）。
//
// set/delete 语句按 schema 树驱动对 candidate 做变更（语句→模型执行期翻译）：
// 关键字 → 对象/数组容器（连字符转下划线）；实例参数 → 具名数组元素
//（身份字段见 identityFields）；取值叶子 → 叶子赋值（类型不符由反序列化校验兜底）。
// 权限逐命令校验：schema 节点 RequiredClass × aaa.Authorize（FR-SEC-002）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/schema"
	"github.com/xzjt/nfvis/internal/state"
)

// authorizer 授权接口（aaa.Service 实现）。
type authorizer interface {
	Authorize(cfgClass string, required schema.Class, path ...string) bool
}

// identityFields 具名数组创建元素时的身份字段（与 model.Flatten 键控一致，默认 name）。
var identityFields = map[string]string{
	"ports":         "seq",
	"routes":        "prefix",
	"hugepages":     "page_size",
	"ntp":           "server",
	"numa":          "node",
	"rules":         "seq",
	"l3_interfaces": "interface",
	"per_dev":       "interface",
}

// CLIEResult 单条命令的执行结果。
type CLIEResult struct {
	Output string   `json:"output"`
	Mode   string   `json:"mode"`
	Path   []string `json:"path"`
	Prompt string   `json:"prompt"`
}

// cliExecutor 守护进程侧 CLI 执行器。会话（模式/层级）按持有者+接入源隔离。
type cliExecutor struct {
	engine *config.Engine
	authz  authorizer
	diag   DiagRuntime  // 诊断命令（M3-9；nil = 报不可用）
	state  *state.State // 接口计数快照（monitor；nil = 报不可用）
	mu     sync.Mutex
	sess   map[string]*cliSession
	// structured 当前命令的结构化输出快照（display json/xml 用；单命令执行期内有效）
	structured any
}

type cliSession struct {
	Mode string
	Path []string
}

func newCLIExecutor(e *config.Engine, a authorizer) *cliExecutor {
	return &cliExecutor{engine: e, authz: a, sess: map[string]*cliSession{}}
}

// setRuntime 注入诊断与运行态数据源（M3-9；Server.New 装配，测试可省略）。
func (x *cliExecutor) setRuntime(diag DiagRuntime, st *state.State) {
	x.diag, x.state = diag, st
}

func promptOf(s *cliSession) string {
	if s.Mode == "config" {
		if len(s.Path) == 0 {
			return "nfvis# "
		}
		return "[edit " + strings.Join(s.Path, " ") + "] nfvis# "
	}
	return "nfvis> "
}

// Execute 执行一行命令。
func (x *cliExecutor) Execute(user, class, source, line string) CLIEResult {
	x.mu.Lock()
	defer x.mu.Unlock()
	key := user + "@" + source
	s := x.sess[key]
	if s == nil {
		s = &cliSession{Mode: "oper"}
		x.sess[key] = s
	}

	cmd, pipes, perr := splitPipes(strings.TrimSpace(line))
	x.structured = nil
	var out string
	if perr != nil {
		out = "%% " + perr.Error() + "\n"
	} else if canon, cerr := x.canonicalize(s, cmd); cerr != nil {
		out = "%% " + cerr.Error() + "\n" // FR-CLI-004：歧义/未知命令在此报错并列出候选
	} else {
		out = x.dispatch(user, class, source, s, canon, cmd)
		out = x.applyPipes(out, pipes)
	}
	cur := x.sess[key]
	if cur == nil {
		cur = &cliSession{Mode: "oper"}
	}
	return CLIEResult{Output: out, Mode: cur.Mode, Path: append([]string{}, cur.Path...), Prompt: promptOf(cur)}
}

// canonicalize 按当前模式/层级把命令 token 规整为规范关键字（FR-CLI-004：
// 无歧义前缀即可执行，与 Tab 补全同源）。set/delete/edit/show 的路径相对当前
// edit 层级解析；annotate 的注释文本与 load/save 文件名原样保留。
func (x *cliExecutor) canonicalize(s *cliSession, cmd string) ([]string, error) {
	toks := strings.Fields(cmd)
	if len(toks) == 0 {
		return nil, nil
	}
	if s.Mode != "config" {
		return schema.Canonicalize(schema.OperRoot(), toks)
	}
	head, err := schema.Canonicalize(schema.ConfigRoot(), toks[:1])
	if err != nil {
		return nil, err
	}
	switch head[0] {
	case "annotate":
		return append(head, toks[1:]...), nil // 注释文本含空格，仅规整命令字
	case "set", "delete", "edit", "show":
		// 配置模式 `show configuration ...` 委托操作模式查看 committed；
		// configuration 只在操作树建模，故先在操作树解析该前缀。
		if head[0] == "show" && len(toks) > 1 {
			if two, err := schema.Canonicalize(schema.OperRoot(), toks[:2]); err == nil &&
				len(two) == 2 && two[1] == "configuration" {
				rest, err := schema.Canonicalize(schema.OperRoot(), toks[2:])
				if err != nil {
					return nil, err
				}
				return append(two, rest...), nil
			}
		}
		base, _, err := schema.Match(schema.ConfigPathTree(), s.Path) // 层级可含身份取值
		if err != nil {
			return nil, err
		}
		rest, err := schema.Canonicalize(base, toks[1:])
		if err != nil {
			return nil, err
		}
		return append(head, rest...), nil
	default:
		return schema.Canonicalize(schema.ConfigRoot(), toks)
	}
}

func (x *cliExecutor) dispatch(user, class, source string, s *cliSession, t []string, raw string) string {
	if len(t) == 0 {
		return ""
	}
	if s.Mode == "oper" {
		return x.execOper(user, class, source, s, t)
	}
	return x.execConfig(user, class, source, s, t, raw)
}

func (x *cliExecutor) allow(class string, n *schema.Node, path ...string) bool {
	return x.authz.Authorize(class, n.RequiredClass(), path...)
}

// ---------- 操作模式 ----------

func (x *cliExecutor) execOper(user, class, source string, s *cliSession, t []string) string {
	switch t[0] {
	case "configure":
		if !x.allow(class, mustNode(schema.OperRoot(), "configure"), "configure") {
			return "%% 无权限进入配置模式（需 super-user）\n"
		}
		// FR-CFG-001：进入配置模式即取得 candidate（副本），被占用时报错
		if err := x.engine.Edit(config.Session{User: user, Source: source}); err != nil {
			return "%% " + err.Error() + "\n"
		}
		s.Mode = "config"
		s.Path = nil
		return ""
	case "exit", "quit":
		delete(x.sess, user+"@"+source)
		return ""
	case "show":
		return x.execOperShow(class, t[1:])
	case "ping":
		return x.execPing(class, t[1:])
	case "traceroute":
		return x.execTraceroute(class, t[1:])
	case "monitor":
		return x.execMonitor(class, t[1:])
	case "clear":
		return x.execClear(class, t[1:])
	case "request", "start", "help":
		if _, _, err := schema.Match(schema.OperRoot(), t); err != nil {
			return fmt.Sprintf("%% 无效命令: %s（输入 ? 查看可用命令）\n", strings.Join(t, " "))
		}
		return "%% 该命令依赖底座运行态，将在 M3/M4 接入后可用\n"
	}
	return fmt.Sprintf("%% 无效命令: %s（输入 ? 查看可用命令）\n", strings.Join(t, " "))
}

func (x *cliExecutor) execOperShow(class string, t []string) string {
	if !x.allow(class, mustNode(schema.OperRoot(), "show"), append([]string{"show"}, t...)...) {
		return "%% 无权限执行 show\n"
	}
	switch {
	case len(t) == 1 && t[0] == "version":
		return "NFViS " + VersionStr + "（M2：配置事务可用，网络底座 M3+ 接入）\n"
	case len(t) >= 1 && t[0] == "configuration":
		if len(t) >= 2 && t[1] == "compare" {
			// show configuration compare rollback <n>
			if len(t) < 4 || t[2] != "rollback" {
				return "%% 语法: show configuration compare rollback <n>\n"
			}
			n, err := strconv.Atoi(t[3])
			if err != nil {
				return "%% rollback 编号须为整数\n"
			}
			diff, err := x.engine.Compare(n)
			if err != nil {
				return "%% " + err.Error() + "\n"
			}
			if diff == "" {
				return "（无差异）\n"
			}
			return diff + "\n"
		}
		cfg, err := x.engine.Committed()
		if err != nil {
			return "%% 读取配置失败: " + err.Error() + "\n"
		}
		if len(t) >= 2 && t[1] == "candidate" {
			cand, _, err := x.engine.Candidate()
			if err != nil {
				return "%% 无活跃 candidate 会话\n"
			}
			cfg = cand
		}
		tree := toJSONTree(cfg)
		x.structured = tree
		out := RenderConfigJSON(tree)
		if out == "" {
			return "（配置为空）\n"
		}
		return out + "\n"
	case len(t) >= 1 && t[0] == "acls":
		return x.execShowAcls(t[1:])
	case len(t) >= 1 && t[0] == "bonds":
		return x.execShowBonds(t[1:])
	case len(t) >= 3 && t[0] == "system" && t[1] == "configuration" && t[2] == "sessions":
		views, err := x.engine.Sessions()
		if err != nil {
			return "%% 查询失败: " + err.Error() + "\n"
		}
		if len(views) == 0 {
			return "（无持锁会话）\n"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Holder       Acquired            Last-Activity       Dirty\n")
		for _, v := range views {
			fmt.Fprintf(&b, "%-12s %-19s %-19s %v\n", v.Holder,
				v.AcquiredAt.Format("2006-01-02 15:04"), v.LastActivity.Format("2006-01-02 15:04"), v.Dirty)
		}
		return b.String()
	}
	return "%% 该 show 命令依赖底座运行态，M3/M4 接入后可用（当前可用：show version / show configuration [candidate|compare rollback n] / show system configuration sessions）\n"
}

// ---------- 配置模式 ----------

func (x *cliExecutor) execConfig(user, class, source string, s *cliSession, t []string, raw string) string {
	switch t[0] {
	case "annotate":
		return x.cfgAnnotate(user, source, raw)
	case "load":
		return x.cfgLoad(user, source, t[1:])
	case "save":
		return x.cfgSave(user, source, t[1:])
	case "set", "delete":
		return x.execSetDelete(user, source, s, t[0], t[1:])
	case "show":
		if len(t) > 1 && t[1] == "configuration" {
			// 配置模式下查看 committed（委托操作模式 show configuration）
			return x.execOperShow(class, t[1:])
		}
		return x.cfgShow(user, source, s, t[1:])
	case "commit":
		return x.cfgCommit(user, source, s, t[1:])
	case "rollback":
		return x.cfgRollback(user, source, t[1:])
	case "discard":
		if err := x.engine.Discard(config.Session{User: user, Source: source}); err != nil {
			return "%% " + err.Error() + "\n"
		}
		return "candidate 已丢弃，会话锁已释放\n"
	case "edit":
		if len(t) < 2 {
			return "%% 语法: edit <path>\n"
		}
		if _, _, err := schema.Match(schema.ConfigPathTree(), append(append([]string{}, s.Path...), t[1:]...)); err != nil {
			return "%% " + err.Error() + "\n"
		}
		s.Path = append(s.Path, t[1:]...)
		return ""
	case "up":
		if len(s.Path) > 0 {
			s.Path = s.Path[:len(s.Path)-1]
		}
		return ""
	case "top":
		s.Path = nil
		return ""
	case "exit":
		if _, dirty, err := x.engine.Candidate(); err == nil && dirty {
			return "%% 存在未提交变更，先 commit 或 discard\n"
		}
		_ = x.engine.Release(config.Session{User: user, Source: source})
		s.Mode = "oper"
		s.Path = nil
		return ""
	case "run":
		if len(t) < 2 {
			return "%% 语法: run <操作模式命令>\n"
		}
		return x.execOper(user, class, source, s, t[1:])
	}
	return fmt.Sprintf("%% 无效命令: %s（输入 ? 查看可用命令）\n", strings.Join(t, " "))
}

func (x *cliExecutor) execSetDelete(user, source string, s *cliSession, op string, stmt []string) string {
	full := append(append([]string{}, s.Path...), stmt...)
	if len(full) == 0 {
		return fmt.Sprintf("%% 语法: %s <path> [value]\n", op)
	}
	if _, _, err := schema.Match(schema.ConfigPathTree(), full); err != nil {
		return "%% " + err.Error() + "\n"
	}
	sess := config.Session{User: user, Source: source}
	if err := x.engine.Edit(sess); err != nil {
		return "%% " + err.Error() + "\n"
	}
	cfg, _, err := x.engine.Candidate()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if op == "set" {
		if err := applyStatement(&cfg, full); err != nil {
			return "%% " + err.Error() + "\n"
		}
	} else {
		if err := deleteStatement(&cfg, full); err != nil {
			return "%% " + err.Error() + "\n"
		}
	}
	if err := x.engine.UpdateCandidate(sess, cfg); err != nil {
		return "%% " + err.Error() + "\n"
	}
	if op == "set" {
		return "[ok] " + strings.Join(full, " ") + "\n"
	}
	return "已删除 " + strings.Join(full, " ") + "（未提交）\n"
}

func (x *cliExecutor) cfgShow(user, source string, s *cliSession, args []string) string {
	cfg, _, err := x.engine.Candidate()
	if err != nil {
		return "%% 无活跃 candidate 会话（先 set 或 configure）\n"
	}
	// FR-CFG-009：非持有者会话只读——展示 committed 而非持有者的 candidate
	views, _ := x.engine.Sessions()
	if len(views) == 0 || views[0].Holder != user+"@"+source {
		cfg, _ = x.engine.Committed()
	}
	tree := toJSONTree(cfg)
	sub, err := navigateJSON(tree, append(append([]string{}, s.Path...), args...))
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	x.structured = sub
	subMap, ok := sub.(map[string]any)
	if !ok {
		return scalarStringOf(sub) + "\n"
	}
	out := RenderConfigJSON(subMap)
	if out == "" {
		return "（无匹配配置）\n"
	}
	return out + "\n"
}

func (x *cliExecutor) cfgCommit(user, source string, s *cliSession, args []string) string {
	opts := config.CommitOpts{}
	switch {
	case len(args) > 0 && args[0] == "check":
		errs, err := x.engine.CommitCheck(config.Session{User: user, Source: source})
		if err != nil {
			return "%% " + err.Error() + "\n"
		}
		if len(errs) > 0 {
			return "校验失败（未提交）:\n" + formatVErrors(errs) + "\n"
		}
		return "校验通过\n"
	case len(args) > 0 && args[0] == "confirmed":
		if len(args) >= 2 {
			minutes, err := strconv.Atoi(args[1])
			if err != nil {
				return "%% confirmed 分钟数须为整数\n"
			}
			opts.ConfirmedMinutes = minutes
		} else {
			opts.ConfirmedMinutes = 10 // FR-CFG-003 缺省 10 分钟
		}
	case len(args) > 0 && args[0] == "and-quit":
		res := x.cfgCommit(user, source, s, nil)
		s.Mode = "oper"
		s.Path = nil
		return res
	}
	res, err := x.engine.Commit(context.Background(), config.Session{User: user, Source: source}, opts)
	if err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			return "校验失败（candidate 保留）:\n" + formatVErrors(ve.Errors) + "\n"
		}
		return "%% " + err.Error() + "\n"
	}
	out := fmt.Sprintf("commit 成功 (revision %d)", res.Revision)
	if res.ConfirmedUntil != nil {
		out += fmt.Sprintf("，confirmed 模式：%s 前再次 commit 确认，否则自动回滚", res.ConfirmedUntil.Format("15:04:05"))
	}
	for _, w := range res.Warnings {
		out += "\n" + w
	}
	return out + "\n"
}

func (x *cliExecutor) cfgRollback(user, source string, args []string) string {
	n := 1
	if len(args) > 0 {
		v, err := strconv.Atoi(args[0])
		if err != nil {
			return "%% rollback 编号须为整数\n"
		}
		n = v
	}
	sess := config.Session{User: user, Source: source}
	if err := x.engine.Edit(sess); err != nil { // 未持锁时先进入编辑态
		return "%% " + err.Error() + "\n"
	}
	if err := x.engine.Rollback(sess, n); err != nil {
		return "%% " + err.Error() + "\n"
	}
	return fmt.Sprintf("candidate 已替换为快照 %d，需 commit 生效\n", n)
}

// ---------- 语句 → 配置模型（JSON 树变更） ----------

// applyStatement 按 schema 树驱动把 set 语句写入配置（语句→模型执行期翻译）。
// 先查语句别名表（CLI 嵌套与模型扁平不一致的语句），再走通用树遍历；
// Diff 兜底：语句必须真实落到模型（未映射语句会报错而非静默丢失）。
func applyStatement(cfg *model.Config, tokens []string) error {
	if rule := matchAlias(tokens); rule != nil {
		before := *cfg
		tree := toJSONTree(*cfg)
		if err := rule.apply(tree, tokens, true); err != nil {
			return err
		}
		return commitTree(cfg, tree, before, tokens)
	}
	before := *cfg
	tree := toJSONTree(*cfg)
	if err := applyTokens(cfgPathRoot(), tree, tokens, true); err != nil {
		return err
	}
	return commitTree(cfg, tree, before, tokens)
}

// deleteStatement 按 schema 树驱动删除语句/子树。
func deleteStatement(cfg *model.Config, tokens []string) error {
	if rule := matchAlias(tokens); rule != nil {
		before := *cfg
		tree := toJSONTree(*cfg)
		if err := rule.apply(tree, tokens, false); err != nil {
			return err
		}
		return commitTree(cfg, tree, before, tokens)
	}
	before := *cfg
	tree := toJSONTree(*cfg)
	if err := applyTokens(cfgPathRoot(), tree, tokens, false); err != nil {
		return err
	}
	return commitTree(cfg, tree, before, tokens)
}

// commitTree JSON 树 → 强类型配置，并要求语句确实产生了变更。
func commitTree(cfg *model.Config, tree map[string]any, before model.Config, tokens []string) error {
	if err := fromJSONTree(tree, cfg); err != nil {
		return err
	}
	if model.Diff(before, *cfg) == "" {
		return fmt.Errorf("语句未产生配置变更（尚未映射到模型或值未变化）: %s", strings.Join(tokens, " "))
	}
	return nil
}

// ---------- 语句别名表（CLI 嵌套 ⇄ 模型扁平不一致的映射） ----------

// aliasRule 一条别名：pattern 中 "*" 匹配任意单 token；apply 对 JSON 树
// 直接落模型字段（set=true 赋值 / set=false 清除）。
type aliasRule struct {
	pattern []string
	apply   func(tree map[string]any, t []string, isSet bool) error
}

// matchAlias 在全部别名规则中按顺序匹配：先基础表（cliexec.go），再网络语句表
// （cli_aliases_net.go）。pattern 末位可用 "**" 表示匹配剩余全部 token
// （用于「一个关键字后跟不定长键值对」的语句，如 acls rule / nat rules）。
func matchAlias(tokens []string) *aliasRule {
	for _, rule := range allAliasRules() {
		if patternMatches(rule.pattern, tokens) {
			return rule
		}
	}
	return nil
}

// allAliasRules 汇总别名规则（顺序即匹配优先级）。
func allAliasRules() []*aliasRule {
	out := make([]*aliasRule, 0, len(statementAliases)+len(statementAliasesNet))
	for i := range statementAliases {
		out = append(out, &statementAliases[i])
	}
	for i := range statementAliasesNet {
		out = append(out, &statementAliasesNet[i])
	}
	return out
}

// patternMatches 判断 pattern 与 tokens 是否匹配；"**" 只允许出现在末位。
func patternMatches(p, tokens []string) bool {
	if n := len(p); n > 0 && p[n-1] == "**" {
		if len(tokens) < n-1 {
			return false
		}
		for j := 0; j < n-1; j++ {
			if p[j] != "*" && p[j] != tokens[j] {
				return false
			}
		}
		return true
	}
	if len(p) != len(tokens) {
		return false
	}
	for j, seg := range p {
		if seg != "*" && seg != tokens[j] {
			return false
		}
	}
	return true
}

// elemByID 在具名数组 tree[arrKey] 中按身份值取元素（不存在报错）。
func elemByID(tree map[string]any, arrKey, ident string) (map[string]any, error) {
	arr, _ := tree[arrKey].([]any)
	fld := identityFields[arrKey]
	if fld == "" {
		fld = "name"
	}
	em, _ := selectElement(arr, fld, ident)
	if em == nil {
		return nil, fmt.Errorf("无匹配配置: %s", ident)
	}
	return em, nil
}

func numField(s string) (any, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil, fmt.Errorf("取值 %q 须为整数", s)
	}
	return float64(n), nil
}

var statementAliases = []aliasRule{
	// set virtual-switches <n> vlan access <vlan> → VSwitch.vlan_access
	{pattern: []string{"virtual-switches", "*", "vlan", "access", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			vs, err := elemByID(tree, "virtual_switches", t[1])
			if err != nil {
				return err
			}
			if !isSet {
				delete(vs, "vlan_access")
				return nil
			}
			v, err := numField(t[4])
			if err != nil {
				return err
			}
			vs["vlan_access"] = v
			return nil
		}},
	// set virtual-machine-functions <n> memory numa node <u> → memory.numa_node
	{pattern: []string{"virtual-machine-functions", "*", "memory", "numa", "node", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			vm, err := elemByID(tree, "virtual_machine_functions", t[1])
			if err != nil {
				return err
			}
			mem, _ := vm["memory"].(map[string]any)
			if mem == nil {
				if !isSet {
					return fmt.Errorf("无匹配配置: memory")
				}
				mem = map[string]any{}
				vm["memory"] = mem
			}
			if !isSet {
				delete(mem, "numa_node")
				return nil
			}
			v, err := numField(t[5])
			if err != nil {
				return err
			}
			mem["numa_node"] = v
			return nil
		}},
	// delete virtual-switches <n> vlan access（4 token 删除形态）
	{pattern: []string{"virtual-switches", "*", "vlan", "access"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			vs, err := elemByID(tree, "virtual_switches", t[1])
			if err != nil {
				return err
			}
			delete(vs, "vlan_access")
			return nil
		}},
	// delete virtual-machine-functions <n> memory numa node（5 token 删除形态）
	{pattern: []string{"virtual-machine-functions", "*", "memory", "numa", "node"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			vm, err := elemByID(tree, "virtual_machine_functions", t[1])
			if err != nil {
				return err
			}
			if mem, ok := vm["memory"].(map[string]any); ok {
				delete(mem, "numa_node")
			}
			return nil
		}},
	// set virtual-machine-functions <n> serial console enable → serial_console=true
	{pattern: []string{"virtual-machine-functions", "*", "serial", "console", "enable"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			vm, err := elemByID(tree, "virtual_machine_functions", t[1])
			if err != nil {
				return err
			}
			if !isSet {
				vm["serial_console"] = false
				return nil
			}
			vm["serial_console"] = true
			return nil
		}},
	// set vpp dpdk dev <ifname> [rx-queues|tx-queues|rx-descriptors|tx-descriptors <n>]
	// per-NIC 覆盖：模型字段是 per_dev 数组（决策 #18），与 CLI 的 dev 层级名不一致
	{pattern: []string{"vpp", "dpdk", "dev", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return dpdkPerDev(tree, t[3], "", nil, isSet)
		}},
	{pattern: []string{"vpp", "dpdk", "dev", "*", "*", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			n, err := numField(t[5])
			if err != nil {
				return err
			}
			return dpdkPerDev(tree, t[3], strings.ReplaceAll(t[4], "-", "_"), n, isSet)
		}},
	// set virtual-switches <n> ports <seq> interface <if> [trunk vlans <list>|native <vlan>]
	{pattern: []string{"virtual-switches", "*", "ports", "*", "interface", "*", "trunk", "vlans", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return portTrunkApply(tree, t, isSet, "interface", 5)
		}},
	{pattern: []string{"virtual-switches", "*", "ports", "*", "interface", "*", "native", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return portNativeApply(tree, t, isSet, 5)
		}},
	// set virtual-switches <n> ports <seq> vnf <vm> interface <vnic> [trunk vlans <list>]
	{pattern: []string{"virtual-switches", "*", "ports", "*", "vnf", "*", "interface", "*", "trunk", "vlans", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return portTrunkApply(tree, t, isSet, "vnf", 7)
		}},
}

// portTrunkApply 端口成员 + trunk VLAN 列表（memberKind/interfaceIdx 按语句形态）。
func portTrunkApply(tree map[string]any, t []string, isSet bool, memberKind string, ifIdx int) error {
	port, err := portElem(tree, t[1], t[3])
	if err != nil {
		return err
	}
	if !isSet {
		delete(port, "trunk")
		return nil
	}
	if memberKind == "interface" {
		port["interface"] = t[ifIdx]
	} else {
		port["vnf"] = t[5]
		port["vnf_interface"] = t[7]
	}
	vlans, err := expandVlanList(t[len(t)-1])
	if err != nil {
		return err
	}
	port["trunk"] = vlans
	return nil
}

// portNativeApply 端口 native VLAN。
func portNativeApply(tree map[string]any, t []string, isSet bool, ifIdx int) error {
	port, err := portElem(tree, t[1], t[3])
	if err != nil {
		return err
	}
	if !isSet {
		delete(port, "native")
		return nil
	}
	port["interface"] = t[ifIdx]
	v, err := numField(t[len(t)-1])
	if err != nil {
		return err
	}
	port["native"] = v
	return nil
}

// portElem 取（或创建）交换机的指定序号端口元素。
func portElem(tree map[string]any, vsName, seq string) (map[string]any, error) {
	vs, err := elemByID(tree, "virtual_switches", vsName)
	if err != nil {
		return nil, err
	}
	arr, _ := vs["ports"].([]any)
	em, _ := selectElement(arr, "seq", seq)
	if em == nil {
		n, err := strconv.Atoi(seq)
		if err != nil {
			return nil, fmt.Errorf("端口序号 %q 须为整数", seq)
		}
		em = map[string]any{"seq": float64(n)}
		arr = append(arr, em)
		vs["ports"] = arr
	}
	return em, nil
}

// expandVlanList "100,200" → [100, 200]（JSON number，反序列化为 []int）。
func expandVlanList(s string) ([]any, error) {
	var out []any
	for _, part := range strings.Split(s, ",") {
		v, err := numField(strings.TrimSpace(part))
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func cfgPathRoot() *schema.Node { return schema.ConfigPathTree() }

func toJSONTree(c model.Config) map[string]any {
	b, _ := json.Marshal(&c)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// fromJSONTree JSON 树 → 强类型配置（类型不符即报错，语句被拒绝）。
func fromJSONTree(m map[string]any, c *model.Config) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	var out model.Config
	if err := json.Unmarshal(b, &out); err != nil {
		return fmt.Errorf("取值类型不符: %v", err)
	}
	*c = out
	return nil
}

func validateTreeJSON(tree map[string]any) error {
	var probe model.Config
	b, _ := json.Marshal(tree)
	if err := json.Unmarshal(b, &probe); err != nil {
		return fmt.Errorf("取值类型不符: %v", err)
	}
	return nil
}

// applyTokens 沿 schema 树消费 tokens，在 JSON 树上赋值（isSet）或删除。
//
// 状态机：关键字下钻区分三种容器——具名数组（有参数子节点，数组挂在其 JSON 键下）、
// 值叶子关键字（取值挂起 pendingKey，下一 token 即值）、对象容器（惰性建 map）。
// 取值后节点停留在值关键字/参数节点上（后续兄弟关键字是其子节点）。
// flag 语句（无值叶子）：当前仅 disable（映射 enabled=false，§2.3），其余经
// applyStatement 的 Diff 兜底报「未映射」。
func applyTokens(root *schema.Node, tree map[string]any, tokens []string, isSet bool) error {
	node := root
	cur := tree
	pendingArrKey := ""         // 关键字下挂具名数组，待参数 token 选择元素
	pendingKey := ""            // 值叶子关键字，待下一 token 赋值
	pendingIdentity := false    // 身份取值模式，待下一 token 选/建数组元素
	var pendingIVK *schema.Node // 身份取值关键字节点（如 page-size）
	pendingIVKSkipped := false  // 身份关键字 token 是否已被透明跳过

	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]

		// 身份取值：本 token 选/建具名数组的元素
		if pendingIdentity {
			if !pendingIVKSkipped {
				if tok != pendingIVK.Name {
					return fmt.Errorf("配置不完整: %s 后缺少 %s 的取值", pendingIVK.Name, pendingIVK.Name)
				}
				pendingIVKSkipped = true
				continue // 透明跳过身份关键字 token（如 page-size）
			}
			arr, _ := cur[pendingArrKey].([]any)
			ident := identityFields[pendingArrKey]
			if ident == "" {
				ident = "name"
			}
			elem, idx := selectElement(arr, ident, tok)
			if !isSet {
				if elem == nil {
					return fmt.Errorf("无匹配配置: %s", tok)
				}
				if i == len(tokens)-1 {
					cur[pendingArrKey] = append(arr[:idx], arr[idx+1:]...)
					return validateTreeJSON(tree)
				}
			}
			if elem == nil {
				elem = map[string]any{ident: typedScalar(tok)}
				arr = append(arr, elem)
				cur[pendingArrKey] = arr
			}
			cur = elem
			node = pendingIVK // 元素内子语句（如 count）是身份关键字的子节点
			pendingIdentity = false
			pendingArrKey = ""
			continue
		}

		// 取值挂起：本 token 即前一键头关键字的值
		if pendingKey != "" {
			if fn, ok := valueTransforms[pendingKey]; ok {
				v, err := fn(tok)
				if err != nil {
					return err
				}
				cur[pendingKey] = v
			} else {
				cur[pendingKey] = scalarForNode(node, tok)
			}
			pendingKey = ""
			// node 停留在值关键字上：后续兄弟关键字（如 page-size 下的 count）是其子节点
			if i == len(tokens)-1 {
				return validateTreeJSON(tree) // 语句结束
			}
			continue
		}

		// 1) 关键字匹配
		var child *schema.Node
		for _, c := range node.Children {
			if c.Kind == schema.Keyword && c.Name == tok {
				child = c
				break
			}
		}
		if child != nil {
			k := jsonKeyOf(child)
			// flag：disable 特例映射 enabled=false（§2.3）；其余 flag 走 Diff 兜底报错
			if !isSet && i == len(tokens)-1 && tok == "disable" {
				cur["enabled"] = false
				return validateTreeJSON(tree)
			}
			if isSet && i == len(tokens)-1 && tok == "disable" {
				cur["enabled"] = false
				return validateTreeJSON(tree)
			}
			if !isSet && i == len(tokens)-1 {
				if _, ok := cur[k]; !ok {
					return fmt.Errorf("无匹配配置: %s", tok)
				}
				delete(cur, k)
				return validateTreeJSON(tree)
			}
			if len(child.Children) > 0 && child.Children[0].IdentityValue {
				// 身份取值数组容器（如 hugepages）：首个子节点是身份取值关键字，
				// 其取值即元素身份；数组挂在本关键字的 JSON 键下
				if _, ok := cur[k]; !ok {
					if !isSet {
						return fmt.Errorf("无匹配配置: %s", tok)
					}
					cur[k] = []any{}
				}
				pendingArrKey = k
				pendingIVK = child.Children[0]
				pendingIVKSkipped = false
				pendingIdentity = true
				node = child
				continue
			}
			if fp := firstParamOf(child); fp != nil && fp.ScalarParam {
				// 标量参数关键字：透明层，取值由参数分支写入父容器
				node = child
				continue
			}
			switch {
			case firstParamOf(child) != nil: // 具名数组容器
				if _, ok := cur[k]; !ok {
					if !isSet {
						return fmt.Errorf("无匹配配置: %s", tok)
					}
					cur[k] = []any{}
				}
				pendingArrKey = k
			case singleValueOf(child) != nil: // 值叶子关键字
				pendingKey = k
			default: // 对象容器
				sub, ok := cur[k].(map[string]any)
				if !ok {
					if !isSet {
						return fmt.Errorf("无匹配配置: %s", tok)
					}
					sub = map[string]any{}
					cur[k] = sub
				}
				cur = sub
			}
			node = child
			continue
		}

		// 2a) 标量参数：取值写入父容器的标量字段（成员标量数组则追加）
		if p := firstParamOf(node); p != nil && p.ScalarParam {
			v := typedScalar(tok)
			if arr, ok := cur[p.ScalarJSONKey].([]any); ok {
				cur[p.ScalarJSONKey] = append(arr, v)
			} else {
				cur[p.ScalarJSONKey] = v
			}
			if i == len(tokens)-1 {
				return validateTreeJSON(tree)
			}
			node = p // 后续兄弟关键字（如 vnf 下的 interface）是参数节点的子节点
			continue
		}

		// 2b) 实例参数：在 pendingArrKey 数组中按身份选/建/删元素
		if p := firstParamOf(node); p != nil && pendingArrKey != "" {
			arr, _ := cur[pendingArrKey].([]any)
			ident := identityFields[pendingArrKey]
			if ident == "" {
				ident = "name"
			}
			elem, idx := selectElement(arr, ident, tok)
			if !isSet {
				if elem == nil {
					return fmt.Errorf("无匹配配置: %s", tok)
				}
				if i == len(tokens)-1 {
					cur[pendingArrKey] = append(arr[:idx], arr[idx+1:]...)
					return validateTreeJSON(tree)
				}
			}
			if elem == nil {
				elem = map[string]any{ident: typedScalar(tok)}
				arr = append(arr, elem)
				cur[pendingArrKey] = arr
			}
			cur = elem
			node = p
			pendingArrKey = ""
			continue
		}

		return fmt.Errorf("未知语句: %q", tok)
	}

	if isSet {
		return fmt.Errorf("配置不完整，缺少取值: %s", strings.Join(tokens, " "))
	}
	return fmt.Errorf("无匹配配置: %s", strings.Join(tokens, " "))
}

func jsonKeyOf(n *schema.Node) string { return strings.ReplaceAll(n.Name, "-", "_") }

func firstParamOf(n *schema.Node) *schema.Node {
	for _, c := range n.Children {
		if c.Kind == schema.Param {
			return c
		}
	}
	return nil
}

func singleValueOf(n *schema.Node) *schema.Node {
	for _, c := range n.Children {
		if c.Kind == schema.Value {
			return c
		}
	}
	return nil
}

// selectElement 在具名数组中按身份值选元素（标量序列化比对，容忍数字/字符串差异）。
func selectElement(arr []any, ident, value string) (map[string]any, int) {
	for i, e := range arr {
		if em, ok := e.(map[string]any); ok {
			if v, ok := em[ident]; ok && scalarEq(v, value) {
				return em, i
			}
		}
	}
	return nil, -1
}

func scalarEq(v any, s string) bool {
	switch x := v.(type) {
	case string:
		return x == s
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64) == s
	case bool:
		return strconv.FormatBool(x) == s
	}
	return false
}

// typedScalar 取值类型推断：true/false → bool；整数 → number；其余 → string。
// valueTransforms 特定 JSON 键的取值变换（核列表 "4-7" → 展开的 int 数组）。
var valueTransforms = map[string]func(string) (any, error){
	"isolated_cores": expandCores,
	"cores":          expandCores,
}

func expandCores(s string) (any, error) {
	var out []any
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, err1 := strconv.Atoi(strings.TrimSpace(lo))
			b, err2 := strconv.Atoi(strings.TrimSpace(hi))
			if err1 != nil || err2 != nil || a > b {
				return nil, fmt.Errorf("核区间 %q 不合法", part)
			}
			for x := a; x <= b; x++ {
				out = append(out, float64(x))
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("核编号 %q 不合法", part)
		}
		out = append(out, float64(n))
	}
	return out, nil
}

func typedScalar(tok string) any {
	switch tok {
	case "true":
		return true
	case "false":
		return false
	}
	if n, err := strconv.ParseInt(tok, 10, 64); err == nil {
		return float64(n)
	}
	return tok
}

// scalarForNode 依 schema 取值节点类型决定 token 的 JSON 形态：数值/布尔类型按
// 字面解析，其余类型（string/core-list/size/ip 等）即使形似数字也保持字符串——
// 否则 `set vpp cpu corelist-workers 5` 会把字符串字段写成数字（FR-SYS-008）。
func scalarForNode(n *schema.Node, tok string) any {
	v := typedScalar(tok)
	if n == nil {
		return v
	}
	if n.Kind == schema.Keyword { // 取值关键字：类型在其值子节点上
		if sv := singleValueOf(n); sv != nil {
			n = sv
		}
	}
	switch n.ParamType {
	case "", "uint", "int", "number", "bool":
		return v
	}
	if _, isNum := v.(float64); isNum {
		return tok
	}
	return v
}

// navigateJSON 按 CLI 路径 token 定位 JSON 子树（show <path>）。
func navigateJSON(tree map[string]any, path []string) (any, error) {
	cur := any(tree)
	lastKey := "" // 下钻进数组时记录其 JSON 键（元素身份字段据此选取）
	for _, tok := range path {
		switch c := cur.(type) {
		case map[string]any:
			m := c
			if v, ok := m[strings.ReplaceAll(tok, "-", "_")]; ok {
				cur = v
				lastKey = strings.ReplaceAll(tok, "-", "_")
				continue
			}
			found := false
			for k, v := range m {
				arr, isArr := v.([]any)
				if !isArr {
					continue
				}
				ident := identityFields[k]
				if ident == "" {
					ident = "name"
				}
				if elem, _ := selectElement(arr, ident, tok); elem != nil {
					cur = elem
					lastKey = k
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("无匹配配置: %s", tok)
			}
		case []any:
			ident := identityFields[lastKey]
			if ident == "" {
				ident = "name"
			}
			elem, _ := selectElement(c, ident, tok)
			if elem == nil {
				return nil, fmt.Errorf("无匹配配置: %s", tok)
			}
			cur = elem
		default:
			return nil, fmt.Errorf("路径 %q 无下层配置", tok)
		}
	}
	return cur, nil
}

// ---------- JunOS 风格渲染 ----------

// RenderConfigJSON 把配置 JSON 树渲染为 JunOS 风格层级文本。
func RenderConfigJSON(m map[string]any) string {
	var b strings.Builder
	renderMap(&b, m, 0)
	renderAnnotations(&b, m)
	return strings.TrimRight(b.String(), "\n")
}

func renderMap(b *strings.Builder, m map[string]any, depth int) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pad := strings.Repeat("    ", depth)
	for _, k := range keys {
		renderValue(b, strings.ReplaceAll(k, "_", "-"), m[k], depth, pad)
	}
}

func renderValue(b *strings.Builder, display string, v any, depth int, pad string) {
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 {
			fmt.Fprintf(b, "%s%s;\n", pad, display)
			return
		}
		fmt.Fprintf(b, "%s%s {\n", pad, display)
		renderMap(b, x, depth+1)
		fmt.Fprintf(b, "%s}\n", pad)
	case []any:
		if len(x) == 0 {
			return
		}
		allScalar := true
		for _, e := range x {
			switch e.(type) {
			case map[string]any, []any:
				allScalar = false
			}
		}
		if allScalar {
			vals := make([]string, 0, len(x))
			for _, e := range x {
				vals = append(vals, scalarStringOf(e))
			}
			fmt.Fprintf(b, "%s%s [ %s ];\n", pad, display, strings.Join(vals, " "))
			return
		}
		for _, e := range x {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if ident := identityName(em); ident != "" {
				fmt.Fprintf(b, "%s%s %s {\n", pad, display, ident)
			} else {
				fmt.Fprintf(b, "%s%s {\n", pad, display)
			}
			renderMap(b, em, depth+1)
			fmt.Fprintf(b, "%s}\n", pad)
		}
	default:
		if display == "password-hash" {
			// FR-SEC-007：口令哈希在 show 输出中脱敏
			fmt.Fprintf(b, "%s%s «已隐藏»;\n", pad, display)
			return
		}
		fmt.Fprintf(b, "%s%s %s;\n", pad, display, scalarStringOf(v))
	}
}

// identityName 具名数组元素的展示身份。
func identityName(em map[string]any) string {
	for _, k := range []string{"name", "interface", "prefix", "seq", "node", "server", "page_size"} {
		if v, ok := em[k]; ok {
			return scalarStringOf(v)
		}
	}
	return ""
}

func scalarStringOf(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprintf("%v", v)
}

// formatVErrors 校验错误列表的 CLI 展示。
func formatVErrors(verrs []model.ValidateError) string {
	var b strings.Builder
	for _, ve := range verrs {
		b.WriteString("  - " + ve.Error() + "\n")
	}
	return b.String()
}

func mustNode(root *schema.Node, names ...string) *schema.Node {
	n, err := schema.Find(root, names...)
	if err != nil {
		return root
	}
	return n
}

// dpdkPerDev 维护 vpp.dpdk.per_dev 数组（per-NIC 覆盖）。
func dpdkPerDev(tree map[string]any, ifname, key string, val any, isSet bool) error {
	vpp, _ := tree["vpp"].(map[string]any)
	if vpp == nil {
		if !isSet {
			return fmt.Errorf("无匹配配置: vpp")
		}
		vpp = map[string]any{}
		tree["vpp"] = vpp
	}
	dpdk, _ := vpp["dpdk"].(map[string]any)
	if dpdk == nil {
		if !isSet {
			return fmt.Errorf("无匹配配置: vpp dpdk")
		}
		dpdk = map[string]any{}
		vpp["dpdk"] = dpdk
	}
	arr, _ := dpdk["per_dev"].([]any)
	elem, idx := selectElement(arr, "interface", ifname)
	if !isSet {
		if elem == nil {
			return fmt.Errorf("无匹配配置: vpp dpdk dev %s", ifname)
		}
		if key == "" {
			dpdk["per_dev"] = append(arr[:idx], arr[idx+1:]...)
			return nil
		}
		delete(elem, key)
		return nil
	}
	if elem == nil {
		elem = map[string]any{"interface": ifname}
		arr = append(arr, elem)
		dpdk["per_dev"] = arr
	}
	if key != "" {
		elem[key] = val
	}
	return nil
}
