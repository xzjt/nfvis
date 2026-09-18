package network

// M3-9：诊断操作（ping/traceroute/clear stats）单测（假 DiagClient/VPPShell/Prober）。

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeDiag struct {
	ifaces  map[string]uint32
	names   map[uint32]string
	addrs4  []IfaceAddr
	addrs6  []IfaceAddr
	cleared []uint32
	err     error
}

func newFakeDiag() *fakeDiag {
	return &fakeDiag{
		ifaces: map[string]uint32{"ens192": 1, "ens224": 2},
		names:  map[uint32]string{1: "ens192", 2: "ens224"},
		addrs4: []IfaceAddr{{SwIfIndex: 1, Prefix: "192.168.155.200/24"}},
	}
}

func (f *fakeDiag) Close() {}
func (f *fakeDiag) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	idx, ok := f.ifaces[ifname]
	return idx, ok, nil
}
func (f *fakeDiag) SwInterfaceNames() (map[uint32]string, error) { return f.names, nil }
func (f *fakeDiag) InterfaceAddresses(isIPv6 bool) ([]IfaceAddr, error) {
	if f.err != nil {
		return nil, f.err
	}
	if isIPv6 {
		return f.addrs6, nil
	}
	return f.addrs4, nil
}
func (f *fakeDiag) ClearInterfaceStats(idx uint32) error {
	if f.err != nil {
		return f.err
	}
	f.cleared = append(f.cleared, idx)
	return nil
}

type fakeShell struct {
	calls [][]string
	out   string
	err   error
}

func (f *fakeShell) Run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	return f.out, f.err
}

type fakeProber struct {
	hops   []Hop
	err    error
	maxTTL int
}

func (f *fakeProber) Trace(_ context.Context, _ string, maxTTL int, _ time.Duration) ([]Hop, error) {
	f.maxTTL = maxTTL
	return f.hops, f.err
}

func diagWith(c DiagClient, sh VPPShell, pr TracerouteProber) *Diagnostics {
	return NewDiagnostics(func() (DiagClient, error) { return c, nil }, sh, pr)
}

func TestPingArgMapping(t *testing.T) {
	sh := &fakeShell{out: "Statistics: 3 sent, 3 received, 0% packet loss\n"}
	d := diagWith(newFakeDiag(), sh, nil)

	if _, err := d.Ping(context.Background(), PingRequest{Host: "10.0.0.9", Count: 3, VRF: "vs-a"}); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	got := strings.Join(sh.calls[0], " ")
	want := "ping 10.0.0.9 repeat 3 table-id " + strconv.FormatUint(uint64(TableID("vs-a")), 10)
	if got != want {
		t.Fatalf("ping 参数映射:\n got=%q\nwant=%q", got, want)
	}

	// source <ip> → 按 VPP 接口地址反查接口名
	if _, err := d.Ping(context.Background(), PingRequest{Host: "10.0.0.9", Source: "192.168.155.200"}); err != nil {
		t.Fatalf("Ping(source): %v", err)
	}
	if got := strings.Join(sh.calls[1], " "); got != "ping 10.0.0.9 source ens192" {
		t.Fatalf("source 反查: %q", got)
	}
}

func TestPingErrors(t *testing.T) {
	if _, err := diagWith(newFakeDiag(), &fakeShell{}, nil).Ping(context.Background(), PingRequest{}); err == nil {
		t.Fatal("空目标应报错")
	}
	if _, err := diagWith(newFakeDiag(), nil, nil).Ping(context.Background(), PingRequest{Host: "10.0.0.1"}); err == nil {
		t.Fatal("未装配 shell 应报错")
	}
	// 源地址不在任何接口 → 明确报错，不落到 vppctl
	sh := &fakeShell{}
	_, err := diagWith(newFakeDiag(), sh, nil).Ping(context.Background(), PingRequest{Host: "10.0.0.1", Source: "10.9.9.9"})
	if err == nil || !strings.Contains(err.Error(), "源地址") {
		t.Fatalf("未命中源地址应报错: %v", err)
	}
	if len(sh.calls) != 0 {
		t.Fatal("源地址解析失败不应调用 vppctl")
	}
	// vppctl 失败时保留已有输出
	sh2 := &fakeShell{out: "Failed: no egress interface\n", err: errors.New("exit 1")}
	out, err := diagWith(newFakeDiag(), sh2, nil).Ping(context.Background(), PingRequest{Host: "10.0.0.1"})
	if err == nil || !strings.Contains(out, "no egress interface") {
		t.Fatalf("应保留 vppctl 输出并报错: out=%q err=%v", out, err)
	}
}

func TestIfaceByAddrIPv6(t *testing.T) {
	f := newFakeDiag()
	f.addrs6 = []IfaceAddr{{SwIfIndex: 2, Prefix: "fd00::1/64"}}
	f.names[2] = "ens224"
	d := diagWith(f, &fakeShell{}, nil)
	got, err := d.ifaceByAddr("fd00::1")
	if err != nil || got != "ens224" {
		t.Fatalf("IPv6 反查: got=%q err=%v", got, err)
	}
}

func TestTracerouteRejectsVRF(t *testing.T) {
	d := diagWith(newFakeDiag(), nil, &fakeProber{})
	_, err := d.Traceroute(context.Background(), TracerouteRequest{Host: "10.0.0.1", VRF: "vs-a"})
	if err == nil || !strings.Contains(err.Error(), "不支持 vrf") {
		t.Fatalf("vrf 应明确报不支持: %v", err)
	}
}

