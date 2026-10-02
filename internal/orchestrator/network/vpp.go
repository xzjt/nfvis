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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/xzjt/nfvis/internal/model"
)

// 默认值与常量。
const (
	DefaultSocket      = "/run/vpp/api.sock" // M3-P0 约定：NFVIS_VPP_SOCK 缺省值
	RequiredVPPVersion = "26.06"             // FR-SYS-007 版本锁定
	defaultAttempts    = 3
	defaultInterval    = 500 * time.Millisecond
	defaultRetryDelay  = 3 * time.Second

	// defaultEnsureProbeTimeout 决策 #348：启动序列里「探一下 VPP 是否已在运行」的硬超时上限。
	// 探测即使卡住（govpp 连接路径不接收 context）也在此上限内返回，绝不拖住 nfvisd 启动。
	defaultEnsureProbeTimeout = 2 * time.Second
	// defaultEnsureStartTimeout 决策 #348：发起拉起（systemctl start --no-block）的防御性超时上限。
	// --no-block 正常立即返回，本上限只兜底异常（如 dbus 卡住），同样不阻塞启动。
	defaultEnsureStartTimeout = 5 * time.Second
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
	return fmt.Sprintf("VPP 版本不匹配：运行 %q，要求 %q", e.Got, e.Required)
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
	appliedHash string // 最近一次落地/重启所依据的 vpp 配置段哈希（pending_restart 判定）
	onConnect   func(version string)
	// gen 成功建立连接的世代：首次连接与每次重连各 +1（决策 #315）。
	// 判据用途：`state == StateConnected` 可能来自**陈旧会话**（VPP 重启后 govpp 尚未
	// 报出断连的那段窗口），世代前进才说明管理器已换成新连接、查询路径可用。
	gen uint64

	statsOnce sync.Once // stats segment 惰性连接（stats_govpp.go）
	statsConn *statsConn
	statsTool StatsTool // statsclient 解码失败/缺项时的同版本工具回退源（决策 #68）

	// 决策 #348：nfvisd 启动时确保 VPP 运行（EnsureRunning）。
	// 拉起是**发起式**的（--no-block），启动序列绝不能被 VPP 拉起阻塞；就绪由既有连接重试循环承担。
	ensureStart        Starter                     // 发起拉起（缺省 systemctl start --no-block vpp）
	ensureProbe        func(context.Context) error // 「VPP 是否已在运行」探测（缺省 socket 存在 + API 可连；测试注入）
	ensureProbeTimeout time.Duration               // 探测硬超时（<=0 取 defaultEnsureProbeTimeout）
	ensureStartTimeout time.Duration               // 发起动作的防御性超时（<=0 取 defaultEnsureStartTimeout）
}

// NewManager 构造管理器（dialer 为 nil 时使用 govpp 实现）。
func NewManager(cfg Config, dialer Dialer) *Manager {
	if dialer == nil {
		dialer = NewGovppDialer()
	}
	cfg = cfg.withDefaults()
	return &Manager{cfg: cfg, dialer: dialer, state: StateDisconnected,
		statsTool: newDefaultStatsTool(cfg.Socket)}
}

// SetStatsTool 替换 stats 回退源（测试注入；nil 表示无回退源）。
func (m *Manager) SetStatsTool(t StatsTool) { m.statsTool = t }

// OnConnect 注册连接成功回调（含首次连接与断线重连，M3-8 恢复收敛接入点）。
// 回调在连接管理协程内同步执行，重活应自行起协程，避免阻塞状态监视。
func (m *Manager) OnConnect(fn func(version string)) { m.onConnect = fn }

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

// StatusView VPP 状态视图（/vpp/status，M3-2/M3-7）。
type StatusView struct {
	Version        string `json:"version"`
	Connected      bool   `json:"connected"`
	PendingRestart bool   `json:"pending_restart"`
	LastError      string `json:"last_error,omitempty"`
}

// SetApplied 记录已应用到 startup.conf 的 vpp 配置段（清除 pending_restart）。
func (m *Manager) SetApplied(vpp *model.VppConfig) {
	m.mu.Lock()
	m.appliedHash = VppSectionHash(vpp)
	m.mu.Unlock()
}

// AppliedHash 已应用的 vpp 配置段哈希（空 = 尚未应用过）。
func (m *Manager) AppliedHash() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.appliedHash
}

