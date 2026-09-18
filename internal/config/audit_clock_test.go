package config

// NFR-006：审计/告警时间戳依赖 NTP，**未同步时事件带未同步标记**。
//
// 落点与取舍：
//   - 标记**随事件一起落库**（audit_log.time_synced，v1→v2 迁移新增可空列）——按「写入那一刻」
//     的事实记录，而不是查询时补算（否则「同步时写、不同步时读」会被错标）。
//   - 可空：迁移前的老记录为 NULL（= 未知），**不谎称已知**。
//   - 探针由调用方注入（Options.TimeSynced），引擎不反向依赖宿主交互包（骨架 §3.5）。

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestAuditTimeSyncedRoundTrip 三种状态都能原样存取：未知(nil) / 已同步(true) / 未同步(false)。
func TestAuditTimeSyncedRoundTrip(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	yes, no := true, false
	for i, tc := range []struct {
		mark string
		want *bool
	}{
		{"未知", nil}, {"已同步", &yes}, {"未同步", &no},
	} {
		if err := s.AppendAudit(AuditEntry{
			Time: base.Add(time.Duration(i) * time.Minute), User: "u", Action: "act",
			Detail: tc.mark, Result: "success", TimeSynced: tc.want,
		}); err != nil {
			t.Fatalf("AppendAudit(%s): %v", tc.mark, err)
		}
	}
	got, err := s.ListAudit(10, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应有 3 条，实得 %d", len(got))
	}
	// 倒序：最新在前 → [未同步(false), 已同步(true), 未知(nil)]
	want := []struct {
		mark string
		val  *bool
	}{{"未同步", &no}, {"已同步", &yes}, {"未知", nil}}
	for i, w := range want {
		if got[i].Detail != w.mark {
			t.Fatalf("第 %d 条应是 %s，实得 %s", i, w.mark, got[i].Detail)
		}
		switch {
		case w.val == nil && got[i].TimeSynced != nil:
			t.Errorf("%s 应存为未知(nil)，实得 %v", w.mark, *got[i].TimeSynced)
		case w.val != nil && got[i].TimeSynced == nil:
			t.Errorf("%s 应存为 %v，实得未知(nil)", w.mark, *w.val)
		case w.val != nil && *got[i].TimeSynced != *w.val:
			t.Errorf("%s 应存为 %v，实得 %v", w.mark, *w.val, *got[i].TimeSynced)
		}
	}
}

// TestMigrationV1ToV2KeepsOldRows 老库（v1，无 time_synced 列）升级后：
// 既有审计行**保留**且标记为「未知」，新写入的行带标记。迁移必须纯增量（FR-OPS-003）。
func TestMigrationV1ToV2KeepsOldRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")

	// 造一个 v1 库：只建初始表、user_version=1，并写入一条「老」审计。
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := raw.Exec(baseSchema); err != nil {
		t.Fatalf("baseSchema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO audit_log (ts, user, action, detail, result) VALUES (?,?,?,?,?)`,
		"2026-09-01T00:00:00Z", "old", "config.commit", "迁移前的记录", "success"); err != nil {
		t.Fatalf("插入老记录: %v", err)
	}
	if err := setVersion(raw, 1); err != nil {
		t.Fatalf("setVersion: %v", err)
	}
	_ = raw.Close()

	// 正常打开 → 触发迁移
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore（应完成 v1→v2 迁移）: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if v, err := userVersion(s.db); err != nil || v != CurrentSchemaVersion {
		t.Fatalf("迁移后版本应为 %d，实得 %d err=%v", CurrentSchemaVersion, v, err)
	}
	got, err := s.ListAudit(10, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("老记录必须保留，实得 %d 条", len(got))
	}
	if got[0].Detail != "迁移前的记录" {
		t.Fatalf("老记录内容被改动: %+v", got[0])
	}
	if got[0].TimeSynced != nil {
		t.Fatalf("老记录应标记为未知(nil)，实得 %v", *got[0].TimeSynced)
	}

	// 迁移后可正常写入带标记的新记录
	no := false
	if err := s.AppendAudit(AuditEntry{
		Time: time.Now(), User: "new", Action: "config.commit",
		Detail: "迁移后的记录", Result: "success", TimeSynced: &no,
	}); err != nil {
		t.Fatalf("迁移后写入: %v", err)
	}
	got, _ = s.ListAudit(10, 0)
	if len(got) != 2 || got[0].TimeSynced == nil || *got[0].TimeSynced {
		t.Fatalf("迁移后新记录应带「未同步」标记: %+v", got[0])
	}
}

// TestEngineStampsTimeSynced 引擎在写审计时统一打标：探针为 false → 记录 false；
// 探针为 nil（未注入）→ 未知（不谎称已同步）。所有写入路径都必须经过打标。
func TestEngineStampsTimeSynced(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "nfvis.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var synced bool
	eng, err := NewEngine(store, nil, Options{Now: time.Now, TimeSynced: func() bool { return synced }})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(eng.Close)

	eng.Audit("admin", "vmf.start", "启动 VM cli-vm", "success") // 未同步
	synced = true
	eng.Audit("admin", "vmf.stop", "停止 VM cli-vm", "success") // 已同步

	got, err := store.ListAudit(10, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应有 2 条，实得 %d", len(got))
	}
	if got[1].TimeSynced == nil || *got[1].TimeSynced {
		t.Errorf("第一条应标记未同步: %+v", got[1])
	}
	if got[0].TimeSynced == nil || !*got[0].TimeSynced {
		t.Errorf("第二条应标记已同步: %+v", got[0])
	}
}

// TestEngineTimeSyncedProbeAbsentLeavesUnknown 未注入探针时不写标记（保持未知）。
func TestEngineTimeSyncedProbeAbsentLeavesUnknown(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "nfvis.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	eng, err := NewEngine(store, nil, Options{Now: time.Now})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(eng.Close)
	eng.Audit("admin", "act", "detail", "success")
	got, _ := store.ListAudit(10, 0)
	if len(got) != 1 || got[0].TimeSynced != nil {
		t.Fatalf("未注入探针应保持未知(nil): %+v", got)
	}
}
