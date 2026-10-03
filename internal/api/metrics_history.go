package api

// 历史时序读视图（决策 #356，FR-SYS-005）。
//
// 三面同源：
//   · CLI  `show system metrics history [name <metric> [last <duration>] [step <duration>]]`
//   · REST `GET /metrics/history?name=&last=&step=&since=&until=&limit=`
//   · Web  「系统 · 历史趋势」页（#/system/metrics）
//
// 数据源 = 独立 SQLite 库（internal/metricshist）——`/metrics` 采样序列的历史。
// 与 `/metrics` 的差别有二：① 本视图**要求鉴权**（Bearer）——`/metrics` 无鉴权是为
// Prometheus 抓取，而历史是给操作者/控制台用的（这条不对称是决策 #356 的既定契约）；
// ② 支持窗口与降采样参数。
//
// 诚实口径（决策 #356）：库不可用/采样器未启用时**仍 200**，以 available=false + reason
// 如实说明；不编造数据，也不把「空」静默当成「真值」。三类空态（存储不可用 / 指标名
// 未知 / 窗口内无点）在 CLI 侧分别如实说明。

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xzjt/nfvis/internal/metricshist"
	"github.com/xzjt/nfvis/internal/model"
)

// parseHistoryDuration 解析 `<n><s|m|h|d>` 形式的时长（如 30s / 5m / 1h / 2d）。
// 空串、垃圾串、零、负数一律拒绝并给出可照做的说明（不含需求编号——用户可见文本）。
func parseHistoryDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("时长不能为空（形如 30s / 5m / 1h / 2d）")
	}
	unit := s[len(s)-1]
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("无效时长 %q（须为正整数 + 单位 s|m|h|d，如 30s / 5m / 1h / 2d）", s)
	}
	switch unit {
	case 's':
		return time.Duration(n) * time.Second, nil
	case 'm':
		return time.Duration(n) * time.Minute, nil
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("无效时长 %q（单位须为 s|m|h|d，如 30s / 5m / 1h / 2d）", s)
}

// historyIntervalSeconds 生效的采样间隔（committed 配置；读不到时回落模型默认）。
func (s *Server) historyIntervalSeconds() int {
	if s.engine == nil {
		return model.MetricsIntervalDefaultSeconds
	}
	cfg, err := s.engine.Committed()
	if err != nil {
		return model.MetricsIntervalDefaultSeconds
	}
	return cfg.MetricsHistoryIntervalSeconds()
}

// historyRetentionDays 生效的保留天数（committed 配置；读不到时回落模型默认）。
func (s *Server) historyRetentionDays() int {
	if s.engine == nil {
		return model.MetricsRetentionDaysDefault
	}
	cfg, err := s.engine.Committed()
	if err != nil {
		return model.MetricsRetentionDaysDefault
	}
	return cfg.MetricsHistoryRetentionDays()
}

// autoHistoryStep 自动降采样步长：max(interval, window/目标点数)（目标 ≤120 点）。
// 保证步长不小于采样间隔（不会出现「比原始间隔还细」的无意义分桶）。
func autoHistoryStep(interval int, since, until int64) int64 {
	step := int64(interval)
	if window := until - since; window > 0 {
		if target := window / int64(metricshist.DefaultStepPoints); target > step {
			step = target
		}
	}
	if step < 1 {
		step = 1
	}
	return step
}

// historyStaleThreshold 采样停滞判据：now - last_tick > max(3×间隔, 180s)。
func historyStaleThreshold(intervalSeconds int) int64 {
	t := int64(3 * intervalSeconds)
	if t < 180 {
		t = 180
	}
	return t
}

