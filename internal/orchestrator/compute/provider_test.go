package compute

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// ---------- mock 底座 ----------

type mockLibvirt struct {
	present      map[string]bool
	states       map[string]int
	defined      []string
	undefined    []string
	started      []string
	shutdown     []string
	destroyed    []string
	rebooted     []string
	defineErr    error
	startErr     error
	shutdownNoop bool // 模拟 ACPI 关机无响应（触发超时强杀）

	domainXML string
	autostart map[string]bool
	snaps     map[string][]string
	snapXML   map[string]string
	reverted  []string
	deleted   []string
}

func newMockLibvirt() *mockLibvirt {
	return &mockLibvirt{present: map[string]bool{}, states: map[string]int{},
		snaps: map[string][]string{}, snapXML: map[string]string{}, autostart: map[string]bool{}}
}

func xmlName(xml string) string {
	const open, close = "<name>", "</name>"
	i := strings.Index(xml, open)
	if i < 0 {
		return ""
	}
	rest := xml[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func (m *mockLibvirt) Define(_ context.Context, xml string) error {
	if m.defineErr != nil {
		return m.defineErr
	}
	name := xmlName(xml)
	m.defined = append(m.defined, name)
	m.present[name] = true
	if _, ok := m.states[name]; !ok {
		m.states[name] = domShutoff
	}
	return nil
}
func (m *mockLibvirt) Undefine(_ context.Context, name string) error {
	m.undefined = append(m.undefined, name)
	m.present[name] = false
	return nil
}
func (m *mockLibvirt) State(_ context.Context, name string) (int, bool, error) {
	if !m.present[name] {
		return 0, false, nil
	}
	return m.states[name], true, nil
}
func (m *mockLibvirt) Start(_ context.Context, name string) error {
	if m.startErr != nil {
		return m.startErr
	}
	m.started = append(m.started, name)
	m.states[name] = domRunning
	return nil
}
func (m *mockLibvirt) Shutdown(_ context.Context, name string) error {
	m.shutdown = append(m.shutdown, name)
	if !m.shutdownNoop {
		m.states[name] = domShutoff
	}
	return nil
}
func (m *mockLibvirt) Destroy(_ context.Context, name string) error {
	m.destroyed = append(m.destroyed, name)
	m.states[name] = domShutoff
	return nil
}
func (m *mockLibvirt) Reboot(_ context.Context, name string) error {
	m.rebooted = append(m.rebooted, name)
	m.states[name] = domRunning
	return nil
}

func (m *mockLibvirt) SetAutostart(_ context.Context, name string, autostart bool) error {
	m.autostart[name] = autostart
	return nil
}

// DumpDomainXML / 快照：内存实现（单测验证 Provider 快照编排与磁盘集合）。
func (m *mockLibvirt) DumpDomainXML(_ context.Context, name string) (string, error) {
	if !m.present[name] {
		return "", fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, name)
	}
	return m.domainXML, nil
}

func (m *mockLibvirt) SnapshotCreate(_ context.Context, domain, snapshotXML string) error {
	if !m.present[domain] {
		return fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, domain)
	}
	name := xmlName(snapshotXML)
	m.snaps[domain] = append(m.snaps[domain], name)
	m.snapXML[domain+"/"+name] = snapshotXML
	return nil
}

func (m *mockLibvirt) SnapshotList(_ context.Context, domain string) ([]SnapshotInfo, error) {
	if !m.present[domain] {
		return nil, fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, domain)
	}
	out := make([]SnapshotInfo, 0, len(m.snaps[domain]))
	for _, n := range m.snaps[domain] {
		out = append(out, SnapshotInfo{Name: n})
	}
	return out, nil
}

func (m *mockLibvirt) SnapshotRevert(_ context.Context, domain, snapshot string) error {
	m.reverted = append(m.reverted, domain+"/"+snapshot)
	return nil
}

func (m *mockLibvirt) SnapshotDelete(_ context.Context, domain, snapshot string) error {
	m.deleted = append(m.deleted, domain+"/"+snapshot)
	return nil
}

// OpenConsole 返回一对内存管道（单测验证 Provider.Console 的状态前置条件）。
func (m *mockLibvirt) OpenConsole(_ context.Context, name string) (io.ReadWriteCloser, error) {
	if !m.present[name] {
		return nil, fmt.Errorf("%w: %s", orchestrator.ErrVMNotFound, name)
	}
	return nopConsole{}, nil
}

