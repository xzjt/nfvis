// Package network 实现网络编排层到 VPP 数据面的适配（工程骨架 §5 M3）。
//
// M3-1：连接管理与版本锁定（FR-SYS-007）。底座调用藏在 Dialer/Session 接口之后，
// 单测注入假实现；govpp 真实实现见 vpp_govpp.go，集成测试用 build tag integration。
// 设计原则（M3 任务清单）：编排器只把 committed 配置收敛到 VPP，状态判断留在
// config/state 层，本包不 import internal/config。
package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// 默认值与常量。
const (
	DefaultSocket      = "/run/vpp/api.sock" // M3-P0 约定：NFVIS_VPP_SOCK 缺省值
	RequiredVPPVersion = "26.06"             // FR-SYS-007 版本锁定
	defaultAttempts    = 3
	defaultInterval    = 500 * time.Millisecond
	defaultRetryDelay  = 3 * time.Second
)

// State 连接状态（对 govpp core.ConnectionState 的本地投影，避免上层依赖 govpp）。
type State int

const (
	StateDisconnected State = iota
	StateConnected
	StateNotResponding
	StateFailed
)

func (s State) String() string {
	switch s {
	case StateConnected:
		return "connected"
	case StateNotResponding:
		return "not-responding"
	case StateFailed:
		return "failed"
	default:
		return "disconnected"
	}
}

// Event 连接状态变化（由 Dialer 的事件通道投递）。
type Event struct {
	State State
	Err   error
}

// Session 一次 VPP 连接会话的能力（govpp 适配实现）。
type Session interface {
	Version() (string, error)
	Disconnect()
}

// Dialer 建立到 VPP 的连接。返回会话与状态事件通道（通道关闭表示不再有事件）。
type Dialer interface {
	Dial(socket string, attempts int, interval time.Duration) (Session, <-chan Event, error)
}

// VersionError VPP 版本不匹配（FR-SYS-007，致命：拒绝启动/停止收敛）。
type VersionError struct {
	Got      string
	Required string
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("VPP 版本不匹配（FR-SYS-007）：运行 %q，要求 %q", e.Got, e.Required)
}

// ErrUnavailable 无法连接 VPP（未运行、套接字缺失等，非致命：降级并重试）。
var ErrUnavailable = errors.New("VPP 不可用")

// Config 连接管理器配置。
type Config struct {
	Socket            string
	RequiredVersion   string
	ReconnectAttempts int
	ReconnectInterval time.Duration
	RetryDelay        time.Duration
	Log               *slog.Logger
}

func (c Config) withDefaults() Config {
	if c.Socket == "" {
		c.Socket = DefaultSocket
	}
	if c.RequiredVersion == "" {
		c.RequiredVersion = RequiredVPPVersion
	}
	if c.ReconnectAttempts <= 0 {
		c.ReconnectAttempts = defaultAttempts
	}
	if c.ReconnectInterval <= 0 {
		c.ReconnectInterval = defaultInterval
	}
	if c.RetryDelay <= 0 {
		c.RetryDelay = defaultRetryDelay
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	return c
}

// Manager VPP 连接管理器：建立连接、校验版本、断线自动重连。
type Manager struct {
	cfg    Config
	dialer Dialer

	mu          sync.Mutex
	session     Session
	events      <-chan Event
	state       State
	version     string
	lastErr     error
	onReconnect func(version string)
}

// NewManager 构造管理器（dialer 为 nil 时使用 govpp 实现）。
func NewManager(cfg Config, dialer Dialer) *Manager {
	if dialer == nil {
		dialer = NewGovppDialer()
	}
	return &Manager{cfg: cfg.withDefaults(), dialer: dialer, state: StateDisconnected}
}

// OnReconnect 注册重连成功回调（M3-8 恢复收敛的接入点）。
func (m *Manager) OnReconnect(fn func(version string)) { m.onReconnect = fn }

// State 当前连接状态。
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Version 最近一次成功连接探测到的 VPP 版本。
func (m *Manager) Version() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.version
}

// LastError 最近一次连接错误（供 /vpp/status，M3-7）。
func (m *Manager) LastError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

