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
	ErrLocked     = errors.New("candidate 会话锁被占用")
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
	// TimeSynced：写入该条时的**宿主时钟是否已与 NTP 同步**（NFR-006：时间戳依赖 NTP，
	// 未同步时事件要带标记）。nil = 未知——迁移前的老记录没有这个信息，
	// **不谎称已知**（渲染层据此显示「未记录」而不是「已同步」）。
	TimeSynced *bool
}

// LockInfo candidate 会话锁持有信息（FR-CFG-009）。
//
// 决策 #317：Holder 是**展示/审计**用的身份键（user@source）；SessionID 是持锁会话的
// **稳定标识**（REST 侧为 token 稳定 ID，决策 #301），两把不同的锁靠它区分（同一用户在
// 同一 Source 下可以有两个会话）。匹配键见 key()。
type LockInfo struct {
	Holder       string
	SessionID    string
	AcquiredAt   time.Time
	LastActivity time.Time
}

// key 会话锁的匹配键：身份键 + 稳定会话 ID（与 config.Session.key 同一算法，单源在
// sessionKeyFor）。迁移前写入的老锁 session_id 为空（退回身份键），与旧语义一致。
func (li LockInfo) key() string { return sessionKeyFor(li.Holder, li.SessionID) }

// ConfirmedInfo commit confirmed 待确认状态（FR-CFG-003/004）。
type ConfirmedInfo struct {
	BaseRev  int
	Deadline time.Time
	Holder   string
}

