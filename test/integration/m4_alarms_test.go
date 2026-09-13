//go:build integration

// M4-10 真机集成测试：异常退出 critical 告警（FR-CMP-017/022）。
// VM：kill -9 QEMU 进程 → libvirt crashed（on_crash=preserve）→ CheckVMAlarms 产生 critical。
// 容器：以非零退出码结束 → CheckContainerAlarms 产生 critical。
package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/compute"
	"github.com/xzjt/nfvis/internal/orchestrator/container"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

func TestAbnormalExitCriticalAlarms(t *testing.T) {
	base := "/var/lib/nfvis/it-m4-10"
	imagesDir, vmsDir := filepath.Join(base, "images"), filepath.Join(base, "vms")
	vhostDir := filepath.Join(base, "vhost")
	for _, d := range []string{imagesDir, vmsDir, vhostDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	img := filepath.Join(imagesDir, "it-m4-10.qcow2")
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", img, "196M").CombinedOutput(); err != nil {
		t.Fatalf("建镜像: %v: %s", err, out)
	}
	alarms := network.NewAlarmStore()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// ---- VM：kill QEMU → crashed → critical ----
	const vmName = "it-m4-10-vm"
	cfg := compute.DefaultConfig()
	cfg.URI = os.Getenv("NFVIS_LIBVIRT_URI")
	cfg.VMsDir, cfg.ImagesDir, cfg.VhostDir = vmsDir, imagesDir, vhostDir
	cfg.StopTimeout = 5 * time.Second
	p, conn, err := compute.NewConnectedProvider(ctx, cfg)
	if err != nil {
		t.Skipf("跳过 VM 告警环节（libvirt 不可用）: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	p.SetAlarms(alarms)
	_ = p.DeleteVM(context.Background(), vmName)
	t.Cleanup(func() { _ = p.DeleteVM(context.Background(), vmName) })

	vm := model.VMFunction{Name: vmName, Image: "it-m4-10.qcow2",
		VCPU: model.VMCpu{Count: 1}, Memory: model.VMMemory{SizeMB: 512, Backing: "normal"}, Autostart: true}
	cfgModel := model.Config{VirtualMachineFunctions: []model.VMFunction{vm}}
	if err := p.DefineVM(ctx, vm, model.AllocationFor(cfgModel, vm)); err != nil {
		t.Fatalf("DefineVM: %v", err)
	}
	if st, _ := p.VMState(ctx, vmName); st != "running" {
		t.Fatalf("应运行: %q", st)
	}
	// kill -9 QEMU（cmdline 含 guest=it-m4-10-vm）
	if out, err := exec.Command("pkill", "-9", "-f", "guest="+vmName).CombinedOutput(); err != nil {
		t.Fatalf("pkill QEMU: %v: %s", err, out)
	}
	deadline := time.Now().Add(30 * time.Second)
	finalState := ""
	for time.Now().Before(deadline) {
		finalState, _ = p.VMState(ctx, vmName)
		if finalState == "crashed" {
			break
		}
		out, _ := exec.Command("virsh", "list", "--all").CombinedOutput()
		t.Logf("等待 crashed：VMState=%q virsh: %s", finalState, out)
		time.Sleep(3 * time.Second)
	}
	if finalState != "crashed" {
		out, _ := exec.Command("virsh", "domstate", vmName, "--reason").CombinedOutput()
		t.Fatalf("libvirt 未进入 crashed（VMState=%q，domstate=%s）", finalState, out)
	}
	if errs := p.CheckVMAlarms(ctx, cfgModel); len(errs) != 0 {
		t.Fatalf("VM 巡检报错: %v", errs)
	}
	crit := activeCritical(alarms, "VM_CRASHED", vmName)
	if crit == nil {
		t.Fatalf("应产生 VM_CRASHED critical 告警: %+v", alarms.List("active"))
	}
	t.Logf("kill QEMU 后 critical 告警: %+v", *crit)

	// ---- 容器：非零退出 → critical ----
	if _, err := os.Stat("/var/run/docker.sock"); err == nil {
		if _, err := exec.Command("docker", "image", "inspect", "alpine:3.20").CombinedOutput(); err == nil {
			ctCfg := container.DefaultConfig()
			ctCfg.MemifDir = orchestrator.DefaultMemifDir
			cp := container.NewConnectedProvider(ctCfg)
			cp.SetAlarms(alarms)
			const ctName = "it-m4-10-ct"
			_ = cp.DeleteContainer(context.Background(), ctName)
			t.Cleanup(func() { _ = cp.DeleteContainer(context.Background(), ctName) })
			ct := model.ContainerFunction{Name: ctName, Image: "alpine:3.20",
				Command: "/bin/sh", Args: []string{"-c", "exit 3"}, Autostart: true}
			ctCfgModel := model.Config{ContainerFunctions: []model.ContainerFunction{ct}}
			if err := cp.ApplyContainer(ctx, ct); err != nil {
				t.Fatalf("ApplyContainer: %v", err)
			}
			waitFor(t, "容器结束（非零退出）", 30*time.Second, func() bool {
				st, _ := cp.ContainerState(ctx, ctName)
				return st == "exited" || st == "dead"
			})
			time.Sleep(500 * time.Millisecond)
			if errs := cp.CheckContainerAlarms(ctx, ctCfgModel); len(errs) != 0 {
				t.Fatalf("容器巡检报错: %v", errs)
			}
			if c := activeCritical(alarms, "CONTAINER_EXITED", ctName); c == nil {
				t.Fatalf("应产生 CONTAINER_EXITED critical 告警: %+v", alarms.List("active"))
			} else {
				t.Logf("容器非零退出 critical 告警: %+v", *c)
			}
		} else {
			t.Logf("跳过容器告警环节：本地无 alpine:3.20 镜像")
		}
	} else {
		t.Logf("跳过容器告警环节：无 Docker socket")
	}
}

func activeCritical(store *network.AlarmStore, code, source string) *network.Alarm {
	for _, a := range store.List("active") {
		if a.Code == code && a.Source == source && a.Severity == network.SeverityCritical {
			aa := a
			return &aa
		}
	}
	return nil
}
