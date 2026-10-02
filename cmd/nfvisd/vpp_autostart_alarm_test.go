package main

// 决策 #348：VPP_AUTOSTART_FAILED 告警的建/消守护（参照 hugepages_alarm_test.go 的写法）。
//
// nfvisd 启动时若未能确保 VPP 运行（拉起失败或超时未就绪）→ Raise（warning，独立 scope）；
// VPP 恢复在线（或本就在运行）→ Resolve 自动消解——与 #329/#346 同一对账口径。

import (
	"errors"
	"testing"

	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

func TestVPPAutostartAlarmRaisedOnFailure(t *testing.T) {
	s := &fakeAlarmSink{}
	vppAutostartAlarms(s, errors.New("拉起 VPP 失败（systemctl start vpp）: exit status 1"))

	if !hasRaise(s, "vpp_autostart|"+network.AlarmVPPAutostartFailed+"|") {
		t.Fatalf("拉起失败应 Raise VPP_AUTOSTART_FAILED：%+v", s.raised)
	}
	if hasResolve(s, "vpp_autostart|"+network.AlarmVPPAutostartFailed) {
		t.Fatalf("失败时不该 Resolve：%+v", s.resolved)
	}
}

func TestVPPAutostartAlarmResolvedWhenOnline(t *testing.T) {
	s := &fakeAlarmSink{}
	vppAutostartAlarms(s, nil) // VPP 已在运行 / 已拉起就绪

	if hasRaise(s, "vpp_autostart|") {
		t.Fatalf("VPP 可用时不该 Raise：%+v", s.raised)
	}
	if !hasResolve(s, "vpp_autostart|"+network.AlarmVPPAutostartFailed) {
		t.Fatalf("VPP 已在线应 Resolve 自动消解：%+v", s.resolved)
	}
}
