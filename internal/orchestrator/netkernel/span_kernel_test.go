package netkernel

import (
	"context"
	"errors"
	"testing"
)

// 端口镜像族单测：只校验命令生成与错误分类（真机行为由集成测试用真 tc 核对）。

func TestSpanApplyIngressCommands(t *testing.T) {
	f := &fakeRunner{}
	m := newSpanManager(f)
	if err := m.Apply(context.Background(), "ens192", "ens256", "ingress"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"tc qdisc add dev ens192 clsact",
		"ip link set dev ens256 up",
		"tc filter del dev ens192 ingress pref 20",
		"tc filter del dev ens192 egress pref 20",
		"tc filter add dev ens192 ingress pref 20 matchall action mirred egress mirror dev ens256",
	} {
		if !f.has(want) {
			t.Fatalf("缺少命令 %q：\n%s", want, f.joined())
		}
	}
	if f.has("tc filter add dev ens192 egress pref 20") {
		t.Fatalf("ingress 方向不应装 egress filter：\n%s", f.joined())
	}
}

func TestSpanApplyBothInstallsBothHooks(t *testing.T) {
	f := &fakeRunner{}
	m := newSpanManager(f)
	if err := m.Apply(context.Background(), "ens192", "ens256", "both"); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"ingress", "egress"} {
		if !f.has("tc filter add dev ens192 " + dir + " pref 20 matchall action mirred egress mirror dev ens256") {
			t.Fatalf("both 应在 %s 上装 filter：\n%s", dir, f.joined())
		}
	}
}

func TestSpanApplyEmptyDirectionMeansBoth(t *testing.T) {
	f := &fakeRunner{}
	m := newSpanManager(f)
	if err := m.Apply(context.Background(), "ens192", "ens256", ""); err != nil {
		t.Fatal(err)
	}
	if !f.has("tc filter add dev ens192 egress pref 20") {
		t.Fatalf("留空方向应等价 both：\n%s", f.joined())
	}
}

func TestSpanApplyRemovesStaleHookWhenDirectionShrinks(t *testing.T) {
	f := &fakeRunner{}
	m := newSpanManager(f)
	if err := m.Apply(context.Background(), "ens192", "ens256", "ingress"); err != nil {
		t.Fatal(err)
	}
	// 无论之前 egress 上有没有装过，改单边前都必须先撤掉它（否则留下过期镜像）。
	if !f.has("tc filter del dev ens192 egress pref 20") {
		t.Fatalf("改单边时应撤掉另一 hook：\n%s", f.joined())
	}
}

func TestSpanApplyRejectsInvalidArgs(t *testing.T) {
	m := newSpanManager(&fakeRunner{})
	ctx := context.Background()
	cases := map[string]func() error{
		"空源口":  func() error { return m.Apply(ctx, "", "ens256", "ingress") },
		"空分析口": func() error { return m.Apply(ctx, "ens192", "", "ingress") },
		"非法方向": func() error { return m.Apply(ctx, "ens192", "ens256", "sideways") },
	}
	for name, call := range cases {
		if err := call(); err == nil {
			t.Fatalf("%s 应报错", name)
		}
	}
}

func TestSpanApplySurfacesAnalyzerFailure(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip link set dev nosuch up",
		out:    "Cannot find device \"nosuch\"",
		err:    errors.New("exit status 1"),
	}}}
	m := newSpanManager(f)
	if err := m.Apply(context.Background(), "ens192", "nosuch", "ingress"); err == nil {
		t.Fatal("分析口不存在应如实报错")
	}
}

func TestSpanDeleteRemovesBothHooksAndReclaimsQdisc(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "tc filter show dev ens192 ingress", out: ""},
		{prefix: "tc filter show dev ens192 egress", out: ""},
	}}
	m := newSpanManager(f)
	if err := m.Delete(context.Background(), "ens192", "both"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"tc filter del dev ens192 ingress pref 20",
		"tc filter del dev ens192 egress pref 20",
		"tc qdisc del dev ens192 clsact",
	} {
		if !f.has(want) {
			t.Fatalf("缺少命令 %q：\n%s", want, f.joined())
		}
	}
}

func TestSpanDeleteKeepsQdiscWhenOtherFamilyBound(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "tc filter show dev ens192 ingress",
			out: "filter protocol all pref 10 matchall chain 0 \n\tpolice 0x1 rate 8Kbit\n"},
		{prefix: "tc filter show dev ens192 egress", out: ""},
	}}
	m := newSpanManager(f)
	if err := m.Delete(context.Background(), "ens192", "both"); err != nil {
		t.Fatal(err)
	}
	if f.has("tc qdisc del dev ens192 clsact") {
		t.Fatalf("同设备仍有其它族绑定时不应回收 qdisc：\n%s", f.joined())
	}
}

func TestSpanBoundParsesAnalyzer(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "tc filter show dev ens192 ingress",
		out: "filter protocol all pref 20 matchall chain 0 \n" +
			"\taction order 1: mirred (Egress Mirror to device ens256) pipe\n",
	}}}
	m := newSpanManager(f)
	dev, ok := m.Bound(context.Background(), "ens192")
	if !ok || dev != "ens256" {
		t.Fatalf("应解析出分析口 ens256，得到 %q ok=%v", dev, ok)
	}
}

func TestSpanBoundFalseWhenNotMirrored(t *testing.T) {
	m := newSpanManager(&fakeRunner{})
	if dev, ok := m.Bound(context.Background(), "ens192"); ok || dev != "" {
		t.Fatalf("无镜像时应返回空,false，得到 %q ok=%v", dev, ok)
	}
}
