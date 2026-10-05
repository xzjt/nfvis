package container

// 决策 #366（R142-12）：exec shell 握手段的有界性回归。
//
// execShellHandshake 是「拨号后 → 升级响应读完」的整段：状态行、响应头、非 101 错误体
// 三处都受 shellHandshakeTimeout 兜底。用 net.Pipe 双端 + goroutine 假服务端覆盖
// （纯用户态，Windows/Linux 皆可跑，无 AF_UNIX 依赖）。测试把 shellHandshakeTimeout
// 临时改小、用完即还原（t.Cleanup）。

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeShellServer 假 dockerd 升级端点：先吞掉请求，再按 script 决定回什么。
// net.Pipe 的 Write 阻塞到对端读走——先 io.Copy(io.Discard) 消费请求再写响应。
type fakeShellServer struct {
	conn net.Conn
}

func startFakeShellServer(t *testing.T) (net.Conn, *fakeShellServer) {
	t.Helper()
	client, server := net.Pipe()
	fs := &fakeShellServer{conn: server}
	// 默认消费请求；用例可在 goroutine 里自行再读。
	go func() { _, _ = io.Copy(io.Discard, server) }()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client, fs
}

// withTinyHandshakeTimeout 把握手超时临时改小（毫秒级），用完还原。
func withTinyHandshakeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := shellHandshakeTimeout
	shellHandshakeTimeout = d
	t.Cleanup(func() { shellHandshakeTimeout = old })
}

// ① 服务端不响应 ⇒ 在 deadline 内返回错误（整测耗时 <2s）。
// 此前这条路径是状态行 ReadString 无 deadline —— dockerd 假死时永久挂起。
func TestExecShellHandshakeNoResponseBounded(t *testing.T) {
	withTinyHandshakeTimeout(t, 150*time.Millisecond)
	conn, fs := startFakeShellServer(t)
	// 覆盖默认 goroutine：不消费请求、不写任何响应——连 Write 都要靠 deadline 兜底。
	_ = fs
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := execShellHandshake(conn, "exec-x")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("无响应应报错")
		}
		if el := time.Since(start); el >= 2*time.Second {
			t.Fatalf("握手应在 deadline（150ms）内有界失败，实际 %v", el)
		}
		if !strings.Contains(err.Error(), "docker exec shell") {
			t.Fatalf("错误应属 exec shell 路径: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("握手永久挂起（deadline 未生效）")
	}
}

// ② 慢滴非 101（状态行 502 + Content-Length 头 + 分几次滴正文）⇒ 有界失败且错误含正文摘录。
func TestExecShellHandshakeRejectSlowDrip(t *testing.T) {
	withTinyHandshakeTimeout(t, 500*time.Millisecond)
	conn, fs := startFakeShellServer(t)
	go func() {
		// 分几次滴：状态行 → 头 → 正文分两块（错误正文 11 字节）。
		_, _ = io.WriteString(fs.conn, "HTTP/1.1 502 Bad Gateway\r\n")
		time.Sleep(30 * time.Millisecond)
		_, _ = io.WriteString(fs.conn, "Content-Type: text/plain\r\nContent-Length: 11\r\n\r\n")
		time.Sleep(30 * time.Millisecond)
		_, _ = io.WriteString(fs.conn, "hello ")
		time.Sleep(30 * time.Millisecond)
		_, _ = io.WriteString(fs.conn, "world")
	}()
	start := time.Now()
	_, err := execShellHandshake(conn, "exec-x")
	if err == nil {
		t.Fatal("非 101 应失败")
	}
	if el := time.Since(start); el >= 2*time.Second {
		t.Fatalf("非 101 应有界失败，实际 %v", el)
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("错误应含状态行: %v", err)
	}
	if !strings.Contains(err.Error(), "hello world") {
		t.Fatalf("错误应含按 Content-Length 锚点读到的正文摘录: %v", err)
	}
}

