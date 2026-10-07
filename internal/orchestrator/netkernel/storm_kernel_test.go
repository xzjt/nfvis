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
		"tc filter add dev ens192 ingress protocol all pref 30 flower dst_mac ff:ff:ff:ff:ff:ff " +
			"action police rate 1000kbit burst 1000000 drop",
		"tc filter add dev ens192 ingress protocol all pref 40 flower dst_mac 01:00:00:00:00:00/01:00:00:00:00:00 " +
			"action police rate 2000kbit burst 2000000 drop",
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
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter show dev ens192 ingress",
		out: "filter protocol all pref 30 flower chain 0 \n" +
			"filter protocol all pref 30 flower chain 0 handle 0x1 \n" +
			"  dst_mac ff:ff:ff:ff:ff:ff\n" +
			"\taction order 1:  police 0x1 rate 1Mbit burst 1000000b mtu 2Kb action drop overhead 0b \n" +
			"filter protocol all pref 40 flower chain 0 \n" +
			"  dst_mac 01:00:00:00:00:00/01:00:00:00:00:00\n" +
			"\taction order 1:  police 0x2 rate 1500Kbit burst 2000000b mtu 2Kb action drop overhead 0b ",
	}}}
	m := newStormManager(f)
	attached, detail, err := m.Dataplane(context.Background(), "ens192")
	if err != nil {
		t.Fatal(err)
	}
	if !attached {
		t.Fatalf("应报已下发，detail=%q", detail)
	}
	for _, want := range []string{"广播 1000 kbps", "组播 1500 kbps"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail 缺 %q，得到 %q", want, detail)
		}
	}
}

func TestStormDataplaneNotAttachedWhenNoOwnFilters(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter show dev ens192 ingress",
		out:    "filter protocol all pref 30 flower chain 0 \n  dst_mac 02:00:00:00:00:01\n\taction order 1: gact action drop",
	}}}
	m := newStormManager(f)
	attached, detail, err := m.Dataplane(context.Background(), "ens192")
	if err != nil {
		t.Fatal(err)
	}
	if attached {
		t.Fatalf("别的族的过滤器不应算作风暴抑制，detail=%q", detail)
	}
}

func TestStormDataplanePropagatesReadError(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter show dev ens192 ingress",
		out:    "Cannot find device \"ens192\"",
		err:    errors.New("exit status 1"),
	}}}
	m := newStormManager(f)
	if _, _, err := m.Dataplane(context.Background(), "ens192"); err == nil {
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
