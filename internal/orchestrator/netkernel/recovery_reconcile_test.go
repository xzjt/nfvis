package netkernel

// 决策 #443：内核数据面下恢复收敛告警的按来源廉价复核（收口 R7-1）——内核侧用例。
// 用例对应决策 #443 的测试清单：① interfaces/<n> IFACE_MISSING 三态；② 存在性读失败
// ⇒ 保留并上抛；③ 二段来源（acls/qos/bonds/vrfs/port-mirroring/vxlan）已删 ⇒ 消解、
// 仍在 ⇒ 保留；④ virtual-switches/<sw>/<leaf> 三分支（交换机删/叶删/叶在）；⑤
// container-functions/<o>/interfaces/<i>（容器删 ⇒ 消解、声明在 ⇒ 保留、畸形 ⇒ 保留）；
// ⑥ family 级与不可解析 ⇒ 保留；⑦ 非本族码不触碰。

import (
	"context"
	"errors"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// raiseKernelRecovery 在 recovery 作用域放一条活动告警（模拟内核恢复重放曾失败落下的告警）。
func raiseKernelRecovery(store *network.AlarmStore, code, source string) {
	store.Raise(alarmScopeRecovery, network.SeverityWarning, code, "测试告警:"+source, source)
}

// kernelRecoveryActive 返回当前活动的恢复收敛族告警（code/source）集合，不含其它码。
func kernelRecoveryActive(store *network.AlarmStore) map[string]bool {
	out := map[string]bool{}
	for _, a := range store.ActiveOf(alarmScopeRecovery) {
		if a.Code == network.AlarmUnconverged || a.Code == network.AlarmIfaceMissing {
			out[a.Code+"/"+a.Source] = true
		}
	}
	return out
}

// ①a 口已出现在内核（`ip -d -j link show` 事实源里有它）→ 消解。
func TestKernelRecoveryIfaceMissingNowPresent(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -d -j link show",
		out:    `[{"ifname":"ens192","flags":["UP","LOWER_UP"],"operstate":"up","mtu":1500}]`,
	}}}
	p := New(f)
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	raiseKernelRecovery(store, network.AlarmIfaceMissing, "interfaces/ens192")
	cfg := model.Config{Interfaces: []model.InterfaceConfig{{Name: "ens192"}}}

	errs := p.ReconcileRecoveryAlarms(context.Background(), cfg)
	if len(errs) != 0 {
		t.Fatalf("查询成功不应上抛错误: %v", errs)
	}
	if got := kernelRecoveryActive(store); len(got) != 0 {
		t.Fatalf("口已在内核，告警应消解: %v", got)
	}
}

// ①b 配置仍声明、但内核里仍没有该口 → 保留（不猜测）。
func TestKernelRecoveryIfaceMissingStillAbsent(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -d -j link show",
		out:    `[{"ifname":"ens224","flags":["UP","LOWER_UP"],"operstate":"up","mtu":1500}]`,
	}}}
	p := New(f)
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	raiseKernelRecovery(store, network.AlarmIfaceMissing, "interfaces/ens192")
	cfg := model.Config{Interfaces: []model.InterfaceConfig{{Name: "ens192"}}}

	errs := p.ReconcileRecoveryAlarms(context.Background(), cfg)
	if len(errs) != 0 {
		t.Fatalf("查询成功时不应上抛错误: %v", errs)
	}
	if got := kernelRecoveryActive(store); len(got) != 1 {
		t.Fatalf("口仍缺，告警应保留: %v", got)
	}
}

// ①c 声明已删（来源对象已不在 committed 配置）→ 消解；且不做存在性查询（廉价）。
func TestKernelRecoveryIfaceSourceDeleted(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	raiseKernelRecovery(store, network.AlarmIfaceMissing, "interfaces/ens192")

	errs := p.ReconcileRecoveryAlarms(context.Background(), model.Config{})
	if len(errs) != 0 {
		t.Fatalf("不应有查询错误: %v", errs)
	}
	if got := kernelRecoveryActive(store); len(got) != 0 {
		t.Fatalf("来源已删的告警应消解: %v", got)
	}
	if len(f.calls) != 0 {
		t.Fatalf("来源已删不应触发存在性查询；实际：\n%s", f.joined())
	}
}

