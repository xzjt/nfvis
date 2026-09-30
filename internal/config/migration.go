package config

import (
	"database/sql"
	"fmt"
)

// 存储版本迁移（骨架 internal/config/migration.go；FR-OPS-003：升级不触碰
// committed 配置，schema 兼容迁移由事务引擎负责）。
//
// 版本记录于 SQLite PRAGMA user_version。迁移链 schemaMigrations[n] 将版本 n
// 升至 n+1；v0→1 为初始建表（migrate 内联执行）。此后引入破坏性 schema 变更时：
// 追加 schemaMigrations[CurrentSchemaVersion] 并将常量递增。
const CurrentSchemaVersion = 4

// schemaMigrations 迁移链（v1 起）。
//
// v1→v2：审计表加 `time_synced` 列（NFR-006：未同步时事件带未同步标记）。
// 列为**可空**：迁移前写入的老记录保持 NULL（= 当时未记录，**不谎称已知**），
// 新记录写 1/0。纯增量、不触碰既有列，故对老库安全（FR-OPS-003：升级不触碰配置数据）。
//
// v2→v3：修订表加 `user` 列（决策 #142：配置提交历史要给出「谁提交的」）。
// 同样**可空**——迁移前的历史快照保持 NULL（当时的提交者没被记录，宁可空着
// 也不编造），API/CLI 回空串；新写入记录提交者。纯增量、不触碰既有列与既有数据。
//
// v3→v4：会话锁表加 `session_id` 列（决策 #317：会话与身份键分离，根治 R79-1）。
// **NOT NULL、默认空串**：迁移前已持有的锁其会话标识未知，置空串——引擎据此退回
// 「按身份键归并」的保守语义（只影响自己、不动他人），且 newEngine 装配时会释放
// 遗留锁（候选只存在于内存，重启后本就无候选）。纯增量、不触碰既有列与既有数据；
// 运维无需任何数据迁移。
var schemaMigrations = map[int]func(*sql.DB) error{
	1: func(db *sql.DB) error {
		if _, err := db.Exec(`ALTER TABLE audit_log ADD COLUMN time_synced INTEGER`); err != nil {
			return fmt.Errorf("audit_log 增加 time_synced 列: %w", err)
		}
		return nil
	},
	2: func(db *sql.DB) error {
		if _, err := db.Exec(`ALTER TABLE config_revisions ADD COLUMN user TEXT`); err != nil {
			return fmt.Errorf("config_revisions 增加 user 列: %w", err)
		}
		return nil
	},
	3: func(db *sql.DB) error {
		if _, err := db.Exec(`ALTER TABLE candidate_lock ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("candidate_lock 增加 session_id 列: %w", err)
		}
		return nil
	},
}

func userVersion(db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("读取存储版本: %w", err)
	}
	return v, nil
}

func setVersion(db *sql.DB, v int) error {
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v)); err != nil {
		return fmt.Errorf("写入存储版本: %w", err)
	}
	return nil
}
