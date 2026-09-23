package compute

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// baseSpec 返回一台最小可用 VM 的组装输入；各用例按需改写。
func baseSpec() DomainSpec {
	return DomainSpec{
		VM: model.VMFunction{
			Name:      "fw-vm",
			Image:     "img-ubuntu",
			VCPU:      model.VMCpu{Count: 2},
			Memory:    model.VMMemory{SizeMB: 1024},
			Disks:     []model.VMDisk{{Name: "data0", SizeGB: 8}},
			CloudInit: &model.CloudInit{Hostname: "fw"},
		},
		Cores:        []int{6, 7},
		HugepageSize: "1G",
		DiskPath:     "/var/lib/nfvis/vms/fw-vm/disk.qcow2",
	}
}

func mustXML(t *testing.T, spec DomainSpec) string {
	t.Helper()
	out, err := BuildDomainXML(spec)
	if err != nil {
		t.Fatalf("BuildDomainXML 失败: %v", err)
	}
	return out
}

func assertContains(t *testing.T, xml string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(xml, w) {
			t.Errorf("XML 缺少片段 %q\n--- XML ---\n%s", w, xml)
		}
	}
}

func assertNotContains(t *testing.T, xml string, notWant ...string) {
	t.Helper()
	for _, w := range notWant {
		if strings.Contains(xml, w) {
			t.Errorf("XML 不应包含片段 %q\n--- XML ---\n%s", w, xml)
		}
	}
}

// 大页内存 + 绑核 + vhost-user 共享内存（M4-1 验收核心）。
func TestBuildDomainXML_HugepagesPinningVhostUser(t *testing.T) {
	spec := baseSpec()
	spec.Interfaces = []InterfaceSpec{{
		Name:   "eth0",
		Type:   IfaceVhostUser,
		Socket: "/run/nfvis/vhost/fw-vm-eth0.sock",
		Queues: 2,
	}}
	xml := mustXML(t, spec)

	assertContains(t, xml,
		`<domain type="kvm">`,
		`<name>fw-vm</name>`,
		`<memory unit="MiB">1024</memory>`,
		`<currentMemory unit="MiB">1024</currentMemory>`,
		`<hugepages>`,
		`<page size="1" unit="G">`,
		`<locked>`,
		`<access mode="shared">`, // vhost-user 需要共享大页内存（FR-NET-020）
		`<vcpu placement="static">2</vcpu>`,
		`<vcpupin vcpu="0" cpuset="6">`,
		`<vcpupin vcpu="1" cpuset="7">`,
		`<interface type="vhostuser">`,
		`<source type="unix" mode="client" path="/run/nfvis/vhost/fw-vm-eth0.sock">`,
		`<model type="virtio">`,
		`<driver queues="2">`,
		`<reconnect enabled="yes" timeout="5">`, // FR-OPS-011：VPP 重启后自动重连
	)
}

// 未指定 MAC 时确定性生成；同参数恒等、不同网卡不同（FR-CFG-011③）。
func TestBuildDomainXML_DefaultMACDeterministic(t *testing.T) {
	spec := baseSpec()
	spec.Interfaces = []InterfaceSpec{
		{Name: "eth0", Type: IfaceVhostUser, Socket: "/s/eth0.sock"},
		{Name: "eth1", Type: IfaceVhostUser, Socket: "/s/eth1.sock"},
	}
	first := mustXML(t, spec)
	second := mustXML(t, spec)
	if first != second {
		t.Fatal("同一 spec 两次组装结果不一致（MAC 生成必须确定）")
	}

	mac0 := DefaultMAC("fw-vm", "eth0")
	mac1 := DefaultMAC("fw-vm", "eth1")
	if mac0 == mac1 {
		t.Fatalf("不同 vNIC 生成了相同 MAC: %s", mac0)
	}
	if mac0 != DefaultMAC("fw-vm", "eth0") {
		t.Fatal("DefaultMAC 非确定")
	}
	// 本地管理地址：首字节 bit1 置位、bit0（组播）清零。
	firstByte := mac0[:2]
	if firstByte != "52" {
		t.Fatalf("MAC 前缀应为 52（本地管理），实际 %s", mac0)
	}
	assertContains(t, first, `address="`+mac0+`"`, `address="`+mac1+`"`)
}

