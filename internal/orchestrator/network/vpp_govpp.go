package network

// govpp 真实连接实现（M3-1）。本文件是唯一 import govpp 的地方，上层只见
// Dialer/Session 接口；VPP 版本锁定与重连状态机在 vpp.go，便于用假实现单测。

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"go.fd.io/govpp/adapter/socketclient"
	"go.fd.io/govpp/binapi/vpe"
	"go.fd.io/govpp/core"

	"github.com/sirupsen/logrus"
)

// govppDialer 基于 govpp 的 Dialer 实现。
type govppDialer struct{}

// NewGovppDialer 返回 govpp 连接实现（NewManager 的缺省 dialer）。
// 日志在**建立任何连接之前**设定（govpp 的 Connection/Channel 在创建时从全局 logger
// 派生自己的子 logger，之后再换就来不及了），故挂在这里。
func NewGovppDialer() Dialer {
	suppressGovppNoise()
	return govppDialer{}
}

func (govppDialer) Dial(socket string, attempts int, interval time.Duration) (Session, <-chan Event, error) {
	client := socketclient.NewVppClient(socket)
	conn, evCh, err := core.AsyncConnect(client, attempts, interval)
	if err != nil {
		return nil, nil, err
	}
	events := make(chan Event, 8)
	go func() {
		defer close(events)
		for e := range evCh {
			events <- Event{State: mapConnState(e.State), Err: e.Error}
		}
	}()
	return &govppSession{conn: conn}, events, nil
}

func mapConnState(s core.ConnectionState) State {
	switch s {
	case core.Connected:
		return StateConnected
	case core.NotResponding:
		return StateNotResponding
	case core.Failed:
		return StateFailed
	default:
		return StateDisconnected
	}
}

type govppSession struct{ conn *core.Connection }

// Version 经 VPP binary API show_version 查询（FR-SYS-007 版本锁定的数据来源）。
func (s *govppSession) Version() (string, error) {
	ch, err := s.conn.NewAPIChannel()
	if err != nil {
		return "", err
	}
	defer ch.Close()
	reply := &vpe.ShowVersionReply{}
	if err := ch.SendRequest(&vpe.ShowVersion{}).ReceiveReply(reply); err != nil {
		return "", err
	}
	if reply.Retval != 0 {
		return "", fmt.Errorf("show_version 失败: retval=%d", reply.Retval)
	}
	return reply.Version, nil
}

func (s *govppSession) Disconnect() {
	if s.conn != nil {
		s.conn.Disconnect()
	}
}

// govppNoisyReply 是 govpp 丢弃陈旧回包时的日志前缀：控制通道收到与期望序号不匹配的
// 回包时，它会把**整个结构体**按 %+v dump 成一行（`ignoring received reply: &{seqNum:…
// data:[…几百个数字…]}`）。VPP 重启后的重连会把 journal 刷满，故按这句过滤。
const govppNoisyReply = "ignoring received reply"

// govppNoiseWriter 只丢掉这一句噪音，其余（含 Error 级与「VPP is not responding」这类
// 真问题）原样写往 out——抑制噪音不等于吞错误。
type govppNoiseWriter struct{ out io.Writer }

// Write 实现 io.Writer。logrus 每条日志调用一次 Write（整行），按内容匹配即可；
// 丢弃时也返回 len(p)，否则会被 logrus 当成短写错误报出来。
func (w govppNoiseWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(govppNoisyReply)) {
		return len(p), nil
	}
	return w.out.Write(p)
}

var govppLogOnce sync.Once

// suppressGovppNoise 初始化 govpp core 的全局日志：级别与格式沿用其缺省，仅过滤上述噪音行。
func suppressGovppNoise() {
	govppLogOnce.Do(func() {
		// 操作者显式开了 govpp 调试（DEBUG_GOVPP）时不动日志：此时噪音正是他要看的
		if os.Getenv(core.DebugEnvVar) != "" {
			return
		}
		logger := logrus.New()
		logger.Out = govppNoiseWriter{out: os.Stderr}
		logger.Formatter = &logrus.TextFormatter{EnvironmentOverrideColors: true}
		core.SetLogger(logger)
	})
}

// connCache 惰性连接缓存 + 「取数失败即失效重连」策略（R84-3 修复）。
//
// 连接只在首次使用时建立并缓存；某次操作失败即视为**连接陈旧**（典型场景：VPP 重启后
// stats segment 换新，旧连接会持续失败），丢弃后重连一次再试，重连后仍失败才把错误上抛。
// 修复前连接只建一次且从不失效，VPP 重启后接口统计**永久**不可用，只能重启 nfvisd 才恢复。
//
// 句柄类型参数化、不引用 govpp 具体类型，是为了让这段状态机可在任意平台用假 connect/close
// 单测（见 vpp_govpp_test.go）；真实句柄为 stats_linux.go 的 *core.StatsConnection。
// 之所以放在本文件（无 build tag）而不是 stats_linux.go，就是不让它跟着 stats 一起变成
// 只能在 Linux 编译——那样开发机（Windows）就跑不到这段策略的单测。
type connCache[T any] struct {
	mu      sync.Mutex
	handle  T
	ready   bool
	connect func() (T, error)
	closeFn func(T)
}

