package netkernel

import (
	"context"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// R2-3 的钉住单测：同一条 clsact hook 上多族共存，靠"判决明确写出来、该让路的让路"实现——
//   - QoS 的 police 写 `conform-exceed drop/continue`（超限丢、未超限继续遍历）；
//   - 镜像的 mirred 尾随 `continue`（默认 pipe 判决会吞掉包）；
//   - 风暴抑制**按档不同**：广播档（本族第一条，pref 30）裸 `drop`——主动终止遍历以保证
//     「广播帧只落广播档」与 VPP 同语义；组播档（本族最后一条，pref 40）`drop/continue`——
//     不让路才有意义时就不让路。
//
// 为什么必须钉在 argv 上：内核语义是"同一 hook 上首个判决 ≥0 的 filter 命中即返回"，判决写错
// 一侧就会把排在后面的族**整族静默屏蔽**（命令成功、filter 在位、被屏蔽那边计数恒 0）。
// 改回旧 argv 时下面各条用例逐条失败（红-绿）。

func TestQoSPoliceArgvCarriesConformExceedContinue(t *testing.T) {
	f := &fakeRunner{}
	m := newQoSManager(f)
	if err := m.Bind(context.Background(), "ens192", "ingress", 8000, 1000); err != nil {
		t.Fatal(err)
	}
	add := ""
	for _, c := range f.calls {
		if strings.HasPrefix(c, "tc filter add dev ens192 ingress pref 10") {
			add = c
		}
	}
	if add == "" {
		t.Fatalf("未下发 QoS filter：\n%s", f.joined())
	}
	if !strings.Contains(add, "conform-exceed drop/continue") {
		t.Fatalf("QoS police 必须写 conform-exceed drop/continue（否则短路同 hook 的其它族），得到：%s", add)
	}
	if strings.HasSuffix(add, " drop") {
		t.Fatalf("QoS police 不应是旧的裸 drop 写法（合规包会被吞掉），得到：%s", add)
	}
}

// 风暴抑制两档的判决**按档不同**（同一份配置、同一语义的对齐口径）：
//   - 广播档（pref 30，本族第一条）裸 `drop`——终止遍历，广播帧不再落组播（I/G）档，
//     与 VPP 侧「两类各有独立分类表、广播帧只落广播档」一致；若写成 drop/continue，
//     `broadcast 10000 + multicast 8` 会把广播实际压到 8 kbps（比配置更紧）。
//     前提：本档短路只影响排在它后面的族（当前只有本族的组播档；将来在 pref 40 之后再挂族要复审）。
//   - 组播档（pref 40，本族最后一条）`conform-exceed drop/continue`——超限丢、未超限继续遍历，
//     避免成为未来更高 pref 族的隐形墙。
//
// 红-绿：把广播档改回带 continue 的 argv（或把组播档改回裸 drop）⇒ 本用例按预期失败。
func TestStormPoliceArgvVerdictsPerClass(t *testing.T) {
	f := &fakeRunner{}
	m := newStormManager(f)
	if err := m.Apply(context.Background(), "ens192",
		&model.StormControl{BroadcastKbps: 1000, MulticastKbps: 2000}); err != nil {
		t.Fatal(err)
	}
	bcast := "action police rate 1000kbit burst 1000000 drop"
	if !f.has("dst_mac ff:ff:ff:ff:ff:ff " + bcast) {
		t.Fatalf("广播档 argv 应为裸 drop（终止遍历）：缺 %q\n%s", bcast, f.joined())
	}
	if f.has("dst_mac ff:ff:ff:ff:ff:ff action police rate 1000kbit burst 1000000 conform-exceed") {
		t.Fatalf("广播档不应带 conform-exceed（continue 会让广播帧继续落组播档、把广播压得比配置更紧）：\n%s", f.joined())
	}
	mcast := "action police rate 2000kbit burst 2000000 conform-exceed drop/continue"
	if !f.has("dst_mac 01:00:00:00:00:00/01:00:00:00:00:00 " + mcast) {
		t.Fatalf("组播档 argv 应为 conform-exceed drop/continue：缺 %q\n%s", mcast, f.joined())
	}
	if f.has("dst_mac 01:00:00:00:00:00/01:00:00:00:00:00 action police rate 2000kbit burst 2000000 drop") {
		t.Fatalf("组播档不应退回裸 drop（会变成未来更高 pref 族的隐形墙）：\n%s", f.joined())
	}
}

func TestSpanMirredArgvCarriesContinue(t *testing.T) {
	f := &fakeRunner{}
	m := newSpanManager(f)
	if err := m.Apply(context.Background(), "ens192", "ens256", "both"); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"ingress", "egress"} {
		want := "tc filter add dev ens192 " + dir + " pref 20 matchall action mirred egress mirror dev ens256 continue"
		if !f.has(want) {
			t.Fatalf("%s 上的 mirred 必须尾随 continue（默认 pipe 判决会短路同 hook 的后续族），实际：\n%s",
				dir, f.joined())
		}
	}
}

// tcPoliceDrops 的判别力：两种"超限丢弃"打印形态都认——**两种在本产品里都实际在用**
// （广播档裸 drop 打印 `action drop`、组播档 slash 形态打印 `action drop/continue`，
// 同一接口上会同时出现），另外也要认升级前旧现场留下的 `action drop`。
// 反向写法（超限继续、未超限被丢）与"没有 police"都不认——读视图不能把失效的过滤器报成在位。
func TestTCPoliceDropsRecognizesBothForms(t *testing.T) {
	cases := map[string]bool{
		// slash 形态：iproute2 把 `conform-exceed drop/continue` 打印成 `action drop/continue`
		"action order 1:  police 0x1 rate 8Kbit burst 1000b action drop/continue overhead 0b": true,
		// 兼容 iproute2 直接打印 conform-exceed 字样的版本
		"police 0x1 rate 8Kbit burst 1000b conform-exceed drop/continue": true,
		// 裸 drop：广播档的当前形态（也是升级前旧现场的形态）
		"action order 1:  police 0x1 rate 8Kbit burst 1000b action drop overhead 0b": true,
		// 反向写法：超限继续、未超限被丢——不是本产品形态
		"action order 1:  police 0x1 rate 8Kbit burst 1000b action continue/drop overhead 0b": false,
		// 别族的过滤器：没有 police
		"action order 1: gact action drop": false,
	}
	for block, want := range cases {
		if got := tcPoliceDrops(block); got != want {
			t.Fatalf("tcPoliceDrops(%q) = %v，期望 %v", block, got, want)
		}
	}
}
