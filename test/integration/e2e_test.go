//go:build integration

// M3-10：真机集成测试（建交换机 → 通流 → 改配置 → 收敛）。
//
// 约定（M3-P0）：build tag `integration` + 环境变量 `NFVIS_VPP_SOCK`（缺省
// /run/vpp/api.sock）；无 VPP socket 时跳过。运行于 nfvis-vm，CI 不跑。
//
// 测试直接装配 store+engine+Provider（不经 nfvisd），使用 it- 前缀对象，
// 可与守护进程共存但建议先停 nfvisd（`pkill -x nfvisd`）避免同口争用。
package integration

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

const (
	itVS    = "it-vs"
	itVRF   = "it-vrf"
	itAddr  = "192.168.155.200/24"
	itAddr2 = "192.168.155.202/24"
)

func vppSocket(t *testing.T) string {
	t.Helper()
	sock := os.Getenv("NFVIS_VPP_SOCK")
	if sock == "" {
		sock = network.DefaultSocket
	}
	if _, err := os.Stat(sock); err != nil {
		t.Skipf("跳过 integration：未找到 VPP socket %s（设置 NFVIS_VPP_SOCK 或在 nfvis-vm 运行）", sock)
	}
	return sock
}

// harness 集成测试装配（store/engine/Provider/连接）。
type harness struct {
	mgr    *network.Manager
	engine *config.Engine
	store  *config.Store
	net    *network.L2Network
	diag   *network.Diagnostics
	sess   config.Session
}

