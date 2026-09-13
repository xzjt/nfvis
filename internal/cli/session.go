// Package cli 实现 NFViS JunOS 风格 CLI 前端（骨架 §2 internal/cli）。
//
// 薄客户端原则（骨架 §3.1）：本包只依赖 schema（补全）与 pkg/cliclient（执行），
// 不得 import internal/config 等业务包——依赖方向由 internal/archtest 守护。
// ?/Tab 补全依据本地 schema（编译期共享，连接断开时仍可编辑提示，§3.3），
// 动态候选实时向 nfvisd 查询、失败退化为占位提示（§5.3）。
package cli

import (
	"strings"

	"github.com/xzjt/nfvis/internal/schema"
	"github.com/xzjt/nfvis/pkg/cliclient"
)

// Version CLI 版本（与 api.VersionStr 同步发布）。
const Version = "1.0.0-dev"

// Backend 会话所需的客户端能力（pkg/cliclient.Client 实现）。
type Backend interface {
	Execute(line, source string) (cliclient.Result, error)
	DynamicCandidates(kind string) ([]string, error)
	Logout() error
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
	res, err := s.client.Execute(line, s.Source)
	if err != nil {
		return "%% " + err.Error() + "\n", s.Prompt()
	}
	s.Mode, s.Path = res.Mode, res.Path
	return res.Output, res.Prompt
}

// Logout 吊销服务端 token（空闲超时自动登出，FR-CLI-006）。
func (s *Session) Logout() {
	if err := s.client.Logout(); err != nil {
		// 已失效/断连时无需提示：本地会话随即结束。
		_ = err
	}
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

// CompleteLine 处理 Tab：唯一匹配补全，多匹配补到公共前缀（FR-CLI-003/§5.2）。
func (s *Session) CompleteLine(line string) string {
	tokens, partial := completionTokens(line)
	root := s.rootForContext(tokens)
	cs := schema.Candidates(root, tokens, partial, s.dynCandidates())
	if len(cs) == 0 {
		return line
	}
	if len(cs) == 1 {
		return strings.TrimSuffix(line, partial) + cs[0].Token + " "
	}
	if common := commonPrefix(cs); len(common) > len(partial) {
		return strings.TrimSuffix(line, partial) + common
	}
	return line
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
