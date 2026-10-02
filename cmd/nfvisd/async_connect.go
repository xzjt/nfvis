package main

// 底座编排的启动后异步接入循环（决策 #351）。
//
// 开机时 libvirtd 要先完成 autostart 才服务客户端握手（round130 实测窗口 ≈90s），
// nfvisd 装配期的单次 10s 有界尝试大概率落在窗口内而失败——失败后快路径照旧降级 +
// 告警（决策 #349），另起本循环兜底：每 30s 一次接入尝试（每次沿用 10s 上界），
// 重试静默（不逐次落日志，避免 journal 刷屏）。接入成功即 onConnected **恰好一次**
//（内含原子换装、INFO 日志、消解对应告警、补跑一次该底座的 EnsureConsistent），
// 随后循环终结——本决策只服务「从未接入」状态，接入成功后连接中断的自动重连不在范围。

import (
	"context"
	"log/slog"
	"time"
)

// 后台接入的节奏常量：30s 节奏 × 每次 10s 有界（与装配期快路径的上界同一语义）；
// 接入成功后的 EnsureConsistent 补跑给 30s（与 runRecovery 的收敛上界一致）。
const (
	asyncConnectInterval       = 30 * time.Second
	asyncConnectAttemptTimeout = 10 * time.Second
	asyncConnectEnsureTimeout  = 30 * time.Second
)

// runAsyncConnect 后台接入循环的驱动（可单测：注入 interval/connect/onConnected）。
//
// 语义（决策 #351 契约）：
//   - **第一次尝试前先等一个 interval**（快路径同步尝试刚刚失败过，立即重试只会立刻
//     再失败一次；统一 30s 节奏——这不是「先试一次再等」的等价写法）；
//   - 尝试失败 ⇒ **静默继续**（不逐次落日志——契约明文）；
//   - 尝试成功 ⇒ 以 asyncConnectEnsureTimeout 上界的 ctx 调 onConnected 恰好一次，
//     随后循环终结；
//   - ctx 取消 ⇒ 退出（不调 onConnected）。
//
// log 参数为调用方签名对称保留：失败静默、成功由 onConnected 落日志，本函数自身不落。
func runAsyncConnect(ctx context.Context, interval time.Duration, log *slog.Logger,
	connect func(ctx context.Context) error, onConnected func(ctx context.Context)) {
	if interval <= 0 {
		interval = asyncConnectInterval
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		if err := connect(ctx); err != nil {
			continue // 失败静默（契约：不逐次落日志）
		}
		ectx, cancel := context.WithTimeout(ctx, asyncConnectEnsureTimeout)
		onConnected(ectx)
		cancel()
		return
	}
}