// ConnectOnce 尝试连接一次并校验版本，成功返回版本号。
// 版本不匹配返回 *VersionError；连不上返回 ErrUnavailable（包装原因）。
func (m *Manager) ConnectOnce(ctx context.Context) (string, error) {
	sess, events, err := m.dialer.Dial(m.cfg.Socket, m.cfg.ReconnectAttempts, m.cfg.ReconnectInterval)
	if err != nil {
		m.setFailure(err)
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	// 等待 Connected/Failed：govpp AsyncConnect 异步重试后经事件通道告知结果。
	select {
	case ev, ok := <-events:
		if !ok {
			sess.Disconnect()
			m.setFailure(ErrUnavailable)
			return "", ErrUnavailable
		}
		if ev.State != StateConnected {
			sess.Disconnect()
			cause := ev.Err
			if cause == nil {
				cause = ErrUnavailable
			}
			m.setFailure(cause)
			return "", fmt.Errorf("%w: %v", ErrUnavailable, cause)
		}
	case <-ctx.Done():
		sess.Disconnect()
		m.setFailure(ctx.Err())
		return "", fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
	}

	ver, err := sess.Version()
	if err != nil {
		sess.Disconnect()
		m.setFailure(err)
		return "", fmt.Errorf("查询 VPP 版本失败: %w", err)
	}
	if !versionMatches(ver, m.cfg.RequiredVersion) {
		sess.Disconnect()
		ve := &VersionError{Got: ver, Required: m.cfg.RequiredVersion}
		m.setFailure(ve)
		return "", ve
	}

	m.mu.Lock()
	m.session, m.events, m.state, m.version, m.lastErr = sess, events, StateConnected, ver, nil
	m.mu.Unlock()
	return ver, nil
}

// Run 持续维护连接：连不上时按 RetryDelay 重试（VPP 未运行时降级重试而非崩溃），
// 断开后自动重连并调用 OnReconnect；版本不匹配为致命错误，直接返回。
func (m *Manager) Run(ctx context.Context) error {
	connectedOnce := false
	for {
		if ctx.Err() != nil {
			m.Close()
			return nil
		}
		ver, err := m.ConnectOnce(ctx)
		if err != nil {
			var ve *VersionError
			if errors.As(err, &ve) {
				return err // FR-SYS-007：版本不符拒绝继续
			}
			m.cfg.Log.Warn("连接 VPP 失败，稍后重试", "socket", m.cfg.Socket, "err", err)
			if !sleepCtx(ctx, m.cfg.RetryDelay) {
				m.Close()
				return nil
			}
			continue
		}
		if connectedOnce && m.onReconnect != nil {
			m.onReconnect(ver) // 重连成功，触发恢复收敛（M3-8）
		}
		connectedOnce = true
		m.cfg.Log.Info("已连接 VPP", "version", ver, "socket", m.cfg.Socket)

		m.watch(ctx) // 阻塞至断开/失败或 ctx 取消
	}
}

// watch 等待连接状态变化；断开后返回由 Run 触发重连。
func (m *Manager) watch(ctx context.Context) {
	m.mu.Lock()
	events := m.events
	m.mu.Unlock()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				m.setFailure(ErrUnavailable)
				m.Close()
				return
			}
			m.mu.Lock()
			m.state, m.lastErr = ev.State, ev.Err
			m.mu.Unlock()
			switch ev.State {
			case StateDisconnected, StateFailed, StateNotResponding:
				m.cfg.Log.Warn("VPP 连接中断，准备重连", "state", ev.State.String(), "err", ev.Err)
				m.Close()
				return
			}
		case <-ctx.Done():
			m.Close()
			return
		}
	}
}

// Close 断开当前会话（幂等）。
func (m *Manager) Close() {
	m.mu.Lock()
	sess := m.session
	m.session, m.events = nil, nil
	if sess != nil {
		m.state = StateDisconnected
	}
	m.mu.Unlock()
	if sess != nil {
		sess.Disconnect()
	}
}

func (m *Manager) setFailure(err error) {
	m.mu.Lock()
	m.state, m.lastErr, m.session, m.events = StateFailed, err, nil, nil
	m.mu.Unlock()
}

// versionMatches 版本前缀匹配（VPP 返回形如 "26.06-release"）。
func versionMatches(got, required string) bool {
	return required == "" || strings.Contains(got, required)
}

// sleepCtx 可被 ctx 取消的等待；返回 false 表示 ctx 已取消。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