func newHarness(t *testing.T, sock string) *harness {
	t.Helper()
	mgr := network.NewManager(network.Config{Socket: sock}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := mgr.ConnectOnce(ctx); err != nil {
		t.Fatalf("连接 VPP 失败: %v", err)
	}
	t.Cleanup(mgr.Close)

	l2 := network.NewL2ProviderFunc(mgr.L2ClientFunc())
	l3 := network.NewL3ProviderFunc(mgr.L3ClientFunc())
	np := network.NewL2Network(orchestrator.NewNoopNetwork(), l2)
	np.SetL3(l3)
	np.SetServices(network.NewServicesProviderFunc(mgr.SvcClientFunc()))
	np.SetACL(network.NewAclProviderFunc(mgr.AclClientFunc()))
	np.SetNAT(network.NewNatProviderFunc(mgr.NatClientFunc()))
	np.SetBond(network.NewBondProviderFunc(mgr.BondClientFunc()))
	np.SetLldp(network.NewLldpProviderFunc(mgr.LldpClientFunc()))
	np.SetAlarms(network.NewAlarmStore())

	store, err := config.OpenStore(filepath.Join(t.TempDir(), "it.db"))
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	engine, err := config.NewEngine(store, orchestrator.NewApplier(np, orchestrator.NewNoopCompute(), orchestrator.NewNoopContainer()), config.Options{})
	if err != nil {
		t.Fatalf("装配引擎: %v", err)
	}
	t.Cleanup(engine.Close)
	return &harness{mgr: mgr, engine: engine, store: store, net: np, diag: mgr.Diagnostics(),
		sess: config.Session{User: "itest", Source: "integration"}}
}

// commit 以整份覆盖方式提交配置（编辑→替换 candidate→commit→释放锁）。
func (h *harness) commit(t *testing.T, cfg model.Config) config.CommitResult {
	t.Helper()
	ctx := context.Background()
	if err := h.engine.Edit(h.sess); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if err := h.engine.UpdateCandidate(h.sess, cfg); err != nil {
		t.Fatalf("update candidate: %v", err)
	}
	res, err := h.engine.Commit(ctx, h.sess, config.CommitOpts{Message: "integration"})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := h.engine.Release(h.sess); err != nil {
		t.Fatalf("release: %v", err)
	}
	return res
}

func baseConfig(addr string) model.Config {
	return model.Config{
		Interfaces: []model.InterfaceConfig{{Name: "ens192"}, {Name: "ens224"}},
		Vrfs: []model.Vrf{{Name: itVRF, L3Interfaces: []model.L3Interface{
			{Interface: "ens192", Addresses: []string{addr}},
		}}},
		VirtualSwitches: []model.VirtualSwitch{{
			Name: itVS, Type: "l2",
			Ports: []model.VSwitchPort{{Seq: 1, Interface: "ens224"}},
		}},
	}
}

func bdExists(t *testing.T, mgr *network.Manager, name string) bool {
	t.Helper()
	c, err := mgr.L2ClientFunc()()
	if err != nil {
		t.Fatalf("L2 客户端: %v", err)
	}
	defer c.Close()
	ok, err := c.BridgeDomainExists(network.BDID(name))
	if err != nil {
		t.Fatalf("查询 BD: %v", err)
	}
	return ok
}

func ifaceHasAddr(t *testing.T, mgr *network.Manager, addr string) bool {
	t.Helper()
	c, err := mgr.DiagClientFunc()()
	if err != nil {
		t.Fatalf("诊断客户端: %v", err)
	}
	defer c.Close()
	addrs, err := c.InterfaceAddresses(false)
	if err != nil {
		t.Fatalf("查询接口地址: %v", err)
	}
	for _, a := range addrs {
		if a.Prefix == addr {
			return true
		}
	}
	return false
}

var pingRecvRe = regexp.MustCompile(`(\d+) received`)

// TestE2EFlow 覆盖验收主链路：建交换机 → 通流 → 改配置 → 收敛。
func TestE2EFlow(t *testing.T) {
	sock := vppSocket(t)
	h := newHarness(t, sock)
	ctx := context.Background()

	// 1) 建交换机 + L3/VRF 并提交
	h.commit(t, baseConfig(itAddr))
	if !bdExists(t, h.mgr, itVS) {
		t.Fatalf("提交后 bridge-domain %s 未创建", itVS)
	}
	if !ifaceHasAddr(t, h.mgr, itAddr) {
		t.Fatalf("提交后 ens192 未配置地址 %s", itAddr)
	}

	// 2) 通流：经 VPP VRF ping 对端（缺省 NAT 网关，可经 NFVIS_IT_PING 覆盖）
	target := os.Getenv("NFVIS_IT_PING")
	if target == "" {
		target = "192.168.155.2"
	}
	out, err := h.diag.Ping(ctx, network.PingRequest{Host: target, VRF: itVRF, Count: 3})
	if err != nil {
		t.Fatalf("ping %s: %v\n%s", target, err, out)
	}
	m := pingRecvRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("ping 输出无统计行:\n%s", out)
	}
	recv, _ := strconv.Atoi(m[1])
	if recv == 0 {
		t.Fatalf("ping %s 经 VRF %s 未收到回包（0 received）:\n%s", target, itVRF, out)
	}
	t.Logf("通流验证：ping %s 收包 %d\n%s", target, recv, out)

	// 3) 改配置：更换 L3 地址并重新提交
	h.commit(t, baseConfig(itAddr2))
	if !ifaceHasAddr(t, h.mgr, itAddr2) {
		t.Fatalf("改配置后 ens192 未更新为新地址 %s", itAddr2)
	}

	// 4) 收敛：手工删除 BD（先摘成员）后 EnsureConsistent 应补建
	c, err := h.mgr.L2ClientFunc()()
	if err != nil {
		t.Fatalf("L2 客户端: %v", err)
	}
	idx, ok, err := c.SwInterfaceIndex("ens224")
	if err != nil || !ok {
		c.Close()
		t.Fatalf("解析 ens224: ok=%v err=%v", ok, err)
	}
	if err := c.SwInterfaceSetL2Bridge(idx, network.BDID(itVS), network.L2PortNormal, 0, false); err != nil {
		c.Close()
		t.Fatalf("摘除成员: %v", err)
	}
	if err := c.BridgeDomainAddDel(network.BDID(itVS), false, false, itVS); err != nil {
		c.Close()
		t.Fatalf("手工删 BD: %v", err)
	}
	c.Close()
	if bdExists(t, h.mgr, itVS) {
		t.Fatal("手工删除后 BD 应已不存在")
	}
	cfg, err := h.engine.Committed()
	if err != nil {
		t.Fatalf("读取 committed: %v", err)
	}
	if errs := h.net.EnsureConsistent(ctx, cfg); len(errs) > 0 {
		t.Fatalf("恢复收敛未收敛项: %v", errs)
	}
	if !bdExists(t, h.mgr, itVS) {
		t.Fatal("恢复收敛后 BD 未补建")
	}

	// 清理：删除本测试对象（BD 成员随删 BD 释放，再删 VRF 表）
	cleanupCfg := model.Config{Interfaces: []model.InterfaceConfig{{Name: "ens192"}, {Name: "ens224"}}}
	h.commit(t, cleanupCfg)
}
