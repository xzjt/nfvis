package main

// 决策 #428：系统基线应用失败告警（SYSTEM_BASELINE_APPLY_FAILED）的建/消守护。
//
// 与 hugepageAlarms/vppAutostartAlarms 同一对账口径：本次应用的**逐项结果即事实**——
// 任一项失败 → Raise（warning，scope="system"、source="system/baseline"，message 逐项含
// 项名/原因/自查命令）；本次没有任何失败项（全部成功、或声明已空）→ Resolve。不靠进程内
// 记忆（nfvisd 重启后由启动时的重试重建）。这里用假宿主命令 + 假告警表锁住建/消判定。

import (
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/system"
)

// baselineAlarmKey 假告警表里的「scope|code」前缀（Raise 记录另带 message）。
func baselineAlarmKey() string {
	return "system|" + system.BaselineApplyFailedAlarmCode
}

// newBaselineAlarmFixture 假宿主命令（探测返回「与配置不同」的现状，保证走应用路径）+
// 可注入的失败命令 + 临时配置根。
func newBaselineAlarmFixture(t *testing.T, failCmd string) *system.SystemBaselineApplier {
	t.Helper()
	run := &baselineRunner{fail: failCmd}
	a := system.NewSystemBaselineApplier(run.run)
	a.Root = t.TempDir()
	a.LookPath = func(string) (string, error) { return "/usr/bin/x", nil }
	return a
}

// 某项应用失败 → 告警在场，message 含项名、原因与自查命令；已成功的项不出现在失败清单里。
func TestSystemBaselineAlarmRaisedOnItemFailure(t *testing.T) {
	a := newBaselineAlarmFixture(t, "hostnamectl set-hostname")
	log, _ := newBaselineLog()
	sink := &fakeAlarmSink{}

	applySystemBaseline(model.Config{System: &model.SystemConfig{
		Hostname: "nfvis-lab", Timezone: "Asia/Shanghai", // 时区应成功、不出现
	}}, a, sink, log)

	if len(sink.raised) != 1 {
		t.Fatalf("一项失败应 Raise 一条告警：%+v", sink.raised)
	}
	got := sink.raised[0]
	if !strings.HasPrefix(got, baselineAlarmKey()+"|") {
		t.Fatalf("告警键不符（scope/code）：%q", got)
	}
	msg := strings.TrimPrefix(got, baselineAlarmKey()+"|")
	for _, want := range []string{"主机名", "模拟宿主命令失败", "自查"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message 应含 %q：%q", want, msg)
		}
	}
	if strings.Contains(msg, "时区") {
		t.Fatalf("已成功的项不该出现在失败清单里：%q", msg)
	}
	if len(sink.resolved) != 0 {
		t.Fatalf("有失败项时不该 Resolve：%+v", sink.resolved)
	}
}

// 同一声明下一次应用成功 → 自动消解（无需重启 nfvisd）。
func TestSystemBaselineAlarmResolvedOnNextSuccess(t *testing.T) {
	a := newBaselineAlarmFixture(t, "hostnamectl set-hostname")
	log, _ := newBaselineLog()
	cfg := model.Config{System: &model.SystemConfig{Hostname: "nfvis-lab"}}

	first := &fakeAlarmSink{}
	applySystemBaseline(cfg, a, first, log)
	if !hasRaise(first, baselineAlarmKey()+"|") {
		t.Fatalf("失败时应 Raise：%+v", first.raised)
	}

	// 宿主故障排除（hostnamectl 恢复正常）——同一声明再次应用成功。
	a.Runner = (&baselineRunner{}).run
	second := &fakeAlarmSink{}
	applySystemBaseline(cfg, a, second, log)
	if len(second.raised) != 0 {
		t.Fatalf("应用成功时不该 Raise：%+v", second.raised)
	}
	if !hasResolve(second, baselineAlarmKey()) {
		t.Fatalf("应用成功应 Resolve：%+v", second.resolved)
	}
}

// 该项声明已空 → 自动消解（整段 system 清空同样成立）。
func TestSystemBaselineAlarmResolvedWhenDeclarationCleared(t *testing.T) {
	a := newBaselineAlarmFixture(t, "hostnamectl set-hostname")
	log, _ := newBaselineLog()

	first := &fakeAlarmSink{}
	applySystemBaseline(model.Config{System: &model.SystemConfig{Hostname: "nfvis-lab"}}, a, first, log)
	if !hasRaise(first, baselineAlarmKey()+"|") {
		t.Fatalf("失败时应 Raise：%+v", first.raised)
	}

	// 声明整段清空：Apply 空转（零结果）⇒ 无失败项 ⇒ 消解。
	second := &fakeAlarmSink{}
	applySystemBaseline(model.Config{}, a, second, log)
	if len(second.raised) != 0 {
		t.Fatalf("声明清空后不该 Raise：%+v", second.raised)
	}
	if !hasResolve(second, baselineAlarmKey()) {
		t.Fatalf("声明清空应 Resolve：%+v", second.resolved)
	}
}

// 本机不具备条件的「如实跳过」不是应用失败（三态语义不变，仍只落日志）：不建告警；
// 本次没有任何失败项，已有告警同样按对账消解。
func TestSystemBaselineAlarmSkipIsNotFailure(t *testing.T) {
	a := newBaselineAlarmFixture(t, "")
	a.LookPath = func(string) (string, error) { return "", errors.New("not found") } // 本机无 chrony
	log, _ := newBaselineLog()
	sink := &fakeAlarmSink{}

	applySystemBaseline(model.Config{System: &model.SystemConfig{
		Ntp: []model.NtpServer{{Server: "ntp.ubuntu.com"}},
	}}, a, sink, log)

	if len(sink.raised) != 0 {
		t.Fatalf("如实跳过不该建告警：%+v", sink.raised)
	}
	if !hasResolve(sink, baselineAlarmKey()) {
		t.Fatalf("无失败项应 Resolve：%+v", sink.resolved)
	}
}

// 未装配落地器时不猜测：既不 Raise 也不 Resolve（没有任何应用事实可对账）。
func TestSystemBaselineAlarmNoApplierNoVerdict(t *testing.T) {
	sink := &fakeAlarmSink{}
	log, _ := newBaselineLog()
	applySystemBaseline(model.Config{System: &model.SystemConfig{Hostname: "nfvis-lab"}}, nil, sink, log)
	if len(sink.raised) != 0 || len(sink.resolved) != 0 {
		t.Fatalf("未装配落地器时不该动告警：raised=%+v resolved=%+v", sink.raised, sink.resolved)
	}
}
