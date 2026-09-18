package network

// 发现 #7 / 决策 #101：管理口守卫——绑定或解绑管理口会当场失去 SSH 与管理 API。

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckManagementPortRefusesEachFact(t *testing.T) {
	business := ManagementFacts{
		DeclaredMgmtIface: "ens160",
		DefaultRouteIface: "ens160",
		ListenIface:       "ens160",
	}

	// 业务口：三条事实都不是它 → 放行（这是真机上的常规路径）
	if err := CheckManagementPort("ens224", business); err != nil {
		t.Fatalf("业务口不应被拒: %v", err)
	}
	// 声明申报的管理口
	err := CheckManagementPort("ens160", business)
	if !errors.Is(err, ErrManagementPort) {
		t.Fatalf("声明的管理口应被拒: %v", err)
	}
	if !strings.Contains(err.Error(), "配置中声明的管理口") {
		t.Fatalf("拒绝理由应可读: %v", err)
	}
	// 未声明，但承载默认路由
	err = CheckManagementPort("ens160", ManagementFacts{DefaultRouteIface: "ens160"})
	if !errors.Is(err, ErrManagementPort) || !strings.Contains(err.Error(), "默认路由") {
		t.Fatalf("承载默认路由的口应被拒: %v", err)
	}
	// 未声明、无默认路由线索，但正是本进程监听的网卡
	err = CheckManagementPort("ens160", ManagementFacts{ListenIface: "ens160"})
	if !errors.Is(err, ErrManagementPort) || !strings.Contains(err.Error(), "监听") {
		t.Fatalf("监听地址所属的口应被拒: %v", err)
	}
	// 拒绝文案要说明后果与出路（不是只丢一句「拒绝」）
	err = CheckManagementPort("ens160", business)
	for _, want := range []string{"SSH", "带外"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("拒绝文案应含 %q: %v", want, err)
		}
	}
}

// 事实未知（未声明管理口、读不到路由表、监听是通配）时**不得**误拒——宁漏不误。
func TestCheckManagementPortNoFalseRefusal(t *testing.T) {
	if err := CheckManagementPort("ens224", ManagementFacts{}); err != nil {
		t.Fatalf("事实未知时不应拒绝: %v", err)
	}
	// 空名/空事实不应 panic 或误判
	if err := CheckManagementPort("", ManagementFacts{DeclaredMgmtIface: "ens160"}); err != nil {
		t.Fatalf("空口名不应被拒: %v", err)
	}
	// 业务口与默认路由同网段（真机情形：ens192/ens224 与 ens160 同在 VMnet8）
	// → 只要不被这三条事实命中就必须放行
	if err := CheckManagementPort("ens192", ManagementFacts{
		DeclaredMgmtIface: "", DefaultRouteIface: "ens160", ListenIface: "ens160",
	}); err != nil {
		t.Fatalf("同网段的业务口不应被拒（低置信度线索不作拒绝理由）: %v", err)
	}
}

func TestDefaultRouteIfaceOf(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "route")
	content := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"ens192\t000010AC\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"ens160\t00000000\t020010AC\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DefaultRouteIfaceOf(path); got != "ens160" {
		t.Fatalf("应识别默认路由所在网卡: %q", got)
	}
	// 无默认路由（只有明细路由）→ 未知
	if err := os.WriteFile(path, []byte("Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"+
		"ens192\t000010AC\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DefaultRouteIfaceOf(path); got != "" {
		t.Fatalf("无默认路由应返回空: %q", got)
	}
	// 文件不存在（非 Linux / 容器内）→ 未知，不报错
	if got := DefaultRouteIfaceOf(filepath.Join(dir, "missing")); got != "" {
		t.Fatalf("文件缺失应返回空: %q", got)
	}
}

func TestIfaceOfIP(t *testing.T) {
	// 未分配的地址（TEST-NET-1）→ 未知
	if got := IfaceOfIP("192.0.2.123"); got != "" {
		t.Fatalf("未分配地址应返回空: %q", got)
	}
	if got := IfaceOfIP("not-an-ip"); got != "" {
		t.Fatalf("非法 IP 应返回空: %q", got)
	}
	// 本机某个真实地址应能反查到网卡名（跨平台：不假定名字）
	self := firstLocalIP(t)
	if got := IfaceOfIP(self); got == "" {
		t.Fatalf("本机地址 %s 应能反查到网卡", self)
	}
}

// 解绑路径给的是 PCI 地址：用绑定记录反查口名，才判得出是不是管理口。
func TestIfaceOfPCIViaBindings(t *testing.T) {
	rec := NewBindings(filepath.Join(t.TempDir(), "b.json"))
	if err := rec.Set("ens160", "0000:1a:00.0"); err != nil {
		t.Fatal(err)
	}
	if name, ok := rec.IfaceOfPCI("0000:1a:00.0"); !ok || name != "ens160" {
		t.Fatalf("应反查到 ens160: %q %v", name, ok)
	}
	if _, ok := rec.IfaceOfPCI("0000:ff:00.0"); ok {
		t.Fatal("未知 PCI 不应命中")
	}
	// 半角大小写差异也应命中（PCI 十六进制）
	if name, ok := rec.IfaceOfPCI("0000:1A:00.0"); !ok || name != "ens160" {
		t.Fatalf("大小写不敏感: %q %v", name, ok)
	}
}

// firstLocalIP 取本机第一个非回环、非链路本地地址（跨平台，不假定网卡名）。
func firstLocalIP(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skip("取不到本机地址")
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			return ipn.IP.String()
		}
	}
	t.Skip("无可用本机地址")
	return ""
}

// 解绑路径可能给 PCI 地址：必须能解析成口名，否则「按 PCI 解绑管理口」拦不住。
func TestResolveIfaceName(t *testing.T) {
	root := t.TempDir()
	// 内核仍绑着网卡：/sys/bus/pci/devices/<pci>/net/<name>
	// 注：夹具用连字符形式的 PCI —— Windows 文件名不允许冒号，而本包单测在开发机上也要跑
	//（与 dpdkbind_test.go 同一取舍）；此处 PCI 只作为路径片段，不解析其格式。
	kernDir := filepath.Join(root, "bus", "pci", "devices", "0000-1a-00.0", "net", "ens160")
	if err := os.MkdirAll(kernDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := KernelIfaceOfPCIIn(root, "0000-1a-00.0"); got != "ens160" {
		t.Fatalf("内核网卡名解析失败: %q", got)
	}
	// 已交 DPDK：内核无 netdev 目录 → 由绑定记录兜底
	rec := NewBindings(filepath.Join(t.TempDir(), "b.json"))
	if err := rec.Set("ens224", "0000:13:00.0"); err != nil {
		t.Fatal(err)
	}
	// 口名直接通过
	if name, ok := ResolveIfaceName("ens192", rec); !ok || name != "ens192" {
		t.Fatalf("口名应直接通过: %q %v", name, ok)
	}
	// 空值/认不出的 PCI → 未知
	if _, ok := ResolveIfaceName("", rec); ok {
		t.Fatal("空值应为未知")
	}
	if _, ok := ResolveIfaceName("0000:ff:00.0", rec); ok {
		t.Fatal("记录与内核都没有的 PCI 应为未知（不拦，避免堵住带外修复）")
	}
}
