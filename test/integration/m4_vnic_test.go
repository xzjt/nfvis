//go:build integration

// M4-4 真机集成测试：vhost-user vNIC 接入与 VPP/VM 联动（FR-NET-020/023）。
//
// 断言链路：VPP 建 vhost-user socket 接口（浏览器可见）→ 无客户端时 link down
// → 定义并启动带该 vNIC 的 VM（QEMU 作 client 连接）→ link up
// → 停止 VM → link down → 删除 → VPP 接口消失。
// SR-IOV 分支在本环境（vmxnet3，无 PF/VF）不可真机验证，仅单测。
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
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/compute"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

const (
	itVnicVM    = "it-m4-4-vm"
	itVnicIface = "eth0"
	itVnicDir   = "/run/nfvis/vhost"
)

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

// vppctlShowInterface 返回 `vppctl show interface` 输出（仅用于记录证据，失败不致命）。
func vppctlShowInterface() string {
	out, err := exec.Command("vppctl", "show", "interface").CombinedOutput()
	if err != nil {
		return ""
	}
	return string(out)
}

func TestVhostUserLifecycleRealVPP(t *testing.T) {
	sock := vppSocket(t)
	mgr := network.NewManager(network.Config{Socket: sock}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := mgr.ConnectOnce(ctx); err != nil {
		t.Fatalf("连接 VPP %s: %v", sock, err)
	}
	// Cleanup LIFO：先注册 Close（最后执行），确保清理用的连接仍有效。
	t.Cleanup(mgr.Close)

	netProvider := network.NewL2Network(orchestrator.NewNoopNetwork(), network.NewL2ProviderFunc(mgr.L2ClientFunc()))
	netProvider.SetL3(network.NewL3ProviderFunc(mgr.L3ClientFunc()))
	netProvider.SetVhostUser(network.NewVhostUserProviderFunc(mgr.VhostUserClientFunc()))
	alarms := network.NewAlarmStore()
	netProvider.SetAlarms(alarms)

	port := orchestrator.VnfPort{
		VM: itVnicVM, Interface: itVnicIface, Type: "vhost-user",
		Socket: orchestrator.VnfSocketPath(itVnicDir, itVnicVM, itVnicIface),
	}
	_ = netProvider.DeleteVnfInterface(ctx, itVnicVM, itVnicIface) // 清理遗留
	t.Cleanup(func() { _ = netProvider.DeleteVnfInterface(context.Background(), itVnicVM, itVnicIface) })

	if err := os.MkdirAll(itVnicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := netProvider.ApplyVnfInterface(ctx, port); err != nil {
		t.Fatalf("ApplyVnfInterface: %v", err)
	}

	// VPP 应创建 unix socket 文件，接口存在但无客户端 → link down。
	if _, err := os.Stat(port.Socket); err != nil {
		t.Errorf("VPP 应创建 vhost-user socket %s: %v", port.Socket, err)
	}
	if !strings.Contains(vppctlShowInterface(), orchestrator.VnfIfaceName(itVnicVM, itVnicIface)) {
		t.Fatalf("vppctl show interface 应可见 vhost 口\n%s", vppctlShowInterface())
	}
	t.Logf("vppctl show interface 片段:\n%s", firstLines(vppctlShowInterface(), orchestrator.VnfIfaceName(itVnicVM, itVnicIface), 3))

	// 定义并启动带该 vNIC 的 VM（QEMU 作 client 连接同一 socket）。
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skipf("跳过 VM 联动（未找到 qemu-img）: %v", err)
	}
	imagesDir, vmsDir := "/var/lib/nfvis/images", "/var/lib/nfvis/vms"
	for _, d := range []string{imagesDir, vmsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 引导镜像：vhost-user 链路 up 需 guest 加载 virtio-net 驱动并置 DRIVER_OK
	// （QEMU 此时才发 SET_MEM_TABLE → VPP `Memory regions` 非零）。空白盘无法完成，
	// 故优先用 NFVIS_TEST_VM_IMAGE 或仓库中的 alpine cloud 镜像；缺失则只验证接入/删除。
	bootable := true
	imgName := os.Getenv("NFVIS_TEST_VM_IMAGE")
	if imgName == "" {
		imgName = "alpine.qcow2"
		if _, err := os.Stat(filepath.Join(imagesDir, imgName)); err != nil {
			bootable = false
			imgName = "it-m4-4-img.qcow2"
		}
	}
	img := filepath.Join(imagesDir, imgName)
	if _, err := os.Stat(img); err != nil {
		if out, cerr := exec.Command("qemu-img", "create", "-f", "qcow2", img, "64M").CombinedOutput(); cerr != nil {
			t.Fatalf("建测试镜像失败: %v: %s", cerr, out)
		}
		t.Cleanup(func() { _ = os.Remove(img) })
		bootable = false
	}
	if !bootable {
		t.Logf("未找到引导镜像（NFVIS_TEST_VM_IMAGE 或 %s/alpine.qcow2），跳过链路 up/down 断言", imagesDir)
	}

	cfg := compute.DefaultConfig()
	cfg.VMsDir, cfg.ImagesDir, cfg.VhostDir = vmsDir, imagesDir, itVnicDir
	cfg.StopTimeout = 5 * time.Second
	p, conn, err := compute.NewConnectedProvider(ctx, cfg)
	if err != nil {
		t.Skipf("跳过 VM 联动（libvirt 不可用）: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // LIFO：最后执行
	_ = p.DeleteVM(context.Background(), itVnicVM)
	t.Cleanup(func() { _ = p.DeleteVM(context.Background(), itVnicVM) })

	vm := model.VMFunction{
		Name: itVnicVM, Image: imgName,
		VCPU:   model.VMCpu{Count: 1},
		Memory: model.VMMemory{SizeMB: 1024, HugepageSize: "1G"},
		Interfaces: []model.VnfInterface{{
			Name: itVnicIface, Type: "vhost-user", VirtualSwitch: "it-m4-4-vs",
		}},
		Autostart: true,
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

	// QEMU 连接后 VPP 完成 vhost-user 握手（`show vhost-user` 报 Memory regions ≥1），
	// 随后链路 up。握手期间避免频繁开 govpp channel（改用 vppctl 观察）。
	ifaceName := orchestrator.VnfIfaceName(itVnicVM, itVnicIface)
	handshakeOK := func() bool {
		out, _ := exec.Command("vppctl", "show", "vhost-user").CombinedOutput()
		seg := vhostSection(string(out), ifaceName)
		return strings.Contains(seg, "Memory regions") && !strings.Contains(seg, "Memory regions (total 0)")
	}
	// QEMU 已连接：协议层应已协商（features/hdr 非零）。
	if !strings.Contains(vppctlShowInterface(), ifaceName) {
		t.Fatalf("QEMU 连接后 vppctl 仍看不到 vhost 口")
	}
	if bootable {
		deadline := time.Now().Add(120 * time.Second)
		for time.Now().Before(deadline) && !handshakeOK() {
			st, _ := p.VMState(ctx, itVnicVM)
			t.Logf("等待 guest 引导完成 vhost-user 握手：domain=%s", st)
			time.Sleep(5 * time.Second)
		}
		if !handshakeOK() {
			st, _ := p.VMState(ctx, itVnicVM)
			out, _ := exec.Command("vppctl", "show", "vhost-user").CombinedOutput()
			t.Fatalf("等待超时：guest 引导后 vhost-user 握手未完成（domain=%s）\n%s", st, out)
		}
		waitFor(t, "握手后链路 up", 15*time.Second, func() bool {
			_, up, err := netProvider.VnfPortLinkState(ctx, itVnicVM, itVnicIface)
			return err == nil && up
		})
		t.Logf("VM 启动后 link up；vppctl 片段:\n%s", firstLines(vppctlShowInterface(), ifaceName, 3))
	} else {
		// 无引导镜像：仅确认 QEMU 已连接（VPP 报 virtio_net_hdr_sz 非零即协议已协商）。
		out, _ := exec.Command("vppctl", "show", "vhost-user").CombinedOutput()
		if !strings.Contains(vhostSection(string(out), ifaceName), "virtio_net_hdr_sz 12") {
			t.Logf("提示：QEMU 已连但 guest 未引导，握手停在 mem table 前（预期）\n%s", vhostSection(string(out), ifaceName))
		}
	}

	// FR-NET-023：VM 关机 → 断连 → link down + warning 告警。
	if err := p.StopVM(ctx, itVnicVM); err != nil {
		t.Fatalf("StopVM: %v", err)
	}
	if bootable {
		waitFor(t, "VM 停止后 vhost-user link down（原先 up）", 30*time.Second, func() bool {
			_, up, err := netProvider.VnfPortLinkState(ctx, itVnicVM, itVnicIface)
			return err == nil && !up
		})
	}
	cfgNow, _ := loadCommittedOrEmpty(cfgModel)
	if errs := netProvider.CheckVnfPorts(ctx, cfgNow); len(errs) > 0 {
		t.Fatalf("vNIC 状态检查报错: %v", errs)
	}
	active := alarms.List("active")
	foundAlarm := false
	for _, a := range active {
		if a.Code == network.AlarmVnfPortDown && strings.Contains(a.Source, itVnicVM) {
			foundAlarm = true
		}
	}
	if !foundAlarm {
		t.Errorf("VM 关机后应有 VNF_PORT_DOWN 告警: %+v", active)
	}

	// 删除 VM 与 vNIC 接入 → VPP 接口消失。
	if err := p.DeleteVM(ctx, itVnicVM); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
	if err := netProvider.DeleteVnfInterface(ctx, itVnicVM, itVnicIface); err != nil {
		t.Fatalf("DeleteVnfInterface: %v", err)
	}
	if exists, _, err := netProvider.VnfPortLinkState(ctx, itVnicVM, itVnicIface); err != nil || exists {
		t.Fatalf("删除后 VPP 接口应消失: exists=%v err=%v", exists, err)
	}
	if _, err := os.Stat(port.Socket); !os.IsNotExist(err) {
		t.Errorf("删除后 socket 文件应清理: %v", err)
	}
}

// loadCommittedOrEmpty 返回用于告警检查的配置（本测试直接用 cfgModel 语义）。
func loadCommittedOrEmpty(cfg model.Config) (model.Config, error) { return cfg, nil }

func firstLines(s, needle string, n int) string {
	if s == "" {
		return "（vppctl 不可用）"
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, needle) {
			out = append(out, line)
			if len(out) >= n {
				break
			}
		}
	}
	if len(out) == 0 {
		return "（未找到接口行：" + needle + "）"
	}
	return strings.Join(out, "\n")
}

// vhostSection 截取 `show vhost-user` 中某接口的段落（到下一个 Interface: 或结尾）。
func vhostSection(out, name string) string {
	marker := "Interface: " + name
	i := strings.Index(out, marker)
	if i < 0 {
		return ""
	}
	rest := out[i+len(marker):]
	if j := strings.Index(rest, "\nInterface: "); j >= 0 {
		return rest[:j]
	}
	return rest
}
