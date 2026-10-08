package netkernel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// stormTestCallIndex 返回第一条包含 substr 的命令下标（-1 = 未出现），用于断言命令先后。
func stormTestCallIndex(f *fakeRunner, substr string) int {
	for i, c := range f.calls {
		if strings.Contains(c, substr) {
			return i
		}
	}
	return -1
}

func TestStormApplyBothClassesEmitsClsactAndTwoPoliceFilters(t *testing.T) {
	f := &fakeRunner{}
	m := newStormManager(f)
	err := m.Apply(context.Background(), "ens192", &model.StormControl{BroadcastKbps: 1000, MulticastKbps: 2000})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"tc qdisc add dev ens192 clsact",
		// 广播档：裸 drop（终止遍历，广播帧不再落组播档——与 VPP 侧「广播帧只落广播档」对齐）；
		// 组播档：slash 形态（超限丢、未超限继续遍历）。两档判决不同的取舍见 stormVerdictBroadcast。
		"tc filter add dev ens192 ingress protocol all pref 30 flower dst_mac ff:ff:ff:ff:ff:ff " +
			"action police rate 1000kbit burst 1000000 drop",
		"tc filter add dev ens192 ingress protocol all pref 40 flower dst_mac 01:00:00:00:00:00/01:00:00:00:00:00 " +
			"action police rate 2000kbit burst 2000000 conform-exceed drop/continue",
	} {
		if !f.has(want) {
			t.Fatalf("缺少命令 %q；实际：\n%s", want, f.joined())
		}
	}
	// 先删后加（幂等口径）：两条删都必须排在两条加之前。
	del10 := stormTestCallIndex(f, "tc filter del dev ens192 ingress pref 30")
	del20 := stormTestCallIndex(f, "tc filter del dev ens192 ingress pref 40")
	add10 := stormTestCallIndex(f, "pref 30 flower dst_mac ff:ff:ff:ff:ff:ff")
	if del10 < 0 || del20 < 0 || add10 < 0 || del10 > add10 || del20 > add10 {
		t.Fatalf("应先删自己的过滤器再加；实际：\n%s", f.joined())
	}
}

func TestStormApplyOnlyBroadcastOmitsMulticastFilter(t *testing.T) {
	f := &fakeRunner{}
	m := newStormManager(f)
	if err := m.Apply(context.Background(), "ens192", &model.StormControl{BroadcastKbps: 500}); err != nil {
		t.Fatal(err)
	}
	if !f.has("dst_mac ff:ff:ff:ff:ff:ff") {
		t.Fatalf("应下发广播过滤器；实际：\n%s", f.joined())
	}
	if f.has("dst_mac 01:00:00:00:00:00/01:00:00:00:00:00") {
		t.Fatalf("未声明的组播不应下发；实际：\n%s", f.joined())
	}
	// 未声明的那一类仍要先删（可能上一版配过）。
	if !f.has("tc filter del dev ens192 ingress pref 40") {
		t.Fatalf("两类都应先删；实际：\n%s", f.joined())
	}
}

func TestStormApplyIdempotentSkipsExistingClsact(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc qdisc show dev ens192",
		out:    "qdisc clsact 0: dev ens192 root refcnt 2",
	}}}
	m := newStormManager(f)
	if err := m.Apply(context.Background(), "ens192", &model.StormControl{MulticastKbps: 100}); err != nil {
		t.Fatal(err)
	}
	if f.has("tc qdisc add") {
		t.Fatalf("clsact 已在时不应重复 add；实际：\n%s", f.joined())
	}
}

func TestStormApplyNilTearsDownAndReclaimsIdleClsact(t *testing.T) {
	f := &fakeRunner{}
	m := newStormManager(f)
	if err := m.Apply(context.Background(), "ens192", nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"tc filter del dev ens192 ingress pref 30",
		"tc filter del dev ens192 ingress pref 40",
		"tc qdisc del dev ens192 clsact",
	} {
		if !f.has(want) {
			t.Fatalf("撤除路径缺少 %q；实际：\n%s", want, f.joined())
		}
	}
}

func TestStormTeardownKeepsClsactWhenEgressFiltersRemain(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter show dev ens192 egress",
		out:    "filter protocol all pref 30 flower chain 0 \n  not_in_hw",
	}}}
	m := newStormManager(f)
	if err := m.Teardown(context.Background(), "ens192"); err != nil {
		t.Fatal(err)
	}
	if f.has("tc qdisc del") {
		t.Fatalf("egress 还有别的族的过滤器时不得删 clsact；实际：\n%s", f.joined())
	}
}

