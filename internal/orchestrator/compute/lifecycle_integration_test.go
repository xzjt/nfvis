//go:build integration

package compute

// M4-3 真机集成测试（build tag integration，CI 不跑）：VNF 生命周期
// 定义 → 启动 → 状态 → 重启 → 停止（超时强杀）→ 删除级联。
//
// 在 nfvis-vm 上运行；需 libvirtd 与 1G 空闲大页（本环境 1 页，故用例串行且 VM=1G）。
// 用临时目录存放盘与 seed，避免污染生产路径。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

func TestVMLifecycleRealLibvirt(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skipf("跳过：未找到 qemu-img: %v", err)
	}
	if _, err := exec.LookPath("cloud-localds"); err != nil {
		t.Skipf("跳过：未找到 cloud-localds: %v", err)
	}

	// 使用生产路径（QEMU 以 libvirt-qemu 运行，需目录可穿越；t.TempDir 的 0700 会被拒）。
	imagesDir := "/var/lib/nfvis/images"
	vmsDir := "/var/lib/nfvis/vms"
	vhostDir := "/run/nfvis/vhost"
	for _, d := range []string{imagesDir, vmsDir, vhostDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const imgName = "it-m4-3-img.qcow2"
	img := filepath.Join(imagesDir, imgName)
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", img, "64M").CombinedOutput(); err != nil {
		t.Fatalf("建测试镜像失败: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = os.Remove(img) })

	uri := os.Getenv("NFVIS_LIBVIRT_URI")
	if uri == "" {
		uri = DefaultURI
	}
	cfg := DefaultConfig()
	cfg.URI = uri
	cfg.VMsDir = vmsDir
	cfg.ImagesDir = imagesDir
	cfg.VhostDir = vhostDir
	cfg.StopTimeout = 5 * time.Second // 无 OS 的空白盘不响应 ACPI，走超时强杀路径

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	p, conn, err := NewConnectedProvider(ctx, cfg)
	if err != nil {
		t.Skipf("跳过 libvirt 集成测试（%s 连接失败）: %v", uri, err)
	}
	// Cleanup 为 LIFO：先注册的 Close 最后执行，保证清理用的连接仍有效。
	t.Cleanup(func() { _ = conn.Close() })
	if v, err := conn.Version(); err == nil {
		t.Logf("libvirt %s @ %s", v, uri)
	}

	const name = "it-m4-3-vm"
	_ = p.DeleteVM(context.Background(), name) // 清理遗留
	t.Cleanup(func() { _ = p.DeleteVM(context.Background(), name) })

	vm := model.VMFunction{
		Name:   name,
		Image:  imgName,
		VCPU:   model.VMCpu{Count: 1},
		Memory: model.VMMemory{SizeMB: 1024, HugepageSize: "1G"},
		// 无 vNIC：vhost-user 通流属 M4-4（需 VPP 侧 socket 先建）
		CloudInit: &model.CloudInit{Hostname: "it-m4-3"},
		Autostart: true,
	}
	// 绑核由 M4-2 账本确定性重算（隔离核 [1,2,3]；VPP 预留 4,5 不在池内）。
	cfgModel := model.Config{
		ResourcePools: &model.ResourcePool{
			Hugepages: []model.HPool{{PageSize: "1G", Count: 1}},
			CPU:       &model.CPUSetup{IsolatedCores: []int{1, 2, 3}},
		},
		Vpp:                     &model.VppConfig{CPU: &model.VppCPU{MainCore: 4, CorelistWorkers: "5"}},
		VirtualMachineFunctions: []model.VMFunction{vm},
	}
	alloc := model.AllocationFor(cfgModel, vm)
	if err := p.DefineVM(ctx, vm, alloc); err != nil {
		t.Fatalf("DefineVM: %v", err)
	}

	// autostart → 运行中
	if st, err := p.VMState(ctx, name); err != nil || st != orchestrator.VMStateRunning {
		t.Fatalf("autostart 后应 running，实际 %q（err=%v）", st, err)
	}

	// 落盘与 domain XML 与配置一致（并交叉核对账本绑核 = 真实 domain cpuset，
	// 闭合 M4-2 「show resource-pools 与实际 domain 一致」的验收项）。
	xml, err := conn.DumpXML(ctx, name)
	if err != nil {
		t.Fatalf("DumpXML: %v", err)
	}
	if len(alloc.Cores) != 1 {
		t.Fatalf("账本应为该 VM 分配 1 核，实际 %v", alloc.Cores)
	}
	for _, want := range []string{
		"<name>" + name + "</name>",
		fmt.Sprintf("<vcpupin vcpu='0' cpuset='%d'/>", alloc.Cores[0]),
		"<page size='1048576' unit='KiB'/>",
		filepath.Join(vmsDir, name, "seed.iso"),
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("dumpxml 缺少 %q\n%s", want, xml)
		}
	}
	t.Logf("账本绑核 %v 与真实 domain vcpupin 一致", alloc.Cores)
	if _, err := os.Stat(filepath.Join(vmsDir, name, "seed.iso")); err != nil {
		t.Errorf("应生成 cloud-init seed ISO: %v", err)
	}
	if _, err := os.Stat(filepath.Join(vmsDir, name, "disk.qcow2")); err != nil {
		t.Errorf("应克隆出主盘: %v", err)
	}

	// ACPI 重启（运行中）
	if err := p.RestartVM(ctx, name); err != nil {
		t.Fatalf("RestartVM: %v", err)
	}
	if st, _ := p.VMState(ctx, name); st != orchestrator.VMStateRunning {
		t.Fatalf("重启后应 running，实际 %q", st)
	}

	// 停止：空白盘不响应 ACPI → 超时强杀
	start := time.Now()
	if err := p.StopVM(ctx, name); err != nil {
		t.Fatalf("StopVM: %v", err)
	}
	if st, _ := p.VMState(ctx, name); st != orchestrator.VMStateShutoff {
		t.Fatalf("停止后应 shutoff，实际 %q", st)
	}
	t.Logf("停止耗时 %s（StopTimeout=%s，空白盘不响应 ACPI 故走强杀）", time.Since(start).Round(time.Millisecond), cfg.StopTimeout)

	// 关机态 restart → 启动
	if err := p.RestartVM(ctx, name); err != nil {
		t.Fatalf("关机态 RestartVM: %v", err)
	}
	if st, _ := p.VMState(ctx, name); st != orchestrator.VMStateRunning {
		t.Fatalf("关机态 restart 后应 running，实际 %q", st)
	}
	if err := p.StartVM(ctx, name); err != nil {
		t.Fatalf("已运行 StartVM 应幂等: %v", err)
	}

	// 删除级联：domain 消失 + 落盘清理
	if err := p.DeleteVM(ctx, name); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
	if st, _ := p.VMState(ctx, name); st != orchestrator.VMStateAbsent {
		t.Fatalf("删除后应 absent，实际 %q", st)
	}
	if _, err := os.Stat(filepath.Join(vmsDir, name)); !os.IsNotExist(err) {
		t.Errorf("删除后 VM 目录应清理: %v", err)
	}
	// 再删一次幂等
	if err := p.DeleteVM(ctx, name); err != nil {
		t.Fatalf("重复删除应幂等: %v", err)
	}
}
