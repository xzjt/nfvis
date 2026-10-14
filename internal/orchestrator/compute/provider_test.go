package compute

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	definedXML   []string // 每次 Define 收到的域 XML（决策 #455：断言重定义后的形态）
	undefined    []string
	started      []string
	shutdown     []string
	destroyed    []string
	rebooted     []string
	defineErr    error
	startErr     error
	shutdownNoop bool // 模拟 ACPI 关机无响应（触发超时强杀）
	// pauseOnStart 模拟 vhost-user 后端未就绪：Start 后域停在 paused(starting up)（决策 #311）。
	pauseOnStart bool
	// crashOnStart 模拟域启动后立刻退出（SHUTOFF+CRASHED）（决策 #311）。
	crashOnStart bool

	domainXML string
	autostart map[string]bool
	reasons   map[string]int
	snaps     map[string][]string
	snapXML   map[string]string
	reverted  []string
	deleted   []string
}

func newMockLibvirt() *mockLibvirt {
	return &mockLibvirt{present: map[string]bool{}, states: map[string]int{},
		snaps: map[string][]string{}, snapXML: map[string]string{}, autostart: map[string]bool{}, reasons: map[string]int{}}
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
	m.definedXML = append(m.definedXML, xml)
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
	state, _, exists, err := m.StateReason(context.Background(), name)
	return state, exists, err
}

