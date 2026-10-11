package netkernel

// 内核数据面诊断（diag.go）的参数守卫与有界口径单测（v3 半程体检 R2-23）。
// 风格：假 Runner 记录命令、断言错误；不触碰宿主内核（不 exec 真工具）。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
)

// diagRecRunner 记录命令并返回预设结果（不 exec）。
type diagRecRunner struct {
	calls []string
	out   string
	err   error
}

func (r *diagRecRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	return r.out, r.err
}

func (r *diagRecRunner) joined() string { return strings.Join(r.calls, "\n") }

// count 上界：越界报可读错误且**不执行**宿主命令（不静默截断）；0 仍是「未指定」的既有语义。
func TestDiagPingCountBounds(t *testing.T) {
	ctx := context.Background()
	r := &diagRecRunner{}
	d := NewDiag(r, nil)

	_, err := d.Ping(ctx, "10.0.0.1", "", "", diagCountMax+1, false)
	if err == nil {
		t.Fatalf("count 超过上限应报错，实际接受 %d", diagCountMax+1)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(diagCountMax)) || !strings.Contains(err.Error(), "不静默截断") {
		t.Fatalf("越界错误应给出上限且说明不截断，得到 %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("越界被拒时不应执行宿主命令：\n%s", r.joined())
	}

	// 边界值照常执行。
	if _, err := d.Ping(ctx, "10.0.0.1", "", "", diagCountMax, false); err != nil {
		t.Fatalf("上界值本身应放行：%v", err)
	}
	if want := fmt.Sprintf("ping -c %d 10.0.0.1", diagCountMax); !strings.Contains(r.joined(), want) {
		t.Fatalf("缺少命令 %q：\n%s", want, r.joined())
	}

	// 负值与 0：负值不合法、0 = 未指定（默认 5，既有语义不变）。
	r.calls = nil
	if _, err := d.Ping(ctx, "10.0.0.1", "", "", -1, false); err == nil {
		t.Fatal("负 count 应被拒绝")
	}
	if len(r.calls) != 0 {
		t.Fatalf("负 count 被拒时不应执行宿主命令：\n%s", r.joined())
	}
	if _, err := d.Ping(ctx, "10.0.0.1", "", "", 0, false); err != nil {
		t.Fatalf("count 缺省（0）应放行：%v", err)
	}
	if !strings.Contains(r.joined(), "ping -c 5 10.0.0.1") {
		t.Fatalf("count 缺省应为 5：\n%s", r.joined())
	}
}

// 以 `-` 开头的用户可控值会被底层 ping/traceroute 当**选项**解析（`--` 分隔不够）：
// 入口直接拒绝，且不得执行任何宿主命令。
func TestDiagRejectsOptionLikeArgs(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		call func(*Diag) (string, error)
	}{
		{"ping 目标", func(d *Diag) (string, error) { return d.Ping(ctx, "-f", "", "", 1, false) }},
		{"ping 源地址", func(d *Diag) (string, error) { return d.Ping(ctx, "10.0.0.1", "-e", "", 1, false) }},
		{"ping vrf", func(d *Diag) (string, error) { return d.Ping(ctx, "10.0.0.1", "", "-e", 1, false) }},
		{"traceroute 目标", func(d *Diag) (string, error) { return d.Traceroute(ctx, "--", "", false) }},
		{"traceroute vrf", func(d *Diag) (string, error) { return d.Traceroute(ctx, "10.0.0.1", "-i", false) }},
	}
	for _, tc := range cases {
		r := &diagRecRunner{}
		if _, err := tc.call(NewDiag(r, nil)); err == nil || !strings.Contains(err.Error(), "选项") {
			t.Fatalf("%s 以 - 开头应被拒并说明原因，得到 %v", tc.name, err)
		}
		if len(r.calls) != 0 {
			t.Fatalf("%s 被拒时不应执行宿主命令：\n%s", tc.name, r.joined())
		}
	}

	// 正控：正常值照常执行（VRF 作用域 + IPv6 参数顺序不变）。
	r := &diagRecRunner{}
	d := NewDiag(r, nil)
	if _, err := d.Ping(ctx, "2001:db8::1", "", "vs-l3", 2, true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Traceroute(ctx, "10.0.0.1", "vs-l3", false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ip vrf exec vs-l3 ping -c 2 -6 2001:db8::1",
		"ip vrf exec vs-l3 traceroute 10.0.0.1",
	} {
		if !strings.Contains(r.joined(), want) {
			t.Fatalf("缺少命令 %q：\n%s", want, r.joined())
		}
	}
}

// diagBlockRunner 模拟「宿主工具挂住」：记录 Run 收到的 ctx 是否带 deadline，并阻塞到 ctx 结束。
type diagBlockRunner struct {
	hasDeadline bool
	deadline    time.Time
}

func (r *diagBlockRunner) Run(ctx context.Context, _ string, _ ...string) (string, error) {
	r.deadline, r.hasDeadline = ctx.Deadline()
	<-ctx.Done()
	return "", ctx.Err()
}

// 内部上界：调用方不给超时（无 deadline 的 ctx）时，交给底层工具的 ctx 也必须带 deadline
// （ping 30s 档、traceroute 60s 档）。红-绿：修复前该 ctx 就是调用方的无上界 ctx
// （CLI 用 context.Background()）⇒ 宿主工具挂住即调用挂住。
func TestDiagAppliesInternalDeadline(t *testing.T) {
	cases := []struct {
		name string
		call func(context.Context, *Diag) (string, error)
		max  time.Duration
	}{
		{"ping", func(ctx context.Context, d *Diag) (string, error) {
			return d.Ping(ctx, "10.0.0.1", "", "", 1, false)
		}, diagPingTimeout},
		{"traceroute", func(ctx context.Context, d *Diag) (string, error) {
			return d.Traceroute(ctx, "10.0.0.1", "", false)
		}, diagTracerouteTimeout},
	}
	for _, tc := range cases {
		r := &diagBlockRunner{}
		// 父 ctx 无 deadline（只可取消）：上界只能来自内部。
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()
		before := time.Now()
		if _, err := tc.call(ctx, NewDiag(r, nil)); err == nil {
			t.Fatalf("%s：ctx 结束时调用应如实返回错误", tc.name)
		}
		cancel()
		if !r.hasDeadline {
			t.Fatalf("%s：交给底层工具的 ctx 必须带内部超时上界（当前无）", tc.name)
		}
		// 允许亚毫秒级抖动：deadline 由被测代码在 before 之后取「当前时间」算出，
		// 严格 `≤ tc.max` 会因两次 time.Now() 的先后差假红（实测见过 30.0004s）。
		const slack = 50 * time.Millisecond
		if left := r.deadline.Sub(before); left <= 0 || left > tc.max+slack {
			t.Fatalf("%s：内部上界应≈ %s（允许 %s 抖动），实际 %s", tc.name, tc.max, slack, left)
		}
	}
	// 与 VPP 侧同档的量级守卫（ping 30s；不设上界或设成分钟级即回归）。
	if diagPingTimeout > time.Minute || diagTracerouteTimeout > 2*time.Minute {
		t.Fatalf("诊断上界过大（ping %s / traceroute %s）", diagPingTimeout, diagTracerouteTimeout)
	}
}

// 有界是**生效**的（不只是给 ctx 设了值）：调用方给更紧的 deadline 时，调用在期限内结束并如实报错。
func TestDiagBoundedByCallerDeadline(t *testing.T) {
	r := &diagBlockRunner{}
	d := NewDiag(r, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := d.Ping(ctx, "10.0.0.1", "", "", 1, false)
	if err == nil {
		t.Fatal("宿主工具挂住时应在 deadline 到期后如实返回错误")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("错误应可判为超时（context.DeadlineExceeded），得到 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("调用未被 deadline 约束（耗时 %s）", elapsed)
	}
}

// ---------- 决策 #457：vrf 参数按内核设备名解析 ----------

// diagResolverFor 构造「从配置解析内核 VRF 设备名」的解析器（与装配处同一实现）。
func diagResolverFor(cfg model.Config) VRFResolver {
	return func(name string) (string, error) { return KernelVRFDeviceName(cfg, name) }
}

// L2 网关交换机 ⇒ `ip vrf exec vr-<名>`；L3 交换机 / vrfs 条目 ⇒ 原名；显式 gateway.vrf 生效；
// ping 与 traceroute 共用 runVrf ⇒ 同改。
func TestDiagVRFResolvesKernelDeviceNames(t *testing.T) {
	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{
			{Name: "lan", Type: "l2", Gateway: &model.VSGateway{Addresses: []string{"192.168.99.1/24"}}},
			{Name: "vs-l3", Type: "l3"},
			{Name: "wan", Type: "l2", Gateway: &model.VSGateway{Addresses: []string{"10.0.0.1/24"}, Vrf: "explicit-vrf"}},
		},
		Vrfs: []model.Vrf{{Name: "vs-l3"}},
	}
	ctx := context.Background()

	r := &diagRecRunner{}
	d := NewDiag(r, diagResolverFor(cfg))
	for _, call := range []struct {
		host, vrf string
	}{
		{"192.168.99.10", "lan"}, // L2 网关域 ⇒ vr-lan
		{"10.0.0.2", "vs-l3"},    // L3 ⇒ 原名
		{"10.0.0.3", "wan"},      // 显式 gateway.vrf ⇒ 其设备名
	} {
		if _, err := d.Ping(ctx, call.host, "", call.vrf, 2, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{
		"ip vrf exec vr-lan ping -c 2 192.168.99.10",
		"ip vrf exec vs-l3 ping -c 2 10.0.0.2",
		"ip vrf exec explicit-vrf ping -c 2 10.0.0.3",
	} {
		if !strings.Contains(r.joined(), want) {
			t.Fatalf("缺少命令 %q：\n%s", want, r.joined())
		}
	}

	// traceroute 与 ping 共用 runVrf：同样解析成设备名。
	r2 := &diagRecRunner{}
	if _, err := NewDiag(r2, diagResolverFor(cfg)).Traceroute(ctx, "192.168.99.10", "lan", false); err != nil {
		t.Fatal(err)
	}
	if want := "ip vrf exec vr-lan traceroute 192.168.99.10"; !strings.Contains(r2.joined(), want) {
		t.Fatalf("traceroute 应同样解析成设备名 %q：\n%s", want, r2.joined())
	}
}

// 长名截断与命名单源（GatewayVRFName）一致——不另抄一份命名规则。
func TestDiagVRFLongNameTruncationMatchesNaming(t *testing.T) {
	long := "abcdefghijklmno" // 15 字符：`vr-`+名 超上限 ⇒ 走哈希截断
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{
		{Name: long, Type: "l2", Gateway: &model.VSGateway{Addresses: []string{"192.168.99.1/24"}}},
	}}
	want := GatewayVRFName(long)
	if want == "vr-"+long {
		t.Fatalf("前提不成立：GatewayVRFName(%q) = %q 应走截断", long, want)
	}
	r := &diagRecRunner{}
	if _, err := NewDiag(r, diagResolverFor(cfg)).Ping(context.Background(), "192.168.99.10", "", long, 1, false); err != nil {
		t.Fatal(err)
	}
	if got := "ip vrf exec " + want + " ping -c 1 192.168.99.10"; !strings.Contains(r.joined(), got) {
		t.Fatalf("长名截断应与命名单源一致 %q：\n%s", got, r.joined())
	}
}

// 未知 vrf ⇒ 明确报错并列出已知设备；不得执行宿主命令。未声明网关的 L2 交换机没有专属 VRF，
// 同样按「未知」报错。
func TestDiagVRFUnknownReportsKnownDevices(t *testing.T) {
	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{
			{Name: "lan", Type: "l2", Gateway: &model.VSGateway{Addresses: []string{"192.168.99.1/24"}}},
			{Name: "vs-l3", Type: "l3"},
			{Name: "nogw", Type: "l2"}, // 未声明网关：没有专属 VRF
		},
		Vrfs: []model.Vrf{{Name: "vs-l3"}},
	}
	for _, bad := range []string{"nope", "nogw"} {
		r := &diagRecRunner{}
		_, err := NewDiag(r, diagResolverFor(cfg)).Ping(context.Background(), "10.0.0.1", "", bad, 1, false)
		if err == nil {
			t.Fatalf("未知 vrf %q 应报错", bad)
		}
		if !strings.Contains(err.Error(), bad) {
			t.Fatalf("报错应点名未知对象 %q：%v", bad, err)
		}
		for _, want := range []string{"vr-lan", "vs-l3"} { // 已知设备清单
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("报错应列出已知设备 %q：%v", want, err)
			}
		}
		if len(r.calls) != 0 {
			t.Fatalf("解析失败时不得执行宿主命令：\n%s", r.joined())
		}
	}

	// traceroute 同口径。
	r := &diagRecRunner{}
	if _, err := NewDiag(r, diagResolverFor(cfg)).Traceroute(context.Background(), "10.0.0.1", "nope", false); err == nil {
		t.Fatal("traceroute 未知 vrf 应报错")
	}
	if len(r.calls) != 0 {
		t.Fatalf("traceroute 解析失败时不得执行宿主命令：\n%s", r.joined())
	}
}

// resolver 未注入 ⇒ 沿用按对象名映射的既有行为（不破坏既有装配/单测）。
func TestDiagVRFNilResolverKeepsLegacy(t *testing.T) {
	r := &diagRecRunner{}
	if _, err := NewDiag(r, nil).Ping(context.Background(), "10.0.0.1", "", "vs-l3", 1, false); err != nil {
		t.Fatal(err)
	}
	if want := "ip vrf exec vs-l3 ping -c 1 10.0.0.1"; !strings.Contains(r.joined(), want) {
		t.Fatalf("nil 解析器应沿用对象名映射 %q：\n%s", want, r.joined())
	}
}
