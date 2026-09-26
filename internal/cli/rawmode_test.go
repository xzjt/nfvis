package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/schema"
)

// ---------- 决策 #81：raw 模式按键行为（Tab 多匹配列出 / `?` 按键即时） ----------

// syncBuffer 并发安全缓冲：ReadLine 在 goroutine 中写出、测试在主 goroutine 读。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newRawTestEditor 构造 raw 态编辑器（stdin 用管道；无 TTY 时 MakeRaw 会失败，
// 故直接置 raw，专测按键分发）。输出走与生产一致的 raw 感知流（crlfWriter），
// 落到可断言的缓冲而不污染测试 stdout。
func newRawTestEditor(t *testing.T, fn Completer) (*Editor, *syncBuffer, *os.File) {
	t.Helper()
	r, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pw.Close(); r.Close() })
	out := &syncBuffer{}
	e := &Editor{
		in:       r,
		out:      &crlfWriter{w: out, raw: func() bool { return true }},
		history:  NewHistory(),
		idle:     NewIdleGuard(0, nil),
		complete: fn,
		raw:      true,
	}
	return e, out, pw
}

// fixedCompleter 模拟补全回调：行文本不变（无进展），候选由测试给定。
func fixedCompleter(line string, cs []schema.Candidate) Completer {
	return func(string) (string, []schema.Candidate) { return line, cs }
}

