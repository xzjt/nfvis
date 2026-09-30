package network

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ifapi "go.fd.io/govpp/binapi/interface"
)

// 决策 #83：运行态端口清单——VPP 侧与内核侧必须各自正确，且都不含「虚拟接口」。

// 内核侧：只认有 `device` 链接的物理口（lo/docker0/virbr0 这类虚拟接口无 device）。
func TestKernelIfnamesOnlyPhysical(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"ens160", "ens192"} { // 物理口：有 device
		if err := os.MkdirAll(filepath.Join(root, n, "device"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"lo", "docker0", "virbr0"} { // 虚拟接口：无 device
		if err := os.MkdirAll(filepath.Join(root, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := sysfsNetRoot
	sysfsNetRoot = root
	t.Cleanup(func() { sysfsNetRoot = old })

	got, err := (&L2Network{}).KernelIfnames()
	if err != nil {
		t.Fatalf("KernelIfnames: %v", err)
	}
	if strings.Join(got, ",") != "ens160,ens192" {
		t.Fatalf("应只返回物理口且已排序: %v", got)
	}
}

// 决策 #302：内核侧事实逐文件读 sysfs——驱动/MAC/速率/状态/MTU；读不到或解析不了的
// 字段保持零值（上层「取不到就不给」，不编造）。lo 一类无 device 的虚拟口进不到清单。
func TestKernelIfFactsReadSysfs(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, files map[string]string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, name, "device"), 0o755); err != nil {
			t.Fatal(err)
		}
		for k, v := range files {
			if err := os.WriteFile(filepath.Join(root, name, k), []byte(v), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mkVirtual := func(name string, files map[string]string) { // 无 device 的虚拟接口
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
		for k, v := range files {
			if err := os.WriteFile(filepath.Join(root, name, k), []byte(v), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// ens160：全量事实（driver 是 device/ 下的符号链接，与真机 sysfs 同构）
	mk("ens160", map[string]string{
		"operstate": "up\n", "flags": "0x1003\n", "speed": "10000\n",
		"address": "00:50:56:aa:bb:cc\n", "mtu": "1500\n",
	})
	if err := os.Symlink("../../vmxnet3", filepath.Join(root, "ens160", "device", "driver")); err != nil {
		t.Fatal(err)
	}
	// ens224：operstate unknown（LinkKnown=false）、口未连 speed=-1（取不到）、无 driver 链接
	mk("ens224", map[string]string{
		"operstate": "unknown\n", "flags": "0x1002\n", "speed": "-1\n",
		"address": "00:11:22:33:44:55\n",
	})
	// lo：无 device —— 物理口判断过滤掉
	mkVirtual("lo", map[string]string{"operstate": "unknown\n"})

	old := sysfsNetRoot
	sysfsNetRoot = root
	t.Cleanup(func() { sysfsNetRoot = old })

	got, err := (&L2Network{}).KernelIfFacts()
	if err != nil {
		t.Fatalf("KernelIfFacts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应只含两个物理口（lo 无 device 被过滤）: %+v", got)
	}
	byName := map[string]KernelIfFacts{}
	for _, f := range got {
		byName[f.Name] = f
	}
	f := byName["ens160"]
	if !f.AdminUp || !f.LinkUp || !f.LinkKnown || f.SpeedMbps != 10000 ||
		f.MAC != "00:50:56:aa:bb:cc" || f.Driver != "vmxnet3" || f.MTU != 1500 {
		t.Fatalf("ens160 事实不符: %+v", f)
	}
	g := byName["ens224"]
	if g.LinkKnown || g.AdminUp {
		t.Fatalf("operstate unknown / flags 无 IFF_UP 位时不可判定: %+v", g)
	}
	if g.SpeedMbps != 0 || g.Driver != "" {
		t.Fatalf("speed 异常值应取不到、无 driver 链接应为空（不编造）: %+v", g)
	}
	if g.MAC != "00:11:22:33:44:55" || g.Name != "ens224" {
		t.Fatalf("ens224 名字与 MAC 应照实读出: %+v", g)
	}
}

// VPP 侧：取 VPP 接口名，去重排序，剔除 VPP 内置 loopback（不是物理口）。
func TestVPPIfnamesSkipsLoopbackAndSorts(t *testing.T) {
	f := newFakeL2()
	f.names = map[uint32]SwIfInfo{
		1: {Name: "ens224"},
		2: {Name: "ens192"},
		3: {Name: vppLoopbackName},
		4: {Name: "ens192"}, // 重复
		5: {Name: ""},       // 脏数据
	}
	n := &L2Network{l2: NewL2Provider(f)}

	got, err := n.VPPIfnames()
	if err != nil {
		t.Fatalf("VPPIfnames: %v", err)
	}
	if strings.Join(got, ",") != "ens192,ens224" {
		t.Fatalf("应去重排序并剔除 local0: %v", got)
	}
}

// VPP 不可用时返回错误：调用方据此退化为「仅关键字」，不得回退到「已配置接口名」。
func TestVPPIfnamesErrorWhenClientFails(t *testing.T) {
	n := &L2Network{l2: NewL2ProviderFunc(func() (L2Client, error) {
		return nil, errors.New("VPP socket 不可用")
	})}
	if _, err := n.VPPIfnames(); err == nil {
		t.Fatal("客户端不可用时应报错")
	}
	// 未装配（nil）也不得 panic
	var nilNet *L2Network
	if _, err := nilNet.VPPIfnames(); err == nil {
		t.Fatal("未接入时应报错")
	}
}

// R86-7：接口运行态清单必须把 L3 MTU 透传出来（接口读视图的「有效 MTU」取自这里；
// 此前 SwIfInfo 没有该字段，Web 控制台 MTU 列因此恒为 —）。
func TestInterfaceStatesCarriesMTU(t *testing.T) {
	f := newFakeL2()
	f.names = map[uint32]SwIfInfo{
		1: {Name: "ens192", Mtu: 9000},
		2: {Name: "ens224"}, // 未上报 MTU（0）
	}
	n := &L2Network{l2: NewL2Provider(f)}

	got, err := n.InterfaceStates()
	if err != nil {
		t.Fatalf("InterfaceStates: %v", err)
	}
	if got["ens192"].Mtu != 9000 {
		t.Fatalf("ens192 的 Mtu 应为 9000，实得 %+v", got["ens192"])
	}
	if got["ens224"].Mtu != 0 {
		t.Fatalf("未上报 MTU 的口应为 0（取不到就不给由上层决定），实得 %+v", got["ens224"])
	}
}

// R86-7：L3 MTU 取 sw_interface_details.Mtu 的下标 0（1/2/3 依次是 IP4/IP6/MPLS）；
// 长度异常时返回 0——读视图据此「取不到就不给」，不得编造。
func TestSwIfL3MTUPicksL3Index(t *testing.T) {
	if got := swIfL3MTU(&ifapi.SwInterfaceDetails{Mtu: []uint32{9000, 1500, 1500, 1500}}); got != 9000 {
		t.Fatalf("L3 MTU 应取 Mtu[0]，实得 %d（取错下标会把 IP4/IP6 的 MTU 当成 L3）", got)
	}
	if got := swIfL3MTU(&ifapi.SwInterfaceDetails{}); got != 0 {
		t.Fatalf("Mtu 缺失时应为 0，实得 %d", got)
	}
}
