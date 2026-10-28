package cli

// L12（round51 评审）：串口接管期间，REPL 主循环与「服务端→本地」的 copy 协程**同时写**
// 同一个 r.out（raw 下是 crlfWriter）。crlfWriter 的 prev/has 是跨 Write 的**可变状态**，
// 无同步即数据竞争；copyConsole 返回（Ctrl-]/断开/写错）时该协程还可能卡在一次 Write 里，
// 而 REPL 已经在重绘提示符。
//
// 修法两面（都钉在下述用例里）：
//   - crlfWriter 自持互斥（并发 Write 串行化，prev/has 不再被交叉改写）；
//   - runConsole 返回前 join 那个协程（joinConsoleCopy：关流 → 等它退出）。
//
// ⚠️ 本机 -race 不可用（需要 cgo），故用例用**可阻塞的底层 writer**做确定性交错复现。

import (
	"io"
	"sync"
	"testing"
	"time"
)

// gateWriter 底层 writer：第 1 次 Write **进入后阻塞**在 gate 上（模拟「协程正卡在一次写里」），
// 记录每次进入与完成，供用例断言「有没有被重入 / 有没有在 join 之后继续写」。
type gateWriter struct {
	entered chan struct{}
	gate    chan struct{}
	mu      sync.Mutex
	calls   int      // Write 被进入的次数（含仍阻塞者）
	done    []string // 已完成的块（按完成顺序）
}

func newGateWriter() *gateWriter {
	return &gateWriter{entered: make(chan struct{}), gate: make(chan struct{})}
}

func (g *gateWriter) Write(p []byte) (int, error) {
	g.mu.Lock()
	g.calls++
	c := g.calls
	g.mu.Unlock()
	if c == 1 {
		close(g.entered)
		<-g.gate
	}
	g.mu.Lock()
	g.done = append(g.done, string(p))
	g.mu.Unlock()
	return len(p), nil
}

func (g *gateWriter) completed() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string{}, g.done...)
}

// TestCRLFWriterSerializesConcurrentWrites 并发 Write 必须串行：第一写仍阻塞在底层时，
// 第二写不得挤进底层（进入次数须仍为 1）。
// 红-绿：去掉 crlfWriter 的互斥，第二写会立刻进底层（calls=2）——用例即红。
func TestCRLFWriterSerializesConcurrentWrites(t *testing.T) {
	gw := newGateWriter()
	w := &crlfWriter{w: gw, raw: func() bool { return true }}

	go func() { _, _ = w.Write([]byte("a\n")) }() // 进入底层后在 gate 上阻塞（此时已持有 crlfWriter 锁）
	select {
	case <-gw.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("第一写未进入底层 writer")
	}

	second := make(chan struct{})
	go func() {
		_, _ = w.Write([]byte("b\n"))
		close(second)
	}()
	// 互斥下第二写必须等第一写放行；无互斥则它已挤进底层（calls=2）。
	time.Sleep(60 * time.Millisecond)
	gw.mu.Lock()
	calls := gw.calls
	gw.mu.Unlock()
	if calls != 1 {
		t.Fatalf("并发 Write 未被串行化（底层被重入 %d 次）：crlfWriter 的 prev/has 会被交叉改写", calls)
	}

	close(gw.gate) // 放行第一写
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("第二写在放行后仍未完成（疑似死锁）")
	}
	if got := gw.completed(); len(got) != 2 || got[0] != "a\r\n" || got[1] != "b\r\n" {
		t.Fatalf("两块须各自完整、按序落地: %q", got)
	}
}

// TestJoinConsoleCopyWaitsForCopyGoroutine joinConsoleCopy 必须等到底层写**完成**——
// 否则 REPL 会与仍卡在 Write 里的 copy 协程并发写同一 writer。
// 红-绿：把 joinConsoleCopy 改成「只关流不等待」，本用例在第一个 select 就报红。
func TestJoinConsoleCopyWaitsForCopyGoroutine(t *testing.T) {
	pr, pw := io.Pipe()
	gw := newGateWriter()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(gw, pr)
		close(done)
	}()
	// 服务端先发一块：copy 协程进入底层 Write 并在 gate 上阻塞（＝Ctrl-] 返回时它还在写）。
	if _, err := pw.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gw.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("copy 协程未进入底层 writer")
	}

	joined := make(chan struct{})
	go func() {
		joinConsoleCopy(pr, done)
		close(joined)
	}()
	select {
	case <-joined:
		t.Fatal("copy 协程仍卡在 Write 里时 join 就返回了（提示符会与它并发写同一 writer）")
	case <-time.After(80 * time.Millisecond):
	}

	close(gw.gate) // 放行那一写 ⇒ io.Copy 返回 ⇒ done 关闭 ⇒ join 可返回
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("放行后 join 仍未返回")
	}

	// join 返回后再等一会儿：copy 协程不得再写任何东西。
	n := len(gw.completed())
	time.Sleep(40 * time.Millisecond)
	if got := len(gw.completed()); got != n {
		t.Fatalf("join 返回后 copy 协程仍在写（%d → %d），与提示符重绘并发", n, got)
	}
	_ = pw.Close()
}
