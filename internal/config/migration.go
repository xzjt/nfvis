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
const CurrentSchemaVersion = 1

// schemaMigrations 迁移链（v1 起，暂无历史版本需要迁移）。
var schemaMigrations = map[int]func(*sql.DB) error{}

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
