package main

// 动态持有层（决策 #351）的单元守护：
//   1. 未接入语义表逐法——静默成功/空结果/错误文案（与 internal/api nil 分支逐字对齐）；
//   2. 换装后全部方法转发到真实实现（注入假件，断言参数与返回值原样穿透）；
//   3. Connected() 前后变化与连接记账的关闭。
//
// 文案断言用的是与 internal/api nil 分支**相同的字符串**（api 侧常量未导出：
// errComputeUnavailable = "%% " + 正文 + "\n"，此处锁正文逐字一致）。

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/compute"
	"github.com/xzjt/nfvis/internal/orchestrator/container"
)

// 与 internal/api nil 分支逐字相同的正文（改动任一侧本测试即红，防止漂移）。
const (
	wantComputeBody   = "计算编排未接入（libvirt 未装配），运行态不可用"
	wantConsoleBody   = "串口 console 不可用（libvirt 未装配）"
	wantContainerBody = "容器编排未接入（Docker 未装配），运行态不可用"
)

// nopRWC 空 io.ReadWriteCloser（转发断言用）。
type nopRWC struct{ io.ReadWriteCloser }

// fakeCompute 记录调用的 computeFacade 假件。
type fakeCompute struct {
	mu           sync.Mutex
	defined      []model.VMFunction
	deletedVM    []string
	started      []string
	stopped      []string
	restarted    []string
	startChecked []string
	refreshed    []model.VMFunction
	states       []string
	consoled     []string
	snapCreated  [][3]string
	snapListed   []string
	snapReverted [][2]string
	snapDeleted  [][2]string
	ensured      []model.Config
	alarmRuns    []model.Config

	stateVal   string
	stateErr   error
	consoleRWC io.ReadWriteCloser
	consoleErr error
	snapErr    error
	snapInfos  []compute.SnapshotInfo
}

func (f *fakeCompute) DefineVM(_ context.Context, vm model.VMFunction, _ model.AllocatedResources) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defined = append(f.defined, vm)
	return nil
}

func (f *fakeCompute) DeleteVM(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedVM = append(f.deletedVM, name)
	return nil
}

func (f *fakeCompute) StartVM(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, name)
	return nil
}

func (f *fakeCompute) StopVM(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, name)
	return nil
}

func (f *fakeCompute) RestartVM(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarted = append(f.restarted, name)
	return nil
}

func (f *fakeCompute) StartVMChecked(ctx context.Context, name string) (orchestrator.VMStartProbe, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startChecked = append(f.startChecked, name)
	return orchestrator.VMStartProbe{OK: true, State: orchestrator.VMStateRunning}, nil
}

func (f *fakeCompute) RefreshSeed(_ context.Context, vm model.VMFunction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshed = append(f.refreshed, vm)
	return nil
}

func (f *fakeCompute) VMState(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states = append(f.states, name)
	return f.stateVal, f.stateErr
}

func (f *fakeCompute) Console(_ context.Context, name string) (io.ReadWriteCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.consoled = append(f.consoled, name)
	return f.consoleRWC, f.consoleErr
}

func (f *fakeCompute) SnapshotCreate(_ context.Context, domain, name, description string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapCreated = append(f.snapCreated, [3]string{domain, name, description})
	return f.snapErr
}

func (f *fakeCompute) Snapshots(_ context.Context, domain string) ([]compute.SnapshotInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapListed = append(f.snapListed, domain)
	return f.snapInfos, f.snapErr
}

func (f *fakeCompute) SnapshotRevert(_ context.Context, domain, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapReverted = append(f.snapReverted, [2]string{domain, name})
	return f.snapErr
}

func (f *fakeCompute) SnapshotDelete(_ context.Context, domain, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapDeleted = append(f.snapDeleted, [2]string{domain, name})
	return f.snapErr
}

func (f *fakeCompute) EnsureConsistent(_ context.Context, cfg model.Config) []error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured = append(f.ensured, cfg)
	return nil
}

func (f *fakeCompute) CheckVMAlarms(_ context.Context, cfg model.Config) []error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alarmRuns = append(f.alarmRuns, cfg)
	return nil
}