// ② 存在性查询失败 → 保守保留并计入返回错误（问不出来 ≠ 已复原）。
func TestKernelRecoveryKeepsOnIfaceQueryFailure(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{{
		prefix: "ip -d -j link show",
		err:    errors.New("ip: 内核接口清单读取失败（注入）"),
	}}}
	p := New(f)
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	raiseKernelRecovery(store, network.AlarmIfaceMissing, "interfaces/ens192")
	cfg := model.Config{Interfaces: []model.InterfaceConfig{{Name: "ens192"}}}

	errs := p.ReconcileRecoveryAlarms(context.Background(), cfg)
	if len(errs) != 1 {
		t.Fatalf("查询错误必须上抛: %v", errs)
	}
	if got := kernelRecoveryActive(store); len(got) != 1 {
		t.Fatalf("查询失败应保守留警: %v", got)
	}
}

// ③ 二段来源：已删 ⇒ 消解；仍在声明集 ⇒ 保留（UNCONVERGED 无法廉价证实「现在能 apply
// 成功」，不猜——决策 #333 的核心保守口径）。interfaces/<n> 的 UNCONVERGED 同样不查询。
func TestKernelRecoveryTwoSegmentSources(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	raiseKernelRecovery(store, network.AlarmUnconverged, "acls/gone")
	for _, src := range []string{
		"qos/kept", "bonds/kept", "vrfs/kept", "port-mirroring/kept",
		"vxlan/kept", "interfaces/kept-if",
	} {
		raiseKernelRecovery(store, network.AlarmUnconverged, src)
	}
	cfg := model.Config{
		QosPolicies:   []model.QosPolicy{{Name: "kept"}},
		Bonds:         []model.Bond{{Name: "kept"}},
		Vrfs:          []model.Vrf{{Name: "kept"}},
		PortMirroring: []model.PortMirroring{{Name: "kept"}},
		VxlanTunnels:  []model.VxlanTunnel{{Name: "kept"}},
		Interfaces:    []model.InterfaceConfig{{Name: "kept-if"}},
	}

	errs := p.ReconcileRecoveryAlarms(context.Background(), cfg)
	if len(errs) != 0 {
		t.Fatalf("不应有查询错误: %v", errs)
	}
	got := kernelRecoveryActive(store)
	if len(got) != 6 {
		t.Fatalf("已删的一条应消解、仍声明的六条应保留: %v", got)
	}
	for _, want := range []string{
		network.AlarmUnconverged + "/qos/kept",
		network.AlarmUnconverged + "/bonds/kept",
		network.AlarmUnconverged + "/vrfs/kept",
		network.AlarmUnconverged + "/port-mirroring/kept",
		network.AlarmUnconverged + "/vxlan/kept",
		network.AlarmUnconverged + "/interfaces/kept-if",
	} {
		if !got[want] {
			t.Fatalf("仍声明应保留 %s: %v", want, got)
		}
	}
}

// ④ virtual-switches/<sw>/<leaf> 三分支：交换机已删 ⇒ 消解（两类叶）；交换机在但叶已删
// ⇒ 消解；交换机在且叶仍声明 ⇒ 保留；未知 leaf / 超过三段 ⇒ 保留。
func TestKernelRecoveryVirtualSwitchLeafSources(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	raiseKernelRecovery(store, network.AlarmUnconverged, "virtual-switches/vs-gone/dhcp-relay")
	raiseKernelRecovery(store, network.AlarmUnconverged, "virtual-switches/vs-gone/dhcp-server")
	raiseKernelRecovery(store, network.AlarmUnconverged, "virtual-switches/vs-a/learn-limit")
	raiseKernelRecovery(store, network.AlarmUnconverged, "virtual-switches/vs-a/dhcp-relay")
	raiseKernelRecovery(store, network.AlarmUnconverged, "virtual-switches/vs-a/dhcp-server")
	raiseKernelRecovery(store, network.AlarmUnconverged, "virtual-switches/vs-a/some-future")
	raiseKernelRecovery(store, network.AlarmUnconverged, "virtual-switches/vs-a/learn-limit/extra")
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{{
		Name: "vs-a", Type: "l2",
		DhcpRelayServer:     "192.168.99.10",
		DhcpServerPoolStart: "192.168.99.100",
		DhcpServerPoolEnd:   "192.168.99.110",
	}}}

	errs := p.ReconcileRecoveryAlarms(context.Background(), cfg)
	if len(errs) != 0 {
		t.Fatalf("不应有查询错误: %v", errs)
	}
	got := kernelRecoveryActive(store)
	if len(got) != 4 {
		t.Fatalf("交换机删/叶删的三条应消解、其余四条应保留: %v", got)
	}
	for _, want := range []string{
		network.AlarmUnconverged + "/virtual-switches/vs-a/dhcp-relay",
		network.AlarmUnconverged + "/virtual-switches/vs-a/dhcp-server",
		network.AlarmUnconverged + "/virtual-switches/vs-a/some-future",
		network.AlarmUnconverged + "/virtual-switches/vs-a/learn-limit/extra",
	} {
		if !got[want] {
			t.Fatalf("应保留 %s: %v", want, got)
		}
	}
}

