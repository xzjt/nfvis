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
	"sync"
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
	image := testVMImage(t)
	sock := vppSocket(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
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
			// guest 内的网卡名由 **guest 的命名策略**决定，不是产品模型里的逻辑名
			// （模型写 eth0，Debian 用可预测名 enp1s0/ens3）——写死 eth0 会让 runcmd
			// 报 `Cannot find device "eth0"`，cloud-final 随之失败、guest 没有地址。
			// 本用例的 guest 只有一块非 lo 网卡，按实际存在的那个配。
			UserData: "#cloud-config\nruncmd:\n  - |\n" + indentLines(guestNetSetup(itGuestIP), "    "),
		},
		Autostart: true,
	}
	cfg := model.Config{
		ResourcePools: &model.ResourcePool{
			Hugepages: []model.HPool{{PageSize: "1G", Count: 1}},
			CPU:       &model.CPUSetup{IsolatedCores: vmIsolatedCores(t)},
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

	// guest 侧诊断：串口**持续**回读（与下面的 ping 等待并行）。cloud-init 的 final 阶段
	// 要等 network-online（本 BD 无 DHCP，约 120s）才跑，故不能在 ping 之前阻塞式读一小段；
	// 失败时把整段串口输出带进错误信息，一眼能分清「guest 没配成地址」与「VPP 没转发」。
	var serialMu sync.Mutex
	var serial strings.Builder
	if stream, err := comp.Console(ctx, itChainVM); err == nil {
		defer func() { _ = stream.Close() }()
		go func() {
			buf := make([]byte, 4096)
			for {
				n, rerr := stream.Read(buf)
				if n > 0 {
					serialMu.Lock()
					serial.Write(buf[:n])
					serialMu.Unlock()
				}
				if rerr != nil {
					return
				}
			}
		}()
	} else {
		t.Logf("串口打开失败（跳过 guest 诊断）: %v", err)
	}
	dumpSerial := func() string {
		serialMu.Lock()
		out := serial.String()
		serialMu.Unlock()
		if dump := os.Getenv("NFVIS_SERIAL_DUMP"); dump != "" {
			_ = os.WriteFile(dump, []byte(out), 0o644)
		}
		return out
	}

	pingOK := false
	// BVI 网关地址位于 L2 交换机专属 VRF 的表中（决策 #32），ping 需指定 table-id。
	//
	// 等待窗口按 guest 侧真机事实定：guest 的地址由 cloud-init 的 final 阶段（runcmd）
	// 配置，而该阶段 After=network-online.target；本用例的 BD 上没有 DHCP，等待超时约
	// 120s，故 guest 到 uptime ~135s 才有地址（串口实测 modules:final at Up 134.15s）。
	// 窗口取 4 分钟给引导波动留余量；按 alpine 的启动速度定小窗口会误判为通流失败。
	tableID := network.TableID("vr-" + itChainVS)
	deadline := time.Now().Add(4 * time.Minute)
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
		t.Fatalf("VPP 未能 ping 通 guest %s（%s，table %d 经 vhost-user）：\n%s\n--- interfaces ---\n%s\n--- guest 串口（末尾）---\n%s",
			itGuestIP, itChainVS, tableID, lastOut, ifs, tailStr(dumpSerial(), 3000))
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

// guestNetSetup 返回给 guest 配地址的 shell 片段：按**实际存在**的非 lo 网卡配置。
// 不能写死接口名——guest 的命名策略（systemd 可预测网名）与产品模型里的逻辑名
// （eth0）是两回事，见调用处注释。
func guestNetSetup(addr string) string {
	return "for d in /sys/class/net/*; do\n" +
		"  d=${d##*/}\n" +
		"  [ \"$d\" = lo ] && continue\n" +
		"  ip addr add " + addr + "/24 dev \"$d\" 2>/dev/null && ip link set \"$d\" up && break\n" +
		"done\n"
}

// indentLines 给每行加前缀，用于把命令写进 YAML 块标量（runcmd 的 `- |`）。
func indentLines(s, prefix string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString(prefix + line + "\n")
	}
	return b.String()
}

func mustCommitted(t *testing.T, e *config.Engine) model.Config {
	t.Helper()
	c, err := e.Committed()
	if err != nil {
		t.Fatal(err)
	}
	return c
}
