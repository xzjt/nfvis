package system

// 决策 #305：`request system storage format-data` 的编排与诚实性单测（mock/mem 层，无真实底座）。
//
// 重点覆盖四组判据：
//  ① **保留节逐字段不变**（保命条款，最关键）：management/api/login 三节 + interfaces + vpp.dpdk
//     在 format 前后逐字段相等；
//  ② 收敛与容错：受管对象被清空、cpu/memory/resource-pools 一并清掉；
//  ③ 数据清理清单与统计、**幂等**（第二次如实回「已是出厂态」）；
//  ④ **部分失败如实报告**（注入删不掉的镜像，断言残留逐条列出且返回错误）。

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/images"
	"github.com/xzjt/nfvis/internal/model"
)

// fakeEngine 内存事务引擎桩：UpdateCandidate 暂存、Commit 落为 committed 并递增修订。
type fakeEngine struct {
	cfg     model.Config
	pending model.Config
	rev     int
	commits int
}

func (f *fakeEngine) Committed() (model.Config, error) { return f.cfg, nil }
func (f *fakeEngine) CurrentRevision() (int, error)    { return f.rev, nil }
func (f *fakeEngine) Edit(config.Session) error        { return nil }
func (f *fakeEngine) Release(config.Session) error     { return nil }
func (f *fakeEngine) UpdateCandidate(_ config.Session, c model.Config) error {
	f.pending = c
	return nil
}
func (f *fakeEngine) Commit(context.Context, config.Session, config.CommitOpts) (config.CommitResult, error) {
	f.cfg = f.pending
	f.rev++
	f.commits++
	return config.CommitResult{Revision: f.rev}, nil
}

// fakeImages 镜像仓库桩见 backup_test.go（metas/deleted/fail/seen）——本文件复用同一份。

// fullDemoConfig 一份「什么都有」的配置：保留节 + 待删的全部受管对象。
func fullDemoConfig() model.Config {
	up := true
	return model.Config{
		System: &model.SystemConfig{
			Hostname: "demo-host", // 非保留：应清
			Timezone: "Asia/Shanghai",
			Management: &model.MgmtConfig{
				Interface: "ens160", Address: "192.168.155.10/24", Gateway: "192.168.155.1",
			},
			API: &model.APIConfig{Port: 443, TokenTTLMinutes: 30, MaxSessions: 8, TLSSelfSigned: true},
			Login: &model.SystemLogin{
				Banner: "authorized only",
				Users: []model.LoginUserConfig{
					{Name: "admin", PasswordHash: "pbkdf2$sha256$1$c2FsdA==$aGFzaA==", Class: "super-user"},
					{Name: "viewer", PasswordHash: "pbkdf2$sha256$1$c2FsdA==$aGFzaA==", Class: "read-only"},
				},
				Classes:        []model.ClassDef{{Name: "netop", Allow: []string{"show"}}},
				PasswordPolicy: &model.PasswordPolicy{MinLength: 12, Complexity: true, ExpireDays: 90},
			},
			Syslog: &model.SyslogConfig{RemoteHost: "10.0.0.9", Severity: "info"},
			Health: &model.HealthThresholds{CPUTempCelsius: 80},
			Ntp:    []model.NtpServer{{Server: "ntp.example", Prefer: true}},
			Kernel: &model.KernelConfig{IOMMU: "on"},
		},
		Interfaces: []model.InterfaceConfig{
			{Name: "ens192", Description: "uplink", MTU: 9000, Enabled: &up,
				Sriov: &model.InterfaceSriov{VFCount: 4}, IngressPolicy: "pol1"},
		},
		Vpp: &model.VppConfig{
			CPU:    &model.VppCPU{MainCore: 2, CorelistWorkers: "3-4"},
			Memory: &model.VppMemory{MainHeapSize: "2G", HugepagePreference: "1G"},
			DPDK: &model.VppDPDK{
				Dev:       model.VppDevDefault{RxQueues: 2},
				PerDev:    []model.VppDevOverride{{Interface: "ens192", RxQueues: 4}},
				UIODriver: "vfio-pci",
			},
			Plugins: []model.VppPlugin{{Name: "nat", State: "enable"}},
		},
		Bonds:           []model.Bond{{Name: "bond0", Members: []string{"ens224", "ens225"}}},
		VirtualSwitches: []model.VirtualSwitch{{Name: "vs-l2", Type: "l2", VlanAccess: 100}},
		Vrfs:            []model.Vrf{{Name: "vs-l3", L3Interfaces: []model.L3Interface{{Interface: "ens226"}}, Routes: []model.Route{{Prefix: "0.0.0.0/0"}}}},
		Acls:            []model.Acl{{Name: "acl1"}},
		Nat: &model.NatConfig{
			SourcePools: []model.NatSourcePool{{Name: "pool1"}},
			Rules:       []model.NatRule{{Seq: 10}},
			Static:      []model.NatStatic{{}},
		},
		PortMirroring:           []model.PortMirroring{{Name: "span1"}},
		QosPolicies:             []model.QosPolicy{{Name: "qos1"}},
		Protocols:               &model.ProtocolsConfig{LLDP: &model.LldpConfig{Enabled: true, Interfaces: []model.LldpInterface{{Interface: "ens192"}}}},
		VirtualMachineFunctions: []model.VMFunction{{Name: "vnf-a"}},
		ContainerFunctions:      []model.ContainerFunction{{Name: "ct-a"}},
		Annotations:             map[string]string{"x": "y"},
	}
}

