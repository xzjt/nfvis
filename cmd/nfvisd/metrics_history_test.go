package main

// 决策 #356：历史采样循环的可注入接缝单测——间隔取自 committed 配置、采集失败只记
// last_error 不中断循环、裁剪按时到点触发。

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/metrics"
	"github.com/xzjt/nfvis/internal/metricshist"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

func newSamplerStore(t *testing.T) *metricshist.Store {
	t.Helper()
	st, err := metricshist.Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatalf("打开历史库: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// newSamplerEngine 建真实引擎并提交带 metrics.history 的配置。
func newSamplerEngine(t *testing.T, interval, retention int) *config.Engine {
	t.Helper()
	store, err := config.OpenStore(filepath.Join(t.TempDir(), "nfvis.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	engine, err := config.NewEngine(store, orchestrator.NewNoopApplier(), config.Options{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(engine.Close)
	sess := config.Session{User: "system", Source: "console"}
	if err := engine.Edit(sess); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	cfg := model.Config{System: &model.SystemConfig{Metrics: &model.MetricsConfig{
		History: &model.MetricsHistoryConfig{IntervalSeconds: interval, RetentionDays: retention},
	}}}
	if err := engine.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	if _, err := engine.Commit(context.Background(), sess, config.CommitOpts{AllowNoSuperUser: true}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	_ = engine.Release(sess)
	return engine
}

func TestMetricsHistoryRunnerIntervalFromConfig(t *testing.T) {
	st := newSamplerStore(t)
	eng := newSamplerEngine(t, 30, 3)

	var slept []time.Duration
	calls := 0
	r := &metricsHistoryRunner{
		store:  st,
		engine: eng,
		now:    time.Now,
		sleep: func(ctx context.Context, d time.Duration) bool {
			slept = append(slept, d)
			calls++
			return calls <= 61 // 决策 #372：分片睡眠——61 片覆盖 2 个 30s 周期后退出
		},
		gather: func(ctx context.Context) ([]metrics.Sample, error) {
			return []metrics.Sample{{Name: "nfvis_t", Value: 1}}, nil
		},
		pruneEvery:    time.Hour,
		maxRows:       metricshist.MaxRows,
		errorEvery:    0,
		gatherTimeout: time.Second,
	}
	r.run(context.Background())

	// 决策 #372（R142 A2）：睡眠按 ≤1s 分片、每片重读间隔——30s 间隔 = 30 片一次 tick。
	if len(slept) != 62 {
		t.Fatalf("应睡眠 62 次（61 次成功 + 1 次取消）后退出，实际 %d 次", len(slept))
	}
	for i, d := range slept {
		if d > time.Second {
			t.Fatalf("第 %d 片睡眠 %v 超过分片上限 1s（间隔改小将无法及时生效）", i, d)
		}
	}
	lastTick, lastErr, err := st.Health()
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if lastTick == 0 || lastErr != "" {
		t.Fatalf("成功采样后应有心跳且无错误，得到 lastTick=%d lastErr=%q", lastTick, lastErr)
	}
	if stats, _ := st.Stats(); stats.Samples != 2 {
		t.Fatalf("两次成功采样应写 2 行，实际 %d", stats.Samples)
	}
}

func TestMetricsHistoryRunnerGatherFailureKeepsLooping(t *testing.T) {
	st := newSamplerStore(t)
	eng := newSamplerEngine(t, 10, 7)

	sleeps := 0
	r := &metricsHistoryRunner{
		store:  st,
		engine: eng,
		now:    time.Now,
		sleep: func(ctx context.Context, d time.Duration) bool {
			sleeps++
			return sleeps <= 11 // 10 片 = 1 个 10s 周期（触发一次失败采集）+ 1 次取消
		},
		gather: func(ctx context.Context) ([]metrics.Sample, error) {
			return nil, errors.New("底座不可达")
		},
		pruneEvery:    time.Hour,
		maxRows:       metricshist.MaxRows,
		errorEvery:    0,
		gatherTimeout: time.Second,
	}
	r.run(context.Background())

	if sleeps != 12 {
		t.Fatalf("采集失败不应终止循环（应睡 12 次退出），实际 %d", sleeps)
	}
	_, lastErr, err := st.Health()
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !strings.Contains(lastErr, "采集失败") || !strings.Contains(lastErr, "底座不可达") {
		t.Fatalf("采集失败应如实记入 last_error，得到 %q", lastErr)
	}
}

func TestMetricsHistoryRunnerPrunesOnSchedule(t *testing.T) {
	st := newSamplerStore(t)
	eng := newSamplerEngine(t, 30, 1)

	base := time.Now()
	clock := base
	// 先种一条远超保留窗（1 天）的旧样本。
	if err := st.Append(base.Add(-48*time.Hour).Unix(), []metricshist.Sample{{Name: "old", Value: 1}}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	calls := 0
	r := &metricsHistoryRunner{
		store:  st,
		engine: eng,
		now:    func() time.Time { return clock },
		sleep: func(ctx context.Context, d time.Duration) bool {
			clock = clock.Add(d) // 假时钟随睡眠前进
			calls++
			return calls <= 151 // 150 片 = 5 个 30s 周期（第 3 轮到点裁剪）
		},
		gather: func(ctx context.Context) ([]metrics.Sample, error) {
			return []metrics.Sample{{Name: "nfvis_t", Value: 1}}, nil
		},
		pruneEvery:    90 * time.Second, // 每 3 轮（30s×3）触发一次
		maxRows:       metricshist.MaxRows,
		errorEvery:    0,
		gatherTimeout: time.Second,
	}
	r.run(context.Background())

	// 5 轮采集写入 5 行；旧样本在到点裁剪时被删除 ⇒ 总数应为 5（若从未裁剪会是 6）。
	stats, err := st.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Samples != 5 {
		t.Fatalf("到点裁剪应删掉超窗旧样本（期望 5 行，实际 %d）", stats.Samples)
	}
}

// 决策 #397（R171-17）：采样循环**不得每秒**读一次 committed 配置——旧实现分片睡眠每片都
// engine.Committed()（持 engine.mu 做 DB 读 + 全配置反序列化），与所有 CLI/API 配置操作互斥。
// 用注入的 intervalFn 计数：30 片（30s）内重读应 ≤8 次（约每 5s 一次），旧实现会读 31 次。
func TestMetricsHistoryRunnerDoesNotReadIntervalEverySecond(t *testing.T) {
	st := newSamplerStore(t)
	clock := time.Unix(1_700_000_000, 0)
	calls, reads := 0, 0
	r := &metricsHistoryRunner{
		store: st,
		now:   func() time.Time { return clock },
		sleep: func(ctx context.Context, d time.Duration) bool {
			clock = clock.Add(d)
			calls++
			return calls <= 30 // 30 片 = 30s
		},
		intervalFn: func() int { reads++; return 3600 },
		gather: func(ctx context.Context) ([]metrics.Sample, error) {
			return []metrics.Sample{{Name: "nfvis_t", Value: 1}}, nil
		},
		pruneEvery:    time.Hour,
		maxRows:       metricshist.MaxRows,
		errorEvery:    0,
		gatherTimeout: time.Second,
	}
	r.run(context.Background())

	if reads == 0 {
		t.Fatal("间隔至少应读取一次")
	}
	if reads > 8 {
		t.Fatalf("30 片内间隔重读 %d 次（旧实现每秒一次=31）；降频后应 ≤8", reads)
	}
}

// （旧实现会先睡满 3600s，读视图按新间隔算 stale 阈值 ⇒ 假 stale）。
func TestMetricsHistoryRunnerReschedulesOnIntervalShrink(t *testing.T) {
	st := newSamplerStore(t)
	eng := newSamplerEngine(t, 3600, 7)

	clock := time.Unix(1_700_000_000, 0)
	base := clock
	calls := 0
	changed := false
	var gatherAt []time.Duration
	r := &metricsHistoryRunner{
		store:  st,
		engine: eng,
		now:    func() time.Time { return clock },
		sleep: func(ctx context.Context, d time.Duration) bool {
			clock = clock.Add(d)
			calls++
			// 第 5 片时把间隔从 3600s 改成 10s（模拟操作者改配置）
			if calls == 5 && !changed {
				changed = true
				sess := config.Session{User: "system", Source: "console"}
				if err := eng.Edit(sess); err != nil {
					t.Fatalf("Edit: %v", err)
				}
				cfg := model.Config{System: &model.SystemConfig{Metrics: &model.MetricsConfig{
					History: &model.MetricsHistoryConfig{IntervalSeconds: 10, RetentionDays: 7},
				}}}
				if err := eng.UpdateCandidate(sess, cfg); err != nil {
					t.Fatalf("UpdateCandidate: %v", err)
				}
				if _, err := eng.Commit(context.Background(), sess, config.CommitOpts{AllowNoSuperUser: true}); err != nil {
					t.Fatalf("Commit: %v", err)
				}
				_ = eng.Release(sess)
			}
			return calls <= 30 // 30 片后退出
		},
		gather: func(ctx context.Context) ([]metrics.Sample, error) {
			gatherAt = append(gatherAt, clock.Sub(base)) // 记录每次采集的时钟（相对基准）
			return []metrics.Sample{{Name: "nfvis_t", Value: 1}}, nil
		},
		pruneEvery:    time.Hour,
		maxRows:       metricshist.MaxRows,
		errorEvery:    0,
		gatherTimeout: time.Second,
	}
	r.run(context.Background())

	// 第 5 片（t=+5s）改小间隔后，下一片起按 10s 计时 ⇒ **首个采集应在改后 ≤10s**
	// （旧实现会先睡满 3600s——本用例即钉住该回归）。
	if len(gatherAt) == 0 {
		t.Fatal("间隔调小后应有采集发生")
	}
	if gatherAt[0] > 15*time.Second {
		t.Fatalf("首个采集应在改间隔（+5s）后 ≤10s 内发生，实际 +%v（旧实现 +3600s）", gatherAt[0])
	}
}
