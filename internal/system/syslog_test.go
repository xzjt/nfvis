package system

// FR-SYS-004 / FR-OPS-022（决策 #69）：RFC 5424 远程 syslog 转发单测。

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// ---------- 报文格式（纯函数） ----------

func TestFormatRFC5424(t *testing.T) {
	ts := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	// user(1)*8 + warning(4) = 12
	got := FormatRFC5424(ts, "nfvis", "nfvisd", 1234, "alarm", 1, 4, "接口 ens192 down")
	if !strings.HasPrefix(got, "<12>1 2026-09-14T12:00:00Z nfvis nfvisd 1234 alarm - ") {
		t.Fatalf("报文头不符: %q", got)
	}
	if !strings.HasSuffix(got, "接口 ens192 down") {
		t.Fatalf("消息体不符: %q", got)
	}
	// UTF-8 MSG 应以 BOM 起头（RFC 5424 §6.4 建议）
	if !strings.Contains(got, "\uFEFF接口") {
		t.Fatalf("UTF-8 消息应带 BOM: %q", got)
	}
}

func TestFormatRFC5424NilValues(t *testing.T) {
	got := FormatRFC5424(time.Unix(0, 0), "", "", 0, "", 0, 6, "x")
	// host/app/procid/msgid 均空 → NILVALUE "-"；facility 0(daemon? kern)*8+6
	want := "<6>1 1970-01-01T00:00:00Z - - - - - \uFEFFx"
	if got != want {
		t.Fatalf("NILVALUE 处理错误:\n 得到 %q\n 期望 %q", got, want)
	}
}

func TestFacilityAndSeverityCode(t *testing.T) {
	cases := map[string]int{"daemon": 3, "local0": 16, "local7": 23, "USER": 1, " user ": 1}
	for name, want := range cases {
		if got, ok := FacilityCode(name); !ok || got != want {
			t.Fatalf("FacilityCode(%q) = %d,%v，期望 %d", name, got, ok, want)
		}
	}
	if _, ok := FacilityCode("nope"); ok {
		t.Fatal("未知 facility 应返回 false")
	}
	if got, ok := SeverityCode("warn"); !ok || got != 4 {
		t.Fatalf("SeverityCode(warn) = %d,%v，期望 4", got, ok)
	}
	if got, ok := SeverityCode("err"); !ok || got != 3 {
		t.Fatalf("SeverityCode(err) = %d,%v，期望 3", got, ok)
	}
}

// ---------- 转发行为（注入假 dialer） ----------

type fakeConn struct {
	buf      bytes.Buffer
	closed   bool
	writeErr error
}

func (c *fakeConn) Write(p []byte) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return c.buf.Write(p)
}
func (c *fakeConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *fakeConn) Close() error                     { c.closed = true; return nil }
func (c *fakeConn) LocalAddr() net.Addr              { return nil }
func (c *fakeConn) RemoteAddr() net.Addr             { return nil }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

func newTestForwarder(cfg SyslogConfig) (*SyslogForwarder, *fakeConn) {
	f := NewSyslogForwarder(cfg)
	c := &fakeConn{}
	f.SetDialer(func(_, _ string) (net.Conn, error) { return c, nil })
	f.hostname = "nfvis"
	f.now = func() time.Time { return time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC) }
	return f, c
}

// 未配置目标时为空操作（不得拨号）。
func TestForwardDisabledIsNoop(t *testing.T) {
	f := NewSyslogForwarder(SyslogConfig{})
	dialed := false
	f.SetDialer(func(_, _ string) (net.Conn, error) { dialed = true; return &fakeConn{}, nil })
	if err := f.Forward(3, "nfvisd", "log", "x"); err != nil {
		t.Fatalf("未配置目标应无错误: %v", err)
	}
	if dialed {
		t.Fatal("未配置目标不应拨号")
	}
	if f.LastError() != nil {
		t.Fatalf("LastError 应为 nil: %v", f.LastError())
	}
}

func TestForwardWritesRFC5424(t *testing.T) {
	f, c := newTestForwarder(SyslogConfig{Host: "10.0.0.9", Port: 514, Facility: "local0", Severity: "info"})
	// local0(16)*8 + err(3) = 131
	if err := f.Forward(3, "nfvisd", "alarm", "[VM_CRASHED] vm1 异常退出"); err != nil {
		t.Fatal(err)
	}
	out := c.buf.String()
	if !strings.HasPrefix(out, "<131>1 2026-09-14T12:00:00Z nfvis nfvisd ") {
		t.Fatalf("PRI/host/app 不符: %q", out)
	}
	if !strings.Contains(out, "alarm - ") || !strings.Contains(out, "[VM_CRASHED] vm1 异常退出") {
		t.Fatalf("消息体不符: %q", out)
	}
	if f.LastError() != nil {
		t.Fatalf("成功转发后 LastError 应为 nil: %v", f.LastError())
	}
}

