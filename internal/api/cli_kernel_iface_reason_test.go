package api

// 决策 #431：内核数据面下「已声明却没进数据面」的口，`show interfaces physical` 的备注列
// 点名原因——① 仍绑定在 vfio-pci（附 PCI 与照做路径）；② 被 systemd-networkd 持有为 down
// （附「改为 manual 并 netplan apply」的人工做法）；取不到原因沿用既有「已声明未生效」。
//
// 事实源经 PortInventory 注入（fakePorts.ifReasons），本文件只验**渲染**；VPP 数据面下的
// 既有渲染不受影响（reason 只在内核数据面下被查询）。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// ifReasonKit 现场：声明 declared 里的口，注入端口清单与「原因」事实源；数据面由 vppCtl 自报。
func ifReasonKit(t *testing.T, dataplane string, declared, kernel, vpp []string, ifReasons map[string]network.KernelIfReason) *cliExecutor {
	t.Helper()
	x, engine := newCLIKit(t)
	x.setVppCtl(fakeVppCtl{st: VppStatus{Mode: dataplane}})
	x.setPorts(fakePorts{kernel: kernel, vpp: vpp, ifReasons: ifReasons})

	sess := config.Session{User: "system", Source: "console"}
	if err := engine.Edit(sess); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	cfg, _ := engine.Committed()
	if cfg.System == nil {
		cfg.System = &model.SystemConfig{}
	}
	cfg.System.DataPlane = dataplane
	for _, n := range declared {
		cfg.Interfaces = append(cfg.Interfaces, model.InterfaceConfig{Name: n})
	}
	if err := engine.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	if _, err := engine.Commit(context.Background(), sess, config.CommitOpts{}); err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			t.Fatalf("Commit: %v %+v", err, ve.Errors)
		}
		t.Fatalf("Commit: %v", err)
	}
	_ = engine.Release(sess)
	return x
}

// ① vfio 残留：口内核里没有、仍被 vfio-pci 占用 → 备注点名驱动/PCI 与交还路径。
func TestShowInterfacesKernelNamesVfioResidue(t *testing.T) {
	x := ifReasonKit(t, model.DataPlaneKernel, []string{"ens192"}, nil, nil,
		map[string]network.KernelIfReason{
			"ens192": {Kind: network.KernelIfReasonVFIO, Driver: "vfio-pci", PCI: "0000:0b:00.0"},
		})
	out := x.Execute("admin", aaaClassSU, "ssh", "show interfaces physical").Output
	for _, want := range []string{"ens192", "vfio-pci", "0000:0b:00.0", "request interfaces ens192 unbind-dpdk --yes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("内核数据面下 vfio 残留应在备注列点名 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "已声明未生效") {
		t.Fatalf("点名原因后不应再只写「已声明未生效」：\n%s", out)
	}
}

// ② networkd 持有为 down：口在内核里但管理态为 down → 备注点名 networkd 与人工做法。
func TestShowInterfacesKernelNamesNetworkdDown(t *testing.T) {
	x := ifReasonKit(t, model.DataPlaneKernel, []string{"ens192"}, []string{"ens192"}, nil,
		map[string]network.KernelIfReason{
			"ens192": {Kind: network.KernelIfReasonNetworkdDown},
		})
	// 内核运行态里该口存在但管理态 down（被策略压着）。
	x.setVppState(fakeVppState{ifs: map[string]InterfaceState{"ens192": {AdminUp: false}}})
	out := x.Execute("admin", aaaClassSU, "ssh", "show interfaces physical").Output
	for _, want := range []string{"ens192", "systemd-networkd", "netplan", "manual", "netplan apply", "不代改 netplan"} {
		if !strings.Contains(out, want) {
			t.Fatalf("内核数据面下 networkd 持有 down 应在备注列点名 %q：\n%s", want, out)
		}
	}
}

// ③ 取不到原因：沿用既有「已声明未生效」（不编造原因）。
func TestShowInterfacesKernelReasonUnavailableFallsBack(t *testing.T) {
	x := ifReasonKit(t, model.DataPlaneKernel, []string{"ens192"}, nil, nil, nil)
	out := x.Execute("admin", aaaClassSU, "ssh", "show interfaces physical").Output
	if !strings.Contains(out, "已声明未生效") {
		t.Fatalf("取不到原因时应沿用「已声明未生效」：\n%s", out)
	}
	for _, unwanted := range []string{"vfio-pci", "systemd-networkd"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("取不到原因时不得编造 %q：\n%s", unwanted, out)
		}
	}
}

// 口在内核里且管理态 up：已在数据面生效，不得点名原因（避免把正常口误标）。
func TestShowInterfacesKernelUpNotNamed(t *testing.T) {
	x := ifReasonKit(t, model.DataPlaneKernel, []string{"ens192"}, []string{"ens192"}, nil,
		map[string]network.KernelIfReason{
			"ens192": {Kind: network.KernelIfReasonNetworkdDown},
		})
	x.setVppState(fakeVppState{ifs: map[string]InterfaceState{"ens192": {AdminUp: true}}})
	out := x.Execute("admin", aaaClassSU, "ssh", "show interfaces physical").Output
	if strings.Contains(out, "systemd-networkd") {
		t.Fatalf("管理态 up 的口不应被点名原因：\n%s", out)
	}
}

// VPP 数据面：即使事实源给了原因也不查询/不渲染——既有渲染逐字不受影响。
func TestShowInterfacesVPPIgnoresKernelReason(t *testing.T) {
	x := ifReasonKit(t, model.DataPlaneVPP, []string{"ens192"}, []string{"ens192"}, nil,
		map[string]network.KernelIfReason{
			"ens192": {Kind: network.KernelIfReasonVFIO, Driver: "vfio-pci", PCI: "0000:0b:00.0"},
		})
	out := x.Execute("admin", aaaClassSU, "ssh", "show interfaces physical").Output
	for _, unwanted := range []string{"vfio-pci", "systemd-networkd", "unbind-dpdk"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("VPP 数据面下不得渲染内核原因 %q：\n%s", unwanted, out)
		}
	}
	// VPP 数据面下该口既不在 VPP 运行态 → 沿用既有「已声明未生效」
	if !strings.Contains(out, "已声明未生效") {
		t.Fatalf("VPP 数据面下应沿用既有「已声明未生效」：\n%s", out)
	}
}