type nopConsole struct{}

func (nopConsole) Read([]byte) (int, error)    { return 0, io.EOF }
func (nopConsole) Write(p []byte) (int, error) { return len(p), nil }
func (nopConsole) Close() error                { return nil }

type mockStorage struct {
	files   map[string]bool
	dirs    []string
	clones  []string
	blanks  []string
	removed []string
}

func newMockStorage(files ...string) *mockStorage {
	s := &mockStorage{files: map[string]bool{}}
	for _, f := range files {
		s.files[f] = true
	}
	return s
}
func (s *mockStorage) EnsureDir(p string) error { s.dirs = append(s.dirs, p); return nil }
func (s *mockStorage) Exists(p string) bool     { return s.files[p] }
func (s *mockStorage) Clone(_ context.Context, image, disk string) error {
	if !s.files[image] {
		return fmt.Errorf("backing 镜像不存在: %s", image)
	}
	s.clones = append(s.clones, image+"->"+disk)
	s.files[disk] = true
	return nil
}
func (s *mockStorage) CreateBlank(_ context.Context, disk string, sizeGB int) error {
	s.blanks = append(s.blanks, fmt.Sprintf("%s:%dG", disk, sizeGB))
	s.files[disk] = true
	return nil
}
func (s *mockStorage) RemoveAll(p string) error {
	s.removed = append(s.removed, p)
	for f := range s.files {
		if strings.HasPrefix(f, p+"/") {
			delete(s.files, f)
		}
	}
	return nil
}

type mockSeed struct{ built []string }

func (s *mockSeed) Build(_ context.Context, _ model.VMFunction, iso string) error {
	s.built = append(s.built, iso)
	return nil
}

// ---------- 测试装配 ----------

func testConfig() Config {
	c := DefaultConfig()
	c.VMsDir = "/vms"
	c.VhostDir = "/run/vhost"
	c.ImagesDir = "/images"
	c.StopTimeout = 50 * time.Millisecond
	return c
}

func newTestProvider(api libvirtAPI, store storageAPI, seed seedBuilder) *Provider {
	p := NewProvider(testConfig(), api, store, seed)
	p.poll = time.Millisecond
	p.sleep = func(context.Context, time.Duration) error { return nil }
	return p
}

func vmFixture(name string) model.VMFunction {
	return model.VMFunction{
		Name:   name,
		Image:  "img.qcow2",
		VCPU:   model.VMCpu{Count: 2},
		Memory: model.VMMemory{SizeMB: 1024, HugepageSize: "1G"},
	}
}

// ---------- DefineVM ----------

