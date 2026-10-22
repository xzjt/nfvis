package compute

// 修 BUG-05（round42 报告）：PCI 直通与**数据面占用**的运行期互斥。
//
// 背景：配置里数据面占用按端口名写（`vpp dpdk dev ens224`）、直通按 BDF 写，两者的映射
// 只在运行态（绑定记录 / sysfs）——model 是叶子包读不到，故 model 层那版校验在真实现场
// （per_dev=ens224 + pci-device 0000:13:00.0）**不会触发**。真机实测后果：commit 成功，
// start 才被 QEMU 拒：`Could not open '/dev/vfio/8': Device or resource busy`。
//
// 本用例钉住编排层的这道闸：BDF 被判为已被数据面占用 ⇒ define 前就拒绝并给照做路径；
// 未占用 ⇒ 正常定义（不误伤）；未注入检查器 ⇒ 与修复前逐字同行为。

import (
	"context"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// 数据面已占用的 BDF：define 前拒绝，文案点名占用者与释放路径，且**不定义域**。
func TestDefineVM_PCIDeviceClaimedByDataplaneRejected(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProvider(api, newMockStorage("/images/img.qcow2"), nil)
	p.SetPCIDeviceChecker(func(string) (bool, error) { return true, nil }) // 设备存在
	var asked []string
	p.SetDataplaneClaimChecker(func(bdf string) (string, bool) {
		asked = append(asked, bdf)
		if bdf == "0000:13:00.0" {
			return "vpp dpdk 单网卡覆盖（ens224）", true
		}
		return "", false
	})

	vm := vmFixture("fw-vm")
	vm.PCIDevices = []string{"0000:13:00.0"}
	err := p.DefineVM(context.Background(), vm, model.AllocatedResources{HugepageSize: "1G"})
	if err == nil {
		t.Fatal("已被数据面占用的设备应在 define 前被拒绝（否则 commit 成功、start 才炸）")
	}
	if len(asked) != 1 || asked[0] != "0000:13:00.0" {
		t.Fatalf("占用检查器应收到归一 BDF: %v", asked)
	}
	for _, want := range []string{"0000:13:00.0", "数据面占用", "ens224", "request vpp restart", "unbind-dpdk"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("拒绝文案应含 %q: %v", want, err)
		}
	}
	if len(api.defined) != 0 {
		t.Fatalf("拒绝时不得定义域: %v", api.defined)
	}
}

// 未被数据面占用：正常定义（不误伤；多个设备里只有被占的那个会拒）。
func TestDefineVM_PCIDeviceNotClaimedPasses(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProvider(api, newMockStorage("/images/img.qcow2"), nil)
	p.SetPCIDeviceChecker(func(string) (bool, error) { return true, nil })
	p.SetDataplaneClaimChecker(func(string) (string, bool) { return "", false })

	vm := vmFixture("fw-vm")
	vm.PCIDevices = []string{"0000:03:00.0"}
	if err := p.DefineVM(context.Background(), vm, model.AllocatedResources{HugepageSize: "1G"}); err != nil {
		t.Fatalf("未占用的设备应正常定义: %v", err)
	}
	if len(api.defined) != 1 || api.defined[0] != "fw-vm" {
		t.Fatalf("应定义 fw-vm: %v", api.defined)
	}
}

// 未注入占用检查器：行为与修复前一致（只做存在性检查）——保证既有调用方/单测不被打扰。
func TestDefineVM_NoDataplaneCheckerKeepsOldBehavior(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProvider(api, newMockStorage("/images/img.qcow2"), nil)
	p.SetPCIDeviceChecker(func(string) (bool, error) { return true, nil })

	vm := vmFixture("fw-vm")
	vm.PCIDevices = []string{"0000:13:00.0"}
	if err := p.DefineVM(context.Background(), vm, model.AllocatedResources{HugepageSize: "1G"}); err != nil {
		t.Fatalf("未注入占用检查时不应拒绝（与修复前同行为）: %v", err)
	}
}

// 设备不存在时先报「不在本机」——占用检查不得抢在存在性检查之前改变既有文案。
func TestDefineVM_MissingDeviceStillReportsMissingFirst(t *testing.T) {
	api := newMockLibvirt()
	p := newTestProvider(api, newMockStorage("/images/img.qcow2"), nil)
	p.SetPCIDeviceChecker(func(string) (bool, error) { return false, nil })
	p.SetDataplaneClaimChecker(func(string) (string, bool) { return "bond0 成员", true })

	vm := vmFixture("fw-vm")
	vm.PCIDevices = []string{"0000:13:00.0"}
	err := p.DefineVM(context.Background(), vm, model.AllocatedResources{HugepageSize: "1G"})
	if err == nil {
		t.Fatal("不存在的设备应被拒绝")
	}
	if !strings.Contains(err.Error(), "不在本机") {
		t.Fatalf("应优先报「不在本机」（既有文案）: %v", err)
	}
}
