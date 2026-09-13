//go:build integration

// M4-11 主链路：建交换机 → 建 VM（vhost-user）→ 通流 → 改配置（vCPU）→ 删除归还资源（FR-OPS-012/FR-CMP）。
// 经真实事务引擎 + applier 下发（网络 + 计算），端到端验证 committed 配置落到 VPP/libvirt。
package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/compute"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

const (
	itChainVS   = "it-m4-11-vs"
	itChainVM   = "it-m4-11-vm"
	itGuestIP   = "192.168.77.2"
	itGatewayIP = "192.168.77.1"
)

func TestMainChainVMRealVPPAndLibvirt(t *testing.T) {
	image := os.Getenv("NFVIS_TEST_VM_IMAGE")
	if image == "" {
		image = "alpine.qcow2"
	}
	if _, err := os.Stat(filepath.Join("/var/lib/nfvis/images", image)); err != nil {
		t.Skipf("跳过：无可引导镜像 /var/lib/nfvis/images/%s", image)
	}
	sock := vppSocket(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	// ---- VPP 网络编排 + libvirt 计算编排 ----
	mgr := network.NewManager(network.Config{Socket: sock}, nil)
	if _, err := mgr.ConnectOnce(ctx); err != nil {
		t.Fatalf("连接 VPP: %v", err)
	}
	t.Cleanup(mgr.Close)

	np := network.NewL2Network(orchestrator.NewNoopNetwork(), network.NewL2ProviderFunc(mgr.L2ClientFunc()))
	np.SetL3(network.NewL3ProviderFunc(mgr.L3ClientFunc()))
	np.SetServices(network.NewServicesProviderFunc(mgr.SvcClientFunc()))
	np.SetVhostUser(network.NewVhostUserProviderFunc(mgr.VhostUserClientFunc()))
	np.SetMemif(network.NewMemifProviderFunc(mgr.MemifClientFunc()))
	alarms := network.NewAlarmStore()
	np.SetAlarms(alarms)

	computeCfg := compute.DefaultConfig()
	computeCfg.URI = os.Getenv("NFVIS_LIBVIRT_URI")
	computeCfg.VhostDir = orchestrator.DefaultVhostDir
	computeCfg.StopTimeout = 10 * time.Second
	comp, conn, err := compute.NewConnectedProvider(ctx, computeCfg)
	if err != nil {
		t.Skipf("跳过（libvirt 不可用）: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	comp.SetAlarms(alarms)
	np.SetSocketDirs(computeCfg.VhostDir, orchestrator.DefaultMemifDir)
	if err := os.MkdirAll(computeCfg.VhostDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = comp.DeleteVM(context.Background(), itChainVM)
	t.Cleanup(func() { _ = comp.DeleteVM(context.Background(), itChainVM) })
	t.Cleanup(func() {
		// 清理 VPP 侧 vNIC/BD
		_ = np.DeleteVnfInterface(context.Background(), itChainVM, "eth0")
		_ = np.DeleteBridgeDomain(context.Background(), itChainVS)
	})

	applier := orchestrator.NewApplier(np, comp, orchestrator.NewNoopContainer(),
		orchestrator.WithVhostDir(computeCfg.VhostDir), orchestrator.WithMemifDir(orchestrator.DefaultMemifDir))
	store, err := config.OpenStore(filepath.Join(t.TempDir(), "it-m4-11.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	engine, err := config.NewEngine(store, applier, config.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	sess := config.Session{User: "itest", Source: "integration"}
	commit := func(cfg model.Config) {
		t.Helper()
		if err := engine.Edit(sess); err != nil {
			t.Fatalf("edit: %v", err)
		}
		if err := engine.UpdateCandidate(sess, cfg); err != nil {
			t.Fatalf("update candidate: %v", err)
		}
		if _, err := engine.Commit(ctx, sess, config.CommitOpts{Message: "it-m4-11"}); err != nil {
			verrs, _ := engine.CommitCheck(sess)
			t.Fatalf("commit: %v；校验明细: %v", err, verrs)
		}
		if err := engine.Release(sess); err != nil {
			t.Fatalf("release: %v", err)
		}
	}

	vm := model.VMFunction{
		Name: itChainVM, Image: image,
		VCPU:   model.VMCpu{Count: 1},
		Memory: model.VMMemory{SizeMB: 1024, HugepageSize: "1G"},
		Interfaces: []model.VnfInterface{{
			Name: "eth0", Type: "vhost-user", VirtualSwitch: itChainVS,
		}},
		CloudInit: &model.CloudInit{
			Hostname: "it-m4-11",
			UserData: "#cloud-config\nruncmd:\n  - ip addr add " + itGuestIP + "/24 dev eth0\n  - ip link set eth0 up\n",
		},
		Autostart: true,
	}
	cfg := model.Config{
		ResourcePools: &model.ResourcePool{
			Hugepages: []model.HPool{{PageSize: "1G", Count: 1}},
			CPU:       &model.CPUSetup{IsolatedCores: []int{1, 2, 3}},
		},
		VirtualSwitches: []model.VirtualSwitch{{
			Name: itChainVS, Type: "l2",
			// VNF vNIC 经端口条目接入 BD（附录 A #31：端口 → sw_interface_set_l2_bridge）；
			// 缺此条目则 vhost 接口不会成为 BD 成员，BVI 无法转发到 guest。
			Ports:   []model.VSwitchPort{{Seq: 0, Vnf: itChainVM, VnfInterface: "eth0"}},
			Gateway: &model.VSGateway{Addresses: []string{itGatewayIP + "/24"}},
		}},
		VirtualMachineFunctions: []model.VMFunction{vm},
	}
	commit(cfg)
	t.Cleanup(func() {
		c := cfg
		c.VirtualMachineFunctions = nil
		c.VirtualSwitches = nil
		_ = func() error {
			if err := engine.Edit(sess); err != nil {
				return err
			}
			if err := engine.UpdateCandidate(sess, c); err != nil {
				return err
			}
			_, err := engine.Commit(context.Background(), sess, config.CommitOpts{Message: "cleanup"})
			return err
		}()
	})

	// ---- 通流：guest 经 vhost-user 接入 BD，VPP 侧 BVI 网关 ping guest ----
	waitFor(t, "vhost-user 接口链路 up（guest 引导完成）", 150*time.Second, func() bool {
		exists, up, err := np.VnfPortLinkState(ctx, itChainVM, "eth0")
		return err == nil && exists && up
	})
	t.Log("vhost-user 链路 up")

	// guest 侧诊断：cloud-init 是否配置了 eth0（经串口回读，M4-5 能力）。
	if stream, err := comp.Console(ctx, itChainVM); err == nil {
		defer stream.Close()
		bufCh := make(chan string, 1)
		go func() {
			var b strings.Builder
			buf := make([]byte, 4096)
			dl := time.Now().Add(60 * time.Second)
			for time.Now().Before(dl) {
				n, rerr := stream.Read(buf)
				if n > 0 {
					b.Write(buf[:n])
					if strings.Contains(b.String(), "NFVIS-GUEST-DONE") {
						break
					}
				}
				if rerr != nil {
					break
				}
			}
			bufCh <- b.String()
		}()
		select {
		case out := <-bufCh:
			t.Logf("guest 串口配置输出: %s", tailStr(out, 800))
		case <-time.After(65 * time.Second):
			t.Log("guest 串口读取超时")
		}
	} else {
		t.Logf("串口打开失败（跳过 guest 诊断）: %v", err)
	}

	pingOK := false
	// BVI 网关地址位于 L2 交换机专属 VRF 的表中（决策 #32），ping 需指定 table-id。
	tableID := network.TableID("vr-" + itChainVS)
	deadline := time.Now().Add(60 * time.Second)
	var lastOut string
	for time.Now().Before(deadline) {
		out, _ := exec.Command("vppctl", "ping", itGuestIP,
			"table-id", strconv.FormatUint(uint64(tableID), 10),
			"repeat", "3", "interval", "0.5").CombinedOutput()
		lastOut = string(out)
		if strings.Contains(lastOut, "received") && !strings.Contains(lastOut, "0 received") {
			pingOK = true
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !pingOK {
		ifs, _ := exec.Command("vppctl", "show", "interface").CombinedOutput()
		t.Fatalf("VPP 未能 ping 通 guest %s（%s，table %d 经 vhost-user）：\n%s\n--- interfaces ---\n%s",
			itGuestIP, itChainVS, tableID, lastOut, ifs)
	}
	t.Logf("通流成功（VPP → guest %s over vhost-user）：\n%s", itGuestIP, firstLines(lastOut, "received", 2))

	// ---- 改配置：关机后改 vCPU 1→2，核绑定与账本随之更新（FR-CMP-012）----
	if err := comp.StopVM(ctx, itChainVM); err != nil {
		t.Fatalf("StopVM: %v", err)
	}
	vm2 := vm
	vm2.VCPU = model.VMCpu{Count: 2}
	cfg.VirtualMachineFunctions = []model.VMFunction{vm2}
	commit(cfg)
	xml, err := conn.DumpXML(ctx, itChainVM)
	if err != nil {
		t.Fatalf("DumpXML: %v", err)
	}
	if !strings.Contains(xml, "<vcpupin vcpu='0'") || !strings.Contains(xml, "<vcpupin vcpu='1'") {
		t.Fatalf("改配置后应有 2 个 vcpupin:\n%s", xml)
	}
	ledger := model.NewPoolLedger(mustCommitted(t, engine))
	if errs := ledger.Allocate(mustCommitted(t, engine)); len(errs) != 0 {
		t.Fatalf("账本分配: %v", errs)
	}
	if got := ledger.CPU.VMCores[itChainVM]; len(got) != 2 {
		t.Fatalf("账本应为该 VM 分配 2 核，实际 %v", got)
	}
	t.Logf("改配置 vCPU=2 后：domain vcpupin 2 个、账本绑核 %v", ledger.CPU.VMCores[itChainVM])

	// ---- 删除：级联清理 domain/VPP 端口，资源归还 ----
	// candidate 必须自洽：端口引用已删 VM 会被校验拒绝（FR-CFG-002），故一并移除交换机。
	cfg.VirtualMachineFunctions = nil
	cfg.VirtualSwitches = nil
	commit(cfg)
	if st, _ := comp.VMState(ctx, itChainVM); st != "absent" {
		t.Fatalf("删除后应 absent: %q", st)
	}
	if exists, _, _ := np.VnfPortLinkState(ctx, itChainVM, "eth0"); exists {
		t.Fatal("删除后 VPP vhost-user 接口应清理")
	}
	after := model.NewPoolLedger(mustCommitted(t, engine))
	_ = after.Allocate(mustCommitted(t, engine))
	if got := after.Hugepages["1G"].Allocated; got != 0 {
		t.Fatalf("删除后大页应归还（allocated=0），实际 %d", got)
	}
	if len(after.CPU.VMCores) != 0 {
		t.Fatalf("删除后绑核应归还，实际 %v", after.CPU.VMCores)
	}
	t.Log("删除后 domain/VPP 端口清理，资源池已归还（大页 allocated=0、无绑核）")
}

func mustCommitted(t *testing.T, e *config.Engine) model.Config {
	t.Helper()
	c, err := e.Committed()
	if err != nil {
		t.Fatal(err)
	}
	return c
}
