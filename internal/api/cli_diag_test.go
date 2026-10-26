package api

// M3-9：CLI 操作命令（ping/traceroute/monitor/clear）执行器单测。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/schema"
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
	pingIPv6 bool

	trOut  string
	trErr  error
	trVRF  string
	trIPv6 bool

	cleared  []string
	clearErr error
}

func (fakeCounters) RuntimeStats(context.Context) (state.RuntimeStats, bool) {
	return state.RuntimeStats{}, false
}

func (f *fakeDiagRT) Ping(_ context.Context, host, source, vrf string, count int, ipv6 bool) (string, error) {
	f.pingHost, f.pingSrc, f.pingVRF, f.pingCnt, f.pingIPv6 = host, source, vrf, count, ipv6
	return f.pingOut, f.pingErr
}
func (f *fakeDiagRT) Traceroute(_ context.Context, host, vrf string, ipv6 bool) (string, error) {
	f.trVRF, f.trIPv6 = vrf, ipv6
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

// TestCLIPingIPv6 决策 #330：`ping ipv6 <host>` 与 `ping <host> ipv6` 两种写法
// 都在命令树上成立、执行器同源；族选择透传给运行时。`ping ipv6`（缺目标）报语法。
func TestCLIPingIPv6(t *testing.T) {
	x, _ := newCLIKit(t)
	d := &fakeDiagRT{pingOut: "Statistics: 2 sent, 2 received, 0% packet loss\n"}
	x.setRuntime(d, nil)

	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "ping ipv6 2001:db8::1 count 2").Output; !strings.Contains(out, "2 received") {
		t.Fatalf("ping ipv6 应执行: %q", out)
	}
	if d.pingHost != "2001:db8::1" || !d.pingIPv6 || d.pingCnt != 2 {
		t.Fatalf("ping ipv6 参数: %+v", d)
	}
	// 族选择器写在目标之后（命令树同形）：同样识别为 v6
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "ping 2001:db8::1 ipv6").Output; !strings.Contains(out, "2 received") {
		t.Fatalf("ping <host> ipv6 应执行: %q", out)
	}
	if !d.pingIPv6 {
		t.Fatalf("目标之后的 ipv6 也应识别: %+v", d)
	}
	// 缺目标
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "ping ipv6").Output; !strings.Contains(out, "语法") {
		t.Fatalf("ping ipv6 缺目标应报语法: %q", out)
	}
	// 不显式给族：IPv4 语义（ipv6=false）
	d.pingIPv6 = true
	x.Execute("admin", aaa.ClassSuperUser, "ssh", "ping 10.0.0.1")
	if d.pingIPv6 {
		t.Fatalf("未给 ipv6 不应置位: %+v", d)
	}
}

// TestCLITracerouteIPv6 决策 #330：`traceroute ipv6 <host>` 透传族；vrf 报不支持（不降级）。
func TestCLITracerouteIPv6(t *testing.T) {
	x, _ := newCLIKit(t)
	d := &fakeDiagRT{trOut: "traceroute to 2001:db8::1, 30 hops max, 宿主侧 ICMPv6\n 1  2001:db8::1  0.5 ms\n"}
	x.setRuntime(d, nil)

	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "traceroute ipv6 2001:db8::1").Output
	if !strings.Contains(out, "ICMPv6") || !d.trIPv6 {
		t.Fatalf("traceroute ipv6 应透传 v6: out=%q %+v", out, d)
	}
	// vrf 明确报不支持（底座返回，执行器原样回显）
	x.setRuntime(&fakeDiagRT{trErr: errors.New("traceroute 不支持 vrf \"vs-a\"")}, nil)
	out = x.Execute("admin", aaa.ClassSuperUser, "ssh", "traceroute ipv6 2001:db8::1 vrf vs-a").Output
	if !strings.Contains(out, "不支持 vrf") {
		t.Fatalf("v6 vrf 应报不支持: %q", out)
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
		RxPackets: 123, TxPackets: 456, RxBytes: 789, TxBytes: 1011, Drops: 77,
	}}))
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "monitor interfaces ens192 interval 2").Output
	// 列名与 `show interfaces <n> statistics` **同一份定义**（ifaceDropsCols）：VPP 口径只有
	// 一列 `drops`，值取 stats 的方向无关总丢弃（决策 #504）。
	for _, want := range []string{"rx-pkts", "drops", "ens192", "123", "456", "789", "1011", "77"} {
		if !strings.Contains(out, want) {
			t.Fatalf("快照缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "rx-drops") || strings.Contains(out, "tx-drops") {
		t.Fatalf("VPP 数据面下 monitor 不得再有方向丢弃列（该口径不适用，发了就是编造方向）：\n%s", out)
	}
	// 接口统计取不到：文案须如实说「这一刻连接没就绪 / 口不在数据面」，
	// 不得说成「stats 未接入」（stats 是接入了的；与 `show interfaces <n> statistics` 同口径）
	x.setRuntime(nil, state.New(fakeCounters{ok: false}))
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "monitor interfaces ensX").Output; !strings.Contains(out, "统计暂不可用（不存在或 stats 连接未就绪）") {
		t.Fatalf("应提示统计暂不可用（stats 连接未就绪）: %q", out)
	}
	// 语法错误
	if out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "monitor interfaces").Output; !strings.Contains(out, "语法") {
		t.Fatalf("缺接口应报语法: %q", out)
	}
}

