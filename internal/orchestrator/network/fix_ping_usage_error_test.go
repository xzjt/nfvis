package network

// 修 BUG-04（round42 报告）：`ping <非法目标>` 报错误文案却以 **0** 退出，脚本/自动化据此
// 做连通性断言会**假通过**。
//
// 真机取证（v3.0.24）：
//
//	$ nfvis-cli -c "ping not-a-host!!"; echo rc=$?
//	ping: expecting IP4/IP6 address `not-a-host!!'. Usage: ping <addr> ...
//	rc=0
//	$ vppctl ping not-a-host!! ; echo $?      → 0（vppctl 自身就以 0 退出）
//
// 根因：Ping 的既有取舍是「判不出 Statistics 汇总行就不判失败」（避免输出格式一变就误报），
// 而非法参数的输出里根本没有汇总行 ⇒ 落进「不判失败」这一支。本用例钉住「参数没被接受」
// 必须如实失败，同时保证既有两条支路（0 发出 / 0 应答）与正常路径不被误伤。

import (
	"context"
	"strings"
	"testing"
)

// fakePingShell 返回预设输出与错误（模拟 vppctl）。
type fakePingShell struct {
	out string
	err error
}

func (f fakePingShell) Run(context.Context, ...string) (string, error) { return f.out, f.err }

// 非法目标：vppctl 打 usage 但 rc=0 ⇒ 必须返回错误（修复前返回 nil）。
func TestPingInvalidTargetReportsFailure(t *testing.T) {
	const usage = "ping: expecting IP4/IP6 address `not-a-host!!'. Usage: ping <addr> [source <intf>] [size <datasz>] [repeat <count>] [verbose]\n"
	d := NewDiagnostics(nil, fakePingShell{out: usage}, nil)
	out, err := d.Ping(context.Background(), PingRequest{Host: "not-a-host!!"})
	if err == nil {
		t.Fatalf("非法目标必须判失败（vppctl 以 0 退出，输出里没有 Statistics 行）：%q", out)
	}
	if !strings.Contains(err.Error(), "未被 vppctl 接受") {
		t.Fatalf("错误应说明参数未被接受: %v", err)
	}
	if !strings.Contains(out, "expecting IP4/IP6") {
		t.Fatalf("原文应原样带回给操作者: %q", out)
	}
}

// 正常连通：有汇总行且 received>0 ⇒ 成功（回归，不得被 usage 判据误伤）。
func TestPingSuccessStillSucceeds(t *testing.T) {
	const ok = "\nStatistics: 3 sent, 3 received, 0% packet loss\n"
	d := NewDiagnostics(nil, fakePingShell{out: ok}, nil)
	if _, err := d.Ping(context.Background(), PingRequest{Host: "10.0.0.1"}); err != nil {
		t.Fatalf("正常连通不应失败: %v", err)
	}
}

// 0 发出 / 0 应答两条既有支路仍按原口径失败（回归）。
func TestPingZeroSentAndZeroReceivedStillFail(t *testing.T) {
	cases := []struct{ name, out string }{
		{"零发出（无出口）", "Statistics: 0 sent, 0 received, 0% packet loss\n"},
		{"有发出无应答", "Statistics: 2 sent, 0 received, 100% packet loss\n"},
	}
	for _, c := range cases {
		d := NewDiagnostics(nil, fakePingShell{out: c.out}, nil)
		if _, err := d.Ping(context.Background(), PingRequest{Host: "10.0.0.1"}); err == nil {
			t.Fatalf("%s 应判失败", c.name)
		}
	}
}

// 汇总行缺失但也不是 usage 错误（例如将来 vppctl 改了输出格式）：**仍不判失败**——
// 这条取舍是既有的、有意为之（宁可漏报不误报），本轮不得因修 BUG-04 把它改掉。
func TestPingWithoutSummaryAndNoUsageStillPasses(t *testing.T) {
	d := NewDiagnostics(nil, fakePingShell{out: "some future output format\n"}, nil)
	if _, err := d.Ping(context.Background(), PingRequest{Host: "10.0.0.1"}); err != nil {
		t.Fatalf("判不出汇总行（且非参数错）时不应判失败: %v", err)
	}
}