// 显式 MAC 优先于缺省生成。
func TestBuildDomainXML_ExplicitMAC(t *testing.T) {
	spec := baseSpec()
	spec.Interfaces = []InterfaceSpec{{
		Name: "eth0", Type: IfaceVhostUser, Socket: "/s/e.sock", MAC: "52:54:00:aa:bb:cc",
	}}
	xml := mustXML(t, spec)
	assertContains(t, xml, `address="52:54:00:aa:bb:cc"`)
	assertNotContains(t, xml, `address="`+DefaultMAC("fw-vm", "eth0")+`"`)
}

// backing=normal + 无 vhost-user：不写大页与 numatune；串口缺省启用（FR-CMP-014）。
func TestBuildDomainXML_NormalBackingDefaultsSerialOn(t *testing.T) {
	spec := baseSpec()
	spec.HugepageSize = ""
	spec.VM.Memory.Backing = "normal"
	xml := mustXML(t, spec)

	assertNotContains(t, xml, "<memoryBacking>", "<hugepages>")
	assertContains(t, xml,
		`<serial type="pty">`,
		`<console type="pty">`,
		`<target type="serial" port="0">`,
	)
}

// serial_console=false 时不生成串口/console。
func TestBuildDomainXML_SerialConsoleDisabled(t *testing.T) {
	spec := baseSpec()
	off := false
	spec.VM.SerialConsole = &off
	xml := mustXML(t, spec)
	assertNotContains(t, xml, "<serial", "<console")
}

// 附加数据盘 virtio + cloud-init seed cdrom（FR-CMP-016/018）。
func TestBuildDomainXML_DataDisksAndSeedISO(t *testing.T) {
	spec := baseSpec()
	spec.SeedISO = "/var/lib/nfvis/vms/fw-vm/seed.iso"
	spec.DataDisks = []DataDiskSpec{
		{Name: "data0", Path: "/var/lib/nfvis/vms/fw-vm/data0.qcow2"},
		{Name: "data1", Path: "/var/lib/nfvis/vms/fw-vm/data1.qcow2", Format: "raw"},
	}
	xml := mustXML(t, spec)

	assertContains(t, xml,
		`<source file="/var/lib/nfvis/vms/fw-vm/data0.qcow2">`,
		`<target dev="vdb" bus="virtio">`,
		`<source file="/var/lib/nfvis/vms/fw-vm/data1.qcow2">`,
		`<target dev="vdc" bus="virtio">`,
		`<driver name="qemu" type="raw">`,
		// seed 走 **virtio 磁盘**（决策 #139，收口 #22）：此前是 sata 光盘，缺 ahci 的 guest
		// 内核看不到 → cloud-init 静默自禁。设备名接在数据盘之后（vdb/vdc → vdd）。
		`<source file="/var/lib/nfvis/vms/fw-vm/seed.iso">`,
		`<target dev="vdd" bus="virtio">`,
		`<readonly>`,
	)
}

// ISO 镜像按 cdrom 挂载并从光驱引导。
func TestBuildDomainXML_ISOImageBootsFromCdrom(t *testing.T) {
	spec := baseSpec()
	spec.DiskPath = "/var/lib/nfvis/images/installer.iso"
	spec.DiskFormat = "iso"
	spec.SeedISO = "/var/lib/nfvis/vms/fw-vm/seed.iso"
	xml := mustXML(t, spec)

	assertContains(t, xml,
		`<boot dev="cd">`,
		`<disk type="file" device="cdrom">`,
		`<target dev="vdb" bus="virtio">`, // seed 走 virtio（决策 #139）
		`<target dev="vdb" bus="virtio">`, // seed 走 virtio（决策 #139）
	)
}