func TestDefineVM_PreparesDiskSeedAndDefines(t *testing.T) {
	api := newMockLibvirt()
	store := newMockStorage("/images/img.qcow2")
	seed := &mockSeed{}
	p := newTestProvider(api, store, seed)

	vm := vmFixture("fw-vm")
	vm.Interfaces = []model.VnfInterface{{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs1"}}
	vm.CloudInit = &model.CloudInit{Hostname: "fw"}
	vm.Disks = []model.VMDisk{{Name: "data0", SizeGB: 8}}

	if err := p.DefineVM(context.Background(), vm, model.AllocatedResources{Cores: []int{6, 7}, HugepageSize: "1G"}); err != nil {
		t.Fatalf("DefineVM: %v", err)
	}
	if len(api.defined) != 1 || api.defined[0] != "fw-vm" {
		t.Fatalf("应定义 fw-vm: %v", api.defined)
	}
	if len(store.clones) != 1 || store.clones[0] != "/images/img.qcow2->/vms/fw-vm/disk.qcow2" {
		t.Fatalf("应从镜像克隆主盘: %v", store.clones)
	}
	if len(store.blanks) != 1 || store.blanks[0] != "/vms/fw-vm/data-data0.qcow2:8G" {
		t.Fatalf("应创建 8G 空数据盘: %v", store.blanks)
	}
	if len(seed.built) != 1 || seed.built[0] != "/vms/fw-vm/seed.iso" {
		t.Fatalf("应生成 seed ISO: %v", seed.built)
	}
	// vhost-user vNIC 缺省 2 对队列（真机握手必需，见 DefaultVhostUserQueues 注释）。
	if spec, err := p.specFor(vm, model.AllocatedResources{HugepageSize: "1G"}); err != nil {
		t.Fatal(err)
	} else if len(spec.Interfaces) != 1 || spec.Interfaces[0].Queues != DefaultVhostUserQueues {
		t.Fatalf("vhost-user 应缺省 %d 对队列: %+v", DefaultVhostUserQueues, spec.Interfaces)
	}
}

func TestDefineVM_AutostartStartsShutoffVM(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProvider(api, newMockStorage("/images/img.qcow2"), nil)
	vm := vmFixture("fw-vm")
	vm.Autostart = true

	if err := p.DefineVM(context.Background(), vm, model.AllocatedResources{HugepageSize: "1G"}); err != nil {
		t.Fatalf("DefineVM: %v", err)
	}
	if len(api.started) != 1 || api.started[0] != "fw-vm" {
		t.Fatalf("autostart 应启动: %v", api.started)
	}
	// 幂等：再次 Define 不应重复启动。
	if err := p.DefineVM(context.Background(), vm, model.AllocatedResources{HugepageSize: "1G"}); err != nil {
		t.Fatalf("重复 DefineVM: %v", err)
	}
	if len(api.started) != 1 {
		t.Fatalf("已运行不应重复启动: %v", api.started)
	}
}

func TestDefineVM_MissingImageErrors(t *testing.T) {
	p := newTestProvider(newMockLibvirt(), newMockStorage(), nil)
	err := p.DefineVM(context.Background(), vmFixture("fw-vm"), model.AllocatedResources{HugepageSize: "1G"})
	if err == nil || !strings.Contains(err.Error(), "不在仓库中") {
		t.Fatalf("镜像缺失应明确报错: %v", err)
	}
}

func TestDefineVM_ISOImageSkipsClone(t *testing.T) {
	api := newMockLibvirt()
	store := newMockStorage()
	p := newTestProvider(api, store, nil)
	vm := vmFixture("iso-vm")
	vm.Image = "installer.iso"

	if err := p.DefineVM(context.Background(), vm, model.AllocatedResources{HugepageSize: "1G"}); err != nil {
		t.Fatalf("ISO 镜像不应要求克隆: %v", err)
	}
	if len(store.clones) != 0 {
		t.Fatalf("ISO 不应克隆: %v", store.clones)
	}
}

func TestDefineVM_CloudInitWithoutSeedBuilderErrors(t *testing.T) {
	vm := vmFixture("fw-vm")
	vm.CloudInit = &model.CloudInit{Hostname: "fw"}
	p := newTestProvider(newMockLibvirt(), newMockStorage("/images/img.qcow2"), nil)
	err := p.DefineVM(context.Background(), vm, model.AllocatedResources{HugepageSize: "1G"})
	if err == nil || !strings.Contains(err.Error(), "cloud-init") {
		t.Fatalf("seed 未启用应明确报错: %v", err)
	}
}

func TestDefineVM_SriovWithoutResolverErrors(t *testing.T) {
	vm := vmFixture("fw-vm")
	vm.Interfaces = []model.VnfInterface{{
		Name: "eth1", Type: "sriov-vf",
		Sriov: &model.SriovBind{PhysicalInterface: "ens192", VFID: 1},
	}}
	p := newTestProvider(newMockLibvirt(), newMockStorage("/images/img.qcow2"), nil)
	err := p.DefineVM(context.Background(), vm, model.AllocatedResources{HugepageSize: "1G"})
	if err == nil || !strings.Contains(err.Error(), "SR-IOV") {
		t.Fatalf("无 VF 解析应明确报不支持: %v", err)
	}
}

func TestDefineVM_SriovWithResolver(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProvider(api, newMockStorage("/images/img.qcow2"), nil)
	p.SetVFResolver(func(pf string, vf int) (string, error) {
		if pf != "ens192" || vf != 1 {
			return "", fmt.Errorf("unexpected %s %d", pf, vf)
		}
		return "0000:0b:10.1", nil
	})
	vm := vmFixture("fw-vm")
	vm.Interfaces = []model.VnfInterface{{
		Name: "eth1", Type: "sriov-vf",
		Sriov: &model.SriovBind{PhysicalInterface: "ens192", VFID: 1},
	}}
	if err := p.DefineVM(context.Background(), vm, model.AllocatedResources{HugepageSize: "1G"}); err != nil {
		t.Fatalf("注入解析后应成功: %v", err)
	}
}

// ---------- DeleteVM ----------

func TestDeleteVM_CascadeActiveDomain(t *testing.T) {
	api := newMockLibvirt()
	store := newMockStorage("/images/img.qcow2", "/vms/fw-vm/disk.qcow2")
	p := newTestProvider(api, store, nil)
	vm := vmFixture("fw-vm")
	if err := p.DefineVM(context.Background(), vm, model.AllocatedResources{HugepageSize: "1G"}); err != nil {
		t.Fatal(err)
	}
	_ = api.Start(context.Background(), "fw-vm")

	if err := p.DeleteVM(context.Background(), "fw-vm"); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
	if len(api.destroyed) != 1 || len(api.undefined) != 1 {
		t.Fatalf("运行中应先强停再删除定义: destroyed=%v undefined=%v", api.destroyed, api.undefined)
	}
	if len(store.removed) != 1 || store.removed[0] != "/vms/fw-vm" {
		t.Fatalf("应清理 VM 目录: %v", store.removed)
	}
}

func TestDeleteVM_AbsentIsIdempotent(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProvider(api, newMockStorage(), nil)
	if err := p.DeleteVM(context.Background(), "ghost"); err != nil {
		t.Fatalf("删除不存在的 VM 应幂等: %v", err)
	}
	if len(api.undefined) != 0 || len(api.destroyed) != 0 {
		t.Fatalf("不应有 libvirt 操作: %v %v", api.undefined, api.destroyed)
	}
}

// ---------- 生命周期 ----------

func newRunningProvider(name string) (*Provider, *mockLibvirt, *mockStorage) {
	api := newMockLibvirt()
	store := newMockStorage("/images/img.qcow2")
	p := newTestProvider(api, store, nil)
	api.present[name] = true
	api.states[name] = domRunning
	return p, api, store
}

func TestStartVM_IdempotentAndNotFound(t *testing.T) {
	p, api, _ := newRunningProvider("fw-vm")
	if err := p.StartVM(context.Background(), "fw-vm"); err != nil {
		t.Fatalf("已运行应幂等成功: %v", err)
	}
	if len(api.started) != 0 {
		t.Fatalf("不应重复启动: %v", api.started)
	}
	if err := p.StartVM(context.Background(), "ghost"); err == nil {
		t.Fatal("未定义应报错")
	}
}

func TestStopVM_Graceful(t *testing.T) {
	p, api, _ := newRunningProvider("fw-vm")
	if err := p.StopVM(context.Background(), "fw-vm"); err != nil {
		t.Fatalf("StopVM: %v", err)
	}
	if len(api.shutdown) != 1 || len(api.destroyed) != 0 {
		t.Fatalf("优雅关机不应强杀: shutdown=%v destroyed=%v", api.shutdown, api.destroyed)
	}
}

func TestStopVM_TimeoutKills(t *testing.T) {
	p, api, _ := newRunningProvider("fw-vm")
	api.shutdownNoop = true // ACPI 无响应
	if err := p.StopVM(context.Background(), "fw-vm"); err != nil {
		t.Fatalf("StopVM: %v", err)
	}
	if len(api.destroyed) != 1 {
		t.Fatalf("超时应强杀: %v", api.destroyed)
	}
}

func TestStopVM_NotRunningIsNoop(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProvider(api, newMockStorage(), nil)
	api.present["fw-vm"] = true
	api.states["fw-vm"] = domShutoff
	if err := p.StopVM(context.Background(), "fw-vm"); err != nil {
		t.Fatal(err)
	}
	if len(api.shutdown) != 0 {
		t.Fatalf("已关机不应再关机: %v", api.shutdown)
	}
}

func TestRestartVM_RunningRebootsShutoffStarts(t *testing.T) {
	p, api, _ := newRunningProvider("fw-vm")
	if err := p.RestartVM(context.Background(), "fw-vm"); err != nil {
		t.Fatal(err)
	}
	if len(api.rebooted) != 1 {
		t.Fatalf("运行中应 ACPI 重启: %v", api.rebooted)
	}
	api.states["fw-vm"] = domShutoff
	if err := p.RestartVM(context.Background(), "fw-vm"); err != nil {
		t.Fatal(err)
	}
	if len(api.started) != 1 {
		t.Fatalf("关机态 restart 应启动: %v", api.started)
	}
}

func TestVMState(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProvider(api, newMockStorage(), nil)

	if st, err := p.VMState(context.Background(), "ghost"); err != nil || st != "absent" {
		t.Fatalf("未定义应为 absent: %q %v", st, err)
	}
	cases := map[int]string{
		domRunning: "running", domBlocked: "running",
		domPaused: "paused", domPMSuspended: "paused",
		domCrashed: "crashed", domShutoff: "shutoff", domShutdown: "shutoff",
	}
	for libState, want := range cases {
		api.present["fw-vm"] = true
		api.states["fw-vm"] = libState
		got, err := p.VMState(context.Background(), "fw-vm")
		if err != nil || got != want {
			t.Errorf("libvirt 状态 %d → %q，期望 %q（err=%v）", libState, got, want, err)
		}
	}
}

// ---------- 恢复收敛 ----------

func TestEnsureConsistent_DefinesAllAndCollectsErrors(t *testing.T) {
	api := newMockLibvirt()
	store := newMockStorage("/images/img.qcow2")
	p := newTestProvider(api, store, nil)

	cfg := model.Config{
		ResourcePools: &model.ResourcePool{
			Hugepages: []model.HPool{{PageSize: "1G", Count: 4}},
			CPU:       &model.CPUSetup{IsolatedCores: []int{0, 1, 2, 3}},
		},
		VirtualMachineFunctions: []model.VMFunction{vmFixture("fw-vm"), vmFixture("probe-vm")},
	}
	if errs := p.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("收敛应成功: %v", errs)
	}
	if len(api.defined) != 2 {
		t.Fatalf("应补建 2 台 VM: %v", api.defined)
	}

	// 第二台镜像缺失 → 收集为错误但不阻塞第一台（用全新存储，避免首轮已克隆的盘遮蔽）。
	p2 := newTestProvider(newMockLibvirt(), newMockStorage("/images/img.qcow2"), nil)
	cfg.VirtualMachineFunctions[1].Image = "missing.qcow2"
	errs := p2.EnsureConsistent(context.Background(), cfg)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "probe-vm") {
		t.Fatalf("应收集 probe-vm 的不可收敛错误: %v", errs)
	}
}

