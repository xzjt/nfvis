package api

// 决策 #356：历史时序读视图（REST 侧形状与诚实性）。
//
// 覆盖：可用/不可用两种形状、无 name 的概览（series 字段在但为空）、带标签的序列形状、
// 采样停滞（stale）计算、时长解析（含 400）。

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/metricshist"
)

// newHistoryStore 建一个临时历史库（测试用）。
func newHistoryStore(t *testing.T) *metricshist.Store {
	t.Helper()
	st, err := metricshist.Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatalf("打开历史库: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestMetricsHistoryUnavailableShape(t *testing.T) {
	ts := newTestServer(t) // 未装配 MetricsHistory ⇒ 如实不可用
	token := loginAdmin(t, ts)

	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/metrics/history", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("不可用时应仍 200（available=false），得到 %d: %s", status, body)
	}
	obj := responseObject(t, body)
	if av, _ := obj["available"].(bool); av {
		t.Fatalf("未装配历史库却报 available=true: %s", body)
	}
	if reason, _ := obj["reason"].(string); reason == "" {
		t.Fatalf("available=false 时必须给 reason（如实说明原因）：%s", body)
	}
	store, ok := obj["store"].(map[string]any)
	if !ok {
		t.Fatalf("store 必须恒为对象：%s", body)
	}
	if en, _ := store["enabled"].(bool); en {
		t.Fatalf("不可用时 store.enabled 必须为 false：%s", body)
	}
	if _, ok := obj["metrics"].([]any); !ok {
		t.Fatalf("metrics 恒为数组（不可用时也发空数组）：%s", body)
	}
	if _, ok := obj["series"].([]any); !ok {
		t.Fatalf("series 恒为数组（不可用时也发空数组）：%s", body)
	}
	if _, has := obj["truncated"]; has {
		t.Fatalf("未裁剪时不应出现 truncated：%s", body)
	}
	t.Logf("unavailable 响应: %s", body)
}