// metricsHistoryView 构建历史时序读视图（CLI 与 REST **同一实现**）。返回字段严格对齐
// openapi `MetricsHistory` 契约：
//
//	available / reason（仅不可用）/ sample_interval_seconds / retention_days /
//	metrics（恒有，可能为空）/ series（恒有，可能为空）/ truncated（仅裁剪时）/ store（恒有）
//
// since/until/step 为 unix 秒（step>0，调用方已解析/自动推导），limit 为每条序列点数上限。
func (s *Server) metricsHistoryView(name string, since, until, step int64, limit int) map[string]any {
	interval := s.historyIntervalSeconds()
	retention := s.historyRetentionDays()

	// 形状稳定：metrics/series 恒为数组、store 恒为对象（契约声明如此）。
	view := map[string]any{
		"available":               false,
		"sample_interval_seconds": interval,
		"retention_days":          retention,
		"metrics":                 []string{},
		"series":                  []any{},
	}
	hist := s.history
	path, disabled := "", ""
	if hist != nil {
		path, disabled = hist.Path, hist.Disabled
	}
	store := map[string]any{"path": path, "enabled": false}
	view["store"] = store

	if hist == nil || hist.Store == nil {
		reason := disabled
		if reason == "" {
			reason = "历史时序存储未启用"
		}
		view["reason"] = reason
		return view
	}

	// 库可打开：概览读数如实取自库本身。
	st, err := hist.Store.Stats()
	if err != nil {
		store["enabled"] = false
		view["reason"] = "历史时序存储读取失败：" + err.Error()
		return view
	}
	view["available"] = true
	store["enabled"] = true
	store["size_bytes"] = st.SizeBytes
	store["series"] = st.Series
	store["samples"] = st.Samples
	if st.OldestTS != 0 {
		store["oldest_ts"] = st.OldestTS
	}
	if st.NewestTS != 0 {
		store["newest_ts"] = st.NewestTS
	}
	// 采样器心跳：仅当曾有过一次心跳才报「上次采样/是否停滞」（无记录不编造时刻）。
	if lastTick, lastErr, herr := hist.Store.Health(); herr == nil {
		if lastTick != 0 {
			store["last_tick_ts"] = lastTick
			store["stale"] = time.Now().Unix()-lastTick > historyStaleThreshold(interval)
		}
		if lastErr != "" {
			store["last_error"] = lastErr
		}
	}

	// 已知指标名清单（概览/动态补全共用；恒为数组）。
	if names, nerr := hist.Store.MetricNames(); nerr == nil {
		if names == nil {
			names = []string{}
		}
		view["metrics"] = names
	}

	// name 省略＝只回概览（series 恒为空数组）。
	if name == "" {
		return view
	}

	qr, qerr := hist.Store.Query(metricshist.Query{
		Name: name, Since: since, Until: until, Step: step, Limit: limit,
	})
	if qerr != nil {
		// 库可打开但查询失败：**不得**答成「窗口内无数据」——那会把「读不出来」说成「没有」。
		// 如实降级为 available=false + reason（本视图这一次不可信），series 保持空数组。
		view["available"] = false
		view["reason"] = "历史时序查询失败：" + qerr.Error()
		store["enabled"] = false
		if s.log != nil {
			s.log.Warn("历史时序查询失败", "name", name, "err", qerr)
		}
		return view
	}
	out := make([]any, 0, len(qr.Series))
	for _, sr := range qr.Series {
		labels := sr.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		pts := make([]any, 0, len(sr.Points))
		for _, p := range sr.Points {
			pts = append(pts, map[string]any{"ts": p.TS, "value": p.Value})
		}
		out = append(out, map[string]any{"name": sr.Name, "labels": labels, "points": pts})
	}
	view["series"] = out
	// truncated 取自存储侧的**真值**（有条序列的原始点数超过 limit 被裁掉），
	// 不用「点数恰好等于 limit」去猜——那会把「正好这么多点」误报成被裁剪。
	if qr.Truncated {
		view["truncated"] = true
	}
	return view
}

