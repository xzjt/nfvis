package main

// 底座编排的进程生命周期常驻驱动（决策 #351 引入、决策 #354 状态机化）。
//
// #351 的后台接入循环是「失败才起、成功即终结」的一次性循环，只服务「从未接入」状态：
// 开机时 libvirtd 要先完成 autostart 才服务客户端握手（round130 实测窗口 ≈90s），
// nfvisd 装配期的单次 10s 有界尝试大概率落在窗口内而失败——失败后快路径照旧降级 +
// 告警（决策 #349），另起循环兜底。但**接入成功后**连接中断（libvirtd 重启/崩溃/楔死）
// 仍是死路：既有 *Conn 已断、调用全线报错、无自动恢复（重启 nfvis 是唯一出路）。
//
// #354 把驱动扩展为常驻状态机（每个底座一个进程生命周期 goroutine）：
//
//	未接入 ──每 30s 一次有界接入尝试（10s 上界，失败静默）──▶ 接入成功
//	                                                        │ 换装 + onFirstConnected
//	                                                        ▼
//	已接入 ◀──每 15s 探活（5s 上界）；成功清零失败计数──┬── 连续 2 次失败 ⇒ 判连接中断
//	                                                   │     onLost（运行期告警，同码）
//	                                                   ▼
//	                                          复连段：≤3 次尝试（10s 上界、1s 退避）
//	                                            成功 ⇒ 换装 + onReconnected（消警+Ensure）
//	                                            失败 ⇒ 告警保持，退回「未接入」态继续
//	                                                   30s 节奏（此后一直如此，永不放弃）
//
// 全程 ctx.Done() 即退出。探活是只读 libvirt RPC，不取 recoveryMu（与 15s 巡检使用的
// VPP API 锁不冲突）；接入/复连成功后的收尾（含 EnsureConsistent）由装配层回调在
// recoveryMu 内执行（照 #351 的锁纪律）。驱动的 interval/timeout/阈值与 connect/probe/
// 回调全部可注入，单测可缩到毫秒级，不依赖真实时钟硬编码。
//
// Docker 侧不需要重连（HTTP over unix socket 无会话态，dockerd 重启后既有 client 自然
// 恢复——决策 #354 实测语义，如实登记），保留 runAsyncConnect 的「接入成功即终结」
// 一次性窄包装（见文件末尾）。

import (
	"context"
	"log/slog"
	"time"
)

// 状态机节奏常量。
//
// 未接入态沿用 #351 的三个取值（asyncConnectInterval 即契约里的“未接入重试节奏”）；
// 已接入态与中断处置的取值来自决策 #354 契约②③。
const (
	// 未接入态：每 30s 一次接入尝试、每次 10s 上界；接入成功后回调的 ctx 给 30s
	// （与 runRecovery 的收敛上界一致）。
	asyncConnectInterval       = 30 * time.Second
	asyncConnectAttemptTimeout = 10 * time.Second
	asyncConnectEnsureTimeout  = 30 * time.Second

	// 已接入态：每 15s 一次轻量探活、每次 5s 上界；连续 2 次失败才判中断
	// （避免单次抖动误判）。
	connectedProbeInterval = 15 * time.Second
	probeTimeout           = 5 * time.Second
	probeFailThreshold     = 2

	// 复连段：既有有界尝试口径（10s × 至多 3 次、尝试间 1s 退避）。
	asyncConnectAttempts = 3
	asyncConnectBackoff  = 1 * time.Second
)

// watchOptions 常驻驱动的可注入参数（生产用 defaultWatchOptions；单测缩到毫秒级）。
type watchOptions struct {
	retryInterval  time.Duration // 未接入态：接入尝试节奏
	attemptTimeout time.Duration // 单次接入/复连尝试上界
	attempts       int           // 一次复连段的有界尝试次数
	backoff        time.Duration // 复连尝试之间的退避
	probeInterval  time.Duration // 已接入态：探活节奏
	probeTimeout   time.Duration // 单次探活上界
	failThreshold  int           // 连续探活失败判中断的阈值
	ensureTimeout  time.Duration // 接入成功回调（EnsureConsistent 等）的 ctx 上界
}

