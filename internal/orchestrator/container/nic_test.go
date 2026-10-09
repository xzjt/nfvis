package container

// 决策 #441：容器侧 vNIC 接入生命周期胶水的单测（假钩子 + 既有 mockDocker/假告警表）。
//
// 覆盖面：钩子未注入（VPP 数据面）⇒ 零调用；start/restart/stop/delete 的接入次序与失败如实上报；
// PID 无效（0/缺失）不猜不接；EnsureConsistent 的运行中补接；15s 巡检的补接/清残留与告警建消。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// nicAttachCall 一次 Attach 的完整参数（断言 owner/ifaces/pid）。
type nicAttachCall struct {
	owner  string
	ifaces []model.VnfInterface
	pid    int
}

// fakeNIC 记录容器侧接入/清理的假钩子（可注入失败）。
type fakeNIC struct {
	mu        sync.Mutex
	order     []string // "attach:<owner>" / "delete:<owner>"，供次序断言
	attaches  []nicAttachCall
	deletes   []string
	attachErr error
	deleteErr error
}

func (f *fakeNIC) Attach(_ context.Context, owner string, ifaces []model.VnfInterface, pid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, "attach:"+owner)
	f.attaches = append(f.attaches, nicAttachCall{owner, append([]model.VnfInterface(nil), ifaces...), pid})
	return f.attachErr
}

func (f *fakeNIC) Delete(_ context.Context, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, "delete:"+owner)
	f.deletes = append(f.deletes, owner)
	return f.deleteErr
}

// ① 钩子未注入（VPP 数据面）⇒ 全部生命周期与巡检路径零接入调用（memif 路径逐字不变）。
func TestNICHookNilKeepsVPCPath(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	hook := &fakeNIC{}     // 存在但**不**注入
	ct := ctFixture("ct1") // 带 memif vNIC
	cfg := model.Config{ContainerFunctions: []model.ContainerFunction{ct}}
	ctx := context.Background()

	if err := p.ApplyContainer(ctx, ct); err != nil {
		t.Fatalf("ApplyContainer: %v", err)
	}
	if err := p.StartContainer(ctx, "ct1"); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	if err := p.StopContainer(ctx, "ct1"); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
	if err := p.RestartContainer(ctx, "ct1"); err != nil {
		t.Fatalf("RestartContainer: %v", err)
	}
	if err := p.DeleteContainer(ctx, "ct1"); err != nil {
		t.Fatalf("DeleteContainer: %v", err)
	}
	if errs := p.EnsureConsistent(ctx, cfg); len(errs) != 0 {
		t.Fatalf("EnsureConsistent: %v", errs)
	}
	if errs := p.CheckContainerNICs(ctx, cfg); len(errs) != 0 {
		t.Fatalf("CheckContainerNICs: %v", errs)
	}
	if len(hook.order) != 0 {
		t.Fatalf("钩子未注入时不得有任何接入调用: %v", hook.order)
	}
}

// ② start 成功 + 声明 vNIC ⇒ Attach 带对的 owner/ifaces/pid（pid 取自 inspect）。
func TestStartContainerAttachesNICs(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	hook := &fakeNIC{}
	p.SetNICHook(hook)
	ct := ctFixture("ct1")
	p.SetConfigSource(func() (model.Config, error) {
		return model.Config{ContainerFunctions: []model.ContainerFunction{ct}}, nil
	})
	m.states["ct1"] = orchestrator.CTStateExited
	m.pids["ct1"] = 4242

	if err := p.StartContainer(context.Background(), "ct1"); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	if len(hook.attaches) != 1 {
		t.Fatalf("应恰好接入一次: %v", hook.order)
	}
	got := hook.attaches[0]
	if got.owner != "ct1" || got.pid != 4242 {
		t.Fatalf("owner/pid 不符: %+v", got)
	}
	if len(got.ifaces) != 1 || got.ifaces[0].Name != "eth0" || got.ifaces[0].VirtualSwitch != "vs1" {
		t.Fatalf("ifaces 应为该容器声明的 vNIC: %+v", got.ifaces)
	}

	// 配置里没有该容器（陈旧/带外容器）⇒ 无可接入声明，空操作不报错。
	m.states["ct9"] = orchestrator.CTStateExited
	if err := p.StartContainer(context.Background(), "ct9"); err != nil {
		t.Fatalf("配置外的容器应空操作: %v", err)
	}
	if len(hook.attaches) != 1 {
		t.Fatalf("配置外的容器不得接入: %v", hook.order)
	}
}

