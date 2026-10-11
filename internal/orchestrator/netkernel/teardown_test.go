package netkernel

// 数据面整体 teardown（R12-7）与代际重建门控（R12-8）的内核侧用例：
//   - 带对象 teardown 逐个删除内核对象 + 整体回收四张 nfvis-* nft 表；
//   - teardown 失败进告警（独立 scope）且如实返回错误（调用方据此告警、启动继续）；
//   - 一次真正的全量重放清除「强制重建」待办（门控）。

import (
	"context"
	"errors"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// 带对象 teardown：bridge/VRF/bond/vxlan 逐个删、四张 nft 表整体回收。
func TestTeardownDataplaneDeletesDeclaredObjectsAndNftTables(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{{Name: "vs-lan", Type: "l2"}},
		Vrfs:            []model.Vrf{{Name: "vs-l3"}},
		Bonds:           []model.Bond{{Name: "bond0"}},
		VxlanTunnels:    []model.VxlanTunnel{{Name: "vx1"}},
	}
	if err := p.TeardownDataplane(context.Background(), cfg); err != nil {
		t.Fatalf("teardown 应成功（对象都已不存在/删除幂等）: %v", err)
	}
	for _, want := range []string{
		"ip link del vs-lan",    // L2 交换机 bridge
		"ip link del vr-vs-lan", // 它自己创建的网关 VRF
		"ip link del vs-l3",     // L3 交换机 VRF
		"ip link del bond0",     // bond
		"ip link del vx1",       // vxlan
		"nft delete table netdev nfvis-acl",
		"nft delete table netdev nfvis-portsec",
		"nft delete table inet nfvis-nat",
		"nft delete table inet nfvis-forward",
	} {
		if !f.has(want) {
			t.Fatalf("teardown 应执行 %q；实际：\n%s", want, f.joined())
		}
	}
	// 负面断言（round23 真机教训）：不得把**族名**当表名（曾生成 `nft delete table inet inet`，
	// 必报语法错 ⇒ 切换误判未清理 + 假告警）。正向 `has()` 会被步骤 4 的正确删除喂绿，抓不到它。
	if f.has("nft delete table inet inet") {
		t.Fatalf("teardown 不得生成 `nft delete table inet inet`（族名被当表名）：\n%s", f.joined())
	}
}

// teardown 幂等：空配置也要回收四张 nft 表（残留对象随表一并清），且不报错。
func TestTeardownDataplaneIdempotentOnEmptyConfig(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	if err := p.TeardownDataplane(context.Background(), model.Config{}); err != nil {
		t.Fatalf("空配置 teardown 应成功: %v", err)
	}
	for _, want := range []string{
		"nft delete table netdev nfvis-acl",
		"nft delete table netdev nfvis-portsec",
		"nft delete table inet nfvis-nat",
		"nft delete table inet nfvis-forward",
	} {
		if !f.has(want) {
			t.Fatalf("空配置也应回收 %q；实际：\n%s", want, f.joined())
		}
	}
}

// teardown 失败：如实返回错误 + 独立 scope 的告警落表（启动继续由调用方保证）。
func TestTeardownDataplaneFailureRaisesAlarm(t *testing.T) {
	f := &fakeRunner{failAll: errors.New("注入失败：底座不可用")}
	p := New(f)
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{{Name: "vs-lan", Type: "l2"}}}

	err := p.TeardownDataplane(context.Background(), cfg)
	if err == nil {
		t.Fatal("有对象未清理干净应如实返回错误")
	}
	if !teardownAlarmActive(store) {
		t.Fatalf("teardown 失败应落告警（独立 scope）；活动告警：%+v", store.List("active"))
	}

	// 复原：一次成功的 teardown 自动消解该告警（幂等、可重试）。
	f2 := &fakeRunner{}
	p2 := New(f2)
	p2.SetAlarms(store)
	if err := p2.TeardownDataplane(context.Background(), model.Config{}); err != nil {
		t.Fatalf("复原 teardown 应成功: %v", err)
	}
	if teardownAlarmActive(store) {
		t.Fatal("成功 teardown 后告警应消解")
	}
}

// 一次真正的全量重放清除「强制重建」待办（门控）——这是 15s 巡检「未重放」告警消解的判据。
func TestEnsureConsistentSatisfiesReplayGate(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	gate := network.NewReplayGate()
	p.SetReplayGate(gate)
	gate.Require("数据面实现发生变化（测试）")

	_ = p.EnsureConsistent(context.Background(), model.Config{})

	if pending, reason := gate.Pending(); pending {
		t.Fatalf("全量重放后待办应清除，仍为：%s", reason)
	}
}

func teardownAlarmActive(store *network.AlarmStore) bool {
	for _, a := range store.List("active") {
		if a.Code == AlarmTeardownFailed {
			return true
		}
	}
	return false
}
