package config

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动（免 CGO，见骨架 §4）
)

// 事务引擎错误。ErrLocked 映射 API 409（会话锁占用），ErrNoRevision 映射 404。
var (
	ErrLocked     = errors.New("candidate 会话锁被占用（FR-CFG-009）")
	ErrNoRevision = errors.New("配置快照不存在")
)

// storeKeepRevisions 保留的 committed 快照数：当前 1 份 + 历史 50 份（FR-CFG-005）。
const storeKeepRevisions = 51

// AuditEntry 配置变更审计记录（FR-CFG-010）。
type AuditEntry struct {
	Time   time.Time
	User   string
	Action string // config.commit / config.rollback / config.rollback-auto / config.confirm
	Detail string // JunOS 风格 diff 或操作说明
	Result string // success | failure
}

// LockInfo candidate 会话锁持有信息（FR-CFG-009）。
type LockInfo struct {
	Holder       string
	AcquiredAt   time.Time
	LastActivity time.Time
}

// ConfirmedInfo commit confirmed 待确认状态（FR-CFG-003/004）。
type ConfirmedInfo struct {
	BaseRev  int
	Deadline time.Time
	Holder   string
}

// Store SQLite 持久化：committed 快照、candidate 会话锁、confirmed 待确认、审计日志。
type Store struct {
	db *sql.DB
}

// OpenStore 打开（必要时创建）存储。DSN 启用 WAL 与 busy_timeout；
// 事务引擎内部串行（单写），_txlock=immediate 防止多连接写竞争。
func OpenStore(path string) (*Store, error) {
	dsn := "file:" + filepath.ToSlash(path) + "?_journal_mode=WAL&_busy_timeout=5000&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite: %w", err)
	}
	db.SetMaxOpenConns(4)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// baseSchema 初始表结构（v0→1）。
const baseSchema = `
CREATE TABLE IF NOT EXISTS config_revisions (
    rev          INTEGER PRIMARY KEY AUTOINCREMENT,
    committed_at TEXT NOT NULL,
    message      TEXT NOT NULL DEFAULT '',
    config_json  BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS candidate_lock (
    lock_id       INTEGER PRIMARY KEY CHECK (lock_id = 1),
    holder        TEXT NOT NULL,
    acquired_at   TEXT NOT NULL,
    last_activity TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS confirmed_pending (
    pending_id INTEGER PRIMARY KEY CHECK (pending_id = 1),
    base_rev   INTEGER NOT NULL,
    deadline   TEXT NOT NULL,
    holder     TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS audit_log (
    audit_id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts       TEXT NOT NULL,
    user     TEXT NOT NULL,
    action   TEXT NOT NULL,
    detail   TEXT NOT NULL,
    result   TEXT NOT NULL
);`