// fakeContainer 记录调用的 containerFacade 假件。
type fakeContainer struct {
	mu        sync.Mutex
	applied   []model.ContainerFunction
	deleted   []string
	started   []string
	stopped   []string
	restarted []string
	states    []string
	logs      []string
	ensured   []model.Config
	alarmRuns []model.Config

	stateVal string
	stateErr error
	logsVal  string
	logsErr  error
}

func (f *fakeContainer) ApplyContainer(_ context.Context, ct model.ContainerFunction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, ct)
	return nil
}

func (f *fakeContainer) DeleteContainer(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, name)
	return nil
}

func (f *fakeContainer) StartContainer(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, name)
	return nil
}

func (f *fakeContainer) StopContainer(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, name)
	return nil
}

func (f *fakeContainer) RestartContainer(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarted = append(f.restarted, name)
	return nil
}

func (f *fakeContainer) ContainerState(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states = append(f.states, name)
	return f.stateVal, f.stateErr
}

func (f *fakeContainer) ContainerLogs(_ context.Context, name string, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, name)
	return f.logsVal, f.logsErr
}
func (f *fakeContainer) ContainerExec(_ context.Context, name, command string, _ time.Duration) (container.ExecResult, error) {
	return container.ExecResult{}, nil
}

func (f *fakeContainer) ContainerShell(_ context.Context, name string) (io.ReadWriteCloser, error) {
	return nil, nil
}

func (f *fakeContainer) EnsureConsistent(_ context.Context, cfg model.Config) []error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured = append(f.ensured, cfg)
	return nil
}

func (f *fakeContainer) CheckContainerAlarms(_ context.Context, cfg model.Config) []error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alarmRuns = append(f.alarmRuns, cfg)
	return nil
}

func TestDynamicComputeDegradedSemantics(t *testing.T) {
	h := newDynamicCompute()
	if h.Connected() {
		t.Fatal("零值持有层应处于未接入态")
	}
	ctx := context.Background()
	vm := model.VMFunction{Name: "vm-a"}
	alloc := model.AllocatedResources{}

	// 静默成功：提交路径在降级期照常工作。
	if err := h.DefineVM(ctx, vm, alloc); err != nil {
		t.Fatalf("未接入 DefineVM 应静默成功: %v", err)
	}
	if err := h.DeleteVM(ctx, "vm-a"); err != nil {
		t.Fatalf("未接入 DeleteVM 应静默成功: %v", err)
	}
	// 空结果：巡检/恢复收敛不产生噪声。
	if errs := h.EnsureConsistent(ctx, model.Config{}); len(errs) != 0 {
		t.Fatalf("未接入 EnsureConsistent 应为空: %v", errs)
	}
	if errs := h.CheckVMAlarms(ctx, model.Config{}); len(errs) != 0 {
		t.Fatalf("未接入 CheckVMAlarms 应为空: %v", errs)
	}
	// 生命周期/seed/启动回读：与 nil 分支同文案的错误。
	for name, fn := range map[string]func() error{
		"StartVM":   func() error { return h.StartVM(ctx, "vm-a") },
		"StopVM":    func() error { return h.StopVM(ctx, "vm-a") },
		"RestartVM": func() error { return h.RestartVM(ctx, "vm-a") },
		"RefreshSeed": func() error {
			return h.RefreshSeed(ctx, vm)
		},
	} {
		if err := fn(); err == nil || err.Error() != wantComputeBody {
			t.Fatalf("未接入 %s 应报与 nil 分支同文案的错误: %v", name, err)
		}
	}
	if _, err := h.StartVMChecked(ctx, "vm-a"); err == nil || err.Error() != wantComputeBody {
		t.Fatalf("未接入 StartVMChecked 应报同文案错误: %v", err)
	}
	// VMState：错误（show 路径经既有错误分支渲染「-」）。
	if st, err := h.VMState(ctx, "vm-a"); err == nil || err.Error() != wantComputeBody || st != "" {
		t.Fatalf("未接入 VMState 应报同文案错误: state=%q err=%v", st, err)
	}
	// Console：独立文案。
	if rwc, err := h.Console(ctx, "vm-a"); err == nil || err.Error() != wantConsoleBody || rwc != nil {
		t.Fatalf("未接入 Console 应报 console 文案错误: rwc=%v err=%v", rwc, err)
	}
	// 快照 4 法：与快照 nil 分支（errComputeUnavailable 正文）同文案。
	if err := h.SnapshotCreate(ctx, "vm-a", "s1", ""); err == nil || err.Error() != wantComputeBody {
		t.Fatalf("未接入 SnapshotCreate 应报同文案错误: %v", err)
	}
	if rows, err := h.Snapshots(ctx, "vm-a"); err == nil || err.Error() != wantComputeBody || rows != nil {
		t.Fatalf("未接入 Snapshots 应报同文案错误: rows=%v err=%v", rows, err)
	}
	if err := h.SnapshotRevert(ctx, "vm-a", "s1"); err == nil || err.Error() != wantComputeBody {
		t.Fatalf("未接入 SnapshotRevert 应报同文案错误: %v", err)
	}
	if err := h.SnapshotDelete(ctx, "vm-a", "s1"); err == nil || err.Error() != wantComputeBody {
		t.Fatalf("未接入 SnapshotDelete 应报同文案错误: %v", err)
	}
}

