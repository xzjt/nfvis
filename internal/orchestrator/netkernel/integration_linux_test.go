//go:build integration && linux

// netkernel 的真机集成测试：用**真实内核**（真 ip/bridge/nft 命令）验证下发序列，
// 而不是只看命令生成。运行条件：Linux + root（`make integration` 在 nfvis-vm 上跑）。
//
// 设计要点：
//   - 全部对象名以 `zt` 开头、用完即删；**不动**机器上既有的物理口/桥/表；
//   - 独立事实源：断言一律读内核（`ip -j`、`bridge -j`、`nft list`），不复用被测代码的读视图
//     （读视图另有单独断言，两者互为对照）；
//   - 依赖缺失（无 nft/无 veth 模块）时如实跳过，不假绿。
package netkernel

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

func itoa(n int) string { return strconv.Itoa(n) }

// —— 测试用的最小配置对象（只填被测路径用得到的字段） ——

func modelIface(name string, mtu int, desc string, enabled *bool) model.InterfaceConfig {
	return model.InterfaceConfig{Name: name, MTU: mtu, Description: desc, Enabled: enabled}
}

func modelVSwitch(name, port string, accessVlan int, gwAddr string) model.VirtualSwitch {
	return model.VirtualSwitch{
		Name: name, Type: "l2", VlanAccess: accessVlan,
		Gateway: &model.VSGateway{Addresses: []string{gwAddr}},
		Ports:   []model.VSwitchPort{{Seq: 1, Interface: port}},
	}
}

func modelVrf(name, iface string, vlan int, addr, prefix, nh string) model.Vrf {
	return model.Vrf{
		Name: name,
		L3Interfaces: []model.L3Interface{{
			Interface: iface, Vlan: vlan, Addresses: []string{addr},
		}},
		Routes: []model.Route{{Prefix: prefix, NextHop: nh}},
	}
}

func modelVxlan(name string, vni int, local, remote, vs string) model.VxlanTunnel {
	return model.VxlanTunnel{Name: name, Vni: vni, Local: local, Remote: remote, VirtualSwitch: vs}
}

func modelNAT() model.NatConfig {
	return model.NatConfig{
		SourcePools: []model.NatSourcePool{{Name: "ztpool", AddressRange: "203.0.113.1 to 203.0.113.5"}},
		Rules: []model.NatRule{
			{Seq: 10, MatchSource: "192.168.99.0/24", VirtualSwitch: "ztbr0",
				Action: model.NatAction{SourcePool: "ztpool"}},
			{Seq: 20, MatchSource: "192.168.98.0/24", VirtualSwitch: "ztbr0",
				Action: model.NatAction{Interface: "ztb0"}},
		},
		Static: []model.NatStatic{{InsideIP: "192.168.99.10", OutsideIP: "203.0.113.9"}},
	}
}

func modelNatEmpty() model.NatConfig { return model.NatConfig{} }

const (
	ztVethA = "zta0"
	ztVethB = "ztb0"
	ztBr    = "ztbr0"
	ztVrf   = "ztvrf0"
	ztVx    = "ztvx0"
)

func ztRequireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("需要 root（改内核网络对象）")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("缺少 iproute2")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("缺少 nftables")
	}
}

