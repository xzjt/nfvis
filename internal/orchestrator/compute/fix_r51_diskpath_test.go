package compute

// round51 G1：compute **不信任任何由配置字段拼出的落盘路径**（纵深防御第二层）。
//
// 第一层是 model 层的名字格式校验；本层不问名字长什么样，只问「拼出来的路径是否仍在基准目录
// 之内」——真正以 root 执行 EnsureDir / qemu-img create / 克隆的是本包，任何绕过格式校验的
// 入口（REST 直写、历史遗留配置、将来新增的调用方）都会在这里被拦住。用例走**真实路径**
// （DefineVM → specFor → prepare），并配正控证明合法名字不受影响。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// 越界盘名：`data-` 前缀会吃掉一格，故需多上跳几级才能真正走出 <vmsDir>/<vmName>。
const escapingDisk = "x/../../../../../../etc/cron.d/nfvis"

func TestFixR51DataDiskPathChecked(t *testing.T) {
	// 合法名（含点、但不含路径段）不受影响——不得把普通名字误判为越界。
	if got, err := DataDiskPathChecked("/vms", "fw-vm", "data0"); err != nil || got != "/vms/fw-vm/data-data0.qcow2" {
		t.Fatalf("合法数据盘路径应通过: got=%q err=%v", got, err)
	}
	if _, err := DataDiskPathChecked("/vms", "fw-vm", "a..b"); err != nil {
		t.Fatalf("含点但无路径段的名字不该被拒: %v", err)
	}

	_, err := DataDiskPathChecked("/vms", "fw-vm", escapingDisk)
	var pe *DerivedPathError
	if !errors.As(err, &pe) {
		t.Fatalf("越界盘名应报 DerivedPathError: %v", err)
	}
	if !errors.Is(err, ErrDerivedPathEscape) {
		t.Fatalf("应可 errors.Is(ErrDerivedPathEscape): %v", err)
	}
	// 错误要能照着排查：点名 VM、哪个盘、越界到了哪。
	for _, want := range []string{"fw-vm", escapingDisk, "/etc/cron.d/nfvis"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("报错应含 %q: %v", want, err)
		}
	}
	if pe.Path != "/etc/cron.d/nfvis.qcow2" {
		t.Fatalf("越界路径解析不符: %q", pe.Path)
	}
}

// DefineVM 走真实路径（specFor）：越界盘名在**落盘之前**被拒，且什么都没落下。
func TestFixR51DefineVMRejectsEscapingDataDiskName(t *testing.T) {
	api := newMockLibvirt()
	store := newMockStorage("/images/img.qcow2")
	p := newTestProvider(api, store, &mockSeed{})

	vm := vmFixture("fw-vm")
	vm.Disks = []model.VMDisk{{Name: escapingDisk, SizeGB: 1}}

	if err := p.DefineVM(context.Background(), vm, model.AllocatedResources{}); !errors.Is(err, ErrDerivedPathEscape) {
		t.Fatalf("越界盘名应被拒: %v", err)
	}
	if len(store.dirs) != 0 || len(store.blanks) != 0 || len(store.clones) != 0 || len(api.defined) != 0 {
		t.Fatalf("拒绝路径上不得建目录/建盘/克隆/定义域: dirs=%v blanks=%v clones=%v defined=%v",
			store.dirs, store.blanks, store.clones, api.defined)
	}
}

// prepare 是**真正落盘**的那一道：即便调用方绕过 specFor 递进来一个越界 Path，也挡在
// EnsureDir/CreateBlank 之前（"不能只校验一处"）。
func TestFixR51PrepareRejectsEscapingDataDiskPath(t *testing.T) {
	api := newMockLibvirt()
	store := newMockStorage("/images/img.qcow2", "/vms/fw-vm/disk.qcow2")
	p := newTestProvider(api, store, &mockSeed{})

	vm := vmFixture("fw-vm")
	spec := DomainSpec{
		VM:         vm,
		DiskFormat: DefaultDiskFormat,
		DiskPath:   "/vms/fw-vm/disk.qcow2", // 已存在 ⇒ 主盘阶段直接跳过
		DataDisks:  []DataDiskSpec{{Name: "data0", Path: "/etc/cron.d/evil.qcow2"}},
	}
	if err := p.prepare(context.Background(), vm, spec); !errors.Is(err, ErrDerivedPathEscape) {
		t.Fatalf("prepare 应复核数据盘路径: %v", err)
	}
	if len(store.dirs) != 0 || len(store.blanks) != 0 || len(store.clones) != 0 {
		t.Fatalf("拒绝路径上不得建目录/建盘/克隆: dirs=%v blanks=%v clones=%v", store.dirs, store.blanks, store.clones)
	}
}

