package netkernel

import (
	"context"
	"errors"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// R2-24：`ip -d -j link show` 才有 `linkinfo`——不带 `-d` 时 DevType 恒得「physical」
// （真机实测 bridge/vrf/vlan/bond/veth 全都没有该键）。本用例钉住命令形态与解析。
func TestInterfaceStatesReadsDevTypeFromDeviceDetails(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -d -j link show",
		out: `[{"ifname":"vs-l3","flags":["UP"],"operstate":"up","mtu":1500,
		        "linkinfo":{"info_kind":"vrf"}},
		       {"ifname":"vs-lan","flags":["UP"],"operstate":"up","mtu":1500,
		        "linkinfo":{"info_kind":"bridge"}},
		       {"ifname":"ens192","flags":["UP"],"operstate":"up","mtu":9000}]`,
	}}}
	got, err := NewRuntime(f).InterfaceStates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !f.has("ip -d -j link show") {
		t.Fatalf("必须读设备详情（-d）才有 linkinfo；实际：\n%s", f.joined())
	}
	for name, want := range map[string]string{
		"vs-l3": "vrf", "vs-lan": "bridge", "ens192": "physical",
	} {
		if got[name].DevType != want {
			t.Fatalf("%s 的 DevType 应为 %q，得到 %q", name, want, got[name].DevType)
		}
	}
}

// 真机 3.0.5~dev1 回归（R2-15① 的修复引入）：读视图必须跟**当前 committed 配置**走，
// 而不是装配期写入的进程内快照——提交路径不经过 Provider（Apply* 只拿到单个对象），
// 快照只在装配与恢复收敛时更新，于是提交后读视图滞留旧快照：新建的交换机一条都列不出来
// （`show virtual-switches` 恒空），15s 巡检也不刷新（它只更新 EnsureForwarding 的入参）。
func TestReadViewsFollowConfigSourceNotStaleSnapshot(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "bridge -j link show", out: `[]`},
		{prefix: "ip -j link show", out: `[{"ifname":"vs-lan"}]`},
		{prefix: "ip -d -j link show", out: `[{"ifname":"vs-lan","linkinfo":{"info_kind":"bridge"}}]`},
	}}
	p := New(f)
	p.SetConfig(model.Config{}) // 装配期快照：空（提交不会刷新它）
	live := model.Config{}
	p.SetConfigSource(func() (model.Config, error) { return live, nil })

	if got, err := p.BridgeDomains(); err != nil || len(got) != 0 {
		t.Fatalf("来源为空时不应列出交换机：%+v %v", got, err)
	}
	// 一次提交后 committed 变了：读视图必须**立刻**看到它。
	live = model.Config{VirtualSwitches: []model.VirtualSwitch{{Name: "vs-lan", Type: "l2"}}}
	got, err := p.BridgeDomains()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "vs-lan" {
		t.Fatalf("新提交的交换机应立刻可见（用陈旧快照时这里恒空）：%+v", got)
	}
	names, err := p.VPPIfnames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "vs-lan" {
		t.Fatalf("VPPIfnames（产品自持设备判定）必须同一来源：%v", names)
	}
}

// 来源读失败时回落既有快照：不把「读不到配置」显示成「没有配置」。
func TestConfigSourceFailureFallsBackToSnapshot(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "bridge -j link show", out: `[]`},
		{prefix: "ip -j link show", out: `[{"ifname":"vs-lan"}]`},
		{prefix: "ip -d -j link show", out: `[{"ifname":"vs-lan","linkinfo":{"info_kind":"bridge"}}]`},
	}}
	p := New(f)
	p.SetConfig(model.Config{VirtualSwitches: []model.VirtualSwitch{{Name: "vs-lan", Type: "l2"}}})
	p.SetConfigSource(func() (model.Config, error) {
		return model.Config{}, errors.New("db down")
	})
	got, err := p.BridgeDomains()
	if err != nil || len(got) != 1 || got[0].Name != "vs-lan" {
		t.Fatalf("来源失败时应回落到快照：%+v %v", got, err)
	}
}

// R2-15①：`bridge -j link show` 以端口为行——旧实现按它枚举 bridge-domain，零成员的交换机
// 根本不进列表，读视图于是对真实存在的交换机回「在数据面中不存在」。改为**按配置声明枚举**，
// 逐台查内核：内核里有该 bridge（info_kind=bridge）才报，成员按实况。
func TestBridgeDomainsEnumeratesDeclaredSwitchesIncludingEmpty(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "bridge -j link show", out: `[{"ifname":"ens192","master":"vs-lan"}]`},
		{prefix: "ip -d -j link show", out: `[
			{"ifname":"vs-lan","linkinfo":{"info_kind":"bridge"}},
			{"ifname":"vs-empty","linkinfo":{"info_kind":"bridge"}},
			{"ifname":"virbr0","linkinfo":{"info_kind":"bridge"}}]`},
	}}
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{
		{Name: "vs-lan", Type: "l2"},
		{Name: "vs-empty", Type: "l2"}, // 零成员：必须仍然列出
		{Name: "vs-gone", Type: "l2"},  // 配置声明了、内核没有：如实不出现（不编造空壳）
		{Name: "vs-l3", Type: "l3"},    // L3 交换机不是 bridge
		// 内核里的 virbr0 未在配置中声明：产品不报（内核没有「产品自持」标记）。
	}}
	got, err := NewRuntime(f).BridgeDomains(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "vs-lan" || got[1].Name != "vs-empty" {
		t.Fatalf("应按配置声明枚举（含零成员交换机，跳过内核缺失者），得到 %+v", got)
	}
	if got[0].ID != 0 {
		t.Fatalf("内核 bridge 没有 BD-ID 概念，ID 必须为 0，得到 %d", got[0].ID)
	}
	if len(got[0].Ports) != 1 || got[0].Ports[0].Name != "ens192" {
		t.Fatalf("成员口应取内核实况，得到 %+v", got[0].Ports)
	}
	if len(got[1].Ports) != 0 {
		t.Fatalf("零成员交换机的成员列表应为空，得到 %+v", got[1].Ports)
	}
}
