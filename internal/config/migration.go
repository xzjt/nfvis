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
//
// round49 缺陷 #4（中危）：迁移的 **DDL 与版本推进此前是两次独立提交**，两步之间断电/被杀
// 会留下「列已加、版本未前进」的半成品库；下次启动从同一版本再来一遍，`ALTER TABLE` 撞
// 「duplicate column name」直接失败——**服务再也起不来**（版本锁定语义把自己锁死）。
// 两处修法：① 每个步骤的 DDL 与版本推进**同一个事务**提交（全有或全无）；
// ② `ADD COLUMN` 幂等（先查 `PRAGMA table_info`，列已存在即视为已应用），历史半成品库可启动。
const CurrentSchemaVersion = 4

// migrationDB 迁移步骤可用的最小接口：`*sql.DB` 与 `*sql.Tx` 都满足。
//
// 取接口而非具体类型有两个用：迁移步骤在事务里跑（传 *sql.Tx）；既有的迁移测试
// （history_test.go / audit_clock_test.go）直接用 `*sql.DB` 调 `schemaMigrations[n]`，签名不变即
// 继续可用。
type migrationDB interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
}

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
//
// 每一步都经 addColumnIfMissing（列已存在即视为已应用，见 round49 缺陷 #4）。
var schemaMigrations = map[int]func(migrationDB) error{
	1: func(q migrationDB) error {
		return addColumnIfMissing(q, "audit_log", "time_synced", `INTEGER`)
	},
	2: func(q migrationDB) error {
		return addColumnIfMissing(q, "config_revisions", "user", `TEXT`)
	},
	3: func(q migrationDB) error {
		return addColumnIfMissing(q, "candidate_lock", "session_id", `TEXT NOT NULL DEFAULT ''`)
	},
}

// addColumnIfMissing 幂等地给表加一列：列已存在（历史半成品库：DDL 提交过、版本没跟上）
// 即视为该步骤已应用，不再重复 ALTER——重复 ALTER 会以 `duplicate column name` 失败，
// 从而让服务永远起不来（round49 缺陷 #4 的现场）。
func addColumnIfMissing(q migrationDB, table, column, decl string) error {
	has, err := columnExists(q, table, column)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := q.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, decl)); err != nil {
		return fmt.Errorf("%s 增加 %s 列: %w", table, column, err)
	}
	return nil
}

// columnExists 该表是否已有该列（`PRAGMA table_info` 的 cid/name 列表）。
// 表不存在时返回 false（后续 ALTER 会如实报「no such table」）。
func columnExists(q migrationDB, table, column string) (bool, error) {
	rows, err := q.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, fmt.Errorf("读取 %s 表结构: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		// table_info 的列：cid, name, type, notnull, dflt_value, pk
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("读取 %s 表结构: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("读取 %s 表结构: %w", table, err)
	}
	return false, nil
}

func userVersion(db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("读取存储版本: %w", err)
	}
	return v, nil
}

// setVersion 写入存储版本。参数取接口：迁移步骤在事务里推进版本（`*sql.Tx`），
// 既有的迁移测试仍以 `*sql.DB` 调用（签名兼容）。
func setVersion(q migrationDB, v int) error {
	if _, err := q.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v)); err != nil {
		return fmt.Errorf("写入存储版本: %w", err)
	}
	return nil
}