// defaultWatchOptions 生产取值（决策 #354 契约②③）。
func defaultWatchOptions() watchOptions {
	return watchOptions{
		retryInterval:  asyncConnectInterval,
		attemptTimeout: asyncConnectAttemptTimeout,
		attempts:       asyncConnectAttempts,
		backoff:        asyncConnectBackoff,
		probeInterval:  connectedProbeInterval,
		probeTimeout:   probeTimeout,
		failThreshold:  probeFailThreshold,
		ensureTimeout:  asyncConnectEnsureTimeout,
	}
}

// normalized 零值/负值回落生产默认（单测只填关心的项也不会得到 0 间隔死循环）。
func (o watchOptions) normalized() watchOptions {
	d := defaultWatchOptions()
	if o.retryInterval <= 0 {
		o.retryInterval = d.retryInterval
	}
	if o.attemptTimeout <= 0 {
		o.attemptTimeout = d.attemptTimeout
	}
	if o.attempts <= 0 {
		o.attempts = d.attempts
	}
	if o.backoff <= 0 {
		o.backoff = d.backoff
	}
	if o.probeInterval <= 0 {
		o.probeInterval = d.probeInterval
	}
	if o.probeTimeout <= 0 {
		o.probeTimeout = d.probeTimeout
	}
	if o.failThreshold <= 0 {
		o.failThreshold = d.failThreshold
	}
	if o.ensureTimeout <= 0 {
		o.ensureTimeout = d.ensureTimeout
	}
	return o
}

// baseWatchDeps 常驻驱动的注入面（装配层给生产实现；单测注入计数假件）。
type baseWatchDeps struct {
	log *slog.Logger
	// connect 一次有界接入/复连尝试：成功时已完成换装（旧连接关闭、新连接记账）；
	// 驱动传入的 ctx 已按 attemptTimeout 上界。
	connect func(ctx context.Context) error
	// probe 轻量探活：nil = 连接健康，非 nil = 本次探活失败；
	// 驱动传入的 ctx 已按 probeTimeout 上界。
	probe func(ctx context.Context) error
	// onFirstConnected 首次接入成功后的收尾（INFO + 消警 + EnsureConsistent 恰一次）。
	onFirstConnected func(ctx context.Context)
	// onReconnected 中断后复连成功的收尾（同上，INFO 文案为「已恢复」）。
	onReconnected func(ctx context.Context)
	// onLost 连续探活失败判定连接中断：raise 运行期告警（同码同 scope/source 键）。
	onLost func(err error)
}

// runBaseWatch 常驻状态机的驱动（决策 #354）。startConnected = 装配期快路径是否已接入：
// true 直接进入探活态，false 从「每 retryInterval 一次有界接入尝试」开始。
// 两个形态都常驻到 ctx 结束——接入成功不返回、复连失败也不返回（永不放弃）。
func runBaseWatch(ctx context.Context, startConnected bool, opts watchOptions, deps baseWatchDeps) {
	opts = opts.normalized()
	if deps.log == nil {
		deps.log = slog.Default()
	}
	everConnected := startConnected
	connected := startConnected

	for ctx.Err() == nil {
		if !connected {
			// 未接入态：先等一个节奏再做一次有界接入尝试（快路径刚失败过，立即重试
			// 只会立刻再失败一次——这不是「先试一次再等」的等价写法，照 #351）。
			if !sleepCtx(ctx, opts.retryInterval) {
				return
			}
			if err := withTimeout(ctx, opts.attemptTimeout, deps.connect); err != nil {
				continue // 失败静默（契约：不逐次落日志，避免 journal 刷屏）
			}
			if ctx.Err() != nil {
				return // 停机取消：换装可能已发生，但不再跑收尾回调
			}
			notifyConnected(ctx, opts, deps, everConnected)
			everConnected, connected = true, true
			continue
		}

		// 已接入态：探活；连续 failThreshold 次失败 ⇒ 判中断并进复连段。
		fails := 0
		for connected && ctx.Err() == nil {
			if !sleepCtx(ctx, opts.probeInterval) {
				return
			}
			err := withTimeout(ctx, opts.probeTimeout, deps.probe)
			if ctx.Err() != nil {
				return // 停机取消不判失败、不告警、不复连
			}
			if err == nil {
				fails = 0 // 探活成功：阈值计数清零（抖动不累积）
				continue
			}
			fails++
			if fails < opts.failThreshold {
				continue // 单次失败不动作
			}
			if deps.onLost != nil {
				deps.onLost(err) // 运行期告警（raise；同码同 scope/source）
			}
			deps.log.Warn("底座连接中断，开始自动复连", "err", err)
			ok, lastErr := reconnectBounded(ctx, opts, deps)
			if ctx.Err() != nil {
				return
			}
			if !ok {
				// 复连段失败：告警保持（不重复 raise——同键语义即“仍在场”），
				// 退回未接入态继续按 30s 节奏重试（此后一直如此，永不放弃）。
				deps.log.Warn("自动复连未成功，告警保持；将继续按接入节奏重试", "err", lastErr)
				connected, fails = false, 0
				break
			}
			notifyConnected(ctx, opts, deps, true)
			everConnected = true
			fails = 0
		}
	}
}

