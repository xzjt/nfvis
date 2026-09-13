//go:build integration

package compute

// M4-1 真机集成测试（build tag integration，CI 不跑）。
// 在 nfvis-vm 上运行：make integration（需 libvirtd 运行；NFVIS_LIBVIRT_URI 缺省 qemu:///system）。
//
// 覆盖 M4-1 验收：连接/版本探测 + domain XML 经真机 libvirt 定义后
// `virsh dumpxml` 与配置一致（绑核 cpuset、大页、vhost-user socket 路径）。

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
)

func connectOrSkip(t *testing.T) (*Conn, context.Context) {
	t.Helper()
	uri := os.Getenv("NFVIS_LIBVIRT_URI")
	if uri == "" {
		uri = DefaultURI
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	c, err := Connect(ctx, uri)
	if err != nil {
		t.Skipf("跳过 libvirt 集成测试（%s 连接失败）: %v", uri, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, ctx
}

func TestLibvirtConnectAndVersion(t *testing.T) {
	c, _ := connectOrSkip(t)

	libVer, err := c.Version()
	if err != nil {
		t.Fatalf("版本探测失败: %v", err)
	}
	hvVer, err := c.HypervisorVersion()
	if err != nil {
		t.Fatalf("hypervisor 版本探测失败: %v", err)
	}
	t.Logf("libvirt %s，hypervisor(QEMU) %s @ %s", libVer, hvVer, c.URI())

	// 真机实测 libvirt 12.0.0（docs/M4-P0-环境核验记录.md）；仅断言非空且形如 x.y.z。
	for _, v := range []string{libVer, hvVer} {
		if strings.Count(v, ".") != 2 {
			t.Fatalf("版本格式应为 major.minor.release，实际 %q", v)
		}
	}
}

func TestDefineDomainXMLRealLibvirt(t *testing.T) {
	c, ctx := connectOrSkip(t)

	const name = "it-m4-1-vm"
	sockPath := "/run/nfvis/vhost/" + name + "-eth0.sock"
	diskPath := "/var/lib/nfvis/vms/" + name + "/disk.qcow2"

	spec := DomainSpec{
		VM: model.VMFunction{
			Name:   name,
			Image:  "it-image",
			VCPU:   model.VMCpu{Count: 2},
			Memory: model.VMMemory{SizeMB: 1024, HugepageSize: "1G"},
		},
		Cores:        []int{6, 7},
		HugepageSize: "1G",
		DiskPath:     diskPath,
		SeedISO:      "/var/lib/nfvis/vms/" + name + "/seed.iso",
		Interfaces: []InterfaceSpec{
			{Name: "eth0", Type: IfaceVhostUser, Socket: sockPath, Queues: 2},
		},
	}
	xml, err := BuildDomainXML(spec)
	if err != nil {
		t.Fatalf("组装 XML 失败: %v", err)
	}

	// 遗留清理（上次失败可能留下定义）。
	_ = c.Undefine(ctx, name)
	if err := c.Define(ctx, xml); err != nil {
		t.Fatalf("定义 domain 失败: %v\n--- XML ---\n%s", err, xml)
	}
	t.Cleanup(func() { _ = c.Undefine(context.Background(), name) })

	dumped, err := c.DumpXML(ctx, name)
	if err != nil {
		t.Fatalf("dumpxml 失败: %v", err)
	}
	t.Logf("virsh dumpxml 摘要:\n%s", dumped)

	// libvirt 规范化后（KiB 页、单引号属性）应与配置语义一致。
	for _, want := range []string{
		"<name>" + name + "</name>",
		"<vcpu placement='static'>2</vcpu>",
		"<vcpupin vcpu='0' cpuset='6'/>",
		"<vcpupin vcpu='1' cpuset='7'/>",
		"<hugepages>",
		"<page size='1048576' unit='KiB'/>", // 1G 大页（libvirt 归一为 KiB）
		"<locked/>",
		"<access mode='shared'/>",
		"<interface type='vhostuser'>",
		"<source type='unix' path='" + sockPath + "' mode='client'/>",
		"<model type='virtio'/>",
		"<driver queues='2'/>",
		"<serial type='pty'>",
	} {
		if !strings.Contains(dumped, want) {
			t.Errorf("dumpxml 缺少片段 %q", want)
		}
	}

	if err := c.Undefine(ctx, name); err != nil {
		t.Fatalf("删除 domain 失败: %v", err)
	}
	// 删除后再 dump 应失败。
	if _, err := c.DumpXML(ctx, name); err == nil {
		t.Fatal("删除后仍能 dumpxml，说明未真正清理")
	}
}
