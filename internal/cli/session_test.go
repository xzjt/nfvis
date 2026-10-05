package cli

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/cliparse"
	"github.com/xzjt/nfvis/internal/schema"
	"github.com/xzjt/nfvis/pkg/cliclient"
)

// ---------- W7：internal/cli 独立单测（补全/前缀匹配/提示符） ----------

// stubClient 补全单测不需要真实连接（动态候选查询失败退化为 nil，§5.3）。
type stubClient struct{}

func (stubClient) Execute(line, source string) (cliclient.Result, error) {
	return cliclient.Result{Mode: "oper", Prompt: "nfvis> "}, nil
}

// DynamicCandidates 假的动态候选来源：接口名一族自决策 #83 起按语义分 kind
// （show interfaces physical 等处用 vpp-ifnames）。
func (stubClient) DynamicCandidates(kind string) ([]string, error) {
	switch kind {
	case "vpp-ifnames", "ifnames":
		return []string{"ens2f0", "ens2f1"}, nil
	}
	return nil, nil
}

func (stubClient) Logout() error { return nil }

// DialConsole 补全/会话单测不涉及串口（M4-12）；返回明确错误以免误用。
func (stubClient) DialConsole(wsPath, what string) (io.ReadWriteCloser, error) {
	return nil, errors.New("stub 不支持 console")
}

// MetricsText 补全/会话单测不涉及向导事实（setup_test 用自己的 fake）。
func (stubClient) MetricsText() (string, error) { return "", nil }

func newTestSession(mode string) *Session {
	s := New(stubClient{}, "ssh")
	s.Mode = mode
	return s
}

func TestPromptRendering(t *testing.T) {
	s := newTestSession("oper")
	if got := s.Prompt(); got != "nfvis> " {
		t.Fatalf("oper 提示符: %q", got)
	}
	s.Mode = "config"
	if got := s.Prompt(); got != "nfvis# " {
		t.Fatalf("config 顶层提示符: %q", got)
	}
	s.Path = []string{"system", "interfaces"}
	if got := s.Prompt(); got != "[edit system interfaces] nfvis# " {
		t.Fatalf("层级提示符: %q", got)
	}
}

func TestCompletionTokens(t *testing.T) {
	tokens, partial := completionTokens("show interfaces ens")
	if len(tokens) != 2 || tokens[0] != "show" || partial != "ens" {
		t.Fatalf("补全上下文解析: %v %q", tokens, partial)
	}
	tokens, partial = completionTokens("show ")
	if len(tokens) != 1 || partial != "" {
		t.Fatalf("尾随空格应为新 token: %v %q", tokens, partial)
	}
}

