package netkernel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// fdbJSON 构造 `bridge -j fdb show br <br>` 的输出：含 1 条 bridge 自身条目 + 1 条 self 条目
// （两者都应被 Runtime.MACTable 过滤），外加 learned 条正常学习表项。
func fdbJSON(learned int) string {
	rows := []string{
		`{"mac":"aa:bb:cc:dd:ee:ff","dev":"vs-ll","vlan":0,"master":"vs-ll","flags":[]}`,
		`{"mac":"00:11:22:33:44:55","dev":"vs-ll","vlan":0,"master":"vs-ll","flags":["self"]}`,
	}
	for i := 0; i < learned; i++ {
		rows = append(rows, fmt.Sprintf(
			`{"mac":"02:00:00:00:00:%02x","dev":"ens192","vlan":0,"master":"vs-ll","flags":[]}`, i))
	}
	return "[" + strings.Join(rows, ",") + "]"
}

func learnLimitCfg(limit int) model.Config {
	return model.Config{VirtualSwitches: []model.VirtualSwitch{
		{Name: "vs-ll", Type: "l2", LearnLimit: limit},
	}}
}

// 决策 #435：计数 ≥ 阈值建警、回落到阈值下自动消解；过滤口径复用 Runtime.MACTable
// （bridge 自身条目与 self 条目不计入）。
func TestCheckLearnLimitsRaisesAndResolves(t *testing.T) {
	above := &fakeRunner{replies: []fakeReply{{prefix: "bridge -j fdb show br vs-ll", out: fdbJSON(3)}}}
	p := New(above)
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	if errs := p.CheckLearnLimits(context.Background(), learnLimitCfg(3)); len(errs) != 0 {
		t.Fatalf("计数可读时不应报错：%v", errs)
	}
	active := store.List("active")
	if len(active) != 1 {
		t.Fatalf("计数 ≥ 阈值应建 1 条告警，得到 %+v", active)
	}
	a := active[0]
	if a.Code != AlarmBridgeFdbLimitReached || a.Severity != network.SeverityWarning || a.Source != "vs-ll" {
		t.Fatalf("告警形状不符（码/级别/来源），得到 %+v", a)
	}
	for _, want := range []string{"vs-ll", "3", "bridge fdb show br vs-ll"} {
		if !strings.Contains(a.Message, want) {
			t.Fatalf("告警文案应含 %q，得到 %q", want, a.Message)
		}
	}

	// 计数回落到阈值下 → 同 source 自动消解。
	below := &fakeRunner{replies: []fakeReply{{prefix: "bridge -j fdb show br vs-ll", out: fdbJSON(2)}}}
	p2 := New(below)
	p2.SetAlarms(store)
	if errs := p2.CheckLearnLimits(context.Background(), learnLimitCfg(3)); len(errs) != 0 {
		t.Fatalf("计数可读时不应报错：%v", errs)
	}
	if got := store.List("active"); len(got) != 0 {
		t.Fatalf("计数回落应消解，得到 %+v", got)
	}
}

// 决策 #435：计数取不到（命令失败）时**不误报、也不据此消警**——既有告警保留，读取失败如实报出。
func TestCheckLearnLimitsReadFailurePreservesAlarm(t *testing.T) {
	// 前置：先建一条告警。
	above := &fakeRunner{replies: []fakeReply{{prefix: "bridge -j fdb show br vs-ll", out: fdbJSON(3)}}}
	p := New(above)
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	p.CheckLearnLimits(context.Background(), learnLimitCfg(3))
	if len(store.List("active")) != 1 {
		t.Fatalf("前置：应先有 1 条告警")
	}

	// 命令失败：不新增、也不消解（保留）。
	broken := &fakeRunner{replies: []fakeReply{
		{prefix: "bridge -j fdb show br vs-ll", err: errors.New("bridge: command failed")},
	}}
	p2 := New(broken)
	p2.SetAlarms(store)
	errs := p2.CheckLearnLimits(context.Background(), learnLimitCfg(3))
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "vs-ll") {
		t.Fatalf("读取失败应如实报出（带交换机名）：%v", errs)
	}
	active := store.List("active")
	if len(active) != 1 || active[0].Code != AlarmBridgeFdbLimitReached {
		t.Fatalf("读取失败不得消警、也不得新增告警，得到 %+v", active)
	}
}

// 决策 #435：声明/对象消失即自动消解（对账清警口径，与 #188/#333 同族）；未声明 learn-limit
// 的交换机与 type=l3 交换机不参与计数（也不建警）。
func TestCheckLearnLimitsResolvesWhenDeclarationRemoved(t *testing.T) {
	above := &fakeRunner{replies: []fakeReply{{prefix: "bridge -j fdb show br vs-ll", out: fdbJSON(5)}}}
	p := New(above)
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	p.CheckLearnLimits(context.Background(), learnLimitCfg(1))
	if len(store.List("active")) != 1 {
		t.Fatalf("前置：应先有 1 条告警")
	}

	// 声明删除（配置里没有该交换机）→ 滞留告警消解。
	p2 := New(&fakeRunner{})
	p2.SetAlarms(store)
	if errs := p2.CheckLearnLimits(context.Background(), model.Config{}); len(errs) != 0 {
		t.Fatalf("无声明时不应报错：%v", errs)
	}
	if got := store.List("active"); len(got) != 0 {
		t.Fatalf("声明删除应消解滞留告警，得到 %+v", got)
	}

	// 未声明 learn-limit / type=l3：不发 fdb 查询、不建警。
	f := &fakeRunner{}
	p3 := New(f)
	p3.SetAlarms(store)
	skip := model.Config{VirtualSwitches: []model.VirtualSwitch{
		{Name: "vs-plain", Type: "l2"},             // 无 learn-limit
		{Name: "vs-l3", Type: "l3", LearnLimit: 1}, // L3（L2 专属，不应参与）
	}}
	if errs := p3.CheckLearnLimits(context.Background(), skip); len(errs) != 0 {
		t.Fatalf("跳过项不应报错：%v", errs)
	}
	if f.has("fdb") {
		t.Fatalf("无 learn-limit/L3 交换机不应查询 fdb：\n%s", f.joined())
	}
	if got := store.List("active"); len(got) != 0 {
		t.Fatalf("跳过项不应建警，得到 %+v", got)
	}
}

// 决策 #435：未注入告警表（测试/工具场景）时空操作，不 panic。
func TestCheckLearnLimitsWithoutAlarmStore(t *testing.T) {
	p := New(&fakeRunner{})
	if errs := p.CheckLearnLimits(context.Background(), learnLimitCfg(1)); len(errs) != 0 {
		t.Fatalf("未注入告警表时应为空操作：%v", errs)
	}
}
