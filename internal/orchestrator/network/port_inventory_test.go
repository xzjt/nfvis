package network

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
