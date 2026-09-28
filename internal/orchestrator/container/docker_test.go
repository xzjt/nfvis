package container

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

type mockDocker struct {
	states    map[string]string // name → 契约状态
	specs     map[string]CreateSpec
	calls     []string
	err       error
	logs      string
	exitCodes map[string]int
	oomKilled map[string]bool
	stateErr  error // State 查询失败注入（查询失败不清警的单测）
	oomErr    error // OOMKilled 查询失败注入
}

func newMockDocker() *mockDocker {
	return &mockDocker{states: map[string]string{}, specs: map[string]CreateSpec{}, exitCodes: map[string]int{},
		oomKilled: map[string]bool{}}
}

func (m *mockDocker) Create(_ context.Context, name string, spec CreateSpec) error {
	if m.err != nil {
		return m.err
	}
	m.specs[name] = spec
	m.states[name] = orchestrator.CTStateExited
	m.calls = append(m.calls, "create:"+name)
	return nil
}
func (m *mockDocker) Start(_ context.Context, name string) error {
	m.states[name] = orchestrator.CTStateRunning
	m.calls = append(m.calls, "start:"+name)
	return nil
}
func (m *mockDocker) Stop(_ context.Context, name string) error {
	m.states[name] = orchestrator.CTStateExited
	m.calls = append(m.calls, "stop:"+name)
	return nil
}
func (m *mockDocker) Restart(_ context.Context, name string) error {
	m.states[name] = orchestrator.CTStateRunning
	m.calls = append(m.calls, "restart:"+name)
	return nil
}
func (m *mockDocker) Remove(_ context.Context, name string, force bool) error {
	delete(m.states, name)
	m.calls = append(m.calls, fmt.Sprintf("remove:%s:%v", name, force))
	return nil
}
func (m *mockDocker) ExitCode(_ context.Context, name string) (int, bool, error) {
	_, ok := m.states[name]
	return m.exitCodes[name], ok, nil
}
func (m *mockDocker) OOMKilled(_ context.Context, name string) (bool, bool, error) {
	if m.oomErr != nil {
		return false, false, m.oomErr
	}
	_, ok := m.states[name]
	return m.oomKilled[name], ok, nil
}
func (m *mockDocker) LoadImage(_ context.Context, _, _ string) error { return nil }

func (m *mockDocker) RemoveImage(_ context.Context, ref string) error {
	m.calls = append(m.calls, "rmi:"+ref)
	return nil
}
func (m *mockDocker) State(_ context.Context, name string) (string, bool, error) {
	if m.stateErr != nil {
		return "", false, m.stateErr
	}
	s, ok := m.states[name]
	return s, ok, nil
}
func (m *mockDocker) Logs(_ context.Context, name string, tail int) (string, error) {
	return m.logs, nil
}

func ctFixture(name string) model.ContainerFunction {
	return model.ContainerFunction{
		Name: name, Image: "alpine:3.20",
		VCPU: 2, MemoryMB: 256,
		Env:           map[string]string{"B": "2", "A": "1"},
		Command:       "/bin/sh",
		Args:          []string{"-c", "sleep 60"},
		RestartPolicy: "on-failure",
		Interfaces:    []model.VnfInterface{{Name: "eth0", Type: "memif", VirtualSwitch: "vs1"}},
		Autostart:     true,
	}
}

