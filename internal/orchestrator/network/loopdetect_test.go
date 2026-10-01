package network

// 决策 #337：采样式 L2 环路检测单测（纯检测器 + L2Network.CheckLoop 接线）。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/state"
)

func loopSnap(port string) map[string]string { return map[string]string{"00:11:22:33:44:55": port} }

// 抖动判据：首轮建立、单向一次不判、A→B→A 回跳判（连续 ≥2 轮不同口 + 回跳）。
func TestLoopDetectorJitterRequiresBounceBack(t *testing.T) {
	d := newLoopDetector()
	if v := d.observe("vs", loopSnap("ens192"), 1, 0, nil); v.suspected {
		t.Fatalf("首轮不判，实际: %s", v.message)
	}
	if v := d.observe("vs", loopSnap("ens224"), 1, 0, nil); v.suspected {
		t.Fatal("单向一次移动不判（避免把正常 MAC 迁移当环路）")
	}
	v := d.observe("vs", loopSnap("ens192"), 1, 0, nil)
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
	d.observe("vs", loopSnap("ens192"), 1, 0, nil)
	d.observe("vs", loopSnap("ens224"), 1, 0, nil)
	d.observe("vs", loopSnap("ens192"), 1, 0, nil) // 判抖动
	if v := d.observe("vs", loopSnap("ens192"), 1, 0, nil); v.calmReached {
		t.Fatal("仅 1 轮平静不应达消解阈值")
	}
	if v := d.observe("vs", loopSnap("ens192"), 1, 0, nil); !v.calmReached {
		t.Fatal("连续 2 轮平静应达消解阈值")
	}
}

