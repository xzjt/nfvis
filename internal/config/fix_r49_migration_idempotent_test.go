package config

// round49 缺陷 #4（中危）：迁移链每步 DDL 与 `user_version` 推进**分开提交**，
// 且 `ALTER TABLE … ADD COLUMN` 不幂等。
//
// 现场：`migrate()` 每步先 `step(s.db)` 再单独 `setVersion(s.db, v+1)` —— 两步之间断电/被杀
// 就会留下「列已加、版本未前进」的半成品库；下次启动从同一版本再走一遍，`ALTER TABLE` 撞
// 「duplicate column name」直接失败，**服务拒绝启动**（版本锁定语义反而把自己锁死）。
// v0→1 的建表与版本推进同样分开提交（半成品库的表已建、版本为 0，靠 `CREATE TABLE IF NOT
// EXISTS` 侥幸能重入——但语义上仍是两个事务）。
//
// 修法：① 每个迁移步骤 + 版本推进包在**同一个事务**里提交（全有或全无）；② `ADD COLUMN`
// 幂等（先查 `PRAGMA table_info`，列已存在即视为已应用），使历史半成品库也能启动。

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestMigrationIdempotentWhenColumnsAlreadyAdded：造一个「列已加、user_version 落后」的库，
// 断言能正常打开、数据保留、版本推进到 CurrentSchemaVersion。
func TestMigrationIdempotentWhenColumnsAlreadyAdded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "halfdone.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := raw.Exec(baseSchema); err != nil {
		t.Fatalf("baseSchema: %v", err)
	}
	// 半成品：v1→v2 与 v2→v3 的列都已加上，但版本停在 1（模拟「DDL 提交了、版本没提交」）。
	if _, err := raw.Exec(`ALTER TABLE audit_log ADD COLUMN time_synced INTEGER`); err != nil {
		t.Fatalf("预加 time_synced: %v", err)
	}
	if _, err := raw.Exec(`ALTER TABLE config_revisions ADD COLUMN user TEXT`); err != nil {
		t.Fatalf("预加 user: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO config_revisions (committed_at, message, config_json) VALUES (?,?,?)`,
		"2026-10-01T00:00:00Z", "半成品库的旧快照", `{"system":{"hostname":"old"}}`); err != nil {
		t.Fatalf("插入旧快照: %v", err)
	}
	if err := setVersion(raw, 1); err != nil {
		t.Fatalf("setVersion: %v", err)
	}
	_ = raw.Close()

	s, err := OpenStore(path) // 此前会因 duplicate column name 直接失败
	if err != nil {
		t.Fatalf("列已存在但版本落后时应能启动（幂等迁移），实际: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if v, err := userVersion(s.db); err != nil || v != CurrentSchemaVersion {
		t.Fatalf("版本应推进到 %d，实得 %d err=%v", CurrentSchemaVersion, v, err)
	}
	got, err := s.ListRevisions(0)
	if err != nil || len(got) != 1 || got[0].Message != "半成品库的旧快照" {
		t.Fatalf("旧快照必须保留原样: %+v err=%v", got, err)
	}
	if got[0].User != "" {
		t.Fatalf("迁移前的老快照没有提交者信息，应为空串（不谎称已知），实得 %q", got[0].User)
	}
	data, err := s.LoadRevision(got[0].Rev)
	if err != nil || string(data) != `{"system":{"hostname":"old"}}` {
		t.Fatalf("升级不触碰配置数据: %s err=%v", data, err)
	}
	// 迁移完成后可正常写入（新列可用）
	if _, err := s.AppendRevision([]byte(`{}`), time.Now(), "after", "admin"); err != nil {
		t.Fatalf("迁移后追加修订: %v", err)
	}
}

// TestMigrateV0PartialBuildStillStarts：v0 的「半成品库」——表已建（或建了一半）而
// `user_version` 仍为 0（这正是「建表与版本推进分两个事务」能留下的状态）——必须能启动并
// 收敛到 CurrentSchemaVersion，而不是在建表/加列上撞重复错误。
func TestMigrateV0PartialBuildStillStarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v0half.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := raw.Exec(baseSchema); err != nil {
		t.Fatalf("baseSchema: %v", err)
	}
	// 半成品：版本停在 0，而 v1→v2/v2→v3 的列**已经**加上（旧实现「DDL 提交了、版本没提交」）。
	if _, err := raw.Exec(`ALTER TABLE audit_log ADD COLUMN time_synced INTEGER`); err != nil {
		t.Fatalf("预加 time_synced: %v", err)
	}
	if _, err := raw.Exec(`ALTER TABLE config_revisions ADD COLUMN user TEXT`); err != nil {
		t.Fatalf("预加 user: %v", err)
	}
	if err := setVersion(raw, 0); err != nil {
		t.Fatalf("setVersion(0): %v", err)
	}
	_ = raw.Close()

	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("v0 半成品库应能启动: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if v, err := userVersion(s.db); err != nil || v != CurrentSchemaVersion {
		t.Fatalf("版本应推进到 %d，实得 %d err=%v", CurrentSchemaVersion, v, err)
	}
	// 迁移完成后新列真的可用（不是「版本推进了、列没有」的反向半成品）
	if _, err := s.AppendRevision([]byte(`{}`), time.Now(), "after-v0-half", "admin"); err != nil {
		t.Fatalf("迁移后追加修订: %v", err)
	}
	if err := s.AppendAudit(AuditEntry{Time: time.Now(), User: "admin", Action: "act", Detail: "d", Result: "success"}); err != nil {
		t.Fatalf("迁移后写审计（time_synced 列）: %v", err)
	}
	revs, err := s.ListRevisions(0)
	if err != nil || len(revs) != 1 || revs[0].User != "admin" {
		t.Fatalf("config_revisions.user 列应可用: %+v err=%v", revs, err)
	}
}
