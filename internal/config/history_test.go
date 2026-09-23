package config

// 配置提交历史（决策 #142）：`Store.ListRevisions` 的元数据与顺序、`Engine.History`
// 的 current 标记与**空历史回空切片**（JSON 里是 `[]` 而不是 `null`），
// 以及存储 v2→v3 迁移对**老快照**的处理（迁移前没有提交者列 → 空串，不谎称已知）。

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestListRevisionsMetadataAndOrder(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	for _, r := range []struct{ msg, user string }{
		{"第一版", "alice"},
		{"第二版", "bob"},
		{"", "carol"}, // 未填提交说明
	} {
		if _, err := s.AppendRevision([]byte(`{"system":{"hostname":"h"}}`), now, r.msg, r.user); err != nil {
			t.Fatalf("AppendRevision: %v", err)
		}
	}

	got, err := s.ListRevisions(0)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应有 3 条修订，实得 %d", len(got))
	}
	if got[0].Rev != 3 || got[2].Rev != 1 {
		t.Fatalf("应按 rev 降序（最新在前），实得 %d…%d", got[0].Rev, got[2].Rev)
	}
	if got[0].User != "carol" || got[1].User != "bob" || got[2].User != "alice" {
		t.Fatalf("提交者未如实回填: %q %q %q", got[0].User, got[1].User, got[2].User)
	}
	if got[0].Message != "" || got[2].Message != "第一版" {
		t.Fatalf("提交说明未如实回填: %q %q", got[0].Message, got[2].Message)
	}
	if got[0].CommittedAt.IsZero() {
		t.Fatalf("提交时间未回填: %+v", got[0])
	}
	// limit 生效：只取最近 N 份
	top, err := s.ListRevisions(1)
	if err != nil || len(top) != 1 || top[0].Rev != 3 {
		t.Fatalf("ListRevisions(1) 应只回最新一份，实得 %+v err=%v", top, err)
	}
}

// TestMigrationV2ToV3KeepsOldRevisions 老库（v2，修订表无 user 列）升级后：
// 既有快照**保留**、提交者标记为「未记录」（空串），新写入的快照带提交者。
// 迁移必须纯增量（FR-OPS-003：升级不触碰配置数据）。
func TestMigrationV2ToV3KeepsOldRevisions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")

	// 造一个 v2 库：初始表 + v1→v2 迁移（审计表加 time_synced），user_version=2。
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := raw.Exec(baseSchema); err != nil {
		t.Fatalf("baseSchema: %v", err)
	}
	if err := schemaMigrations[1](raw); err != nil {
		t.Fatalf("迁移 v1→v2: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO config_revisions (committed_at, message, config_json) VALUES (?,?,?)`,
		"2026-09-01T00:00:00Z", "迁移前的快照", `{"system":{"hostname":"old"}}`); err != nil {
		t.Fatalf("插入老快照: %v", err)
	}
	if err := setVersion(raw, 2); err != nil {
		t.Fatalf("setVersion: %v", err)
	}
	_ = raw.Close()

	s, err := OpenStore(path) // 触发 v2→v3 迁移
	if err != nil {
		t.Fatalf("OpenStore（应完成 v2→v3 迁移）: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if v, err := userVersion(s.db); err != nil || v != CurrentSchemaVersion {
		t.Fatalf("迁移后版本应为 %d，实得 %d err=%v", CurrentSchemaVersion, v, err)
	}

	got, err := s.ListRevisions(0)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(got) != 1 || got[0].Message != "迁移前的快照" {
		t.Fatalf("老快照必须保留原样，实得 %+v", got)
	}
	if got[0].User != "" {
		t.Fatalf("迁移前的老快照没有提交者信息，应为空串（不谎称已知），实得 %q", got[0].User)
	}
	// 配置正文未受影响（升级不触碰配置数据）
	data, err := s.LoadRevision(got[0].Rev)
	if err != nil || string(data) != `{"system":{"hostname":"old"}}` {
		t.Fatalf("老快照正文被改动: %s err=%v", data, err)
	}

	// 迁移后可正常写入带提交者的新快照
	if _, err := s.AppendRevision([]byte(`{}`), time.Now(), "迁移后", "admin"); err != nil {
		t.Fatalf("AppendRevision: %v", err)
	}
	got, _ = s.ListRevisions(0)
	if got[0].User != "admin" {
		t.Fatalf("迁移后的新快照应记录提交者，实得 %q", got[0].User)
	}
}

// TestEngineHistoryCurrentAndEmptyArray Engine.History 的 current 标记与空态形状。
func TestEngineHistoryCurrentAndEmptyArray(t *testing.T) {
	k := newEngineKit(t) // 预置 rev1 基线
	for _, r := range []struct{ msg, user string }{{"第二版", "bob"}, {"第三版", "carol"}} {
		if _, err := k.store.AppendRevision([]byte(`{}`), k.clock.Now(), r.msg, r.user); err != nil {
			t.Fatalf("AppendRevision: %v", err)
		}
	}

	got, err := k.engine.History(0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应有 3 条历史，实得 %d", len(got))
	}
	if got[0].Rev != 3 || !got[0].Current || got[0].User != "carol" || got[0].Comment != "第三版" {
		t.Fatalf("最新一份的标记/提交者/说明不符: %+v", got[0])
	}
	for _, r := range got[1:] {
		if r.Current {
			t.Fatalf("只有最新一份该标 current，rev %d 也被标了", r.Rev)
		}
	}

	// 清空修订表（模拟「无历史」）：必须回**空切片**而不是 nil——
	// 契约声明是数组，发 null 会让按契约写的客户端踩空。
	if err := k.store.PruneRevisions(0); err != nil {
		t.Fatalf("PruneRevisions: %v", err)
	}
	empty, err := k.engine.History(0)
	if err != nil {
		t.Fatalf("History（空库）: %v", err)
	}
	if empty == nil {
		t.Fatal("无历史时应回空切片而不是 nil（JSON 会变成 null）")
	}
	b, err := json.Marshal(empty)
	if err != nil || string(b) != "[]" {
		t.Fatalf("无历史应序列化为 []，实得 %s err=%v", b, err)
	}
}