// 低于配置阈值不转发（severity 数值越小越严重）。
func TestForwardLevelFiltering(t *testing.T) {
	f, c := newTestForwarder(SyslogConfig{Host: "10.0.0.9", Severity: "warn"})
	if err := f.Forward(6, "nfvisd", "log", "info 不该转发"); err != nil {
		t.Fatal(err)
	}
	if c.buf.Len() != 0 {
		t.Fatalf("低于阈值不应转发: %q", c.buf.String())
	}
	if err := f.Forward(4, "nfvisd", "log", "warn 应转发"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.buf.String(), "warn 应转发") {
		t.Fatalf("阈值内应转发: %q", c.buf.String())
	}
}

// TCP 使用 RFC 6587 octet-counting 分帧。
func TestForwardTCPFraming(t *testing.T) {
	f, c := newTestForwarder(SyslogConfig{Host: "10.0.0.9", Network: "tcp"})
	if err := f.Forward(4, "nfvisd", "log", "hello"); err != nil {
		t.Fatal(err)
	}
	out := c.buf.String()
	idx := strings.Index(out, " ")
	if idx <= 0 {
		t.Fatalf("缺少长度前缀: %q", out)
	}
	declared := out[:idx]
	body := out[idx+1:]
	if declared != itoa(len(body)) {
		t.Fatalf("长度前缀 %s 与实际 %d 不符", declared, len(body))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// 连接失败时返回错误并记录 LastError，调用方不被阻塞。
func TestForwardDialError(t *testing.T) {
	f := NewSyslogForwarder(SyslogConfig{Host: "10.0.0.9"})
	f.SetDialer(func(_, _ string) (net.Conn, error) { return nil, errors.New("connection refused") })
	err := f.Forward(3, "nfvisd", "log", "x")
	if err == nil {
		t.Fatal("拨号失败应返回错误")
	}
	if f.LastError() == nil || !strings.Contains(f.LastError().Error(), "connection refused") {
		t.Fatalf("LastError 应记录原因: %v", f.LastError())
	}
}

// 拨号失败进入熔断：冷却期内不再拨号（转发丢弃），冷却结束恢复，换目标立即解禁。
func TestForwardDialFailureCooldown(t *testing.T) {
	f := NewSyslogForwarder(SyslogConfig{Host: "10.0.0.9"})
	base := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	f.now = func() time.Time { return base }
	dials := 0
	f.SetDialer(func(_, _ string) (net.Conn, error) {
		dials++
		return nil, errors.New("connection refused")
	})

	if err := f.Forward(3, "nfvisd", "log", "x"); err == nil {
		t.Fatal("拨号失败应返回错误")
	}
	if err := f.Forward(3, "nfvisd", "log", "y"); err == nil {
		t.Fatal("冷却期内转发丢弃仍应返回错误")
	}
	if dials != 1 {
		t.Fatalf("冷却期内不应重复拨号，实际 %d 次", dials)
	}

	// 冷却结束后恢复拨号
	f.now = func() time.Time { return base.Add(syslogRetryCooldown) }
	_ = f.Forward(3, "nfvisd", "log", "z")
	if dials != 2 {
		t.Fatalf("冷却结束后应重新拨号，实际 %d 次", dials)
	}

	// 换目标：立即解禁，不沿用旧目标的熔断
	f.now = func() time.Time { return base }
	f.Configure(SyslogConfig{Host: "10.0.0.10"})
	_ = f.Forward(3, "nfvisd", "log", "w")
	if dials != 3 {
		t.Fatalf("换目标后应立即拨号，实际 %d 次", dials)
	}
}

// 写失败后下次重连（连接置空）。
func TestForwardWriteErrorReconnects(t *testing.T) {
	f, c := newTestForwarder(SyslogConfig{Host: "10.0.0.9"})
	c.writeErr = errors.New("broken pipe")
	if err := f.Forward(3, "nfvisd", "log", "x"); err == nil {
		t.Fatal("写失败应返回错误")
	}
	if !c.closed {
		t.Fatal("写失败后应关闭连接以便重连")
	}
	dials := 0
	f.SetDialer(func(_, _ string) (net.Conn, error) { dials++; return &fakeConn{}, nil })
	if err := f.Forward(3, "nfvisd", "log", "y"); err != nil {
		t.Fatalf("重连后应成功: %v", err)
	}
	if dials != 1 {
		t.Fatalf("应重连一次，实际 %d", dials)
	}
}

// 目标变更时关闭旧连接。
func TestConfigureClosesOnTargetChange(t *testing.T) {
	f, c := newTestForwarder(SyslogConfig{Host: "10.0.0.9"})
	if err := f.Forward(3, "nfvisd", "log", "x"); err != nil {
		t.Fatal(err)
	}
	f.Configure(SyslogConfig{Host: "10.0.0.10"})
	if !c.closed {
		t.Fatal("目标变更应关闭旧连接")
	}
	// 同一目标重复配置不应关闭
	c2 := &fakeConn{}
	f.SetDialer(func(_, _ string) (net.Conn, error) { return c2, nil })
	if err := f.Forward(3, "nfvisd", "log", "y"); err != nil {
		t.Fatal(err)
	}
	f.Configure(SyslogConfig{Host: "10.0.0.10"})
	if c2.closed {
		t.Fatal("同一目标重复配置不应关闭连接")
	}
}

// ---------- slog 接入 ----------

func TestSyslogHandlerForwardsRecords(t *testing.T) {
	f, c := newTestForwarder(SyslogConfig{Host: "10.0.0.9", Severity: "debug"})
	var buf bytes.Buffer
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelDebug)
	h := SyslogHandler{Inner: slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: lv}), Fwd: f, App: "nfvisd"}
	log := slog.New(h)

	log.Warn("接口告警", "iface", "ens192")
	log.Debug("调试信息")

	out := c.buf.String()
	if !strings.Contains(out, "接口告警") || !strings.Contains(out, "iface=ens192") {
		t.Fatalf("应转发含属性的记录: %q", out)
	}
	// UDP 下每条记录是一个独立报文，故不追加换行；用报文头计数断言两条都已发出
	if n := strings.Count(out, ">1 2026-09-14T12:00:00Z nfvis nfvisd "); n != 2 {
		t.Fatalf("应转发两条报文，实际 %d: %q", n, out)
	}
	if !strings.Contains(out, "调试信息") {
		t.Fatalf("debug 记录在阈值内应转发: %q", out)
	}
	if buf.Len() == 0 { // 本地日志仍须落地
		t.Fatal("本地日志不应被吞")
	}
	if !strings.Contains(buf.String(), "调试信息") {
		t.Fatalf("本地应含 debug 记录: %q", buf.String())
	}
}

