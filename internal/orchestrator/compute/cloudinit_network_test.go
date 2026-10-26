package compute

// seed 的 `network-config`：O-4（报告 docs/REPORT-v3.0.29.md §6.3）引入，决策 #503（R47-1）改形态。
//
// 现场（O-4）：`vm1`（Cirros）的 seed 里只有 `meta-data` + `user-data`——`cloud-localds` 只传两个
// 文件（见 disk_libvirt.go 的 cloudLocaldsSeed.Build）。依赖 cloud-init 网络配置的镜像起来后
// 网卡上没有 DHCP 客户端在跑 ⇒ 拿不到地址 ⇒ 反复重试 metadata（串口刷
// `failed N/20: request failed`），进不到登录态，产品侧日志/告警毫无提示。
// 修法：有 vNIC 时产出 cloud-init v2 网络配置，经 `cloud-localds --network-config=<path>` 一并
// 塞进 seed；无 vNIC 时行为与修复前一致（空串）。
//
// 现场（R47-1，round47 真机）：初版按 `ethN`（vNIC 声明顺序）写键，而 Debian 12+/13 有 systemd
// 可预测命名会把 virtio 网卡改名成 `enp1s0` ⇒ netplan 指向不存在的 eth0 ⇒ guest 一个 DHCP 报文
// 都不发 ⇒ 租约永远拿不到（VM 却 running、vNIC 却 up，产品侧零提示）。
// 决策 #503 改成**按 MAC 匹配**：每块 vNIC 一条 `<vNIC 名>: {match: {macaddress}, set-name, dhcp4}`，
// 与 guest 的命名策略无关。MAC 取配置显式值，否则 DefaultMAC(vm, vNIC)——与 domain XML 同一函数。

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

// netplanEntry 从生成的 network-config 里解出的一条 ethernets 条目（顺序保留）。
type netplanEntry struct {
	Name     string // ethernets 下的键（= vNIC 名）
	MAC      string // match.macaddress 的字面值（已去引号）
	SetName  string // set-name 的字面值（已去引号）
	DHCP4    bool
	HasMatch bool
}

// parseNetworkConfig 最小自校准解析器：只认本产品产出的形状（顶层注释/version/ethernets +
// 每条 4 级缩进固定键）。不引外部 YAML 依赖；**遇到认不出的行直接判失败**——这样「形状漂了」
// 不会被静默放过（与产品其它「产物形状」守护同口径）。
func parseNetworkConfig(t *testing.T, cfg string) []netplanEntry {
	t.Helper()
	var (
		entries []netplanEntry
		sawVer  bool
		sawEths bool
		cur     *netplanEntry
		inMatch bool
	)
	for _, ln := range strings.Split(strings.TrimRight(cfg, "\n"), "\n") {
		switch {
		case ln == "" || strings.HasPrefix(ln, "#"):
			continue
		case ln == "version: 2":
			sawVer = true
		case ln == "ethernets:":
			sawEths = true
		case strings.HasPrefix(ln, "  ") && !strings.HasPrefix(ln, "   ") && strings.HasSuffix(ln, ":"):
			entries = append(entries, netplanEntry{Name: unquoteYAML(strings.TrimSuffix(strings.TrimPrefix(ln, "  "), ":"))})
			cur = &entries[len(entries)-1]
			inMatch = false
		case ln == "    match:" && cur != nil:
			cur.HasMatch = true
			inMatch = true
		case strings.HasPrefix(ln, "      macaddress: ") && cur != nil && inMatch:
			cur.MAC = unquoteYAML(strings.TrimPrefix(ln, "      macaddress: "))
		case strings.HasPrefix(ln, "    set-name: ") && cur != nil:
			cur.SetName = unquoteYAML(strings.TrimPrefix(ln, "    set-name: "))
			inMatch = false
		case ln == "    dhcp4: true" && cur != nil:
			cur.DHCP4 = true
			inMatch = false
		default:
			t.Fatalf("network-config 出现认不出的行 %q（形状漂了）:\n%s", ln, cfg)
		}
	}
	if !sawVer || !sawEths {
		t.Fatalf("顶层应含 `version: 2` 与 `ethernets:`：%q", cfg)
	}
	return entries
}

// unquoteYAML 去掉简单双引号（本产品只产出裸名或双引号名，不含转义）。
func unquoteYAML(s string) string {
	if len(s) >= 2 && strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		return s[1 : len(s)-1]
	}
	return s
}

