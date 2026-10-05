package metricshist

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func mustQuery(t *testing.T, st *Store, q Query) []Series {
	t.Helper()
	r, err := st.Query(q)
	if err != nil {
		t.Fatalf("Query(%+v): %v", q, err)
	}
	return r.Series
}

func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestOpenIdempotentAndVersionGuard(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("首次 Open: %v", err)
	}
	if err := st.Append(100, []Sample{{Name: "m", Value: 1}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	_ = st.Close()

	// 再开一次不得清库（CREATE TABLE IF NOT EXISTS + 不 bump 版本）。
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("二次 Open: %v", err)
	}
	defer func() { _ = st2.Close() }()
	got := mustQuery(t, st2, Query{Name: "m"})
	if len(got) != 1 || len(got[0].Points) != 1 {
		t.Fatalf("二次打开后数据丢失: %+v", got)
	}

	// 更高版本必须拒绝（不认识的 schema 不硬上）。
	if _, err := st2.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatalf("置版本: %v", err)
	}
	_ = st2.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("更高 schema 版本应被拒绝")
	}
}

// TestReopenIfReplaced 决策 #365：库文件被删/替换后必须重开——否则旧连接仍持已删 inode，
// 读视图继续显示旧历史、空间不释放（真机：format-data/rm 后 80598 样本仍在读数里）。
func TestReopenIfReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Append(100, []Sample{{Name: "m", Value: 1}}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// 文件仍在：重开检查是空操作（数据不动）。
	if err := st.ReopenIfReplaced(); err != nil {
		t.Fatalf("文件在场时不应报错: %v", err)
	}
	if got := mustQuery(t, st, Query{Name: "m"}); len(got) != 1 {
		t.Fatalf("文件在场时数据不应变化: %+v", got)
	}

	// 删除库文件（format-data / 操作者 rm 的等价动作）：重开后读数回到「无历史」。
	// ⚠️ Windows 不允许删除被进程打开的文件（本用例在此平台无从模拟）——该路径由
	// Linux/真机验证（round148：format-data 后文件 unlink、读视图回到「无历史」）。
	if runtime.GOOS == "windows" {
		t.Skip("Windows 无法删除被打开的文件；该路径在 Linux/真机验证")
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("删除库文件: %v", err)
	}
	if err := st.ReopenIfReplaced(); err != nil {
		t.Fatalf("文件被删后重开应成功: %v", err)
	}
	if got := mustQuery(t, st, Query{Name: "m"}); len(got) != 0 {
		t.Fatalf("重开后应为空库（无历史）: %+v", got)
	}
	if stt, err := st.Stats(); err != nil || stt.Samples != 0 || stt.Series != 0 {
		t.Fatalf("重开后统计应为 0: %+v/%v", stt, err)
	}
	// 新库可继续写（schema 已重建）。
	if err := st.Append(200, []Sample{{Name: "m2", Value: 2}}); err != nil {
		t.Fatalf("重开后 Append: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("重开后文件应被重建: %v", err)
	}
}

func TestAppendQueryRoundTripWithLabels(t *testing.T) {
	st := openTemp(t)
	samples := []Sample{
		{Name: "nfvis_vpp_interface_rx_packets", Labels: map[string]string{"interface": "ens192"}, Value: 10},
		{Name: "nfvis_vpp_interface_rx_packets", Labels: map[string]string{"interface": "ens224"}, Value: 20},
		{Name: "nfvis_system_cpu_utilization_ratio", Value: 0.5},
	}
	if err := st.Append(1000, samples); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// 同一时刻再写一轮（值变化），验证同序列多点。
	if err := st.Append(1060, []Sample{
		{Name: "nfvis_vpp_interface_rx_packets", Labels: map[string]string{"interface": "ens192"}, Value: 30},
	}); err != nil {
		t.Fatalf("Append2: %v", err)
	}

	got := mustQuery(t, st, Query{Name: "nfvis_vpp_interface_rx_packets"})
	if len(got) != 2 {
		t.Fatalf("应得 2 条序列（按标签分组），得 %d: %+v", len(got), got)
	}
	// 顺序按 labels JSON 升序：ens192 < ens224。
	if got[0].Labels["interface"] != "ens192" || got[1].Labels["interface"] != "ens224" {
		t.Fatalf("序列顺序/标签错: %+v", got)
	}
	if len(got[0].Points) != 2 || got[0].Points[0].TS != 1000 || got[0].Points[1].Value != 30 {
		t.Fatalf("ens192 点集错: %+v", got[0].Points)
	}

	// 无标签序列：labels 应回空对象而不是 null。
	g2 := mustQuery(t, st, Query{Name: "nfvis_system_cpu_utilization_ratio"})
	if len(g2) != 1 || g2[0].Labels == nil || len(g2[0].Labels) != 0 {
		t.Fatalf("无标签序列应为空标签对象: %+v", g2)
	}
}

func TestAppendEmptyIsNoop(t *testing.T) {
	st := openTemp(t)
	if err := st.Append(1000, nil); err != nil {
		t.Fatalf("空批次应成功: %v", err)
	}
	names, err := st.MetricNames()
	if err != nil {
		t.Fatalf("MetricNames: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("空批次不该留下序列: %v", names)
	}
}

func TestQueryWindowAndUnknownName(t *testing.T) {
	st := openTemp(t)
	for _, ts := range []int64{100, 200, 300, 400} {
		if err := st.Append(ts, []Sample{{Name: "m", Value: float64(ts)}}); err != nil {
			t.Fatalf("Append %d: %v", ts, err)
		}
	}
	got := mustQuery(t, st, Query{Name: "m", Since: 200, Until: 300})
	if len(got) != 1 || len(got[0].Points) != 2 {
		t.Fatalf("窗口过滤错: %+v", got)
	}
	if got[0].Points[0].Value != 200 || got[0].Points[1].Value != 300 {
		t.Fatalf("窗口边界应闭区间: %+v", got[0].Points)
	}
	// 未知指标名 → 空结果（不是错误，也不是编造）。
	none := mustQuery(t, st, Query{Name: "nope"})
	if len(none) != 0 {
		t.Fatalf("未知指标名应回空: %+v", none)
	}
}

func TestDownsampleTakesLastInBucket(t *testing.T) {
	st := openTemp(t)
	for ts := int64(0); ts < 10; ts++ {
		if err := st.Append(ts, []Sample{{Name: "m", Value: float64(ts)}}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	got := mustQuery(t, st, Query{Name: "m", Step: 3})
	// 桶 [0,2]->2、[3,5]->5、[6,8]->8、[9]->9：取桶内最后一个。
	want := []int64{2, 5, 8, 9}
	if len(got[0].Points) != len(want) {
		t.Fatalf("桶数错: %+v", got[0].Points)
	}
	for i, w := range want {
		if got[0].Points[i].TS != w {
			t.Fatalf("第 %d 桶应取 ts=%d，得 %+v", i, w, got[0].Points)
		}
	}
}

func TestQueryLimitKeepsNewest(t *testing.T) {
	st := openTemp(t)
	for ts := int64(0); ts < 10; ts++ {
		if err := st.Append(ts, []Sample{{Name: "m", Value: float64(ts)}}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	got := mustQuery(t, st, Query{Name: "m", Limit: 3})
	pts := got[0].Points
	if len(pts) != 3 || pts[0].TS != 7 || pts[2].TS != 9 {
		t.Fatalf("limit 应保留最新 3 点: %+v", pts)
	}
}

func TestPruneByAgeAndReadBack(t *testing.T) {
	st := openTemp(t)
	for _, ts := range []int64{100, 200, 300, 400} {
		if err := st.Append(ts, []Sample{{Name: "m", Value: float64(ts)}}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	res, err := st.Prune(250, 0)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res.DeletedByAge != 2 {
		t.Fatalf("应删 2 行（ts<250），得 %d", res.DeletedByAge)
	}
	if res.SamplesAfter != 2 || res.OldestTSAfter != 300 {
		t.Fatalf("回读读数错: %+v", res)
	}
	if res.RetentionCutof != 250 {
		t.Fatalf("窗口截止应如实回填: %+v", res)
	}
}

func TestPruneByRowCapAndOrphanSeries(t *testing.T) {
	st := openTemp(t)
	// 两条序列混合写入，便于验证孤立序列清理。
	for ts := int64(0); ts < 5; ts++ {
		if err := st.Append(ts, []Sample{
			{Name: "a", Value: float64(ts)},
			{Name: "b", Value: float64(ts)},
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	// 行顶 4：越顶按最旧删 6 行 → a 的第 0/1/2 与 b 的第 0/1/2（按 (ts,rowid) 序）。
	res, err := st.Prune(0, 4)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res.DeletedByAge != 0 {
		t.Fatalf("cutoff=0 不该按年龄删: %+v", res)
	}
	if res.DeletedByRows != 6 || res.SamplesAfter != 4 {
		t.Fatalf("行顶裁剪错: %+v", res)
	}

	// 时间窗把剩下的点全部裁掉（ts<5）→ 两条序列都变孤立，应被清理。
	res2, err := st.Prune(5, 0)
	if err != nil {
		t.Fatalf("Prune2: %v", err)
	}
	if res2.DeletedByAge != 4 || res2.SamplesAfter != 0 {
		t.Fatalf("全裁应删 4 行、回读 0: %+v", res2)
	}
	if res2.OrphanSeries != 2 {
		t.Fatalf("应清理 2 条孤立序列（a、b），得 %+v", res2)
	}
	names, err := st.MetricNames()
	if err != nil {
		t.Fatalf("MetricNames: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("孤立序列未清理干净: %v", names)
	}
}

func TestStatsAndMetricNamesSorted(t *testing.T) {
	st := openTemp(t)
	if err := st.Append(100, []Sample{{Name: "z", Value: 1}, {Name: "a", Value: 2}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := st.Append(200, []Sample{{Name: "a", Value: 3}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	stt, err := st.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stt.Series != 2 || stt.Samples != 3 || stt.OldestTS != 100 || stt.NewestTS != 200 {
		t.Fatalf("Stats 错: %+v", stt)
	}
	if stt.SizeBytes <= 0 {
		t.Fatalf("库文件大小应 >0（取不到才记 0）: %+v", stt)
	}
	names, err := st.MetricNames()
	if err != nil {
		t.Fatalf("MetricNames: %v", err)
	}
	if fmt.Sprint(names) != "[a z]" {
		t.Fatalf("指标名应去重且排序: %v", names)
	}
}

func TestEmptyStatsAreZeroNotFabricated(t *testing.T) {
	st := openTemp(t)
	stt, err := st.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stt.Series != 0 || stt.Samples != 0 || stt.OldestTS != 0 || stt.NewestTS != 0 {
		t.Fatalf("空库读数应全 0（不编造）: %+v", stt)
	}
}

func TestHealthMeta(t *testing.T) {
	st := openTemp(t)
	ts, errMsg, err := st.Health()
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if ts != 0 || errMsg != "" {
		t.Fatalf("无心跳时应如实回 0/空: %d %q", ts, errMsg)
	}
	if err := st.SetLastTick(12345); err != nil {
		t.Fatalf("SetLastTick: %v", err)
	}
	if err := st.SetLastError("boom"); err != nil {
		t.Fatalf("SetLastError: %v", err)
	}
	ts, errMsg, err = st.Health()
	if err != nil {
		t.Fatalf("Health2: %v", err)
	}
	if ts != 12345 || errMsg != "boom" {
		t.Fatalf("心跳读数错: %d %q", ts, errMsg)
	}
	// 下次成功心跳应清掉上次错误。
	if err := st.SetLastTick(12400); err != nil {
		t.Fatalf("SetLastTick2: %v", err)
	}
	_, errMsg, _ = st.Health()
	if errMsg != "" {
		t.Fatalf("成功心跳应清空上次错误: %q", errMsg)
	}
}

func TestPrometheusLabelsArePreservedIncludingCommas(t *testing.T) {
	// 标签值含逗号/等号时不得因拼接歧义而串味（JSON 落库的动机）。
	st := openTemp(t)
	lbl := map[string]string{"reason": "a=b,c", "source": `x"y`}
	if err := st.Append(1, []Sample{{Name: "m", Labels: lbl, Value: 7}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got := mustQuery(t, st, Query{Name: "m"})
	if len(got) != 1 || got[0].Labels["reason"] != "a=b,c" || got[0].Labels["source"] != `x"y` {
		t.Fatalf("标签还原错: %+v", got[0].Labels)
	}
}

// db 字段在测试里用于制造「更高版本」场景。
var _ = sql.ErrNoRows

// TestQueryTruncatedIsTruthful：「truncated」必须是真值——点数**恰好**等于 limit 不算被裁，
// 只有原始点数**超过** limit 而被裁才算（避免把「正好这么多点」误报成截断）。
func TestQueryTruncatedIsTruthful(t *testing.T) {
	st := openTemp(t)
	for ts := int64(0); ts < 5; ts++ {
		if err := st.Append(ts, []Sample{{Name: "m", Value: float64(ts)}}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	// 恰好 5 点、limit=5：没丢数据 → 不置 truncated。
	r, err := st.Query(Query{Name: "m", Limit: 5})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(r.Series) != 1 || len(r.Series[0].Points) != 5 {
		t.Fatalf("应得 5 点: %+v", r)
	}
	if r.Truncated {
		t.Fatalf("点数恰好等于 limit 不该报截断（真值判据）")
	}
	// limit=4：真的裁掉 1 点 → 置 truncated，且保留最近 4 点。
	r2, err := st.Query(Query{Name: "m", Limit: 4})
	if err != nil {
		t.Fatalf("Query2: %v", err)
	}
	if !r2.Truncated {
		t.Fatalf("点数超过 limit 应报截断")
	}
	pts := r2.Series[0].Points
	if len(pts) != 4 || pts[0].TS != 1 || pts[3].TS != 4 {
		t.Fatalf("应保留最近 4 点: %+v", pts)
	}
}

// 决策 #372（R142 A8）：step>0 时载入窗口被收窄到「最新 limit+1 个桶」——结果必须与
// 「全窗口降采样 + 裁 limit」逐点一致（更旧的点本来就会被裁掉），且 truncated 为真值。
func TestQueryLimitWithStepNarrowsWindowSameResult(t *testing.T) {
	st := openTemp(t)
	// 每 10s 一点，共 100 点（ts 1000..1990）。
	for i := 0; i < 100; i++ {
		if err := st.Append(int64(1000+i*10), []Sample{{Name: "m", Value: float64(i)}}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	const step, limit = int64(30), 5
	// 参考：不做 limit（全窗口降采样）后再手工取最新 limit 点。
	full := mustQuery(t, st, Query{Name: "m", Step: step, Limit: 0})
	ref := full[0].Points
	if len(ref) <= limit {
		t.Fatalf("参考点数应大于 limit（构造前提）：%d", len(ref))
	}
	ref = ref[len(ref)-limit:]
	res, err := st.Query(Query{Name: "m", Step: step, Limit: limit})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	pts := res.Series[0].Points
	if len(pts) != len(ref) {
		t.Fatalf("收窄后点数应与参考一致：%d vs %d", len(pts), len(ref))
	}
	for i := range pts {
		if pts[i].TS != ref[i].TS {
			t.Fatalf("第 %d 点与参考不一致（收窄改变了结果）：%+v vs %+v", i, pts[i], ref[i])
		}
	}
	if !res.Truncated {
		t.Fatalf("窗口被收窄（真值丢数据）时 truncated 应为 true")
	}
}

// 决策 #372（R142 A8）+ 决策 #376（锚点修正）：载入有界计划的纯函数表。
func TestPlanBoundedLoad(t *testing.T) {
	// step>0 + 最新样本：窗口收窄到「最新样本起算的 limit+1 个桶」并置真值截断。
	since, desc, tr := planBoundedLoad(Query{Since: 0, Until: 10000, Step: 30, Limit: 5}, 10000, true)
	if since != 10000-6*30 || desc != 0 || !tr {
		t.Fatalf("step>0 应收窄窗口并置截断：since=%d desc=%d tr=%v", since, desc, tr)
	}
	// 锚点＝**窗口内最新样本**而非 until（决策 #376 回归）：until 晚于最新样本超过一个 step 时，
	// 不得把本应保留的旧点裁掉（旧实现锚 until=10000 ⇒ since=9996，会丢点）。
	if since, _, tr := planBoundedLoad(Query{Since: 0, Until: 10000, Step: 1, Limit: 3}, 9996, true); since != 9996-4 || !tr {
		t.Fatalf("锚点应为最新样本（不是 until）：since=%d tr=%v", since, tr)
	}
	// 窗口本来就在 limit+1 桶内：不收窄、不置截断。
	if since, _, tr := planBoundedLoad(Query{Since: 9900, Until: 10000, Step: 30, Limit: 5}, 10000, true); since != 9900 || tr {
		t.Fatalf("窗口已足够小不应收窄：since=%d tr=%v", since, tr)
	}
	// 哨兵 until + 最新样本：以最新样本为锚（否则收窄到未来空窗）。
	since, _, tr = planBoundedLoad(Query{Since: 0, Until: storeMaxUntil, Step: 30, Limit: 5}, 5000, true)
	if since != 5000-6*30 || !tr {
		t.Fatalf("哨兵 until 应以最新样本为锚：since=%d tr=%v", since, tr)
	}
	// 无样本（hasNewest=false）：保持原 since（不猜）。
	if since, _, tr := planBoundedLoad(Query{Since: 7, Until: 10000, Step: 30, Limit: 5}, 0, false); since != 7 || tr {
		t.Fatalf("无样本时不应收窄：since=%d tr=%v", since, tr)
	}
	// step<=0：DESC 限行 limit+1。
	if since, desc, tr := planBoundedLoad(Query{Since: 0, Until: 100, Step: 0, Limit: 5}, 0, false); since != 0 || desc != 6 || tr {
		t.Fatalf("step<=0 应走 DESC 限行：since=%d desc=%d tr=%v", since, desc, tr)
	}
	// 无 limit：原样（调用方要全窗口）。
	if since, desc, tr := planBoundedLoad(Query{Since: 3, Until: 100, Step: 30, Limit: 0}, 0, false); since != 3 || desc != 0 || tr {
		t.Fatalf("无 limit 应原样：since=%d desc=%d tr=%v", since, desc, tr)
	}
}