func TestSeverityOfSlog(t *testing.T) {
	cases := []struct {
		l    slog.Level
		want int
	}{
		{slog.LevelDebug, 7}, {slog.LevelInfo, 6}, {slog.LevelWarn, 4}, {slog.LevelError, 3},
	}
	for _, c := range cases {
		if got := SeverityOfSlog(c.l); got != c.want {
			t.Fatalf("SeverityOfSlog(%v) = %d，期望 %d", c.l, got, c.want)
		}
	}
}

// WithAttrs/WithGroup 必须保留转发器（否则派生 logger 会静默丢失转发）。
func TestSyslogHandlerDerivedKeepsForwarder(t *testing.T) {
	f, c := newTestForwarder(SyslogConfig{Host: "10.0.0.9", Severity: "debug"})
	h := SyslogHandler{Inner: slog.NewJSONHandler(io.Discard, nil), Fwd: f, App: "nfvisd"}
	derived := h.WithAttrs([]slog.Attr{slog.String("k", "v")}).WithGroup("g")
	slog.New(derived).Info("派生记录")
	if !strings.Contains(c.buf.String(), "派生记录") {
		t.Fatalf("派生 handler 应仍转发: %q", c.buf.String())
	}
}

// Configure 空目标 → 停止转发。
func TestConfigureDisableStopsForwarding(t *testing.T) {
	f, c := newTestForwarder(SyslogConfig{Host: "10.0.0.9"})
	if err := f.Forward(3, "nfvisd", "log", "before"); err != nil {
		t.Fatal(err)
	}
	n := c.buf.Len()
	f.Configure(SyslogConfig{})
	if err := f.Forward(3, "nfvisd", "log", "after"); err != nil {
		t.Fatal(err)
	}
	if c.buf.Len() != n {
		t.Fatal("清空目标后不应再转发")
	}
}