func (m *mockLibvirt) StateReason(_ context.Context, name string) (int, int, bool, error) {
	if !m.present[name] {
		return 0, 0, false, nil
	}
	return m.states[name], m.reasons[name], true, nil
}
func (m *mockLibvirt) Start(_ context.Context, name string) error {
	if m.startErr != nil {
		return m.startErr
	}
	m.started = append(m.started, name)
	switch {
	case m.pauseOnStart:
		m.states[name] = domPaused
		m.reasons[name] = pausedReasonStartingUp
	case m.crashOnStart:
		m.states[name] = domShutoff
		m.reasons[name] = shutoffReasonCrashed
	default:
		m.states[name] = domRunning
	}
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

// OpenVNC 同 OpenConsole（单测验证 Provider.VNC 的状态前置条件）。
func (m *mockLibvirt) OpenVNC(_ context.Context, name string) (io.ReadWriteCloser, error) {
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
	// logs 域日志内容（决策 #311：ReadTail 供启动失败诊断）。
	logs map[string]string
}

func newMockStorage(files ...string) *mockStorage {
	s := &mockStorage{files: map[string]bool{}, logs: map[string]string{}}
	for _, f := range files {
		s.files[f] = true
	}
	return s
}

// ReadTail 返回预置的日志内容；未预置即「取不到」（error），与真实读文件同语义。
func (s *mockStorage) ReadTail(p string, _ int) (string, error) {
	if txt, ok := s.logs[p]; ok {
		return txt, nil
	}
	return "", os.ErrNotExist
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
	// 探测窗口取极小值：单测不真实等待（sleep 已注入为 no-op）。
	c.StartProbeWindow = time.Millisecond
	c.LibvirtLogDir = "/var/log/libvirt/qemu"
	return c
}

func newTestProvider(api libvirtAPI, store storageAPI, seed seedBuilder) *Provider {
	p := NewProvider(testConfig(), api, store, seed)
	p.poll = time.Millisecond
	p.sleep = func(context.Context, time.Duration) error { return nil }
	return p
}

// newTestProviderProbe 同 newTestProvider，但注入数据面可用性判定（决策 #314）。
func newTestProviderProbe(api libvirtAPI, store storageAPI, seed seedBuilder, probe func() error) *Provider {
	c := testConfig()
	c.DataPlaneProbe = probe
	p := NewProvider(c, api, store, seed)
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

// ---------- 通用 PCI 直通（FR-CMP-023） ----------

// 不存在的设备：define 前如实拒绝、不定义域，文案给照做路径（归一后的 BDF 传给检查器）。
func TestDefineVM_PCIDeviceMissingRejected(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProvider(api, newMockStorage("/images/img.qcow2"), nil)
	var checked []string
	p.SetPCIDeviceChecker(func(bdf string) (bool, error) {
		checked = append(checked, bdf)
		return false, nil
	})
	vm := vmFixture("fw-vm")
	vm.PCIDevices = []string{"FF:1F.7"}
	err := p.DefineVM(context.Background(), vm, model.AllocatedResources{HugepageSize: "1G"})
	if err == nil {
		t.Fatal("不存在的 PCI 设备应被拒绝")
	}
	if len(checked) != 1 || checked[0] != "0000:ff:1f.7" {
		t.Fatalf("检查器应收到归一 BDF: %v", checked)
	}
	for _, want := range []string{"0000:ff:1f.7", "lspci", "/sys/bus/pci/devices", "unbind-dpdk"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("拒绝文案应含照做路径 %q: %v", want, err)
		}
	}
	if len(api.defined) != 0 {
		t.Fatalf("拒绝时不得定义域: %v", api.defined)
	}
}

// 存在的设备：define 成功（检查器按声明序收到归一 BDF）。
func TestDefineVM_PCIDevicePresentDefines(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProvider(api, newMockStorage("/images/img.qcow2"), nil)
	var checked []string
	p.SetPCIDeviceChecker(func(bdf string) (bool, error) {
		checked = append(checked, bdf)
		return true, nil
	})
	vm := vmFixture("fw-vm")
	vm.PCIDevices = []string{"0000:03:00.0", "04:1f.7"}
	if err := p.DefineVM(context.Background(), vm, model.AllocatedResources{HugepageSize: "1G"}); err != nil {
		t.Fatalf("设备存在时应成功: %v", err)
	}
	if len(api.defined) != 1 || api.defined[0] != "fw-vm" {
		t.Fatalf("应定义 fw-vm: %v", api.defined)
	}
	if fmt.Sprint(checked) != "[0000:03:00.0 0000:04:1f.7]" {
		t.Fatalf("检查器收到的归一 BDF 不符: %v", checked)
	}
}

// 未注入检查器而声明了设备：明确报错，不静默放行（与 vfPCI 同口径）。
func TestDefineVM_PCIDeviceCheckerUnavailable(t *testing.T) {
	p := newTestProvider(newMockLibvirt(), newMockStorage("/images/img.qcow2"), nil)
	vm := vmFixture("fw-vm")
	vm.PCIDevices = []string{"0000:03:00.0"}
	err := p.DefineVM(context.Background(), vm, model.AllocatedResources{HugepageSize: "1G"})
	if err == nil || !strings.Contains(err.Error(), "未提供存在性检查") {
		t.Fatalf("无检查器应明确报错: %v", err)
	}
	if err := p.DefineVM(context.Background(), vmFixture("fw-vm"), model.AllocatedResources{HugepageSize: "1G"}); err != nil {
		t.Fatalf("无设备的 VM 不受影响: %v", err)
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

// 决策 #485：Provider.VNC 与 Console 同构的状态前置——运行中可打开，未定义/未运行如实报错。
func TestProviderVNCStateGate(t *testing.T) {
	p, _, _ := newRunningProvider("fw-vm")
	stream, err := p.VNC(context.Background(), "fw-vm")
	if err != nil || stream == nil {
		t.Fatalf("运行中的 VM 应能打开 VNC: %v", err)
	}
	_ = stream.Close()

	if _, err := p.VNC(context.Background(), "ghost"); err == nil {
		t.Fatal("未定义的 VM 应报错")
	}

	api := newMockLibvirt()
	api.present["off-vm"] = true
	api.states["off-vm"] = domShutoff
	p2 := newTestProvider(api, newMockStorage(), nil)
	if _, err := p2.VNC(context.Background(), "off-vm"); err == nil {
		t.Fatal("未运行的 VM 应报错（VNC 端口未分配）")
	}
}

// TestStartVMChecked_UnexpectedStateSurfacesDiagnosis 决策 #311：启动后停在 vhost-user
// 初始化失败的典型态 paused(starting up) ⇒ 非 OK，并给出域状态/reason、日志摘录与恢复建议。
func TestStartVMChecked_UnexpectedStateSurfacesDiagnosis(t *testing.T) {
	api := newMockLibvirt()
	store := newMockStorage()
	api.present["fw-vm"] = true
	api.states["fw-vm"] = domShutoff
	api.pauseOnStart = true // Start 后停在 paused(starting up)
	store.logs["/var/log/libvirt/qemu/fw-vm.log"] = "2026-09-30: qemu: vhost-user: connect failed"
	p := newTestProvider(api, store, nil)

	probe, err := p.StartVMChecked(context.Background(), "fw-vm")
	if err != nil {
		t.Fatalf("StartVMChecked 不应报错（域已创建，只是没起来）: %v", err)
	}
	if probe.OK {
		t.Fatalf("停在 paused 不应判 OK：%+v", probe)
	}
	if probe.State != orchestrator.VMStatePaused {
		t.Fatalf("State 应为 paused，实得 %q", probe.State)
	}
	if !strings.Contains(probe.Reason, "starting up") {
		t.Fatalf("Reason 应含 libvirt reason，实得 %q", probe.Reason)
	}
	if !strings.Contains(probe.Detail, "vhost-user") {
		t.Fatalf("Detail 应含域日志摘录，实得 %q", probe.Detail)
	}
	if probe.LogPath == "" || len(probe.Hints) == 0 {
		t.Fatalf("应给出日志路径与恢复建议：%+v", probe)
	}
	// 恢复建议须是可照做的已知路径（不臆测根因）。
	joined := strings.Join(probe.Hints, " ")
	if !strings.Contains(joined, "request vpp restart") {
		t.Fatalf("恢复建议应含已验证路径 request vpp restart，实得 %v", probe.Hints)
	}
}

// TestStartVMChecked_NormalStartNoDiagnosis 正常启动路径：OK、无 reason/detail/hints，
// 不改变既有语义（决策 #311 的「正常路径不变」）。
func TestStartVMChecked_NormalStartNoDiagnosis(t *testing.T) {
	api := newMockLibvirt()
	api.present["fw-vm"] = true
	api.states["fw-vm"] = domShutoff
	p := newTestProvider(api, newMockStorage(), nil)

	probe, err := p.StartVMChecked(context.Background(), "fw-vm")
	if err != nil {
		t.Fatal(err)
	}
	if !probe.OK || probe.State != orchestrator.VMStateRunning {
		t.Fatalf("正常启动应 OK/running，实得 %+v", probe)
	}
	if probe.Reason != "" || probe.Detail != "" || len(probe.Hints) != 0 {
		t.Fatalf("正常启动不应带诊断字段：%+v", probe)
	}
	if len(api.started) != 1 {
		t.Fatalf("应真的启动一次: %v", api.started)
	}
}

// TestStartVMChecked_CrashedAfterStart 启动后域立刻退出（SHUTOFF+CRASHED）也判失败，
// 契约态映射为 crashed 并给崩溃处置建议（决策 #311）。
func TestStartVMChecked_CrashedAfterStart(t *testing.T) {
	api := newMockLibvirt()
	api.present["fw-vm"] = true
	api.states["fw-vm"] = domShutoff
	api.crashOnStart = true
	p := newTestProvider(api, newMockStorage(), nil)

	probe, err := p.StartVMChecked(context.Background(), "fw-vm")
	if err != nil {
		t.Fatal(err)
	}
	if probe.OK {
		t.Fatalf("启动后立刻退出不应判 OK：%+v", probe)
	}
	if probe.State != orchestrator.VMStateCrashed {
		t.Fatalf("State 应为 crashed，实得 %q（reason=%q）", probe.State, probe.Reason)
	}
	if len(probe.Hints) == 0 {
		t.Fatalf("应给出恢复建议：%+v", probe)
	}
}

// ---------- 数据面前置判定（决策 #314） ----------

// TestStartVM_DataPlaneUnavailableFailsFastBeforeLibvirt 决策 #314：VPP 不可用时，start 前置判定
// 立即失败——**不调用 libvirt Start**（不进会阻塞的 vhost-user/socket 准备阶段），错误含指引。
func TestStartVM_DataPlaneUnavailableFailsFastBeforeLibvirt(t *testing.T) {
	api := newMockLibvirt()
	api.present["fw-vm"] = true
	api.states["fw-vm"] = domShutoff // 关机态：否则幂等分支直接成功，判不到前置
	p := newTestProviderProbe(api, newMockStorage(), nil,
		func() error { return errors.New("VPP 未连接（状态 failed）") })

	_, err := p.StartVMChecked(context.Background(), "fw-vm")
	if !errors.Is(err, orchestrator.ErrDataPlaneUnavailable) {
		t.Fatalf("应返回数据面不可用错误（哨兵可判），实得 %v", err)
	}
	if len(api.started) != 0 {
		t.Fatalf("不可用时不得调用 libvirt Start（阻塞点）: %v", api.started)
	}
	for _, want := range []string{"数据面（VPP）当前不可用", "未启动虚拟机", "request vpp restart", "show vpp"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误文案应含 %q：%v", want, err)
		}
	}
}

// TestStartVM_DataPlaneAvailableProceeds 可用时前置判定放行：正常启动与既有语义一致。
func TestStartVM_DataPlaneAvailableProceeds(t *testing.T) {
	api := newMockLibvirt()
	api.present["fw-vm"] = true
	api.states["fw-vm"] = domShutoff
	calls := 0
	p := newTestProviderProbe(api, newMockStorage(), nil, func() error { calls++; return nil })

	probe, err := p.StartVMChecked(context.Background(), "fw-vm")
	if err != nil || !probe.OK || probe.State != orchestrator.VMStateRunning {
		t.Fatalf("可用时应正常启动：probe=%+v err=%v", probe, err)
	}
	if calls != 1 || len(api.started) != 1 {
		t.Fatalf("应判定一次并真的启动一次: calls=%d started=%v", calls, api.started)
	}
}

// TestRestartVM_DataPlaneUnavailableOnOffBranch 决策 #314：restart 的 off→start 分支同样前置判定，
// 且同样**不进 libvirt Start**。
func TestRestartVM_DataPlaneUnavailableOnOffBranch(t *testing.T) {
	api := newMockLibvirt()
	api.present["fw-vm"] = true
	api.states["fw-vm"] = domShutoff
	p := newTestProviderProbe(api, newMockStorage(), nil,
		func() error { return errors.New("VPP 未运行") })

	err := p.RestartVM(context.Background(), "fw-vm")
	if !errors.Is(err, orchestrator.ErrDataPlaneUnavailable) {
		t.Fatalf("off→start 分支应返回数据面不可用错误，实得 %v", err)
	}
	if len(api.started) != 0 || len(api.rebooted) != 0 {
		t.Fatalf("不可用时不得调用 libvirt Start/Reboot: started=%v rebooted=%v", api.started, api.rebooted)
	}
}

// TestRestartVM_RunningUsesRebootWithoutDataPlaneCheck 运行中 restart 走 ACPI 重启、不经启动前置
// 判定——不因 VPP 状态改变既有重启语义（决策 #314 的覆盖边界）。
func TestRestartVM_RunningUsesRebootWithoutDataPlaneCheck(t *testing.T) {
	api := newMockLibvirt()
	api.present["fw-vm"] = true
	api.states["fw-vm"] = domRunning
	called := false
	p := newTestProviderProbe(api, newMockStorage(), nil,
		func() error { called = true; return errors.New("VPP 未运行") })

	if err := p.RestartVM(context.Background(), "fw-vm"); err != nil {
		t.Fatalf("运行中 restart 应走 ACPI 重启成功: %v", err)
	}
	if called {
		t.Fatal("运行中 restart 不应触发数据面前置判定（只有 off→start 分支才判）")
	}
	if len(api.rebooted) != 1 || len(api.started) != 0 {
		t.Fatalf("应只调用 Reboot: rebooted=%v started=%v", api.rebooted, api.started)
	}
}

// TestStartVM_NoProbeKeepsLegacyBehavior 未注入判定（nil）时行为与既有完全一致（不判定、直接启动）。
func TestStartVM_NoProbeKeepsLegacyBehavior(t *testing.T) {
	api := newMockLibvirt()
	api.present["fw-vm"] = true
	api.states["fw-vm"] = domShutoff
	p := newTestProvider(api, newMockStorage(), nil) // 既有构造，无 DataPlaneProbe
	if err := p.StartVM(context.Background(), "fw-vm"); err != nil {
		t.Fatalf("未注入判定时启动应照常成功: %v", err)
	}
	if len(api.started) != 1 {
		t.Fatalf("应真的启动一次: %v", api.started)
	}
}

// TestStateReasonText 只报事实的可读映射（决策 #311）。
func TestStateReasonText(t *testing.T) {
	cases := []struct {
		state, reason int
		want          string
	}{
		{domPaused, pausedReasonStartingUp, "paused (starting up)"},
		{domPaused, pausedReasonIoerror, "paused (I/O error)"},
		{domPaused, 99, "paused (reason 99)"}, // 未知 reason 如实给编号，不编造短语
		{domCrashed, 0, "crashed"},
		{domShutoff, shutoffReasonFailed, "shutoff (failed)"},
		{domShutoff, shutoffReasonCrashed, "crashed"},
		{domRunning, 0, "running"},
	}
	for _, c := range cases {
		if got := StateReasonText(c.state, c.reason); got != c.want {
			t.Errorf("StateReasonText(%d,%d) = %q，期望 %q", c.state, c.reason, got, c.want)
		}
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

// ActiveOf 仅为满足 orchestrator.AlarmSink（计算侧不做按 scope 对账清警）。
func (f *fakeSink) ActiveOf(string) []orchestrator.AlarmRef { return nil }

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

// recordingSink 进程内告警表（对账建/消测试用）：按 (code, source) 记账，支持 ActiveOf/Resolve
// ——与生产 network.AlarmStore 的语义一致（同键幂等、按 code+source 消解）。
type recordingSink struct {
	active map[string]orchestrator.AlarmRef
	order  []string
}

func newRecordingSink() *recordingSink {
	return &recordingSink{active: map[string]orchestrator.AlarmRef{}}
}

func recordingKey(code, source string) string { return code + "\x00" + source }

func (s *recordingSink) Raise(_, _, code, _, source string) {
	k := recordingKey(code, source)
	if _, ok := s.active[k]; !ok {
		s.order = append(s.order, k)
	}
	s.active[k] = orchestrator.AlarmRef{Code: code, Source: source}
}

func (s *recordingSink) Resolve(_, code, source string) bool {
	k := recordingKey(code, source)
	if _, ok := s.active[k]; !ok {
		return false
	}
	delete(s.active, k)
	return true
}

func (s *recordingSink) ActiveOf(string) []orchestrator.AlarmRef {
	out := make([]orchestrator.AlarmRef, 0, len(s.active))
	for _, k := range s.order {
		if a, ok := s.active[k]; ok {
			out = append(out, a)
		}
	}
	return out
}

// 决策 #485：孤儿画像对账告警——配置有画像、镜像仓库无该镜像 ⇒ VM_IMAGE_PROFILE_ORPHAN（warning）；
// 镜像出现即自动消解；换一张空告警表模拟 nfvisd 重启后仍按事实重建（不靠进程内记忆）。
func TestImageProfileOrphanAlarmRebuiltAndResolved(t *testing.T) {
	p := newTestProvider(newMockLibvirt(), newMockStorage(), nil)
	p.SetImageExists(func(name string) bool { return name == "img-ok" })
	sink := newRecordingSink()
	p.SetAlarms(sink)

	cfg := model.Config{VMImageProfiles: []model.VMImageProfile{
		{Image: "img-ok", Machine: "q35"},
		{Image: "img-gone", Machine: "q35"},
	}}

	// 恢复收敛路径（无 VM ⇒ 不产生 VM 收敛告警）
	if errs := p.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("无 VM 时收敛应无错误: %v", errs)
	}
	active := sink.ActiveOf(orchestrator.RecoveryScopeCompute)
	if len(active) != 1 || active[0].Code != orchestrator.VMImageProfileOrphan ||
		active[0].Source != "vm-image-profile/img-gone" {
		t.Fatalf("应产生孤儿画像告警（仅 img-gone）: %+v", active)
	}

	// 15s 巡检路径：事实未变 ⇒ 保持（幂等）
	if errs := p.CheckVMAlarms(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("巡检应无错误: %v", errs)
	}
	if got := sink.ActiveOf(orchestrator.RecoveryScopeCompute); len(got) != 1 {
		t.Fatalf("事实未变告警应保持: %+v", got)
	}

	// 镜像出现 ⇒ 自动消警
	p.SetImageExists(func(string) bool { return true })
	p.CheckVMAlarms(context.Background(), cfg)
	if got := sink.ActiveOf(orchestrator.RecoveryScopeCompute); len(got) != 0 {
		t.Fatalf("镜像出现应自动消警: %+v", got)
	}

	// 跨「重启」：换一张空告警表（进程内历史全丢），仓库仍缺 img-gone ⇒ 按事实重建
	p.SetImageExists(func(name string) bool { return name == "img-ok" })
	sink2 := newRecordingSink()
	p.SetAlarms(sink2)
	p.EnsureConsistent(context.Background(), cfg)
	active = sink2.ActiveOf(orchestrator.RecoveryScopeCompute)
	if len(active) != 1 || active[0].Code != orchestrator.VMImageProfileOrphan ||
		active[0].Source != "vm-image-profile/img-gone" {
		t.Fatalf("重启后孤儿画像告警必须仍可见（按事实重建）: %+v", active)
	}
}

// 未注入镜像仓库探针（ImageExists 为 nil）⇒ 不猜测：既不建也不消（生产在镜像仓库未装配时即此情形）。
func TestImageProfileOrphanSkippedWithoutImageProbe(t *testing.T) {
	p := newTestProvider(newMockLibvirt(), newMockStorage(), nil)
	sink := newRecordingSink()
	p.SetAlarms(sink)
	// 预置一条历史活动告警，验证无探针时不被误消
	sink.Raise(orchestrator.RecoveryScopeCompute, orchestrator.SeverityWarning,
		orchestrator.VMImageProfileOrphan, "旧告警", "vm-image-profile/img-gone")

	cfg := model.Config{VMImageProfiles: []model.VMImageProfile{{Image: "img-gone", Machine: "q35"}}}
	p.EnsureConsistent(context.Background(), cfg)
	if got := sink.ActiveOf(orchestrator.RecoveryScopeCompute); len(got) != 1 {
		t.Fatalf("无镜像探针时不得猜测（保守留警）: %+v", got)
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

// ---------- 域新鲜度前置（决策 #455） ----------

// newTestProviderKernel 构造内核数据面的 provider（域生成走 bridge 形态，vNIC 需 virtual_switch）。
func newTestProviderKernel(api libvirtAPI, store storageAPI, seed seedBuilder) *Provider {
	c := testConfig()
	c.DataPlane = model.DataPlaneKernel
	p := NewProvider(c, api, store, seed)
	p.poll = time.Millisecond
	p.sleep = func(context.Context, time.Duration) error { return nil }
	return p
}

// TestStartVM_RedefinesStaleDomainFromCurrentConfig 决策 #455：配置变更后 start 必须按当前配置
// 重定义域（而非沿用先前持久化的旧域）——内核数据面下应产出 bridge 形态域，不落 vhost-user。
// 红-绿：修复前 StartVM 直接启动旧域、从不重定义。
func TestStartVM_RedefinesStaleDomainFromCurrentConfig(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProviderKernel(api, newMockStorage("/images/img.qcow2"), nil)

	vm := vmFixture("fw-vm")
	vm.Interfaces = []model.VnfInterface{{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-lan"}}
	// 当前配置（生产由提交期 DefineVM / 15s 巡检喂入）。
	p.setVMSpec(vm, model.AllocatedResources{HugepageSize: "1G"})
	// 先前持久化的旧域：存在且关机态（内容可能是为 vhost-user 写的老定义）。
	api.present["fw-vm"] = true
	api.states["fw-vm"] = domShutoff

	if err := p.StartVM(context.Background(), "fw-vm"); err != nil {
		t.Fatalf("StartVM: %v", err)
	}
	if len(api.definedXML) != 1 {
		t.Fatalf("启动前应按当前配置重定义域一次: defined=%v", api.defined)
	}
	if !strings.Contains(api.definedXML[0], `<interface type="bridge">`) ||
		strings.Contains(api.definedXML[0], "vhostuser") {
		t.Fatalf("start 应按当前配置（内核 bridge）重定义域，而非沿用旧 vhost-user 域:\n%s", api.definedXML[0])
	}
	if len(api.started) != 1 || api.states["fw-vm"] != domRunning {
		t.Fatalf("应启动到 running: started=%v state=%d", api.started, api.states["fw-vm"])
	}
}

// TestStartVM_DestroysStalePausedDomainBeforeRedefine 决策 #455：陈旧 paused 域是活动态，
// 幂等分支会直接返回、永远起不来；start 必须先销毁、再按当前配置重定义、再启动。
func TestStartVM_DestroysStalePausedDomainBeforeRedefine(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProviderKernel(api, newMockStorage("/images/img.qcow2"), nil)

	vm := vmFixture("fw-vm")
	vm.Interfaces = []model.VnfInterface{{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-lan"}}
	p.setVMSpec(vm, model.AllocatedResources{HugepageSize: "1G"})
	api.present["fw-vm"] = true
	api.states["fw-vm"] = domPaused
	api.reasons["fw-vm"] = pausedReasonStartingUp

	if err := p.StartVM(context.Background(), "fw-vm"); err != nil {
		t.Fatalf("StartVM: %v", err)
	}
	if len(api.destroyed) != 1 {
		t.Fatalf("陈旧 paused 域应先被销毁: destroyed=%v", api.destroyed)
	}
	if len(api.definedXML) != 1 {
		t.Fatalf("销毁后应按当前配置重定义: defined=%v", api.defined)
	}
	if len(api.started) != 1 || api.states["fw-vm"] != domRunning {
		t.Fatalf("应重定义并启动到 running: started=%v state=%d", api.started, api.states["fw-vm"])
	}
}

// TestRestartVM_RedefinesStalePausedDomain 决策 #455：restart 的 off→start 分支同口径
// （陈旧 paused 域先销毁、按当前配置重定义、再启动）。
func TestRestartVM_RedefinesStalePausedDomain(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProviderKernel(api, newMockStorage("/images/img.qcow2"), nil)

	vm := vmFixture("fw-vm")
	vm.Interfaces = []model.VnfInterface{{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-lan"}}
	p.setVMSpec(vm, model.AllocatedResources{HugepageSize: "1G"})
	api.present["fw-vm"] = true
	api.states["fw-vm"] = domPaused

	if err := p.RestartVM(context.Background(), "fw-vm"); err != nil {
		t.Fatalf("RestartVM: %v", err)
	}
	if len(api.destroyed) != 1 || len(api.definedXML) != 1 || len(api.started) != 1 {
		t.Fatalf("restart 应先销毁陈旧 paused 域、重定义、再启动: destroyed=%v defined=%v started=%v",
			api.destroyed, api.defined, api.started)
	}
	if api.states["fw-vm"] != domRunning {
		t.Fatalf("restart 后应 running，实得 %d", api.states["fw-vm"])
	}
}

// TestStartVM_RedefineFailureIsActionable 决策 #455：重定义失败（当前配置缺 virtual_switch）
// 时如实返回可诊断错误，且**不启动旧域**（不把错误推到「起成 paused」之后）。
func TestStartVM_RedefineFailureIsActionable(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProviderKernel(api, newMockStorage("/images/img.qcow2"), nil)

	vm := vmFixture("fw-vm")
	vm.Interfaces = []model.VnfInterface{{Name: "eth0", Type: "vhost-user"}} // 缺 virtual_switch
	p.setVMSpec(vm, model.AllocatedResources{HugepageSize: "1G"})
	api.present["fw-vm"] = true
	api.states["fw-vm"] = domShutoff

	err := p.StartVM(context.Background(), "fw-vm")
	if err == nil || !strings.Contains(err.Error(), "缺少接入的虚拟交换机") {
		t.Fatalf("重定义失败应如实报错（含可诊断原因），实得 %v", err)
	}
	if len(api.started) != 0 {
		t.Fatalf("重定义失败不得启动旧域: %v", api.started)
	}
}

// TestStartVM_NoCurrentSpecKeepsLegacySemantics 决策 #455：拿不到当前声明（provider 未收到过
// 该 VM 的配置）时不臆测、不重定义，维持既有启动语义（启动已存在的域）。
func TestStartVM_NoCurrentSpecKeepsLegacySemantics(t *testing.T) {
	api := newMockLibvirt()
	api.present["fw-vm"] = true
	api.states["fw-vm"] = domShutoff
	p := newTestProvider(api, newMockStorage(), nil) // 无 spec 缓存

	if err := p.StartVM(context.Background(), "fw-vm"); err != nil {
		t.Fatalf("无当前声明时应照常启动已存在的域: %v", err)
	}
	if len(api.definedXML) != 0 {
		t.Fatalf("无当前声明时不应重定义: %v", api.defined)
	}
	if len(api.started) != 1 {
		t.Fatalf("应真的启动一次: %v", api.started)
	}
}

// TestStartVM_MissingDomainRedefinedFromCurrentSpec 决策 #455：域缺失但当前配置里有该 VM
// 时按当前配置补建后再启动（不再直接报未定义）。
func TestStartVM_MissingDomainRedefinedFromCurrentSpec(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProvider(api, newMockStorage("/images/img.qcow2"), nil)

	vm := vmFixture("fw-vm")
	p.setVMSpec(vm, model.AllocatedResources{HugepageSize: "1G"})
	// 域不存在（present 缺省 false）。

	if err := p.StartVM(context.Background(), "fw-vm"); err != nil {
		t.Fatalf("域缺失但配置在时应补建并启动: %v", err)
	}
	if len(api.defined) != 1 || len(api.started) != 1 || api.states["fw-vm"] != domRunning {
		t.Fatalf("应按当前配置补建并启动: defined=%v started=%v state=%d", api.defined, api.started, api.states["fw-vm"])
	}
}

// FR-CMP-017：crashed → critical 告警；恢复则消警。
func TestCheckVMAlarms(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProvider(api, newMockStorage(), nil)
	sink := &fakeSink{}
	p.SetAlarms(sink)
	cfg := model.Config{VirtualMachineFunctions: []model.VMFunction{vmFixture("fw-vm")}}
	api.present["fw-vm"] = true

	api.states["fw-vm"] = domCrashed
	if errs := p.CheckVMAlarms(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("巡检不应报错: %v", errs)
	}
	if len(sink.raised) != 1 || sink.raised[0] != "fw-vm" {
		t.Fatalf("crashed 应告警: %v", sink.raised)
	}
	api.states["fw-vm"] = domRunning
	p.CheckVMAlarms(context.Background(), cfg)
	if len(sink.resolved) != 1 {
		t.Fatalf("恢复应消警: %v", sink.resolved)
	}
}

// ---------- 有效启动画像接入（决策 #485，Task 9）----------

// 给定镜像画像 + 实例覆盖时，组装出的 DomainSpec 启动字段应等于有效画像
// （实例覆盖 > 镜像画像 > 产品默认，合并由 model.EffectiveStartup 给出）。
func TestDomainSpecUsesEffectiveStartup(t *testing.T) {
	profiles := []model.VMImageProfile{{Image: "img1", Machine: "pc", Firmware: "uefi"}}
	vm := model.VMFunction{Name: "v1", Image: "img1", VCPU: model.VMCpu{Count: 1}, Memory: model.VMMemory{SizeMB: 512},
		Startup: &model.VMStartupOverride{Console: "vnc"}}
	spec := startupDomainSpec(vm, profiles)
	if spec.Machine != "pc" || spec.Firmware != "uefi" || spec.Console != "vnc" {
		t.Fatalf("effective profile not applied: %+v", spec)
	}
}

// 无画像、无覆盖时启动字段回落产品默认（既有行为不变：q35/host-passthrough/bios/virtio/hd/kvm/serial）。
func TestDomainSpecDefaultStartupWithoutProfile(t *testing.T) {
	spec := startupDomainSpec(vmFixture("v1"), nil)
	if spec.Machine != model.DefaultMachine || spec.CPUMode != model.DefaultCPUMode ||
		spec.Firmware != model.DefaultFirmware || spec.DiskIface != model.DefaultDiskIface ||
		spec.BootOrder != model.DefaultBootOrder || spec.Accel != model.DefaultAccel ||
		spec.Console != model.DefaultConsole {
		t.Fatalf("无画像应回落产品默认: %+v", spec)
	}
}

// 装配配置（compute.Config）里的画像经生产路径 specFor 进入 DomainSpec。
func TestSpecForUsesConfiguredProfiles(t *testing.T) {
	c := testConfig()
	c.VMImageProfiles = []model.VMImageProfile{{Image: "img.qcow2", Machine: "pc-i440fx", Accel: "tcg", BootOrder: "cd"}}
	p := NewProvider(c, newMockLibvirt(), newMockStorage("/images/img.qcow2"), nil)
	spec, err := p.specFor(vmFixture("fw-vm"), model.AllocatedResources{HugepageSize: "1G"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Machine != "pc-i440fx" || spec.Accel != "tcg" || spec.BootOrder != "cd" {
		t.Fatalf("装配配置的画像应进入 DomainSpec: %+v", spec)
	}
	// 资源类字段不因画像改变（账本单一真源，决策 #481 红线）。
	if spec.HugepageSize != "1G" || len(spec.Cores) != 0 {
		t.Fatalf("画像不得触碰资源字段: %+v", spec)
	}
}

// 恢复收敛（重放路径）按当次 committed 配置刷新画像：EnsureConsistent 后定义出的域应带新画像。
func TestEnsureConsistentRefreshesProfiles(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProvider(api, newMockStorage("/images/img.qcow2"), nil)

	cfg := model.Config{
		ResourcePools: &model.ResourcePool{
			Hugepages: []model.HPool{{PageSize: "1G", Count: 4}},
			CPU:       &model.CPUSetup{IsolatedCores: []int{0, 1, 2, 3}},
		},
		VMImageProfiles:         []model.VMImageProfile{{Image: "img.qcow2", Firmware: "uefi"}},
		VirtualMachineFunctions: []model.VMFunction{vmFixture("fw-vm")},
	}
	if errs := p.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("恢复收敛不应报错: %v", errs)
	}
	if len(api.definedXML) != 1 || !strings.Contains(api.definedXML[0], `firmware="efi"`) {
		t.Fatalf("重放应使用 committed 画像（firmware=uefi）: %v", api.definedXML)
	}
}

// 15s 巡检同样按 committed 刷新画像（提交画像变更后、下次 start/restart 重定义即生效）。
func TestCheckVMAlarmsRefreshesProfiles(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProvider(api, newMockStorage("/images/img.qcow2"), nil)

	cfg := model.Config{
		VMImageProfiles:         []model.VMImageProfile{{Image: "img.qcow2", Machine: "pc"}},
		VirtualMachineFunctions: []model.VMFunction{vmFixture("fw-vm")},
	}
	p.CheckVMAlarms(context.Background(), cfg)

	// 巡检刷新后，specFor（start/restart 的重定义路径）应带上新画像。
	spec, err := p.specFor(vmFixture("fw-vm"), model.AllocatedResources{HugepageSize: "1G"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Machine != "pc" {
		t.Fatalf("巡检应刷新画像: %+v", spec)
	}
}

// 决策 #485 复审：firmware=uefi 的宿主能力前置检查——探测到缺失时如实拒绝并给补救；
// 探测到在位/无法判定时放行（保守，不误拒合法组合）。
func TestCheckFirmwareUEFIProbe(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProvider(api, newMockStorage("/images/img.qcow2"), nil)
	spec := DomainSpec{VM: model.VMFunction{Name: "fw-vm"}, Firmware: "uefi"}

	p.SetUEFIFirmwareProbe(func() (bool, string) { return true, "OVMF 缺失（测试）" })
	err := p.checkFirmware(spec)
	if err == nil || !strings.Contains(err.Error(), "firmware=uefi") || !strings.Contains(strings.ToLower(err.Error()), "ovmf") {
		t.Fatalf("缺少 OVMF 应如实拒绝并给补救: %v", err)
	}

	p.SetUEFIFirmwareProbe(func() (bool, string) { return false, "" })
	if err := p.checkFirmware(spec); err != nil {
		t.Fatalf("探测到固件在位不应拒绝: %v", err)
	}

	// 非 uefi 不触发固件检查。
	if err := p.checkFirmware(DomainSpec{VM: model.VMFunction{Name: "fw-vm"}, Firmware: "bios"}); err != nil {
		t.Fatalf("bios 不应触发固件检查: %v", err)
	}

	// 默认探测器在无法判定宿主（非 POSIX / 未知布局）时保守放行；本仓 CI 主机即此情形。
	_, _ = hostUEFIFirmwareMissing()
}
