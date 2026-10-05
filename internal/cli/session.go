// Package cli 实现 NFViS JunOS 风格 CLI 前端（骨架 §2 internal/cli）。
//
// 薄客户端原则（骨架 §3.1）：本包只依赖 schema（补全）与 pkg/cliclient（执行），
// 不得 import internal/config 等业务包——依赖方向由 internal/archtest 守护。
// ?/Tab 补全依据本地 schema（编译期共享，连接断开时仍可编辑提示，§3.3），
// 动态候选实时向 nfvisd 查询、失败退化为占位提示（§5.3）。
package cli

import (
	"io"
	"strings"

	"github.com/xzjt/nfvis/internal/schema"
	"github.com/xzjt/nfvis/pkg/cliclient"
)

// Version CLI 版本（与 api.VersionStr 同步发布）。
//
// 用 var 而非 const，以便打包时经 ldflags 注入发布版本
// （`make deb VERSION=x.y.z` 同时注入本变量与 api.VersionStr）——
// 否则发布的 deb 里 `nfvis-cli -version` 会显示 1.0.0-dev 而 `nfvisd` 显示正式版本，两者不一致。
var Version = "1.0.0-dev"

// Backend 会话所需的客户端能力（pkg/cliclient.Client 实现）。
type Backend interface {
	Execute(line, source string) (cliclient.Result, error)
	DynamicCandidates(kind string) ([]string, error)
	Logout() error
	// DialConsole 连接会话 WebSocket（M4-12，FR-CMP-014）；wsPath 来自 Result.Console，
	// what 为会话显示名（「串口」/「容器终端」，决策 #375/R142 B10——同一函数被两类会话共用）。
	DialConsole(wsPath, what string) (io.ReadWriteCloser, error)
	// MetricsText 拉取 /api/v1/metrics 原始文本（setup 向导读主机事实，决策 #107）。
	MetricsText() (string, error)
}

// Session CLI 会话：本地维护模式/层级（渲染提示符与补全上下文），
// 执行与动态候选经 Backend 转发 nfvisd。
type Session struct {
	client Backend
	Source string // ssh | console（影响 FR-CFG-012 自锁判定）
	Mode   string // oper | config
	Path   []string
	// class 本会话的服务端权威 login class（决策 #324；由 cmd/nfvis-cli 从登录响应注入）。
	// `?`/Tab 候选据此过滤：本会话无权执行的入口不列出，避免「列了但一用就 403」。
	// 空串/未知（含自定义 class）时**不过滤**——薄客户端拿不到路径 ACL，执行路径仍由
	// 服务端拒绝（纵深防御保留）；这条边界如实登记在决策行。
	class string
}

// New 构造会话（初始操作模式）。
func New(client Backend, source string) *Session {
	return &Session{client: client, Source: source, Mode: "oper"}
}

// SetClass 注入服务端权威的本会话 login class（决策 #324）。
func (s *Session) SetClass(class string) { s.class = class }

// candidateFilter 按本会话 class 过滤 `?`/Tab 候选的谓词（决策 #324）。
// 判定与运行期授权**同源**：预置档的等级映射取自 schema.PresetClassLevel（internal/aaa
// 的预置档判定同一张表），候选按命令树节点的 RequiredClass() 判等级；非预置 class
// （自定义，纯路径 ACL）返回 nil = 不过滤（边界，见 Session.class 注释）。
func (s *Session) candidateFilter() schema.CandidateFilter {
	lvl, ok := schema.PresetClassLevel(s.class)
	if !ok {
		return nil
	}
	return func(_ []string, n *schema.Node) bool { return lvl.Covers(n.RequiredClass()) }
}

