package system

// FR-SYS-004 / FR-OPS-022（决策 #69）：RFC 5424 远程 syslog 转发。
//
// 在 nfvisd 内直接实现，不经 rsyslog/syslog-ng——journald drop-in 无法直连远端主机，
// 依赖外部转发守护会引入安装期依赖。底座（网络连接）藏在 dial 后，单测注入假实现。

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xzjt/nfvis/internal/model"
)

// 远程 syslog 缺省值与词汇表以 model 为唯一真源（FR-SYS-004，决策 #69）。
const (
	DefaultSyslogPort     = model.DefaultSyslogPort
	DefaultSyslogFacility = model.DefaultSyslogFacility
	DefaultSyslogSeverity = model.DefaultSyslogSeverity
	DefaultSyslogNetwork  = model.DefaultSyslogNetwork
)

// FacilityCode 解析 facility 名（词汇表见 model）。
func FacilityCode(name string) (int, bool) { return model.FacilityCode(name) }

// SeverityCode 解析 severity 名（词汇表见 model）。
func SeverityCode(name string) (int, bool) { return model.SeverityCode(name) }

// SyslogConfig 远程转发参数（零值 Host 表示不转发）。
type SyslogConfig struct {
	Host     string
	Port     int
	Facility string // 缺省 user
	Severity string // 最低转发级别，缺省 info
	Network  string // udp（缺省）| tcp
}

func (c SyslogConfig) withDefaults() SyslogConfig {
	if c.Port == 0 {
		c.Port = DefaultSyslogPort
	}
	if c.Facility == "" {
		c.Facility = DefaultSyslogFacility
	}
	if c.Severity == "" {
		c.Severity = DefaultSyslogSeverity
	}
	if c.Network == "" {
		c.Network = DefaultSyslogNetwork
	}
	return c
}

// Enabled 是否配置了转发目标。
func (c SyslogConfig) Enabled() bool { return strings.TrimSpace(c.Host) != "" }

// severityThreshold 配置的最低转发级别（未知级别回落 info）。
func (c SyslogConfig) severityThreshold() int {
	if code, ok := SeverityCode(c.Severity); ok {
		return code
	}
	return model.SyslogSeverityCode[DefaultSyslogSeverity]
}

// FormatRFC5424 生成一条 RFC 5424 报文（纯函数，便于单测）。
//
// 形如：<PRI>1 TIMESTAMP HOST APP PROCID MSGID SD MSG
// 空字段按规范写 NILVALUE（-）；MSG 为 UTF-8，按规范建议以 BOM 起头。
func FormatRFC5424(t time.Time, host, app string, pid int, msgID string, facility, severity int, msg string) string {
	pri := facility*8 + severity
	nilv := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	procid := "-"
	if pid > 0 {
		procid = strconv.Itoa(pid)
	}
	return fmt.Sprintf("<%d>1 %s %s %s %s %s - \uFEFF%s",
		pri, t.UTC().Format(time.RFC3339Nano), nilv(host), nilv(app), procid, nilv(msgID), msg)
}

// SyslogForwarder 远程 syslog 转发器（可重配置；未配置目标时为空操作）。
type SyslogForwarder struct {
	mu       sync.Mutex
	cfg      SyslogConfig
	conn     net.Conn
	dial     func(network, addr string) (net.Conn, error)
	hostname string
	lastErr  error
	now      func() time.Time
	// 拨号失败后的熔断时间点：之前 Forward 直接丢弃，不再反复拨号
	retryAfter time.Time
}

const (
	// 拨号失败后的冷却期：远端不可达时每条日志都烧一次 3s 拨号超时，
	// 日志路径会被拖死——冷却期内丢弃转发。
	syslogRetryCooldown = 30 * time.Second
	// 单条写超时：TCP 对端不读时 Write 可能无限阻塞。
	syslogWriteTimeout = 5 * time.Second
)

// NewSyslogForwarder 构造转发器（dial 为 nil 时用 net.DialTimeout）。
func NewSyslogForwarder(cfg SyslogConfig) *SyslogForwarder {
	host, _ := os.Hostname()
	return &SyslogForwarder{
		cfg:      cfg.withDefaults(),
		dial:     func(network, addr string) (net.Conn, error) { return net.DialTimeout(network, addr, 3*time.Second) },
		hostname: host,
		now:      time.Now,
	}
}

// SetDialer 替换连接实现（单测注入）。
func (f *SyslogForwarder) SetDialer(dial func(network, addr string) (net.Conn, error)) { f.dial = dial }

// Configure 应用新配置（目标变化时重建连接）。
func (f *SyslogForwarder) Configure(cfg SyslogConfig) {
	cfg = cfg.withDefaults()
	f.mu.Lock()
	defer f.mu.Unlock()
	same := f.cfg.Host == cfg.Host && f.cfg.Port == cfg.Port && f.cfg.Network == cfg.Network
	f.cfg = cfg
	if !same {
		f.closeLocked()
		f.retryAfter = time.Time{} // 换目标：立即尝试，不沿用旧目标的熔断
	}
}

