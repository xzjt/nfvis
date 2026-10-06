// Package metricshist 历史时序存储（决策 #356，FR-SYS-005）：把 `/metrics` 的采样序列
// 落到**独立 SQLite 库**（默认 /var/lib/nfvis/metrics.db），供 CLI / REST / Web 三面回溯。
//
// 设计要点（详见 docs/superpowers/specs/2026-10-03-metrics-history-design.md）：
//   - 维度表 series(id,name,labels) + 窄事实表 samples(series_id,ts,value)：行宽 ≈40B（含索引）；
//   - 独立库：不与配置库（nfvis.db）的事务/候选锁竞争，也不进配置备份/恢复语义；
//   - 保留**双界**：时间窗（retention-days）+ 硬行顶（MaxRows），越顶裁最旧；
//   - 「写成功 ≠ 收敛」：Prune 裁剪后回读，读数反映事实；
//   - 标签以**键排序的规范 JSON** 落库（唯一约束用），避开 k=v 拼接在值含 `,`/`=` 时的歧义。
package metricshist

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动（免 CGO，与配置库同款）
)

// 存储/查询口径的默认与边界值。
//
// 采样间隔与保留天数的**默认值与取值范围在 internal/model**（配置面单一真源，
// 校验与读视图共用）——本包只负责「怎么存」，不重复定义配置口径。
const (
	// MaxRows 硬行顶：与时间窗无关的磁盘兜底（≈2e6×40B ≈ 80MB）。
	MaxRows int64 = 2_000_000

	// DefaultStepPoints 自动降采样的目标点数上限；DefaultLimit 每条序列返回点数上限。
	DefaultStepPoints = 120
	DefaultLimit      = 500

	// MaxQueryLimit 每条序列可请求的点数上界（读视图据此夹取，避免一次拉爆内存）。
	MaxQueryLimit = 10_000

	// currentSchema 本库 schema 版本（PRAGMA user_version）。
	currentSchema = 1

	// metaLastTick / metaLastError 采样器心跳键（读视图据此如实报「上次采样/上次错误」）。
	metaLastTick  = "last_tick_ts"
	metaLastError = "last_error"
)

// Sample 一条待写入的采样样本（Name 为不含标签的指标名，与 metrics.Sample 同口径）。
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Point 序列上的一个时间点。
type Point struct {
	TS    int64
	Value float64
}

// Series 一条历史序列（指标名 + 标签集 + 时间点）。
type Series struct {
	Name   string
	Labels map[string]string
	Points []Point
}

// Query 历史查询参数。Name 必填（读视图按指标取，不支持一次全量拉取）。
// Since/Until 为 unix 秒闭区间；Step>0 时按桶降采样（取桶内**最后一个**样本）；
// Limit>0 时每条序列最多返回该点数。
type Query struct {
	Name  string
	Since int64
	Until int64
	Step  int64
	Limit int
}

// Stats 存储概览（读视图的「库大小/序列数/样本数/时间范围」）。
type Stats struct {
	Series    int64
	Samples   int64
	OldestTS  int64 // 无数据时为 0
	NewestTS  int64 // 无数据时为 0
	SizeBytes int64 // 库文件大小（含 -wal/-shm 之外的主文件；取不到为 0）
}

// PruneResult 一次裁剪的结果（含**回读**后的实际读数——写成功 ≠ 收敛）。
type PruneResult struct {
	DeletedByAge   int64 // 因超出时间窗删除的行数
	DeletedByRows  int64 // 因超出硬行顶删除的行数
	OrphanSeries   int64 // 清理掉的孤立序列数
	SamplesAfter   int64 // 回读：裁剪后样本行数
	OldestTSAfter  int64 // 回读：裁剪后最旧样本时刻（无数据为 0）
	RetentionCutof int64 // 本次使用的时间窗截止（unix 秒）
}

// Store 历史时序库。并发安全：Append/Prune 串行（写锁），读走独立连接。
type Store struct {
	db   *sql.DB
	path string

	mu     sync.Mutex
	series map[string]int64 // 规范标签 JSON → series.id（进程内缓存，避免每次 SELECT）
}

const schema = `
CREATE TABLE IF NOT EXISTS series (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    name   TEXT NOT NULL,
    labels TEXT NOT NULL DEFAULT '{}',
    UNIQUE(name, labels)
);
CREATE TABLE IF NOT EXISTS samples (
    series_id INTEGER NOT NULL,
    ts        INTEGER NOT NULL,
    value     REAL    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_samples_ts        ON samples(ts);
CREATE INDEX IF NOT EXISTS idx_samples_series_ts ON samples(series_id, ts);
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);`