func TestDynamicComputeForwardsAfterSwap(t *testing.T) {
	h := newDynamicCompute()
	fake := &fakeCompute{stateVal: orchestrator.VMStateRunning, consoleRWC: nopRWC{}}
	h.Swap(fake, nil)
	if !h.Connected() {
		t.Fatal("Swap 后应处于已接入态")
	}
	ctx := context.Background()
	vm := model.VMFunction{Name: "vm-a"}

	if err := h.DefineVM(ctx, vm, model.AllocatedResources{}); err != nil {
		t.Fatalf("DefineVM 转发失败: %v", err)
	}
	if err := h.DeleteVM(ctx, "vm-a"); err != nil {
		t.Fatalf("DeleteVM 转发失败: %v", err)
	}
	if err := h.StartVM(ctx, "vm-a"); err != nil {
		t.Fatalf("StartVM 转发失败: %v", err)
	}
	if err := h.StopVM(ctx, "vm-a"); err != nil {
		t.Fatalf("StopVM 转发失败: %v", err)
	}
	if err := h.RestartVM(ctx, "vm-a"); err != nil {
		t.Fatalf("RestartVM 转发失败: %v", err)
	}
	if probe, err := h.StartVMChecked(ctx, "vm-a"); err != nil || !probe.OK {
		t.Fatalf("StartVMChecked 转发失败: probe=%+v err=%v", probe, err)
	}
	if err := h.RefreshSeed(ctx, vm); err != nil {
		t.Fatalf("RefreshSeed 转发失败: %v", err)
	}
	if st, err := h.VMState(ctx, "vm-a"); err != nil || st != orchestrator.VMStateRunning {
		t.Fatalf("VMState 转发失败: state=%q err=%v", st, err)
	}
	rwc, err := h.Console(ctx, "vm-a")
	if err != nil || rwc == nil {
		t.Fatalf("Console 转发失败: rwc=%v err=%v", rwc, err)
	}
	fake.snapErr = errors.New("libvirt boom")
	if err := h.SnapshotCreate(ctx, "vm-a", "s1", "d"); err == nil || err.Error() != "libvirt boom" {
		t.Fatalf("SnapshotCreate 应原样穿透假件错误: %v", err)
	}
	fake.snapErr = nil
	fake.snapInfos = []compute.SnapshotInfo{{Name: "s1"}}
	if rows, err := h.Snapshots(ctx, "vm-a"); err != nil || len(rows) != 1 || rows[0].Name != "s1" {
		t.Fatalf("Snapshots 转发失败: rows=%v err=%v", rows, err)
	}
	if err := h.SnapshotRevert(ctx, "vm-a", "s1"); err != nil {
		t.Fatalf("SnapshotRevert 转发失败: %v", err)
	}
	if err := h.SnapshotDelete(ctx, "vm-a", "s1"); err != nil {
		t.Fatalf("SnapshotDelete 转发失败: %v", err)
	}
	if errs := h.EnsureConsistent(ctx, model.Config{}); len(errs) != 0 {
		t.Fatalf("EnsureConsistent 转发失败: %v", errs)
	}
	if errs := h.CheckVMAlarms(ctx, model.Config{}); len(errs) != 0 {
		t.Fatalf("CheckVMAlarms 转发失败: %v", errs)
	}
	// 参数确实到达了真实实现（抽两条核对）。
	if len(fake.startChecked) != 1 || fake.startChecked[0] != "vm-a" {
		t.Fatalf("StartVMChecked 参数未到达实现: %v", fake.startChecked)
	}
	if len(fake.consoled) != 1 || fake.consoled[0] != "vm-a" {
		t.Fatalf("Console 参数未到达实现: %v", fake.consoled)
	}
}

