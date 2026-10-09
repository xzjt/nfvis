package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

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

	// 决策 #432：inspect 事实注入——Docker 原始状态（restarting 等）与重启次数。
	// rawStates 缺省回落到契约状态同名（restarting 场景需显式注入）；restartUnknown 里的
	// 容器模拟「应答未给 RestartCount」（取不到 ⇒ 读视图省略该字段）。
	rawStates      map[string]string
	restartCounts  map[string]int
	restartUnknown map[string]bool
	inspects       []string // inspect 调用记账（断言一次读数只 inspect 一次）

	// 决策 #357：容器内执行命令的记账与结果注入。
	execCmd     string
	execTimeout time.Duration
	execResult  ExecResult
	execErr     error

	// 决策 #358：交互式终端（返回的流与注入错误）。
	shell    io.ReadWriteCloser
	shellErr error
}

func newMockDocker() *mockDocker {
	return &mockDocker{states: map[string]string{}, specs: map[string]CreateSpec{}, exitCodes: map[string]int{},
		oomKilled: map[string]bool{}, rawStates: map[string]string{}, restartCounts: map[string]int{},
		restartUnknown: map[string]bool{}}
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

// Inspect 单次 inspect 的容器事实（决策 #432）：状态 + 原始状态 + 重启次数。
func (m *mockDocker) Inspect(_ context.Context, name string) (ContainerFacts, bool, error) {
	m.inspects = append(m.inspects, name)
	if m.stateErr != nil {
		return ContainerFacts{}, false, m.stateErr
	}
	s, ok := m.states[name]
	if !ok {
		return ContainerFacts{}, false, nil
	}
	raw := m.rawStates[name]
	if raw == "" {
		raw = s // 缺省：原始状态与契约状态同名（restarting 需显式注入）
	}
	f := ContainerFacts{State: s, RawState: raw}
	if !m.restartUnknown[name] {
		f.RestartCount, f.RestartsKnown = m.restartCounts[name], true
	}
	return f, true, nil
}
func (m *mockDocker) Logs(_ context.Context, name string, tail int) (string, error) {
	return m.logs, nil
}

func (m *mockDocker) Exec(_ context.Context, name, command string, timeout time.Duration) (ExecResult, error) {
	m.calls = append(m.calls, "exec:"+name)
	m.execCmd, m.execTimeout = command, timeout
	return m.execResult, m.execErr
}

func (m *mockDocker) ExecShell(_ context.Context, name string) (io.ReadWriteCloser, error) {
	m.calls = append(m.calls, "shell:"+name)
	if m.shellErr != nil {
		return nil, m.shellErr
	}
	return m.shell, nil
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
	// resolveCalls Resolve 的逐次明细：断言按**码**取用——巡检每轮都会对本轮「无此告警」的码
	// 幂等调一次 Resolve，按总次数断言的既有用例会被新增告警码带偏（决策 #432）。
	resolveCalls []sinkAlarm
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
	f.resolveCalls = append(f.resolveCalls, sinkAlarm{scope: scope, code: code, source: source})
	for i, a := range f.active {
		if a.scope == scope && a.code == code && a.source == source {
			f.active = append(f.active[:i], f.active[i+1:]...)
			return true
		}
	}
	return false
}

// resolvedOf 按告警码取 Resolve 的 source 序列（按码断言，新增码不打乱既有计数）。
func (f *fakeSink) resolvedOf(code string) []string {
	var out []string
	for _, c := range f.resolveCalls {
		if c.code == code {
			out = append(out, c.source)
		}
	}
	return out
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
	if len(sink.resolvedOf(orchestrator.ContainerExited)) != 1 {
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
		if len(sink.active) != 0 || len(sink.resolvedOf(orchestrator.ContainerExited)) != 1 {
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

// 决策 #357：容器内执行命令的前置判定与透传。
func TestProviderContainerExec(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(Config{Socket: "/tmp/none.sock"}, m)
	ctx := context.Background()

	// ① 不存在 ⇒ ErrVMNotFound（API 映射 404）
	if _, err := p.ContainerExec(ctx, "ghost", "echo hi", time.Second); !errors.Is(err, orchestrator.ErrVMNotFound) {
		t.Fatalf("不存在应报 ErrVMNotFound，得 %v", err)
	}

	// ② 非运行态 ⇒ ErrContainerNotRunning（API 映射 409）；**不得**下发到 Docker
	m.states["ct-a"] = orchestrator.CTStateExited
	if _, err := p.ContainerExec(ctx, "ct-a", "echo hi", time.Second); !errors.Is(err, orchestrator.ErrContainerNotRunning) {
		t.Fatalf("非运行态应报 ErrContainerNotRunning，得 %v", err)
	}
	for _, c := range m.calls {
		if c == "exec:ct-a" {
			t.Fatalf("非运行态不得下发 exec（调用记录 %v）", m.calls)
		}
	}

	// ③ 空命令 ⇒ 拒绝（不猜）
	m.states["ct-a"] = orchestrator.CTStateRunning
	if _, err := p.ContainerExec(ctx, "ct-a", "   ", time.Second); err == nil {
		t.Fatal("空命令应被拒")
	}

	// ④ 运行态 ⇒ 透传命令/超时与结果
	m.execResult = ExecResult{ExitCode: 3, HasExitCode: true, Stdout: "ok\n"}
	got, err := p.ContainerExec(ctx, "ct-a", "echo ok", 7*time.Second)
	if err != nil {
		t.Fatalf("运行态执行: %v", err)
	}
	if m.execCmd != "echo ok" || m.execTimeout != 7*time.Second {
		t.Fatalf("命令/超时未透传: %q %v", m.execCmd, m.execTimeout)
	}
	if !got.HasExitCode || got.ExitCode != 3 || got.Stdout != "ok\n" {
		t.Fatalf("结果未透传: %+v", got)
	}

	// ⑤ 超时结果如实透传（HasExitCode=false ⇒ 渲染层不得报 0）
	m.execResult = ExecResult{Stdout: "partial", TimedOut: true}
	got, err = p.ContainerExec(ctx, "ct-a", "sleep 60", time.Second)
	if err != nil || !got.TimedOut || got.HasExitCode {
		t.Fatalf("超时应如实透传（TimedOut 且无退出码）: %+v err=%v", got, err)
	}

	// ⑥ 默认超时：<=0 时回落 30s（不把 0 透传给底座）
	m.execResult = ExecResult{ExitCode: 0, HasExitCode: true}
	if _, err := p.ContainerExec(ctx, "ct-a", "true", 0); err != nil {
		t.Fatalf("默认超时执行: %v", err)
	}
	if m.execTimeout != 30*time.Second {
		t.Fatalf("timeout<=0 应回落 30s，得 %v", m.execTimeout)
	}
}

// ---------- 决策 #432：重启次数读数 + 崩溃重启循环告警 ----------

// 读视图数据源：状态与重启次数**同一次 inspect** 取回；取不到时 restartKnown=false
// （调用方据此省略字段，不回落为 0）。
func TestContainerStatusRestartCount(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	m.states["ct1"] = orchestrator.CTStateRunning
	m.restartCounts["ct1"] = 7

	// ① 有值：状态与次数一起给出。
	st, n, known, err := p.ContainerStatus(context.Background(), "ct1")
	if err != nil || st != orchestrator.CTStateRunning || !known || n != 7 {
		t.Fatalf("读数应为 running/7/known，得 %q %d %v %v", st, n, known, err)
	}

	// ② 契约状态把 restarting 并入 running（规格 #44 不改），但原始状态必须可判（巡检用）。
	m.rawStates["ct1"] = "restarting"
	if st, _, _, _ := p.ContainerStatus(context.Background(), "ct1"); st != orchestrator.CTStateRunning {
		t.Fatalf("restarting 仍应映射为 running（规格 #44 不改）: %q", st)
	}

	// ③ 取不到：应答没有 RestartCount ⇒ known=false（不得冒充 0 次）。
	m.restartUnknown["ct1"] = true
	if st, n, known, err = p.ContainerStatus(context.Background(), "ct1"); err != nil ||
		st != orchestrator.CTStateRunning || known || n != 0 {
		t.Fatalf("取不到应为 known=false（不编造 0）: %q %d %v %v", st, n, known, err)
	}

	// ④ 容器不存在 ⇒ absent 且无错误、次数取不到。
	if st, n, known, err = p.ContainerStatus(context.Background(), "ghost"); err != nil ||
		st != orchestrator.CTStateAbsent || known || n != 0 {
		t.Fatalf("不存在应为 absent/known=false: %q %d %v %v", st, n, known, err)
	}

	// ⑤ 底座不可达 ⇒ 归一为 ErrContainerUnavailable（API 503），不是「取不到 0 次」。
	m.stateErr = fmt.Errorf("docker down")
	if _, _, known, err = p.ContainerStatus(context.Background(), "ct1"); !errors.Is(err, orchestrator.ErrContainerUnavailable) || known {
		t.Fatalf("底座不可达应报 ErrContainerUnavailable 且次数未知: %v known=%v", err, known)
	}

	// ⑥ ContainerState 与 ContainerStatus 同源（一次读数只 inspect 一次）。
	m.stateErr = nil
	m.restartUnknown["ct1"] = false
	before := len(m.inspects)
	if _, err := p.ContainerState(context.Background(), "ct1"); err != nil {
		t.Fatal(err)
	}
	if len(m.inspects) != before+1 {
		t.Fatalf("单次读数应只 inspect 一次，实际 %v", m.inspects[before:])
	}
}

// 崩溃重启循环：`restarting` 且重启次数**较上次巡检有增长**、**累计 ≥3** 才报
// （warning `CONTAINER_RESTART_LOOP`）；转为非 restarting 即消警。
func TestCheckContainerAlarmsRestartLoop(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	sink := &fakeSink{}
	p.SetAlarms(sink)
	cfg := model.Config{ContainerFunctions: []model.ContainerFunction{ctFixture("ct1")}}
	ctx := context.Background()

	// 首次巡检：契约状态 running（restarting 并入）、原始状态 restarting——无上一轮读数可比 ⇒ 不报。
	m.states["ct1"] = orchestrator.CTStateRunning
	m.rawStates["ct1"] = "restarting"
	m.restartCounts["ct1"] = 1
	if errs := p.CheckContainerAlarms(ctx, cfg); len(errs) != 0 {
		t.Fatalf("巡检不应报错: %v", errs)
	}
	if len(sink.details) != 0 {
		t.Fatalf("首轮无增长可比 ⇒ 不报: %+v", sink.details)
	}
	// 一次巡检一次 inspect（不为重启次数新增第二次查询）。
	if len(m.inspects) != 1 {
		t.Fatalf("每轮每容器应只 inspect 一次，实际 %v", m.inspects)
	}

	// 有增长但累计 <3 ⇒ 不报。
	m.restartCounts["ct1"] = 2
	p.CheckContainerAlarms(ctx, cfg)
	if len(sink.details) != 0 {
		t.Fatalf("累计 2 次不应报: %+v", sink.details)
	}

	// 有增长且累计 ≥3 ⇒ warning 报出。
	m.restartCounts["ct1"] = 3
	p.CheckContainerAlarms(ctx, cfg)
	if len(sink.details) != 1 {
		t.Fatalf("累计 3 次且有增长应报: %+v", sink.details)
	}
	d := sink.details[0]
	if d.severity != orchestrator.SeverityWarning || d.code != orchestrator.ContainerRestartLoop || d.source != "ct1" {
		t.Fatalf("告警级别/码/源不符: %+v", d)
	}
	for _, want := range []string{"ct1", "3", "request container-functions ct1 log", "docker inspect ct1"} {
		if !strings.Contains(d.message, want) {
			t.Errorf("告警文案应含 %q: %q", want, d.message)
		}
	}

	// 仍在 restarting 但本次无增长 ⇒ 告警**保持活动**（每轮 Raise 是幂等更新：AlarmStore 对
	// 同 code+source 的活动告警只刷新 message/severity、保留 RaisedAt；fakeSink 的 active 去重
	// 同口径），且不得被消解。注意断言的是 active（不是 Raise 次数——判据不再要求增长，故每轮都会 Raise）。
	m.restartCounts["ct1"] = 3
	p.CheckContainerAlarms(ctx, cfg)
	if len(sink.active) != 1 || len(sink.resolvedOf(orchestrator.ContainerRestartLoop)) != 0 {
		t.Fatalf("无增长应保持既有告警: active=%v resolved=%v", sink.active, sink.resolved)
	}

	// 转为 running ⇒ 自动消解。
	delete(m.rawStates, "ct1")
	p.CheckContainerAlarms(ctx, cfg)
	if len(sink.active) != 0 {
		t.Fatalf("转 running 应消警: %+v", sink.active)
	}
}

// 次数不增长**也报**（Docker 重启指数退避 ⇒ 两轮间未必涨）；次数低于门槛不报；计数回退（删除重建）后重新起算。
func TestCheckContainerAlarmsRestartLoopNoGrowthStillReported(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	sink := &fakeSink{}
	p.SetAlarms(sink)
	cfg := model.Config{ContainerFunctions: []model.ContainerFunction{ctFixture("ct1")}}
	ctx := context.Background()
	m.states["ct1"] = orchestrator.CTStateRunning
	m.rawStates["ct1"] = "restarting"
	m.restartCounts["ct1"] = 5

	// 两轮读数相同（次数 ≥3、两轮之间没涨）⇒ **仍应报**：Docker 重启有指数退避，成熟循环
	// 40~60s 才涨一次而巡检 15s 一轮，「两轮都在涨」几乎不可达（真机实测：restart-count 15、
	// 告警空）。restarting 本身即「没在服务、Docker 反复拉起」，次数门槛足够。
	p.CheckContainerAlarms(ctx, cfg)
	p.CheckContainerAlarms(ctx, cfg)
	if len(sink.active) != 1 || sink.active[0].code != orchestrator.ContainerRestartLoop {
		t.Fatalf("restarting 且累计 ≥3 应报（不要求两轮增长）: %+v", sink.active)
	}

	// 次数低于门槛（偶发一次自动重启）⇒ 不报（用全新 provider/sink：上面的告警已活动，
	// 「次数变低」不会把它消掉——活动告警只在容器不再 restarting 或对象消失时消解）。
	m2 := newMockDocker()
	p2 := NewProvider(DefaultConfig(), m2)
	sink2 := &fakeSink{}
	p2.SetAlarms(sink2)
	m2.states["ct1"] = orchestrator.CTStateRunning
	m2.rawStates["ct1"] = "restarting"
	m2.restartCounts["ct1"] = 2
	p2.CheckContainerAlarms(ctx, cfg)
	if len(sink2.active) != 0 {
		t.Fatalf("累计低于门槛不应报: %+v", sink2.active)
	}

	// 计数回退 = 容器删除重建（Docker 计数从 0 起算）⇒ 按新值重新起算，不因旧计数大而永远追不上。
	m2.restartCounts["ct1"] = 0
	p2.CheckContainerAlarms(ctx, cfg)
	if len(sink2.active) != 0 {
		t.Fatalf("重建后计数 0 不应报: %+v", sink2.active)
	}
	m2.restartCounts["ct1"] = 3
	p2.CheckContainerAlarms(ctx, cfg)
	if len(sink2.active) != 1 || sink2.active[0].code != orchestrator.ContainerRestartLoop {
		t.Fatalf("重建后重新起算应能报出: %+v", sink2.active)
	}
}

// 对象从配置删除 ⇒ 崩溃重启循环告警随对账消解（沿用既有清警口径）。
func TestCheckContainerAlarmsRestartLoopResolvesRemovedContainer(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	sink := &fakeSink{}
	p.SetAlarms(sink)
	sink.seed(orchestrator.RecoveryScopeContainer, orchestrator.ContainerRestartLoop, "walk-ct")
	m.states["walk-ct"] = orchestrator.CTStateRunning
	m.rawStates["walk-ct"] = "restarting"
	m.restartCounts["walk-ct"] = 9
	cfg := model.Config{ContainerFunctions: []model.ContainerFunction{{Name: "walk-ct", Image: "alpine:3.20"}}}

	// 仍在循环（次数已记 9）：对象还在配置里 ⇒ 告警保留。
	p.CheckContainerAlarms(context.Background(), cfg)
	if len(sink.active) != 1 {
		t.Fatalf("对象仍在配置里应保留告警: %+v", sink.active)
	}

	// 从配置删除 ⇒ 对账清警，且计数记忆一并丢弃。
	p.CheckContainerAlarms(context.Background(), model.Config{})
	if len(sink.active) != 0 {
		t.Fatalf("已删除对象的告警应被清掉: %+v", sink.active)
	}
	if _, seen := p.restartSeenOf("walk-ct"); seen {
		t.Fatal("计数记忆应随对象消失一并丢弃（有界）")
	}
}