// ③ PID 无效（0/缺失）⇒ 如实报错且不 Attach（不猜一个不存在的 netns）。
func TestStartContainerAttachPIDInvalid(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	hook := &fakeNIC{}
	p.SetNICHook(hook)
	ct := ctFixture("ct1")
	p.SetConfigSource(func() (model.Config, error) {
		return model.Config{ContainerFunctions: []model.ContainerFunction{ct}}, nil
	})
	m.states["ct1"] = orchestrator.CTStateExited // pids 缺省 0

	err := p.StartContainer(context.Background(), "ct1")
	if err == nil {
		t.Fatal("PID 无效应如实报错（容器已启动但未接入）")
	}
	if !strings.Contains(err.Error(), "已启动，但容器 vNIC 接入失败") || !strings.Contains(err.Error(), "PID 无效") {
		t.Fatalf("报错应点名「已启动但接入失败」与 PID 无效: %v", err)
	}
	if len(hook.attaches) != 0 {
		t.Fatalf("PID 无效时不得 Attach: %v", hook.order)
	}
}

// ④a 停容器**不删宿主端**（宿主端随声明存在）：容器端随 netns 销毁自然消失，宿主端留给
// 下一次 start 幂等重接（消除 start 落在两轮巡检之间的 ≤15s 断网窗口，也不与网络侧
// 「按声明确保宿主端」的巡检互相建/删抖动）。
func TestStopContainerKeepsHostEnd(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	hook := &fakeNIC{}
	p.SetNICHook(hook)
	ct := ctFixture("ct1")
	p.SetConfigSource(func() (model.Config, error) {
		return model.Config{ContainerFunctions: []model.ContainerFunction{ct}}, nil
	})
	ctx := context.Background()

	m.states["ct1"] = orchestrator.CTStateRunning
	if err := p.StopContainer(ctx, "ct1"); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
	if len(hook.order) != 0 {
		t.Fatalf("stop 不应清宿主端（宿主端随声明存在）: %v", hook.order)
	}
	// 重复 stop（已停幂等）同样不动宿主端。
	if err := p.StopContainer(ctx, "ct1"); err != nil {
		t.Fatalf("重复 stop: %v", err)
	}
	if len(hook.order) != 0 {
		t.Fatalf("重复 stop 也不应清宿主端: %v", hook.order)
	}
	// 停后再 start：宿主端保留，Attach 幂等地把重建的容器端移回 netns。
	m.pids["ct1"] = 88
	if err := p.StartContainer(ctx, "ct1"); err != nil {
		t.Fatalf("停后 start 应重接: %v", err)
	}
	if len(hook.order) != 1 || hook.order[0] != "attach:ct1" || hook.attaches[0].pid != 88 {
		t.Fatalf("停后 start 应只重接、不删宿主端: %v", hook.order)
	}
}

