package orchestrator

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

func TestVnfSocketPath(t *testing.T) {
	if got := VnfSocketPath("/run/nfvis/vhost", "fw-vm", "eth0"); got != "/run/nfvis/vhost/fw-vm-eth0.sock" {
		t.Fatalf("socket 路径: %s", got)
	}
}

func TestVnfIfaceName(t *testing.T) {
	if got := VnfIfaceName("fw-vm", "eth0"); got != "vh-fw-vm-eth0" {
		t.Fatalf("接口名: %s", got)
	}
	// 超长（>63）→ 确定性哈希名，且不截断碰撞。
	longVM := strings.Repeat("a", 60)
	longIface := strings.Repeat("b", 60)
	got := VnfIfaceName(longVM, longIface)
	if len(got) > 63 || !strings.HasPrefix(got, "vh-") {
		t.Fatalf("超长应回退哈希名且 ≤63: %q(%d)", got, len(got))
	}
	if got != VnfIfaceName(longVM, longIface) {
		t.Fatal("哈希名非确定")
	}
	if got == VnfIfaceName(longVM, longIface+"x") {
		t.Fatal("不同 vNIC 不应生成同名")
	}
}

func TestVnfPortTag(t *testing.T) {
	if got := VnfPortTag("fw-vm", "eth0"); got != "nfvis:vnf:fw-vm:eth0" {
		t.Fatalf("tag: %s", got)
	}
}

// ---------- 决策 #170：VNF/容器侧 vNIC 声明的交换机归属必须进入 BD 成员集 ----------

// nicOf 构造一个声明了虚拟交换机的 VNF vNIC。
func nicOf(name, vs string) model.VnfInterface {
	return model.VnfInterface{Name: name, Type: "vhost-user", VirtualSwitch: vs}
}

func portOfSwitch(vs model.VirtualSwitch, vnf, vnfIface string) (model.VSwitchPort, bool) {
	for _, p := range vs.Ports {
		if p.Vnf == vnf && p.VnfInterface == vnfIface {
			return p, true
		}
	}
	return model.VSwitchPort{}, false
}

// VNF 侧声明 virtual-switch 的 vNIC 必须成为该交换机的端口（否则 vhost 口永不进 BD）。
func TestSwitchMembersOfProjectsVnfNicDeclaration(t *testing.T) {
	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{
			{Name: "vs-a", Type: "l2", Ports: []model.VSwitchPort{{Seq: 3, Interface: "ens192"}}},
			{Name: "vs-l3", Type: "l3"},
		},
		VirtualMachineFunctions: []model.VMFunction{{Name: "vm-a", Interfaces: []model.VnfInterface{
			nicOf("eth0", "vs-a"),
			nicOf("eth1", "vs-l3"), // L3 交换机经同名 VRF 编排，不进 BD
			nicOf("eth2", ""),      // 未接入任何交换机
		}}},
		ContainerFunctions: []model.ContainerFunction{{Name: "ct-a", Interfaces: []model.VnfInterface{
			{Name: "eth0", Type: "memif", VirtualSwitch: "vs-a"},
		}}},
	}
	out, errs := SwitchMembersOf(cfg, DefaultVhostDir, DefaultMemifDir)
	if len(errs) != 0 {
		t.Fatalf("不应有无法归位的声明: %v", errs)
	}
	if len(out) != 2 || out[0].Name != "vs-a" {
		t.Fatalf("交换机顺序/数量不符: %+v", out)
	}
	// 交换机侧端口保留，且合成端口接在其后（序号不与配置内已用序号冲突）
	if len(out[0].Ports) != 3 || out[0].Ports[0].Interface != "ens192" {
		t.Fatalf("vs-a 应含 1 个既有端口 + 2 个合成端口: %+v", out[0].Ports)
	}
	vp, ok := portOfSwitch(out[0], "vm-a", "eth0")
	if !ok {
		t.Fatalf("VNF 的 vNIC eth0 应成为 vs-a 端口: %+v", out[0].Ports)
	}
	if vp.Seq <= 3 {
		t.Fatalf("合成端口序号应接在配置已用序号之后: %+v", out[0].Ports)
	}
	if got := VnfIfaceName(vp.Vnf, vp.VnfInterface); got != "vh-vm-a-eth0" {
		t.Fatalf("成员口应解析为 %s，实际 %s", "vh-vm-a-eth0", got)
	}
	ct, ok := func() (model.VSwitchPort, bool) {
		for _, p := range out[0].Ports {
			if p.Container == "ct-a" && p.ContainerInterface == "eth0" {
				return p, true
			}
		}
		return model.VSwitchPort{}, false
	}()
	if !ok || MemifIfaceName(ct.Container, ct.ContainerInterface) != "mf-ct-a-eth0" {
		t.Fatalf("容器的 memif 口应成为 vs-a 端口: %+v", out[0].Ports)
	}
	// L3 交换机不因 VNF 声明而获得端口（走 VRF 路径）
	if len(out[1].Ports) != 0 {
		t.Fatalf("L3 交换机不应有 L2 端口: %+v", out[1].Ports)
	}
	// 合成只作用于副本：调用方配置不得被改写（append 别名）
	if len(cfg.VirtualSwitches[0].Ports) != 1 {
		t.Fatalf("不得改写调用方配置: %+v", cfg.VirtualSwitches[0].Ports)
	}
}

// 交换机侧已显式声明的同一 vNIC 不重复添加（显式声明可带 trunk/native/acl 属性）。
func TestSwitchMembersOfKeepsExplicitPortOnce(t *testing.T) {
	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{{Name: "vs-a", Type: "l2", Ports: []model.VSwitchPort{
			{Seq: 1, Vnf: "vm-a", VnfInterface: "eth0", TrunkVlans: []int{100}},
		}}},
		VirtualMachineFunctions: []model.VMFunction{{Name: "vm-a", Interfaces: []model.VnfInterface{nicOf("eth0", "vs-a")}}},
	}
	out, errs := SwitchMembersOf(cfg, DefaultVhostDir, DefaultMemifDir)
	if len(errs) != 0 {
		t.Fatalf("不应有错误: %v", errs)
	}
	if len(out[0].Ports) != 1 || len(out[0].Ports[0].TrunkVlans) != 1 {
		t.Fatalf("显式端口应保留且不重复: %+v", out[0].Ports)
	}
}

// 声明的交换机不存在 → 返回错误（调用方据此失败/告警），不得静默跳过。
func TestSwitchMembersOfReportsUnresolvableSwitch(t *testing.T) {
	cfg := model.Config{
		VirtualMachineFunctions: []model.VMFunction{{Name: "vm-a", Interfaces: []model.VnfInterface{nicOf("eth0", "vs-ghost")}}},
	}
	out, errs := SwitchMembersOf(cfg, DefaultVhostDir, DefaultMemifDir)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "vs-ghost") || !strings.Contains(errs[0].Error(), "vm-a") {
		t.Fatalf("应报无法归位的声明（点名交换机与 vNIC）: %v", errs)
	}
	if len(out) != 0 {
		t.Fatalf("无交换机可投影: %+v", out)
	}
}
