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
