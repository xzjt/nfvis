package netkernel

// 决策 #426②③：内核数据面下「口仍在用吗」的探测（解绑前守卫的判据），以及恢复收敛对
// 「声明了口但内核里没有它、仍被 DPDK 驱动占用」这类未收敛项的如实点名。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// 判据：master（bridge/bond/VRF 成员）与自身地址；都没有即「不在用」；
// 口在内核里不存在时如实返回错误（调用方按「探测不到不拦」处理——这正是 DPDK 残留的形态）。
func TestKernelIfaceInUse(t *testing.T) {
	f := &fakeRunner{replies: []fakeReply{
		{prefix: "ip -j link show dev vs-port", out: `[{"ifname":"vs-port","master":"vs-lan"}]`},
		{prefix: "ip -j link show dev vs-l3port", out: `[{"ifname":"vs-l3port","master":"vr-wan"}]`},
		{prefix: "ip -j link show dev ens192", out: `[{"ifname":"ens192"}]`},
		{prefix: "ip -j addr show dev ens192",
			out: `[{"ifname":"ens192","addr_info":[{"family":"inet","local":"192.168.99.1"},{"family":"inet6","local":"2001:db8::1"}]}]`},
		{prefix: "ip -j link show dev ens224", out: `[{"ifname":"ens224"}]`},
		{prefix: "ip -j addr show dev ens224", out: `[{"ifname":"ens224","addr_info":[]}]`},
		{prefix: "ip -j link show dev gone0",
			out: `Device "gone0" does not exist.`, err: errors.New("exit status 1")},
	}}
	p := New(f)

	for _, tc := range []struct {
		name    string
		inUse   bool
		wantWhy string
	}{
		{"vs-port", true, "vs-lan 的成员口"},
		{"vs-l3port", true, "vr-wan 的成员口"},
		{"ens192", true, "带 IP 地址（2 个）"},
		{"ens224", false, ""},
	} {
		inUse, why, err := p.KernelIfaceInUse(context.Background(), tc.name)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if inUse != tc.inUse {
			t.Fatalf("%s: inUse = %v，期望 %v", tc.name, inUse, tc.inUse)
		}
		if tc.wantWhy != "" && !strings.Contains(why, tc.wantWhy) {
			t.Fatalf("%s: 原因应含 %q，得到 %q", tc.name, tc.wantWhy, why)
		}
		if tc.wantWhy == "" && why != "" {
			t.Fatalf("%s: 不在用不应给原因：%q", tc.name, why)
		}
	}
	// 口不在内核里（DPDK 已接管）→ 探测返回错误，由守卫按「不拦」处置。
	if _, _, err := p.KernelIfaceInUse(context.Background(), "gone0"); err == nil {
		t.Fatal("口不存在时应如实返回错误（不是「不在用」）")
	}
	// 空名不猜。
	if _, _, err := p.KernelIfaceInUse(context.Background(), "  "); err == nil {
		t.Fatal("空名应报错")
	}
}

// 恢复收敛的未收敛项点名：声明了口、内核里没有它、仍绑在 vfio-pci 上 → 错误与告警都要
// 说出真正的持有者与交还命令（沿用既有 IFACE_MISSING 码，不新造告警码）。
func TestEnsureConsistentNamesDPDKHeldPort(t *testing.T) {
	newBadRunner := func() *fakeRunner {
		return &fakeRunner{replies: []fakeReply{
			{prefix: "sysctl -n", out: "1\n"},
			{prefix: "ip link set dev ens224 up",
				out: `Cannot find device "ens224"`, err: errors.New("exit status 1")},
		}}
	}
	cfg := model.Config{Interfaces: []model.InterfaceConfig{{Name: "ens224"}}}

	// 正控：装了点名探测 → 文案含驱动名与照做路径。
	p := New(newBadRunner())
	store := network.NewAlarmStore()
	p.SetAlarms(store)
	p.SetHeldPortProbe(func(ifname string) (string, string, bool) {
		if ifname != "ens224" {
			return "", "", false
		}
		return "vfio-pci", "0000:13:00.0", true
	})
	errs := p.EnsureConsistent(context.Background(), cfg)
	if len(errs) != 1 {
		t.Fatalf("应有一条未收敛项：%v", errs)
	}
	msg := errs[0].Error()
	for _, want := range []string{"interfaces/ens224", "vfio-pci", "0000:13:00.0", "unbind-dpdk --yes"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误文案应含 %q（要能照着做）：%s", want, msg)
		}
	}
	active := store.List("active")
	if len(active) != 1 {
		t.Fatalf("应落一条告警：%+v", active)
	}
	if active[0].Code != network.AlarmIfaceMissing || active[0].Severity != network.SeverityError {
		t.Fatalf("码/级别必须沿用既有口径（%s/error）：%+v", network.AlarmIfaceMissing, active[0])
	}
	if !strings.Contains(active[0].Message, "vfio-pci") || active[0].Source != "interfaces/ens224" {
		t.Fatalf("告警也要点名持有者：%+v", active[0])
	}

	// 对照组：没有探测（未注入 / 取不到）→ 维持底座原文，不猜驱动。
	p2 := New(newBadRunner())
	store2 := network.NewAlarmStore()
	p2.SetAlarms(store2)
	if errs := p2.EnsureConsistent(context.Background(), cfg); len(errs) != 1 {
		t.Fatalf("仍应有一条未收敛项：%v", errs)
	} else if strings.Contains(errs[0].Error(), "vfio-pci") {
		t.Fatalf("未注入探测不得凭空点名：%v", errs[0])
	}
	if got := store2.List("active"); len(got) != 1 || got[0].Code != network.AlarmIfaceMissing {
		t.Fatalf("对照组告警口径不变：%+v", got)
	}

	// 只对「设备不存在」类错误附加：其它失败原因原样上报（不把驱动残留的结论硬套上去）。
	p3 := New(&fakeRunner{replies: []fakeReply{
		{prefix: "sysctl -n", out: "1\n"},
		{prefix: "ip link set dev ens224 up", out: "RTNETLINK answers: Operation not permitted",
			err: errors.New("exit status 2")},
	}})
	p3.SetHeldPortProbe(func(string) (string, string, bool) { return "vfio-pci", "0000:13:00.0", true })
	errs3 := p3.EnsureConsistent(context.Background(), cfg)
	if len(errs3) != 1 || strings.Contains(errs3[0].Error(), "vfio-pci") {
		t.Fatalf("非「设备不存在」错误不应附加点名：%v", errs3)
	}
}