func TestBuildCreateSpec(t *testing.T) {
	cfg := DefaultConfig()
	ct := ctFixture("ct1")
	spec := BuildCreateSpec(ct, cfg)
	if spec.Image != "alpine:3.20" || spec.MemoryBytes != 256*1024*1024 || spec.NanoCPUs != 2_000_000_000 {
		t.Fatalf("镜像/内存/CPU 映射不符: %+v", spec)
	}
	if spec.RestartPolicy != "on-failure" || len(spec.Entrypoint) != 1 || spec.Entrypoint[0] != "/bin/sh" {
		t.Fatalf("重启策略/entrypoint 不符: %+v", spec)
	}
	if len(spec.Cmd) != 2 || spec.Cmd[0] != "-c" {
		t.Fatalf("args 应映射 Cmd: %v", spec.Cmd)
	}
	// env 键排序（A 在 B 前）
	if len(spec.Env) != 2 || spec.Env[0] != "A=1" || spec.Env[1] != "B=2" {
		t.Fatalf("env 应按键排序: %v", spec.Env)
	}
	wantBind := "/run/nfvis/memif/ct1-eth0.sock:/run/memif/eth0.sock"
	if len(spec.Binds) != 1 || spec.Binds[0] != wantBind {
		t.Fatalf("memif socket 应挂载进容器: %v", spec.Binds)
	}
	if !spec.Privileged || len(spec.CapAdd) != 1 || spec.CapAdd[0] != "NET_ADMIN" {
		t.Fatalf("memif 容器应具网络特权: privileged=%v caps=%v", spec.Privileged, spec.CapAdd)
	}

	// 缺省值：无 restart_policy → no；无 memif → 非特权无 binds。
	plain := model.ContainerFunction{Name: "p", Image: "img", MemoryMB: 64}
	sp := BuildCreateSpec(plain, cfg)
	if sp.RestartPolicy != "no" || sp.Privileged || len(sp.Binds) != 0 {
		t.Fatalf("缺省规格不符: %+v", sp)
	}
}

func TestApplyContainerLifecycle(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	ct := ctFixture("ct1")

	if err := p.ApplyContainer(context.Background(), ct); err != nil {
		t.Fatalf("ApplyContainer: %v", err)
	}
	if m.states["ct1"] != orchestrator.CTStateRunning {
		t.Fatalf("autostart 应启动: %v", m.states)
	}
	if len(m.specs["ct1"].Binds) != 1 {
		t.Fatalf("应带 memif 挂载: %+v", m.specs["ct1"])
	}
	// 幂等：已运行不再创建/启动。
	before := len(m.calls)
	if err := p.ApplyContainer(context.Background(), ct); err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != before {
		t.Fatalf("已运行应幂等: %v", m.calls[before:])
	}

	// 停止后 autostart 再 Apply → 启动
	_ = p.StopContainer(context.Background(), "ct1")
	if err := p.ApplyContainer(context.Background(), ct); err != nil {
		t.Fatal(err)
	}
	if m.states["ct1"] != orchestrator.CTStateRunning {
		t.Fatalf("停止后 Apply 应重启: %v", m.states)
	}

	// 状态 / 日志
	if st, err := p.ContainerState(context.Background(), "ct1"); err != nil || st != orchestrator.CTStateRunning {
		t.Fatalf("状态: %q %v", st, err)
	}
	m.logs = "hello\n"
	if out, err := p.ContainerLogs(context.Background(), "ct1", 10); err != nil || out != "hello\n" {
		t.Fatalf("日志: %q %v", out, err)
	}
	if st, err := p.ContainerState(context.Background(), "ghost"); err != nil || st != orchestrator.CTStateAbsent {
		t.Fatalf("不存在应为 absent: %q %v", st, err)
	}

	// 生命周期守卫
	if err := p.StartContainer(context.Background(), "ghost"); err == nil {
		t.Fatal("不存在容器 start 应报错")
	}
	if err := p.StopContainer(context.Background(), "ct1"); err != nil {
		t.Fatal(err)
	}
	if err := p.StopContainer(context.Background(), "ct1"); err != nil {
		t.Fatalf("已停止 stop 应幂等: %v", err)
	}
	if err := p.RestartContainer(context.Background(), "ct1"); err != nil {
		t.Fatal(err)
	}

	// 删除（force）+ 幂等
	if err := p.DeleteContainer(context.Background(), "ct1"); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteContainer(context.Background(), "ct1"); err != nil {
		t.Fatalf("重复删除应幂等: %v", err)
	}
	if _, exists, _ := m.State(context.Background(), "ct1"); exists {
		t.Fatal("应已删除")
	}
}