// ---------- ① 保留节逐字段不变（保命条款） ----------

func TestFormatDataKeepsReservedSectionsVerbatim(t *testing.T) {
	before := fullDemoConfig()
	eng := &fakeEngine{cfg: before}
	m := NewManager(Config{Dir: t.TempDir()}, eng, nil, "test")

	if _, err := m.FormatData(context.Background(), "admin"); err != nil {
		t.Fatalf("FormatData: %v", err)
	}
	got, _ := eng.Committed()

	// 三节 + 物理口 + DPDK 逐字段相等（reflect.DeepEqual 是最强判据：任何字段被改/丢都会红）。
	if !reflect.DeepEqual(got.System.Management, before.System.Management) {
		t.Errorf("management 节被改动:\n  before=%+v\n  after =%+v", before.System.Management, got.System.Management)
	}
	if !reflect.DeepEqual(got.System.API, before.System.API) {
		t.Errorf("api 节被改动:\n  before=%+v\n  after =%+v", before.System.API, got.System.API)
	}
	if !reflect.DeepEqual(got.System.Login, before.System.Login) {
		t.Errorf("login 节被改动:\n  before=%+v\n  after =%+v", before.System.Login, got.System.Login)
	}
	if !reflect.DeepEqual(got.Interfaces, before.Interfaces) {
		t.Errorf("物理口声明被改动:\n  before=%+v\n  after =%+v", before.Interfaces, got.Interfaces)
	}
	if got.Vpp == nil || !reflect.DeepEqual(got.Vpp.DPDK, before.Vpp.DPDK) {
		t.Errorf("vpp.dpdk 声明被改动: after=%+v", got.Vpp)
	}

	// ② 收敛：其余一切清空（含 cpu/memory/resource-pools 与所有受管对象）。
	if got.Vpp != nil && (got.Vpp.CPU != nil || got.Vpp.Memory != nil || len(got.Vpp.Plugins) != 0) {
		t.Errorf("vpp 的非 DPDK 子节应被清空: %+v", got.Vpp)
	}
	if got.System.Hostname != "" || got.System.Syslog != nil || got.System.Health != nil ||
		len(got.System.Ntp) != 0 || got.System.Kernel != nil || got.System.Timezone != "" {
		t.Errorf("system 的非保留节应被清空: %+v", got.System)
	}
	if len(got.Bonds)+len(got.VirtualSwitches)+len(got.Vrfs)+len(got.Acls)+
		len(got.PortMirroring)+len(got.QosPolicies)+len(got.VirtualMachineFunctions)+
		len(got.ContainerFunctions) != 0 {
		t.Errorf("受管对象应全部清空: %+v", got)
	}
	if got.Nat != nil || got.Protocols != nil || got.Annotations != nil {
		t.Errorf("nat/protocols/annotations 应被清空: %+v", got)
	}

	// 结果里如实列出保留节。
	wantKept := []string{"system.management", "system.api", "system.login", "interfaces", "vpp.dpdk"}
	res, _ := m.FormatData(context.Background(), "admin")
	if !reflect.DeepEqual(res.KeptSections, wantKept) {
		t.Errorf("kept_sections 应为 %v，实得 %v", wantKept, res.KeptSections)
	}
}

// ---------- ③ 数据清理清单与统计 / 幂等 ----------