func TestStormTeardownToleratesAbsentQdisc(t *testing.T) {
	// 无 clsact 时 `tc filter del` 报 `Parent Qdisc doesn't exists.`（真机实测）：应按已达成。
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter del dev ens192 ingress",
		out:    "Error: Parent Qdisc doesn't exists.\nWe have an error talking to the kernel",
		err:    errors.New("exit status 2"),
	}}}
	m := newStormManager(f)
	if err := m.Teardown(context.Background(), "ens192"); err != nil {
		t.Fatalf("qdisc 不在时应按已达成，得到 %v", err)
	}
}

func TestStormTeardownToleratesAbsentFilterChain(t *testing.T) {
	// clsact 在、但该 pref 的过滤器不在时 `tc filter del` 报 `Cannot find specified filter chain.`
	// （真机实测）：应按已达成。
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter del dev ens192 ingress",
		out:    "Error: Cannot find specified filter chain.\nWe have an error talking to the kernel",
		err:    errors.New("exit status 2"),
	}}}
	m := newStormManager(f)
	if err := m.Apply(context.Background(), "ens192", &model.StormControl{BroadcastKbps: 1}); err != nil {
		t.Fatalf("过滤器不在时应按已达成，得到 %v", err)
	}
}

func TestStormApplyEmptyDeclarationTearsDown(t *testing.T) {
	f := &fakeRunner{}
	m := newStormManager(f)
	if err := m.Apply(context.Background(), "ens192", &model.StormControl{}); err != nil {
		t.Fatal(err)
	}
	if f.has("tc filter add") {
		t.Fatalf("空声明不应下发任何过滤器；实际：\n%s", f.joined())
	}
	if !f.has("tc filter del dev ens192 ingress pref 30") {
		t.Fatalf("空声明应走撤除路径；实际：\n%s", f.joined())
	}
}

func TestStormApplyPropagatesQdiscError(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc qdisc add dev ens192 clsact",
		out:    "Cannot find device \"ens192\"",
		err:    errors.New("exit status 1"),
	}}}
	m := newStormManager(f)
	err := m.Apply(context.Background(), "ens192", &model.StormControl{BroadcastKbps: 1})
	if err == nil || !strings.Contains(err.Error(), "clsact") {
		t.Fatalf("qdisc 下发失败应如实上报，得到 %v", err)
	}
}

func TestStormDataplaneReportsClassesAndRates(t *testing.T) {
	// 真机形态：同一接口上两档的打印**本来就不一样**——广播档是裸 drop（`action drop`）、
	// 组播档是 slash 形态（`action drop/continue`）。读视图必须两种都认（见 tcPoliceDrops），
	// 否则"在位限速"会被报成"未下发"。
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter show dev ens192 ingress",
		out: "filter protocol all pref 30 flower chain 0 \n" +
			"filter protocol all pref 30 flower chain 0 handle 0x1 \n" +
			"  dst_mac ff:ff:ff:ff:ff:ff\n" +
			"\taction order 1:  police 0x1 rate 1Mbit burst 1000000b mtu 2Kb action drop overhead 0b \n" +
			"filter protocol all pref 40 flower chain 0 \n" +
			"  dst_mac 01:00:00:00:00:00/01:00:00:00:00:00\n" +
			"\taction order 1:  police 0x2 rate 1500Kbit burst 2000000b mtu 2Kb action drop/continue overhead 0b ",
	}}}
	m := newStormManager(f)
	fact, err := m.Dataplane(context.Background(), "ens192")
	if err != nil {
		t.Fatal(err)
	}
	if !fact.Attached {
		t.Fatalf("应报已下发，detail=%q", fact.Detail)
	}
	for _, want := range []string{"广播 1000 kbps", "组播 1500 kbps"} {
		if !strings.Contains(fact.Detail, want) {
			t.Fatalf("detail 缺 %q，得到 %q", want, fact.Detail)
		}
	}
	// 消费方真正读的是逐类速率（api 的 Kinds）——真话必须落在这里（R2-15②）。
	if fact.BroadcastKbps != 1000 || fact.MulticastKbps != 1500 {
		t.Fatalf("逐类实测速率不符：broadcast=%d multicast=%d", fact.BroadcastKbps, fact.MulticastKbps)
	}
}