func TestEnsureConsistentContainers(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	cfg := model.Config{ContainerFunctions: []model.ContainerFunction{ctFixture("ct1"), ctFixture("ct2")}}
	if errs := p.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("收敛应成功: %v", errs)
	}
	if len(m.states) != 2 {
		t.Fatalf("应补建 2 个容器: %v", m.states)
	}
	// 失败收集
	m2 := newMockDocker()
	m2.err = fmt.Errorf("docker down")
	p2 := NewProvider(DefaultConfig(), m2)
	errs := p2.EnsureConsistent(context.Background(), cfg)
	if len(errs) != 2 || !strings.Contains(errs[0].Error(), "ct1") {
		t.Fatalf("应逐容器收集错误: %v", errs)
	}
}

func TestDockerStateToContract(t *testing.T) {
	cases := map[string]string{
		"running": orchestrator.CTStateRunning, "restarting": orchestrator.CTStateRunning,
		"paused": orchestrator.CTStateRunning, "created": orchestrator.CTStateExited,
		"exited": orchestrator.CTStateExited, "dead": orchestrator.CTStateDead,
		"": orchestrator.CTStateExited, "weird": orchestrator.CTStateExited,
	}
	for in, want := range cases {
		if got := dockerStateToContract(in); got != want {
			t.Errorf("docker 状态 %q → %q，期望 %q", in, got, want)
		}
	}
}

// sinkAlarm 假告警表里的一条活动告警（对账清警只需 scope/code/source）。
type sinkAlarm struct{ scope, severity, code, message, source string }

// fakeSink 记录告警并模拟活动集合（ActiveOf 供对账清警断言；与真实表同口径：同键幂等）。
type fakeSink struct {
	raised   []string // Raise 的 source 序列（逐次追加）
	details  []sinkAlarm
	active   []sinkAlarm // 当前活动告警
	resolved []string    // Resolve 的 source 序列（逐次追加）
}

func (f *fakeSink) Raise(scope, severity, code, message, source string) {
	f.raised = append(f.raised, source)
	f.details = append(f.details, sinkAlarm{scope, severity, code, message, source})
	for _, a := range f.active {
		if a.scope == scope && a.code == code && a.source == source {
			return
		}
	}
	f.active = append(f.active, sinkAlarm{scope, severity, code, message, source})
}

func (f *fakeSink) Resolve(scope, code, source string) bool {
	f.resolved = append(f.resolved, source)
	for i, a := range f.active {
		if a.scope == scope && a.code == code && a.source == source {
			f.active = append(f.active[:i], f.active[i+1:]...)
			return true
		}
	}
	return false
}

func (f *fakeSink) ActiveOf(scope string) []orchestrator.AlarmRef {
	var out []orchestrator.AlarmRef
	for _, a := range f.active {
		if a.scope == scope {
			out = append(out, orchestrator.AlarmRef{Code: a.code, Source: a.source})
		}
	}
	return out
}

// seed 预置一条活动告警：模拟此前检查已 Raise、对象随后被删除的场景。
func (f *fakeSink) seed(scope, code, source string) {
	f.active = append(f.active, sinkAlarm{scope: scope, code: code, source: source})
}

func TestEnsureConsistentAlarms(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	sink := &fakeSink{}
	p.SetAlarms(sink)
	cfg := model.Config{ContainerFunctions: []model.ContainerFunction{ctFixture("ct1")}}
	if errs := p.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("收敛应成功: %v", errs)
	}
	if len(sink.resolved) != 1 || len(sink.raised) != 0 {
		t.Fatalf("成功应收敛告警: raised=%v resolved=%v", sink.raised, sink.resolved)
	}

	m2 := newMockDocker()
	m2.err = fmt.Errorf("docker down")
	p2 := NewProvider(DefaultConfig(), m2)
	sink2 := &fakeSink{}
	p2.SetAlarms(sink2)
	if errs := p2.EnsureConsistent(context.Background(), cfg); len(errs) != 1 {
		t.Fatalf("应收集 1 个错误: %v", errs)
	}
	if len(sink2.raised) != 1 || sink2.raised[0] != "ct1" {
		t.Fatalf("应上报容器未收敛告警: %v", sink2.raised)
	}
}

