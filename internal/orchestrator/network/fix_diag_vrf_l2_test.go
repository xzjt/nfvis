package network

// BUG-11（真机验证）：`ping <host> vrf <L2 交换机名>` 查错 VPP 表，恒报「未发出任何报文」。
//
// 由来：L2 交换机的 BVI 网关落在**专属 VRF** `vr-<交换机名>`（GatewayVRFName），而
// `Ping` 此前把操作者输入当表名直接算 TableID ⇒ 查到的是另一张（往往不存在的）表 ⇒
// VPP `no egress interface` ⇒ 产品报不可达，而链路其实是通的（假阴性）。
// 真机对照：`vrf vs-lan` 恒失败、`vrf vr-vs-lan` 1 sent / 1 received。
//
// 本文件钉住解析口径的四种输入（resolver 注入前/后），并顺带钉住表尾说明**点名实际表名**——
// 否则读到「没发出去」的人无从判断是路由问题还是域填错。

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// pingTableID 跑一次 ping 并取出发给 vppctl 的 `table-id` 值（无则返回 ""）。
func pingTableID(t *testing.T, d *Diagnostics, vrf string) string {
	t.Helper()
	sh := &fakeShell{out: "Statistics: 1 sent, 1 received, 0% packet loss\n"}
	d.shell = sh
	if _, err := d.Ping(context.Background(), PingRequest{Host: "10.10.10.100", Count: 1, VRF: vrf}); err != nil {
		t.Fatalf("Ping(vrf=%q): %v", vrf, err)
	}
	if len(sh.calls) != 1 {
		t.Fatalf("应恰好发起一次 vppctl：%v", sh.calls)
	}
	args := sh.calls[0]
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "table-id" {
			return args[i+1]
		}
	}
	return ""
}

// l2GatewayCfg 一台带 BVI 网关的 L2 交换机 + 一条显式 VRF（回归对照）。
func l2GatewayCfg() model.Config {
	return model.Config{
		VirtualSwitches: []model.VirtualSwitch{
			{Name: "vs-lan", Type: "l2", Gateway: &model.VSGateway{Addresses: []string{"10.10.10.1/24"}}},
			{Name: "vs-plain", Type: "l2"}, // 未配网关：没有专属 VRF
		},
		Vrfs: []model.Vrf{{Name: "vr-core"}},
	}
}

func resolverFromCfg(cfg model.Config) VRFTableResolver {
	return ConfigVRFTableResolver(func() (model.Config, error) { return cfg, nil })
}

// ① L2 交换机名 ⇒ 网关域的表（vr-<交换机名>）。
func TestPingVRFResolvesL2SwitchToGatewayTable(t *testing.T) {
	d := diagWith(newFakeDiag(), nil, nil)
	d.SetVRFTableResolver(resolverFromCfg(l2GatewayCfg()))

	got, want := pingTableID(t, d, "vs-lan"), strconv.FormatUint(uint64(TableID(GatewayVRFName("vs-lan"))), 10)
	if got != want {
		t.Fatalf("L2 交换机名应解析为网关域 vr-vs-lan 的表 %s，实得 %s", want, got)
	}
	if got == strconv.FormatUint(uint64(TableID("vs-lan")), 10) {
		t.Fatal("解析后仍落在同名（非网关）表上——即缺陷本身")
	}
}

// ② 显式 `vr-<名>` 写法照旧可用（表号与 ① 相同）。
func TestPingVRFExplicitGatewayNameStillWorks(t *testing.T) {
	d := diagWith(newFakeDiag(), nil, nil)
	d.SetVRFTableResolver(resolverFromCfg(l2GatewayCfg()))

	implicit := pingTableID(t, d, "vs-lan")
	explicit := pingTableID(t, d, GatewayVRFName("vs-lan"))
	if implicit != explicit {
		t.Fatalf("显式 vr-vs-lan 与交换机名 vs-lan 必须落在同一张表：%s vs %s", explicit, implicit)
	}
	if want := strconv.FormatUint(uint64(TableID(GatewayVRFName("vs-lan"))), 10); explicit != want {
		t.Fatalf("显式写法表号应为 %s，实得 %s", want, explicit)
	}
}

// ③ 普通 VRF 名（不是 L2 交换机）⇒ 与解析前完全一致（回归）。
func TestPingVRFPlainNameUnchanged(t *testing.T) {
	d := diagWith(newFakeDiag(), nil, nil)
	d.SetVRFTableResolver(resolverFromCfg(l2GatewayCfg()))

	for _, name := range []string{"vr-core", "vs-plain", "vs-unknown"} {
		if got, want := pingTableID(t, d, name), strconv.FormatUint(uint64(TableID(name)), 10); got != want {
			t.Fatalf("普通名 %q 应原样按名派生表号 %s，实得 %s", name, want, got)
		}
	}
}

