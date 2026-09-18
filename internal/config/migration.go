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
const CurrentSchemaVersion = 2

// schemaMigrations 迁移链（v1 起）。
//
// v1→v2：审计表加 `time_synced` 列（NFR-006：未同步时事件带未同步标记）。
// 列为**可空**：迁移前写入的老记录保持 NULL（= 当时未记录，**不谎称已知**），
// 新记录写 1/0。纯增量、不触碰既有列，故对老库安全（FR-OPS-003：升级不触碰配置数据）。
var schemaMigrations = map[int]func(*sql.DB) error{
	1: func(db *sql.DB) error {
		if _, err := db.Exec(`ALTER TABLE audit_log ADD COLUMN time_synced INTEGER`); err != nil {
			return fmt.Errorf("audit_log 增加 time_synced 列: %w", err)
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
