package main

// 决策 #394④：15s 巡检的**快照新鲜**（每个复核步骤用当次 committed 快照）。
//
// 旧行为：一次 engine.Committed() 快照喂给全部步骤——快照读完后对象被提交删除，巡检仍按旧快照
// 把它「复活」（孤儿 DHCP tap），或按旧声明误删刚提交的 relay proxy（round171 C2-F4）。

import (
	"context"
	"errors"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// TestRunReconcileCycleUsesFreshSnapshotPerStep：前一步骤执行后对象被删除（下一次 Committed 已不含
// 它），后一步骤必须看到删除后的配置——不得沿用旧快照。
func TestRunReconcileCycleUsesFreshSnapshotPerStep(t *testing.T) {
	snaps := []model.Config{
		{VirtualSwitches: []model.VirtualSwitch{{Name: "vs-a", DhcpServerPoolStart: "192.168.1.10"}}},
		{}, // 提交删除后
	}
	i := 0
	snap := func() (model.Config, error) {
		cfg := snaps[i]
		if i < len(snaps)-1 {
			i++
		}
		return cfg, nil
	}
	var seen []int
	steps := []reconcileStep{
		{"first", func(_ context.Context, cfg model.Config) { seen = append(seen, len(cfg.VirtualSwitches)) }},
		{"second", func(_ context.Context, cfg model.Config) { seen = append(seen, len(cfg.VirtualSwitches)) }},
	}
	runReconcileCycle(context.Background(), snap, steps, discardLogger())
	if len(seen) != 2 || seen[0] != 1 || seen[1] != 0 {
		t.Fatalf("每步应读当次快照（第 2 步应看到已删）: %v", seen)
	}
}

// TestRunReconcileCycleSkipsOnSnapshotError：取快照失败即跳过该步（不沿用上一次快照、不猜）。
func TestRunReconcileCycleSkipsOnSnapshotError(t *testing.T) {
	calls := 0
	i := 0
	snap := func() (model.Config, error) {
		i++
		if i == 1 {
			return model.Config{VirtualSwitches: []model.VirtualSwitch{{Name: "vs-a"}}}, nil
		}
		return model.Config{}, errors.New("db down")
	}
	steps := []reconcileStep{
		{"ok", func(context.Context, model.Config) { calls++ }},
		{"fail", func(context.Context, model.Config) { calls++ }},
	}
	runReconcileCycle(context.Background(), snap, steps, discardLogger())
	if calls != 1 {
		t.Fatalf("取快照失败应跳过该步: calls=%d", calls)
	}
}