// 内核网络数据面下 monitor 的丢弃列**按方向**（决策 #504）：sysfs 有 rx_dropped 与
// tx_dropped 两个真值，故两列而非一列，且不得出现方向无关的 `drops` 总数列。
func TestCLIMonitorKernelDirectionalDrops(t *testing.T) {
	x := kernelDPKit(t)
	x.setRuntime(nil, state.NewCounters(fakeKernelCounters{ok: true, c: state.InterfaceCounters{
		RxPackets: 10, TxPackets: 20, RxBytes: 30, TxBytes: 40,
		RxErrors: 1, TxErrors: 2, RxDrops: 5, TxDrops: 6, DirectionalDrops: true,
	}}))
	out := x.Execute("admin", aaa.ClassSuperUser, "ssh", "monitor interfaces ens192").Output
	var head, row string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		if f[0] == "Interface" {
			head = strings.Join(f, " ")
		}
		if f[0] == "ens192" {
			row = strings.Join(f, " ")
		}
	}
	wantHead := "Interface rx-pkts tx-pkts rx-bytes tx-bytes rx-errors tx-errors rx-drops tx-drops"
	if head != wantHead {
		t.Fatalf("内核数据面表头应为 %q，实得 %q\n%s", wantHead, head, out)
	}
	if want := "ens192 10 20 30 40 1 2 5 6"; row != want {
		t.Fatalf("内核数据面数据行应为 %q，实得 %q\n%s", want, row, out)
	}
}

// fakeKernelCounters 只实现 state.Counters（内核数据面的窄接口：只有接口计数）。
type fakeKernelCounters struct {
	c  state.InterfaceCounters
	ok bool
}

func (f fakeKernelCounters) InterfaceCounters(context.Context, string) (state.InterfaceCounters, bool) {
	return f.c, f.ok
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

// TestCLIDiagIPv6Candidates 决策 #330 真机验证遗留（round110 登记的「候选正确探针」缺口）：
// round110 曾用「`ping ipv6 127.0.0.1` 执行是否报错」当候选探针——那是**执行**路径，不是候选查询。
// 正确探针是直接断言候选树：`?`/Tab 在 tokens ["ping"] 与 ["traceroute"] 位置都应列出
// 名为 `ipv6` 的候选（决策 #330 把 ipv6 建成可选无值叶子关键字，与 <host> 平级）。
func TestCLIDiagIPv6Candidates(t *testing.T) {
	for _, tokens := range [][]string{{"ping"}, {"traceroute"}} {
		cs := schema.CandidatesFiltered(schema.OperRoot(), tokens, "", nil, nil)
		toks := make([]string, 0, len(cs))
		for _, c := range cs {
			toks = append(toks, c.Token)
		}
		if !hasTok(toks, "ipv6") {
			t.Errorf("`%s ?` 候选应含 ipv6（可选族选择器）：%v", strings.Join(tokens, " "), toks)
		}
	}
}