// PendingRestart committed vpp 段与已应用段不一致时为 true（FR-SYS-009）。
// 尚未应用过（appliedHash 为空）且存在 vpp 配置时也视为待重启。
func (m *Manager) PendingRestart(vpp *model.VppConfig) bool {
	m.mu.Lock()
	applied := m.appliedHash
	m.mu.Unlock()
	current := VppSectionHash(vpp)
	if applied == "" {
		return vpp != nil && current != VppSectionHash(nil)
	}
	return applied != current
}

// StatusView 汇总连接与 pending_restart（/vpp/status 数据源）。
func (m *Manager) StatusView(vpp *model.VppConfig) StatusView {
	m.mu.Lock()
	view := StatusView{
		Version:   m.version,
		Connected: m.state == StateConnected,
	}
	if m.lastErr != nil {
		view.LastError = m.lastErr.Error()
	}
	m.mu.Unlock()
	view.PendingRestart = m.PendingRestart(vpp)
	return view
}

// ConnGeneration 连接世代：每次成功建立连接（首次或重连）自增（决策 #315）。
//
// 用途：`request vpp restart` 在返回前要确认「查询真的可用」。**只看 StateConnected 不够**——
// VPP 进程重启后，govpp 检测到断连有一个窗口（健康探针间隔 1s、连续 2 次超时才报
// NotResponding），此窗口内管理器仍持有**旧会话**、状态仍是 Connected，随后任何查询都会
// 在旧 socket 上写入而得到 `write: broken pipe`（round34/35 登记的「重启返回后立即查询偶发
// broken pipe」）。世代的语义是「管理器已换成新连接」，它是把这事说清楚的唯一事实源。
func (m *Manager) ConnGeneration() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gen
}

// ConnectedSince 报告「当前会话可用，且建立于 since 之后」：已连接且世代已前进。
// 供重启路径判定「返回即可查询」（与 /vpp/status 同一管理器，决策 #314 的单一事实源）。
func (m *Manager) ConnectedSince(since uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state == StateConnected && m.gen > since
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
	m.gen++
	m.mu.Unlock()
	return ver, nil
}

// ---------- 决策 #348：nfvisd 启动时确保 VPP 运行 ----------

// SetEnsureStarter 注入发起拉起实现（nil 表示缺省 systemctl start --no-block vpp）。
func (m *Manager) SetEnsureStarter(s Starter) { m.ensureStart = s }

// SetEnsureProbe 注入「VPP 是否已在运行」探测（测试用；nil 表示缺省真实探测）。
func (m *Manager) SetEnsureProbe(fn func(context.Context) error) { m.ensureProbe = fn }

// EnsureRunning 确保 VPP 数据面在运行（决策 #348）。
//
// 背景：VPP 有意不自启（systemd 不起它），由 nfvisd/手工管理；而此前的实现只「连接失败后
// 重试」、从不拉起 ⇒ 整机重启后数据面长时间不可用，须人工 systemctl start vpp。本方法在
// nfvisd 启动装配里（连接管理 Run 之前）**调用一次**，补齐这半边：
//   - VPP 已在运行（或管理器已有可用连接）⇒ **不做任何动作**（幂等、不滥用拉起）；
//   - VPP 未运行 ⇒ **发起式**拉起一次（systemctl start --no-block vpp，提交 job 即返回）；
//   - 每次启动**只尝试一次**，失败即返回、不循环重试。
//
// **本方法绝不阻塞启动序列**：不在这里等待 VPP 就绪——就绪探测交给既有的连接重试循环
// （Run 会在 VPP 起来后连上并进入恢复收敛）。探测与发起都套有界的硬超时
// （defaultEnsureProbeTimeout / defaultEnsureStartTimeout），即便探针或 systemctl 卡住也在
// 上限内返回。本方法只保证「已在线」或「已发起拉起」，不建立连接、不改动连接状态、不谎称
// 数据面可用（可用性由连接状态体现）；发起失败时如实返回错误（带手查路径）供上层告警。
func (m *Manager) EnsureRunning(ctx context.Context) error {
	if m.State() == StateConnected {
		return nil // 已有可用连接：不滥拉
	}
	if m.probeOnce(ctx) == nil {
		m.cfg.Log.Info("VPP 已在运行，无需拉起", "socket", m.cfg.Socket)
		return nil
	}
	m.cfg.Log.Info("检测到 VPP 未运行，发起拉起（不等待）", "unit", "vpp")
	starter := m.ensureStart
	if starter == nil {
		starter = NewSystemctlStarter()
	}
	done := make(chan error, 1)
	go func() { done <- starter.Start(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			m.cfg.Log.Error("发起拉起 VPP 失败", "err", err,
				"自查", "systemctl status vpp / journalctl -u vpp")
			return fmt.Errorf("发起拉起 VPP 失败（systemctl start --no-block vpp）：%v；"+
				"请查 systemctl status vpp 与 journalctl -u vpp，或手工 systemctl start vpp", err)
		}
		m.cfg.Log.Info("已发起拉起 VPP；就绪由连接重试循环接管", "unit", "vpp")
		return nil
	case <-time.After(m.startTimeout()):
		// 发起动作异常地迟迟不返回（--no-block 正常应立即返回）：不阻塞启动，交连接重试循环。
		m.cfg.Log.Warn("发起拉起 VPP 的动作未及时返回，不阻塞启动（就绪由连接重试循环接管）", "unit", "vpp")
		return nil
	case <-ctx.Done():
		return fmt.Errorf("发起拉起 VPP 被取消：%v", ctx.Err())
	}
}

