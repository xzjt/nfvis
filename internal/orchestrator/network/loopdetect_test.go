package network

// 决策 #337：采样式 L2 环路检测单测（纯检测器 + L2Network.CheckLoop 接线）。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

func loopSnap(port string) map[string]string { return map[string]string{"00:11:22:33:44:55": port} }

// 抖动判据：首轮建立、单向一次不判、A→B→A 回跳判（连续 ≥2 轮不同口 + 回跳）。
func TestLoopDetectorJitterRequiresBounceBack(t *testing.T) {
	d := newLoopDetector()
	if v := d.observe("vs", loopSnap("ens192"), 1, 0); v.suspected {
		t.Fatalf("首轮不判，实际: %s", v.message)
	}
	if v := d.observe("vs", loopSnap("ens224"), 1, 0); v.suspected {
		t.Fatal("单向一次移动不判（避免把正常 MAC 迁移当环路）")
	}
	v := d.observe("vs", loopSnap("ens192"), 1, 0)
	if !v.suspected {
		t.Fatal("A→B→A 回跳应判环路")
	}
	if !strings.Contains(v.message, "00:11:22:33:44:55") || !strings.Contains(v.message, "ens192") || !strings.Contains(v.message, "ens224") {
		t.Fatalf("告警消息应含 MAC 与两个成员口，实际: %s", v.message)
	}
}

// 消解判据：连续 2 轮平静才 calmReached。
func TestLoopDetectorCalmRounds(t *testing.T) {
	d := newLoopDetector()
	d.observe("vs", loopSnap("ens192"), 1, 0)
	d.observe("vs", loopSnap("ens224"), 1, 0)
	d.observe("vs", loopSnap("ens192"), 1, 0) // 判抖动
	if v := d.observe("vs", loopSnap("ens192"), 1, 0); v.calmReached {
		t.Fatal("仅 1 轮平静不应达消解阈值")
	}
	if v := d.observe("vs", loopSnap("ens192"), 1, 0); !v.calmReached {
		t.Fatal("连续 2 轮平静应达消解阈值")
	}
}

// 学习表逼近上限：仅配了 learn-limit 才判，阈值 ≥90%。
func TestLoopDetectorLearnLimitSaturation(t *testing.T) {
	if v := newLoopDetector().observe("vs", nil, 9, 10); !v.suspected {
		t.Fatal("9/10=90% 应判逼近上限")
	}
	if v := newLoopDetector().observe("vs", nil, 8, 10); v.suspected {
		t.Fatal("8/10 未达 90% 不应判")
	}
	if v := newLoopDetector().observe("vs", nil, 100, 0); v.suspected {
		t.Fatal("未配 learn-limit 时不得判逼近上限")
	}
	// 证据要能读出条目数与上限
	v := newLoopDetector().observe("vs", nil, 900, 1000)
	if !v.suspected || !strings.Contains(v.message, "900") || !strings.Contains(v.message, "1000") {
		t.Fatalf("逼近上限告警应含条目数/上限，实际: %s", v.message)
	}
}

// CheckLoop 接线：抖动 Raise → 连续平静 Resolve；对象消失清警。
func TestCheckLoopRaisesAndResolves(t *testing.T) {
	f := newFakeL2()
	net := NewL2Network(nil, NewL2Provider(f))
	alarms := NewAlarmStore()
	net.SetAlarms(alarms)
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{{Name: "vs-a", Type: "l2"}}}
	bd := BDID("vs-a")
	ctx := context.Background()

	// 1→2→1：同一 MAC 在两个成员口之间回跳
	f.macs[bd] = []MACEntry{{MAC: "00:11:22:33:44:55", SwIfIndex: 1}}
	net.CheckLoop(ctx, cfg)
	f.macs[bd] = []MACEntry{{MAC: "00:11:22:33:44:55", SwIfIndex: 2}}
	net.CheckLoop(ctx, cfg)
	f.macs[bd] = []MACEntry{{MAC: "00:11:22:33:44:55", SwIfIndex: 1}}
	if errs := net.CheckLoop(ctx, cfg); errs != nil {
		t.Fatalf("CheckLoop 不应报错: %v", errs)
	}
	active := alarms.List(AlarmActive)
	if len(active) != 1 || active[0].Code != AlarmLoopSuspected || active[0].Source != "vs-a" {
		t.Fatalf("抖动应 Raise LOOP_SUSPECTED（source=vs-a），实际 %+v", active)
	}

	// 连续 2 轮同口（平静）→ 消警
	net.CheckLoop(ctx, cfg)
	net.CheckLoop(ctx, cfg)
	if got := alarms.List(AlarmActive); len(got) != 0 {
		t.Fatalf("连续 2 轮平静应消警，实际 %+v", got)
	}

	// 再抖一次 Raise（2→1 两轮即成回跳），然后把交换机从配置移除 → 对象消失清警
	f.macs[bd] = []MACEntry{{MAC: "00:11:22:33:44:55", SwIfIndex: 2}}
	net.CheckLoop(ctx, cfg)
	f.macs[bd] = []MACEntry{{MAC: "00:11:22:33:44:55", SwIfIndex: 1}}
	net.CheckLoop(ctx, cfg)
	if len(alarms.List(AlarmActive)) == 0 {
		t.Fatal("再次抖动应 Raise")
	}
	net.CheckLoop(ctx, model.Config{})
	if got := alarms.List(AlarmActive); len(got) != 0 {
		t.Fatalf("交换机从配置移除后应清警，实际 %+v", got)
	}
}

// 查询失败如实回错误（不谎报「无环路」）；未注入告警表时只检测不落告警。
func TestCheckLoopQueryFailureAndNoAlarms(t *testing.T) {
	f := newFakeL2()
	f.err = errors.New("vpp down")
	net := NewL2Network(nil, NewL2Provider(f))
	net.SetAlarms(NewAlarmStore())
	errs := net.CheckLoop(context.Background(), model.Config{VirtualSwitches: []model.VirtualSwitch{{Name: "vs-a", Type: "l2"}}})
	if len(errs) == 0 {
		t.Fatal("MAC 表查询失败应如实回错误")
	}

	f2 := newFakeL2()
	net2 := NewL2Network(nil, NewL2Provider(f2)) // 未 SetAlarms
	if errs := net2.CheckLoop(context.Background(), model.Config{VirtualSwitches: []model.VirtualSwitch{{Name: "vs-a", Type: "l2"}}}); errs != nil {
		t.Fatalf("未注入告警表时应静默检测: %v", errs)
	}
}
