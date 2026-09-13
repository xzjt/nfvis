//go:build integration

// M4-7 真机集成测试：VPP memif endpoint + 容器生命周期/日志（FR-CMP-020~022、FR-NET-022）。
//
// 容器侧 memif 通流需镜像自带 memif 客户端（离线环境无此镜像），故本测试验证：
// VPP 侧 memif socket/接口创建与清理、容器创建/启停/重启/日志/删除、memif socket 挂载。
package integration

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/container"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

const (
	itCtName  = "it-m4-7-ct"
	itCtIface = "eth0"
	itCtImage = "alpine:3.20"
)

func TestContainerLifecycleWithMemifRealDocker(t *testing.T) {
	if _, err := os.Stat("/var/run/docker.sock"); err != nil {
		t.Skipf("跳过：无 Docker socket: %v", err)
	}
	if out, err := exec.Command("docker", "image", "inspect", itCtImage).CombinedOutput(); err != nil {
		t.Skipf("跳过：本地无镜像 %s（docker pull 后重跑）: %s", itCtImage, out)
	}
	sock := vppSocket(t)

	mgr := network.NewManager(network.Config{Socket: sock}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := mgr.ConnectOnce(ctx); err != nil {
		t.Fatalf("连接 VPP: %v", err)
	}
	t.Cleanup(mgr.Close)

	memifDir := orchestrator.DefaultMemifDir
	if err := os.MkdirAll(memifDir, 0o755); err != nil {
		t.Fatal(err)
	}
	netProvider := network.NewL2Network(orchestrator.NewNoopNetwork(), nil)
	netProvider.SetMemif(network.NewMemifProviderFunc(mgr.MemifClientFunc()))

	port := orchestrator.VnfPort{
		VM: itCtName, Interface: itCtIface, Type: "memif",
		Socket: orchestrator.MemifSocketPath(memifDir, itCtName, itCtIface),
	}
	_ = netProvider.DeleteVnfInterface(ctx, itCtName, itCtIface)
	t.Cleanup(func() { _ = netProvider.DeleteVnfInterface(context.Background(), itCtName, itCtIface) })

	if err := netProvider.ApplyVnfInterface(ctx, port); err != nil {
		t.Fatalf("ApplyVnfInterface(memif): %v", err)
	}
	if _, err := os.Stat(port.Socket); err != nil {
		t.Errorf("VPP 应创建 memif socket %s: %v", port.Socket, err)
	}
	if out, _ := exec.Command("vppctl", "show", "memif").CombinedOutput(); !strings.Contains(string(out), orchestrator.MemifIfaceName(itCtName, itCtIface)) {
		t.Errorf("vppctl show memif 应可见 %s:\n%s", orchestrator.MemifIfaceName(itCtName, itCtIface), out)
	}
	t.Logf("VPP memif 端点已创建：%s（socket %s）", orchestrator.MemifIfaceName(itCtName, itCtIface), port.Socket)

	// 容器编排
	ctCfg := container.DefaultConfig()
	ctCfg.MemifDir = memifDir
	p := container.NewConnectedProvider(ctCfg)
	_ = p.DeleteContainer(context.Background(), itCtName)
	t.Cleanup(func() { _ = p.DeleteContainer(context.Background(), itCtName) })

	ct := model.ContainerFunction{
		Name: itCtName, Image: itCtImage,
		VCPU: 1, MemoryMB: 128,
		Command: "/bin/sh",
		Args:    []string{"-c", "echo CT-MARK-OK; sleep 120"},
		Interfaces: []model.VnfInterface{{
			Name: itCtIface, Type: "memif", VirtualSwitch: "it-m4-7-vs",
		}},
		Autostart: true,
	}
	if err := p.ApplyContainer(ctx, ct); err != nil {
		t.Fatalf("ApplyContainer: %v", err)
	}
	if st, err := p.ContainerState(ctx, itCtName); err != nil || st != orchestrator.CTStateRunning {
		t.Fatalf("容器应 running: %q err=%v", st, err)
	}

	// 日志（云-init 前的 stdout）应含标记
	waitFor(t, "容器日志出现标记", 20*time.Second, func() bool {
		out, err := p.ContainerLogs(ctx, itCtName, 50)
		return err == nil && strings.Contains(out, "CT-MARK-OK")
	})
	logs, _ := p.ContainerLogs(ctx, itCtName, 50)
	t.Logf("容器日志片段: %q", strings.TrimSpace(logs))

	// 生命周期
	if err := p.StopContainer(ctx, itCtName); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st, _ := p.ContainerState(ctx, itCtName); st != orchestrator.CTStateExited {
		t.Fatalf("停止后应 exited: %q", st)
	}
	if err := p.StartContainer(ctx, itCtName); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.RestartContainer(ctx, itCtName); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if err := p.StartContainer(ctx, itCtName); err != nil {
		t.Fatalf("已运行 Start 应幂等: %v", err)
	}
	if err := p.DeleteContainer(ctx, itCtName); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if st, _ := p.ContainerState(ctx, itCtName); st != orchestrator.CTStateAbsent {
		t.Fatalf("删除后应 absent: %q", st)
	}

	// memif 端点清理
	if err := netProvider.DeleteVnfInterface(ctx, itCtName, itCtIface); err != nil {
		t.Fatalf("DeleteVnfInterface: %v", err)
	}
	if out, _ := exec.Command("vppctl", "show", "memif").CombinedOutput(); strings.Contains(string(out), orchestrator.MemifIfaceName(itCtName, itCtIface)) {
		t.Errorf("删除后 vppctl 不应再显示 memif 接口:\n%s", out)
	}
	t.Log("容器生命周期与 VPP memif 端点创建/清理完成（容器侧 memif 通流需自带 memif 客户端的镜像，未验证）")
}