func TestTracerouteUsesProberAndFormats(t *testing.T) {
	pr := &fakeProber{hops: []Hop{
		{TTL: 1, Addr: "192.168.155.1", RTT: 1200 * time.Microsecond},
		{TTL: 2, Timeout: true},
		{TTL: 3, Addr: "10.0.0.1", RTT: 3 * time.Millisecond},
	}}
	out, err := diagWith(newFakeDiag(), nil, pr).Traceroute(context.Background(), TracerouteRequest{Host: "10.0.0.1"})
	if err != nil {
		t.Fatalf("Traceroute: %v", err)
	}
	for _, want := range []string{"traceroute to 10.0.0.1", "192.168.155.1", "1.200 ms", " 2  *", "10.0.0.1", "3.000 ms"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, out)
		}
	}
	if pr.maxTTL != 30 {
		t.Fatalf("maxTTL 应为 30，实际 %d", pr.maxTTL)
	}
	if _, err := diagWith(newFakeDiag(), nil, nil).Traceroute(context.Background(), TracerouteRequest{Host: "x"}); err == nil {
		t.Fatal("未装配 prober 应报错")
	}
}

func TestClearInterfaceStats(t *testing.T) {
	f := newFakeDiag()
	d := diagWith(f, nil, nil)
	if err := d.ClearInterfaceStats(context.Background(), ""); err != nil {
		t.Fatalf("清全部: %v", err)
	}
	if len(f.cleared) != 1 || f.cleared[0] != ^uint32(0) {
		t.Fatalf("缺省应清全部(~0): %v", f.cleared)
	}
	if err := d.ClearInterfaceStats(context.Background(), "ens224"); err != nil {
		t.Fatalf("清指定: %v", err)
	}
	if f.cleared[1] != 2 {
		t.Fatalf("ens224 应为 idx 2: %v", f.cleared)
	}
	err := d.ClearInterfaceStats(context.Background(), "ens999")
	if !errors.Is(err, ErrIfaceUnavailable) {
		t.Fatalf("接口缺失应标记 ErrIfaceUnavailable: %v", err)
	}
}

// TestPingZeroSentIsFailure（附录 A #89）：VPP 一个包都没发出去时不得原样放行。
//
// 真机实测（2026-09-16）：`ping 192.168.155.2`（管理口网关，属内核平面）输出
// `Failed: no egress interface` ×5 + `Statistics: 0 sent, 0 received, 0% packet loss`，
// **返回码 0、无 `%`**——判定侧（CLI 的 %/%%、cli-fulltest.sh 的 _is_fail）只看错误行与退出码，
// 于是「一个包没发出去」被算作**通过**（冒烟脚本里那条 ping 用例一直是假绿）。
func TestPingZeroSentIsFailure(t *testing.T) {
	const noEgress = "Failed: no egress interface\nFailed: no egress interface\n\nStatistics: 0 sent, 0 received, 0% packet loss\n"

	out, err := diagWith(newFakeDiag(), &fakeShell{out: noEgress}, nil).
		Ping(context.Background(), PingRequest{Host: "192.168.155.2"})
	if err == nil {
		t.Fatal("0 发包必须报错（否则判定侧把它算作通过）")
	}
	if !strings.Contains(err.Error(), "VPP 数据面") {
		t.Errorf("报错应点明平面口径: %v", err)
	}
	// 输出要能照着做：说明平面归属 + 给出宿主 ping / traceroute 这条路
	if !strings.Contains(out, "内核平面") || !strings.Contains(out, "traceroute") || !strings.Contains(out, "192.168.155.2") {
		t.Errorf("输出应给出下一步（平面归属 + 宿主侧手段）: %q", out)
	}

	// 有应答 → 真通，不报错
	if _, err := diagWith(newFakeDiag(), &fakeShell{out: "Statistics: 2 sent, 2 received, 0% packet loss\n"}, nil).
		Ping(context.Background(), PingRequest{Host: "10.0.0.9"}); err != nil {
		t.Fatalf("有应答不应报错: %v", err)
	}
	// 有发包但一个应答都没有：**也要判失败**（附录 A #93）——ping 是连通性测试，没通就是失败；
	// 否则 `ping <不可达>` 返回 0，调用方与判定侧都会以为通了。真机实测：VPP ping 自己的
	// 回环地址也是 `2 sent, 0 received`。
	uo, uerr := diagWith(newFakeDiag(), &fakeShell{out: "Statistics: 2 sent, 0 received, 100% packet loss\n"}, nil).
		Ping(context.Background(), PingRequest{Host: "10.0.0.9"})
	if uerr == nil {
		t.Fatal("发出但无应答必须报错")
	}
	if !strings.Contains(uerr.Error(), "无应答") {
		t.Errorf("应说明是「发了没回」而非「没发出去」: %v", uerr)
	}
	if !strings.Contains(uo, "已从 VPP 发出") || !strings.Contains(uo, "10.0.0.9") {
		t.Errorf("输出应给出下一步（对端在线/放行 ICMP/路由）: %q", uo)
	}
	// **判不出就不判**：输出格式变化（无汇总行）时不得制造假红
	if _, err := diagWith(newFakeDiag(), &fakeShell{out: "some other output\n"}, nil).
		Ping(context.Background(), PingRequest{Host: "10.0.0.9"}); err != nil {
		t.Fatalf("判不出时不得误报: %v", err)
	}
}
