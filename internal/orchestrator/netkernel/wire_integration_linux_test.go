//go:build integration && linux

// Provider 级的真机集成测试：走**声明路径**（Provider.Apply* → 内核），验证装配层的接线
// 是否正确——各族的 manager 级用例已在各自文件里覆盖，这里补的是「谁在什么时候被调用、
// 绑定从哪条声明解析出来」这一段（例如 QoS 绑定由 interfaces[].ingress_policy 触发、
// ACL 绑定由 vrfs[].l3_interfaces[].acl_in 触发）。
package netkernel

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

const (
	zwA0, zwA1 = "zwa0", "zwa1"
	zwB0, zwB1 = "zwb0", "zwb1"
	zwC0       = "zwc0"
	zwBr       = "zwbr0"
	zwVrf      = "zwvrf0"
)

func zwCleanup() {
	for _, d := range []string{zwBr, zwVrf, zwA0, zwB0, zwC0} {
		_, _ = exec.Command("ip", "link", "del", d).CombinedOutput()
	}
}

func zwOut(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, _ := exec.Command(name, args...).CombinedOutput()
	return string(out)
}

// TestKernelDataPlaneBindingsRealKernel 声明 → 内核：五族绑定全部经 Provider 下发，并用
// 内核事实逐条核对；再经 TeardownInterface 逐条回收。
func TestKernelDataPlaneBindingsRealKernel(t *testing.T) {
	ztRequireRoot(t)
	ctx := context.Background()
	zwCleanup()
	defer zwCleanup()

	// 三对 veth：A 组做交换机成员（风暴抑制）、C 组做交换机成员（端口安全）、B 组做三层口与镜像源。
	for _, pair := range [][2]string{{zwA0, zwA1}, {zwB0, zwB1}, {zwC0, "zwc1"}} {
		ztRun(t, "ip", "link", "add", pair[0], "type", "veth", "peer", "name", pair[1])
	}

	p := New(NewExecRunner())
	acl := model.Acl{Name: "zwacl", Rules: []model.AclRule{
		{Seq: 10, Source: "any", Destination: "any", Protocol: "icmp", Action: "deny"},
		{Seq: 20, Source: "any", Destination: "any", Action: "permit"},
	}}
	// Provider 的绑定解析要能从配置快照里取到策略本体（与真实装配一致：EnsureConsistent 会先 SetConfig）。
	p.SetConfig(model.Config{
		Acls:        []model.Acl{acl},
		QosPolicies: []model.QosPolicy{{Name: "zwpol", Cir: 8000, Cbs: 1000}},
	})

	// —— 1) L2 交换机（两个成员口） ——
	if err := p.ApplyBridgeDomain(ctx, model.VirtualSwitch{
		Name: zwBr, Type: "l2", VlanAccess: 100,
		Ports: []model.VSwitchPort{{Seq: 1, Interface: zwA0}, {Seq: 2, Interface: zwC0}},
	}); err != nil {
		t.Fatalf("ApplyBridgeDomain: %v", err)
	}

	// —— 2) 接口声明：A0 风暴抑制、C0 端口安全、B0 限速（三族各绑一个口，互不干扰） ——
	if err := p.ApplyQos(ctx, model.QosPolicy{Name: "zwpol", Cir: 8000, Cbs: 1000}); err != nil {
		t.Fatalf("ApplyQos: %v", err)
	}
	if err := p.ApplyInterface(ctx, model.InterfaceConfig{
		Name: zwA0, MTU: 1400, StormControl: &model.StormControl{BroadcastKbps: 8000, MulticastKbps: 4000},
	}); err != nil {
		t.Fatalf("ApplyInterface（风暴抑制）: %v", err)
	}
	if err := p.ApplyInterface(ctx, model.InterfaceConfig{
		Name: zwC0, PortSecurity: []model.PortSecMAC{"aa:bb:cc:00:00:01"},
	}); err != nil {
		t.Fatalf("ApplyInterface（端口安全）: %v", err)
	}
	if err := p.ApplyInterface(ctx, model.InterfaceConfig{
		Name: zwB0, IngressPolicy: "zwpol",
	}); err != nil {
		t.Fatalf("ApplyInterface（限速）: %v", err)
	}

	// 独立事实源核对
	if got := zwOut(t, "tc", "filter", "show", "dev", zwA0, "ingress"); !strings.Contains(got, "police") {
		t.Errorf("风暴抑制未挂到 %s 入向：\n%s", zwA0, got)
	}
	if got := zwOut(t, "tc", "filter", "show", "dev", zwB0, "ingress"); !strings.Contains(got, "police") {
		t.Errorf("限速未挂到 %s 入向：\n%s", zwB0, got)
	}
	if got := zwOut(t, "nft", "list", "table", "netdev", "nfvis-portsec"); !strings.Contains(got, "aa:bb:cc:00:00:01") {
		t.Errorf("端口安全白名单未下发：\n%s", got)
	}
	if got := zwOut(t, "ip", "-j", "-d", "link", "show", "dev", zwC0); !strings.Contains(got, `"learning":false`) {
		t.Errorf("端口安全的桥口学习未关闭：%s", got)
	}
	// 读视图与内核实况一致（消费方真正读的字段：storm 的 Kinds、portsec 的 TagPresent/RuleCount
	// ——R2-15②：事实只塞进 Reason 时，detail 会把"在位工作"报成"未收敛"）。
	if sd, ok := p.StormDataplane(ctx, zwA0); !ok || !strings.Contains(sd.Reason, "广播") {
		t.Errorf("风暴抑制读视图与实况不符：ok=%v reason=%q", ok, sd.Reason)
	} else if kd := sd.Kinds["broadcast"]; !kd.PolicerPresent || kd.CirKbps != 8000 {
		t.Errorf("风暴抑制读视图的逐类速率与配置不符（期望 broadcast/8000）：%+v", sd.Kinds)
	}
	if pd, ok := p.PortSecDataplane(ctx, zwC0); !ok {
		t.Errorf("端口安全读视图应报在位：%+v", pd)
	} else if !pd.TagPresent || pd.RuleCount != 1 || !pd.Bound {
		t.Errorf("端口安全读视图字段与内核实况不符（链在场/1 条规则/已绑定）：%+v", pd)
	}

	// —— 3) ACL：ApplyACL + 三层接口 acl-in 绑定 ——
	if err := p.ApplyACL(ctx, acl); err != nil {
		t.Fatalf("ApplyACL: %v", err)
	}
	if err := p.ApplyVRF(ctx, model.Vrf{
		Name:         zwVrf,
		L3Interfaces: []model.L3Interface{{Interface: zwB1, Addresses: []string{"10.97.0.1/24"}, AclIn: "zwacl"}},
	}); err != nil {
		t.Fatalf("ApplyVRF（含 acl-in）: %v", err)
	}
	got := zwOut(t, "nft", "list", "table", "netdev", "nfvis-acl")
	if !strings.Contains(got, "jump") || !strings.Contains(got, zwB1) {
		t.Errorf("ACL 未绑定到三层接口 %s：\n%s", zwB1, got)
	}
	if name, ok := p.aclMgr().Bound(ctx, zwB1); !ok || name != "zwacl" {
		t.Errorf("ACL 绑定读视图不符：name=%q ok=%v", name, ok)
	}

	// —— 4) 端口镜像：源 B1 → 分析口 A1 ——
	if err := p.ApplySpan(ctx, model.PortMirroring{
		Name: "zwspan", Analyzer: zwA1,
		Source: model.PMSource{Interface: zwB1, Direction: "both"},
	}); err != nil {
		t.Fatalf("ApplySpan: %v", err)
	}
	if got := zwOut(t, "tc", "filter", "show", "dev", zwB1, "ingress"); !strings.Contains(got, "mirred") {
		t.Errorf("镜像未挂到 %s 入向：\n%s", zwB1, got)
	}

	// —— 5) 逐条回收 ——
	for _, ifc := range []model.InterfaceConfig{{Name: zwA0}, {Name: zwC0}, {Name: zwB0}} {
		if err := p.TeardownInterface(ctx, ifc); err != nil {
			t.Fatalf("TeardownInterface(%s): %v", ifc.Name, err)
		}
	}
	if got := zwOut(t, "tc", "filter", "show", "dev", zwA0, "ingress"); strings.Contains(got, "police") {
		t.Errorf("风暴抑制未回收：\n%s", got)
	}
	if got := zwOut(t, "tc", "filter", "show", "dev", zwB0, "ingress"); strings.Contains(got, "police") {
		t.Errorf("限速未回收：\n%s", got)
	}
	if got := zwOut(t, "nft", "list", "table", "netdev", "nfvis-portsec"); strings.Contains(got, "aa:bb:cc:00:00:01") {
		t.Errorf("端口安全白名单未回收：\n%s", got)
	}
	if got := zwOut(t, "ip", "-j", "-d", "link", "show", "dev", zwC0); strings.Contains(got, `"learning":false`) {
		t.Errorf("端口安全回收后桥口学习未恢复：%s", got)
	}
	if err := p.DeleteSpan(ctx, "zwspan"); err != nil {
		t.Fatalf("DeleteSpan: %v", err)
	}
	if got := zwOut(t, "tc", "filter", "show", "dev", zwB1, "ingress"); strings.Contains(got, "mirred") {
		t.Errorf("镜像未回收：\n%s", got)
	}
	// 三层接口回收时必须一并摘掉 ACL 绑定（否则 nft 里留着指向已消失接口的 jump）。
	if err := p.DeleteL3Interface(ctx, zwVrf, model.L3Interface{Interface: zwB1, AclIn: "zwacl"}); err != nil {
		t.Fatalf("DeleteL3Interface: %v", err)
	}
	if _, ok := p.aclMgr().Bound(ctx, zwB1); ok {
		t.Errorf("三层接口回收后 ACL 绑定仍在")
	}
	if err := p.DeleteVRF(ctx, zwVrf); err != nil {
		t.Fatalf("DeleteVRF: %v", err)
	}
	if err := p.DeleteBridgeDomain(ctx, zwBr); err != nil {
		t.Fatalf("DeleteBridgeDomain: %v", err)
	}
	if err := p.DeleteACL(ctx, "zwacl"); err != nil {
		t.Fatalf("DeleteACL: %v", err)
	}
}