// Execute 执行一行命令并返回**完整结果**（含服务端的结构化提示标记 Warning），
// 同时更新本地模式/层级（提示符与 ?/Tab 补全据此渲染）。
//
// 脚本模式（`-c`）用它区分「提示」与「失败」：`Warning` 为真表示服务端明确回了
// 非失败提示（当前为「语句未产生配置变更」），应继续执行后续语句——不去猜输出
// 文本前缀（`%%` 是错误前缀，提示行不带它；而语句文本自身可能含 `%`）。
func (s *Session) Execute(line string) cliclient.Result {
	res, err := s.client.Execute(line, s.Source)
	if err != nil {
		// 传输层失败（连不上/超时）与 ExecuteLine 同口径：`%%` + 本地提示符。
		return cliclient.Result{Output: "%% " + err.Error() + "\n", Prompt: s.Prompt()}
	}
	s.Mode, s.Path = res.Mode, res.Path
	return res
}

// ExecuteLine 执行一行命令，返回输出与更新后的提示符。
func (s *Session) ExecuteLine(line string) (string, string) {
	res := s.Execute(line)
	return res.Output, res.Prompt
}

// ExecuteFull 执行一行命令并返回完整结果（含更新后的提示符与可能的串口接管请求）。
// 供 REPL 处理 `request … console`（M4-12，FR-CMP-014）与交互确认。
func (s *Session) ExecuteFull(line string) (string, string, *cliclient.ConsoleRequest) {
	res := s.Execute(line)
	return res.Output, res.Prompt, res.Console
}

// Logout 吊销服务端 token（空闲超时自动登出，FR-CLI-006）。
func (s *Session) Logout() {
	if err := s.client.Logout(); err != nil {
		// 已失效/断连时无需提示：本地会话随即结束。
		_ = err
	}
}

// Teardown 退出前收尾：丢弃 candidate → 退出配置模式 → 吊销 token；返回各步输出。
//
// 服务端配置会话按**会话标识**保留（决策 #317：身份键 + token 稳定 ID，与 token 生命周期
// 绑定的语义见决策 #301），不做收尾会把「配置模式 + candidate 锁」留给下一次登录——表现为
// 下一次 `configure` 报 `%% 无效命令`，且 `?` 候选与执行都按上一模式解释。`-c` 脚本与交互
// REPL 的两条退出路径（EOF / 空闲超时）都必须调用（附录 A #82④）。
func (s *Session) Teardown() []string {
	var outs []string
	if s.Mode == "config" {
		// discard 释放 candidate 与会话锁（无变更时也安全）；随后 exit 才能离开配置模式
		//（存在未提交变更时 exit 会拒绝，故顺序不可颠倒）。
		if out, _ := s.ExecuteLine("discard"); out != "" {
			outs = append(outs, out)
		}
		if out, _ := s.ExecuteLine("exit"); out != "" {
			outs = append(outs, out)
		}
	}
	s.Logout()
	return outs
}

// DialConsole 连接会话 WebSocket（M4-12，FR-CMP-014）；wsPath 来自 ExecuteFull 的接管请求，
// what 为会话显示名（决策 #375/R142 B10）。
func (s *Session) DialConsole(wsPath, what string) (io.ReadWriteCloser, error) {
	return s.client.DialConsole(wsPath, what)
}

// MetricsText 透传主机指标原始文本（setup 向导的事实源，决策 #107）。
func (s *Session) MetricsText() (string, error) {
	return s.client.MetricsText()
}

// Prompt 渲染当前提示符（oper: nfvis>；config: [edit path] nfvis#）。
func (s *Session) Prompt() string {
	if s.Mode == "config" {
		if len(s.Path) == 0 {
			return "nfvis# "
		}
		return "[edit " + strings.Join(s.Path, " ") + "] nfvis# "
	}
	return "nfvis> "
}

// Candidates 返回当前位置（line 已含正在输入的前缀）的补全候选。
// 以 "？" 结尾的行视为 ? 查询；动态候选查询失败退化为占位提示（§5.3）。
// 行内含未引用 `|` 时切到管道段补全（§5 第 9 条，决策 #155 补充三）。
func (s *Session) Candidates(line string) []schema.Candidate {
	line = strings.TrimSuffix(line, "?")
	_, _, cs := s.completionContext(line)
	return cs
}