// FR-CMP-022：dead / 非零退出 → critical 告警；running 消警。
func TestCheckContainerAlarms(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	sink := &fakeSink{}
	p.SetAlarms(sink)
	cfg := model.Config{ContainerFunctions: []model.ContainerFunction{ctFixture("ct1")}}

	m.states["ct1"] = orchestrator.CTStateDead
	if errs := p.CheckContainerAlarms(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("巡检不应报错: %v", errs)
	}
	if len(sink.raised) != 1 || sink.raised[0] != "ct1" {
		t.Fatalf("dead 应告警: %v", sink.raised)
	}
	// 非零退出码
	sink.raised = nil
	m.states["ct1"] = orchestrator.CTStateExited
	m.exitCodes["ct1"] = 3
	p.CheckContainerAlarms(context.Background(), cfg)
	if len(sink.raised) != 1 {
		t.Fatalf("非零退出应告警: %v", sink.raised)
	}
	// 正常退出（0）→ 消警
	m.exitCodes["ct1"] = 0
	p.CheckContainerAlarms(context.Background(), cfg)
	if len(sink.resolved) != 1 {
		t.Fatalf("正常退出应消警: %v", sink.resolved)
	}
}

// classifyExit 纯函数：OOMKilled 是异常判据核心；0/137/143（docker stop 的正常结果）为已停止。
func TestClassifyExit(t *testing.T) {
	cases := []struct {
		code int
		oom  bool
		want exitOutcome
	}{
		{0, false, exitStopped}, {137, false, exitStopped}, {143, false, exitStopped},
		{1, false, exitAbnormal}, {139, false, exitAbnormal},
		{137, true, exitOOM}, {0, true, exitOOM}, // OOMKilled 优先于退出码
	}
	for _, c := range cases {
		if got := classifyExit(c.code, c.oom); got != c.want {
			t.Errorf("classifyExit(%d, %v) = %v，期望 %v", c.code, c.oom, got, c.want)
		}
	}
}

// round86 缺陷 2：request container-functions <名> stop → docker stop 退出码 137/143，
// 属「已停止」而非异常退出：不 Raise，且清掉既有告警（此前报 critical「异常退出」）。
func TestCheckContainerAlarmsStoppedNotAlarmed(t *testing.T) {
	for _, code := range []int{137, 143} {
		m := newMockDocker()
		p := NewProvider(DefaultConfig(), m)
		sink := &fakeSink{}
		p.SetAlarms(sink)
		// 预置一条（例如上次检查的残留）活动告警
		sink.seed(orchestrator.RecoveryScopeContainer, orchestrator.ContainerExited, "walk-ct")
		m.states["walk-ct"] = orchestrator.CTStateExited
		m.exitCodes["walk-ct"] = code
		cfg := model.Config{ContainerFunctions: []model.ContainerFunction{{Name: "walk-ct", Image: "alpine:3.20"}}}
		if errs := p.CheckContainerAlarms(context.Background(), cfg); len(errs) != 0 {
			t.Fatalf("巡检不应报错: %v", errs)
		}
		if len(sink.raised) != 0 {
			t.Fatalf("主动停止（退出码 %d）不应告警: %v", code, sink.raised)
		}
		if len(sink.active) != 0 || len(sink.resolved) != 1 {
			t.Fatalf("主动停止应消警: active=%+v resolved=%v", sink.active, sink.resolved)
		}
	}
}

// OOMKilled=true（即使退出码是 137）→ critical，文案指向内存超限而非「异常退出」。
func TestCheckContainerAlarmsOOMKilled(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	sink := &fakeSink{}
	p.SetAlarms(sink)
	m.states["ct1"] = orchestrator.CTStateExited
	m.exitCodes["ct1"] = 137
	m.oomKilled["ct1"] = true
	cfg := model.Config{ContainerFunctions: []model.ContainerFunction{ctFixture("ct1")}}
	if errs := p.CheckContainerAlarms(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("巡检不应报错: %v", errs)
	}
	if len(sink.details) != 1 {
		t.Fatalf("OOM 应告警: %+v", sink.details)
	}
	d := sink.details[0]
	if d.severity != orchestrator.SeverityCritical || d.code != orchestrator.ContainerExited || d.source != "ct1" {
		t.Fatalf("OOM 告警级别/码/源不符: %+v", d)
	}
	if !strings.Contains(d.message, "内存超限") || strings.Contains(d.message, "异常退出") {
		t.Fatalf("OOM 告警文案应指向内存超限: %q", d.message)
	}
}