// newConnCache 构造连接缓存。connect 负责建立连接；closeFn 负责关闭被丢弃的连接（可为 nil）。
func newConnCache[T any](connect func() (T, error), closeFn func(T)) *connCache[T] {
	return &connCache[T]{connect: connect, closeFn: closeFn}
}

// errConnCacheTimeout useBounded 的硬上界信号：某次 fn 在 bound 内没跑完，
// 句柄已被关闭、缓存已清（错误里用 %w 包装它，调用方/单测可 errors.Is 辨识）。
var errConnCacheTimeout = errors.New("连接读取超时")

// use 在缓存连接上执行 fn；fn 失败视为连接陈旧，失效重连一次后重试。
// 整段持锁：既避免并发重建连接，也避免半关闭的连接被并发使用。
func (c *connCache[T]) use(fn func(T) error) error {
	return c.useInternal(0, fn)
}

// useBounded 与 use 同语义，但**单次 fn 执行**另有硬上界（决策 #362）：
// fn 放进 goroutine（缓冲结果通道），上界到即 closeFn 关闭当前句柄（中断可能永久
// 挂起的读）+ 清缓存，并返回包装 errConnCacheTimeout 的错误。该路径**不重连重试**
// ——上界语义优先，避免再叠一个上界；句柄已清，下一次调用走 ensureLocked 重建。
//
// fn 的普通失败仍沿用 use 的既有策略（R84-3：取数失败即失效重连一次再试），
// 只是每段执行各受一个 bound 约束。bound<=0 表示不加硬上界（退回 use 语义）。
//
// 句柄类型参数化、不引用 govpp 具体类型，故本段逻辑在开发机（Windows）也能单测。
func (c *connCache[T]) useBounded(bound time.Duration, fn func(T) error) error {
	return c.useInternal(bound, fn)
}

// useInternal use/useBounded 的公共实现：run 负责在（可选）硬上界内执行一次 fn。
// 整段持锁（含上界等待）：既保持既有串行，也保证清缓存/重建不与并发调用交错。
func (c *connCache[T]) useInternal(bound time.Duration, fn func(T) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, err := c.ensureLocked()
	if err != nil {
		return err
	}
	firstErr := c.runBoundedLocked(h, bound, fn)
	if firstErr == nil {
		return nil
	}
	if errors.Is(firstErr, errConnCacheTimeout) {
		// 上界已到：句柄已在 runBoundedLocked 内关闭并清缓存，不再重连重试。
		return firstErr
	}
	c.dropLocked()
	h2, err := c.ensureLocked()
	if err != nil {
		// 重连也失败：两条错误一起上抛（调用方只判可用性，日志里能看到全貌）
		return fmt.Errorf("连接重建失败: %w（首次失败：%v）", err, firstErr)
	}
	return c.runBoundedLocked(h2, bound, fn)
}

// runBoundedLocked 在 h 上执行一次 fn（调用方持锁）；bound>0 时 fn 在 goroutine 内
// 执行并受硬上界约束。上界到 ⇒ 关闭并丢弃句柄（缓存清空，下次调用重建）并返回
// 包装 errConnCacheTimeout 的错误；goroutine 的结果送带缓冲通道后退出（不阻塞、不泄漏）。
func (c *connCache[T]) runBoundedLocked(h T, bound time.Duration, fn func(T) error) error {
	if bound <= 0 {
		return fn(h)
	}
	done := make(chan error, 1)
	go func() { done <- fn(h) }()
	t := time.NewTimer(bound)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
		c.dropLocked()
		return fmt.Errorf("读取 VPP 统计超时（%s）：已关闭并丢弃挂起连接，下次调用重建（%w）", bound, errConnCacheTimeout)
	}
}

// ensureLocked 复用已建立的连接，没有则建立（调用方持锁）。
func (c *connCache[T]) ensureLocked() (T, error) {
	if c.ready {
		return c.handle, nil
	}
	h, err := c.connect()
	if err != nil {
		var zero T
		return zero, err
	}
	c.handle, c.ready = h, true
	return h, nil
}

// dropLocked 丢弃缓存连接，下次 use 重新建立（调用方持锁）。
// 关闭失败无需处理：连接已不再复用，错误只说明它早就坏了。
func (c *connCache[T]) dropLocked() {
	if !c.ready {
		return
	}
	var zero T
	h := c.handle
	c.handle, c.ready = zero, false
	if c.closeFn != nil {
		c.closeFn(h)
	}
}