// ④b delete 清宿主端（幂等）：容器在 ⇒ 删后清；容器已不在 ⇒ 仍清一次残留（幂等）；
// 清理失败如实上报（容器已删，不能报成功）。
func TestDeleteContainerDetachesNICs(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	hook := &fakeNIC{}
	p.SetNICHook(hook)
	ctx := context.Background()

	// 容器在（已停）⇒ 删除后清宿主端。
	m.states["ct1"] = orchestrator.CTStateExited
	if err := p.DeleteContainer(ctx, "ct1"); err != nil {
		t.Fatalf("DeleteContainer: %v", err)
	}
	if len(hook.deletes) != 1 || hook.deletes[0] != "ct1" {
		t.Fatalf("delete 应清宿主端: %v", hook.order)
	}
	// 容器已不在 ⇒ 仍清一次残留（幂等）。
	if err := p.DeleteContainer(ctx, "ct1"); err != nil {
		t.Fatalf("容器已不在时 delete 应幂等: %v", err)
	}
	if len(hook.deletes) != 2 {
		t.Fatalf("容器已不在时也应清残留（幂等）: %v", hook.order)
	}

	// 清理失败如实上报（容器已经删掉，不能报成功）。
	hook.deleteErr = errors.New("netlink 繁忙")
	if err := p.DeleteContainer(ctx, "ct1"); err == nil || !strings.Contains(err.Error(), "宿主端 vNIC") {
		t.Fatalf("清理失败应如实报错: %v", err)
	}
}

// ⑤ restart：先删旧宿主端、再按声明重新接入（netns 重建后必须重接）。
func TestRestartContainerDetachesThenAttaches(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	hook := &fakeNIC{}
	p.SetNICHook(hook)
	ct := ctFixture("ct1")
	p.SetConfigSource(func() (model.Config, error) {
		return model.Config{ContainerFunctions: []model.ContainerFunction{ct}}, nil
	})
	m.states["ct1"] = orchestrator.CTStateRunning
	m.pids["ct1"] = 77

	if err := p.RestartContainer(context.Background(), "ct1"); err != nil {
		t.Fatalf("RestartContainer: %v", err)
	}
	want := []string{"delete:ct1", "attach:ct1"}
	if len(hook.order) != 2 || hook.order[0] != want[0] || hook.order[1] != want[1] {
		t.Fatalf("restart 应「先删后接」: got %v want %v", hook.order, want)
	}
	if hook.attaches[0].pid != 77 {
		t.Fatalf("重接应带重建后 netns 的 PID: %+v", hook.attaches[0])
	}
}

// ApplyContainer 的 autostart 启动同样是一次 start：netns 刚建好即接入（钩子为 nil 时空操作）。
func TestApplyContainerAutostartAttaches(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	hook := &fakeNIC{}
	p.SetNICHook(hook)
	m.pids["ct1"] = 5150
	ct := ctFixture("ct1") // Autostart=true，带 memif vNIC

	if err := p.ApplyContainer(context.Background(), ct); err != nil {
		t.Fatalf("ApplyContainer: %v", err)
	}
	if len(hook.attaches) != 1 || hook.attaches[0].pid != 5150 {
		t.Fatalf("autostart 启动后应接入: %v", hook.order)
	}
}