// probeOnce 做一次「VPP 是否已在运行」探测，硬超时上限 defaultEnsureProbeTimeout（可注入）：
// 探测即使卡住（govpp 连接路径不接收 context）也在此上限内返回——启动序列不被拖住。
func (m *Manager) probeOnce(ctx context.Context) error {
	pt := m.ensureProbeTimeout
	if pt <= 0 {
		pt = defaultEnsureProbeTimeout
	}
	pctx, cancel := context.WithTimeout(ctx, pt)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.ensureProbeFn()(pctx) }()
	select {
	case err := <-done:
		return err
	case <-pctx.Done():
		return fmt.Errorf("探测 VPP 是否在运行超时（%s）：%v", pt, pctx.Err())
	}
}

// startTimeout 发起拉起的防御性超时上限（<=0 取 defaultEnsureStartTimeout）。
func (m *Manager) startTimeout() time.Duration {
	if m.ensureStartTimeout > 0 {
		return m.ensureStartTimeout
	}
	return defaultEnsureStartTimeout
}

// ensureProbeFn 返回就绪探测（注入优先，缺省真实探测）。
func (m *Manager) ensureProbeFn() func(context.Context) error {
	if m.ensureProbe != nil {
		return m.ensureProbe
	}
	return m.probeRunning
}

// probeRunning 真实探测「VPP 是否已在运行」：API 套接字存在且能建立一次 binary API 连接
// （决策 #348：健康判定必须基于实际连接，但不再等待就绪——探测有硬超时，见 probeOnce）。
// 用独立连接探测，不碰连接管理器的会话。
func (m *Manager) probeRunning(ctx context.Context) error {
	if _, err := os.Stat(m.cfg.Socket); err != nil {
		return fmt.Errorf("VPP API 套接字 %s 不可用：%w", m.cfg.Socket, err)
	}
	sess, events, err := m.dialer.Dial(m.cfg.Socket, 1, 0)
	if err != nil {
		return fmt.Errorf("连接 %s：%w", m.cfg.Socket, err)
	}
	defer sess.Disconnect()
	select {
	case ev, ok := <-events:
		if !ok {
			return fmt.Errorf("连接 %s：未返回连接结果", m.cfg.Socket)
		}
		if ev.State != StateConnected {
			if ev.Err != nil {
				return fmt.Errorf("连接 %s：%v", m.cfg.Socket, ev.Err)
			}
			return fmt.Errorf("连接 %s：状态 %s", m.cfg.Socket, ev.State)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("连接 %s：%v", m.cfg.Socket, ctx.Err())
	}
}

// Run 持续维护连接：连不上时按 RetryDelay 重试（VPP 未运行时降级重试而非崩溃），
// 每次连接成功（含首次）调用 OnConnect 以触发恢复收敛；版本不匹配为致命错误，直接返回。
func (m *Manager) Run(ctx context.Context) error {
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
		if m.onConnect != nil {
			m.onConnect(ver) // 连接成功：恢复收敛（FR-OPS-010/011）
		}
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