// FR-CLI-003 / 契约 §5.2「多匹配→响铃并列出」——此前只响铃，列候选从未实现。
func TestTabListsCandidatesOnMultiMatch(t *testing.T) {
	cs := []schema.Candidate{
		{Token: "virtual-machine-functions", Desc: "VM VNF"},
		{Token: "virtual-switches", Desc: "虚拟交换机"},
	}
	e, out, w := newRawTestEditor(t, fixedCompleter("show ", cs))
	go func() {
		_, _ = w.WriteString("show \t\r")
	}()
	line, err := e.ReadLine("nfvis> ")
	if err != nil {
		t.Fatalf("ReadLine 不应报错: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "\a") {
		t.Fatalf("多匹配应响铃: %q", got)
	}
	for _, c := range cs {
		if !strings.Contains(got, "\r\n  "+c.Token) {
			t.Fatalf("多匹配应列出候选 %s（各条自成一行）: %q", c.Token, got)
		}
	}
	if line != "show " {
		t.Fatalf("多匹配无进展时行文本不应被改写: %q", line)
	}
}

// 无候选时只响铃（不列出空列表）。
func TestTabRingsWhenNoCandidates(t *testing.T) {
	e, out, w := newRawTestEditor(t, fixedCompleter("zzz", nil))
	go func() { _, _ = w.WriteString("zzz\t\r") }()
	line, err := e.ReadLine("nfvis> ")
	if err != nil {
		t.Fatalf("ReadLine 不应报错: %v", err)
	}
	if line != "zzz" {
		t.Fatalf("行文本不应改变: %q", line)
	}
	if !strings.Contains(out.String(), "\a") {
		t.Fatalf("无候选应响铃: %q", out.String())
	}
}

// 唯一匹配/补到公共前缀时只改写行文本，不列候选。
func TestTabCompletesWithoutListingWhenProgress(t *testing.T) {
	e, out, w := newRawTestEditor(t, func(string) (string, []schema.Candidate) {
		return "configure ", []schema.Candidate{{Token: "configure", Desc: "进入配置模式"}}
	})
	go func() { _, _ = w.WriteString("conf\t\r") }()
	line, err := e.ReadLine("nfvis> ")
	if err != nil {
		t.Fatalf("ReadLine 不应报错: %v", err)
	}
	if line != "configure " {
		t.Fatalf("唯一匹配应补全行文本: %q", line)
	}
	if strings.Contains(out.String(), "\a") {
		t.Fatalf("唯一匹配不应响铃: %q", out.String())
	}
	if strings.Contains(out.String(), "进入配置模式") {
		t.Fatalf("唯一匹配不应列出候选: %q", out.String())
	}
}

// FR-CLI-002 / 契约 §5.1「任意位置输入 ?：列出…并回显已输入部分」。
// 关键：**只按键、不回车**候选就必须出现（修复前须等 Enter 提交后才打印）。
func TestQuestionMarkListsCandidatesImmediately(t *testing.T) {
	cs := []schema.Candidate{
		{Token: "version", Desc: "版本汇总"},
		{Token: "virtual-switches", Desc: "虚拟交换机"},
	}
	e, out, w := newRawTestEditor(t, fixedCompleter("show ver", cs))
	done := make(chan string, 1)
	go func() {
		line, _ := e.ReadLine("nfvis> ")
		done <- line
	}()

	if _, err := w.WriteString("show ver?"); err != nil { // 注意：不回车
		t.Fatal(err)
	}
	// 等**全部**候选都落进输出再断言：只等第一条会在 CI 负载下读到
	// "第一条已写、第二条还没写"的中间态（2026-09-22 round42 的 CI 实测抓到一次）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := out.String()
		all := true
		for _, c := range cs {
			if !strings.Contains(got, c.Token) {
				all = false
				break
			}
		}
		if all {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := out.String()
	if !strings.Contains(got, "\r\n") {
		t.Fatalf("列候选前应先换行（否则首条候选粘在提示符行上）: %q", got)
	}
	for _, c := range cs {
		if !strings.Contains(got, "\r\n  "+c.Token) {
			t.Fatalf("`?` 应在按键时立即列出候选（无需回车），且各条自成一行: %q", got)
		}
	}
	if !strings.Contains(got, "nfvis> show ver") {
		t.Fatalf("契约 §5.1 要求列出候选后回显已输入部分: %q", got)
	}

	// `?` 不进入行文本：随后的回车提交的是 "show ver"。
	if _, err := w.WriteString("\r"); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-done:
		if line != "show ver" {
			t.Fatalf("`?` 不应进入行文本: %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("回车后 ReadLine 未返回")
	}
}

// `?` 在无候选处只响铃（不静默、不列出空列表）。
func TestQuestionMarkRingsWhenNoCandidates(t *testing.T) {
	e, out, _ := newRawTestEditor(t, fixedCompleter("zzz", nil))
	e.line = []rune("zzz")
	e.helpLine("nfvis> ")
	if !strings.Contains(out.String(), "\a") {
		t.Fatalf("无候选应响铃: %q", out.String())
	}
}

// REPL 输出必须与行编辑器共用同一流，raw 期间才由 crlfWriter 负责换行（决策 #81①）。
func TestREPLOutGoesThroughEditorWriter(t *testing.T) {
	rp := NewREPL(New(stubClient{}, "ssh"), NewHistory(), NewIdleGuard(0, nil))
	if rp.out != rp.editor.Out() {
		t.Fatalf("REPL 输出应经 editor 的 raw 感知流（否则命令输出会阶梯错位）")
	}
}

// TestEditorMultibyteInput（决策 #157，round82 真机走查发现）：raw 模式逐字节到达的
// UTF-8 序列必须按字符（rune）插入——此前逐字节当 rune 追加，中文等输入被双重编码存坏
// （真机实测 description "中文" 落库为 ä¸­æ）。畸形序列（孤儿续字节/非法首字节）丢弃
// 且不影响后续 ASCII 输入。
func TestEditorMultibyteInput(t *testing.T) {
	var e Editor
	e.out = io.Discard
	for _, b := range []byte("中ab") {
		if r, ok := e.accum.feed(b); ok {
			e.insertRune(r, "")
		}
	}
	if string(e.line) != "中ab" {
		t.Fatalf("多字节输入应按字符插入: %q", string(e.line))
	}
	// 行中插入（cursor < len）：把 é 插到「中|ab」的中后面
	e.cursor = 1
	for _, b := range []byte("é") {
		if r, ok := e.accum.feed(b); ok {
			e.insertRune(r, "")
		}
	}
	if string(e.line) != "中éab" {
		t.Fatalf("行中插入多字节应正确: %q", string(e.line))
	}
	// 孤儿续字节：丢弃
	if _, ok := e.accum.feed(0x8f); ok {
		t.Fatalf("孤儿续字节应丢弃")
	}
	// 畸形后 ASCII 输入恢复
	if r, ok := e.accum.feed('x'); !ok || r != 'x' {
		t.Fatalf("畸形序列后 ASCII 输入应恢复: %v %v", r, ok)
	}
	// 二字节序列 é（先回到行尾）
	e.cursor = len(e.line)
	for _, b := range []byte{0xC3, 0xA9} {
		if r, ok := e.accum.feed(b); ok {
			e.insertRune(r, "")
		}
	}
	if !strings.HasSuffix(string(e.line), "é") {
		t.Fatalf("二字节 UTF-8 应正确解码: %q", string(e.line))
	}
}