// Open 打开（必要时创建）历史库。DSN 与配置库同口径：WAL + busy_timeout + 立即写锁。
// 不创建父目录：目录不存在时如实报错（路径由装配层保证）。
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", dsnFor(path))
	if err != nil {
		return nil, fmt.Errorf("打开历史时序库: %w", err)
	}
	db.SetMaxOpenConns(4)
	s := &Store{db: db, path: path, series: map[string]int64{}}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// dsnFor 库 DSN（WAL + busy_timeout + 立即写锁，与配置库同口径）。
func dsnFor(path string) string {
	return "file:" + path + "?_journal_mode=WAL&_busy_timeout=5000&_txlock=immediate"
}

// ReopenIfReplaced 库文件已被删除/替换时重开（决策 #365；round142 体检 A4 的同族根治）。
//
// 背景（真机实证）：`request system storage format-data` 清数据分区、或操作者
// `rm /var/lib/nfvis/metrics.db` 之后，**运行中的连接仍持有已删除的 inode**——读视图继续
// 显示旧历史、磁盘空间也不释放（round148 真机：文件已 unlink，读视图仍报 80598 个样本、
// `lsof` 12 个 `metrics.db (deleted)` 句柄）。采样器每轮 tick 与读视图入口都调用本方法：
// 文件不存在 ⇒ 关旧连接（释放已删 inode）并按当前路径重开（Open 会重建 schema，读数回到
// 「无历史」）；文件仍在 ⇒ 不动。
//
// 不做 inode 比较：跨平台无稳定口径，且本产品的删除者只有 format-data 与操作者本人
// （「删除后立刻被他人重建」的窗口不适用，如实登记）。
func (s *Store) ReopenIfReplaced() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查历史库文件 %s: %w", s.path, err)
	}
	if s.db != nil {
		_ = s.db.Close() // 释放已删 inode（空间随之回落）
	}
	db, err := sql.Open("sqlite", dsnFor(s.path))
	if err != nil {
		return fmt.Errorf("重开历史时序库: %w", err)
	}
	db.SetMaxOpenConns(4)
	s.db, s.series = db, map[string]int64{}
	if err := s.init(); err != nil {
		_ = db.Close()
		return err
	}
	return nil
}

func (s *Store) init() error {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("读取历史库 schema 版本: %w", err)
	}
	if v > currentSchema {
		return fmt.Errorf("历史库 schema 版本 %d 高于当前支持版本 %d", v, currentSchema)
	}
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("初始化历史库表结构: %w", err)
	}
	// PRAGMA 不接受参数绑定，用常量拼接（currentSchema 是编译期常量，非外部输入）。
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", currentSchema)); err != nil {
		return fmt.Errorf("写入历史库 schema 版本: %w", err)
	}
	return nil
}

// Close 关闭库。
func (s *Store) Close() error { return s.db.Close() }

// Path 库文件路径。
func (s *Store) Path() string { return s.path }

// labelsJSON 标签的规范形式：键排序的 JSON 对象（json.Marshal 对 map 键排序，确定性）。
// nil/空标签 → `{}`（唯一约束需要非空稳定串）。
func labelsJSON(l map[string]string) string {
	if len(l) == 0 {
		return "{}"
	}
	b, err := json.Marshal(l)
	if err != nil {
		return "{}" // map[string]string 不会失败；兜底保守
	}
	return string(b)
}

// parseLabels 规范形式还原为标签集；无法解析时如实返回空集（不编造键值）。
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	if s == "" || s == "{}" {
		return out
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return map[string]string{}
	}
	return out
}