// TestDynamicComputeProbe 探活转发（决策 #354）：未接入 ⇒ 与 nil 分支同文案的错误（防御性，
// 探活只在已接入态被调用）；换装后经既有廉价 RPC VMState(保留域名) 判定，错误原样穿透。
// 方法选择缘由见 dynamic_provider.go 的 Probe 注释（Provider 无 Conn.Version() 透传）。
func TestDynamicComputeProbe(t *testing.T) {
	ctx := context.Background()
	h := newDynamicCompute()
	if err := h.Probe(ctx); err == nil || err.Error() != wantComputeBody {
		t.Fatalf("未接入 Probe 应报同文案错误: %v", err)
	}
	fake := &fakeCompute{}
	h.Swap(fake, nil)
	if err := h.Probe(ctx); err != nil {
		t.Fatalf("已接入 Probe 应成功: %v", err)
	}
	if len(fake.states) != 1 || fake.states[0] != computeProbeVMName {
		t.Fatalf("探活应经保留域名调用 VMState: %v", fake.states)
	}
	fake.stateErr = errors.New("connection closed")
	if err := h.Probe(ctx); err == nil || err.Error() != "connection closed" {
		t.Fatalf("探活失败应原样穿透假件错误: %v", err)
	}
}

func TestDynamicComputeCloseClosesTrackedConn(t *testing.T) {
	h := newDynamicCompute()
	if err := h.Close(); err != nil {
		t.Fatalf("未接入时 Close 应为空操作: %v", err)
	}
	// 零值 *compute.Conn 的 Close 幂等返回 nil（conn_libvirt.go 对 l==nil 有守卫），
	// 用它验证记账关闭路径不 panic、可重复调用。
	h.Swap(&fakeCompute{}, &compute.Conn{})
	h.Swap(&fakeCompute{}, nil) // 覆盖换装：旧连接应被防御性关闭，不 panic
	if err := h.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close 应可重复调用: %v", err)
	}
	if !h.Connected() {
		t.Fatal("Close 只关连接记账，不撤销已接入的实现")
	}
}