// ⑥⑦ 巡检：运行中缺接入 ⇒ 补（失败建告警、恢复消警）；已停 ⇒ 清残留；无 vNIC/钩子未注入 ⇒ 空操作。
func TestCheckContainerNICs(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	hook := &fakeNIC{}
	p.SetNICHook(hook)
	sink := &fakeSink{}
	p.SetAlarms(sink)
	ct := ctFixture("ct1")
	cfg := model.Config{ContainerFunctions: []model.ContainerFunction{ct}}
	ctx := context.Background()

	// 运行中 + 补接失败 ⇒ 错误 + 未收敛告警（同族：scope recovery、warning、source container-nics）。
	m.states["ct1"] = orchestrator.CTStateRunning
	m.pids["ct1"] = 99
	hook.attachErr = errors.New("宿主端缺失")
	errs := p.CheckContainerNICs(ctx, cfg)
	if len(errs) != 1 {
		t.Fatalf("补接失败应如实进未收敛项: %v", errs)
	}
	if len(sink.raised) != 1 || sink.raised[0] != "container-nics" {
		t.Fatalf("应建 container-nics 告警: %v", sink.raised)
	}
	if d := sink.details[0]; d.scope != "recovery" || d.severity != orchestrator.SeverityWarning ||
		d.code != orchestrator.RecoveryUnconverged {
		t.Fatalf("告警口径应为恢复收敛同族: %+v", d)
	}

	// 下一轮补接成功 ⇒ 自动消警（幂等重试）。
	hook.attachErr = nil
	if errs := p.CheckContainerNICs(ctx, cfg); len(errs) != 0 {
		t.Fatalf("恢复后不应有未收敛项: %v", errs)
	}
	if got := sink.resolvedOf(orchestrator.RecoveryUnconverged); len(got) != 1 || got[0] != "container-nics" {
		t.Fatalf("恢复后应消警: %v", sink.resolveCalls)
	}

	// 已停（容器对象还在、只是没跑）⇒ **不动宿主端**（宿主端随声明存在；与网络侧
	//「按声明确保宿主端」的巡检不互相建/删抖动）。
	m.states["ct1"] = orchestrator.CTStateExited
	before := len(hook.order)
	if errs := p.CheckContainerNICs(ctx, cfg); len(errs) != 0 {
		t.Fatalf("已停容器不应报错: %v", errs)
	}
	if len(hook.order) != before {
		t.Fatalf("已停容器不应清宿主端: %v", hook.order)
	}

	// 容器已不存在（inspect 报 not found）⇒ 清宿主端残留（幂等）。
	delete(m.states, "ct1")
	if errs := p.CheckContainerNICs(ctx, cfg); len(errs) != 0 {
		t.Fatalf("清残留不应报错: %v", errs)
	}
	if len(hook.deletes) != 1 || hook.deletes[0] != "ct1" {
		t.Fatalf("已不存在的容器应清宿主端残留: order=%v", hook.order)
	}

	// 无 vNIC 声明 ⇒ 空操作；钩子未注入（VPP）⇒ 空操作。
	plain := model.Config{ContainerFunctions: []model.ContainerFunction{{Name: "plain", Image: "img"}}}
	if errs := p.CheckContainerNICs(ctx, plain); len(errs) != 0 {
		t.Fatalf("无 vNIC 应空操作: %v", errs)
	}
	p2 := NewProvider(DefaultConfig(), m)
	if errs := p2.CheckContainerNICs(ctx, cfg); len(errs) != 0 {
		t.Fatalf("钩子未注入应空操作: %v", errs)
	}
}

// EnsureConsistent 重放：运行中容器补一次幂等接入（宿主端被带外删掉/半接入都能自愈）。
func TestEnsureConsistentAttachesRunningNICs(t *testing.T) {
	m := newMockDocker()
	p := NewProvider(DefaultConfig(), m)
	hook := &fakeNIC{}
	p.SetNICHook(hook)
	ct := ctFixture("ct1")
	cfg := model.Config{ContainerFunctions: []model.ContainerFunction{ct}}

	// 已运行：ApplyContainer 幂等早退，重放只做补接。
	m.states["ct1"] = orchestrator.CTStateRunning
	m.pids["ct1"] = 31
	if errs := p.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("EnsureConsistent: %v", errs)
	}
	if len(hook.attaches) != 1 || hook.attaches[0].pid != 31 {
		t.Fatalf("运行中容器应补接一次: %v", hook.order)
	}

	// 未运行（无 autostart 现场）：不接（没有可移入的 netns）——此处用无 autostart 的声明对照。
	m2 := newMockDocker()
	p2 := NewProvider(DefaultConfig(), m2)
	hook2 := &fakeNIC{}
	p2.SetNICHook(hook2)
	ct2 := ctFixture("ct2")
	ct2.Autostart = false
	m2.states["ct2"] = orchestrator.CTStateExited
	if errs := p2.EnsureConsistent(context.Background(), model.Config{ContainerFunctions: []model.ContainerFunction{ct2}}); len(errs) != 0 {
		t.Fatalf("EnsureConsistent(未运行): %v", errs)
	}
	if len(hook2.order) != 0 {
		t.Fatalf("未运行容器不得接入: %v", hook2.order)
	}
}