// 广播档（裸 drop）与升级前的旧现场：`… burst <n> drop` 打印成 `action drop`——读视图照常认
// （不能报成"未下发"）。广播档当前就是这个形态；组播档在修复前的旧现场同样可能打印它。
func TestStormDataplaneToleratesPlainDropForm(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter show dev ens192 ingress",
		out: "filter protocol all pref 30 flower chain 0 \n" +
			"  dst_mac ff:ff:ff:ff:ff:ff\n" +
			"\taction order 1:  police 0x1 rate 1Mbit burst 1000000b mtu 2Kb action drop overhead 0b \n" +
			"filter protocol all pref 40 flower chain 0 \n" +
			"  dst_mac 01:00:00:00:00:00/01:00:00:00:00:00\n" +
			"\taction order 1:  police 0x2 rate 2Mbit burst 2000000b mtu 2Kb action drop overhead 0b ",
	}}}
	m := newStormManager(f)
	fact, err := m.Dataplane(context.Background(), "ens192")
	if err != nil {
		t.Fatal(err)
	}
	if !fact.Attached || fact.BroadcastKbps != 1000 || fact.MulticastKbps != 2000 {
		t.Fatalf("裸 drop 形态应照常认作在位（广播 1000 / 组播 2000 kbps），得到 %+v", fact)
	}
}

// 反向写法（`action continue/drop`：超限继续、未超限被丢）不是本产品的形态：认了会把
// "限速失效的过滤器"报成在位。这里钉住"不认"。
func TestStormDataplaneRejectsReversedVerdictForm(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter show dev ens192 ingress",
		out: "filter protocol all pref 30 flower chain 0 \n" +
			"  dst_mac ff:ff:ff:ff:ff:ff\n" +
			"\taction order 1:  police 0x1 rate 1Mbit burst 1000000b mtu 2Kb action continue/drop overhead 0b ",
	}}}
	m := newStormManager(f)
	fact, err := m.Dataplane(context.Background(), "ens192")
	if err != nil {
		t.Fatal(err)
	}
	if fact.Attached {
		t.Fatalf("反向判决不应算作本产品的风暴抑制：%+v", fact)
	}
}

// 过滤器在、但速率解析不出来：如实报「读不到实况」，不猜一个速率（猜出来的 CIR 会掩盖
// "数据面与配置不一致"）。消费方据此渲染成"不可核对（原因）"而不是"未收敛"。
func TestStormDataplaneRateUnparsableIsReadFailure(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter show dev ens192 ingress",
		out: "filter protocol all pref 30 flower chain 0 \n" +
			"  dst_mac ff:ff:ff:ff:ff:ff\n" +
			"\taction order 1:  police 0x1 burst 1000000b action drop/continue\n",
	}}}
	m := newStormManager(f)
	if _, err := m.Dataplane(context.Background(), "ens192"); err == nil {
		t.Fatalf("速率读不到应如实报错，而不是猜一个值")
	}
}

func TestStormDataplaneNotAttachedWhenNoOwnFilters(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter show dev ens192 ingress",
		out:    "filter protocol all pref 30 flower chain 0 \n  dst_mac 02:00:00:00:00:01\n\taction order 1: gact action drop",
	}}}
	m := newStormManager(f)
	fact, err := m.Dataplane(context.Background(), "ens192")
	if err != nil {
		t.Fatal(err)
	}
	if fact.Attached {
		t.Fatalf("别的族的过滤器不应算作风暴抑制，detail=%q", fact.Detail)
	}
}

func TestStormDataplanePropagatesReadError(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter show dev ens192 ingress",
		out:    "Cannot find device \"ens192\"",
		err:    errors.New("exit status 1"),
	}}}
	m := newStormManager(f)
	if _, err := m.Dataplane(context.Background(), "ens192"); err == nil {
		t.Fatalf("读不到实况应返回错误")
	}
}

func TestStormBurstBytesMirrorsEightSecondWindow(t *testing.T) {
	if got := stormBurstBytes(1000); got != 1000000 {
		t.Fatalf("1000 kbps 的突发桶应为 1000000 字节，得到 %d", got)
	}
	if stormBurstBytes(0) < 1 {
		t.Fatalf("非正速率也应给出正的桶容量")
	}
}

func TestStormUnitToKbpsParsesTcFormats(t *testing.T) {
	cases := map[string]int{
		"1Mbit":    1000,
		"1500Kbit": 1500,
		"500bit":   0,
		"1Gbit":    1000000,
		"abc":      -1,
	}
	for in, want := range cases {
		if got := stormUnitToKbps(in); got != want {
			t.Fatalf("stormUnitToKbps(%q) = %d，期望 %d", in, got, want)
		}
	}
}
