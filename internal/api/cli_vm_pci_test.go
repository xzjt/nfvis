package api

// FR-CMP-023：通用 PCI 直通设备——语句 → 模型 → 提交 → 读视图（CLI/REST 同源）
// 与 display set 往返；追加语义（同值幂等）、按值删除/清空、非法 BDF 即时报错。

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

func TestCLIVMPCIDeviceStatement(t *testing.T) {
	x, engine := newCLIKit(t)

	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set virtual-machine-functions fw-vm image base.qcow2",
		"set virtual-machine-functions fw-vm vcpu count 2 pin false",
		"set virtual-machine-functions fw-vm memory size-mb 512",
		"set virtual-machine-functions fw-vm memory backing normal",
		"set virtual-machine-functions fw-vm pci-device 0000:03:00.0",
	)
	cfg, _, err := engine.Candidate()
	if err != nil {
		t.Fatalf("读取 candidate: %v", err)
	}
	if len(cfg.VirtualMachineFunctions) != 1 ||
		len(cfg.VirtualMachineFunctions[0].PCIDevices) != 1 ||
		cfg.VirtualMachineFunctions[0].PCIDevices[0] != "0000:03:00.0" {
		t.Fatalf("pci-device 应追加进 VMFunction.PCIDevices: %+v", cfg.VirtualMachineFunctions)
	}

	// 同值（含不同写法）重复 set：无变更提示（非错误、不重复追加）。
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "set virtual-machine-functions fw-vm pci-device 03:00.0")
	if !res.Warning || strings.Contains(res.Output, "%%") || !strings.Contains(res.Output, "未产生配置变更") {
		t.Fatalf("重复设备应为「无变更」提示: warning=%v out=%q", res.Warning, res.Output)
	}
	cfg, _, _ = engine.Candidate()
	if len(cfg.VirtualMachineFunctions[0].PCIDevices) != 1 {
		t.Fatalf("重复设备不得追加第二条: %+v", cfg.VirtualMachineFunctions[0].PCIDevices)
	}

	// 追加第二台设备（存原始写法，往返以声明值为准）。
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"set virtual-machine-functions fw-vm pci-device 04:10.1")
	cfg, _, _ = engine.Candidate()
	if len(cfg.VirtualMachineFunctions[0].PCIDevices) != 2 ||
		cfg.VirtualMachineFunctions[0].PCIDevices[1] != "04:10.1" {
		t.Fatalf("第二台设备应追加: %+v", cfg.VirtualMachineFunctions[0].PCIDevices)
	}

	// 提交后从 committed 读：display set 反推（每设备一条）与 detail 配置面视图。
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "commit", "exit")
	ds := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration | display set").Output
	for _, want := range []string{
		"set virtual-machine-functions fw-vm pci-device 0000:03:00.0",
		"set virtual-machine-functions fw-vm pci-device 04:10.1",
	} {
		if !strings.Contains(ds, want) {
			t.Fatalf("display set 应反推出 %q:\n%s", want, ds)
		}
	}
	// 注入设备事实源：detail 附「已在系统中 / 未在系统中」实测态（FR-CMP-023）。
	x.pciExists = func(bdf string) (bool, error) { return bdf == "0000:03:00.0", nil }
	detail := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-machine-functions fw-vm detail").Output
	for _, want := range []string{"pci-devices", "0000:03:00.0", "04:10.1", "已在系统中", "未在系统中"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail 应列出直通 PCI 设备与实测态（%q）:\n%s", want, detail)
		}
	}
	// 事实源未接入：如实说无法核对（不猜）。
	x.pciExists = nil
	detail = x.Execute("admin", aaa.ClassSuperUser, "ssh", "show virtual-machine-functions fw-vm detail").Output
	if !strings.Contains(detail, "无法核对") {
		t.Fatalf("未接入事实源应如实说无法核对:\n%s", detail)
	}

	// 按值删除（不同写法即同一设备）：只剩第二条。
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "delete virtual-machine-functions fw-vm pci-device 0000:03:00.0", "commit", "exit")
	cfg, _ = engine.Committed()
	if len(cfg.VirtualMachineFunctions[0].PCIDevices) != 1 ||
		cfg.VirtualMachineFunctions[0].PCIDevices[0] != "04:10.1" {
		t.Fatalf("delete 应按归一 BDF 去掉一条: %+v", cfg.VirtualMachineFunctions[0].PCIDevices)
	}

	// 删除不存在的一条：如实报「无匹配配置」。
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "configure")
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "delete virtual-machine-functions fw-vm pci-device 0000:03:00.0")
	if !strings.Contains(res.Output, "无匹配配置") {
		t.Fatalf("删除不存在的设备应报无匹配配置: %q", res.Output)
	}

	// 不带取值清空本 VM 全部设备。
	run(t, x, "admin", aaa.ClassSuperUser, "ssh", "delete virtual-machine-functions fw-vm pci-device", "commit", "exit")
	cfg, _ = engine.Committed()
	if len(cfg.VirtualMachineFunctions[0].PCIDevices) != 0 {
		t.Fatalf("清空后不应再有直通设备: %+v", cfg.VirtualMachineFunctions[0].PCIDevices)
	}
}

// 非法 BDF 在 set 期即报错（与提交期校验同源 NormalizeBDF），不落进 candidate。
func TestCLIVMPCIDeviceSyntaxRejectedAtSetTime(t *testing.T) {
	x, engine := newCLIKit(t)
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure", "set virtual-machine-functions fw-vm image base.qcow2")
	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "set virtual-machine-functions fw-vm pci-device 0000:03:00.9")
	if !strings.Contains(res.Output, "function") {
		t.Fatalf("非法 function 应即时报语法错误: %q", res.Output)
	}
	cfg, _, _ := engine.Candidate()
	if len(cfg.VirtualMachineFunctions[0].PCIDevices) != 0 {
		t.Fatalf("非法值不得落进 candidate: %+v", cfg.VirtualMachineFunctions[0].PCIDevices)
	}
	// set 形态缺取值：按语法不完整拒绝（不得借清空语义静默得逞）。
	res = x.Execute("admin", aaa.ClassSuperUser, "ssh", "set virtual-machine-functions fw-vm pci-device")
	if !strings.Contains(res.Output, "配置不完整") || strings.Contains(res.Output, "[ok]") {
		t.Fatalf("set 缺取值应报配置不完整: %q", res.Output)
	}
}

// REST：VMFunction.pci_devices 读写与 CLI 同源（同一模型字段）。
func TestRESTVMPCIDevices(t *testing.T) {
	ts := newTestServer(t)
	token := loginAdmin(t, ts)
	seedVMPool(t, ts, token)

	body := vmBody("fw-vm")
	body["pci_devices"] = []string{"0000:03:00.0"}
	status, _, data := cfgRequest(t, http.MethodPost, ts.URL+APIPrefix+"/virtual-machine-functions", token,
		body, map[string]string{"X-NFVIS-Auto-Commit": "true"})
	if status != http.StatusCreated {
		t.Fatalf("创建带直通设备的 VM: %d %s", status, data)
	}

	status, _, data = cfgRequest(t, http.MethodGet, ts.URL+APIPrefix+"/virtual-machine-functions/fw-vm", token, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(data), `"pci_devices":["0000:03:00.0"]`) {
		t.Fatalf("详情应回 pci_devices: %d %s", status, data)
	}
}