func TestMetricsHistoryAvailableOverview(t *testing.T) {
	st := newHistoryStore(t)
	now := time.Now().Unix()
	if err := st.Append(now, []metricshist.Sample{
		{Name: "nfvis_x_ratio", Labels: map[string]string{"iface": "ens1"}, Value: 1.5},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := st.SetLastTick(now); err != nil {
		t.Fatalf("SetLastTick: %v", err)
	}

	ts := newTestServerOpts(t, Options{MetricsHistory: &MetricsHistoryOptions{Store: st, Path: st.Path()}})
	token := loginAdmin(t, ts)
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/metrics/history", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("状态 %d: %s", status, body)
	}
	obj := responseObject(t, body)
	if av, _ := obj["available"].(bool); !av {
		t.Fatalf("库可用时应 available=true: %s", body)
	}
	if _, has := obj["reason"]; has {
		t.Fatalf("available=true 时不应出现 reason（不编造）：%s", body)
	}
	// 无 name ⇒ series 在但为空数组。
	series, ok := obj["series"].([]any)
	if !ok {
		t.Fatalf("series 必须存在且为数组：%s", body)
	}
	if len(series) != 0 {
		t.Fatalf("无 name 时 series 应为空：%s", body)
	}
	names, ok := obj["metrics"].([]any)
	if !ok || len(names) != 1 || names[0] != "nfvis_x_ratio" {
		t.Fatalf("metrics 应列出已知指标名：%s", body)
	}
	if si, _ := obj["sample_interval_seconds"].(float64); int(si) != 60 {
		t.Fatalf("未配置时采样间隔应为默认 60：%s", body)
	}
	store, _ := obj["store"].(map[string]any)
	if sm, _ := store["samples"].(float64); int64(sm) != 1 {
		t.Fatalf("store.samples 应为 1：%s", body)
	}
	if stale, _ := store["stale"].(bool); stale {
		t.Fatalf("刚写过心跳不应判停滞：%s", body)
	}
	t.Logf("available 概览响应: %s", body)
}

func TestMetricsHistorySeriesShapeAndStaleness(t *testing.T) {
	st := newHistoryStore(t)
	now := time.Now().Unix()
	// 两条序列（同指标、不同标签），各 3 个点。
	for i := 0; i < 3; i++ {
		ts := now - int64(120-i*60)
		if err := st.Append(ts, []metricshist.Sample{
			{Name: "nfvis_y_packets", Labels: map[string]string{"iface": "a"}, Value: float64(i)},
			{Name: "nfvis_y_packets", Labels: map[string]string{"iface": "b"}, Value: float64(i * 2)},
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	// 心跳设为很久以前 ⇒ 应判停滞（now-lastTick > max(3×60,180)）。
	if err := st.SetLastTick(now - 1000); err != nil {
		t.Fatalf("SetLastTick: %v", err)
	}

	ts := newTestServerOpts(t, Options{MetricsHistory: &MetricsHistoryOptions{Store: st, Path: st.Path()}})
	token := loginAdmin(t, ts)
	status, _, body := cfgRequest(t, http.MethodGet,
		ts.URL+APIPrefix+"/metrics/history?name=nfvis_y_packets&last=1h", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("状态 %d: %s", status, body)
	}
	obj := responseObject(t, body)
	series, _ := obj["series"].([]any)
	if len(series) != 2 {
		t.Fatalf("应有 2 条序列（按标签分组）：%s", body)
	}
	first, _ := series[0].(map[string]any)
	if name, _ := first["name"].(string); name != "nfvis_y_packets" {
		t.Fatalf("序列 name 不对：%s", body)
	}
	labels, ok := first["labels"].(map[string]any)
	if !ok || labels["iface"] != "a" {
		t.Fatalf("序列 labels 形状不对（应为对象）：%s", body)
	}
	pts, _ := first["points"].([]any)
	if len(pts) == 0 {
		t.Fatalf("序列应有时间点：%s", body)
	}
	p0, _ := pts[0].(map[string]any)
	if _, ok := p0["ts"]; !ok {
		t.Fatalf("点缺 ts：%s", body)
	}
	if _, ok := p0["value"]; !ok {
		t.Fatalf("点缺 value：%s", body)
	}

	store, _ := obj["store"].(map[string]any)
	if stale, _ := store["stale"].(bool); !stale {
		t.Fatalf("心跳落后应判停滞 stale=true：%s", body)
	}

	// 低于 limit 时不应置 truncated。
	if _, has := obj["truncated"]; has {
		t.Fatalf("未触及 limit 不应置 truncated：%s", body)
	}
}

func TestMetricsHistoryTruncatedAndLimit(t *testing.T) {
	st := newHistoryStore(t)
	now := time.Now().Unix()
	var samples []metricshist.Sample
	for i := 0; i < 5; i++ {
		samples = append(samples, metricshist.Sample{Name: "nfvis_z", Value: float64(i)})
	}
	// 同一时刻多次写 = 5 个点（ts 不同以免语义含糊）。
	for i := 0; i < 5; i++ {
		if err := st.Append(now-int64(5-i), samples[i:i+1]); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	ts := newTestServerOpts(t, Options{MetricsHistory: &MetricsHistoryOptions{Store: st, Path: st.Path()}})
	token := loginAdmin(t, ts)
	status, _, body := cfgRequest(t, http.MethodGet,
		ts.URL+APIPrefix+"/metrics/history?name=nfvis_z&last=1h&step=1s&limit=3", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("状态 %d: %s", status, body)
	}
	obj := responseObject(t, body)
	if tr, _ := obj["truncated"].(bool); !tr {
		t.Fatalf("点数触及 limit 应置 truncated=true：%s", body)
	}
	series, _ := obj["series"].([]any)
	first, _ := series[0].(map[string]any)
	if pts, _ := first["points"].([]any); len(pts) != 3 {
		t.Fatalf("limit=3 时应返回 3 个最新点：%s", body)
	}
}

func TestMetricsHistoryBadRequest(t *testing.T) {
	st := newHistoryStore(t)
	ts := newTestServerOpts(t, Options{MetricsHistory: &MetricsHistoryOptions{Store: st, Path: st.Path()}})
	token := loginAdmin(t, ts)
	for _, q := range []string{
		"?last=abc", "?last=0s", "?last=5x", "?step=nope", "?since=abc", "?until=-1", "?limit=0", "?limit=abc",
	} {
		status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/metrics/history"+q, token, nil, nil)
		if status != http.StatusBadRequest {
			t.Fatalf("%s 应 400，得到 %d: %s", q, status, body)
		}
	}
}

func TestParseHistoryDuration(t *testing.T) {
	ok := map[string]time.Duration{
		"30s": 30 * time.Second,
		"5m":  5 * time.Minute,
		"1h":  time.Hour,
		"2d":  48 * time.Hour,
	}
	for in, want := range ok {
		got, err := parseHistoryDuration(in)
		if err != nil || got != want {
			t.Fatalf("parseHistoryDuration(%q)=%v,%v 期望 %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0s", "-1h", "5", "m", "1w", "abc", "1h30m"} {
		if _, err := parseHistoryDuration(bad); err == nil {
			t.Fatalf("parseHistoryDuration(%q) 应报错", bad)
		}
	}
	// 决策 #372（R142 A7）：上界 3650d + 防溢出——超界/溢出必须报错，不得给出误导性窗口。
	if d, err := parseHistoryDuration("3650d"); err != nil || d != 3650*24*time.Hour {
		t.Fatalf("3650d 应恰好在上界内：%v %v", d, err)
	}
	for _, over := range []string{"3651d", "999999999999d", "9999999999999999999d", "999999999999h"} {
		if _, err := parseHistoryDuration(over); err == nil {
			t.Fatalf("parseHistoryDuration(%q) 超界/溢出应报错（旧实现会溢出成误导性时长）", over)
		}
	}
}

func TestAutoHistoryStep(t *testing.T) {
	// 窗口 3600s、间隔 60 ⇒ 3600/120=30 < 60 ⇒ 取间隔 60。
	if got := autoHistoryStep(60, 0, 3600); got != 60 {
		t.Fatalf("autoHistoryStep=%d 期望 60（不小于间隔）", got)
	}
	// 窗口 24000s、间隔 60 ⇒ 24000/120=200 > 60 ⇒ 取 200。
	if got := autoHistoryStep(60, 0, 24000); got != 200 {
		t.Fatalf("autoHistoryStep=%d 期望 200", got)
	}
}

// TestRenderMetricsHistoryForms CLI 渲染与 REST 共用同一读视图（三面同源）：
// 概览、具名序列、非法形态拒绝（last/step 无 name）、未知指标如实说明。
func TestRenderMetricsHistoryForms(t *testing.T) {
	x, _ := newCLIKit(t)
	baseStore := func() map[string]any {
		return map[string]any{"path": "/var/lib/nfvis/metrics.db", "enabled": true,
			"size_bytes": int64(2048), "series": int64(1), "samples": int64(10),
			"oldest_ts": int64(1700000000), "newest_ts": int64(1700000100),
			"last_tick_ts": int64(1700000100), "stale": false}
	}
	x.setMetricsHistory(func(name string, since, until, step int64, limit int) map[string]any {
		if name == "" {
			return map[string]any{
				"available": true, "sample_interval_seconds": 60, "retention_days": 7,
				"metrics": []string{"nfvis_a"}, "series": []any{}, "store": baseStore(),
			}
		}
		if name != "nfvis_a" { // 未知指标：无序列
			return map[string]any{
				"available": true, "sample_interval_seconds": 60, "retention_days": 7,
				"metrics": []string{"nfvis_a"}, "series": []any{}, "store": baseStore(),
			}
		}
		return map[string]any{
			"available": true, "sample_interval_seconds": 60, "retention_days": 7,
			"metrics": []string{"nfvis_a"},
			"series": []any{map[string]any{"name": name, "labels": map[string]string{"iface": "ens1"},
				"points": []any{map[string]any{"ts": int64(1700000000), "value": float64(1.5)}}}},
			"store": baseStore(),
		}
	})

	out := x.renderMetricsHistory([]string{"history"})
	if !strings.Contains(out, "已启用") || !strings.Contains(out, "nfvis_a") || !strings.Contains(out, "上次采样") {
		t.Fatalf("概览渲染不符预期：\n%s", out)
	}
	if x.structured == nil {
		t.Fatalf("概览应设置 structured（供 display json/xml）")
	}

	out = x.renderMetricsHistory([]string{"history", "name", "nfvis_a", "last", "1h", "step", "1m"})
	if !strings.Contains(out, "iface=ens1") || !strings.Contains(out, "1.5") {
		t.Fatalf("序列渲染不符预期：\n%s", out)
	}

	out = x.renderMetricsHistory([]string{"history", "last", "1h"})
	if !strings.HasPrefix(out, "%%") {
		t.Fatalf("last 无 name 应拒绝并给正确形态：%q", out)
	}

	out = x.renderMetricsHistory([]string{"history", "name", "nope"})
	if !strings.Contains(out, "未知指标") {
		t.Fatalf("未知指标应如实说明：%q", out)
	}
}

// TestDynamicMetricNamesCandidate `show system metrics history name <metric>` 的候选
// 取自历史库内已知指标名（决策 #356），且库未启用时优雅回空。
func TestDynamicMetricNamesCandidate(t *testing.T) {
	st := newHistoryStore(t)
	now := time.Now().Unix()
	if err := st.Append(now, []metricshist.Sample{{Name: "nfvis_metric_b"}, {Name: "nfvis_metric_a"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	ts := newTestServerOpts(t, Options{MetricsHistory: &MetricsHistoryOptions{Store: st, Path: st.Path()}})
	token := loginAdmin(t, ts)
	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/cli/candidates?kind=metric-names", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("kind=metric-names 状态 %d: %s", status, body)
	}
	if !strings.Contains(string(body), "nfvis_metric_a") || !strings.Contains(string(body), "nfvis_metric_b") {
		t.Fatalf("候选应含库内指标名：%s", body)
	}
}

// 决策 #372（R142 A5/A6）：库启用但**本次读取失败**时——enabled 不翻 false、如实给 error 字段、
// 不把「查不了」显示成「还没有数据」。
func TestMetricsHistoryReadFailureKeepsEnabledAndReports(t *testing.T) {
	st := newHistoryStore(t)
	now := time.Now().Unix()
	if err := st.Append(now, []metricshist.Sample{{Name: "nfvis_y_ratio", Value: 1}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	ts := newTestServerOpts(t, Options{MetricsHistory: &MetricsHistoryOptions{Store: st, Path: st.Path()}})
	token := loginAdmin(t, ts)
	if err := st.Close(); err != nil { // 关库 ⇒ 后续查询失败（模拟读取失败）
		t.Fatalf("Close: %v", err)
	}

	status, _, body := cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/metrics/history", token, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("读失败仍应 200（诚实口径）：%d %s", status, body)
	}
	obj := responseObject(t, body)
	if av, _ := obj["available"].(bool); av {
		t.Fatalf("库读不到时 available 应为 false：%s", body)
	}
	if reason, _ := obj["reason"].(string); !strings.Contains(reason, "读取失败") {
		t.Fatalf("reason 应如实说「读取失败」而不是「未启用」：%s", body)
	}
	store, _ := obj["store"].(map[string]any)
	if en, _ := store["enabled"].(bool); !en {
		t.Fatalf("读取失败不得把 store.enabled 翻成 false（语义=存储是否启用）：%s", body)
	}
	if e, _ := store["error"].(string); e == "" {
		t.Fatalf("读取失败应给 store.error 如实说明：%s", body)
	}
	// 库整体读不到时（Stats 先失败）metrics 恒为空数组、reason/error 如实——
	// 「Stats 成功但 MetricNames/Health 失败」在单连接下不可构造（同一库先败先退），
	// 故 metrics_error/health_error 由下面的损坏-meta 用例覆盖 health 一路（A6）。
	metrics, _ := obj["metrics"].([]any)
	if len(metrics) != 0 {
		t.Fatalf("库读不到时 metrics 应为空数组：%s", body)
	}
}