// ④ 未注入解析器（nil）⇒ 旧行为逐字不变（既有调用方不受影响）。
func TestPingVRFNilResolverKeepsOldBehavior(t *testing.T) {
	d := diagWith(newFakeDiag(), nil, nil) // 不注入 resolver
	if got, want := pingTableID(t, d, "vs-lan"), strconv.FormatUint(uint64(TableID("vs-lan")), 10); got != want {
		t.Fatalf("未注入解析器时应按名直算（表号 %s），实得 %s", want, got)
	}
}

// ⑤ 解析器报错 ⇒ 上抛（不静默查错表、不落到 vppctl）。
func TestPingVRFResolverErrorIsReported(t *testing.T) {
	d := diagWith(newFakeDiag(), nil, nil)
	d.SetVRFTableResolver(func(string) (string, error) { return "", errors.New("配置读不出来") })
	sh := &fakeShell{out: "Statistics: 1 sent, 1 received, 0% packet loss\n"}
	d.shell = sh

	_, err := d.Ping(context.Background(), PingRequest{Host: "10.10.10.100", VRF: "vs-lan"})
	if err == nil || !strings.Contains(err.Error(), "配置读不出来") {
		t.Fatalf("解析失败应上抛原错误，实得 %v", err)
	}
	if len(sh.calls) != 0 {
		t.Fatalf("解析失败时不应调用 vppctl：%v", sh.calls)
	}
}

// ⑥ 显式 `gateway vrf <名>` 优先于派生名（与 ApplyGateway 同源）。
func TestPingVRFExplicitGatewayVrfWins(t *testing.T) {
	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{{Name: "vs-lan", Type: "l2",
			Gateway: &model.VSGateway{Addresses: []string{"10.10.10.1/24"}, Vrf: "vr-shared"}}},
		Vrfs: []model.Vrf{{Name: "vr-shared"}},
	}
	d := diagWith(newFakeDiag(), nil, nil)
	d.SetVRFTableResolver(resolverFromCfg(cfg))

	if got, want := pingTableID(t, d, "vs-lan"),
		strconv.FormatUint(uint64(TableID("vr-shared")), 10); got != want {
		t.Fatalf("显式 gateway.vrf 应优先：want %s got %s", want, got)
	}
}

// ⑦ 失败说明**点名实际查询的表名**：读到「没发出去」的人要能分辨「表里没路由」与「域填错」。
func TestPingNoEgressNoteNamesResolvedTable(t *testing.T) {
	d := diagWith(newFakeDiag(), nil, nil)
	d.SetVRFTableResolver(resolverFromCfg(l2GatewayCfg()))
	sh := &fakeShell{out: "Failed: no egress interface\n\nStatistics: 0 sent, 0 received, 0% packet loss\n"}
	d.shell = sh

	out, err := d.Ping(context.Background(), PingRequest{Host: "10.10.10.100", Count: 1, VRF: "vs-lan"})
	if err == nil {
		t.Fatal("0 发包必须报错")
	}
	if !strings.Contains(out, GatewayVRFName("vs-lan")) {
		t.Fatalf("未发出的说明应点名解析后的域 %q：\n%s", GatewayVRFName("vs-lan"), out)
	}
	// 未指定 vrf 时不该出现「本次查询的 VRF 域」这句（没有域可点名）。
	sh2 := &fakeShell{out: "Statistics: 0 sent, 0 received, 0% packet loss\n"}
	d.shell = sh2
	out, _ = d.Ping(context.Background(), PingRequest{Host: "10.10.10.100"})
	if strings.Contains(out, "本次查询的 VRF 域") {
		t.Fatalf("未指定 vrf 时不应点名域：\n%s", out)
	}
}

// ⑧ 配置来源为 nil ⇒ 返回 nil 解析器（装配处忘了接线也不会 panic，语义回到旧行为）。
func TestConfigVRFTableResolverNilSource(t *testing.T) {
	if fn := ConfigVRFTableResolver(nil); fn != nil {
		t.Fatal("cfgSrc 为 nil 时应返回 nil 解析器")
	}
	// 配置读取失败：如实上抛（不静默当成「名字没问题」）。
	fn := ConfigVRFTableResolver(func() (model.Config, error) { return model.Config{}, errors.New("库打不开") })
	if _, err := fn("vs-lan"); err == nil || !strings.Contains(err.Error(), "库打不开") {
		t.Fatalf("配置读取失败应上抛：%v", err)
	}
}
