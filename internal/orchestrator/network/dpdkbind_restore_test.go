package network

// round49（外部走查）：DPDK 绑定中途失败不还原原驱动。
//
// 缺陷形态：Bind 的顺序是「unbind 原驱动 → 写 driver_override → bind 目标驱动」，后两步任一
// 失败此前**直接返回**：网卡已被解绑、又没绑上目标驱动，就此留在无驱动状态，而错误文案只说
// 失败原因（如 sysfs 写失败），看不出「设备现在可能已无驱动、需要人工处理」这个更严重的事实。
//
// 本文件钉住：失败时先尽力把设备交还**原驱动**（同一 sysfs 写路径），恢复成功在文案里说明；
// 恢复失败给类型化错误 ErrDriverRestoreFailed（含 PCI/原驱动/目标驱动/两个错误）并落一条
// error 级告警（可注入的告警落点）。「目标驱动不可用」在 unbind 之前失败，不得尝试回绑。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const restoreTestPCI = "0000-13-00.0"

// failWritesOn 让指定路径的 sysfs 写失败（其余照常记录并落盘）。
func failWritesOn(t *testing.T, b *DPDKBinder, paths ...string) map[string]bool {
	t.Helper()
	blocked := map[string]bool{}
	for _, p := range paths {
		blocked[p] = true
	}
	base := b.WriteFile
	b.WriteFile = func(path string, data []byte) error {
		if blocked[path] {
			return errors.New("sysfs 写失败（注入）")
		}
		return base(path, data)
	}
	return blocked
}

// failWriteOf 只让「把 payload 写到 path」这一次写失败（清空类写入照常）——精确模拟某一步失败。
func failWriteOf(t *testing.T, b *DPDKBinder, path, payload string) {
	t.Helper()
	base := b.WriteFile
	b.WriteFile = func(p string, data []byte) error {
		if p == path && strings.TrimSpace(string(data)) == payload {
			return errors.New("sysfs 写失败（注入）")
		}
		return base(p, data)
	}
}

func TestDPDKBinderBindRestoresOriginalDriverOnOverrideFailure(t *testing.T) {
	root, _ := fakeSysfs(t, "ens224", "0000-13-00.0", "vmxnet3")
	b, writes := newTestBinder(t, root)
	override := filepath.Join(root, "bus", "pci", "devices", restoreTestPCI, "driver_override")
	failWriteOf(t, b, override, DefaultUioDriver)

	_, err := b.Bind(context.Background(), "ens224", "")
	if err == nil {
		t.Fatal("driver_override 写失败应报错")
	}
	if !strings.Contains(err.Error(), "已还原原驱动 vmxnet3") {
		t.Fatalf("恢复成功时文案应说明已还还原驱动；实际: %v", err)
	}
	w := *writes
	// 恢复的第一动作必须是把设备回绑原驱动（同一 sysfs 写路径：<原驱动>/bind）
	bindKey := filepath.Join(root, "bus", "pci", "drivers", "vmxnet3", "bind")
	if got := w[bindKey]; len(got) != 1 || got[0] != restoreTestPCI {
		t.Fatalf("失败后应回绑原驱动（写 %s=%s）；实际: %v", bindKey, restoreTestPCI, got)
	}
	// 不得把设备留在「被钉在目标驱动」的覆盖值上：恢复要清 driver_override
	if got := w[override]; len(got) != 1 || strings.TrimSpace(got[0]) != "" {
		t.Fatalf("恢复应清空 driver_override；实际: %v", got)
	}
}

func TestDPDKBinderBindRestoresOriginalDriverOnTargetBindFailure(t *testing.T) {
	root, _ := fakeSysfs(t, "ens224", "0000-13-00.0", "vmxnet3")
	b, writes := newTestBinder(t, root)
	failWriteOf(t, b, filepath.Join(root, "bus", "pci", "drivers", DefaultUioDriver, "bind"), restoreTestPCI)

	_, err := b.Bind(context.Background(), "ens224", "")
	if err == nil {
		t.Fatal("目标驱动 bind 失败应报错")
	}
	if !strings.Contains(err.Error(), "已还原原驱动 vmxnet3") {
		t.Fatalf("恢复成功时文案应说明已还原原驱动；实际: %v", err)
	}
	if got := (*writes)[filepath.Join(root, "bus", "pci", "drivers", "vmxnet3", "bind")]; len(got) != 1 {
		t.Fatalf("失败后应回绑原驱动；实际: %v", got)
	}
}

func TestDPDKBinderBindTypedErrorWhenRestoreAlsoFails(t *testing.T) {
	root, _ := fakeSysfs(t, "ens224", "0000-13-00.0", "vmxnet3")
	b, _ := newTestBinder(t, root)
	failWritesOn(t, b,
		filepath.Join(root, "bus", "pci", "devices", restoreTestPCI, "driver_override"),
		filepath.Join(root, "bus", "pci", "drivers", DefaultUioDriver, "bind"),
		filepath.Join(root, "bus", "pci", "drivers", "vmxnet3", "bind"))

	_, err := b.Bind(context.Background(), "ens224", "")
	if err == nil {
		t.Fatal("两次失败都应报错")
	}
	if !errors.Is(err, ErrDriverRestoreFailed) {
		t.Fatalf("回绑也失败必须是类型化错误 ErrDriverRestoreFailed；实际: %v", err)
	}
	for _, want := range []string{"可能已无驱动", restoreTestPCI, "vmxnet3", DefaultUioDriver} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误须含 %q（PCI/原驱动/目标驱动/后果）；实际: %v", want, err)
		}
	}
}

