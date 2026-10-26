package config

// round49（主会话接线配套）：审计写入失败要有出口（C 组已做），**恢复**同样要有出口。
//
// 装配方据 `OnAuditError` 落一条 `AUDIT_WRITE_FAILED` 告警；若没有恢复信号，这条告警会
// 永不消解（而审计通道恢复是常态）——所以引擎在「曾失败且已上窗、如今写成功」时上报一次
// `EventAuditWriteRecovered`，装配方据此 Resolve。本文件钉住这个契约：失败上窗一次、
// 恢复上报一次、之后再失败再上窗（结果变化即重新可见）。

import "testing"

func TestAuditWriteRecoveryEmitsRecoveredEvent(t *testing.T) {
	store := openTestStore(t)
	clock := newFakeClock()
	timers := newTimerSink()
	var events []Event
	hookCalls := 0
	e, err := NewEngine(store, &mockApplier{}, Options{
		Now:          clock.Now,
		AfterFunc:    timers.after,
		OnEvent:      func(ev Event) { events = append(events, ev) },
		OnAuditError: func(AuditEntry, error) { hookCalls++ },
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer e.Close()

	injectAuditInsertFailure(t, store)
	e.Audit("admin", "round49.test", "detail", "success")
	if hookCalls != 1 {
		t.Fatalf("审计写失败应上窗一次钩子，实际 %d 次", hookCalls)
	}
	for _, ev := range events {
		if ev.Type == EventAuditWriteRecovered {
			t.Fatalf("尚未恢复不得上报 recovered: %+v", ev)
		}
	}

	if _, err := store.db.Exec(`DROP TRIGGER r49_noaudit`); err != nil {
		t.Fatalf("拆除审计失败触发器: %v", err)
	}
	e.Audit("admin", "round49.test", "detail", "success")
	if hookCalls != 1 {
		t.Fatalf("恢复后不应再报失败，实际钩子 %d 次", hookCalls)
	}
	recovered := 0
	for _, ev := range events {
		if ev.Type == EventAuditWriteRecovered {
			recovered++
		}
	}
	if recovered != 1 {
		t.Fatalf("恢复应恰好上报一次 recovered 事件，实际 %d 次（events=%+v）", recovered, events)
	}

	// 恢复后再次坏掉：应重新上窗（去重键已复位），并再次在恢复时上报
	injectAuditInsertFailure(t, store)
	e.Audit("admin", "round49.test", "detail", "success")
	if hookCalls != 2 {
		t.Fatalf("再次失败应重新上窗（钩子 2 次），实际 %d 次", hookCalls)
	}
	if _, err := store.db.Exec(`DROP TRIGGER r49_noaudit`); err != nil {
		t.Fatalf("再次拆除触发器: %v", err)
	}
	e.Audit("admin", "round49.test", "detail", "success")
	recovered = 0
	for _, ev := range events {
		if ev.Type == EventAuditWriteRecovered {
			recovered++
		}
	}
	if recovered != 2 {
		t.Fatalf("第二轮恢复应再上报一次（共 2 次），实际 %d 次（events=%+v）", recovered, events)
	}
}
