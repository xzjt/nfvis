package compute

// O-4（报告 docs/REPORT-v3.0.29.md §6.3）：seed.iso 缺 `network-config`。
//
// 现场：`vm1`（Cirros）的 seed 里只有 `meta-data` + `user-data`——`cloud-localds` 只传两个
// 文件（见 disk_libvirt.go 的 cloudLocaldsSeed.Build）。依赖 cloud-init 网络配置的镜像起来后
// 网卡上没有 DHCP 客户端在跑 ⇒ 拿不到地址 ⇒ 反复重试 metadata（串口刷
// `failed N/20: request failed`），进不到登录态，产品侧日志/告警毫无提示。
//
// 修法：有 vNIC 时产出 cloud-init v2 网络配置（`ethN: dhcp4: true`），经
// `cloud-localds --network-config=<path>` 一并塞进 seed；无 vNIC 时行为与修复前一致（空串）。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

func vmWithNics(names ...string) model.VMFunction {
	vm := model.VMFunction{Name: "vm1", Image: "cirros"}
	for _, n := range names {
		vm.Interfaces = append(vm.Interfaces, model.VnfInterface{Name: n, Type: "vhost-user"})
	}
	return vm
}

// 单个 vNIC ⇒ v2 配置且 eth0 DHCP。
func TestBuildNetworkConfigSingleNic(t *testing.T) {
	got := BuildNetworkConfig(vmWithNics("nic0"))
	if !strings.Contains(got, "version: 2") {
		t.Fatalf("应为 cloud-init v2：%q", got)
	}
	if !strings.Contains(got, "eth0:") || !strings.Contains(got, "dhcp4: true") {
		t.Fatalf("应含 eth0/dhcp4: true：%q", got)
	}
	if strings.Contains(got, "eth1:") {
		t.Fatalf("单 vNIC 不应出现 eth1：%q", got)
	}
}

// 两个 vNIC ⇒ 按声明顺序 eth0/eth1（libvirt 按 vNIC 声明顺序挂 virtio）。
func TestBuildNetworkConfigTwoNics(t *testing.T) {
	got := BuildNetworkConfig(vmWithNics("nic0", "nic1"))
	for _, want := range []string{"eth0:", "eth1:", "dhcp4: true"} {
		if !strings.Contains(got, want) {
			t.Fatalf("应含 %q：%q", want, got)
		}
	}
	// 顺序：eth0 在 eth1 之前（与 vNIC 声明序一致）
	if strings.Index(got, "eth0:") > strings.Index(got, "eth1:") {
		t.Fatalf("eth0 应在 eth1 之前（按 vNIC 声明顺序）：%q", got)
	}
	if strings.Contains(got, "eth2:") {
		t.Fatalf("只有两个 vNIC，不应出现 eth2：%q", got)
	}
}

// 无 vNIC ⇒ 空串（不产出 network-config，行为与修复前一致）。
func TestBuildNetworkConfigNoNics(t *testing.T) {
	if got := BuildNetworkConfig(model.VMFunction{Name: "vm1"}); got != "" {
		t.Fatalf("无 vNIC 应返回空串（不产出 network-config）：%q", got)
	}
}

// 传参形态：有网络配置时带 `--network-config=<path>`；无则**不**带（旧两件形态）。
// 参数顺序按 `cloud-localds --help`：`cloud-localds [options] output user-data [meta-data]`。
func TestCloudLocaldsArgs(t *testing.T) {
	got := cloudLocaldsArgs("/vms/vm1/seed.iso", "/vms/vm1/user-data", "/vms/vm1/meta-data",
		"/vms/vm1/network-config")
	// 4 个参数：iso + --network-config=<path> + user-data + meta-data
	if len(got) != 4 {
		t.Fatalf("应 4 个参数（iso + option + user-data + meta-data），实际 %d: %v", len(got), got)
	}
	if got[0] != "/vms/vm1/seed.iso" {
		t.Fatalf("第 1 个参数应是输出 iso（cloud-localds 的 output 位置）：%v", got)
	}
	if got[1] != "--network-config=/vms/vm1/network-config" {
		t.Fatalf("应传 --network-config=<path>：%v", got)
	}
	if got[2] != "/vms/vm1/user-data" || got[3] != "/vms/vm1/meta-data" {
		t.Fatalf("user-data / meta-data 应排在选项之后（cloud-localds 的位置参数）：%v", got)
	}

	// 无网络配置：逐字回到修复前的三件形态
	bare := cloudLocaldsArgs("/vms/vm1/seed.iso", "/vms/vm1/user-data", "/vms/vm1/meta-data", "")
	want := []string{"/vms/vm1/seed.iso", "/vms/vm1/user-data", "/vms/vm1/meta-data"}
	if len(bare) != len(want) {
		t.Fatalf("无 network-config 时应恰好 3 个参数：%v", bare)
	}
	for i := range want {
		if bare[i] != want[i] {
			t.Fatalf("无 network-config 时参数 %d 应为 %q，实际 %q（整组 %v）", i, want[i], bare[i], bare)
		}
	}
}

// 生成的 network-config 必须是**能被 cloud-init 解析的 YAML 结构**（最小自校准：
// 不引外部依赖，只校验缩进层级与键名——与产品其它「产物形状」守护同口径）。
func TestBuildNetworkConfigShape(t *testing.T) {
	got := BuildNetworkConfig(vmWithNics("nic0", "nic1"))
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if strings.HasPrefix(lines[0], " ") {
		t.Fatalf("首行应是顶层注释/键：%q", lines[0])
	}
	var sawVersion, sawEthernets bool
	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "#"):
			continue
		case ln == "version: 2":
			sawVersion = true
		case ln == "ethernets:":
			sawEthernets = true
		}
	}
	if !sawVersion || !sawEthernets {
		t.Fatalf("顶层应含 `version: 2` 与 `ethernets:`：%q", got)
	}
}