// SR-IOV VF 直通：hostdev + VF PCI 地址（FR-NET-021，本环境不可真机验证）。
func TestBuildDomainXML_SriovHostdev(t *testing.T) {
	spec := baseSpec()
	spec.Interfaces = []InterfaceSpec{{
		Name: "eth1", Type: IfaceSriovVF, VFPCI: "0000:0b:10.1",
	}}
	xml := mustXML(t, spec)
	assertContains(t, xml,
		`<hostdev mode="subsystem" type="pci" managed="yes">`,
		`<address domain="0x0000" bus="0x0b" slot="0x10" function="0x1">`,
	)
	assertNotContains(t, xml, "vhostuser")
}

// MAC 重复不在此层拦截（FR-CFG-011③ 在 M4-2/3 校验），此处仅确认两网卡都落 XML。
func TestBuildDomainXML_MultipleVhostUser(t *testing.T) {
	spec := baseSpec()
	spec.Interfaces = []InterfaceSpec{
		{Name: "eth0", Type: IfaceVhostUser, Socket: "/s/0.sock", TargetDev: "vnet0"},
		{Name: "eth1", Type: IfaceVhostUser, Socket: "/s/1.sock", TargetDev: "vnet1"},
	}
	xml := mustXML(t, spec)
	assertContains(t, xml, "/s/0.sock", "/s/1.sock", `<target dev="vnet0">`, `<target dev="vnet1">`)
}

// NUMA 亲和在账本给出节点时写入 numatune（FR-CMP-002）。
func TestBuildDomainXML_NumaTune(t *testing.T) {
	spec := baseSpec()
	node := 0
	spec.NumaNode = &node
	xml := mustXML(t, spec)
	assertContains(t, xml, `<numatune>`, `<memory mode="strict" nodeset="0">`)
}

func TestBuildDomainXML_Errors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*DomainSpec)
		wantSub string
	}{
		{"空名", func(s *DomainSpec) { s.VM.Name = "" }, "VM 名不能为空"},
		{"vcpu 为 0", func(s *DomainSpec) { s.VM.VCPU.Count = 0 }, "vcpu.count 必须 > 0"},
		{"内存为 0", func(s *DomainSpec) { s.VM.Memory.SizeMB = 0 }, "memory.size-mb 必须 > 0"},
		{"缺主盘", func(s *DomainSpec) { s.DiskPath = "" }, "缺少主盘路径"},
		{"vhost-user 缺 socket", func(s *DomainSpec) {
			s.Interfaces = []InterfaceSpec{{Name: "eth0", Type: IfaceVhostUser}}
		}, "缺少 socket 路径"},
		{"sriov 缺 VF", func(s *DomainSpec) {
			s.Interfaces = []InterfaceSpec{{Name: "eth0", Type: IfaceSriovVF}}
		}, "缺少 VF PCI 地址"},
		{"sriov VF 地址非法", func(s *DomainSpec) {
			s.Interfaces = []InterfaceSpec{{Name: "eth0", Type: IfaceSriovVF, VFPCI: "0b:10.1"}}
		}, "格式非法"},
		{"未知 vNIC 类型", func(s *DomainSpec) {
			s.Interfaces = []InterfaceSpec{{Name: "eth0", Type: "tap"}}
		}, "不受支持"},
		{"大页页大小缺失", func(s *DomainSpec) { s.HugepageSize = "" }, "未指定大页页大小"},
		{"大页页大小非法", func(s *DomainSpec) { s.HugepageSize = "4K" }, "不支持的大页页大小"},
		{"数据盘缺路径", func(s *DomainSpec) {
			s.DataDisks = []DataDiskSpec{{Name: "d0"}}
		}, "数据盘 d0 缺少路径"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := baseSpec()
			tc.mutate(&spec)
			_, err := BuildDomainXML(spec)
			if err == nil {
				t.Fatalf("应报错但成功了")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误信息应含 %q，实际: %v", tc.wantSub, err)
			}
		})
	}
}