// 目标驱动不可用（模块未加载且加载失败）在 **unbind 之前**失败：原驱动未被解绑，
// 不得尝试回绑（那只会是无意义的写入）。
func TestDPDKBinderBindDoesNotRestoreWhenTargetDriverMissing(t *testing.T) {
	root, _ := fakeSysfs(t, "ens224", "0000-13-00.0", "vmxnet3")
	if err := os.RemoveAll(filepath.Join(root, "bus", "pci", "drivers", DefaultUioDriver)); err != nil {
		t.Fatal(err)
	}
	b, writes := newTestBinder(t, root)
	b.ModuleLoader = func(string) error { return errors.New("modprobe: not found") }

	_, err := b.Bind(context.Background(), "ens224", "")
	if err == nil {
		t.Fatal("目标驱动不可用应报错")
	}
	if errors.Is(err, ErrDriverRestoreFailed) {
		t.Fatalf("原驱动未被解绑时不得报「未能还原原驱动」；实际: %v", err)
	}
	w := *writes
	if got := w[filepath.Join(root, "bus", "pci", "drivers", "vmxnet3", "bind")]; len(got) != 0 {
		t.Fatalf("原驱动未被解绑时不得回绑；实际: %v", got)
	}
	if got := w[filepath.Join(root, "bus", "pci", "drivers", "vmxnet3", "unbind")]; len(got) != 0 {
		t.Fatalf("目标驱动不可用时也不该已解绑原驱动；实际: %v", got)
	}
}

// fakeAlarmSink 记录告警建/消（单测不建真告警表）。
type fakeAlarmSink struct {
	raised   []string
	resolved []string
}

func (f *fakeAlarmSink) Raise(scope, severity, code, message, source string) {
	f.raised = append(f.raised, scope+"/"+severity+"/"+code+"/"+source)
}

func (f *fakeAlarmSink) Resolve(scope, code, source string) bool {
	f.resolved = append(f.resolved, scope+"/"+code+"/"+source)
	return true
}

// 恢复失败必须落一条 error 级告警（事后可查），同 PCI 下一次绑定成功即消警。
func TestDPDKBinderBindRestoreFailureRaisesAlarmAndSuccessResolves(t *testing.T) {
	root, _ := fakeSysfs(t, "ens224", "0000-13-00.0", "vmxnet3")
	b, _ := newTestBinder(t, root)
	sink := &fakeAlarmSink{}
	b.Alarms = sink
	orig := b.WriteFile
	override := filepath.Join(root, "bus", "pci", "devices", restoreTestPCI, "driver_override")
	targetBind := filepath.Join(root, "bus", "pci", "drivers", DefaultUioDriver, "bind")
	failWritesOn(t, b, override, targetBind,
		filepath.Join(root, "bus", "pci", "drivers", "vmxnet3", "bind"))

	_, err := b.Bind(context.Background(), "ens224", "")
	if err == nil {
		t.Fatal("该现场应报错")
	}
	if len(sink.raised) != 1 {
		t.Fatalf("恢复失败应落一条告警；实际 %v", sink.raised)
	}
	want := dpdkAlarmScope + "/" + SeverityError + "/" + AlarmDPDKRestoreFailed + "/" + restoreTestPCI
	if sink.raised[0] != want {
		t.Fatalf("告警形状应为 scope/severity/code/source=%q；实际 %q", want, sink.raised[0])
	}

	// 修好现场后重新绑定成功 ⇒ 该告警自动消解（不把已不成立的状态留在表里）。
	b.WriteFile = orig
	if _, err := b.Bind(context.Background(), "ens224", ""); err != nil {
		t.Fatalf("修好后绑定应成功: %v", err)
	}
	wantResolve := dpdkAlarmScope + "/" + AlarmDPDKRestoreFailed + "/" + restoreTestPCI
	if len(sink.resolved) != 1 || sink.resolved[0] != wantResolve {
		t.Fatalf("绑定成功应消警 %q；实际 %v", wantResolve, sink.resolved)
	}
}

// 幂等路径（设备已就位目标驱动）也是「该口现在正常」的事实：旧的还原失败告警应随之消解。
func TestDPDKBinderBindIdempotentPathResolvesRestoreAlarm(t *testing.T) {
	root, _ := fakeSysfs(t, "ens224", "0000-13-00.0", DefaultUioDriver)
	b, _ := newTestBinder(t, root)
	sink := &fakeAlarmSink{}
	b.Alarms = sink
	if _, err := b.Bind(context.Background(), "ens224", ""); err != nil {
		t.Fatalf("已就位应幂等成功: %v", err)
	}
	want := dpdkAlarmScope + "/" + AlarmDPDKRestoreFailed + "/" + restoreTestPCI
	if len(sink.resolved) != 1 || sink.resolved[0] != want {
		t.Fatalf("幂等路径应消警 %q；实际 %v", want, sink.resolved)
	}
}
