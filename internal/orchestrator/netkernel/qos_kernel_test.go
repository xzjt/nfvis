package netkernel

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// QoS 族单测：只校验命令生成与错误分类（真机行为由 qos_span_kernel_integration_test.go 用真 tc 核对）。

func TestQoSBindIngressThenEgressCommands(t *testing.T) {
	f := &fakeRunner{}
	m := newQoSManager(f)
	ctx := context.Background()

	if err := m.Bind(ctx, "ens192", "ingress", 8000, 1000); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"tc qdisc add dev ens192 clsact",
		"tc filter del dev ens192 ingress pref 10",
		"tc filter add dev ens192 ingress pref 10 matchall action police rate 8000bit burst 1000 conform-exceed drop/continue",
	}
	if got := strings.Join(f.calls, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("入向下发序列不符：\n%s", got)
	}

	f.calls = nil
	if err := m.Bind(ctx, "ens192", "egress", 16000, 2000); err != nil {
		t.Fatal(err)
	}
	if !f.has("tc filter add dev ens192 egress pref 10 matchall action police rate 16000bit burst 2000 conform-exceed drop/continue") {
		t.Fatalf("出向命令不符：\n%s", f.joined())
	}
	// 绑出向不应碰入向的 filter。
	if f.has("tc filter del dev ens192 ingress") {
		t.Fatalf("绑出向不应摘入向 filter：\n%s", f.joined())
	}
}

func TestQoSBindIsDeleteThenAdd(t *testing.T) {
	f := &fakeRunner{}
	m := newQoSManager(f)
	ctx := context.Background()
	if err := m.Bind(ctx, "ens192", "ingress", 8000, 1000); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := m.Bind(ctx, "ens192", "ingress", 8000, 1000); err != nil {
		t.Fatal(err)
	}
	// 重复绑定必须先删再加（tc filter add 会重复追加，只 add 会叠出多条计量器）。
	delIdx, addIdx := -1, -1
	for i, c := range f.calls {
		if delIdx < 0 && c == "tc filter del dev ens192 ingress pref 10" {
			delIdx = i
		}
		if strings.HasPrefix(c, "tc filter add dev ens192 ingress pref 10") {
			addIdx = i
		}
	}
	if delIdx < 0 || addIdx < 0 || delIdx > addIdx {
		t.Fatalf("重复绑定应先删后加：%v", f.calls)
	}
}

func TestQoSBindToleratesExistingClsact(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc qdisc add dev ens192 clsact",
		out:    "Error: Exclusivity flag on, cannot modify.",
		err:    errors.New("exit status 2"),
	}}}
	m := newQoSManager(f)
	if err := m.Bind(context.Background(), "ens192", "ingress", 8000, 1000); err != nil {
		t.Fatalf("clsact 已存在应按幂等处理，得到 %v", err)
	}
}

func TestQoSBindToleratesEmptyFilterChain(t *testing.T) {
	// 真机实测：clsact 刚建、该 hook 上一条 filter 都没有时，按 pref 删除报
	// "Cannot find specified filter chain."（与本就没绑同义），绑定必须照常继续。
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter del dev ens192 ingress pref 10",
		out:    "Error: Cannot find specified filter chain.\nWe have an error talking to the kernel",
		err:    errors.New("exit status 2"),
	}}}
	m := newQoSManager(f)
	if err := m.Bind(context.Background(), "ens192", "ingress", 8000, 1000); err != nil {
		t.Fatalf("空 filter 链时绑定应照常继续，得到 %v", err)
	}
	if !f.has("tc filter add dev ens192 ingress pref 10 matchall action police rate 8000bit burst 1000 conform-exceed drop/continue") {
		t.Fatalf("绑定应仍然下发：\n%s", f.joined())
	}
}

func TestQoSBindRejectsInvalidArgs(t *testing.T) {
	m := newQoSManager(&fakeRunner{})
	ctx := context.Background()
	cases := map[string]func() error{
		"空接口名": func() error { return m.Bind(ctx, "", "ingress", 8000, 1000) },
		"非法方向": func() error { return m.Bind(ctx, "ens192", "sideways", 8000, 1000) },
		"速率为零": func() error { return m.Bind(ctx, "ens192", "ingress", 0, 1000) },
		"速率为负": func() error { return m.Bind(ctx, "ens192", "ingress", -1, 1000) },
		"突发为零": func() error { return m.Bind(ctx, "ens192", "ingress", 8000, 0) },
	}
	for name, call := range cases {
		if err := call(); err == nil {
			t.Fatalf("%s 应报错", name)
		}
	}
}

func TestQoSUnbindKeepsQdiscWhenOtherDirectionBound(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "tc filter show dev ens192 ingress", out: ""},
		{prefix: "tc filter show dev ens192 egress",
			out: "filter protocol all pref 10 matchall chain 0 \n\tpolice 0x1 rate 8Kbit\n"},
	}}
	m := newQoSManager(f)
	if err := m.Unbind(context.Background(), "ens192", "ingress"); err != nil {
		t.Fatal(err)
	}
	if !f.has("tc filter del dev ens192 ingress pref 10") {
		t.Fatalf("应摘除 ingress filter：\n%s", f.joined())
	}
	if f.has("tc qdisc del dev ens192 clsact") {
		t.Fatalf("另一方向仍绑定时不应回收 qdisc：\n%s", f.joined())
	}
}

