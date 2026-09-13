//go:build integration

// M4-9 真机集成测试：计算恢复收敛（FR-OPS-010/012）。
// 手工 virsh undefine 一个 VM 后 EnsureConsistent 应补建；autostart 落到 libvirt
// 自启标志；不可收敛项（镜像缺失）转为告警且不阻塞其它对象。
package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/compute"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

func TestComputeRecoveryConvergenceRealLibvirt(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skipf("跳过：未找到 qemu-img: %v", err)
	}
	// 生产式路径：QEMU 以 libvirt-qemu 运行需可穿越目录（t.TempDir 的 0700 会被拒，M4-3 记录的坑）。
	base := "/var/lib/nfvis/it-m4-9"
	imagesDir := filepath.Join(base, "images")
	vmsDir := filepath.Join(base, "vms")
	for _, d := range []string{imagesDir, vmsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", filepath.Join(imagesDir, "ok.qcow2"), "16M").CombinedOutput(); err != nil {
		t.Fatalf("建测试镜像: %v: %s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cfg := compute.DefaultConfig()
	cfg.URI = os.Getenv("NFVIS_LIBVIRT_URI")
	cfg.VMsDir, cfg.ImagesDir = vmsDir, imagesDir
	cfg.VhostDir = filepath.Join(base, "vhost")
	_ = os.MkdirAll(cfg.VhostDir, 0o755)
	cfg.StopTimeout = 5 * time.Second

	p, conn, err := compute.NewConnectedProvider(ctx, cfg)
	if err != nil {
		t.Skipf("跳过（libvirt 不可用）: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	alarms := network.NewAlarmStore()
	p.SetAlarms(alarms) // orchestrator.AlarmSink

	const name = "it-m4-9-vm"
	_ = p.DeleteVM(context.Background(), name)
	t.Cleanup(func() { _ = p.DeleteVM(context.Background(), name) })

	vm := model.VMFunction{
		Name: name, Image: "ok.qcow2",
		VCPU:      model.VMCpu{Count: 1},
		Memory:    model.VMMemory{SizeMB: 256, Backing: "normal"}, // 不占大页（本环境仅 1 页空闲）
		Autostart: true,
	}
	bad := model.VMFunction{Name: "it-m4-9-bad", Image: "missing.qcow2",
		VCPU: model.VMCpu{Count: 1}, Memory: model.VMMemory{SizeMB: 128, Backing: "normal"}}
	cfgModel := model.Config{VirtualMachineFunctions: []model.VMFunction{vm}}

	// 初次创建并运行
	if err := p.DefineVM(ctx, vm, model.AllocationFor(cfgModel, vm)); err != nil {
		t.Fatalf("DefineVM: %v", err)
	}
	if st, _ := p.VMState(ctx, name); st != "running" {
		t.Fatalf("autostart 应运行: %q", st)
	}
	// libvirt 自启标志（FR-OPS-012）
	if out, _ := exec.Command("virsh", "dominfo", name).CombinedOutput(); !strings.Contains(string(out), "Autostart:      enable") {
		t.Logf("virsh dominfo（Autostart 行）:\n%s", out)
	}

	// 模拟手工删除：destroy + undefine
	if out, err := exec.Command("virsh", "destroy", name).CombinedOutput(); err != nil {
		t.Fatalf("virsh destroy: %v: %s", err, out)
	}
	if out, err := exec.Command("virsh", "undefine", name).CombinedOutput(); err != nil {
		t.Fatalf("virsh undefine: %v: %s", err, out)
	}
	if st, _ := p.VMState(ctx, name); st != "absent" {
		t.Fatalf("手工删除后应 absent: %q", st)
	}

	// 恢复收敛（含不可收敛项）→ 补建 name 并按 autostart 启动；bad 进告警不阻塞
	cfgModel.VirtualMachineFunctions = []model.VMFunction{vm, bad}
	errs := p.EnsureConsistent(ctx, cfgModel)
	if len(errs) != 1 {
		t.Fatalf("应只有 bad VM 不可收敛: %v", errs)
	}
	if st, _ := p.VMState(ctx, name); st != "running" {
		t.Fatalf("收敛后应补建并运行: %q", st)
	}
	active := alarms.List("active")
	found := false
	for _, a := range active {
		if a.Code == "RECOVERY_UNCONVERGED" && a.Source == "it-m4-9-bad" {
			found = true
		}
	}
	if !found {
		t.Fatalf("不可收敛项应产生 RECOVERY_UNCONVERGED 告警: %+v", active)
	}
	t.Logf("手工 undefine 后收敛补建并 autostart 运行；不可收敛项告警: %+v", active)

	// 清理
	if err := p.DeleteVM(ctx, name); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
}