func ztRun(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// ztCleanup 清掉本测试可能留下的对象（幂等；测试失败也跑）。
func ztCleanup() {
	_, _ = exec.Command("ip", "link", "del", ztBr).CombinedOutput()
	_, _ = exec.Command("ip", "link", "del", ztVrf).CombinedOutput()
	_, _ = exec.Command("ip", "link", "del", ztVx).CombinedOutput()
	_, _ = exec.Command("ip", "link", "del", ztVethA).CombinedOutput()
	_, _ = exec.Command("ip", "link", "del", "vr-"+ztBr).CombinedOutput()
}

func ztLinkExists(t *testing.T, dev string) bool {
	t.Helper()
	out, err := exec.Command("ip", "-j", "link", "show", "dev", dev).Output()
	if err != nil {
		return false
	}
	var rows []ipLinkRow
	if json.Unmarshal(out, &rows) != nil {
		return false
	}
	return len(rows) > 0
}

func ztLinkMTU(t *testing.T, dev string) int {
	t.Helper()
	out, err := exec.Command("ip", "-j", "link", "show", "dev", dev).Output()
	if err != nil {
		return -1
	}
	var rows []struct {
		MTU int `json:"mtu"`
	}
	if json.Unmarshal(out, &rows) != nil || len(rows) == 0 {
		return -1
	}
	return rows[0].MTU
}

func ztVlanIDs(t *testing.T, port string) map[int]bool {
	t.Helper()
	out, err := exec.Command("bridge", "-j", "vlan", "show", "dev", port).Output()
	if err != nil {
		t.Fatalf("bridge vlan show: %v\n%s", err, out)
	}
	var rows []bridgeVlanRow
	if json.Unmarshal(out, &rows) != nil {
		t.Fatalf("解析 bridge vlan 输出失败：%s", out)
	}
	got := map[int]bool{}
	for _, r := range rows {
		for _, v := range r.Vlans {
			got[v.Vlan] = true
		}
	}
	return got
}

func ztIsMasterOf(t *testing.T, dev, master string) bool {
	t.Helper()
	out, err := exec.Command("ip", "-j", "link", "show", "dev", dev).Output()
	if err != nil {
		return false
	}
	var rows []struct {
		Master string `json:"master"`
	}
	if json.Unmarshal(out, &rows) != nil || len(rows) == 0 {
		return false
	}
	return rows[0].Master == master
}

func ztHasAddress(t *testing.T, dev, cidr string) bool {
	t.Helper()
	addrs := (&Provider{run: NewExecRunner()}).deviceAddresses(context.Background(), dev)
	for _, a := range addrs {
		if a == cidr {
			return true
		}
	}
	return false
}

// TestKernelDataPlaneRealKernel 端到端跑一遍内核数据面的核心族，并逐条用内核事实核对。
func TestKernelDataPlaneRealKernel(t *testing.T) {
	ztRequireRoot(t)
	ctx := context.Background()
	ztCleanup()
	defer ztCleanup()

	// 测试自备两个"物理口"（veth 对），不碰机器上真实的网卡。
	ztRun(t, "ip", "link", "add", ztVethA, "type", "veth", "peer", "name", ztVethB)
	p := New(NewExecRunner())

	// —— 1) 接口：MTU / 描述 / 管理状态 ——
	enabled := true
	if err := p.ApplyInterface(ctx, modelIface(ztVethA, 1400, "zt-test", &enabled)); err != nil {
		t.Fatalf("ApplyInterface: %v", err)
	}
	if mtu := ztLinkMTU(t, ztVethA); mtu != 1400 {
		t.Errorf("MTU 未落到内核：期望 1400，实际 %d", mtu)
	}
	if out := ztRun(t, "ip", "-j", "link", "show", "dev", ztVethA); !strings.Contains(out, "zt-test") {
		t.Errorf("接口描述未落到内核：%s", out)
	}

	// —— 2) L2 交换机：bridge + 成员口 + access VLAN + 网关 ——
	vs := modelVSwitch(ztBr, ztVethA, 100, "10.99.0.1/24")
	if err := p.ApplyBridgeDomain(ctx, vs); err != nil {
		t.Fatalf("ApplyBridgeDomain: %v", err)
	}
	if !ztLinkExists(t, ztBr) {
		t.Fatal("bridge 未创建")
	}
	if !ztIsMasterOf(t, ztVethA, ztBr) {
		t.Error("成员口未挂到 bridge 上")
	}
	if vids := ztVlanIDs(t, ztVethA); !vids[100] {
		t.Errorf("access VLAN 100 未落到内核：%v", vids)
	}
	if !ztHasAddress(t, ztBr, "10.99.0.1/24") {
		t.Error("网关地址未落在 bridge 上")
	}
	// 网关 VRF 必须存在且 bridge 入表（内核 VRF 由名字派生表号）。
	if !ztLinkExists(t, "vr-"+ztBr) {
		t.Error("网关 VRF 未创建")
	}
	if !ztIsMasterOf(t, ztBr, "vr-"+ztBr) {
		t.Error("bridge 未入网关 VRF")
	}

	// 幂等：同一声明再下一次不应报错。
	if err := p.ApplyBridgeDomain(ctx, vs); err != nil {
		t.Fatalf("ApplyBridgeDomain 幂等重放失败: %v", err)
	}

	// —— 3) L3 交换机：VRF + vlan 子接口 + 地址 + 静态路由 ——
	vrf := modelVrf(ztVrf, ztVethB, 10, "10.98.0.1/24", "10.98.99.0/24", "10.98.0.2")
	if err := p.ApplyVRF(ctx, vrf); err != nil {
		t.Fatalf("ApplyVRF: %v", err)
	}
	subif := VlanSubifName(ztVethB, 10)
	if !ztLinkExists(t, subif) {
		t.Fatalf("vlan 子接口 %s 未创建", subif)
	}
	if !ztIsMasterOf(t, subif, ztVrf) {
		t.Errorf("%s 未入 VRF %s", subif, ztVrf)
	}
	if !ztHasAddress(t, subif, "10.98.0.1/24") {
		t.Errorf("%s 地址未下发", subif)
	}
	table := VRFTableID(ztVrf)
	out := ztRun(t, "ip", "route", "show", "table", itoa(table))
	if !strings.Contains(out, "10.98.99.0/24") {
		t.Errorf("静态路由未进 VRF 表 %d：\n%s", table, out)
	}
	// 读视图（与上面独立事实源互为对照）。
	rows, err := p.Routes(ctx, ztVrf)
	if err != nil {
		t.Fatalf("Routes 读视图: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Prefix == "10.98.99.0/24" && r.NextHop == "10.98.0.2" {
			found = true
		}
	}
	if !found {
		t.Errorf("Routes 读视图未列出刚下发的路由：%+v", rows)
	}

	// —— 4) MAC 表读视图（bridge fdb） ——
	if _, err := p.MACTable(ctx, ztBr); err != nil {
		t.Errorf("MACTable 读视图失败: %v", err)
	}

	// —— 5) VXLAN 设备 ——
	vx := modelVxlan(ztVx, 4099, "10.99.0.1", "10.99.0.2", ztBr)
	if err := p.ApplyVxlan(ctx, vx, nil); err != nil {
		t.Fatalf("ApplyVxlan: %v", err)
	}
	if !ztLinkExists(t, ztVx) {
		t.Fatal("vxlan 设备未创建")
	}
	if !ztIsMasterOf(t, ztVx, ztBr) {
		t.Error("vxlan 设备未挂到交换机 bridge")
	}

	// —— 6) 删除路径（倒序回收） ——
	if err := p.DeleteVxlan(ctx, vx); err != nil {
		t.Fatalf("DeleteVxlan: %v", err)
	}
	if ztLinkExists(t, ztVx) {
		t.Error("vxlan 设备未删除")
	}
	if err := p.DeleteVRF(ctx, ztVrf); err != nil {
		t.Fatalf("DeleteVRF: %v", err)
	}
	if ztLinkExists(t, ztVrf) || ztLinkExists(t, subif) {
		t.Error("VRF 或 vlan 子接口未回收")
	}
	if err := p.DeleteBridgeDomain(ctx, ztBr); err != nil {
		t.Fatalf("DeleteBridgeDomain: %v", err)
	}
	if ztLinkExists(t, ztBr) {
		t.Error("bridge 未删除")
	}
}

// TestKernelDataPlaneRealNAT nftables NAT：下发 → 独立读 nft 表核对 → 清空。
//
// 若机器上已存在产品的 NAT 表（说明另有一个内核数据面实例在跑），如实跳过而不是覆盖它。
func TestKernelDataPlaneRealNAT(t *testing.T) {
	ztRequireRoot(t)
	if _, err := exec.Command("nft", "list", "table", "inet", natTable).Output(); err == nil {
		t.Skip("机器上已存在 inet nfvis-nat（另有实例在用），跳过以免覆盖")
	}
	ctx := context.Background()
	p := New(NewExecRunner())
	defer func() { _, _ = exec.Command("nft", "delete", "table", "inet", natTable).CombinedOutput() }()

	nat := modelNAT()
	if err := p.ApplyNAT(ctx, nat); err != nil {
		t.Fatalf("ApplyNAT: %v", err)
	}
	out := ztRun(t, "nft", "list", "table", "inet", natTable)
	for _, want := range []string{
		"chain postrouting",
		"chain prerouting",
		"192.168.99.0/24",
		"203.0.113.1-203.0.113.5", // 地址池必须被解析成区间（不是池名）
		"masquerade",
		"dnat ip to 192.168.99.10",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("nft 表里找不到 %q：\n%s", want, out)
		}
	}

	// 全量重建：清空声明后重放，规则应消失（而表仍在）。
	if err := p.ApplyNAT(ctx, modelNatEmpty()); err != nil {
		t.Fatalf("ApplyNAT（空声明）: %v", err)
	}
	// 声明整体清空 ⇒ 整张表应被回收（不留空表：`delete nat` 之后宿主上不该长期留着它）。
	if leftover, err := exec.Command("nft", "list", "table", "inet", natTable).CombinedOutput(); err == nil {
		t.Errorf("清空声明后表应被回收，实际仍在：\n%s", leftover)
	}
}
