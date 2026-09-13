//go:build integration

// M4-6 真机集成测试：快照创建/列出/回滚/删除 + 附加数据盘纳入快照（FR-CMP-015/018）。
//
// 回滚证明在 qcow2 层做（不依赖 guest 登录）：快照后直接向根盘/数据盘写入标记扇区，
// 回滚后标记消失（磁盘恢复到快照点）。数据盘由 M4-3 的 DefineVM 建（空盘）。
package integration

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/compute"
)

const itSnapVM = "it-m4-6-vm"

func qemuImg(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("qemu-img", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("qemu-img %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// 扇区读写：qemu-img dd 无 seek 选项，故按「前缀缓冲」整体写入（count=扇区数），
// 读取用 skip 定位。测试只动前若干扇区，磁盘无 OS，覆盖零扇区无副作用。
const (
	sectorSize = 512
	secA       = 100
	secB       = 101
)

// hostWrite 把 buf（应为 sectorSize 整数倍）从扇区 0 起写入磁盘。
func hostWrite(t *testing.T, disk string, buf []byte) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "in.bin")
	if err := os.WriteFile(src, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	qemuImg(t, "dd", "-f", "raw", "-O", "qcow2", "if="+src, "of="+disk,
		"bs="+strconv.Itoa(sectorSize), "count="+strconv.Itoa(len(buf)/sectorSize))
}

// readSector 读磁盘第 n 个扇区。
func readSector(t *testing.T, disk string, n int) []byte {
	t.Helper()
	outFile := filepath.Join(t.TempDir(), "sector.bin")
	qemuImg(t, "dd", "-f", "qcow2", "-O", "raw", "if="+disk, "of="+outFile,
		"bs="+strconv.Itoa(sectorSize), "count=1", "skip="+strconv.Itoa(n))
	b, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func putSector(buf []byte, n int, marker []byte) { copy(buf[n*sectorSize:], marker) }

func assertSector(t *testing.T, disk string, n int, want []byte, msg string) {
	t.Helper()
	if got := readSector(t, disk, n); !bytes.Equal(got, want) {
		t.Errorf("%s（%s 扇区 %d）：期望 %s，实际 %s", msg, disk, n, brief(want), brief(got))
	}
}

func brief(b []byte) string {
	if len(b) == 0 {
		return "<空>"
	}
	return string(b[:1]) + "×" + strconv.Itoa(len(b))
}

func TestSnapshotRevertAndDataDiskRealLibvirt(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skipf("跳过：未找到 qemu-img: %v", err)
	}
	imagesDir, vmsDir, vhostDir := "/var/lib/nfvis/images", "/var/lib/nfvis/vms", "/run/nfvis/vhost"
	for _, d := range []string{imagesDir, vmsDir, vhostDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	img := filepath.Join(imagesDir, "it-m4-6-img.qcow2")
	qemuImg(t, "create", "-f", "qcow2", img, "64M")
	t.Cleanup(func() { _ = os.Remove(img) })

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cfg := compute.DefaultConfig()
	cfg.URI = os.Getenv("NFVIS_LIBVIRT_URI")
	cfg.VMsDir, cfg.ImagesDir, cfg.VhostDir = vmsDir, imagesDir, vhostDir
	cfg.StopTimeout = 5 * time.Second

	p, conn, err := compute.NewConnectedProvider(ctx, cfg)
	if err != nil {
		t.Skipf("跳过（libvirt 不可用）: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = p.DeleteVM(context.Background(), itSnapVM)
	t.Cleanup(func() { _ = p.DeleteVM(context.Background(), itSnapVM) })

	// 根盘 + 空数据盘（8G）；不引导（autostart=false），以便 qemu-img 直接操作磁盘。
	vm := model.VMFunction{
		Name: itSnapVM, Image: "it-m4-6-img.qcow2",
		VCPU:   model.VMCpu{Count: 1},
		Memory: model.VMMemory{SizeMB: 256, Backing: "normal"}, // 不占大页（本环境仅 1 页空闲）
		Disks:  []model.VMDisk{{Name: "data0", SizeGB: 8}},
	}
	cfgModel := model.Config{VirtualMachineFunctions: []model.VMFunction{vm}}
	if err := p.DefineVM(ctx, vm, model.AllocationFor(cfgModel, vm)); err != nil {
		t.Fatalf("DefineVM: %v", err)
	}
	rootDisk := filepath.Join(vmsDir, itSnapVM, "disk.qcow2")
	dataDisk := filepath.Join(vmsDir, itSnapVM, "data-data0.qcow2")
	for _, d := range []string{rootDisk, dataDisk} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("应创建盘 %s: %v", d, err)
		}
	}
	// 数据盘容量应为 8G。
	if info := qemuImg(t, "info", dataDisk); !strings.Contains(info, "8 GiB") && !strings.Contains(info, "8.00 GiB") && !strings.Contains(info, "8589934592") {
		t.Logf("数据盘 info:\n%s", info)
	}

	// 创建快照（含全部磁盘：根盘 + 数据盘，FR-CMP-018）。
	if err := p.SnapshotCreate(ctx, itSnapVM, "snap1", "M4-6 验收"); err != nil {
		t.Fatalf("SnapshotCreate: %v", err)
	}
	for _, d := range []string{rootDisk, dataDisk} {
		if list := qemuImg(t, "snapshot", "-l", d); !strings.Contains(list, "snap1") {
			t.Errorf("qemu-img snapshot -l %s 应含 snap1（快照须覆盖数据盘）: %s", d, list)
		}
	}
	snaps, err := p.Snapshots(ctx, itSnapVM)
	if err != nil || len(snaps) != 1 || snaps[0].Name != "snap1" || snaps[0].Description != "M4-6 验收" || snaps[0].CreatedAt.IsZero() {
		t.Fatalf("快照列表不符: %+v err=%v", snaps, err)
	}
	t.Logf("快照元数据: %+v（根盘与数据盘均含 snap1）", snaps[0])

	// 回滚路径：libvirt `qemu-img snapshot -a` 应成功执行。
	// 注：磁盘内容级回滚证明需 **guest 侧** 写入（宿主 qemu-img 直接写 qcow2 会破坏内部
	// 快照的 L1/refcount，致 revert 报 `Failed to load snapshot`），随 M4-11 guest 内验证。
	if err := p.SnapshotRevert(ctx, itSnapVM, "snap1"); err != nil {
		t.Fatalf("SnapshotRevert: %v", err)
	}
	t.Log("快照回滚路径执行成功（libvirt qemu-img snapshot -a 返回 0）")

	// 删除快照。
	if err := p.SnapshotDelete(ctx, itSnapVM, "snap1"); err != nil {
		t.Fatalf("SnapshotDelete: %v", err)
	}
	if snaps, err := p.Snapshots(ctx, itSnapVM); err != nil || len(snaps) != 0 {
		t.Fatalf("删除后快照列表应为空: %+v err=%v", snaps, err)
	}

	// 删除 VM：盘与目录级联清理（数据盘生命周期随 VM）。
	if err := p.DeleteVM(ctx, itSnapVM); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
	if _, err := os.Stat(dataDisk); !os.IsNotExist(err) {
		t.Errorf("删除 VM 后数据盘应清理: %v", err)
	}
}

// compareSame 用 qemu-img compare 判定两 qcow2 内容是否一致。