// Config 返回当前配置。
func (f *SyslogForwarder) Config() SyslogConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}

// LastError 最近一次转发错误（成功或未配置时为 nil）。
func (f *SyslogForwarder) LastError() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastErr
}

// Close 关闭连接。
func (f *SyslogForwarder) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeLocked()
}

func (f *SyslogForwarder) closeLocked() error {
	if f.conn == nil {
		return nil
	}
	err := f.conn.Close()
	f.conn = nil
	return err
}

// Forward 按配置转发一条消息（未配置目标或低于级别阈值时为空操作）。
//
// 拨号在锁外进行（上限 3s），失败后进入 30s 冷却熔断；写带 5s 超时且
// 仅在写期间持锁——远程 syslog 不可达或对端不读时，转发不会长时间
// 拖死日志调用方。
func (f *SyslogForwarder) Forward(severity int, app, msgID, msg string) error {
	f.mu.Lock()
	if !f.cfg.Enabled() {
		f.mu.Unlock()
		return nil
	}
	if severity > f.cfg.severityThreshold() {
		f.mu.Unlock()
		return nil // 低于阈值不转发
	}
	facility := 1 // user
	if code, ok := FacilityCode(f.cfg.Facility); ok {
		facility = code
	}
	line := FormatRFC5424(f.now(), f.hostname, app, os.Getpid(), msgID, facility, severity, msg)
	network, addr := f.cfg.Network, net.JoinHostPort(f.cfg.Host, strconv.Itoa(f.cfg.Port))
	conn := f.conn
	inCooldown := !f.retryAfter.IsZero() && f.now().Before(f.retryAfter)
	f.mu.Unlock()

	// 拨号在锁外（可达 3s），不阻塞其他转发；并发拨号仅保留先安装的一条
	if conn == nil {
		if inCooldown {
			return f.cooldownErr(addr)
		}
		c, err := f.dial(network, addr)
		if err != nil {
			f.mu.Lock()
			ferr := fmt.Errorf("连接远程 syslog %s: %w", addr, err)
			f.lastErr = ferr
			f.retryAfter = f.now().Add(syslogRetryCooldown)
			f.mu.Unlock()
			return ferr
		}
		f.mu.Lock()
		if f.conn == nil {
			f.conn = c
		} else {
			_ = c.Close()
		}
		f.retryAfter = time.Time{}
		conn = f.conn
		f.mu.Unlock()
	}

	payload := []byte(line)
	if network == "tcp" {
		// RFC 6587 octet-counting 分帧（多帧不得交错，写在锁内串行）
		payload = []byte(strconv.Itoa(len(payload)) + " " + line)
	}
	_ = conn.SetWriteDeadline(f.now().Add(syslogWriteTimeout))
	f.mu.Lock()
	_, err := conn.Write(payload)
	f.mu.Unlock()
	if err == nil {
		f.mu.Lock()
		f.lastErr = nil
		f.mu.Unlock()
		return nil
	}
	_ = conn.Close()
	f.mu.Lock()
	if f.conn == conn {
		f.conn = nil // 下条消息重连
	}
	ferr := fmt.Errorf("转发远程 syslog: %w", err)
	f.lastErr = ferr
	f.mu.Unlock()
	return ferr
}

// cooldownErr 熔断冷却期内丢弃转发：返回最近一次失败原因（无则给通用说明）。
func (f *SyslogForwarder) cooldownErr(addr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastErr != nil {
		return f.lastErr
	}
	return fmt.Errorf("远程 syslog %s 熔断冷却中，转发已丢弃", addr)
}

// SeverityOfSlog slog 级别 → syslog severity。
func SeverityOfSlog(l slog.Level) int {
	switch {
	case l < slog.LevelInfo:
		return 7 // debug
	case l < slog.LevelWarn:
		return 6 // info
	case l < slog.LevelError:
		return 4 // warning
	default:
		return 3 // err
	}
}

// SyslogHandler 包装 slog.Handler：记录照常落本地，同时转发到远程 syslog（FR-OPS-030/FR-SYS-004）。
type SyslogHandler struct {
	Inner slog.Handler
	Fwd   *SyslogForwarder
	App   string
}

func (h SyslogHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.Inner.Enabled(ctx, l)
}

func (h SyslogHandler) Handle(ctx context.Context, r slog.Record) error {
	if h.Fwd != nil {
		var b strings.Builder
		b.WriteString(r.Message)
		r.Attrs(func(a slog.Attr) bool {
			fmt.Fprintf(&b, " %s=%v", a.Key, a.Value.Any())
			return true
		})
		// 转发失败不阻塞本地日志（原因经 LastError 可查）
		_ = h.Fwd.Forward(SeverityOfSlog(r.Level), h.App, "log", b.String())
	}
	return h.Inner.Handle(ctx, r)
}

func (h SyslogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return SyslogHandler{Inner: h.Inner.WithAttrs(attrs), Fwd: h.Fwd, App: h.App}
}

func (h SyslogHandler) WithGroup(name string) slog.Handler {
	return SyslogHandler{Inner: h.Inner.WithGroup(name), Fwd: h.Fwd, App: h.App}
}