// fakeSink 记录恢复收敛告警（M4-9）。
type fakeSink struct {
	raised   []string
	resolved []string
}

func (f *fakeSink) Raise(_, _, _, _, source string) { f.raised = append(f.raised, source) }
func (f *fakeSink) Resolve(_, _, source string) bool {
	f.resolved = append(f.resolved, source)
	return true
}

func TestEnsureConsistentAlarms(t *testing.T) {
	api := newMockLibvirt()
	store := newMockStorage("/images/img.qcow2")
	p := newTestProvider(api, store, nil)
	sink := &fakeSink{}
	p.SetAlarms(sink)

	good := vmFixture("good-vm")
	bad := vmFixture("bad-vm")
	bad.Image = "missing.qcow2"
	cfg := model.Config{
		ResourcePools: &model.ResourcePool{
			Hugepages: []model.HPool{{PageSize: "1G", Count: 4}},
			CPU:       &model.CPUSetup{IsolatedCores: []int{0, 1, 2, 3}},
		},
		VirtualMachineFunctions: []model.VMFunction{good, bad},
	}
	errs := p.EnsureConsistent(context.Background(), cfg)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "bad-vm") {
		t.Fatalf("应只报 bad-vm: %v", errs)
	}
	if len(sink.raised) != 1 || sink.raised[0] != "bad-vm" {
		t.Fatalf("应上报 bad-vm 告警: %v", sink.raised)
	}
	if len(sink.resolved) != 1 || sink.resolved[0] != "good-vm" {
		t.Fatalf("应收敛 good-vm 告警: %v", sink.resolved)
	}
}

// FR-OPS-012：autostart 须落到 libvirt 域自启标志。
func TestDefineVMSetsLibvirtAutostart(t *testing.T) {
	api := newMockLibvirt()
	store := newMockStorage("/images/img.qcow2")
	p := newTestProvider(api, store, nil)
	vm := vmFixture("fw-vm")
	vm.Autostart = true
	if err := p.DefineVM(context.Background(), vm, model.AllocatedResources{HugepageSize: "1G"}); err != nil {
		t.Fatal(err)
	}
	if !api.autostart["fw-vm"] {
		t.Fatal("应按 autostart 设置 libvirt 自启标志")
	}
	vm.Autostart = false
	if err := p.DefineVM(context.Background(), vm, model.AllocatedResources{HugepageSize: "1G"}); err != nil {
		t.Fatal(err)
	}
	if api.autostart["fw-vm"] {
		t.Fatal("autostart=false 应清除自启标志")
	}
}
