//go:build integration

// A-3（FR-NET-013）：IPv6 静态路由与 v6 转发的真机验证。
//
// 环境限制（如实记录）：nfvis-vm 只有 link-local IPv6、**无 v6 缺省路由**
// （VMnet8 NAT 不提供 IPv6），因此**没有外部 v6 对端**可 ping。
// 本测试用两条互补证据覆盖 v6 L3 能力：
//   ① 配置路径：v6 地址 + v6 静态路由（含默认路由 ::/0）经事务引擎下发，
//      在 VPP 中校验地址与 FIB 条目；
//   ② 数据路径：建 VPP tap，VPP 侧与内核侧各配同网段 v6 地址，
//      由 VPP ping 内核侧地址——真实经过 v6 FIB 查询 + 邻居发现（NDP）+ ICMPv6 往返。
// 未覆盖：跨设备 v6 通流（本环境无 v6 对端，属环境受限，与 LLDP/SR-IOV 同类）。

package integration

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

const (
	itVRF6   = "it-vrf6"
	itAddr6  = "2001:db8:155::1/64"
	itRoute6 = "2001:db8:aaaa::/64"
	itNext6  = "2001:db8:155::2"
	itDeflt6 = "::/0"

	tapName   = "tap0" // create tap 后 VPP 侧与内核侧同名（两端的接口）
	tapVPP6   = "2001:db8:7::1/64"
	tapKern6  = "2001:db8:7::2/64"
	tapKernIP = "2001:db8:7::2"
)

// v6Config 配置路径用：ens192 挂 v6 地址，附带 v6 静态路由（前缀 + 默认路由）。
func v6Config() model.Config {
	return model.Config{
		Interfaces: []model.InterfaceConfig{{Name: "ens192"}},
		Vrfs: []model.Vrf{{
			Name: itVRF6,
			L3Interfaces: []model.L3Interface{
				{Interface: "ens192", Addresses: []string{itAddr6}},
			},
			Routes: []model.Route{
				{Prefix: itRoute6, NextHop: itNext6},
				{Prefix: itDeflt6, NextHop: itNext6},
			},
		}},
	}
}

func ifaceHasAddr6(t *testing.T, mgr *network.Manager, addr string) bool {
	t.Helper()
	c, err := mgr.DiagClientFunc()()
	if err != nil {
		t.Fatalf("诊断客户端: %v", err)
	}
	defer c.Close()
	addrs, err := c.InterfaceAddresses(true)
	if err != nil {
		t.Fatalf("查询 v6 接口地址: %v", err)
	}
	for _, a := range addrs {
		if a.Prefix == addr {
			return true
		}
	}
	return false
}

func fibHasRoute6(t *testing.T, mgr *network.Manager, tableID uint32, prefix string) (network.RouteEntry, bool) {
	t.Helper()
	c, err := mgr.L3ClientFunc()()
	if err != nil {
		t.Fatalf("L3 客户端: %v", err)
	}
	defer c.Close()
	rows, err := c.Routes(tableID, true) // isIP6=true：v6 须显式指定协议（决策 #69）
	if err != nil {
		t.Fatalf("dump v6 FIB: %v", err)
	}
	for _, r := range rows {
		if r.Prefix == prefix {
			return r, true
		}
	}
	return network.RouteEntry{}, false
}

// ① 配置路径：v6 地址与 v6 静态路由经事务引擎下发并在 VPP 中生效。
func TestIPv6AddressAndStaticRoutes(t *testing.T) {
	sock := vppSocket(t)
	h := newHarness(t, sock)

	h.commit(t, v6Config())

	if !ifaceHasAddr6(t, h.mgr, itAddr6) {
		t.Fatalf("提交后 ens192 未配置 v6 地址 %s", itAddr6)
	}
	t.Logf("v6 地址已下发: %s", itAddr6)

	table := network.TableID(itVRF6)
	for _, p := range []string{itRoute6, itDeflt6} {
		r, ok := fibHasRoute6(t, h.mgr, table, p)
		if !ok {
			t.Fatalf("v6 静态路由 %s 未进入 VPP FIB（table %d）", p, table)
		}
		t.Logf("v6 静态路由已下发: %s via %s（table %d）", r.Prefix, r.NextHop, table)
	}

	// 清理：提交空配置，撤销 v6 地址与路由
	h.commit(t, model.Config{})
	if ifaceHasAddr6(t, h.mgr, itAddr6) {
		t.Fatalf("撤销配置后 v6 地址仍在: %s", itAddr6)
	}
}

// ② 数据路径：VPP ↔ 内核 v6 ICMPv6 真实往返（无外部 v6 对端时的最强可达证据）。
func TestIPv6ForwardingICMPv6(t *testing.T) {
	sock := vppSocket(t)
	h := newHarness(t, sock)
	ctx := context.Background()
	shell := network.NewVppctlShell("") // vppctl 经 CLI socket

	if out, err := shell.Run(ctx, "create", "tap"); err != nil {
		t.Fatalf("create tap: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = shell.Run(context.Background(), "delete", "tap", tapName)
		_ = exec.Command("ip", "addr", "del", tapKern6, "dev", tapName).Run()
	})
	time.Sleep(2 * time.Second)

	// VPP 侧
	if out, err := shell.Run(ctx, "set", "interface", "state", tapName, "up"); err != nil {
		t.Fatalf("set up: %v\n%s", err, out)
	}
	if out, err := shell.Run(ctx, "set", "interface", "ip", "address", tapName, tapVPP6); err != nil {
		t.Fatalf("vpp 侧 v6 地址: %v\n%s", err, out)
	}
	// 内核侧（同一 tap 的另一端）
	if out, err := exec.Command("ip", "link", "set", tapName, "up").CombinedOutput(); err != nil {
		t.Fatalf("内核侧 link up: %v\n%s", err, out)
	}
	if out, err := exec.Command("ip", "-6", "addr", "add", tapKern6, "dev", tapName).CombinedOutput(); err != nil {
		t.Fatalf("内核侧 v6 地址: %v\n%s", err, out)
	}
	time.Sleep(2 * time.Second)

	// VPP ping 内核侧 v6 地址（首包可能因 NDP 未解析而丢失）
	out, err := h.diag.Ping(ctx, network.PingRequest{Host: tapKernIP, Count: 4})
	if err != nil {
		t.Fatalf("ping6 %s: %v\n%s", tapKernIP, err, out)
	}
	m := pingRecvRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("ping 输出无统计行:\n%s", out)
	}
	recv, _ := strconv.Atoi(m[1])
	if recv == 0 {
		t.Fatalf("v6 ICMPv6 未收到回包:\n%s", out)
	}
	if !strings.Contains(out, "icmp_seq") {
		t.Fatalf("输出不含 ICMPv6 逐包行:\n%s", out)
	}
	t.Logf("v6 转发验证：VPP ping %s 收包 %d 条\n%s", tapKernIP, recv, out)
}
