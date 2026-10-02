package main

// 启动期底座降级告警（决策 #349/#351）的文案与去向守护。
//
// 降级必须可见：libvirt 连接失败/超时 → COMPUTE_UNAVAILABLE（scope compute、
// source libvirt）；Docker 探测失败 → CONTAINER_UNAVAILABLE（scope container、
// source docker）。两告警文案都要求带「发生了什么 / 独立事实源手查路径 / 恢复路径」，
// 且不得出现内部编号（user_text 规则的单元级镜像）。恢复路径按 #351 如实写后台
// 持续重试接入（成功自动消解、无须重启 nfvis）——不再是「不自动重连、须 restart nfvis」。
// 用独立假告警表记录全部字段（hugepages_alarm_test.go 的 fakeAlarmSink 只记
// scope|code|message，不记 severity，不满足本守护对级别的断言）。

import (
	"errors"
	"strings"
	"testing"
)

// degradeAlarmSink 记录 Raise 的全部字段。
type degradeAlarmSink struct {
	raised []degradeAlarm
}

type degradeAlarm struct {
	scope, severity, code, message, source string
}

func (f *degradeAlarmSink) Raise(scope, severity, code, message, source string) {
	f.raised = append(f.raised, degradeAlarm{scope, severity, code, message, source})
}

func (f *degradeAlarmSink) Resolve(scope, code, source string) bool { return false }

// assertMessage 要素断言：必须含 have 的每一项，且不得含内部编号。
func assertMessage(t *testing.T, a degradeAlarm, have ...string) {
	t.Helper()
	for _, s := range have {
		if !strings.Contains(a.message, s) {
			t.Fatalf("告警文案缺要素 %q：%s", s, a.message)
		}
	}
	for _, bad := range []string{"FR-", "R129", "#349", "决策"} {
		if strings.Contains(a.message, bad) {
			t.Fatalf("告警文案含内部编号 %q（操作者读不懂）：%s", bad, a.message)
		}
	}
}

func TestComputeUnavailableAlarm(t *testing.T) {
	s := &degradeAlarmSink{}
	computeUnavailableAlarm(s, "qemu:///system", errors.New("连接 libvirt qemu:///system 超时"))

	if len(s.raised) != 1 {
		t.Fatalf("应恰好一条告警：%+v", s.raised)
	}
	a := s.raised[0]
	if a.scope != "compute" || a.severity != "warning" || a.code != "COMPUTE_UNAVAILABLE" || a.source != "libvirt" {
		t.Fatalf("scope/severity/code/source 不符契约：%+v", a)
	}
	assertMessage(t, a,
		"未接入 libvirt", "降级运行", "VM 生命周期动作不可用", "已有配置声明不受影响",
		"systemctl status libvirtd", "journalctl -u libvirtd", "virsh -c qemu:///system list",
		"后台持续重试接入", "自动消解", "无需重启 nfvis")
	if !strings.Contains(a.message, "超时") {
		t.Fatalf("文案应带上游原因：%s", a.message)
	}
}

func TestComputeUnavailableAlarmNilErr(t *testing.T) {
	// 纯函数健壮性：err 为 nil 不应拼出「原因: <nil>」。
	s := &degradeAlarmSink{}
	computeUnavailableAlarm(s, "qemu:///system", nil)
	if !strings.Contains(s.raised[0].message, "未知") {
		t.Fatalf("nil 错误应如实写未知：%s", s.raised[0].message)
	}
}

func TestContainerUnavailableAlarm(t *testing.T) {
	s := &degradeAlarmSink{}
	containerUnavailableAlarm(s, "unix:///var/run/docker.sock", errors.New("dial unix /var/run/docker.sock: connect: no such file or directory"))

	if len(s.raised) != 1 {
		t.Fatalf("应恰好一条告警：%+v", s.raised)
	}
	a := s.raised[0]
	if a.scope != "container" || a.severity != "warning" || a.code != "CONTAINER_UNAVAILABLE" || a.source != "docker" {
		t.Fatalf("scope/severity/code/source 不符契约：%+v", a)
	}
	assertMessage(t, a,
		"未接入 Docker", "降级运行", "容器生命周期动作不可用", "已有配置声明不受影响",
		"systemctl status docker", "journalctl -u docker",
		"后台持续重试接入", "自动消解", "无需重启 nfvis")
	if !strings.Contains(a.message, "unix:///var/run/docker.sock") {
		t.Fatalf("文案应带 socket 线索：%s", a.message)
	}
}