func TestQoSUnbindReclaimsQdiscWhenNoFiltersLeft(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "tc filter show dev ens192 ingress", out: ""},
		{prefix: "tc filter show dev ens192 egress", out: ""},
	}}
	m := newQoSManager(f)
	if err := m.Unbind(context.Background(), "ens192", "egress"); err != nil {
		t.Fatal(err)
	}
	if !f.has("tc qdisc del dev ens192 clsact") {
		t.Fatalf("两向都空时应回收 qdisc：\n%s", f.joined())
	}
}

func TestQoSUnbindKeepsQdiscWhenOtherFamilyBound(t *testing.T) {
	// 同设备上还挂着别的族的 filter（这里用 pref 20 的镜像模拟）——不得回收 qdisc。
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "tc filter show dev ens192 ingress",
			out: "filter protocol all pref 20 matchall chain 0 \n\tmirred (Egress Mirror to device ens256) pipe\n"},
		{prefix: "tc filter show dev ens192 egress", out: ""},
	}}
	m := newQoSManager(f)
	if err := m.Unbind(context.Background(), "ens192", "ingress"); err != nil {
		t.Fatal(err)
	}
	if f.has("tc qdisc del dev ens192 clsact") {
		t.Fatalf("同设备仍有别的族绑定时不应回收 qdisc：\n%s", f.joined())
	}
}

func TestQoSUnbindToleratesMissingQdisc(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "tc filter del dev ens192 ingress",
			out: "Error: Parent Qdisc doesn't exists.\nWe have an error talking to the kernel",
			err: errors.New("exit status 2")},
		{prefix: "tc filter show dev ens192 ingress", out: ""},
		{prefix: "tc filter show dev ens192 egress", out: ""},
		{prefix: "tc qdisc del dev ens192 clsact",
			out: "Error: Cannot find specified qdisc on specified device.",
			err: errors.New("exit status 2")},
	}}
	m := newQoSManager(f)
	if err := m.Unbind(context.Background(), "ens192", "ingress"); err != nil {
		t.Fatalf("无 qdisc 时解绑应幂等，得到 %v", err)
	}
}

func TestQoSBoundReadsKernelFact(t *testing.T) {
	// 新写法（修复 R2-3 后下发的形态）：`conform-exceed drop/continue` 被 iproute2 打印成
	// `action drop/continue`——读视图必须认它。
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter show dev ens192 ingress",
		out:    "filter protocol all pref 10 matchall chain 0 \n\tpolice 0x1 rate 8Kbit burst 1000b action drop/continue\n",
	}}}
	m := newQoSManager(f)
	ok, err := m.Bound(context.Background(), "ens192", "ingress")
	if err != nil || !ok {
		t.Fatalf("应读到本族绑定，得到 ok=%v err=%v", ok, err)
	}
}

// 旧写法（修复 R2-3 前的现场）打印成 `action drop`：升级后读旧现场同样要认（不能报成未绑定）。
func TestQoSBoundToleratesLegacyPoliceForm(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter show dev ens192 ingress",
		out:    "filter protocol all pref 10 matchall chain 0 \n\tpolice 0x1 rate 8Kbit burst 1000b action drop\n",
	}}}
	m := newQoSManager(f)
	if ok, err := m.Bound(context.Background(), "ens192", "ingress"); err != nil || !ok {
		t.Fatalf("旧写法应照常认作绑定，得到 ok=%v err=%v", ok, err)
	}
}

// 判决被改成"超限继续"（`action continue/drop` 反向写法）时，pref 还在但限速已不拦包：
// 报"已绑定"就是谎报——读视图只认"超限丢弃"判决。
func TestQoSBoundRejectsReversedVerdict(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter show dev ens192 ingress",
		out:    "filter protocol all pref 10 matchall chain 0 \n\tpolice 0x1 rate 8Kbit burst 1000b action continue/drop\n",
	}}}
	m := newQoSManager(f)
	if ok, err := m.Bound(context.Background(), "ens192", "ingress"); err != nil || ok {
		t.Fatalf("反向判决不应报已绑定，得到 ok=%v err=%v", ok, err)
	}
}

func TestQoSBoundIgnoresOtherPref(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter show dev ens192 ingress",
		out:    "filter protocol all pref 20 matchall chain 0 \n\tmirred (Egress Mirror to device ens256) pipe\n",
	}}}
	m := newQoSManager(f)
	if ok, err := m.Bound(context.Background(), "ens192", "ingress"); err != nil || ok {
		t.Fatalf("别的 pref 的 filter 不应算作本族绑定，得到 ok=%v err=%v", ok, err)
	}
}

func TestQoSBoundFalseWhenNothingBound(t *testing.T) {
	m := newQoSManager(&fakeRunner{})
	if ok, err := m.Bound(context.Background(), "ens192", "ingress"); err != nil || ok {
		t.Fatalf("无绑定时应返回 false,nil，得到 ok=%v err=%v", ok, err)
	}
}