// 单 vNIC：键名 = vNIC 名、MAC = DefaultMAC(vm, vNIC)（逐字）、有 set-name、dhcp4: true。
func TestBuildNetworkConfigSingleNic(t *testing.T) {
	vm := vmWithNics("nic0")
	got := BuildNetworkConfig(vm)

	entries := parseNetworkConfig(t, got)
	if len(entries) != 1 {
		t.Fatalf("单 vNIC 应恰好一条 ethernets 条目，实际 %d：%q", len(entries), got)
	}
	e := entries[0]
	if e.Name != "nic0" {
		t.Errorf("键应为 vNIC 名 nic0，实际 %q：%q", e.Name, got)
	}
	wantMAC := DefaultMAC("vm1", "nic0")
	if e.MAC != wantMAC {
		t.Errorf("match.macaddress 应为 DefaultMAC(%q,%q)=%q，实际 %q：%q", "vm1", "nic0", wantMAC, e.MAC, got)
	}
	if !e.HasMatch {
		t.Errorf("应含 match 段：%q", got)
	}
	if e.SetName != "nic0" {
		t.Errorf("set-name 应为 vNIC 名 nic0，实际 %q：%q", e.SetName, got)
	}
	if !e.DHCP4 {
		t.Errorf("应启用 dhcp4: true：%q", got)
	}
}

// 显式 MAC 优先：配置声明了 interfaces[].mac 时，match.macaddress 逐字等于显式值。
func TestBuildNetworkConfigExplicitMAC(t *testing.T) {
	vm := vmWithNics("nic0")
	vm.Interfaces[0].MAC = "52:54:00:aa:bb:cc"
	got := BuildNetworkConfig(vm)

	entries := parseNetworkConfig(t, got)
	if len(entries) != 1 || entries[0].MAC != "52:54:00:aa:bb:cc" {
		t.Fatalf("match.macaddress 应为显式声明的 52:54:00:aa:bb:cc：%q", got)
	}
	if entries[0].MAC == DefaultMAC("vm1", "nic0") {
		t.Fatalf("显式 MAC 不应被 DefaultMAC 覆盖（两者恰好相同也说明用例选值不当）：%q", got)
	}
}

// 多 vNIC：一块一条、顺序与声明一致、MAC 互不串（每条的 MAC 只对应它自己的 vNIC）。
func TestBuildNetworkConfigMultipleNics(t *testing.T) {
	vm := vmWithNics("nic0", "nic1", "nic2")
	// 中间那块用显式 MAC，检查「按块取值」不会整份串味。
	vm.Interfaces[1].MAC = "52:54:00:11:22:33"
	got := BuildNetworkConfig(vm)

	entries := parseNetworkConfig(t, got)
	wantNames := []string{"nic0", "nic1", "nic2"}
	if len(entries) != len(wantNames) {
		t.Fatalf("应有 %d 条条目，实际 %d：%q", len(wantNames), len(entries), got)
	}
	wantMACs := []string{
		DefaultMAC("vm1", "nic0"),
		"52:54:00:11:22:33",
		DefaultMAC("vm1", "nic2"),
	}
	for i, e := range entries {
		if e.Name != wantNames[i] {
			t.Errorf("第 %d 条键应为 %q（与声明顺序一致），实际 %q：%q", i, wantNames[i], e.Name, got)
		}
		if e.MAC != wantMACs[i] {
			t.Errorf("第 %d 条 MAC 应为 %q，实际 %q（每块一条、互不串）：%q", i, wantMACs[i], e.MAC, got)
		}
		if e.SetName != wantNames[i] {
			t.Errorf("第 %d 条 set-name 应为 %q，实际 %q：%q", i, wantNames[i], e.SetName, got)
		}
		if !e.DHCP4 {
			t.Errorf("第 %d 条应启用 dhcp4：%q", i, got)
		}
	}
}

// 无 vNIC ⇒ 空串（不产出 network-config，既有行为逐字不变）。
func TestBuildNetworkConfigNoNics(t *testing.T) {
	if got := BuildNetworkConfig(model.VMFunction{Name: "vm1"}); got != "" {
		t.Fatalf("无 vNIC 应返回空串（不产出 network-config）：%q", got)
	}
}