// round86 缺陷 1：容器从配置删除后其活动告警（异常退出/未收敛）必须被对账清掉。
func TestCheckContainerAlarmsResolvesRemovedContainer(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	sink := &fakeSink{}
	p.SetAlarms(sink)
	sink.seed(orchestrator.RecoveryScopeContainer, orchestrator.ContainerExited, "walk-ct")
	sink.seed(orchestrator.RecoveryScopeContainer, orchestrator.RecoveryUnconverged, "walk-ct")
	cfg := model.Config{} // 配置中已无该容器
	if errs := p.CheckContainerAlarms(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("巡检不应报错: %v", errs)
	}
	if len(sink.active) != 0 {
		t.Fatalf("已删除容器的告警应被清掉: %+v", sink.active)
	}
}

// 状态查询失败：报错且**不**清警（运行态未知时清警会掩盖真实故障）。
func TestCheckContainerAlarmsQueryFailureKeepsAlarm(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	sink := &fakeSink{}
	p.SetAlarms(sink)
	sink.seed(orchestrator.RecoveryScopeContainer, orchestrator.ContainerExited, "walk-ct")
	m.states["ct1"] = orchestrator.CTStateRunning
	m.stateErr = fmt.Errorf("docker down")
	cfg := model.Config{ContainerFunctions: []model.ContainerFunction{ctFixture("ct1")}}
	errs := p.CheckContainerAlarms(context.Background(), cfg)
	if len(errs) != 1 {
		t.Fatalf("查询失败应上报错误: %v", errs)
	}
	if len(sink.active) != 1 || len(sink.resolved) != 0 {
		t.Fatalf("查询失败不应清警: active=%+v resolved=%v", sink.active, sink.resolved)
	}
}

// 退出原因查询失败：该容器保持既有告警（不清也不重复报），错误照常上报。
func TestCheckContainerAlarmsExitQueryFailureKeepsAlarm(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	sink := &fakeSink{}
	p.SetAlarms(sink)
	sink.seed(orchestrator.RecoveryScopeContainer, orchestrator.ContainerExited, "ct1")
	m.states["ct1"] = orchestrator.CTStateExited
	m.exitCodes["ct1"] = 137
	m.oomErr = fmt.Errorf("docker down")
	cfg := model.Config{ContainerFunctions: []model.ContainerFunction{ctFixture("ct1")}}
	errs := p.CheckContainerAlarms(context.Background(), cfg)
	if len(errs) != 1 {
		t.Fatalf("查询失败应上报错误: %v", errs)
	}
	if len(sink.active) != 1 || len(sink.resolved) != 0 {
		t.Fatalf("退出原因未知时不应动告警: active=%+v resolved=%v", sink.active, sink.resolved)
	}
}

// 非零且非停止码（如 3）仍然是异常退出 → critical。
func TestCheckContainerAlarmsNonZeroStillCritical(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	sink := &fakeSink{}
	p.SetAlarms(sink)
	m.states["ct1"] = orchestrator.CTStateExited
	m.exitCodes["ct1"] = 3
	cfg := model.Config{ContainerFunctions: []model.ContainerFunction{ctFixture("ct1")}}
	if errs := p.CheckContainerAlarms(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("巡检不应报错: %v", errs)
	}
	if len(sink.details) != 1 || sink.details[0].severity != orchestrator.SeverityCritical {
		t.Fatalf("非零退出应 critical: %+v", sink.details)
	}
	if !strings.Contains(sink.details[0].message, "退出码 3") {
		t.Fatalf("告警文案应带退出码: %q", sink.details[0].message)
	}
}