// reconnectBounded 中断后的有界复连段：attempts 次尝试、每次 attemptTimeout 上界、
// 尝试间 backoff 退避。返回是否成功与最后一次错误。
func reconnectBounded(ctx context.Context, opts watchOptions, deps baseWatchDeps) (bool, error) {
	var lastErr error
	for i := 0; i < opts.attempts; i++ {
		if i > 0 && !sleepCtx(ctx, opts.backoff) {
			return false, lastErr
		}
		if ctx.Err() != nil {
			return false, lastErr
		}
		lastErr = withTimeout(ctx, opts.attemptTimeout, deps.connect)
		if lastErr == nil {
			return true, nil
		}
	}
	return false, lastErr
}

// notifyConnected 接入/复连成功后的收尾回调（ctx 以 ensureTimeout 为上界）。
// reconnected 区分首接与复连：装配层用它选 INFO 文案（消警与 EnsureConsistent 共用实现）。
func notifyConnected(ctx context.Context, opts watchOptions, deps baseWatchDeps, reconnected bool) {
	ectx, cancel := context.WithTimeout(ctx, opts.ensureTimeout)
	defer cancel()
	if reconnected {
		if deps.onReconnected != nil {
			deps.onReconnected(ectx)
		}
		return
	}
	if deps.onFirstConnected != nil {
		deps.onFirstConnected(ectx)
	}
}

// withTimeout 以 timeout 为上界执行一次注入调用（<=0 表示不另加上界）。
//
// 说明：上界作用在调用方 ctx 上；对 libvirt 而言 go-libvirt 的单次 RPC 没有 per-call
// deadline，探活/接入的实际中断点由 Conn 层（连接握手有界、RPC 返回错误）保证——
// 「守护进程已停/已重启」这类中断会立即返回错误，这是本决策覆盖的主场景。
func withTimeout(ctx context.Context, timeout time.Duration, fn func(context.Context) error) error {
	if timeout <= 0 {
		return fn(ctx)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return fn(cctx)
}

// sleepCtx 等待 d 或 ctx 结束；返回 false 表示 ctx 已结束（调用方应立即退出）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// runAsyncConnect 「未接入才接入、接入成功即终结」的一次性后台接入循环（决策 #351
// 语义保留；Docker 侧专用——决策 #354 明文 Docker 无需重连）。
//
// 现为常驻驱动的窄包装：首接成功回调里取消驱动 ctx，故只走「未接入 → 接入成功」
// 一段；探活/复连不会发生（probe/onReconnected/onLost 仅为满足驱动接口）。
// 参数与语义与 #351 逐条一致：首次尝试前先等一个 interval（interval<=0 取默认）、
// 失败静默、成功 ⇒ onConnected 恰一次后返回、ctx 取消 ⇒ 退出。
//
// log 参数为调用方签名对称保留（成功由 onConnected 落日志，本函数自身不落）。
func runAsyncConnect(ctx context.Context, interval time.Duration, log *slog.Logger,
	connect func(ctx context.Context) error, onConnected func(ctx context.Context)) {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	runBaseWatch(wctx, false, watchOptions{retryInterval: interval}, baseWatchDeps{
		log:     log,
		connect: connect,
		probe:   func(context.Context) error { return nil },
		onFirstConnected: func(ctx context.Context) {
			onConnected(ctx)
			cancel() // 接入成功即终结（#351 一次性语义）
		},
		onReconnected: func(context.Context) {},
		onLost:        func(error) {},
	})
}