func TestFormatDataPurgesManagedDataAndReportsStats(t *testing.T) {
	root := t.TempDir()
	dirs := map[string]string{
		"backup":    filepath.Join(root, "backup"),
		"captures":  filepath.Join(root, "captures"),
		"coredumps": filepath.Join(root, "coredumps"),
		"tech":      filepath.Join(root, "tech-support"),
		"vms":       filepath.Join(root, "vms"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string, n int) {
		if err := os.WriteFile(p, make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dirs["backup"], "nfvis-backup-x.json"), 100)
	write(filepath.Join(dirs["captures"], "cap.pcap"), 200)
	write(filepath.Join(dirs["coredumps"], "core.1"), 300)
	write(filepath.Join(dirs["tech"], "ts.tar.gz"), 400)
	if err := os.MkdirAll(filepath.Join(dirs["vms"], "vnf-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(dirs["vms"], "vnf-a", "disk.qcow2"), 0) // 目录里嵌套文件

	imgs := &fakeImages{metas: []images.Meta{
		{Name: "alpine.qcow2", SizeBytes: 1000},
		{Name: "alpine:3.20", SizeBytes: 2000},
	}}
	eng := &fakeEngine{cfg: fullDemoConfig()}
	m := NewManager(Config{
		Dir: dirs["backup"], Captures: dirs["captures"], CoreDumps: dirs["coredumps"],
		TechSupport: dirs["tech"], VMs: dirs["vms"],
	}, eng, imgs, "test")

	res, err := m.FormatData(context.Background(), "admin")
	if err != nil {
		t.Fatalf("FormatData: %v", err)
	}
	if len(res.Residuals) != 0 {
		t.Fatalf("不应有残留: %v", res.Residuals)
	}
	// 统计：删除对象按类别、镜像数、清理文件数、释放字节。
	if res.RemovedObjects.Containers != 1 || res.RemovedObjects.VMs != 1 ||
		res.RemovedObjects.VirtualSwitches != 1 || res.RemovedObjects.VRFs != 1 ||
		res.RemovedObjects.Routes != 1 || res.RemovedObjects.ACLs != 1 ||
		res.RemovedObjects.NATRules != 3 || res.RemovedObjects.PortMirroring != 1 ||
		res.RemovedObjects.QosPolicies != 1 || res.RemovedObjects.Bonds != 1 ||
		res.RemovedObjects.LLDPInterfaces != 2 {
		t.Errorf("删除对象统计不符: %+v", res.RemovedObjects)
	}
	if res.RemovedImages != 2 {
		t.Errorf("应删除 2 个镜像，实得 %d（deleted=%v）", res.RemovedImages, imgs.deleted)
	}
	if res.PurgedFiles != 5 {
		t.Errorf("应清理 5 个文件，实得 %d", res.PurgedFiles)
	}
	if want := int64(1000 + 2000 + 100 + 200 + 300 + 400); res.FreedBytes != want {
		t.Errorf("释放字节应为 %d，实得 %d", want, res.FreedBytes)
	}
	for name, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatalf("目录 %s 应仍存在（只清内容）: %v", name, err)
		}
		if len(entries) != 0 {
			t.Errorf("目录 %s 应已清空，实得 %d 项", name, len(entries))
		}
	}

	// 幂等：第二次是「已是出厂态」——不再提交、不再计数、不报错。
	commitsBefore := eng.commits
	res2, err := m.FormatData(context.Background(), "admin")
	if err != nil {
		t.Fatalf("第二次 FormatData 应成功: %v", err)
	}
	if !res2.AlreadyFactory || res2.Status != "already-factory" {
		t.Errorf("第二次应如实回「已是出厂态」: %+v", res2)
	}
	if res2.RemovedImages != 0 || res2.PurgedFiles != 0 || res2.RemovedObjects != (FormatDataCounts{}) {
		t.Errorf("第二次不应假称删了东西: %+v", res2)
	}
	if eng.commits != commitsBefore {
		t.Errorf("已是出厂态时不应再提交配置（修订 0 漂移），提交次数 %d→%d", commitsBefore, eng.commits)
	}
}

// TestFormatDataAlreadyFactoryWhenConfigMinimal：一开始就是保留节最小形态 → 直接如实回「已是出厂态」。
func TestFormatDataAlreadyFactoryWhenConfigMinimal(t *testing.T) {
	base := model.Config{System: &model.SystemConfig{
		Management: &model.MgmtConfig{Interface: "ens160"},
		Login:      &model.SystemLogin{Users: []model.LoginUserConfig{{Name: "admin", Class: "super-user", PasswordHash: "h"}}},
	}}
	eng := &fakeEngine{cfg: base}
	m := NewManager(Config{Dir: t.TempDir()}, eng, nil, "test")
	res, err := m.FormatData(context.Background(), "admin")
	if err != nil {
		t.Fatalf("FormatData: %v", err)
	}
	if !res.AlreadyFactory {
		t.Errorf("保留节最小形态应判为已是出厂态: %+v", res)
	}
	if eng.commits != 0 {
		t.Errorf("已是出厂态不应提交: commits=%d", eng.commits)
	}
}

// ---------- ④ 部分失败如实报告 ----------

func TestFormatDataPartialFailureReportsResiduals(t *testing.T) {
	imgs := &fakeImages{
		metas: []images.Meta{{Name: "ok.qcow2", SizeBytes: 10}, {Name: "stuck.qcow2", SizeBytes: 20}},
		fail:  map[string]error{"stuck.qcow2": errStub("镜像被占用")},
	}
	eng := &fakeEngine{cfg: fullDemoConfig()}
	m := NewManager(Config{Dir: t.TempDir()}, eng, imgs, "test")

	res, err := m.FormatData(context.Background(), "admin")
	if err == nil {
		t.Fatal("有残留时必须返回错误——绝不能在未清干净时报成功")
	}
	if len(res.Residuals) != 1 || !strings.Contains(res.Residuals[0], "stuck.qcow2") {
		t.Errorf("残留应逐条列出未清掉的镜像: %v", res.Residuals)
	}
	if res.RemovedImages != 1 {
		t.Errorf("已清的镜像仍要如实计入统计: %d", res.RemovedImages)
	}
	if !strings.Contains(err.Error(), "未清干净") || !strings.Contains(err.Error(), "stuck.qcow2") {
		t.Errorf("错误文案应说明未清干净且点名残留: %v", err)
	}
}

// errStub 用一个简单的 error 值（避免在测试里引 fmt）。
type errStub string

func (e errStub) Error() string { return string(e) }
