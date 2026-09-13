package api

// M3-9：CLI 操作命令（ping/traceroute/monitor/clear）执行器单测。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/state"
)

type fakeCounters struct {
	c  state.InterfaceCounters
	ok bool
}

func (f fakeCounters) Threads(context.Context) ([]state.Thread, error) { return nil, nil }
func (f fakeCounters) InterfaceCounters(context.Context, string) (state.InterfaceCounters, bool) {
	return f.c, f.ok
}
func (fakeCounters) Buffers(context.Context) (state.Buffers, bool) { return state.Buffers{}, false }
func (fakeCounters) Memory(context.Context) (state.Memory, bool)   { return state.Memory{}, false }

type fakeDiagRT struct {
	pingOut  string
	pingErr  error
	pingHost string
	pingCnt  int
	pingVRF  string
	pingSrc  string

	trOut string
	trErr error
	trVRF string

	cleared  []string
	clearErr error
}

func (f *fakeDiagRT) Ping(_ context.Context, host, source, vrf string, count int) (string, error) {
	f.pingHost, f.pingSrc, f.pingVRF, f.pingCnt = host, source, vrf, count
	return f.pingOut, f.pingErr
}
func (f *fakeDiagRT) Traceroute(_ context.Context, host, vrf string) (string, error) {
	f.trVRF = vrf
	return f.trOut, f.trErr
}
func (f *fakeDiagRT) ClearInterfaceStats(_ context.Context, ifname string) error {
	f.cleared = append(f.cleared, ifname)
	return f.clearErr
}

func TestCLIPingExecution(t *testing.T) {
	x, _ := newCLIKit(t)
	d := &fakeDiagRT{pingOut: "Statistics: 3 sent, 3 received, 0% packet loss\n"}
	x.setRuntime(d, nil)

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "ping 10.0.0.1 source 192.168.1.1 count 3 vrf vs-a").Output
	if !strings.Contains(out, "3 sent") {
		t.Fatalf("应输出 ping 结果: %q", out)
	}
	if d.pingHost != "10.0.0.1" || d.pingSrc != "192.168.1.1" || d.pingVRF != "vs-a" || d.pingCnt != 3 {
		t.Fatalf("参数解析: %+v", d)
	}
}

func TestCLIPingValidationAndPermission(t *testing.T) {
	x, _ := newCLIKit(t)
	d := &fakeDiagRT{}
	x.setRuntime(d, nil)

	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "ping 10.0.0.1 count 0").Output; !strings.Contains(out, "count 必须为正整数") {
		t.Fatalf("count=0 应报错: %q", out)
	}
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "ping").Output; !strings.Contains(out, "语法") {
		t.Fatalf("缺目标应报语法: %q", out)
	}
	if out := x.Execute("admin", aaa.ClassReadOnly, "ssh", "ping 10.0.0.1").Output; !strings.Contains(out, "无权限") {
		t.Fatalf("read-only 应无权限: %q", out)
	}
	// 未装配诊断 → 明确报不可用
	x.setRuntime(nil, nil)
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "ping 10.0.0.1").Output; !strings.Contains(out, "不可用") {
		t.Fatalf("未装配应报不可用: %q", out)
	}
}

func TestCLIPingPartialOutputOnError(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setRuntime(&fakeDiagRT{pingOut: "Failed: no egress interface\n", pingErr: errors.New("exit 1")}, nil)
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "ping 10.0.0.1").Output
	if !strings.Contains(out, "no egress interface") || !strings.Contains(out, "%%") {
		t.Fatalf("应保留部分输出并附错误: %q", out)
	}
}

func TestCLITracerouteExecution(t *testing.T) {
	x, _ := newCLIKit(t)
	d := &fakeDiagRT{trOut: "traceroute to 10.0.0.1, 30 hops max\n 1  192.168.155.1  1.2 ms\n"}
	x.setRuntime(d, nil)

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "traceroute 10.0.0.1").Output
	if !strings.Contains(out, "192.168.155.1") {
		t.Fatalf("应输出 traceroute 结果: %q", out)
	}
	// vrf → 底座明确报不支持，执行器原样回显
	x.setRuntime(&fakeDiagRT{trErr: errors.New("traceroute 不支持 vrf \"vs-a\"")}, nil)
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "traceroute 10.0.0.1 vrf vs-a").Output
	if !strings.Contains(out, "不支持 vrf") {
		t.Fatalf("vrf 应报不支持: %q", out)
	}
}

func TestCLIMonitorSnapshot(t *testing.T) {
	x, _ := newCLIKit(t)
	x.setRuntime(nil, state.New(fakeCounters{ok: true, c: state.InterfaceCounters{
		RxPackets: 123, TxPackets: 456, RxBytes: 789, TxBytes: 1011, RxDrops: 2, TxDrops: 3,
	}}))
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "monitor interfaces ens192 interval 2").Output
	for _, want := range []string{"rx-pkts", "ens192", "123", "456", "789", "1011"} {
		if !strings.Contains(out, want) {
			t.Fatalf("快照缺少 %q:\n%s", want, out)
		}
	}
	// 接口统计不可用
	x.setRuntime(nil, state.New(fakeCounters{ok: false}))
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "monitor interfaces ensX").Output; !strings.Contains(out, "统计不可用") {
		t.Fatalf("应提示统计不可用: %q", out)
	}
	// 语法错误
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "monitor interfaces").Output; !strings.Contains(out, "语法") {
		t.Fatalf("缺接口应报语法: %q", out)
	}
}

func TestCLIClearInterfaceStats(t *testing.T) {
	x, _ := newCLIKit(t)
	d := &fakeDiagRT{}
	x.setRuntime(d, nil)

	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "clear interfaces statistics").Output; !strings.Contains(out, "全部接口") {
		t.Fatalf("清全部输出: %q", out)
	}
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "clear interfaces statistics ens192").Output; !strings.Contains(out, "ens192") {
		t.Fatalf("清指定输出: %q", out)
	}
	if len(d.cleared) != 2 || d.cleared[0] != "" || d.cleared[1] != "ens192" {
		t.Fatalf("clear 调用参数: %v", d.cleared)
	}
	// operator 无权限（clear 为 super-user）
	if out := x.Execute("admin", aaa.ClassOperator, "ssh", "clear interfaces statistics").Output; !strings.Contains(out, "无权限") {
		t.Fatalf("operator 应无权限: %q", out)
	}
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "clear interfaces").Output; !strings.Contains(out, "语法") {
		t.Fatalf("不完整命令应报语法: %q", out)
	}
}
