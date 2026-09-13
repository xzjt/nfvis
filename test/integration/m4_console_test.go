//go:build integration

// M4-5 真机集成测试：cloud-init 注入在 guest 内生效 + 串口 console 可进可退
// （FR-CMP-014/016）。
//
// 手法：user-data 的 cloud-init `runcmd` 向 /dev/ttyS0 写标记 → 经 libvirt 串口
// console 读回该标记，一次性验证「seed ISO 注入 + guest 内 cloud-init 执行 + 串口双向」。
// 需可引导云镜像（NFVIS_TEST_VM_IMAGE 或 /var/lib/nfvis/images/alpine.qcow2），否则跳过。
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
)

const (
	itConsoleVM = "it-m4-5-vm"
	cloudMarker = "NFVIS-CLOUD-INIT-OK"
)

func TestCloudInitAndSerialConsoleRealLibvirt(t *testing.T) {
	if _, err := exec.LookPath("cloud-localds"); err != nil {
		t.Skipf("跳过：未找到 cloud-localds: %v", err)
	}
	imagesDir, vmsDir, vhostDir := "/var/lib/nfvis/images", "/var/lib/nfvis/vms", "/run/nfvis/vhost"
	image := os.Getenv("NFVIS_TEST_VM_IMAGE")
	if image == "" {
		image = "alpine.qcow2"
	}
	if _, err := os.Stat(filepath.Join(imagesDir, image)); err != nil {
		t.Skipf("跳过：无可引导镜像 %s（设置 NFVIS_TEST_VM_IMAGE）", filepath.Join(imagesDir, image))
	}
	for _, d := range []string{imagesDir, vmsDir, vhostDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cfg := compute.DefaultConfig()
	cfg.URI = os.Getenv("NFVIS_LIBVIRT_URI")
	cfg.VMsDir, cfg.ImagesDir, cfg.VhostDir = vmsDir, imagesDir, vhostDir
	cfg.StopTimeout = 10 * time.Second

	p, conn, err := compute.NewConnectedProvider(ctx, cfg)
	if err != nil {
		t.Skipf("跳过（libvirt 不可用）: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = p.DeleteVM(context.Background(), itConsoleVM)
	t.Cleanup(func() { _ = p.DeleteVM(context.Background(), itConsoleVM) })

	vm := model.VMFunction{
		Name: itConsoleVM, Image: image,
		VCPU:   model.VMCpu{Count: 1},
		Memory: model.VMMemory{SizeMB: 1024, HugepageSize: "1G"},
		CloudInit: &model.CloudInit{
			Hostname: "it-m4-5",
			UserData: "#cloud-config\nruncmd:\n  - echo " + cloudMarker + " > /dev/ttyS0\n",
			SSHKeys:  []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITESTKEY nfvis@test"},
		},
		Autostart: true, // 串口缺省启用
	}
	cfgModel := model.Config{
		ResourcePools: &model.ResourcePool{
			Hugepages: []model.HPool{{PageSize: "1G", Count: 1}},
			CPU:       &model.CPUSetup{IsolatedCores: []int{1, 2, 3}},
		},
		VirtualMachineFunctions: []model.VMFunction{vm},
	}
	if err := p.DefineVM(ctx, vm, model.AllocationFor(cfgModel, vm)); err != nil {
		t.Fatalf("DefineVM: %v", err)
	}

	// seed ISO 与 user-data/meta-data 落盘且内容正确（注入侧）。
	vmDir := filepath.Join(vmsDir, itConsoleVM)
	if _, err := os.Stat(filepath.Join(vmDir, "seed.iso")); err != nil {
		t.Errorf("应生成 seed ISO: %v", err)
	}
	ud, err := os.ReadFile(filepath.Join(vmDir, "user-data"))
	if err != nil || !strings.Contains(string(ud), cloudMarker) || !strings.Contains(string(ud), "ssh_authorized_keys") {
		t.Errorf("user-data 应含 runcmd 标记与 SSH 公钥: err=%v\n%s", err, ud)
	}
	md, err := os.ReadFile(filepath.Join(vmDir, "meta-data"))
	if err != nil || !strings.Contains(string(md), "local-hostname: it-m4-5") || !strings.Contains(string(md), "instance-id: "+itConsoleVM) {
		t.Errorf("meta-data 应含 hostname/instance-id: err=%v\n%s", err, md)
	}

	// 串口 console：打开 → 读回 cloud-init 写入的标记 → 关闭（可进可退）。
	stream, err := p.Console(ctx, itConsoleVM)
	if err != nil {
		t.Fatalf("打开串口 console: %v", err)
	}
	outCh := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, rerr := stream.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
				if strings.Contains(b.String(), cloudMarker) {
					outCh <- b.String()
					return
				}
			}
			if rerr != nil {
				outCh <- b.String()
				return
			}
		}
	}()

	select {
	case out := <-outCh:
		if !strings.Contains(out, cloudMarker) {
			t.Fatalf("串口未出现 cloud-init 标记（FR-CMP-016 失败）。串口输出:\n%s", tailStr(out, 2000))
		}
		t.Logf("串口读回 cloud-init 标记 %q（guest 内 runcmd 已执行）；输出片段:\n%s", cloudMarker, tailStr(out, 1200))
	case <-time.After(3 * time.Minute):
		t.Fatal("等待 cloud-init 标记超时（guest 引导/cloud-init 未完成）")
	}
	if err := stream.Close(); err != nil {
		t.Errorf("关闭 console: %v", err)
	}
	t.Log("串口 console 可进可退（打开/读取/关闭成功）")
}

func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