// backing=normal 时忽略 HugepageSize（不校验）；供 M4-2 校验⑤/① 用。
func TestBuildDomainXML_NormalBackingWithVhostUserStillNoHugepages(t *testing.T) {
	spec := baseSpec()
	spec.HugepageSize = ""
	spec.VM.Memory.Backing = "normal"
	spec.Interfaces = []InterfaceSpec{{Name: "eth0", Type: IfaceVhostUser, Socket: "/s/e.sock"}}
	xml := mustXML(t, spec)
	// ① 的拦截在 M4-2 校验层（vhost-user 必须大页）；本层只按 backing 组装。
	assertNotContains(t, xml, "<hugepages>")
	assertContains(t, xml, `type="vhostuser"`)
}

func TestFormatCPUSet(t *testing.T) {
	cases := []struct {
		in   []int
		want string
	}{
		{nil, ""},
		{[]int{5}, "5"},
		{[]int{5, 6, 7}, "5-7"},
		{[]int{1, 2, 3, 5, 9, 10}, "1-3,5,9-10"},
		{[]int{7, 6, 5}, "5-7"}, // 排序
		{[]int{5, 5, 6}, "5-6"}, // 去重
		{[]int{0, 2, 4}, "0,2,4"},
	}
	for _, tc := range cases {
		if got := FormatCPUSet(tc.in); got != tc.want {
			t.Errorf("FormatCPUSet(%v) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestHugepageSpec(t *testing.T) {
	if s, u, err := HugepageSpec("1G"); err != nil || s != 1 || u != "G" {
		t.Fatalf("1G → (%d,%q,%v)", s, u, err)
	}
	if s, u, err := HugepageSpec("2M"); err != nil || s != 2 || u != "M" {
		t.Fatalf("2M → (%d,%q,%v)", s, u, err)
	}
	if _, _, err := HugepageSpec(""); err == nil {
		t.Fatal("空页大小应报错")
	}
	if _, _, err := HugepageSpec("1M"); err == nil {
		t.Fatal("非法页大小应报错")
	}
}

func TestParsePCI(t *testing.T) {
	addr, err := ParsePCI("0000:0b:10.1")
	if err != nil {
		t.Fatal(err)
	}
	if *addr.Domain != 0 || *addr.Bus != 0x0b || *addr.Slot != 0x10 || *addr.Function != 1 {
		t.Fatalf("解析结果错误: %+v", addr)
	}
	if _, err := ParsePCI("0000:0b"); err == nil {
		t.Fatal("字段数不足应报错")
	}
	if _, err := ParsePCI("0000:0b:10"); err == nil {
		t.Fatal("缺 function 应报错")
	}
	if _, err := ParsePCI("zzzz:0b:10.1"); err == nil {
		t.Fatal("非法十六进制应报错")
	}
	if _, err := ParsePCI("0000:0b:xx.1"); err == nil {
		t.Fatal("非法 slot 应报错")
	}
	if _, err := ParsePCI("0000:0b:10.yy"); err == nil {
		t.Fatal("非法 function 应报错")
	}
}

func TestDiskDevName(t *testing.T) {
	if got := diskDevName(1); got != "vdb" {
		t.Fatalf("1 → %q，期望 vdb", got)
	}
	if got := diskDevName(25); got != "vdz" {
		t.Fatalf("25 → %q，期望 vdz", got)
	}
	if got := diskDevName(26); got != "vdaa" {
		t.Fatalf("26 → %q，期望 vdaa", got)
	}
	if got := diskDevName(52); got != "vdba" {
		t.Fatalf("52 → %q，期望 vdba", got)
	}
}

func TestFormatLibVersion(t *testing.T) {
	cases := map[uint64]string{
		12_000_000: "12.0.0",
		12_000_007: "12.0.7",
		10_002_001: "10.2.1",
		0:          "0.0.0",
	}
	for in, want := range cases {
		if got := FormatLibVersion(in); got != want {
			t.Errorf("FormatLibVersion(%d) = %q，期望 %q", in, got, want)
		}
	}
}