// handleMetricsHistory GET /api/v1/metrics/history（决策 #356，read-only）。
//
// 参数非法 → 400（与其它 handler 同款错误体）；存储不可用**不是** 5xx——恒 200 +
// available=false + reason（v1 不设告警码，读视图如实说明即可）。
func (s *Server) handleMetricsHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := time.Now().Unix()

	until := now
	if v := q.Get("until"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST",
				"无效的 until（unix 秒，须为正整数）", nil)
			return
		}
		until = n
	}

	var since int64
	if v := q.Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST",
				"无效的 since（unix 秒，须为正整数）", nil)
			return
		}
		since = n
	} else {
		last := "1h"
		if v := q.Get("last"); v != "" {
			last = v
		}
		d, err := parseHistoryDuration(last)
		if err != nil {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
			return
		}
		since = until - int64(d.Seconds())
	}
	if since > until {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST",
			"since 晚于 until（起始时间必须早于结束时间）", nil)
		return
	}

	limit := metricshist.DefaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST",
				"无效的 limit（须为正整数）", nil)
			return
		}
		limit = n
	}
	if limit > metricshist.MaxQueryLimit {
		// 夹取（而非报错）：读视图宁可少给点也不肯一次拉爆内存；被夹时会置 truncated。
		limit = metricshist.MaxQueryLimit
	}

	step := int64(0)
	if v := q.Get("step"); v != "" {
		d, err := parseHistoryDuration(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
			return
		}
		step = int64(d.Seconds())
	} else {
		step = autoHistoryStep(s.historyIntervalSeconds(), since, until)
	}

	writeJSON(w, http.StatusOK, s.metricsHistoryView(q.Get("name"), since, until, step, limit))
}

// renderMetricsHistory `show system metrics history [name <metric> [last <duration>] [step <duration>]]`。
// t = `history` 之后的 token 切片（execShowSystemDiag 已剥掉 `metrics`）。渲染与 REST
// **同一读视图**（三面同源）：执行器调用注入的 metricsHistoryView，不另查一遍。
func (x *cliExecutor) renderMetricsHistory(t []string) string {
	if len(t) == 0 || t[0] != "history" {
		return "%% 无效命令: show system metrics " + strings.Join(t, " ") +
			"（可用：history [name <metric> [last <duration>] [step <duration>]]）\n"
	}
	rest := t[1:]
	if len(rest) == 0 {
		return x.renderMetricsHistoryOverview()
	}
	if rest[0] != "name" {
		return "%% 无效命令: show system metrics history " + strings.Join(rest, " ") +
			"（可用：history [name <metric> [last <duration>] [step <duration>]]；" +
			"last/step 须跟在 name <metric> 之后）\n"
	}
	if len(rest) < 2 || rest[1] == "" {
		return "%% 语法: show system metrics history name <metric> [last <duration>] [step <duration>]\n"
	}
	name := rest[1]
	last, step := "1h", ""
	for i := 2; i < len(rest); {
		switch rest[i] {
		case "last":
			if i+1 >= len(rest) {
				return "%% 语法: show system metrics history name <metric> last <duration> [step <duration>]\n"
			}
			last = rest[i+1]
			i += 2
		case "step":
			if i+1 >= len(rest) {
				return "%% 语法: show system metrics history name <metric> [last <duration>] step <duration>\n"
			}
			step = rest[i+1]
			i += 2
		default:
			return "%% 无效命令: show system metrics history name " + strings.Join(rest[1:], " ") +
				"（可用：name <metric> [last <duration>] [step <duration>]）\n"
		}
	}

	window, err := parseHistoryDuration(last)
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	now := time.Now().Unix()
	until := now
	since := until - int64(window.Seconds())
	stepSecs := int64(0)
	if step != "" {
		d, err := parseHistoryDuration(step)
		if err != nil {
			return "%% " + err.Error() + "\n"
		}
		stepSecs = int64(d.Seconds())
	} else {
		stepSecs = autoHistoryStep(x.historyIntervalDefault(), since, until)
	}

	view := x.metricsHistoryViewFor(name, since, until, stepSecs, metricshist.DefaultLimit)
	x.structured = view
	return renderMetricsHistorySeries(view, name, window, stepSecs)
}

