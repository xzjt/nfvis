package network

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// ---------- M3-2：startup.conf 生成与 pending_restart（FR-SYS-008/009/010） ----------

func startupFixture() *model.Config {
	return &model.Config{
		ResourcePools: &model.ResourcePool{
			Hugepages: []model.HPool{{PageSize: "1G", Count: 4}},
			CPU:       &model.CPUSetup{IsolatedCores: []int{4, 5, 6, 7}},
		},
		Vpp: &model.VppConfig{
			CPU:    &model.VppCPU{MainCore: 4, CorelistWorkers: "6,7"},
			Memory: &model.VppMemory{MainHeapSize: "2G", BuffersPerNuma: 32768, HugepagePreference: "1G"},
			DPDK: &model.VppDPDK{
				Dev:       model.VppDevDefault{RxQueues: 2, TxQueues: 2, RxDescriptors: 1024, TxDescriptors: 1024},
				PerDev:    []model.VppDevOverride{{Interface: "ens192", RxQueues: 4}},
				UIODriver: "vfio-pci",
			},
			Plugins: []model.VppPlugin{{Name: "acl", State: "enable"}, {Name: "linux-cp", State: "disable"}},
		},
	}
}

func TestGenerateStartupFull(t *testing.T) {
	pci := func(ifname string) (string, error) {
		if ifname == "ens192" {
			return "0000:03:00.0", nil
		}
		return "", errNotFound(ifname)
	}
	out, err := GenerateStartup(startupFixture(), pci)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	wants := []string{
		"dev 0000:03:00.0 {",
		"name ens192", // VPP 接口名固定为配置中的物理口名
		"main-core 4",
		"corelist-workers 6,7",
		"main-heap-size 2G",
		"default-hugepage-size 1G", // hugepage-preference → VPP default-hugepage-size
		"buffers-per-numa 32768",   // 属独立 buffers 段
		"dev default {",
		"num-rx-queues 2",
		"dev 0000:03:00.0 {",
		"num-rx-queues 4",
		"uio-driver vfio-pci",
		"plugin acl_plugin.so { enable }",
		"plugin linux-cp_plugin.so { disable }",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Fatalf("生成文本缺少 %q:\n%s", w, out)
		}
	}
	// buffers-per-numa 不应出现在 memory 段内
	memStart := strings.Index(out, "memory {")
	rest := out[memStart:]
	memEnd := strings.Index(rest, "}\n\n")
	if strings.Contains(rest[:memEnd], "buffers-per-numa") {
		t.Fatalf("buffers-per-numa 不应在 memory 段: \n%s", out)
	}
}

// 单网卡覆盖项缺省回落全局默认：只写非零项。
func TestGenerateStartupOverrideFallback(t *testing.T) {
	cfg := startupFixture()
	cfg.Vpp.DPDK.PerDev = []model.VppDevOverride{{Interface: "ens192", TxDescriptors: 512}}
	pci := func(string) (string, error) { return "0000:03:00.0", nil }
	out, err := GenerateStartup(cfg, pci)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	seg := out[strings.Index(out, "dev 0000:03:00.0 {"):]
	seg = seg[:strings.Index(seg, "}")]
	if !strings.Contains(seg, "num-tx-desc 512") {
		t.Fatalf("覆盖项应写 num-tx-desc: %s", seg)
	}
	if strings.Contains(seg, "num-rx-queues") {
		t.Fatalf("未覆盖项不应写入（应回落全局默认）: %s", seg)
	}
}

func TestGenerateStartupValidation(t *testing.T) {
	// worker 核不在隔离池 → 报错
	cfg := startupFixture()
	cfg.Vpp.CPU.CorelistWorkers = "6,9"
	if _, err := GenerateStartup(cfg, nil); err == nil || !strings.Contains(err.Error(), "isolated-cores") {
		t.Fatalf("应校验 worker 核属于隔离池: %v", err)
	}
	// main-core 不在隔离池 → 报错
	cfg = startupFixture()
	cfg.Vpp.CPU.MainCore = 9
	if _, err := GenerateStartup(cfg, nil); err == nil || !strings.Contains(err.Error(), "main-core") {
		t.Fatalf("应校验 main-core: %v", err)
	}
	// 页大小不一致 → 报错
	cfg = startupFixture()
	cfg.Vpp.Memory.HugepagePreference = "2M"
	if _, err := GenerateStartup(cfg, nil); err == nil || !strings.Contains(err.Error(), "页大小") {
		t.Fatalf("应校验 hugepage-preference: %v", err)
	}
	// 单网卡覆盖但无解析器 → 报错
	cfg = startupFixture()
	if _, err := GenerateStartup(cfg, nil); err == nil || !strings.Contains(err.Error(), "PCI") {
		t.Fatalf("无 PCI 解析器时应报错: %v", err)
	}
}

func TestParseCoreList(t *testing.T) {
	got, err := ParseCoreList("5,7,9-11")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want := []int{5, 7, 9, 10, 11}
	if len(got) != len(want) {
		t.Fatalf("核列表 %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("核列表 %v want %v", got, want)
		}
	}
	for _, bad := range []string{"a", "5-3", "5,,7", "-1"} {
		if _, err := ParseCoreList(bad); err == nil {
			t.Fatalf("%q 应解析失败", bad)
		}
	}
}

func TestVppSectionHashAndPendingRestart(t *testing.T) {
	m := NewManager(Config{Log: slog.New(slog.DiscardHandler)}, &fakeDialer{})
	vppA := &model.VppConfig{CPU: &model.VppCPU{MainCore: 4}}
	vppB := &model.VppConfig{CPU: &model.VppCPU{MainCore: 5}}

	// 尚未应用：有 vpp 配置 → 待重启
	if !m.PendingRestart(vppA) {
		t.Fatalf("未应用过且有 vpp 配置应 pending")
	}
	m.SetApplied(vppA)
	if m.PendingRestart(vppA) {
		t.Fatalf("applied 与当前一致不应 pending")
	}
	if !m.PendingRestart(vppB) {
		t.Fatalf("配置变更后应 pending")
	}
	if VppSectionHash(vppA) == VppSectionHash(vppB) {
		t.Fatalf("不同配置哈希应不同")
	}
	if m.AppliedHash() != VppSectionHash(vppA) {
		t.Fatalf("AppliedHash 不符")
	}
}

func TestApplierApplyAndRestart(t *testing.T) {
	m := NewManager(Config{Log: slog.New(slog.DiscardHandler)}, &fakeDialer{})
	var written string
	var restarted bool
	applier := &Applier{
		Path: "/tmp/startup.conf",
		Mgr:  m,
		PCI:  func(string) (string, error) { return "0000:03:00.0", nil },
		Write: func(path string, data []byte) error {
			if path != "/tmp/startup.conf" {
				t.Fatalf("写入路径: %s", path)
			}
			written = string(data)
			return nil
		},
		Restarter:      restarterFunc(func(context.Context) error { restarted = true; return nil }),
		RestartOnApply: true,
	}
	cfg := startupFixture()
	if _, err := applier.Apply(context.Background(), cfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(written, "main-core 4") || !restarted {
		t.Fatalf("应写盘并重启: written=%v restarted=%v", written != "", restarted)
	}
	if m.PendingRestart(cfg.Vpp) {
		t.Fatalf("Apply 后不应 pending")
	}
}

type restarterFunc func(context.Context) error

func (f restarterFunc) Restart(ctx context.Context) error { return f(ctx) }

func errNotFound(ifname string) error { return &notFoundErr{ifname} }

type notFoundErr struct{ name string }

func (e *notFoundErr) Error() string { return "未找到接口 " + e.name }