func TestRootForContext(t *testing.T) {
	s := newTestSession("oper")
	cs := schema.Candidates(s.rootForContext([]string{"set"}), []string{"set"}, "vir", nil)
	found := 0
	for _, c := range cs {
		if c.Token == "virtual-switches" || c.Token == "virtual-machine-functions" {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("set 上下文应挂配置树（找到 %d 个 virtual-* 候选）", found)
	}
	// oper 模式非 set 上下文 → 操作树
	cs = schema.Candidates(s.rootForContext(nil), nil, "conf", nil)
	if len(cs) != 1 || cs[0].Token != "configure" {
		t.Fatalf("oper 补全上下文应为操作树: %v", cs)
	}
}

func TestCompleteLineUniqueAndCommonPrefix(t *testing.T) {
	s := newTestSession("oper")
	// 唯一匹配：补全并附空格
	if got := s.CompleteLine("conf"); got != "configure " {
		t.Fatalf("唯一匹配应补全: %q", got)
	}
	// 多匹配：补到公共前缀
	if got := s.CompleteLine("show vir"); got != "show virtual-" {
		t.Fatalf("多匹配应补公共前缀: %q", got)
	}
	// 已完整输入的公共前缀：不变
	if got := s.CompleteLine("show virtual-"); got != "show virtual-" {
		t.Fatalf("无进展应保持: %q", got)
	}
}

func TestDynamicCandidatesStub(t *testing.T) {
	s := newTestSession("oper")
	cs := s.Candidates("show interfaces physical e")
	found := false
	for _, c := range cs {
		if c.Token == "ens2f0" {
			found = true
		}
	}
	if !found {
		t.Fatalf("动态候选应含 stub 返回的接口名: %+v", cs)
	}
}

func TestNoBusinessImports(t *testing.T) {
	// 薄客户端守护的包级复核（archtest 已有 go list 版本）：源码文本级断言
	// 本包不得出现业务包 import。
	for _, bad := range []string{"internal/config", "internal/api", "internal/orchestrator", "internal/aaa"} {
		if strings.Contains(sessionSrc, bad) {
			t.Fatalf("internal/cli 不得 import %s（骨架 §3.1）", bad)
		}
	}
}

var sessionSrc = `
import (
	"github.com/xzjt/nfvis/internal/schema"
	"github.com/xzjt/nfvis/pkg/cliclient"
)
`

func TestCompleteLineTrailingTab(t *testing.T) {
	// W2 raw 编辑器在 Tab 处保留分隔符；补全结果不得把 Tab 带进命令
	s := newTestSession("oper")
	if got := s.CompleteLine("conf\t"); got != "configure " {
		t.Fatalf("尾随 Tab 应正常补全: %q", got)
	}
	if got := s.CompleteLine("show vir\t"); got != "show virtual-" {
		t.Fatalf("尾随 Tab 多匹配应补公共前缀: %q", got)
	}
}

// TestPipePositionCompletion（决策 #155 补充三，FR-CLI-002）：未引用 `|` 之后
// 的补全上下文切到管道段——段首列管道关键字、display 补出取值；双引号内的 `|`
// 不视为管道分隔；无管道的行回归命令树补全。
func TestPipePositionCompletion(t *testing.T) {
	s := newTestSession("oper")

	cs := s.Candidates("show configuration | ")
	if len(cs) == 0 {
		t.Fatalf("管道段首应列出管道关键字")
	}
	tokens := map[string]bool{}
	for _, c := range cs {
		tokens[c.Token] = true
		if c.Desc == "" {
			t.Fatalf("管道关键字候选应带描述: %+v", c)
		}
	}
	for _, want := range []string{"match", "except", "count", "last", "begin", "display", "compare"} {
		if !tokens[want] {
			t.Fatalf("管道段首候选应含 %q: %v", want, tokens)
		}
	}

	// Tab：唯一前缀补全到关键字并附空格（base 保留 | 之前全部文本）
	nl, cs := s.Complete("show configuration | disp")
	if len(cs) != 1 || cs[0].Token != "display" {
		t.Fatalf("disp 应唯一补出 display: %v", cs)
	}
	if nl != "show configuration | display " {
		t.Fatalf("Tab 补全结果不对: %q", nl)
	}

	// display 的取值位：json/xml/set
	cs = s.Candidates("show configuration | display ")
	got := map[string]bool{}
	for _, c := range cs {
		got[c.Token] = true
	}
	for _, want := range []string{"json", "xml", "set"} {
		if !got[want] {
			t.Fatalf("display 取值位应含 %q: %v", want, got)
		}
	}

	// 双引号内的 | 不是管道分隔：仍走命令树补全（description 取值位无候选 → 空）
	if cs := s.Candidates("set interfaces ens192 description \"a|b\" "); len(cs) != 0 {
		t.Fatalf("引号内的 | 不得当管道分隔（该位置本就无候选）: %v", cs)
	}

	// 无管道的行回归命令树补全
	cs = s.Candidates("show ")
	found := false
	for _, c := range cs {
		if c.Token == "interfaces" || c.Token == "version" {
			found = true
		}
	}
	if !found {
		t.Fatalf("无管道行应回归命令树补全: %v", cs)
	}
}

// TestCompletionLexerSingleSource（决策 #377/E8）：补全词法与执行路径同源（internal/cliparse）——
// 引号内空白/`|` 不切分、`\"` 转义被识别；补全上下文与执行分词逐字一致。此前
// completionTokens 用 strings.Fields（无视引号）、pipeSegment 自实现且不认转义，二者与执行语义漂移。
func TestCompletionLexerSingleSource(t *testing.T) {
	s := newTestSession("config")

	// 引号内空白不切分：partial 为引号内整段，base 保留开引号
	line := `set system hostname "a b`
	base, partial, _ := s.completionContext(line)
	if partial != "a b" {
		t.Fatalf("引号内空白不应切分，partial=%q", partial)
	}
	if base != `set system hostname "` {
		t.Fatalf("base 应保留开引号: %q", base)
	}

	// `\"` 转义被识别：partial 为解码后的 a" b，base 仍保留开引号
	// （旧 strings.Fields 会切成 `a\` 与 `b`，上下文漂移）
	line = `set system hostname "a\" b`
	base, partial, _ = s.completionContext(line)
	if partial != `a" b` {
		t.Fatalf(`转义 \" 应解码，partial=%q`, partial)
	}
	if base != `set system hostname "` {
		t.Fatalf(`转义场景 base=%q`, base)
	}

	// 与执行分词逐字一致：completionTokens 拼回的 token 序列 == cliparse.SplitFields
	for _, l := range []string{
		`set system hostname "a b`,
		`set interfaces ens192 description "a\" | b"`,
		`show configuration | display set`,
	} {
		toks, part := completionTokens(l)
		got := append(append([]string{}, toks...), part)
		want := cliparse.SplitFields(l)
		if len(got) != len(want) {
			t.Fatalf("分词数量与 cliparse 不一致 %q: %v vs %v", l, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("分词与 cliparse 不一致 %q: %v vs %v", l, got, want)
			}
		}
	}

	// 转义引号让 `|` 保持在引号内：不被当管道分隔（旧 pipeSegment 不认 `\"`，会误判）
	pipeLine := `set interfaces ens192 description "a\" | b"`
	if _, ok := pipeSegment(pipeLine); ok {
		t.Fatalf("转义引号后的 | 仍在引号内，不应视为管道分隔")
	}
	// 对照：真正的管道仍被识别（段含前导空白，与原实现一致）
	if seg, ok := pipeSegment("show configuration | display"); !ok || seg != " display" {
		t.Fatalf("未引用 | 应识别为管道: %q %v", seg, ok)
	}
}

// ---------- 决策 #324：`?`/Tab 候选按服务端权威 class 过滤 ----------

func hasCandidate(cs []schema.Candidate, tok string) bool {
	for _, c := range cs {
		if c.Token == tok {
			return true
		}
	}
	return false
}

func TestCandidatesFilteredByClass(t *testing.T) {
	// read-only：只见只读入口（show/help/exit），request/configure/ping 整族不可见。
	ro := newTestSession("oper")
	ro.SetClass("read-only")
	top := ro.Candidates("")
	if !hasCandidate(top, "show") || !hasCandidate(top, "help") {
		t.Fatalf("read-only 顶层候选应含 show/help: %v", top)
	}
	for _, absent := range []string{"request", "configure", "ping", "traceroute", "monitor", "wizard", "clear", "start"} {
		if hasCandidate(top, absent) {
			t.Errorf("read-only 顶层候选不应含 %q（只读账号只能 show）: %v", absent, top)
		}
	}

	// operator：request/ping 可见，configure/clear/start 不可见；request system 下 super-user 子域不列。
	op := newTestSession("oper")
	op.SetClass("operator")
	top = op.Candidates("")
	if !hasCandidate(top, "request") || !hasCandidate(top, "ping") {
		t.Fatalf("operator 顶层候选应含 request/ping: %v", top)
	}
	for _, absent := range []string{"configure", "clear", "start"} {
		if hasCandidate(top, absent) {
			t.Errorf("operator 顶层候选不应含 super-user-only 的 %q: %v", absent, top)
		}
	}
	sys := op.Candidates("request system ")
	for _, absent := range []string{"reboot", "shutdown", "poweroff", "zeroize", "software", "kernel", "configuration", "ssh", "storage"} {
		if hasCandidate(sys, absent) {
			t.Errorf("operator 的 `request system ?` 不应列出 %q: %v", absent, sys)
		}
	}
	for _, present := range []string{"tech-support", "ntp", "api", "password"} {
		if !hasCandidate(sys, present) {
			t.Errorf("operator 的 `request system ?` 应保留 %q: %v", present, sys)
		}
	}

	// super-user：不变（configure 可见、request system reboot 可见）。
	su := newTestSession("oper")
	su.SetClass("super-user")
	if top = su.Candidates(""); !hasCandidate(top, "configure") {
		t.Fatalf("super-user 顶层候选应含 configure: %v", top)
	}
	if sys = su.Candidates("request system "); !hasCandidate(sys, "reboot") {
		t.Fatalf("super-user 的 `request system ?` 应含 reboot: %v", sys)
	}

	// 自定义 class（非预置）：薄客户端不过滤（边界，见 Session.class 注释）。
	custom := newTestSession("oper")
	custom.SetClass("my-custom-class")
	if top = custom.Candidates(""); !hasCandidate(top, "configure") {
		t.Fatalf("自定义 class 本地不过滤（fail-open 边界）: %v", top)
	}
}