// 反向守护（R47-1 的判据核心）：输出里**不得出现按序号写死的 `ethN:` 键**。
// 这条用例在旧实现（`ethernets: {eth0: {dhcp4: true}}`）上**必然失败**——它守的就是
// 「可预测命名的镜像上 netplan 指向不存在的 eth0」这个现场。
func TestBuildNetworkConfigNoHardcodedEthKeys(t *testing.T) {
	got := BuildNetworkConfig(vmWithNics("nic0"))
	// 旧实现的确切产物形态（键 + 仅 dhcp4）：
	if strings.Contains(got, "  eth0:") {
		t.Fatalf("不应出现按序号写死的 eth0 键（可预测命名镜像上它指向不存在的设备）：%q", got)
	}
	if strings.Contains(got, "eth1:") || strings.Contains(got, "eth2:") {
		t.Fatalf("不应出现任何 ethN 序号键：%q", got)
	}
	// MAC 匹配是本形态的判据：没有 match/macaddress 就等于没修。
	if !strings.Contains(got, "match:") || !strings.Contains(got, "macaddress:") {
		t.Fatalf("应按 MAC 匹配网卡（须含 match/macaddress）：%q", got)
	}
	if !strings.Contains(got, "set-name:") {
		t.Fatalf("应把 guest 内名字设回 vNIC 名（须含 set-name）：%q", got)
	}
}

// 防漂移守护：network-config 的 MAC 必须与**域定义里的 MAC 逐字相同**。
//
// 两处各算一遍 MAC（而不是共用 DefaultMAC）就会产生「netplan 匹配不到任何网卡」——
// 复现出来又是「guest 不发 DHCP」的老症状，且产品侧同样零提示。这里直接把
// BuildDomainXML 的产物与 BuildNetworkConfig 的产物并排比对。
func TestBuildNetworkConfigMACSameAsDomainXML(t *testing.T) {
	// 以 baseSpec 的 VM 为底（它带齐 vcpu/memory/disk 等域组装必需字段），只换名字与 vNIC。
	vm := baseSpec().VM
	vm.Name = "vm1"
	vm.Interfaces = []model.VnfInterface{
		{Name: "nic0", Type: "vhost-user"},
		{Name: "nic1", Type: "vhost-user"},
	}
	spec := baseSpec()
	spec.VM = vm
	for _, nic := range vm.Interfaces {
		spec.Interfaces = append(spec.Interfaces, InterfaceSpec{
			Name: nic.Name, Type: IfaceVhostUser, Socket: "/s/" + nic.Name + ".sock",
		})
	}
	xml := mustXML(t, spec)
	entries := parseNetworkConfig(t, BuildNetworkConfig(vm))
	if len(entries) != 2 {
		t.Fatalf("应两条条目：%q", BuildNetworkConfig(vm))
	}
	for _, e := range entries {
		if !strings.Contains(xml, `address="`+e.MAC+`"`) {
			t.Fatalf("vNIC %s 的 network-config MAC %s 不在域定义里（seed 与域定义已漂移）\n--- XML ---\n%s",
				e.Name, e.MAC, xml)
		}
	}
	// 显式 MAC 同样要与域定义一致。
	vm.Interfaces[0].MAC = "52:54:00:de:ad:01"
	spec.Interfaces[0].MAC = "52:54:00:de:ad:01"
	xml = mustXML(t, spec)
	e0 := parseNetworkConfig(t, BuildNetworkConfig(vm))[0]
	if e0.MAC != "52:54:00:de:ad:01" || !strings.Contains(xml, `address="52:54:00:de:ad:01"`) {
		t.Fatalf("显式 MAC 应在 seed 与域定义两处一致：%q\n--- XML ---\n%s", e0.MAC, xml)
	}
}

// 名字渲染：vNIC 名会同时作为 YAML **键**；纯数字与 YAML 1.1 的裸字面量（true/yes/on/null…）
// 不加引号会被解析成非字符串，netplan 的 ethernets 键必须是字符串 ⇒ 整份配置校验失败。
func TestBuildNetworkConfigNameScalarSafety(t *testing.T) {
	// 普通名字逐字输出（读起来与契约 #503 一致，不加多余引号）。
	got := BuildNetworkConfig(vmWithNics("nic0"))
	if !strings.Contains(got, "\n  nic0:\n") {
		t.Fatalf("普通 vNIC 名应逐字作为键：%q", got)
	}
	// 需要引号的名字：键与 set-name 都加双引号，解析回来仍是原名。
	for _, name := range []string{"123", "true", "No", "on", "null"} {
		vm := vmWithNics(name)
		cfg := BuildNetworkConfig(vm)
		entries := parseNetworkConfig(t, cfg)
		if len(entries) != 1 || entries[0].Name != name {
			t.Fatalf("vNIC 名 %q 渲染后应仍解析回原名：%q", name, cfg)
		}
		if entries[0].SetName != name {
			t.Fatalf("vNIC 名 %q 的 set-name 应仍为原名，实际 %q：%q", name, entries[0].SetName, cfg)
		}
		if !strings.Contains(cfg, `"`+name+`"`) {
			t.Fatalf("vNIC 名 %q 在 YAML 里须加引号（否则被解析成非字符串）：%q", name, cfg)
		}
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