// 学习表逼近上限：仅配了 learn-limit 才判，阈值 ≥90%。
func TestLoopDetectorLearnLimitSaturation(t *testing.T) {
	if v := newLoopDetector().observe("vs", nil, 9, 10, nil); !v.suspected {
		t.Fatal("9/10=90% 应判逼近上限")
	}
	if v := newLoopDetector().observe("vs", nil, 8, 10, nil); v.suspected {
		t.Fatal("8/10 未达 90% 不应判")
	}
	if v := newLoopDetector().observe("vs", nil, 100, 0, nil); v.suspected {
		t.Fatal("未配 learn-limit 时不得判逼近上限")
	}
	// 证据要能读出条目数与上限
	v := newLoopDetector().observe("vs", nil, 900, 1000, nil)
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

// ---------- 判据③（决策 #337 修订）：成员口同时被广播风暴打满 ----------

// stormSamples 构造一个成员口采样集合（at 为采样时刻，rx 为累计 rx 包数）。
func stormSamples(at time.Time, rx map[string]uint64) []loopPortSample {
	out := make([]loopPortSample, 0, len(rx))
	for p, v := range rx {
		out = append(out, loopPortSample{port: p, rx: v, at: at})
	}
	return out
}

// 全成员同时高 pps 连续两轮 → Raise（首轮仅建基线，故需 3 次采样）。
func TestLoopDetectorStormAllMembersTwoRounds(t *testing.T) {
	d := newLoopDetector()
	t0 := time.Unix(0, 0)
	if v := d.observe("vs", nil, 0, 0, stormSamples(t0, map[string]uint64{"ens192": 1000, "ens224": 1000})); v.suspected {
		t.Fatalf("首轮建基线不应判: %s", v.message)
	}
	if v := d.observe("vs", nil, 0, 0, stormSamples(t0.Add(time.Second), map[string]uint64{"ens192": 101000, "ens224": 101000})); v.suspected {
		t.Fatal("仅 1 轮全员高负载不应判（需连续 2 轮）")
	}
	v := d.observe("vs", nil, 0, 0, stormSamples(t0.Add(2*time.Second), map[string]uint64{"ens192": 201000, "ens224": 201000}))
	if !v.suspected {
		t.Fatal("全员同时高 pps 连续 2 轮应判环路嫌疑")
	}
	if !strings.Contains(v.message, "ens192") || !strings.Contains(v.message, "ens224") || !strings.Contains(v.message, "pps") {
		t.Fatalf("风暴告警消息应含各成员口 pps，实际: %s", v.message)
	}
	if !strings.Contains(v.message, "风暴") || !strings.Contains(v.message, "环路") {
		t.Fatalf("消息应写清疑似广播风暴/环路（成员口同时高负载），实际: %s", v.message)
	}
}

// 单个成员口高 pps → 不报（「同时高」才成立；单台忙 VM 只打满自己的口）。
func TestLoopDetectorStormSingleMemberHighNoAlarm(t *testing.T) {
	d := newLoopDetector()
	t0 := time.Unix(0, 0)
	d.observe("vs", nil, 0, 0, stormSamples(t0, map[string]uint64{"ens192": 0, "ens224": 0}))
	d.observe("vs", nil, 0, 0, stormSamples(t0.Add(time.Second), map[string]uint64{"ens192": 100000, "ens224": 0}))
	v := d.observe("vs", nil, 0, 0, stormSamples(t0.Add(2*time.Second), map[string]uint64{"ens192": 200000, "ens224": 0}))
	if v.suspected {
		t.Fatalf("仅单口高负载不应判风暴: %s", v.message)
	}
}

// 两轮中一轮回落 → 不报（连续 2 轮被打断，连胜清零）。
func TestLoopDetectorStormDipResetsStreak(t *testing.T) {
	d := newLoopDetector()
	t0 := time.Unix(0, 0)
	d.observe("vs", nil, 0, 0, stormSamples(t0, map[string]uint64{"ens192": 0, "ens224": 0}))
	d.observe("vs", nil, 0, 0, stormSamples(t0.Add(time.Second), map[string]uint64{"ens192": 100000, "ens224": 100000}))   // streak=1
	d.observe("vs", nil, 0, 0, stormSamples(t0.Add(2*time.Second), map[string]uint64{"ens192": 100000, "ens224": 100000})) // 回落 → streak=0
	v := d.observe("vs", nil, 0, 0, stormSamples(t0.Add(3*time.Second), map[string]uint64{"ens192": 200000, "ens224": 200000}))
	if v.suspected {
		t.Fatalf("中间一轮回落应打断连续 2 轮，不应判: %s", v.message)
	}
}

// 风暴后平静两轮 → Resolve（并入同一平静轮次机制）。
func TestLoopDetectorStormCalmResolves(t *testing.T) {
	d := newLoopDetector()
	t0 := time.Unix(0, 0)
	d.observe("vs", nil, 0, 0, stormSamples(t0, map[string]uint64{"ens192": 0, "ens224": 0}))
	d.observe("vs", nil, 0, 0, stormSamples(t0.Add(time.Second), map[string]uint64{"ens192": 100000, "ens224": 100000}))
	if v := d.observe("vs", nil, 0, 0, stormSamples(t0.Add(2*time.Second), map[string]uint64{"ens192": 200000, "ens224": 200000})); !v.suspected {
		t.Fatal("前置：应先判风暴")
	}
	// 连续两轮 rx 不再增长（pps=0）→ 平静
	if v := d.observe("vs", nil, 0, 0, stormSamples(t0.Add(3*time.Second), map[string]uint64{"ens192": 200000, "ens224": 200000})); v.calmReached {
		t.Fatal("仅 1 轮平静不应达消解阈值")
	}
	if v := d.observe("vs", nil, 0, 0, stormSamples(t0.Add(4*time.Second), map[string]uint64{"ens192": 200000, "ens224": 200000})); !v.calmReached {
		t.Fatal("连续 2 轮平静应达消解阈值")
	}
}

// fakeCounters 假的成员口 rx 计数读物（判据③接线单测）。
type fakeCounters struct {
	rx map[string]uint64
	ok bool
}

func (f *fakeCounters) InterfaceCounters(_ context.Context, ifname string) (state.InterfaceCounters, bool) {
	if !f.ok {
		return state.InterfaceCounters{}, false
	}
	rx, ok := f.rx[ifname]
	if !ok {
		return state.InterfaceCounters{}, false
	}
	return state.InterfaceCounters{RxPackets: rx}, true
}

// 判据③ 接线：成员口清单取 #326 读视图（DerivedSwitchPorts）、计数取 InterfaceCounterReader；
// 全成员高 pps 连续两轮 Raise LOOP_SUSPECTED（source=交换机名），平静两轮后 Resolve。
func TestCheckLoopStormWiring(t *testing.T) {
	f := newFakeL2()
	net := NewL2Network(nil, NewL2Provider(f))
	alarms := NewAlarmStore()
	net.SetAlarms(alarms)
	cnt := &fakeCounters{ok: true, rx: map[string]uint64{"ens192": 0, "ens224": 0}}
	net.SetCounters(cnt)
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{{Name: "vs-a", Type: "l2",
		Ports: []model.VSwitchPort{{Seq: 0, Interface: "ens192"}, {Seq: 1, Interface: "ens224"}}}}}
	ctx := context.Background()

	net.CheckLoop(ctx, cfg) // 基线
	time.Sleep(2 * time.Millisecond)
	cnt.rx["ens192"], cnt.rx["ens224"] = 1<<30, 1<<30
	net.CheckLoop(ctx, cfg) // streak=1
	time.Sleep(2 * time.Millisecond)
	cnt.rx["ens192"], cnt.rx["ens224"] = 2<<30, 2<<30
	if errs := net.CheckLoop(ctx, cfg); errs != nil {
		t.Fatalf("CheckLoop 不应报错: %v", errs)
	}
	active := alarms.List(AlarmActive)
	if len(active) != 1 || active[0].Code != AlarmLoopSuspected || active[0].Source != "vs-a" {
		t.Fatalf("全员高 pps 连续 2 轮应 Raise LOOP_SUSPECTED（source=vs-a），实际 %+v", active)
	}
	// 平静两轮（计数不再增长，pps=0）→ 消警
	net.CheckLoop(ctx, cfg)
	net.CheckLoop(ctx, cfg)
	if got := alarms.List(AlarmActive); len(got) != 0 {
		t.Fatalf("连续 2 轮平静应消警，实际 %+v", got)
	}
}

// 判据③：成员口计数读失败 → 如实回错误且不误报（不把「读不到」当成高负载）。
func TestCheckLoopStormCounterReadFailure(t *testing.T) {
	f := newFakeL2()
	net := NewL2Network(nil, NewL2Provider(f))
	alarms := NewAlarmStore()
	net.SetAlarms(alarms)
	net.SetCounters(&fakeCounters{ok: false})
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{{Name: "vs-a", Type: "l2",
		Ports: []model.VSwitchPort{{Seq: 0, Interface: "ens192"}, {Seq: 1, Interface: "ens224"}}}}}
	errs := net.CheckLoop(context.Background(), cfg)
	if len(errs) == 0 {
		t.Fatal("成员口计数读失败应如实回错误")
	}
	if got := alarms.List(AlarmActive); len(got) != 0 {
		t.Fatalf("读数失败不得误报环路，实际 %+v", got)
	}
}
