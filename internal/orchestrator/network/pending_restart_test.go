package network

// 决策 #400：pending_restart 语义修正的四项要求（R176-1 重启假阳性 / R176-4 DNS 代理误报）。

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

func testManager() *Manager {
	return NewManager(Config{Log: slog.New(slog.DiscardHandler)}, &fakeDialer{})
}

// 要求①：整机重启后（applied 态从持久落点载入、与 committed 一致）⇒ 不 pending。
func TestPendingRestartFalseAfterReboot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vpp-applied.hash")
	vpp := &model.VppConfig{CPU: &model.VppCPU{MainCore: 4}}

	// 上次运行：应用并落盘。
	m := testManager()
	m.SetAppliedStore(NewAppliedStore(path))
	m.SetApplied(vpp)

	// 模拟整机重启：新进程、新 Manager，从磁盘载入已应用态。
	m2 := testManager()
	m2.SetAppliedStore(NewAppliedStore(path))
	if err := m2.LoadApplied(); err != nil {
		t.Fatalf("载入已应用哈希: %v", err)
	}
	if m2.PendingRestart(vpp) {
		t.Fatalf("重启后 VPP 正按 committed 配置运行，不应 pending")
	}
}

// 要求②：仅改 DNS 代理上游（DNSProxyServers）⇒ 不 pending（它不进 startup.conf，决策 #345）。
func TestPendingRestartIgnoresDNSProxyOnlyChange(t *testing.T) {
	m := testManager()
	before := &model.VppConfig{CPU: &model.VppCPU{MainCore: 4}, DNSProxyServers: []string{"8.8.8.8"}}
	m.SetApplied(before)

	after := &model.VppConfig{CPU: &model.VppCPU{MainCore: 4}, DNSProxyServers: []string{"1.1.1.1", "9.9.9.9"}}
	if m.PendingRestart(after) {
		t.Fatalf("仅改 DNS 代理不应 pending（DNSProxyServers 不进 startup.conf）")
	}
}

// 要求③：真实 vpp 段变更（cpu/memory/dpdk/plugins）⇒ 仍 pending。
func TestPendingRestartOnRealVppSectionChange(t *testing.T) {
	m := testManager()
	base := &model.VppConfig{CPU: &model.VppCPU{MainCore: 4}, Memory: &model.VppMemory{MainHeapSize: "2G"}}
	m.SetApplied(base)

	cases := map[string]*model.VppConfig{
		"cpu":     {CPU: &model.VppCPU{MainCore: 5}, Memory: &model.VppMemory{MainHeapSize: "2G"}},
		"memory":  {CPU: &model.VppCPU{MainCore: 4}, Memory: &model.VppMemory{MainHeapSize: "4G"}},
		"dpdk":    {CPU: &model.VppCPU{MainCore: 4}, Memory: &model.VppMemory{MainHeapSize: "2G"}, DPDK: &model.VppDPDK{UIODriver: "vfio-pci"}},
		"plugins": {CPU: &model.VppCPU{MainCore: 4}, Memory: &model.VppMemory{MainHeapSize: "2G"}, Plugins: []model.VppPlugin{{Name: "acl", State: "enable"}}},
	}
	for name, next := range cases {
		if !m.PendingRestart(next) {
			t.Fatalf("%s 变更应 pending", name)
		}
	}
}

// 要求④：生成器输出形状变化（如升级新增 punt 段）⇒ 升级后首启仍 pending 一次（保 #345）。
func TestPendingRestartOnGeneratorShapeChange(t *testing.T) {
	m := testManager()
	vpp := &model.VppConfig{CPU: &model.VppCPU{MainCore: 4}}

	// 旧版本构建生成器输出形状不同（尚未含 punt 段）：其落下的已应用哈希与当前不同。
	m.SetAppliedHash(vppSectionHash(vpp, "shape-before-punt"))
	if !m.PendingRestart(vpp) {
		t.Fatalf("生成器输出形状变化（升级）后应 pending 一次（保 #345）")
	}
	// 升级后一次 request vpp restart（SetApplied）即消解。
	m.SetApplied(vpp)
	if m.PendingRestart(vpp) {
		t.Fatalf("应用后不应 pending")
	}
}

// 无 committed vpp 段时恒不 pending（避免持久化的旧哈希在配置被清后误报）。
func TestPendingRestartNilVppFalse(t *testing.T) {
	m := testManager()
	m.SetApplied(&model.VppConfig{CPU: &model.VppCPU{MainCore: 4}})
	if m.PendingRestart(nil) {
		t.Fatalf("无 committed vpp 段不应 pending")
	}
}

// 落点往返：Save/Load 一致；缺失文件视为「尚未应用」；空哈希清除落点。
func TestAppliedStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "vpp-applied.hash")
	s := NewAppliedStore(path)
	if h, err := s.Load(); err != nil || h != "" {
		t.Fatalf("缺失文件应返回空且无错: h=%q err=%v", h, err)
	}
	if err := s.Save("abc123"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if h, err := s.Load(); err != nil || h != "abc123" {
		t.Fatalf("Load 往返不符: h=%q err=%v", h, err)
	}
	if err := s.Save(""); err != nil {
		t.Fatalf("Save 空: %v", err)
	}
	if h, _ := s.Load(); h != "" {
		t.Fatalf("空哈希应清除落点: h=%q", h)
	}
}