// RevisionInfo 一份 committed 快照的**元数据**（决策 #142：配置提交历史列表）。
//
// 有意**不含配置正文**（config_json）：历史列表只回答「第几版、什么时候、谁提交的、
// 说明了什么」，配置内容要看差异（Engine.Compare）或回滚为 candidate 再看——
// 这样历史出口天然不带敏感字段（FR-SEC-007：快照里可能有口令哈希）。
type RevisionInfo struct {
	Rev         int
	CommittedAt time.Time
	// User 提交者。迁移前（存储 schema v3 之前）写入的快照没有这个信息 → 空串
	// （**不谎称已知**，渲染层据此显示「未记录」）。
	User string
	// Message 提交说明（commit 的 message），未填时为空串。
	Message string
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

// migrate 把存储升级到 CurrentSchemaVersion。
//
// round49 缺陷 #4（中危）：此前每步 `step(s.db)` 之后**单独** `setVersion(s.db, v+1)`，DDL 与
// 版本推进分两个事务——两步之间断电/被杀就留下「列已加、版本未前进」的半成品库，下次启动
// 从同一版本重跑，`ALTER TABLE` 撞 `duplicate column name` 直接失败，**服务再也起不来**。
// 现在每个步骤的 DDL 与版本推进在**同一个事务**内提交：要么该步骤整体完成（含版本推进），
// 要么整体未发生（下次启动从该步骤重来，且步骤本身也已幂等——见 addColumnIfMissing）。
// v0→1 的建表同理与版本推进同事务。
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
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("开启存储初始化事务: %w", err)
		}
		if _, err := tx.Exec(baseSchema); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("初始化表结构: %w", err)
		}
		if err := setVersion(tx, 1); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("提交存储初始化: %w", err)
		}
		v = 1
	}
	for v < CurrentSchemaVersion {
		step := schemaMigrations[v]
		if step == nil {
			return fmt.Errorf("缺少存储迁移步骤 %d→%d", v, v+1)
		}
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("开启存储迁移事务 %d→%d: %w", v, v+1, err)
		}
		if err := step(tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("存储迁移 %d→%d: %w", v, v+1, err)
		}
		if err := setVersion(tx, v+1); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("提交存储迁移 %d→%d: %w", v, v+1, err)
		}
		v++
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
// user 为提交者（决策 #142：历史列表要给出「谁提交的」）；由守护进程自动产生的
// 快照（初始化、confirmed 超时回滚）写 "system"。
func (s *Store) AppendRevision(configJSON []byte, at time.Time, message, user string) (int, error) {
	res, err := s.db.Exec(
		`INSERT INTO config_revisions (committed_at, message, user, config_json) VALUES (?, ?, ?, ?)`,
		fmtTime(at), message, user, configJSON,
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

// ListRevisions 返回最近 limit 份修订的元数据，**最新在前**（决策 #142：
// 配置提交历史；不读 config_json，故不带配置正文）。limit <= 0 时取全部。
func (s *Store) ListRevisions(limit int) ([]RevisionInfo, error) {
	if limit <= 0 {
		limit = storeKeepRevisions
	}
	rows, err := s.db.Query(
		`SELECT rev, committed_at, COALESCE(user, ''), message FROM config_revisions ORDER BY rev DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("查询修订历史: %w", err)
	}
	defer rows.Close()
	var out []RevisionInfo
	for rows.Next() {
		var ri RevisionInfo
		var at string
		if err := rows.Scan(&ri.Rev, &at, &ri.User, &ri.Message); err != nil {
			return nil, fmt.Errorf("读取修订记录: %w", err)
		}
		if ri.CommittedAt, err = parseTime(at); err != nil {
			return nil, fmt.Errorf("解析提交时间: %w", err)
		}
		out = append(out, ri)
	}
	return out, rows.Err()
}

// AppendCommit 在**单个事务**内完成一次提交的落库：追加修订 + 修剪历史 + 写/清 confirmed。
//
// 由来（round49 缺陷 #3，中危）：`Engine.Commit` 此前依次**独立**调用 AppendRevision /
// PruneRevisions / SetConfirmed，而配置库全文没有 BeginTx —— 中途任一步失败（磁盘写满、
// 进程被杀、SQL 报错）都会留下「数据面已生效、修订已写、调用方却收到失败、**confirmed 保护
// 未建立**」的中间态：数据面在跑新版、配置面以为这次提交失败了，且危险变更的超时保护
// 根本不存在。同为「已提交变更」的自动回滚（追加回滚修订 + 清 confirmed）同样只在
// 全有或全无时才有意义。
//
// confirm 语义三态：
//   - 非 nil：写入待确认状态，`BaseRev` 由本方法**在事务内**按「新修订号 - 1」落定
//     （confirmed 的基线恒为「本次提交之前的那一版」，调用方无需、也不应自行推算）；
//     Deadline/Holder 由调用方给出。
//   - nil：在同一事务内**清除**待确认状态（无待确认的提交与自动回滚走这条路径；
//     清一个不存在的行是幂等的空操作）。
//
// keep <= 0 时跳过修剪（与 PruneRevisions 的语义区分开：这里显式取值）。
func (s *Store) AppendCommit(configJSON []byte, at time.Time, message, user string, confirm *ConfirmedInfo, keep int) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("开启提交事务: %w", err)
	}
	// 已 Commit 时 Rollback 返回 ErrTxDone（忽略）；未 Commit 的任一早退都靠它兜底。
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(
		`INSERT INTO config_revisions (committed_at, message, user, config_json) VALUES (?, ?, ?, ?)`,
		fmtTime(at), message, user, configJSON,
	)
	if err != nil {
		return 0, fmt.Errorf("追加修订: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("获取修订号: %w", err)
	}
	rev := int(id)

	if keep > 0 {
		if _, err := tx.Exec(
			`DELETE FROM config_revisions WHERE rev NOT IN (
				SELECT rev FROM config_revisions ORDER BY rev DESC LIMIT ?
			)`, keep,
		); err != nil {
			return 0, fmt.Errorf("清理历史快照: %w", err)
		}
	}

	if confirm == nil {
		if _, err := tx.Exec(`DELETE FROM confirmed_pending WHERE pending_id = 1`); err != nil {
			return 0, fmt.Errorf("清除 confirmed 状态: %w", err)
		}
	} else {
		if _, err := tx.Exec(
			`INSERT INTO confirmed_pending (pending_id, base_rev, deadline, holder)
			 VALUES (1, ?, ?, ?)
			 ON CONFLICT(pending_id) DO UPDATE SET base_rev = excluded.base_rev,
			   deadline = excluded.deadline, holder = excluded.holder`,
			rev-1, fmtTime(confirm.Deadline), confirm.Holder,
		); err != nil {
			return 0, fmt.Errorf("记录 confirmed 状态: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交事务: %w", err)
	}
	return rev, nil
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
//
// holder = 展示用身份键（user@source），sessionID = 会话稳定标识（决策 #317，空串表示
// 无稳定标识的旧式调用）。锁「被占用」的判据是二者构成的匹配键，不再只看身份键。
func (s *Store) AcquireLock(holder, sessionID string, at time.Time) error {
	res, err := s.db.Exec(
		`INSERT INTO candidate_lock (lock_id, holder, session_id, acquired_at, last_activity)
		 VALUES (1, ?, ?, ?, ?)
		 ON CONFLICT(lock_id) DO NOTHING`,
		holder, sessionID, fmtTime(at), fmtTime(at),
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

// RefreshLock 更新持有者的最后活动时间（空闲超时判定依据）。须由**同一会话**调用
// （holder 与 sessionID 共同定位，决策 #317）。
func (s *Store) RefreshLock(holder, sessionID string, at time.Time) error {
	res, err := s.db.Exec(
		`UPDATE candidate_lock SET last_activity = ? WHERE lock_id = 1 AND holder = ? AND session_id = ?`,
		fmtTime(at), holder, sessionID,
	)
	if err != nil {
		return fmt.Errorf("刷新会话锁: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("会话锁 %q 未持有", holder)
	}
	return nil
}

// ReleaseLock 释放锁；非持有者（含同一身份键下的**其它会话**）报错。
func (s *Store) ReleaseLock(holder, sessionID string) error {
	res, err := s.db.Exec(
		`DELETE FROM candidate_lock WHERE lock_id = 1 AND holder = ? AND session_id = ?`, holder, sessionID,
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
		`SELECT holder, session_id, acquired_at, last_activity FROM candidate_lock WHERE lock_id = 1`,
	).Scan(&li.Holder, &li.SessionID, &acquired, &activity)
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
	var synced any // nil → SQL NULL（未知）
	if e.TimeSynced != nil {
		if *e.TimeSynced {
			synced = 1
		} else {
			synced = 0
		}
	}
	_, err := s.db.Exec(
		`INSERT INTO audit_log (ts, user, action, detail, result, time_synced) VALUES (?, ?, ?, ?, ?, ?)`,
		fmtTime(e.Time), e.User, e.Action, e.Detail, e.Result, synced,
	)
	if err != nil {
		return fmt.Errorf("写审计日志: %w", err)
	}
	return nil
}

// ListAudit 按时间倒序返回至多 limit 条审计记录，跳过前 offset 条（FR-API-007 分页）。
func (s *Store) ListAudit(limit, offset int) ([]AuditEntry, error) {
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.Query(
		`SELECT ts, user, action, detail, result, time_synced FROM audit_log ORDER BY audit_id DESC LIMIT ? OFFSET ?`, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("查询审计日志: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts string
		var synced sql.NullInt64
		if err := rows.Scan(&ts, &e.User, &e.Action, &e.Detail, &e.Result, &synced); err != nil {
			return nil, fmt.Errorf("读取审计记录: %w", err)
		}
		if synced.Valid {
			b := synced.Int64 != 0
			e.TimeSynced = &b
		}
		if e.Time, err = parseTime(ts); err != nil {
			return nil, fmt.Errorf("解析审计时间: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