// blockingCloser 测试用的「会阻塞的 Close」（决策 #378/D2）：Close 进入即通知 entered，
// 直到 release 关闭才返回。用于验证换装/关闭在锁外进行——旧实现持写锁关闭会阻塞计算调用。
type blockingCloser struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingCloser() *blockingCloser {
	return &blockingCloser{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (b *blockingCloser) Close() error {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return nil
}

// releaseAll 幂等放行（t.Cleanup 兜底，保证测试不挂死）。
func (b *blockingCloser) releaseAll() { b.once.Do(func() { close(b.release) }) }

// TestDynamicComputeSwapClosesOldConnOutsideLock 决策 #378/D2 红-绿：Swap 在锁内完成换装、
// 锁外关闭被替换的旧连接。旧连接 Close 阻塞期间，计算调用（Connected/VMState）必须立即可用，
// 且新实现已可见（换装原子性）——旧实现在写锁内 Close，这些调用会全部阻塞。
func TestDynamicComputeSwapClosesOldConnOutsideLock(t *testing.T) {
	h := newDynamicCompute()
	old := newBlockingCloser()
	defer old.releaseAll()
	h.Swap(&fakeCompute{}, old) // 换入旧连接（此时无旧连接可关）

	swapDone := make(chan struct{})
	go func() {
		h.Swap(&fakeCompute{stateVal: orchestrator.VMStateRunning}, nil)
		close(swapDone)
	}()

	// 等 Swap 走到锁外、旧连接的 Close 已开始阻塞。
	select {
	case <-old.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Swap 未开始关闭旧连接")
	}

	// 此刻旧连接 Close 仍阻塞：换装应已可见（新实现）。
	connected := make(chan bool, 1)
	go func() { connected <- h.Connected() }()
	select {
	case ok := <-connected:
		if !ok {
			t.Fatal("Swap 期间应已可见新实现（换装原子性）")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Swap 期间 Connected 被阻塞（旧实现持写锁关闭连接）")
	}

	// 另一条计算调用（读锁路径）同样应立即返回。
	stateCh := make(chan error, 1)
	go func() {
		_, err := h.VMState(context.Background(), "vm-a")
		stateCh <- err
	}()
	select {
	case err := <-stateCh:
		if err != nil {
			t.Fatalf("Swap 期间 VMState 应转发成功: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Swap 期间 VMState 被阻塞（旧实现持写锁关闭连接）")
	}

	old.releaseAll()
	select {
	case <-swapDone:
	case <-time.After(2 * time.Second):
		t.Fatal("放行后 Swap 未返回")
	}
}

// TestDynamicComputeCloseClosesConnOutsideLock 决策 #378/D2 红-绿：Close 先在锁内取走 conn
// 并置空、锁外关闭。阻塞的 Close 期间：计算调用不被阻塞、且并发 Close 幂等立即返回 nil。
func TestDynamicComputeCloseClosesConnOutsideLock(t *testing.T) {
	h := newDynamicCompute()
	old := newBlockingCloser()
	defer old.releaseAll()
	h.Swap(&fakeCompute{}, old)

	closeDone := make(chan error, 1)
	go func() { closeDone <- h.Close() }()

	select {
	case <-old.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Close 未开始关闭连接")
	}

	// Close 阻塞在旧连接的 Close 上：Connected 不应被阻塞。
	connected := make(chan bool, 1)
	go func() { connected <- h.Connected() }()
	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("Close 期间 Connected 被阻塞（旧实现持写锁关闭连接）")
	}

	// 幂等：conn 已在锁内置空，并发 Close 应立即返回 nil（不被前一次的阻塞关闭挡住）。
	idem := make(chan error, 1)
	go func() { idem <- h.Close() }()
	select {
	case err := <-idem:
		if err != nil {
			t.Fatalf("并发 Close 应幂等返回 nil: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close 不幂等（第二次调用被第一次的阻塞关闭挡住）")
	}

	old.releaseAll()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close 返回错误: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("放行后 Close 未返回")
	}
}

func TestContainerHolderDegradedSemantics(t *testing.T) {
	h := newContainerHolder()
	if h.Connected() {
		t.Fatal("零值持有层应处于未接入态")
	}
	ctx := context.Background()
	ct := model.ContainerFunction{Name: "ct-a"}

	// 静默成功：noopContainer 现状即如此（提交路径照常）。
	if err := h.ApplyContainer(ctx, ct); err != nil {
		t.Fatalf("未接入 ApplyContainer 应静默成功: %v", err)
	}
	if err := h.DeleteContainer(ctx, "ct-a"); err != nil {
		t.Fatalf("未接入 DeleteContainer 应静默成功: %v", err)
	}
	// 空结果。
	if errs := h.EnsureConsistent(ctx, model.Config{}); len(errs) != 0 {
		t.Fatalf("未接入 EnsureConsistent 应为空: %v", errs)
	}
	if errs := h.CheckContainerAlarms(ctx, model.Config{}); len(errs) != 0 {
		t.Fatalf("未接入 CheckContainerAlarms 应为空: %v", errs)
	}
	// 生命周期/日志：与 nil 分支同文案的错误。
	for name, fn := range map[string]func() error{
		"StartContainer":   func() error { return h.StartContainer(ctx, "ct-a") },
		"StopContainer":    func() error { return h.StopContainer(ctx, "ct-a") },
		"RestartContainer": func() error { return h.RestartContainer(ctx, "ct-a") },
	} {
		if err := fn(); err == nil || err.Error() != wantContainerBody {
			t.Fatalf("未接入 %s 应报与 nil 分支同文案的错误: %v", name, err)
		}
	}
	if out, err := h.ContainerLogs(ctx, "ct-a", 100); err == nil || err.Error() != wantContainerBody || out != "" {
		t.Fatalf("未接入 ContainerLogs 应报同文案错误: out=%q err=%v", out, err)
	}
	// ContainerState：错误（ctStateOf 经既有错误分支渲染「-」）。
	if st, err := h.ContainerState(ctx, "ct-a"); err == nil || err.Error() != wantContainerBody || st != "" {
		t.Fatalf("未接入 ContainerState 应报同文案错误: state=%q err=%v", st, err)
	}
	// 决策 #375（R142 B8）：未接入错误可被判为「底座不可用」——REST 层据此把容器 exec/shell
	// 的 500 如实映射为 503（Docker 启动时即不可达的场景），且文案不变（CLI 逐字打印）。
	if _, err := h.ContainerExec(ctx, "ct-a", "echo x", time.Second); !errors.Is(err, orchestrator.ErrContainerUnavailable) {
		t.Fatalf("未接入错误应可判为 ErrContainerUnavailable: %v", err)
	}
}

func TestContainerHolderForwardsAfterSwap(t *testing.T) {
	h := newContainerHolder()
	fake := &fakeContainer{stateVal: orchestrator.CTStateRunning, logsVal: "log line"}
	h.Swap(fake)
	if !h.Connected() {
		t.Fatal("Swap 后应处于已接入态")
	}
	ctx := context.Background()

	if err := h.ApplyContainer(ctx, model.ContainerFunction{Name: "ct-a"}); err != nil {
		t.Fatalf("ApplyContainer 转发失败: %v", err)
	}
	if err := h.DeleteContainer(ctx, "ct-a"); err != nil {
		t.Fatalf("DeleteContainer 转发失败: %v", err)
	}
	if err := h.StartContainer(ctx, "ct-a"); err != nil {
		t.Fatalf("StartContainer 转发失败: %v", err)
	}
	if err := h.StopContainer(ctx, "ct-a"); err != nil {
		t.Fatalf("StopContainer 转发失败: %v", err)
	}
	if err := h.RestartContainer(ctx, "ct-a"); err != nil {
		t.Fatalf("RestartContainer 转发失败: %v", err)
	}
	if st, err := h.ContainerState(ctx, "ct-a"); err != nil || st != orchestrator.CTStateRunning {
		t.Fatalf("ContainerState 转发失败: state=%q err=%v", st, err)
	}
	if out, err := h.ContainerLogs(ctx, "ct-a", 50); err != nil || out != "log line" {
		t.Fatalf("ContainerLogs 转发失败: out=%q err=%v", out, err)
	}
	if errs := h.EnsureConsistent(ctx, model.Config{}); len(errs) != 0 {
		t.Fatalf("EnsureConsistent 转发失败: %v", errs)
	}
	if errs := h.CheckContainerAlarms(ctx, model.Config{}); len(errs) != 0 {
		t.Fatalf("CheckContainerAlarms 转发失败: %v", errs)
	}
	if len(fake.started) != 1 || fake.started[0] != "ct-a" {
		t.Fatalf("StartContainer 参数未到达实现: %v", fake.started)
	}
}

// TestUnavailableErrorTextsMatchAPINilBranches 锁三段正文逐字（含全角括号与空格）——
// 与 internal/api 的 nil 分支常量对齐，任一侧改动本测试即红。
func TestUnavailableErrorTextsMatchAPINilBranches(t *testing.T) {
	if errComputeNotConnectedText != wantComputeBody ||
		!strings.HasPrefix(errComputeNotConnectedText, "计算编排未接入") ||
		strings.ContainsAny(errComputeNotConnectedText, "()\n") {
		t.Fatalf("compute 正文与 api nil 分支不对齐: %q", errComputeNotConnectedText)
	}
	if errConsoleNotConnectedText != wantConsoleBody {
		t.Fatalf("console 正文与 api nil 分支不对齐: %q", errConsoleNotConnectedText)
	}
	if errContainerNotConnectedText != wantContainerBody {
		t.Fatalf("container 正文与 api nil 分支不对齐: %q", errContainerNotConnectedText)
	}
}