// ⑤ container-functions/<owner>/interfaces/<iface>：声明仍在 ⇒ 保留；容器/接口已删 ⇒
// 消解；畸形（无 /interfaces/、owner 空）⇒ 保守保留。
func TestKernelRecoveryContainerVethSources(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	raiseKernelRecovery(store, network.AlarmUnconverged, "container-functions/ct-a/interfaces/eth0")
	raiseKernelRecovery(store, network.AlarmUnconverged, "container-functions/ct-b/interfaces/eth0")
	raiseKernelRecovery(store, network.AlarmUnconverged, "container-functions/ct-a")
	raiseKernelRecovery(store, network.AlarmUnconverged, "container-functions//interfaces/eth0")
	cfg := model.Config{ContainerFunctions: []model.ContainerFunction{{
		Name:       "ct-a",
		Interfaces: []model.VnfInterface{{Name: "eth0", Type: "memif", VirtualSwitch: "vs-a"}},
	}}}

	errs := p.ReconcileRecoveryAlarms(context.Background(), cfg)
	if len(errs) != 0 {
		t.Fatalf("不应有查询错误: %v", errs)
	}
	got := kernelRecoveryActive(store)
	if len(got) != 3 {
		t.Fatalf("容器已删的一条应消解、其余三条应保留: %v", got)
	}
	for _, want := range []string{
		network.AlarmUnconverged + "/container-functions/ct-a/interfaces/eth0",
		network.AlarmUnconverged + "/container-functions/ct-a",
		network.AlarmUnconverged + "/container-functions//interfaces/eth0",
	} {
		if !got[want] {
			t.Fatalf("应保留 %s: %v", want, got)
		}
	}
}

// ⑥ family 级来源（forwarding/lldp/dns-proxy/nat/virtual-switches）→ 保守保留，
// 即使配置为空也不消解。
func TestKernelRecoveryKeepsFamilySources(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	for _, src := range []string{"forwarding", "lldp", "dns-proxy", "nat", "virtual-switches"} {
		raiseKernelRecovery(store, network.AlarmUnconverged, src)
	}

	errs := p.ReconcileRecoveryAlarms(context.Background(), model.Config{})
	if len(errs) != 0 {
		t.Fatalf("不应有查询错误: %v", errs)
	}
	if got := kernelRecoveryActive(store); len(got) != 5 {
		t.Fatalf("family 级来源应全部保守保留: %v", got)
	}
}

// ⑦ 非本族码不触碰：recovery 作用域里的残渣码（有独立对账路径）在复核后仍活动；
// 未注入告警表时为空操作。
func TestKernelRecoveryDoesNotTouchOtherCodes(t *testing.T) {
	f := &fakeRunner{}
	p := New(f)
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	store.Raise(alarmScopeRecovery, network.SeverityWarning, network.AlarmACLLeftover, "残渣", "acl/stray")

	if errs := p.ReconcileRecoveryAlarms(context.Background(), model.Config{}); len(errs) != 0 {
		t.Fatalf("不应有查询错误: %v", errs)
	}
	found := false
	for _, a := range store.List("active") {
		if a.Code == network.AlarmACLLeftover && a.Source == "acl/stray" {
			found = true
		}
	}
	if !found {
		t.Fatal("残渣码不得被恢复复核触碰")
	}

	// 未注入告警表（装配前/测试场景）⇒ 空操作，不触碰 Runner。
	bare := New(&fakeRunner{})
	if errs := bare.ReconcileRecoveryAlarms(context.Background(), model.Config{}); len(errs) != 0 {
		t.Fatalf("未注入告警表应空操作: %v", errs)
	}
}