// renderMetricsHistoryOverview 概览渲染（无 name 参数）。
func (x *cliExecutor) renderMetricsHistoryOverview() string {
	view := x.metricsHistoryViewFor("", 0, 0, 0, metricshist.DefaultLimit)
	x.structured = view

	var b strings.Builder
	avail, _ := view["available"].(bool)
	if !avail {
		reason, _ := view["reason"].(string)
		if reason == "" {
			reason = "历史时序存储未启用"
		}
		b.WriteString("历史时序存储: 不可用（" + reason + "）\n")
		store := viewStore(view)
		fmt.Fprintf(&b, "库路径:       %s\n", viewText(viewString(store, "path")))
		b.WriteString("提示: 存储不可用时不会编造历史数据；请确认 nfvisd 以 -metrics-db <路径> 启动且该库可打开。\n")
		return b.String()
	}

	store := viewStore(view)
	interval := viewInt(view, "sample_interval_seconds")
	retention := viewInt(view, "retention_days")
	b.WriteString("历史时序存储: 已启用\n")
	fmt.Fprintf(&b, "库路径:       %s（%s）\n", viewText(viewString(store, "path")), humanSize(viewInt64(store, "size_bytes")))
	fmt.Fprintf(&b, "采样间隔:     %ds%s\n", interval, intervalNote(interval))
	fmt.Fprintf(&b, "保留天数:     %d 天（另有 2,000,000 行硬上限兜底）\n", retention)
	fmt.Fprintf(&b, "序列数:       %d\n", viewInt64(store, "series"))
	fmt.Fprintf(&b, "样本数:       %d\n", viewInt64(store, "samples"))

	oldest, hasOld := store["oldest_ts"].(int64)
	newest, hasNew := store["newest_ts"].(int64)
	if hasOld && hasNew {
		fmt.Fprintf(&b, "时间范围:     %s ~ %s\n", fmtHistoryTS(oldest), fmtHistoryTS(newest))
	} else {
		b.WriteString("时间范围:     （无数据）\n")
	}

	if lastTick, ok := store["last_tick_ts"].(int64); ok {
		stale, _ := store["stale"].(bool)
		if stale {
			fmt.Fprintf(&b, "上次采样:     %s（停滞：距上次采样 %s）\n",
				fmtHistoryTS(lastTick), time.Since(time.Unix(lastTick, 0)).Round(time.Second))
		} else {
			fmt.Fprintf(&b, "上次采样:     %s（正常）\n", fmtHistoryTS(lastTick))
		}
	} else {
		b.WriteString("上次采样:     （无记录：采样器尚未完成一次采集）\n")
	}
	if lastErr := viewString(store, "last_error"); lastErr != "" {
		fmt.Fprintf(&b, "上次采样错误: %s\n", lastErr)
	}

	names := viewStrings(view, "metrics")
	if len(names) == 0 {
		b.WriteString("已知指标:     （无：采样器尚未写入任何指标，可用 show system metrics history 稍后重试）\n")
	} else {
		fmt.Fprintf(&b, "已知指标（%d）: %s\n", len(names), strings.Join(names, ", "))
	}
	b.WriteString("按指标查看:   show system metrics history name <指标名> [last 1h] [step 1m]\n")
	b.WriteString("说明: 历史点是采样瞬间的瞬时值/累计值，不是区间聚合；计数器需读侧自行差分（本视图不伪造速率）。\n")
	return b.String()
}