// seriesID 取（必要时建）序列 id。调用方须持有 s.mu。
func (s *Store) seriesID(tx *sql.Tx, name, labels string) (int64, error) {
	key := name + "\x00" + labels
	if id, ok := s.series[key]; ok {
		return id, nil
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO series(name, labels) VALUES(?, ?)`, name, labels); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRow(`SELECT id FROM series WHERE name = ? AND labels = ?`, name, labels).Scan(&id); err != nil {
		return 0, err
	}
	s.series[key] = id
	return id, nil
}

// Append 写入一批样本（ts 为采样时刻，unix 秒）。整批一个事务：要么全落，要么不落。
// 空批次是空操作（采样器在「本轮什么都取不到」时不该制造空洞心跳）。
func (s *Store) Append(ts int64, samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开启写入事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`INSERT INTO samples(series_id, ts, value) VALUES(?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("准备写入语句: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, sm := range samples {
		id, err := s.seriesID(tx, sm.Name, labelsJSON(sm.Labels))
		if err != nil {
			return fmt.Errorf("登记序列 %s: %w", sm.Name, err)
		}
		if _, err := stmt.Exec(id, ts, sm.Value); err != nil {
			return fmt.Errorf("写入样本 %s: %w", sm.Name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交写入事务: %w", err)
	}
	return nil
}

// QueryResult 查询结果。Truncated 为**真值**：有任何一条序列的原始点数超过 Limit
// 而被裁掉（只保留最新 Limit 个点）——不是「点数刚好等于 Limit」的猜测。
type QueryResult struct {
	Series    []Series
	Truncated bool
}

// Query 取某指标在窗口内的序列。Step>0 时按 ts/Step 分桶、每桶取**最后一个**样本
// （与「瞬时值」语义一致；不做平均，不伪造 rate）。
func (s *Store) Query(q Query) (QueryResult, error) {
	if q.Name == "" {
		return QueryResult{}, errors.New("必须指定指标名")
	}
	if q.Until <= 0 {
		q.Until = 1<<62 - 1
	}
	rows, err := s.db.Query(
		`SELECT id, labels FROM series WHERE name = ? ORDER BY labels`, q.Name)
	if err != nil {
		return QueryResult{}, fmt.Errorf("查询序列: %w", err)
	}
	type meta struct {
		id     int64
		labels map[string]string
	}
	var metas []meta
	for rows.Next() {
		var id int64
		var labels string
		if err := rows.Scan(&id, &labels); err != nil {
			_ = rows.Close()
			return QueryResult{}, err
		}
		metas = append(metas, meta{id: id, labels: parseLabels(labels)})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return QueryResult{}, err
	}
	_ = rows.Close()

	res := QueryResult{Series: make([]Series, 0, len(metas))}
	for _, m := range metas {
		pts, truncated, err := s.points(m.id, q)
		if err != nil {
			return QueryResult{}, err
		}
		if truncated {
			res.Truncated = true
		}
		if len(pts) == 0 {
			continue // 窗口内无点：不发空序列
		}
		res.Series = append(res.Series, Series{Name: q.Name, Labels: m.labels, Points: pts})
	}
	return res, nil
}

func (s *Store) points(seriesID int64, q Query) ([]Point, bool, error) {
	// 决策 #372（R142 A8）：载入内存**有界**——此前把窗口内全部原始行读进内存后才裁 limit
	// （注释宣称防拉爆内存，实际未防）。
	//   · step>0：把窗口收窄到「最新 limit+1 个**有值桶**」的起点（决策 #397 更正判据），
	//     与「全窗口 + 降采样 + 裁 limit」**逐点等价**——更旧的桶本来就会被裁掉；
	//   · step<=0：SQL 侧 ORDER BY ts DESC LIMIT limit+1 再反转（保留最新 limit 个）。
	//
	// 决策 #376 的锚点教训（不再需要单独取 newestTS）：收窄必须以**窗口内真实存在的桶**为锚，
	// 不能拿 q.Until（读视图的 until 恒为墙钟 now）当锚。
	// 决策 #397（R171-17）更正：此前按**时间宽度** (limit+1)*step 收窄，只在桶稠密
	// （step ≥ 采样间隔）时等价；step 比采样间隔细时（真机 60s 采样 + step 1s）窗口只覆盖
	// 少数几个桶，会**丢掉本应保留的旧点并谎报 truncated**（`last 1h step 1s` 只回 9 点）。
	since, descLimit, truncated := q.Since, 0, false
	if q.Limit > 0 {
		if q.Step > 0 {
			var err error
			since, truncated, err = s.narrowSinceByBuckets(seriesID, q)
			if err != nil {
				return nil, false, err
			}
		} else {
			descLimit = q.Limit + 1
		}
	}
	q.Since = since
	query := `SELECT ts, value FROM samples WHERE series_id = ? AND ts >= ? AND ts <= ? ORDER BY ts`
	if descLimit > 0 {
		query = `SELECT ts, value FROM samples WHERE series_id = ? AND ts >= ? AND ts <= ? ORDER BY ts DESC LIMIT ?`
	}
	var rows *sql.Rows
	var err error
	if descLimit > 0 {
		rows, err = s.db.Query(query, seriesID, q.Since, q.Until, descLimit)
	} else {
		rows, err = s.db.Query(query, seriesID, q.Since, q.Until)
	}
	if err != nil {
		return nil, false, fmt.Errorf("查询样本: %w", err)
	}
	var raw []Point
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.TS, &p.Value); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		raw = append(raw, p)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, false, err
	}
	_ = rows.Close()
	if descLimit > 0 {
		// DESC 取回：行数超 limit ⇒ 真值截断；保留最新 limit 个（反转成升序）。
		if len(raw) > q.Limit {
			raw = raw[:q.Limit]
			truncated = true
		}
		for i, j := 0, len(raw)-1; i < j; i, j = i+1, j-1 {
			raw[i], raw[j] = raw[j], raw[i]
		}
	}

	pts := downsample(raw, q.Step)
	if q.Limit > 0 && len(pts) > q.Limit {
		// 保留**最新**的 Limit 个点（回溯以近端为本）——这是**真的**丢了数据，如实置位。
		pts = pts[len(pts)-q.Limit:]
		truncated = true
	}
	return pts, truncated, nil
}

// narrowSinceByBuckets 决策 #397（R171-17）：step>0 且有 limit 时，按「最新 limit+1 个
// **有值桶**」求收窄下界——与「全窗口 + 降采样 + 裁 limit」逐点等价（更旧的桶本来就会被裁掉）。
//
// 返回 (since, truncated, err)：
//   - 窗口内有值桶数 ≤ limit：不收窄（since 原样）、truncated=false（没被裁）；
//   - 有值桶数 > limit：truncated=true（真值截断），since 收窄到第 limit+1 个（最旧）有值桶的
//     起点——桶号 b 的最小 ts 为 b*step，取 ts ≥ b*step 恰好完整包含该桶、排除更旧桶；若窗口
//     起点本就更晚（cut ≤ q.Since）则原样，但截断仍为真。
//
// 桶号用 SQL 整数除法 ts/step 计算，与 downsample 的 p.TS/step 是**同一判据**（单一真源）；
// 结果行数被 LIMIT limit+1 界定，且 DISTINCT 结果随 ts 单调（ts/step 随 ts 非减），故载入有界。
func (s *Store) narrowSinceByBuckets(seriesID int64, q Query) (int64, bool, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT ts / ? FROM samples WHERE series_id = ? AND ts >= ? AND ts <= ? ORDER BY 1 DESC LIMIT ?`,
		q.Step, seriesID, q.Since, q.Until, q.Limit+1)
	if err != nil {
		return 0, false, fmt.Errorf("统计窗口内有值桶: %w", err)
	}
	defer func() { _ = rows.Close() }()
	buckets := make([]int64, 0, q.Limit+1)
	for rows.Next() {
		var b int64
		if err := rows.Scan(&b); err != nil {
			return 0, false, err
		}
		buckets = append(buckets, b)
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	if len(buckets) <= q.Limit {
		return q.Since, false, nil // 有值桶数未超 limit：不裁、不收窄
	}
	// buckets 按桶号降序；末位＝第 limit+1 个（最旧）有值桶。cut 为该桶的起点 ts。
	oldest := buckets[len(buckets)-1]
	if cut := oldest * q.Step; cut > q.Since {
		return cut, true, nil
	}
	return q.Since, true, nil // 窗口起点已晚于该桶起点：无需收窄，但截断为真
}

// downsample 按桶取最后一个样本。step<=0 时原样返回。
func downsample(pts []Point, step int64) []Point {
	if step <= 0 || len(pts) == 0 {
		return pts
	}
	out := make([]Point, 0, len(pts))
	var curBucket int64
	first := true
	var last Point
	for _, p := range pts {
		b := p.TS / step
		if first || b != curBucket {
			if !first {
				out = append(out, last)
			}
			curBucket = b
			first = false
		}
		last = p
	}
	if !first {
		out = append(out, last)
	}
	return out
}

// MetricNames 已知指标名（去重、按名排序）——供 CLI 动态补全与读视图列举。
func (s *Store) MetricNames() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT name FROM series ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("查询指标名: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Stats 存储概览。SizeBytes 取主库文件大小；取不到记 0（不编造）。
func (s *Store) Stats() (Stats, error) {
	var st Stats
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM series`).Scan(&st.Series); err != nil {
		return st, fmt.Errorf("统计序列数: %w", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(MIN(ts), 0), COALESCE(MAX(ts), 0) FROM samples`).
		Scan(&st.Samples, &st.OldestTS, &st.NewestTS); err != nil {
		return st, fmt.Errorf("统计样本数: %w", err)
	}
	if fi, err := os.Stat(s.path); err == nil {
		st.SizeBytes = fi.Size()
	}
	return st, nil
}

// Prune 按时间窗与硬行顶双界裁剪，随后清理孤立序列，并**回读**实际结果。
// maxRows<=0 表示不启用行顶。
func (s *Store) Prune(cutoff, maxRows int64) (PruneResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := PruneResult{RetentionCutof: cutoff}
	tx, err := s.db.Begin()
	if err != nil {
		return res, fmt.Errorf("开启裁剪事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	byAge, err := tx.Exec(`DELETE FROM samples WHERE ts < ?`, cutoff)
	if err != nil {
		return res, fmt.Errorf("按时间窗裁剪: %w", err)
	}
	if n, err := byAge.RowsAffected(); err == nil {
		res.DeletedByAge = n
	}

	if maxRows > 0 {
		var total int64
		if err := tx.QueryRow(`SELECT COUNT(*) FROM samples`).Scan(&total); err != nil {
			return res, fmt.Errorf("统计样本行数: %w", err)
		}
		if excess := total - maxRows; excess > 0 {
			rows, err := tx.Exec(
				`DELETE FROM samples WHERE rowid IN (SELECT rowid FROM samples ORDER BY ts, rowid LIMIT ?)`, excess)
			if err != nil {
				return res, fmt.Errorf("按行顶裁剪: %w", err)
			}
			if n, err := rows.RowsAffected(); err == nil {
				res.DeletedByRows = n
			}
		}
	}

	orph, err := tx.Exec(`DELETE FROM series WHERE id NOT IN (SELECT DISTINCT series_id FROM samples)`)
	if err != nil {
		return res, fmt.Errorf("清理孤立序列: %w", err)
	}
	if n, err := orph.RowsAffected(); err == nil {
		res.OrphanSeries = n
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("提交裁剪事务: %w", err)
	}

	// 回读：裁剪后的事实（写成功 ≠ 收敛）。
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(MIN(ts), 0) FROM samples`).
		Scan(&res.SamplesAfter, &res.OldestTSAfter); err != nil {
		return res, fmt.Errorf("回读裁剪结果: %w", err)
	}
	if res.OrphanSeries > 0 {
		s.series = map[string]int64{} // 序列被清理：缓存整体失效（下次按需重建）
	}
	return res, nil
}

// SetMeta / Meta：采样器心跳（last_tick_ts / last_error）与将来的元信息。
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	if err != nil {
		return fmt.Errorf("写入心跳 %s: %w", key, err)
	}
	return nil
}

// Meta 读心跳；不存在时 ok=false（不编造）。
func (s *Store) Meta(key string) (value string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读取心跳 %s: %w", key, err)
	}
	return value, true, nil
}

// SetLastTick 记录采样器心跳（unix 秒）并清空上一次的错误。
func (s *Store) SetLastTick(ts int64) error {
	if err := s.SetMeta(metaLastTick, fmt.Sprintf("%d", ts)); err != nil {
		return err
	}
	return s.SetMeta(metaLastError, "")
}

// SetLastError 记录整轮采样错误（空串＝无错误）。
func (s *Store) SetLastError(msg string) error { return s.SetMeta(metaLastError, msg) }

// Health 采样器心跳读数：lastTickTS（无记录为 0）、lastError（无/空为 ""）。
func (s *Store) Health() (lastTickTS int64, lastError string, err error) {
	v, ok, err := s.Meta(metaLastTick)
	if err != nil {
		return 0, "", err
	}
	if ok {
		if _, e := fmt.Sscanf(v, "%d", &lastTickTS); e != nil {
			lastTickTS = 0
		}
	}
	e, ok, err := s.Meta(metaLastError)
	if err != nil {
		return lastTickTS, "", err
	}
	if ok {
		lastError = e
	}
	return lastTickTS, lastError, nil
}