func (s *Store) migrate() error {
	v, err := userVersion(s.db)
	if err != nil {
		return err
	}
	if v > CurrentSchemaVersion {
		// 版本锁定语义（规格书 §2.3.4）：不认识的 schema 拒绝启动
		return fmt.Errorf("存储 schema 版本 %d 高于当前支持版本 %d", v, CurrentSchemaVersion)
	}
	if v == 0 {
		if _, err := s.db.Exec(baseSchema); err != nil {
			return fmt.Errorf("初始化表结构: %w", err)
		}
		v = 1
		if err := setVersion(s.db, v); err != nil {
			return err
		}
	}
	for v < CurrentSchemaVersion {
		step := schemaMigrations[v]
		if step == nil {
			return fmt.Errorf("缺少存储迁移步骤 %d→%d", v, v+1)
		}
		if err := step(s.db); err != nil {
			return fmt.Errorf("存储迁移 %d→%d: %w", v, v+1, err)
		}
		v++
		if err := setVersion(s.db, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

// LatestRevision 返回当前 committed 的修订号与配置 JSON；空库返回 (0, nil, nil)。
func (s *Store) LatestRevision() (int, []byte, error) {
	var rev int
	var data []byte
	err := s.db.QueryRow(
		`SELECT rev, config_json FROM config_revisions ORDER BY rev DESC LIMIT 1`,
	).Scan(&rev, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("查询最新修订: %w", err)
	}
	return rev, data, nil
}

// LoadRevision 读取指定修订的配置 JSON。
func (s *Store) LoadRevision(rev int) ([]byte, error) {
	var data []byte
	err := s.db.QueryRow(
		`SELECT config_json FROM config_revisions WHERE rev = ?`, rev,
	).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("rev %d: %w", rev, ErrNoRevision)
	}
	if err != nil {
		return nil, fmt.Errorf("读取修订 %d: %w", rev, err)
	}
	return data, nil
}

// AppendRevision 追加一份 committed 快照（追加式，天然满足
// 「快照必须在覆盖 committed 之前拍」，见 AGENTS.md 常见错误）。
func (s *Store) AppendRevision(configJSON []byte, at time.Time, message string) (int, error) {
	res, err := s.db.Exec(
		`INSERT INTO config_revisions (committed_at, message, config_json) VALUES (?, ?, ?)`,
		fmtTime(at), message, configJSON,
	)
	if err != nil {
		return 0, fmt.Errorf("追加修订: %w", err)
	}
	rev, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("获取修订号: %w", err)
	}
	return int(rev), nil
}

// PruneRevisions 只保留最近 keep 份快照。
func (s *Store) PruneRevisions(keep int) error {
	_, err := s.db.Exec(
		`DELETE FROM config_revisions WHERE rev NOT IN (
			SELECT rev FROM config_revisions ORDER BY rev DESC LIMIT ?
		)`, keep,
	)
	if err != nil {
		return fmt.Errorf("清理历史快照: %w", err)
	}
	return nil
}

// AcquireLock 获取 candidate 会话锁；被占用时返回 ErrLocked（包装持有者）。
func (s *Store) AcquireLock(holder string, at time.Time) error {
	res, err := s.db.Exec(
		`INSERT INTO candidate_lock (lock_id, holder, acquired_at, last_activity)
		 VALUES (1, ?, ?, ?)
		 ON CONFLICT(lock_id) DO NOTHING`,
		holder, fmtTime(at), fmtTime(at),
	)
	if err != nil {
		return fmt.Errorf("获取会话锁: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		cur, gerr := s.GetLock()
		if gerr != nil || cur == nil {
			return fmt.Errorf("%w", ErrLocked)
		}
		return fmt.Errorf("%w: 由 %s 持有", ErrLocked, cur.Holder)
	}
	return nil
}

// RefreshLock 更新持有者的最后活动时间（空闲超时判定依据）。
func (s *Store) RefreshLock(holder string, at time.Time) error {
	res, err := s.db.Exec(
		`UPDATE candidate_lock SET last_activity = ? WHERE lock_id = 1 AND holder = ?`,
		fmtTime(at), holder,
	)
	if err != nil {
		return fmt.Errorf("刷新会话锁: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("会话锁 %q 未持有", holder)
	}
	return nil
}

// ReleaseLock 释放锁；非持有者报错。
func (s *Store) ReleaseLock(holder string) error {
	res, err := s.db.Exec(
		`DELETE FROM candidate_lock WHERE lock_id = 1 AND holder = ?`, holder,
	)
	if err != nil {
		return fmt.Errorf("释放会话锁: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("会话锁 %q 未持有", holder)
	}
	return nil
}

// GetLock 返回当前锁信息；无锁返回 (nil, nil)。
func (s *Store) GetLock() (*LockInfo, error) {
	var li LockInfo
	var acquired, activity string
	err := s.db.QueryRow(
		`SELECT holder, acquired_at, last_activity FROM candidate_lock WHERE lock_id = 1`,
	).Scan(&li.Holder, &acquired, &activity)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询会话锁: %w", err)
	}
	if li.AcquiredAt, err = parseTime(acquired); err != nil {
		return nil, fmt.Errorf("解析加锁时间: %w", err)
	}
	if li.LastActivity, err = parseTime(activity); err != nil {
		return nil, fmt.Errorf("解析活动时间: %w", err)
	}
	return &li, nil
}

// SetConfirmed 记录 commit confirmed 待确认状态（单行，覆盖旧值）。
func (s *Store) SetConfirmed(baseRev int, deadline time.Time, holder string) error {
	_, err := s.db.Exec(
		`INSERT INTO confirmed_pending (pending_id, base_rev, deadline, holder)
		 VALUES (1, ?, ?, ?)
		 ON CONFLICT(pending_id) DO UPDATE SET base_rev = excluded.base_rev,
		   deadline = excluded.deadline, holder = excluded.holder`,
		baseRev, fmtTime(deadline), holder,
	)
	if err != nil {
		return fmt.Errorf("记录 confirmed 状态: %w", err)
	}
	return nil
}

// GetConfirmed 返回待确认状态；无则 (nil, nil)。
func (s *Store) GetConfirmed() (*ConfirmedInfo, error) {
	var cf ConfirmedInfo
	var deadline string
	err := s.db.QueryRow(
		`SELECT base_rev, deadline, holder FROM confirmed_pending WHERE pending_id = 1`,
	).Scan(&cf.BaseRev, &deadline, &cf.Holder)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询 confirmed 状态: %w", err)
	}
	if cf.Deadline, err = parseTime(deadline); err != nil {
		return nil, fmt.Errorf("解析 confirmed 截止时间: %w", err)
	}
	return &cf, nil
}

// ClearConfirmed 清除待确认状态（确认或回滚完成后）。
func (s *Store) ClearConfirmed() error {
	_, err := s.db.Exec(`DELETE FROM confirmed_pending WHERE pending_id = 1`)
	if err != nil {
		return fmt.Errorf("清除 confirmed 状态: %w", err)
	}
	return nil
}

// AppendAudit 追加审计记录。
func (s *Store) AppendAudit(e AuditEntry) error {
	_, err := s.db.Exec(
		`INSERT INTO audit_log (ts, user, action, detail, result) VALUES (?, ?, ?, ?, ?)`,
		fmtTime(e.Time), e.User, e.Action, e.Detail, e.Result,
	)
	if err != nil {
		return fmt.Errorf("写审计日志: %w", err)
	}
	return nil
}

// ListAudit 按时间倒序返回至多 limit 条审计记录。
func (s *Store) ListAudit(limit int) ([]AuditEntry, error) {
	rows, err := s.db.Query(
		`SELECT ts, user, action, detail, result FROM audit_log ORDER BY audit_id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("查询审计日志: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts string
		if err := rows.Scan(&ts, &e.User, &e.Action, &e.Detail, &e.Result); err != nil {
			return nil, fmt.Errorf("读取审计记录: %w", err)
		}
		if e.Time, err = parseTime(ts); err != nil {
			return nil, fmt.Errorf("解析审计时间: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