// renderMetricsHistorySeries 某指标的各序列渲染（name 参数）。
func renderMetricsHistorySeries(view map[string]any, name string, window time.Duration, step int64) string {
	var b strings.Builder
	avail, _ := view["available"].(bool)
	if !avail {
		reason, _ := view["reason"].(string)
		if reason == "" {
			reason = "历史时序存储未启用"
		}
		return "历史时序存储: 不可用（" + reason + "）\n"
	}

	series := viewSeries(view)
	if len(series) == 0 {
		// 两种空态要分清：指标名未知 vs 窗口内无点。
		if !containsString(viewStrings(view, "metrics"), name) {
			return fmt.Sprintf("未知指标 %s：存储内尚无该指标。用 show system metrics history 概览可列出已知指标名。\n", name)
		}
		return fmt.Sprintf("指标 %s 在所选窗口（%s）内无采样点（可放宽 last，或确认采样器正在运行：show system metrics history）。\n",
			name, window)
	}
	fmt.Fprintf(&b, "指标 %s（窗口 %s、步长 %ds）\n", name, window, step)
	truncated, _ := view["truncated"].(bool)
	for _, s := range series {
		pts, _ := s["points"].([]any)
		head := fmt.Sprintf("  序列 %s：窗口 %s、步长 %ds、点数 %d", labelsText(s["labels"]), window, step, len(pts))
		if truncated {
			head += "（已被 limit 截断：只保留最近的点）"
		}
		b.WriteString(head + "\n")
		for _, p := range pts {
			pt, _ := p.(map[string]any)
			ts, _ := pt["ts"].(int64)
			val, _ := pt["value"].(float64)
			fmt.Fprintf(&b, "  %s  %s\n", fmtHistoryTS(ts), formatHistoryValue(val))
		}
	}
	return b.String()
}

// metricsHistoryViewFor 调用注入的视图构建器；未注入（测试未接）时回一份如实的不可用视图。
func (x *cliExecutor) metricsHistoryViewFor(name string, since, until, step int64, limit int) map[string]any {
	if x.metricsHistoryView == nil {
		return map[string]any{
			"available":               false,
			"reason":                  "历史时序存储未启用",
			"sample_interval_seconds": x.historyIntervalDefault(),
			"retention_days":          model.MetricsRetentionDaysDefault,
			"metrics":                 []string{},
			"series":                  []any{},
			"store":                   map[string]any{"path": "", "enabled": false},
		}
	}
	return x.metricsHistoryView(name, since, until, step, limit)
}

// historyIntervalDefault 生效的采样间隔（CLI 侧用于自动步长推导）。
func (x *cliExecutor) historyIntervalDefault() int {
	if x.engine == nil {
		return model.MetricsIntervalDefaultSeconds
	}
	cfg, err := x.engine.Committed()
	if err != nil {
		return model.MetricsIntervalDefaultSeconds
	}
	return cfg.MetricsHistoryIntervalSeconds()
}

// ---------- 视图读数小工具 ----------

func viewStore(view map[string]any) map[string]any {
	if m, ok := view["store"].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func viewSeries(view map[string]any) []map[string]any {
	raw, _ := view["series"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, s := range raw {
		if m, ok := s.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func viewInt(view map[string]any, key string) int {
	if v, ok := view[key].(int); ok {
		return v
	}
	return 0
}

func viewInt64(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func viewString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func viewStrings(view map[string]any, key string) []string {
	switch v := view[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// labelsText 标签集文本（无标签时「（无标签）」）。
func labelsText(v any) string {
	m, ok := v.(map[string]string)
	if !ok || len(m) == 0 {
		return "（无标签）"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 排序保证输出稳定（同一条序列每次渲染一致）。
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ", ")
}

func viewText(s string) string {
	if s == "" {
		return "（未知）"
	}
	return s
}

// intervalNote 采样间隔的注记：与**模型默认值**比较（不能拿生效值和它自己比——那样注记恒真）。
// 显式配成 60 与未配置不可区分，此处按值如实说「默认值」即可。
func intervalNote(seconds int) string {
	if seconds == model.MetricsIntervalDefaultSeconds {
		return "（默认值；可用 set system metrics history interval <n> 调整）"
	}
	return "（已配置；set system metrics history interval <n> 调整）"
}

func fmtHistoryTS(ts int64) string {
	return time.Unix(ts, 0).Format("2006-01-02 15:04:05")
}

func formatHistoryValue(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
