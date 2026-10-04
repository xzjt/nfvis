package main

// 历史时序后台采样器（决策 #356，FR-SYS-005）。
//
// 与 `/metrics` **共用同一采集路径**（api.Server.GatherSamples）——能 scrape 到的指标
// 就是能画出趋势的指标，不维护第二套清单。采样落**独立** SQLite 库（不与配置库的
// 事务/候选锁竞争，也不进配置备份/恢复语义）。
//
// 纪律（沿用 #348/#351/#354）：本 goroutine 在服务 READY 之后启动，**不阻塞启动**；
// 间隔每轮从 committed 配置重读（改配置无需重启即生效）；整轮采集失败只记 last_error
// 并下轮重试（可得的序列照写、不可得的当轮不写——与 /metrics 同口径「宁缺不谎报」）；
// 采样停滞只在读视图如实呈现，v1 不设告警码。裁剪每 5 分钟一次（计数/到时判断，不另起
// 定时器），裁剪后 store 内部回读事实（写成功 ≠ 收敛）。

import (
	"context"
	"log/slog"
	"time"

	"github.com/xzjt/nfvis/internal/api"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/metrics"
	"github.com/xzjt/nfvis/internal/metricshist"
	"github.com/xzjt/nfvis/internal/model"
)

const (
	// metricsGatherTimeout 单轮采集的有界上界（编排不可用时不让采样线程挂死）。
	metricsGatherTimeout = 5 * time.Second
	// metricsPruneEvery 裁剪节奏（每 5 分钟一次，按到时判断，不新造定时器）。
	metricsPruneEvery = 5 * time.Minute
	// metricsErrorLogEvery 采样错误日志节流窗口（避免 journal 刷屏；读视图始终能查到 last_error）。
	metricsErrorLogEvery = 5 * time.Minute
)

// runMetricsHistory 后台历史采样循环（决策 #356）。ctx 取消即优雅退出（睡眠可被中断）。
func runMetricsHistory(ctx context.Context, store *metricshist.Store, engine *config.Engine, srv *api.Server, log *slog.Logger) {
	if store == nil || srv == nil {
		return
	}
	r := &metricsHistoryRunner{
		store:         store,
		engine:        engine,
		log:           log,
		now:           time.Now,
		sleep:         sleepWithContext,
		gather:        func(ctx context.Context) ([]metrics.Sample, error) { return srv.GatherSamples(ctx), nil },
		pruneEvery:    metricsPruneEvery,
		maxRows:       metricshist.MaxRows,
		errorEvery:    metricsErrorLogEvery,
		gatherTimeout: metricsGatherTimeout,
	}
	r.run(ctx)
}

// metricsHistoryRunner 采样循环（可注入 now/sleep/gather 便于单测）。
type metricsHistoryRunner struct {
	store  *metricshist.Store
	engine *config.Engine
	log    *slog.Logger

	now    func() time.Time
	sleep  func(ctx context.Context, d time.Duration) bool
	gather func(ctx context.Context) ([]metrics.Sample, error)

	pruneEvery    time.Duration
	maxRows       int64
	errorEvery    time.Duration
	gatherTimeout time.Duration

	lastErrLog time.Time
}

// run 循环：每轮先读生效间隔并按其睡眠（可被 ctx 中断），再采集；到点裁剪。
func (r *metricsHistoryRunner) run(ctx context.Context) {
	lastPrune := r.now()
	for {
		interval := r.intervalSeconds()
		if !r.sleep(ctx, time.Duration(interval)*time.Second) {
			return // ctx 取消
		}
		r.tick(ctx)
		if r.now().Sub(lastPrune) >= r.pruneEvery {
			r.prune()
			lastPrune = r.now()
		}
	}
}

// tick 一轮采集 + 落库 + 心跳。失败记 last_error（节流日志），循环继续。
func (r *metricsHistoryRunner) tick(ctx context.Context) {
	// 决策 #365：库文件被删/替换（format-data / rm）时重开空库——旧连接持已删 inode 会让
	// 「已清掉」只停在 ls 层面（读数与空间都还在旧 inode 上）。
	if err := r.store.ReopenIfReplaced(); err != nil {
		r.reportError("重开历史库失败", err)
		return
	}
	now := r.now()
	gctx, cancel := context.WithTimeout(ctx, r.gatherTimeout)
	samples, err := r.gather(gctx)
	cancel()
	if err != nil {
		r.reportError("采集失败", err)
		return
	}
	if err := r.store.Append(now.Unix(), toHistorySamples(samples)); err != nil {
		r.reportError("写入历史库失败", err)
		return
	}
	// 本轮成功：SetLastTick 同时清空上一次的 last_error（store 内保证）。
	if err := r.store.SetLastTick(now.Unix()); err != nil && r.log != nil {
		r.log.Warn("历史采样心跳写入失败", "err", err)
	}
}

// prune 裁剪到时间窗 + 硬行顶（store 内部回读事实）。
func (r *metricsHistoryRunner) prune() {
	cutoff := r.now().Add(-time.Duration(r.retentionDays()) * 24 * time.Hour).Unix()
	if _, err := r.store.Prune(cutoff, r.maxRows); err != nil {
		r.reportError("裁剪历史库失败", err)
	}
}

// reportError 记 last_error（读视图如实呈现）并按窗口节流日志。
func (r *metricsHistoryRunner) reportError(stage string, err error) {
	_ = r.store.SetLastError(stage + "：" + err.Error())
	if r.log == nil {
		return
	}
	now := r.now()
	if now.Sub(r.lastErrLog) < r.errorEvery {
		return
	}
	r.lastErrLog = now
	r.log.Warn("历史采样未完成（下轮重试；用 show system metrics history 可查上次错误）", "stage", stage, "err", err)
}

// intervalSeconds 生效的采样间隔（committed 配置；读不到回落模型默认）——每轮重读，
// 故改配置无需重启即生效。
func (r *metricsHistoryRunner) intervalSeconds() int {
	if r.engine == nil {
		return model.MetricsIntervalDefaultSeconds
	}
	cfg, err := r.engine.Committed()
	if err != nil {
		return model.MetricsIntervalDefaultSeconds
	}
	return cfg.MetricsHistoryIntervalSeconds()
}

// retentionDays 生效的保留天数（committed 配置；读不到回落模型默认）。
func (r *metricsHistoryRunner) retentionDays() int {
	if r.engine == nil {
		return model.MetricsRetentionDaysDefault
	}
	cfg, err := r.engine.Committed()
	if err != nil {
		return model.MetricsRetentionDaysDefault
	}
	return cfg.MetricsHistoryRetentionDays()
}

// toHistorySamples api/metrics.Sample → metricshist.Sample（同口径：仅取 name/labels/value）。
func toHistorySamples(in []metrics.Sample) []metricshist.Sample {
	out := make([]metricshist.Sample, 0, len(in))
	for _, s := range in {
		out = append(out, metricshist.Sample{Name: s.Name, Labels: s.Labels, Value: s.Value})
	}
	return out
}

// sleepWithContext 可被 ctx 中断的睡眠（避免长时间不可打断的 Sleep 拖慢优雅停机）。
// 返回 true=睡满，false=被取消。
func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
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