// 数据盘**源镜像**同样是用户输入：`image: ../vms/victim/disk` 会克隆**别的 VM 的盘**。
func TestFixR51RejectsEscapingDataDiskImage(t *testing.T) {
	api := newMockLibvirt()
	store := newMockStorage("/images/img.qcow2")
	p := newTestProvider(api, store, &mockSeed{})

	vm := vmFixture("fw-vm")
	vm.Disks = []model.VMDisk{{Name: "data0", Image: "../../vms/victim/disk"}}

	if err := p.DefineVM(context.Background(), vm, model.AllocatedResources{}); !errors.Is(err, ErrDerivedPathEscape) {
		t.Fatalf("越界源镜像应被拒: %v", err)
	}
	// 决定性断言：绝不能把别的 VM 的盘当克隆源（主盘克隆此前已合法发生，故只查与数据盘有关的）。
	for _, c := range store.clones {
		if strings.Contains(c, "victim") || strings.Contains(c, "data-data0") {
			t.Fatalf("越界镜像不应被克隆: %v", store.clones)
		}
	}
}

// 主盘镜像（克隆源 / ISO cdrom 源）同样过同一道复核。
func TestFixR51RejectsEscapingMainDiskImage(t *testing.T) {
	api := newMockLibvirt()
	store := newMockStorage("/images/img.qcow2")
	p := newTestProvider(api, store, &mockSeed{})

	vm := vmFixture("fw-vm")
	vm.Image = "../victim/disk.qcow2"

	if err := p.DefineVM(context.Background(), vm, model.AllocatedResources{}); !errors.Is(err, ErrDerivedPathEscape) {
		t.Fatalf("越界主盘镜像应被拒: %v", err)
	}
	if len(store.clones) != 0 {
		t.Fatalf("拒绝路径上不应发生克隆: %v", store.clones)
	}
}

// 正控：仓库内的合法镜像仍照常克隆（不因加固而误拒）。
func TestFixR51LegitDataDiskImageStillClones(t *testing.T) {
	api := newMockLibvirt()
	store := newMockStorage("/images/img.qcow2", "/images/data.qcow2")
	p := newTestProvider(api, store, &mockSeed{})

	vm := vmFixture("fw-vm")
	vm.Disks = []model.VMDisk{{Name: "data0", Image: "data.qcow2"}}

	// 大页页大小取自资源分配（与既有 DefineVM 用例同形）。
	alloc := model.AllocatedResources{Cores: []int{6, 7}, HugepageSize: "1G"}
	if err := p.DefineVM(context.Background(), vm, alloc); err != nil {
		t.Fatalf("仓库内数据盘镜像应通过: %v", err)
	}
	want := "/images/data.qcow2->/vms/fw-vm/data-data0.qcow2"
	if len(store.clones) != 2 || store.clones[1] != want {
		t.Fatalf("应从仓库镜像克隆数据盘（主盘克隆在前）: %v", store.clones)
	}
}

func TestFixR51ContainedUnder(t *testing.T) {
	cases := []struct {
		base, target string
		want         bool
	}{
		{"/images", "/images/a.qcow2", true},
		{"/images", "/images/sub/a.qcow2", true},
		{"/images", "/images", false},           // base 自身不算「之内」
		{"/images", "/images-sibling/a", false}, // 前缀巧合但不是子目录
		{"/images", "/vms/victim/disk", false},
		{"/images", "/etc/passwd", false},
		{"images", "images/a.qcow2", true}, // 相对基准同样成立
		{"images", "a.qcow2", false},
	}
	for _, c := range cases {
		if got := containedUnder(c.base, c.target); got != c.want {
			t.Errorf("containedUnder(%q,%q)=%v want %v", c.base, c.target, got, c.want)
		}
	}
}
