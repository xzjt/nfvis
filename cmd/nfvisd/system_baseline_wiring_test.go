package main

// 决策 #424：系统基线（主机名/时区/NTP/宿主解析器）落地的 nfvisd 侧接线守护。
//
// 启动与每次提交后都走同一条 `applySystemBaseline`（与证书/日志保留/主机防火墙同路）：
// 落地动作 INFO 留痕、失败与跳过逐条 WARN（含原因），**失败不阻塞调用方**——宿主设置
// 应用失败不得让提交失败或 nfvisd 起不来；未装配 / 未配置时安静跳过。

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/system"
)

// baselineRunner 假宿主命令：探测命令返回「与配置不同」的现状（保证走应用路径），
// 其余命令记录后安静成功。
type baselineRunner struct {
	calls []string
	fail  string // 命中的命令报错（模拟宿主命令失败）
}

func (r *baselineRunner) run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, line)
	if r.fail != "" && strings.Contains(line, r.fail) {
		return "", errors.New("模拟宿主命令失败")
	}
	switch {
	case strings.HasPrefix(line, "hostnamectl hostname"):
		return "nfvis\n", nil
	case strings.HasPrefix(line, "timedatectl show"):
		return "Etc/UTC\n", nil
	case strings.HasPrefix(line, "systemctl is-active"):
		return "active\n", nil
	}
	return "", nil
}

func newBaselineLog() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func TestApplySystemBaselineAppliesAllItems(t *testing.T) {
	run := &baselineRunner{}
	a := system.NewSystemBaselineApplier(run.run)
	a.Root = t.TempDir()
	a.LookPath = func(string) (string, error) { return "/usr/bin/x", nil }
	log, buf := newBaselineLog()

	cfg := model.Config{System: &model.SystemConfig{
		Hostname:   "nfvis-lab",
		Timezone:   "Asia/Shanghai",
		Ntp:        []model.NtpServer{{Server: "ntp.ubuntu.com", Prefer: true}},
		DNSServers: []string{"8.8.8.8"},
	}}
	applySystemBaseline(cfg, a, &fakeAlarmSink{}, log)

	for _, want := range []string{
		"hostnamectl set-hostname nfvis-lab",
		"timedatectl set-timezone Asia/Shanghai",
		"chronyc reload sources",
		"systemctl restart systemd-resolved",
	} {
		if !slices.Contains(run.calls, want) {
			t.Errorf("启动/提交后应执行 %q，实际调用：%v", want, run.calls)
		}
	}
	if !strings.Contains(buf.String(), "系统基线已应用到宿主") {
		t.Errorf("落地动作应有 INFO 留痕，日志：%s", buf.String())
	}
}

func TestApplySystemBaselineFailureLoggedAndNotBlocking(t *testing.T) {
	run := &baselineRunner{fail: "hostnamectl set-hostname"}
	a := system.NewSystemBaselineApplier(run.run)
	a.Root = t.TempDir()
	a.LookPath = func(string) (string, error) { return "/usr/bin/x", nil }
	log, buf := newBaselineLog()

	cfg := model.Config{System: &model.SystemConfig{Hostname: "nfvis-lab", Timezone: "Asia/Shanghai"}}
	applySystemBaseline(cfg, a, &fakeAlarmSink{}, log) // 不返回错误、不 panic：失败只记日志

	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "主机名") {
		t.Errorf("失败应逐条 WARN 并说明原因，日志：%s", buf.String())
	}
	if !slices.Contains(run.calls, "timedatectl set-timezone Asia/Shanghai") {
		t.Errorf("一项失败不得拖住其余项，实际调用：%v", run.calls)
	}
}

func TestApplySystemBaselineSkipsQuietlyWhenNotConfigured(t *testing.T) {
	run := &baselineRunner{}
	a := system.NewSystemBaselineApplier(run.run)
	a.Root = t.TempDir()
	a.LookPath = func(string) (string, error) { return "/usr/bin/x", nil }
	log, buf := newBaselineLog()

	sink := &fakeAlarmSink{}
	applySystemBaseline(model.Config{}, a, sink, log)                              // 未配置 system 段
	applySystemBaseline(model.Config{System: &model.SystemConfig{}}, a, sink, log) // system 段为空
	applySystemBaseline(model.Config{}, nil, sink, log)                            // 未装配落地器

	if len(run.calls) != 0 {
		t.Errorf("未配置不应起任何子进程：%v", run.calls)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("未配置不应产生告警级日志：%s", buf.String())
	}
	if len(sink.raised) != 0 {
		t.Errorf("未配置不应建告警：%+v", sink.raised)
	}
	if len(sink.resolved) != 2 {
		t.Errorf("整段未声明应把告警消解两次（两次空配置）：%+v", sink.resolved)
	}
}
