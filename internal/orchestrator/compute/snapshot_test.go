package compute

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

func TestBuildSnapshotXML(t *testing.T) {
	xml, err := BuildSnapshotXML("snap1", "演示", []string{"vda", "vdb"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<name>snap1</name>", "<description>演示</description>", `name="vda"`, `name="vdb"`, `snapshot="internal"`} {
		if !strings.Contains(xml, want) {
			t.Errorf("快照 XML 缺少 %q\n%s", want, xml)
		}
	}
	if _, err := BuildSnapshotXML("", "", nil); err == nil {
		t.Fatal("空快照名应报错")
	}
	// 无磁盘时不给 <disks>（libvirt 默认含全部内部快照盘）。
	xml2, _ := BuildSnapshotXML("s", "", nil)
	if strings.Contains(xml2, "<disks>") {
		t.Fatalf("无磁盘不应写 <disks>: %s", xml2)
	}
}

func TestSnapshotInfoFromXML(t *testing.T) {
	doc := `<domainsnapshot><name>snap1</name><description>d</description>
	  <creationTime>1757763000</creationTime><state>shutoff</state></domainsnapshot>`
	info, err := SnapshotInfoFromXML(doc)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "snap1" || info.Description != "d" || info.CreatedAt.Unix() != 1757763000 {
		t.Fatalf("解析不符: %+v", info)
	}
	if _, err := SnapshotInfoFromXML("<not-xml"); err == nil {
		t.Fatal("非法 XML 应报错")
	}
}

func TestDiskTargetsOf(t *testing.T) {
	xml := `<domain><devices>
	  <disk type='file' device='disk'><target dev='vda' bus='virtio'/></disk>
	  <disk type='file' device='cdrom'><target dev='sda' bus='sata'/></disk>
	  <disk type='file' device='disk'><target dev='vdb' bus='virtio'/></disk>
	</devices></domain>`
	got := DiskTargetsOf(xml)
	if len(got) != 2 || got[0] != "vda" || got[1] != "vdb" {
		t.Fatalf("应只取 device='disk' 的 target: %v", got)
	}
	if len(DiskTargetsOf("<domain/>")) != 0 {
		t.Fatal("无磁盘应返回空")
	}
}

func TestProviderSnapshotFlow(t *testing.T) {
	api := newMockLibvirt()
	api.present["fw-vm"] = true
	store := newMockStorage("/images/img.qcow2")
	p := newTestProvider(api, store, nil)
	api.domainXML = `<domain><devices>
	  <disk type='file' device='disk'><target dev='vda' bus='virtio'/></disk>
	  <disk type='file' device='disk'><target dev='vdb' bus='virtio'/></disk>
	</devices></domain>`

	ctx := context.Background()
	if err := p.SnapshotCreate(ctx, "fw-vm", "snap1", "d"); err != nil {
		t.Fatalf("SnapshotCreate: %v", err)
	}
	xml := api.snapXML["fw-vm/snap1"]
	if !strings.Contains(xml, `name="vda"`) || !strings.Contains(xml, `name="vdb"`) {
		t.Fatalf("快照应含全部磁盘（根盘+数据盘）: %s", xml)
	}
	snaps, err := p.Snapshots(ctx, "fw-vm")
	if err != nil || len(snaps) != 1 || snaps[0].Name != "snap1" {
		t.Fatalf("快照列表: %+v err=%v", snaps, err)
	}
	if err := p.SnapshotRevert(ctx, "fw-vm", "snap1"); err != nil {
		t.Fatalf("回滚: %v", err)
	}
	if err := p.SnapshotDelete(ctx, "fw-vm", "snap1"); err != nil {
		t.Fatalf("删除: %v", err)
	}
	if len(api.reverted) != 1 || len(api.deleted) != 1 {
		t.Fatalf("应记录回滚/删除: %v %v", api.reverted, api.deleted)
	}
	// 未定义域：明确 ErrVMNotFound。
	if err := p.SnapshotCreate(ctx, "ghost", "s", ""); err == nil {
		t.Fatal("不存在的域应报错")
	}
	if _, err := p.Snapshots(ctx, "ghost"); err == nil {
		t.Fatal("不存在的域列快照应报错")
	}
}

func TestProviderConsoleRequiresRunning(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProvider(api, newMockStorage(), nil)
	if _, err := p.Console(context.Background(), "ghost"); err == nil {
		t.Fatal("不存在域应报错")
	}
	api.present["fw-vm"] = true
	api.states["fw-vm"] = domShutoff
	if _, err := p.Console(context.Background(), "fw-vm"); err == nil {
		t.Fatal("关机态 console 应报错")
	}
	api.states["fw-vm"] = domRunning
	if _, err := p.Console(context.Background(), "fw-vm"); err != nil {
		t.Fatalf("运行态 console 应成功: %v", err)
	}
}

var _ = model.VMFunction{}

// TestDiskTargetsOfExcludesRaw（决策 #159）：内部快照只支持 qcow2 盘——raw 盘
// （如 #139 的 cloud-init seed，readonly virtio 盘）必须排除，否则 libvirt 拒绝
// 整个快照（round83 真机实测：internal snapshot for disk vdb unsupported for
// storage type raw）。
func TestDiskTargetsOfExcludesRaw(t *testing.T) {
	xml := `<domain><devices>
<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/vms/x/disk.qcow2'/><target dev='vda' bus='virtio'/></disk>
<disk type='file' device='disk'><driver name='qemu' type='raw'/><source file='/vms/x/seed.iso'/><readonly/><target dev='vdb' bus='virtio'/></disk>
</devices></domain>`
	got := DiskTargetsOf(xml)
	want := []string{"vda"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("raw 盘应被排除: got %v want %v", got, want)
	}
}
