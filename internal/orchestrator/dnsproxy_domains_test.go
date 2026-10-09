package orchestrator

// 决策 #439：内核数据面 DNS 代理的**域落点**派生（DNSProxyUpstreamsOf 的 Domains 部分）。
// 覆盖：L2 网关多 IPv4/含 IPv6、L3 l3-interface 跨接口收集与去重、显式网关 VRF 映射、
// 上游全空时落点照常派生、以及声明序确定性（plan 的 old/new 比较与巡检复现依赖它）。
// VPP 侧既有行为（Global/PerSwitch）由 apply_test.go 的 TestApplyDNSProxyPlan 与
// network 包的单测另行钉住——本文件只钉落点派生。

import (
	"reflect"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// L2 交换机的网关地址里 v4 全收、v6 不收，且保持声明序。
func TestDNSProxyDomainsL2IPv4OnlyOrdered(t *testing.T) {
	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{{
			Name: "vs-a", Type: "l2",
			Gateway: &model.VSGateway{Addresses: []string{
				"192.168.99.1/24", "2001:db8::1/64", "10.10.0.1/24",
			}},
		}},
	}
	got := DNSProxyUpstreamsOf(cfg)
	want := []DNSProxyDomain{{
		Name:      "vs-a",
		Addresses: []string{"192.168.99.1", "10.10.0.1"},
		VRFDevice: "vr-vs-a", // 无显式 gateway vrf ⇒ 派生 vr-<交换机名>
	}}
	if !reflect.DeepEqual(got.Domains, want) {
		t.Fatalf("L2 落点应只收 IPv4、去前缀并保持声明序: got %+v want %+v", got.Domains, want)
	}
}

// IPv6-only 网关没有 IPv4 落点（内核侧落点是 IPv4 UDP/53 socket）：该交换机不入 Domains。
func TestDNSProxyDomainsL2IPv6OnlyExcluded(t *testing.T) {
	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{{
			Name: "vs-v6", Type: "l2",
			Gateway: &model.VSGateway{Addresses: []string{"2001:db8::1/64"}},
		}},
	}
	if got := DNSProxyUpstreamsOf(cfg); len(got.Domains) != 0 {
		t.Fatalf("IPv6-only 网关不应产生域落点: %+v", got.Domains)
	}
}

// 显式 gateway vrf 优先于派生名（与校验侧 kernelNamedDomains 同一口径）。
func TestDNSProxyDomainsL2ExplicitGatewayVRF(t *testing.T) {
	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{{
			Name: "vs-a", Type: "l2",
			Gateway: &model.VSGateway{Addresses: []string{"192.168.99.1/24"}, Vrf: "vr-custom"},
		}},
	}
	got := DNSProxyUpstreamsOf(cfg)
	if len(got.Domains) != 1 {
		t.Fatalf("应有一个域落点: %+v", got.Domains)
	}
	if want := model.KernelGatewayVRFDevice("vs-a", "vr-custom"); got.Domains[0].VRFDevice != want {
		t.Fatalf("域设备应取模型映射: got %q want %q", got.Domains[0].VRFDevice, want)
	}
	if got.Domains[0].VRFDevice != "vr-custom" {
		t.Fatalf("显式 vr-custom（≤15 字符）应原样作为内核设备名: %q", got.Domains[0].VRFDevice)
	}
}

// L3：跨 l3-interface 收集全部 IPv4，按声明序去重（首次出现位置），域设备取条目名映射。
func TestDNSProxyDomainsL3InterfacesOrderedAndDeduped(t *testing.T) {
	cfg := model.Config{
		Vrfs: []model.Vrf{{
			Name: "vs-tx",
			L3Interfaces: []model.L3Interface{
				{Interface: "ens192", Addresses: []string{"192.168.50.1/24", "192.168.50.2/24"}},
				{Interface: "ens224", Addresses: []string{"2001:db8::1/64", "192.168.50.1/24"}},
			},
		}},
	}
	got := DNSProxyUpstreamsOf(cfg)
	want := []DNSProxyDomain{{
		Name:      "vs-tx",
		Addresses: []string{"192.168.50.1", "192.168.50.2"},
		VRFDevice: model.KernelVRFDevice("vs-tx"),
	}}
	if !reflect.DeepEqual(got.Domains, want) {
		t.Fatalf("L3 落点应跨接口收 IPv4、按声明序去重: got %+v want %+v", got.Domains, want)
	}
	if got.Domains[0].VRFDevice != "vs-tx" {
		t.Fatalf("VRF 设备名应是条目名本身（短名不截断）: %q", got.Domains[0].VRFDevice)
	}
}

// 落点派生与「启用判据」相互独立：无全局/按域上游也照常派生（内核侧该域查不到上游时回 SERVFAIL，
// 但 socket 落点仍要起——判据在实现侧，不在派生侧）。
func TestDNSProxyDomainsDerivedWithoutUpstreams(t *testing.T) {
	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{{
			Name: "vs-gw", Type: "l2",
			Gateway: &model.VSGateway{Addresses: []string{"192.168.99.1/24"}},
		}},
		Vrfs: []model.Vrf{{
			Name:         "vs-l3",
			L3Interfaces: []model.L3Interface{{Interface: "ens192", Addresses: []string{"10.0.0.1/24"}}},
		}},
	}
	got := DNSProxyUpstreamsOf(cfg)
	if len(got.Global) != 0 || len(got.PerSwitch) != 0 {
		t.Fatalf("本例前提：不该有全局/按域上游: %+v", got)
	}
	if len(got.Domains) != 2 || got.Domains[0].Name != "vs-gw" || got.Domains[1].Name != "vs-l3" {
		t.Fatalf("上游全空也应有落点，且 L2 域在 VRF 域之前: %+v", got.Domains)
	}
}

// 同一配置两次派生逐字段一致（顺序含 L2 声明序在前、VRF 声明序在后）——plan 的
// old/new reflect.DeepEqual 与巡检复现都依赖这一确定性。
func TestDNSProxyDomainsDeterministic(t *testing.T) {
	cfg := model.Config{
		Vpp: &model.VppConfig{DNSProxyServers: []string{"8.8.8.8"}},
		VirtualSwitches: []model.VirtualSwitch{
			{Name: "vs-b", Type: "l2", Gateway: &model.VSGateway{Addresses: []string{"192.168.99.1/24"}}},
			{Name: "vs-a", Type: "l3"},
		},
		Vrfs: []model.Vrf{
			{Name: "vs-a", L3Interfaces: []model.L3Interface{{Interface: "ens192", Addresses: []string{"10.1.0.1/24"}}}},
			{Name: "vs-z", L3Interfaces: []model.L3Interface{{Interface: "ens224", Addresses: []string{"10.2.0.1/24"}}}},
		},
	}
	first, second := DNSProxyUpstreamsOf(cfg), DNSProxyUpstreamsOf(cfg)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("同一配置两次派生必须逐字段一致: %+v vs %+v", first, second)
	}
	var names []string
	for _, d := range first.Domains {
		names = append(names, d.Name)
	}
	if !reflect.DeepEqual(names, []string{"vs-b", "vs-a", "vs-z"}) {
		t.Fatalf("域顺序应为 L2 声明序在前、VRF 声明序在后: %v", names)
	}
}