// Complete 处理 Tab：返回补全后的行与该位置候选（FR-CLI-003/§5.2）。
// 以去掉尾随空白后的文本为基准追加，避免把分隔用的空格/Tab 带进补全结果；
// 候选一并返回，供调用方在多匹配无进展时「响铃并列出」。
func (s *Session) Complete(line string) (string, []schema.Candidate) {
	line = strings.TrimRight(line, "\t") // Tab 是触发键，不进入补全文本
	base, partial, cs := s.completionContext(line)
	switch {
	case len(cs) == 0:
		return line, nil
	case len(cs) == 1:
		return base + cs[0].Token + " ", cs
	}
	if common := commonPrefix(cs); len(common) > len(partial) {
		return base + common, cs
	}
	return line, cs
}

// CompleteLine 仅取补全后的行（非 raw 退化路径与既有调用方用）。
func (s *Session) CompleteLine(line string) string {
	nl, _ := s.Complete(line)
	return nl
}

// completionContext 解析补全上下文：已完成 token、正在输入的前缀、候选列表。
// 行内含未引用 `|` 时补全上下文切到**管道段**（§5 第 9 条）：候选来自
// schema.PipeCandidates，base 保留 `|` 及其前的全部文本。
func (s *Session) completionContext(line string) (base, partial string, cs []schema.Candidate) {
	if seg, ok := pipeSegment(line); ok {
		tokens, partial := completionTokens(seg)
		base = strings.TrimSuffix(strings.TrimRight(line, " \t"), partial)
		if strings.HasSuffix(base, "|") {
			base += " " // `|match` 与 `| match` 都合法，补全统一带空格更好读
		}
		return base, partial, schema.PipeCandidates(tokens, partial)
	}
	tokens, partial := completionTokens(line)
	base = strings.TrimSuffix(strings.TrimRight(line, " \t"), partial)
	return base, partial, schema.CandidatesFiltered(s.rootForContext(tokens), tokens, partial,
		s.dynCandidates(), s.candidateFilter())
}

// pipeSegment 返回行内最后一个未引用 `|` 之后的片段与是否存在。
// 引用语义与守护进程 splitPipes 一致：双引号内的 `|` 不是管道分隔。
func pipeSegment(line string) (string, bool) {
	inQuote, last := false, -1
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inQuote = !inQuote
		case '|':
			if !inQuote {
				last = i
			}
		}
	}
	if last < 0 {
		return "", false
	}
	return line[last+1:], true
}

// ---------- 内部 ----------

// completionTokens 解析补全上下文：已完成 token 与正在输入的前缀。
// 尾随空格表示正开新 token（前缀为空，§5.1）。
func completionTokens(line string) ([]string, string) {
	trailingSpace := len(line) > 0 && (line[len(line)-1] == ' ' || line[len(line)-1] == '	')
	tokens := strings.Fields(line)
	switch {
	case trailingSpace:
		return tokens, ""
	case len(tokens) == 0:
		return nil, ""
	default:
		return tokens[:len(tokens)-1], tokens[len(tokens)-1]
	}
}

// rootForContext 依据首 token 与模式选择补全根：
// set/delete/edit 挂配置语句树；run 挂操作树；config 模式其余命令亦为配置树。
func (s *Session) rootForContext(tokens []string) *schema.Node {
	if len(tokens) > 0 {
		switch tokens[0] {
		case "set", "delete", "edit":
			return schema.ConfigRoot()
		case "run":
			return schema.OperRoot()
		}
	}
	if s.Mode == "config" {
		return schema.ConfigRoot()
	}
	return schema.OperRoot()
}

// dynCandidates 动态候选：实时向 nfvisd 查询，失败退化为 nil（§5.3）。
func (s *Session) dynCandidates() func(string) []string {
	return func(kind string) []string {
		toks, err := s.client.DynamicCandidates(kind)
		if err != nil {
			return nil
		}
		return toks
	}
}

// commonPrefix 候选公共前缀（Tab 多匹配时补全到公共前缀）。
func commonPrefix(cs []schema.Candidate) string {
	if len(cs) == 0 {
		return ""
	}
	p := cs[0].Token
	for _, c := range cs[1:] {
		for !strings.HasPrefix(c.Token, p) {
			p = p[:len(p)-1]
			if p == "" {
				return ""
			}
		}
	}
	return p
}