// ③ 正常 101 + 头 + 空行 ⇒ 成功，且返回的流**无 deadline**：
// 握手 deadline 已过（等 2×timeout）后服务端再写、客户端读仍成功——若清 deadline
// 缺失，这次读会在 deadline 到点时报 i/o timeout。
func TestExecShellHandshakeUpgradeClearsDeadline(t *testing.T) {
	const tiny = 100 * time.Millisecond
	withTinyHandshakeTimeout(t, tiny)
	conn, fs := startFakeShellServer(t)
	go func() {
		_, _ = io.WriteString(fs.conn,
			"HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n")
		// 停 2×timeout，让握手 deadline 早已到点，再写首屏数据。
		time.Sleep(3 * tiny)
		_, _ = io.WriteString(fs.conn, "SHELL-OK\n")
	}()
	stream, err := execShellHandshake(conn, "exec-x")
	if err != nil {
		t.Fatalf("101 握手应成功: %v", err)
	}
	// 实现上最诚实的断言：行为验证——deadline 到点后的读仍然成功。
	hs := stream.(*hijackedStream) // 返回类型契约（内部流，同包直证）
	buf := make([]byte, 32)
	_ = hs.conn.SetReadDeadline(time.Now().Add(2 * time.Second)) // 测试自身的兜底限，不是被测限
	n, rerr := stream.Read(buf)
	if rerr != nil {
		t.Fatalf("握手 deadline 清除后应能继续读（首屏数据晚于 deadline 到点）: %v", rerr)
	}
	if !strings.Contains(string(buf[:n]), "SHELL-OK") {
		t.Fatalf("应读到服务端晚到的数据: %q", buf[:n])
	}
}

// ④ 非 101 且 Content-Length 缺省（0）⇒ 不额外等正文：服务端发完头就停住不关连接，
// 客户端仍应立刻返回错误（旧实现在这里靠「连接不开」才停——无锚点会一直等）。
func TestExecShellHandshakeRejectNoContentLength(t *testing.T) {
	withTinyHandshakeTimeout(t, 300*time.Millisecond)
	conn, fs := startFakeShellServer(t)
	go func() {
		_, _ = io.WriteString(fs.conn, "HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain\r\n\r\n")
		// 刻意不发正文、不关连接。
		time.Sleep(2 * time.Second)
	}()
	start := time.Now()
	_, err := execShellHandshake(conn, "exec-x")
	if err == nil {
		t.Fatal("非 101 应失败")
	}
	if el := time.Since(start); el >= 2*time.Second {
		t.Fatalf("CL=0 时不得等正文，应立即返回，实际 %v", el)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("错误应含状态行: %v", err)
	}
}

// ⑤ 非 101 且连接在中途关闭（无 Content-Length、无空行）⇒ 读到多少算多少，报错不挂起。
func TestExecShellHandshakeRejectClosedMidway(t *testing.T) {
	withTinyHandshakeTimeout(t, 300*time.Millisecond)
	conn, fs := startFakeShellServer(t)
	go func() {
		_, _ = io.WriteString(fs.conn, "HTTP/1.1 500 Internal Server Error\r\n")
		time.Sleep(20 * time.Millisecond)
		_ = fs.conn.Close() // 头都没发完就关——错误体无锚点
	}()
	start := time.Now()
	_, err := execShellHandshake(conn, "exec-x")
	if err == nil {
		t.Fatal("非 101 应失败")
	}
	if el := time.Since(start); el >= 2*time.Second {
		t.Fatalf("连接关闭应立即报错，实际 %v", el)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("错误应含状态行: %v", err)
	}
}

// 烟测 readUpgradeRejectBody：CL 越界夹到 8 KiB、非法 CL 按 0（不读正文）。
func TestReadUpgradeRejectBodyAnchors(t *testing.T) {
	// 合法 CL：读满锚点
	r1 := strings.NewReader("Content-Length: 5\r\n\r\nhello extra-not-read")
	got1 := readUpgradeRejectBody(bufio.NewReader(r1))
	if got1 != "hello" {
		t.Fatalf("应按 CL=5 读正文，得 %q", got1)
	}
	// 非法 CL：按 0 处理（不读）
	r2 := strings.NewReader("Content-Length: abc\r\n\r\nshould-not-read")
	if got2 := readUpgradeRejectBody(bufio.NewReader(r2)); got2 != "" {
		t.Fatalf("非法 CL 应按 0 不读正文，得 %q", got2)
	}
	// 无 CL 头直接空行：按 0
	r3 := strings.NewReader("\r\nbody")
	if got3 := readUpgradeRejectBody(bufio.NewReader(r3)); got3 != "" {
		t.Fatalf("无 CL 应不读正文，得 %q", got3)
	}
}
