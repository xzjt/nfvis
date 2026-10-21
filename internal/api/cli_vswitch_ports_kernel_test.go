package api

// 决策 #495（收口 R41-3）：内核数据面下 VNF 派生条目的展示名退回 `<vm>/<nic>`——本文件钉住
// **成员端口读视图**（`switchPortViews`＝配置派生 ∪ 运行态）在这条回退下的两件事：
//
//  1. 回退名**不与运行态成员撞名**：内核侧的运行态口是 libvirt 的 `vnetN`（接口名里不可能
//     出现 `/`，见 model 的 nameRe），故 `switchPortViews` 的展示名去重键不会把 `vnet0` 吞掉，
//     也不会把两条不同事实（声明 vs 运行态）误并成一条——数字口径与修复前一致（修复前是
//     `vh-…` 与 `vnet0` 并列两条，现在只是把前者换成可解释的语义名）。
//  2. VPP 侧**逐字不变**：派生名仍 `vh-<vm>-<nic>`，且与运行态同名成员按既有规则去重
//     （只出配置派生那一条）——这条是「改动不外溢到 VPP」的守护。

import (
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// switchPortsFixtureKernel switchPortsFixture 的内核数据面版本（其它字段逐字不变）。
func switchPortsFixtureKernel() model.Config {
	cfg := switchPortsFixture()
	cfg.System = &model.SystemConfig{DataPlane: model.DataPlaneKernel}
	return cfg
}

// 内核数据面：VNF 派生条目展示名退回 `<vm>/<nic>`，容器条目仍按 #442 给 veth 宿主端名；
// 运行态 `vnet0` 不被去重键吞掉、也不与任何派生名相撞。
func TestSwitchPortViewsKernelVNFLabelFallsBack(t *testing.T) {
	cfg := switchPortsFixtureKernel()
	rows := switchPortViews(cfg, "vs-a", []string{"ens224", "vnet0"})

	labels := make([]string, 0, len(rows))
	byLabel := map[string]model.SwitchPortView{}
	for _, r := range rows {
		l := switchPortLabel(r)
		labels = append(labels, l)
		byLabel[l] = r
	}
	// 静态口 + VNF 派生 + 容器派生 + 运行态 vnet0 = 4 条（ens224 的运行态同名成员按既有规则去重）。
	if len(rows) != 4 {
		t.Fatalf("内核侧应为 4 条（config+vnf+container+runtime），实得 %d: %v", len(rows), labels)
	}
	vnf, ok := byLabel["fw-vm/eth0"]
	if !ok {
		t.Fatalf("VNF 派生条目应退回 <vm>/<nic> 展示名 fw-vm/eth0，实得 %v", labels)
	}
	if vnf.Source != model.PortSourceVNF || vnf.Port != "" {
		t.Fatalf("VNF 派生条目应为 source=vnf 且 Port 为空（不编造设备名），实得 %+v", vnf)
	}
	if _, ok := byLabel["vh-fw-vm-eth0"]; ok {
		t.Fatalf("内核侧不得再出现 VPP 逻辑名 vh-fw-vm-eth0: %v", labels)
	}
	// 运行态 vnet0 必须仍可见（去重键只看展示名，`vnet0` 与 `fw-vm/eth0` 不同域）。
	rt, ok := byLabel["vnet0"]
	if !ok || rt.Source != model.PortSourceRuntime {
		t.Fatalf("运行态成员 vnet0 应作为 source=runtime 保留，实得 %v（%+v）", labels, byLabel)
	}
	// 容器派生名按 #442 给 veth 宿主端（内核侧真实设备），与本改动无关但同域守护。
	host, _ := model.ContainerVethNames("ct-b", "m0")
	if _, ok := byLabel[host]; !ok {
		t.Fatalf("容器派生条目应为 veth 宿主端 %s，实得 %v", host, labels)
	}
}

// VPP 数据面逐字不变：派生名仍 vh-…，且与运行态同名成员去重（只出配置派生那一条）。
func TestSwitchPortViewsVPPLabelUnchanged(t *testing.T) {
	cfg := switchPortsFixture() // 未配置 dataplane ⇒ 回落 vpp
	rows := switchPortViews(cfg, "vs-a", []string{"ens224", "vh-fw-vm-eth0", "mf-ct-b-m0"})

	labels := make([]string, 0, len(rows))
	byLabel := map[string]model.SwitchPortView{}
	for _, r := range rows {
		l := switchPortLabel(r)
		labels = append(labels, l)
		byLabel[l] = r
	}
	if len(rows) != 3 {
		t.Fatalf("VPP 侧应为 3 条（config+vnf+container，运行态同名成员去重），实得 %d: %v", len(rows), labels)
	}
	for label, wantSrc := range map[string]string{
		"ens224":        model.PortSourceConfig,
		"vh-fw-vm-eth0": model.PortSourceVNF,
		"mf-ct-b-m0":    model.PortSourceContainer,
	} {
		r, ok := byLabel[label]
		if !ok || r.Source != wantSrc {
			t.Fatalf("VPP 侧应有 %s（source=%s），实得 %v（%+v）", label, wantSrc, labels, byLabel)
		}
	}
	if got := byLabel["vh-fw-vm-eth0"].Port; got != "vh-fw-vm-eth0" {
		t.Fatalf("VPP 侧 VNF 派生名应逐字不变（vh-fw-vm-eth0），实得 %q", got)
	}
}
