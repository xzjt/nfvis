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
	// DialConsole 连接串口 WebSocket（M4-12，FR-CMP-014）；wsPath 来自 Result.Console。
	DialConsole(wsPath string) (io.ReadWriteCloser, error)
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
}

// New 构造会话（初始操作模式）。
func New(client Backend, source string) *Session {
	return &Session{client: client, Source: source, Mode: "oper"}
}

// ExecuteLine 执行一行命令，返回输出与更新后的提示符。
func (s *Session) ExecuteLine(line string) (string, string) {
	out, prompt, _ := s.ExecuteFull(line)
	return out, prompt
}

// ExecuteFull 执行一行命令并返回完整结果（含更新后的提示符与可能的串口接管请求）。
// 供 REPL 处理 `request … console`（M4-12，FR-CMP-014）与交互确认。
func (s *Session) ExecuteFull(line string) (string, string, *cliclient.ConsoleRequest) {
	res, err := s.client.Execute(line, s.Source)
	if err != nil {
		return "%% " + err.Error() + "\n", s.Prompt(), nil
	}
	s.Mode, s.Path = res.Mode, res.Path
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
// 服务端会话按 user@source 保留（与 token 生命周期无关），不做收尾会把
// 「配置模式 + candidate 锁」留给下一次登录——表现为下一次 `configure` 报
// `%% 无效命令`，且 `?` 候选与执行都按上一模式解释。`-c` 脚本与交互 REPL
// 的两条退出路径（EOF / 空闲超时）都必须调用（附录 A #82④）。
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

// DialConsole 连接串口 WebSocket（M4-12，FR-CMP-014）；wsPath 来自 ExecuteFull 的接管请求。
func (s *Session) DialConsole(wsPath string) (io.ReadWriteCloser, error) {
	return s.client.DialConsole(wsPath)
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
func (s *Session) Candidates(line string) []schema.Candidate {
	line = strings.TrimSuffix(line, "?")
	tokens, partial := completionTokens(line)
	root := s.rootForContext(tokens)
	return schema.Candidates(root, tokens, partial, s.dynCandidates())
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
func (s *Session) completionContext(line string) (base, partial string, cs []schema.Candidate) {
	tokens, partial := completionTokens(line)
	base = strings.TrimSuffix(strings.TrimRight(line, " \t"), partial)
	return base, partial, schema.Candidates(s.rootForContext(tokens), tokens, partial, s.dynCandidates())
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
