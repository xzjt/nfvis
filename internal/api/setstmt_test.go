package api

import (
	"reflect"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// display set 往返性质测试（决策 #155）：语句清单 → 空配置 → display set 反推 →
// 语句再回放 → 配置深比较必须一致。任何一条语句反推不出来或还原不等即红——
// 这正是 #84 担心的「复制配置静默错误」的结构性排除证明。
//
// 用例分两批：
//   - roundTripBaseCases：机械家族（无别名，通用逆走器直接覆盖）——基线必绿；
//   - roundTripAliasCases：别名家族（CLI 嵌套 ⇄ 模型扁平，需家族逆映射发射器，
//     setstmt_families.go）——发射器全部就位后必须全绿。

var roundTripBaseCases = [][]string{
	{"set system hostname nfvis-node"},
	{"set interfaces ens192 mtu 9000"},
	{"set interfaces ens192 description to-TOR"},
	{"set interfaces ens192 description \"to TOR-A\""}, // 含空格取值：引号往返
	{"set interfaces ens192 disable"},
}

// fixture 书写须知（别名表的 apply 语义决定，逐条实测校准）：
//   - 大量别名 apply 用 elemByID 取属主元素（只读，不存在即报「无匹配配置」）——
//     属主必须由更早的语句建出（virtual-switches 用 type、VM/容器用机械可成句的
//     image/vcpu 等语句先行）。
//   - `system kernel params <p>` 一条语句只带一个参数（4 token 别名），多参数逐条 set。
//   - `bonds <n> members` 每条语句只追加一个成员；5 token 形态（members <seq> <if>）
//     取末位 token 为接口名，等价。
//   - `virtual-switches <n> ports <seq> vnf|container <x>` 的 6 token 形态固定按 delete
//     语义处理，set 必须写满 8 token（带 interface）。
//   - l3-interface/static-routes（落点 vrfs 数组）与 qos policies（落点 qos_policies 数组）
//     由根级回落发射器覆盖（rootFallbackEmitters：模型键非树关键字，通用逆走不可达）；
//     回放依赖 vs 侧先发出 type l3（家族发射器先于根层回落执行）。
var roundTripAliasCases = [][]string{
	// system.login（别名：login user ⇄ users 数组、password ⇄ password_hash、class ⇄ classes）
	{"set system login user admin password Admin@123 class super-user"},
	{"set system login class ops allow show", "set system login class ops deny request"},
	// system.management / dns / kernel params（别名或数组标量）
	{"set system management interface ens160", "set system management ip address 192.168.1.10/24", "set system management gateway 192.168.1.1"},
	{"set system dns server 8.8.8.8", "set system dns server 8.8.4.4"},
	{"set system dns server 8.8.8.8 secondary 8.8.4.4"},
	{"set system kernel params transparent_hugepage=never"},
	// system.ntp / health / syslog / api tls（别名）
	{"set system ntp server 1.2.3.4 prefer", "set system ntp server 5.6.7.8"},
	{"set system health thresholds cpu-temp-celsius 85", "set system health thresholds disk-used-percent 90"},
	{"set system syslog host 10.0.0.1 port 514 facility daemon severity info", "set system syslog local level warn", "set system syslog local retention-days 30"},
	{"set system api tls cert-file /etc/nfvis/tls.crt key-file /etc/nfvis/tls.key"},
	// virtual-switches（type/gateway/vlan/ports/cross-connect）
	{"set virtual-switches vs1 type l2"},
	{"set virtual-switches vs2 type l3"}, // l3 壳：同名 VRF 空壳条目随语句自复现
	// vrfs 落点的 L3 数据（根级回落发射器）
	{"set virtual-switches vs-l3 type l3",
		"set virtual-switches vs-l3 l3-interface bvi0 ip address 10.0.0.1/24",
		"set virtual-switches vs-l3 l3-interface bvi0 ip address fd00::1/64",
		"set virtual-switches vs-l3 l3-interface bvi0 acl-in acl1",
		"set virtual-switches vs-l3 static-routes 0.0.0.0/0 next-hop 10.0.0.254",
		"set virtual-switches vs-l3 static-routes 10.8.0.0/16 next-hop 10.0.0.1 distance 5"},
	// 决策 #381：多下一跳（ECMP）——整串为单 token、逗号无空格，往返一致
	{"set virtual-switches vs-l3 type l3",
		"set virtual-switches vs-l3 static-routes 10.0.0.0/8 next-hop 10.0.0.1,10.0.0.2"},
	// qos_policies 落点（根级回落发射器）
	{"set qos policies p1 cir 100000000 cbs 2000"},
	// 决策 #383：vxlan_tunnels 落点（根级回落发射器）——必填三叶一行给全，
	// dst-port/virtual-switch 为可选叶子（含两者的形态一并往返）。
	{"set vxlan tunnels tun1 vni 100 local 10.99.0.1 remote 10.99.0.2"},
	{"set virtual-switches vs1 type l2", "set vxlan tunnels tun1 vni 100 local 10.99.0.1 remote 10.99.0.2 dst-port 5789 virtual-switch vs1"},
	{"set virtual-switches vs1 type l2", "set virtual-switches vs1 vlan access 100"},
	{"set virtual-switches vs1 type l2", "set virtual-switches vs1 gateway ip 192.168.100.1/24"},
	// 决策 #335：dhcp-relay（模型单值字符串，语句树两层关键字）
	{"set virtual-switches vs1 type l2", "set virtual-switches vs1 gateway ip 192.168.100.1/24", "set virtual-switches vs1 dhcp-relay server 192.168.100.2"},
	// 决策 #359：dhcp-server（5 个平铺键；pool 一行两值，可选叶子各一行）
	{"set virtual-switches vs1 type l2", "set virtual-switches vs1 gateway ip 192.168.100.1/24", "set virtual-switches vs1 dhcp-server pool 192.168.100.100 192.168.100.200"},
	{"set virtual-switches vs1 type l2", "set virtual-switches vs1 gateway ip 192.168.100.1/24", "set virtual-switches vs1 dhcp-server pool 192.168.100.100 192.168.100.200", "set virtual-switches vs1 dhcp-server lease-time 600", "set virtual-switches vs1 dhcp-server dns 8.8.8.8", "set virtual-switches vs1 dhcp-server domain-name lab.local"},
	{"set virtual-switches vs1 type l2", "set virtual-switches vs1 ports 1 interface ens192 trunk vlans 100,200"},
	{"set virtual-switches vs1 type l2", "set virtual-switches vs1 ports 1 vnf vnf-a interface eth0"},
	{"set virtual-switches vs1 type l2", "set virtual-switches vs1 ports 1 container ct1 interface memif0"},
	// cross-connect 是开关（模型 bool，树为显式取值叶子）：反推 `cross-connect true`，
	// 端口不再入语句（由该交换机的 ports 承担）
	{"set virtual-switches vs1 type l2", "set virtual-switches vs1 ports 1 interface ens192", "set virtual-switches vs1 ports 2 interface ens224", "set virtual-switches vs1 cross-connect true"},
	// virtual-machine-functions（vcpu/memory/interfaces/serial）
	{"set virtual-machine-functions vnf-a vcpu count 2", "set virtual-machine-functions vnf-a vcpu count 2 pin true"},
	{"set virtual-machine-functions vnf-a vcpu count 2", "set virtual-machine-functions vnf-a memory size-mb 512", "set virtual-machine-functions vnf-a memory numa node 0"},
	{"set virtual-machine-functions vnf-a vcpu count 2", "set virtual-machine-functions vnf-a interfaces eth0 virtual-switch vs1"},
	{"set virtual-machine-functions vnf-a vcpu count 2", "set virtual-machine-functions vnf-a serial console enable"},
	// FR-CMP-023：通用 PCI 直通设备（pci_devices 数组 ⇄ pci-device 关键字，末尾追加发射器）
	{"set virtual-machine-functions vnf-a vcpu count 2", "set virtual-machine-functions vnf-a pci-device 0000:03:00.0", "set virtual-machine-functions vnf-a pci-device 0000:04:1f.7"},
	{"set virtual-machine-functions vnf-a vcpu count 2", "set virtual-machine-functions vnf-a pci-device 03:00.0"},
	// container-functions（vcpu/memory/env/args/interfaces）
	{"set container-functions ct1 image alpine:3.20", "set container-functions ct1 vcpu count 2", "set container-functions ct1 memory size-mb 512"},
	{"set container-functions ct1 image alpine:3.20", "set container-functions ct1 env KEY value", "set container-functions ct1 env PATH /usr/bin"},
	{"set container-functions ct1 image alpine:3.20", "set container-functions ct1 args --bind 8080 -v"},
	{"set container-functions ct1 image alpine:3.20", "set container-functions ct1 interfaces eth0 type memif", "set container-functions ct1 interfaces eth0 virtual-switch vs1"},
	// bonds / acls / nat / port-mirroring / protocols lldp（别名或数组标量）
	{"set bonds bond0 members ens192", "set bonds bond0 members ens224"},
	{"set bonds bond0 lacp mode active interval fast"},
	{"set acls acl1 rule 10 action permit", "set acls acl1 rule 10 source 10.0.0.0/8 protocol tcp"},
	{"set nat source-pool pool1 address-range 100.64.0.10 to 100.64.0.20", "set nat rules 1 match source 10.0.0.0/24 virtual-switch vs1 action source-pool pool1", "set nat static 10.0.0.5 to 203.0.113.5"},
	{"set port-mirroring session1 source interface ens192 direction both", "set port-mirroring session1 analyzer interface ens224"},
	{"set protocols lldp enable true", "set protocols lldp advertisement-interval 30", "set protocols lldp interface ens192 enable true"},
	// vpp dpdk / resource-pools numa / interfaces ingress-policy（别名）
	{"set vpp dpdk dev rx-queues 4", "set vpp dpdk dev ens192 rx-queues 2"},
	// round81 真机实测抓到：per_dev 条目可以**只有身份字段**（{interface: ens192}，
	// 由 4-token 裸语句创建）——发射器必须先发创建语句，否则整个 dpdk 块回放消失
	{"set vpp dpdk dev ens192", "set vpp dpdk dev ens224"},
	{"set resource-pools cpu numa node 0 cores 4,5", "set resource-pools cpu isolated-cores 4,5"},
	{"set interfaces ens192 ingress-policy p1"},
	// 接口风暴抑制（决策 #385）：接口须先声明；两类各一条语句（回放自校验覆盖发射器）
	{"set interfaces ens192 description sd",
		"set interfaces ens192 storm-control broadcast 8000",
		"set interfaces ens192 storm-control multicast 20000"},
	// 主机防火墙（决策 #388）：default_policy 平铺 + 规则数组（JSON 键 rules ⇄ 树关键字 rule，
	// 单复数不一致，须显式发射器）；含声明序 ≠ seq 序的数组（数组声明序必须原样还原）。
	{"set system firewall default-policy drop",
		"set system firewall rule 100 action accept source 192.168.1.0/24 protocol tcp port 22"},
	{"set system firewall rule 200 action drop protocol icmp",
		"set system firewall rule 100 action accept source 2001:db8::/32 protocol udp"},
	{"set system firewall rule 10 action accept source 10.0.0.1"}, // 裸 IP 原样落库（渲染时按 /32 归一）
}

// runDisplaySetRoundTrip 单组语句的往返：apply → toJSONTree → 反推 → 再 apply → 深比较。
func runDisplaySetRoundTrip(t *testing.T, stmts []string) {
	t.Helper()
	build := func() map[string]any {
		var cfg model.Config
		for _, line := range stmts {
			toks := splitFieldsQuoted(line)
			toks = toks[1:] // 去掉 set
			if err := applyStatement(&cfg, toks); err != nil {
				t.Fatalf("fixture 语句 %q 回放失败（fixture 本身必须合法）: %v", line, err)
			}
		}
		return toJSONTree(cfg)
	}
	orig := build()

	w, err := generateSetStmts(orig, nil)
	if err != nil {
		t.Fatalf("display set 反推失败: %v\n原配置: %v", err, orig)
	}
	if len(w.out) == 0 {
		t.Fatalf("display set 反推出 0 条语句（配置不为空却无输出）")
	}

	var rebuilt model.Config
	for _, s := range w.out {
		if err := applyStatement(&rebuilt, s.toks); err != nil {
			t.Fatalf("反推语句 %v 回放失败: %v", s.toks, err)
		}
	}
	want := normalizeJSON(stripUnreplayable(any(orig)))
	got := normalizeJSON(stripUnreplayable(any(toJSONTree(rebuilt))))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("往返不等：\n原配置   %v\n反推语句 %v\n回放结果 %v",
			orig, w.out, got)
	}
}

func TestDisplaySetRoundTripBase(t *testing.T) {
	for i, stmts := range roundTripBaseCases {
		runDisplaySetRoundTrip(t, stmts)
		_ = i
	}
}

func TestDisplaySetRoundTripAliasFamilies(t *testing.T) {
	for _, stmts := range roundTripAliasCases {
		runDisplaySetRoundTrip(t, stmts)
	}
}

// TestDisplaySetMultiNextHopSingleToken（决策 #381）：多下一跳整串必须是**单 token**
// （逗号两侧无空格）——否则 display set 输出经分词器会变成两条语句、回放即不等。
func TestDisplaySetMultiNextHopSingleToken(t *testing.T) {
	var cfg model.Config
	for _, line := range []string{
		"set virtual-switches vs-l3 type l3",
		"set virtual-switches vs-l3 static-routes 10.0.0.0/8 next-hop 10.0.0.1,10.0.0.2",
	} {
		if err := applyStatement(&cfg, splitFieldsQuoted(line)[1:]); err != nil {
			t.Fatalf("fixture 回放失败: %v", err)
		}
	}
	tree := toJSONTree(cfg)
	lines, err := renderSetStatements(tree, nil)
	if err != nil {
		t.Fatalf("反推失败: %v", err)
	}
	joined := strings.Join(lines, "\n")
	want := "set virtual-switches vs-l3 static-routes 10.0.0.0/8 next-hop 10.0.0.1,10.0.0.2"
	if !strings.Contains(joined, want) {
		t.Fatalf("display set 应输出单 token 多下一跳 %q:\n%s", want, joined)
	}
	if strings.Contains(joined, "10.0.0.1, 10.0.0.2") {
		t.Fatalf("多下一跳整串不得被拆开（逗号后不应有空格）:\n%s", joined)
	}
	if got := cfg.Vrfs[0].Routes[0].NextHop; got != "10.0.0.1,10.0.0.2" {
		t.Fatalf("回放取值 = %q，期望整串保留", got)
	}
}

// TestDisplaySetMultiStatementConfig：多条语句共存时反推必须完整（组合而非单条）。
func TestDisplaySetMultiStatementConfig(t *testing.T) {
	runDisplaySetRoundTrip(t, []string{
		"set system hostname nfvis-node",
		"set interfaces ens192 mtu 9000",
		"set interfaces ens192 description to-TOR",
		"set resource-pools hugepages page-size 1G count 32",
		"set system ntp server 1.2.3.4 prefer",
	})
}

// TestDisplaySetSensitiveOmitted：敏感值不入 display set 输出（口令哈希无直设语句，
// 占位符回放会造成静默的凭据替换，决策 #155）——以注释行说明，其余语句照常。
func TestDisplaySetSensitiveOmitted(t *testing.T) {
	var cfg model.Config
	if err := applyStatement(&cfg, splitFieldsQuoted("set system login user admin password Admin@123 class super-user")[1:]); err != nil {
		t.Fatalf("fixture 回放失败: %v", err)
	}
	tree := toJSONTree(cfg)
	lines, err := renderSetStatements(tree, nil)
	if err != nil {
		t.Fatalf("反推失败: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "Admin@123") {
		t.Fatalf("敏感值不得出现在 display set 输出: %v", lines)
	}
	if strings.Contains(joined, "password_hash") {
		t.Fatalf("哈希字段不得作为语句输出: %v", lines)
	}
	if !strings.Contains(joined, "# ") {
		t.Fatalf("省略的敏感值应有注释说明: %v", lines)
	}
	if !strings.Contains(joined, "set system login user admin class super-user") {
		t.Fatalf("class 等非敏感语句照常输出: %v", lines)
	}
}

// TestDisplaySetLevelPrefix：配置模式层级 show（structuredPath 非空）反推的语句
// 必须带绝对路径前缀。
func TestDisplaySetLevelPrefix(t *testing.T) {
	var cfg model.Config
	for _, line := range []string{"set interfaces ens192 mtu 9000", "set interfaces ens192 description to-TOR"} {
		if err := applyStatement(&cfg, splitFieldsQuoted(line)[1:]); err != nil {
			t.Fatalf("fixture 回放失败: %v", err)
		}
	}
	full := toJSONTree(cfg)
	sub, err := navigateJSON(full, []string{"interfaces", "ens192"})
	if err != nil {
		t.Fatalf("导航失败: %v", err)
	}
	lines, err := renderSetStatements(sub.(map[string]any), []string{"interfaces", "ens192"})
	if err != nil {
		t.Fatalf("反推失败: %v", err)
	}
	if len(lines) == 0 {
		t.Fatalf("子树反推出 0 条语句")
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "set interfaces ens192") {
			t.Fatalf("层级反推的语句应带绝对路径前缀: %q", l)
		}
	}
}

// TestDisplaySetFirewallLevelPrefix（决策 #388）：`edit system firewall` 层级的反推
// 带绝对路径前缀（与顶层同源、走家族发射器的同一分支）。
func TestDisplaySetFirewallLevelPrefix(t *testing.T) {
	var cfg model.Config
	for _, line := range []string{
		"set system firewall default-policy drop",
		"set system firewall rule 100 action accept source 10.0.0.0/8",
	} {
		if err := applyStatement(&cfg, splitFieldsQuoted(line)[1:]); err != nil {
			t.Fatalf("fixture 回放失败: %v", err)
		}
	}
	sub, err := navigateJSON(toJSONTree(cfg), []string{"system", "firewall"})
	if err != nil {
		t.Fatalf("导航失败: %v", err)
	}
	lines, err := renderSetStatements(sub.(map[string]any), []string{"system", "firewall"})
	if err != nil {
		t.Fatalf("反推失败: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("防火墙层级反推出 0 条语句")
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "set system firewall") {
			t.Fatalf("层级反推的语句应带绝对路径前缀: %q", l)
		}
	}
}

// TestDisplaySetEmptyL3SwitchRoundTrip（决策 #452，修 round12 R12-4）：空 L3 交换机
// （`set virtual-switches X type l3` 无内容）与其它 L3 交换机共存、且 vrfs[] 顺序与
// virtual_switches[] 声明序不一致时，display set 反推必须无错误且回放深等——空 VRF
// 由 vrfs[] 回落发射器的确定性创建语句自足重建（不再依赖 `type l3` 别名的副作用，
// 此前顺序即破）。
func TestDisplaySetEmptyL3SwitchRoundTrip(t *testing.T) {
	// vs-A 先建成 l2、vs-B 建成 l3、再把 vs-A 改成 l3：virtual_switches=[vs-A,vs-B]，
	// 但 vrfs[] 顺序=[vs-B,vs-A]（VRF 随 type l3 建立的先后追加）。
	runDisplaySetRoundTrip(t, []string{
		"set virtual-switches vs-A type l2",
		"set virtual-switches vs-B type l3",
		"set virtual-switches vs-A type l3",
	})
	// 空 L3 + 带数据的 L3 混合（同样顺序错位）。
	runDisplaySetRoundTrip(t, []string{
		"set virtual-switches vs-A type l2",
		"set virtual-switches vs-B type l3",
		"set virtual-switches vs-B l3-interface ens192 ip address 10.0.0.1/24",
		"set virtual-switches vs-A type l3",
	})
}

// TestDisplaySetL3SwitchSingleCreationStatement（决策 #452）：空 L3 交换机的创建语句
// 只有一条（顶层 `type l3` 让位给 vrfs[] 回落，不双发）。
func TestDisplaySetL3SwitchSingleCreationStatement(t *testing.T) {
	var cfg model.Config
	for _, line := range []string{
		"set virtual-switches vs-A type l2",
		"set virtual-switches vs-B type l3",
		"set virtual-switches vs-A type l3",
	} {
		if err := applyStatement(&cfg, splitFieldsQuoted(line)[1:]); err != nil {
			t.Fatalf("fixture %q 回放失败: %v", line, err)
		}
	}
	lines, err := renderSetStatements(toJSONTree(cfg), nil)
	if err != nil {
		t.Fatalf("反推失败: %v", err)
	}
	for _, name := range []string{"vs-A", "vs-B"} {
		want := "set virtual-switches " + name + " type l3"
		n := 0
		for _, l := range lines {
			if l == want {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%q 应恰发 1 条（不双发），实际 %d 条:\n%s", want, n, strings.Join(lines, "\n"))
		}
	}
}

// TestDisplaySetVhostDerivedL3InterfaceRoundTrip（决策 #452）：vhost 派生名作 L3 接口
// （NAT-inside 文档化形态：VM vNIC 声明 + `l3-interface vh-<vm>-<nic> ip address …`）
// 在组合树里往返；该派生名只应出现在 `virtual-switches … l3-interface` 路径，不得泄漏
// 成顶层 `interfaces` 条目。
func TestDisplaySetVhostDerivedL3InterfaceRoundTrip(t *testing.T) {
	stmts := []string{
		"set virtual-switches vs-lan type l2",
		"set virtual-switches vs-empty type l2", // 先建成 l2，稍后改 l3（制造 vrfs[] 与 virtual_switches[] 顺序错位）
		"set virtual-machine-functions vnf-a vcpu count 2",
		"set virtual-machine-functions vnf-a interfaces nic0 type vhost-user virtual-switch vs-lan",
		"set virtual-machine-functions vnf-a interfaces nic1 type vhost-user virtual-switch vs-nat",
		"set virtual-switches vs-nat type l3",
		"set virtual-switches vs-nat l3-interface vh-vnf-a-nic1 ip address 192.168.200.1/24",
		"set virtual-switches vs-empty type l3",
	}
	runDisplaySetRoundTrip(t, stmts)

	var cfg model.Config
	for _, line := range stmts {
		if err := applyStatement(&cfg, splitFieldsQuoted(line)[1:]); err != nil {
			t.Fatalf("fixture %q 回放失败: %v", line, err)
		}
	}
	lines, err := renderSetStatements(toJSONTree(cfg), nil)
	if err != nil {
		t.Fatalf("反推失败: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "set virtual-switches vs-nat l3-interface vh-vnf-a-nic1 ip address 192.168.200.1/24") {
		t.Fatalf("vhost 派生名应出现在 l3-interface 路径:\n%s", joined)
	}
	if strings.Contains(joined, "set interfaces vh-vnf-a-nic1") {
		t.Fatalf("vhost 派生名不得泄漏成顶层 interfaces 条目:\n%s", joined)
	}
}

// TestDisplaySetL2SwitchWithVrfRoundTrip（决策 #452 边界）：VRF 的同名交换机显式为 l2
// （或 VRF 无同名交换机）时，回落发射器不得补发 `type l3`（会改型/凭空建交换机）——
// 该形态仍由 l3-interface 语句自身建出 VRF 并往返。
func TestDisplaySetL2SwitchWithVrfRoundTrip(t *testing.T) {
	runDisplaySetRoundTrip(t, []string{
		"set interfaces ens192 description x",
		"set virtual-switches vs1 type l2",
		"set virtual-switches vs1 l3-interface ens192 ip address 10.0.0.1/24",
	})
	runDisplaySetRoundTrip(t, []string{
		"set virtual-switches vs-nat l3-interface ens192 ip address 10.0.0.1/24",
	})
}

// TestDisplaySetEditLevelL3Switch：配置模式层级 show（`edit virtual-switches <n>`）下
// L3 交换机的 display set 仍就地发出 `type l3`（顶层回落不参与该路径），保证层级反推可还原。
func TestDisplaySetEditLevelL3Switch(t *testing.T) {
	var cfg model.Config
	for _, line := range []string{
		"set virtual-switches vs-l3 type l3",
		"set virtual-switches vs-l3 l3-interface ens192 ip address 10.0.0.1/24",
	} {
		if err := applyStatement(&cfg, splitFieldsQuoted(line)[1:]); err != nil {
			t.Fatalf("fixture %q 回放失败: %v", line, err)
		}
	}
	sub, err := navigateJSON(toJSONTree(cfg), []string{"virtual-switches", "vs-l3"})
	if err != nil {
		t.Fatalf("导航失败: %v", err)
	}
	lines, err := renderSetStatements(sub.(map[string]any), []string{"virtual-switches", "vs-l3"})
	if err != nil {
		t.Fatalf("层级反推失败: %v", err)
	}
	if len(lines) != 1 || lines[0] != "set virtual-switches vs-l3 type l3" {
		t.Fatalf("层级 show 应就地发出 type l3，实际: %v", lines)
	}
}

// TestDisplaySetEmpty：空配置反推为 0 条语句（调用方显示「配置为空」）。
func TestDisplaySetEmpty(t *testing.T) {
	lines, err := renderSetStatements(map[string]any{}, nil)
	if err != nil {
		t.Fatalf("空配置反推不应报错: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("空配置应反推出 0 条语句: %v", lines)
	}
}

// TestDisplaySetExplicitEnabledTrue（round81 真机实测回归）：`request interfaces enable`
// / REST 会把 enabled=true 显式落库（rev 56 曾让 display set 被自校验拦下）——
// enabled=true 与缺席语义相同（Enabled 未显式置 false 即启用）且语句语法只有 disable，
// 两侧同剥后往返必须通过、输出不得出现 enable 语句。
func TestDisplaySetExplicitEnabledTrue(t *testing.T) {
	var cfg model.Config
	for _, line := range []string{"set interfaces ens224 mtu 9000", "set interfaces ens224 disable"} {
		if err := applyStatement(&cfg, splitFieldsQuoted(line)[1:]); err != nil {
			t.Fatalf("fixture 回放失败: %v", err)
		}
	}
	// 模拟 request interfaces enable / REST PUT 写入的显式 true
	tr := true
	cfg.Interfaces[0].Enabled = &tr
	tree := toJSONTree(cfg)
	if _, ok := tree["interfaces"].([]any)[0].(map[string]any)["enabled"]; !ok {
		t.Fatalf("fixture 前置失效：enabled 应已显式落库")
	}
	w, err := generateSetStmts(tree, nil)
	if err != nil {
		t.Fatalf("显式 enabled=true 应被缺省态剥离后往返通过: %v", err)
	}
	for _, s := range w.out {
		if strings.Join(s.toks, " ") == "interfaces ens224 enable" {
			t.Fatalf("enable 无语句形态，不得出现在输出: %v", w.out)
		}
	}
	if len(w.out) == 0 {
		t.Fatalf("mtu/disable 语句应照常反推: %v", w.out)
	}
}

// TestDisplaySetRoundTripVMImageProfile（决策 #485，Task 7）：镜像启动画像的 display set
// 反推。模型把 cpu 存成**平铺** cpu_mode/cpu_model，而语句树是 cpu→mode/model 两层；
// 机械逆走查不到 "cpu" 键即整棵 cpu 子树跳过，须由家族发射器显式渲染 `cpu mode`/`cpu model`。
// 其余平铺叶子（machine/firmware/disk-interface/boot-order/accel/console/extra-args）
// 与树关键字同形（jsonKeyOf 横线转下划线），机械可达。
func TestDisplaySetRoundTripVMImageProfile(t *testing.T) {
	cfg := model.Config{VMImageProfiles: []model.VMImageProfile{{
		Image: "img1", Machine: "pc", CPUMode: "custom", CPUModel: "Skylake-Client",
		Firmware: "uefi", DiskIface: "ide", BootOrder: "cd", Accel: "tcg",
		Console: "telnet", ExtraArgs: []string{"-global x"},
	}}}
	tree := toJSONTree(cfg)
	stmts, err := renderSetStatements(tree, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	joined := strings.Join(stmts, "\n")
	for _, want := range []string{
		"set vm-image-profiles img1 machine pc",
		"set vm-image-profiles img1 cpu mode custom",
		"set vm-image-profiles img1 cpu model Skylake-Client",
		"set vm-image-profiles img1 firmware uefi",
		"set vm-image-profiles img1 disk-interface ide",
		"set vm-image-profiles img1 boot-order cd",
		"set vm-image-profiles img1 accel tcg",
		"set vm-image-profiles img1 console telnet",
		"set vm-image-profiles img1 extra-args",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("display set missing %q:\n%s", want, joined)
		}
	}
}

// TestDisplaySetRoundTripVMStartupOverride（决策 #485，Task 7）：VNF 实例的 startup
// 覆盖对象与镜像画像同族——cpu 平铺字段须渲染成 `startup cpu mode/model` 两层语句，
// 其余平铺叶子机械可达；整组语句回放后必须与原子树深等（含 cpu 平铺字段）。
func TestDisplaySetRoundTripVMStartupOverride(t *testing.T) {
	runDisplaySetRoundTrip(t, []string{
		"set virtual-machine-functions v1 vcpu count 2",
		"set virtual-machine-functions v1 startup machine q35",
		"set virtual-machine-functions v1 startup cpu mode custom",
		"set virtual-machine-functions v1 startup cpu model Skylake-Client",
		"set virtual-machine-functions v1 startup firmware uefi",
		"set virtual-machine-functions v1 startup extra-args \"-global x\"",
	})
}

// TestDisplaySetEmptyStartupShellPruned（决策 #485，Task 7）：删掉 startup 覆盖的最后一个
// 字段后，元素内嵌的 `startup` 不应留下 `{}` 空壳——空壳在 display set 回放时还原不出、
// 自校验如实报内部错误（Task 6 登记的已知小项，本任务按「回放可重放」纪律收口）。
func TestDisplaySetEmptyStartupShellPruned(t *testing.T) {
	var cfg model.Config
	for _, line := range []string{
		"set virtual-machine-functions v1 image img1",
		"set virtual-machine-functions v1 startup cpu mode custom",
	} {
		if err := applyStatement(&cfg, splitFieldsQuoted(line)[1:]); err != nil {
			t.Fatalf("fixture %q 回放失败: %v", line, err)
		}
	}
	if cfg.VirtualMachineFunctions[0].Startup == nil {
		t.Fatalf("前置失效：startup 覆盖应已落库")
	}
	if err := deleteStatement(&cfg, splitFieldsQuoted("delete virtual-machine-functions v1 startup cpu mode")[1:]); err != nil {
		t.Fatalf("delete startup cpu mode 失败: %v", err)
	}
	if cfg.VirtualMachineFunctions[0].Startup != nil {
		t.Fatalf("删光覆盖字段后 startup 空壳应被回收，实得 %+v", cfg.VirtualMachineFunctions[0].Startup)
	}
	if _, err := renderSetStatements(toJSONTree(cfg), nil); err != nil {
		t.Fatalf("空壳回收后 display set 应可反推: %v", err)
	}
}

// TestDisplaySetVMImageProfileLevelShow（决策 #485，Task 7）：配置模式层级 show
// （`edit vm-image-profiles <img>` / `edit virtual-machine-functions <n> startup`）下，
// 反推语句须带绝对路径前缀、可回放还原（cpu 别名要求属主元素已存在，故须就地补发创建语句）。
func TestDisplaySetVMImageProfileLevelShow(t *testing.T) {
	cfg := model.Config{VMImageProfiles: []model.VMImageProfile{{
		Image: "img1", Machine: "pc", CPUMode: "custom", CPUModel: "Skylake-Client",
	}}}
	sub, err := navigateJSON(toJSONTree(cfg), []string{"vm-image-profiles", "img1"})
	if err != nil {
		t.Fatalf("导航失败: %v", err)
	}
	lines, err := renderSetStatements(sub.(map[string]any), []string{"vm-image-profiles", "img1"})
	if err != nil {
		t.Fatalf("层级反推失败: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "set vm-image-profiles img1 cpu mode custom") {
		t.Fatalf("层级反推应含 cpu mode 语句:\n%s", joined)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "set vm-image-profiles img1") {
			t.Fatalf("层级反推的语句应带绝对路径前缀: %q", l)
		}
	}
}

// TestDisplaySetVMStartupLevelShow（决策 #485，Task 7）：`edit virtual-machine-functions
// <n> startup` 层级 show 的反推须先补发属主 VM 创建语句，否则 cpu 别名回放报「无匹配配置」。
func TestDisplaySetVMStartupLevelShow(t *testing.T) {
	var cfg model.Config
	for _, line := range []string{
		"set virtual-machine-functions v1 image img1",
		"set virtual-machine-functions v1 startup cpu mode custom",
		"set virtual-machine-functions v1 startup machine q35",
	} {
		if err := applyStatement(&cfg, splitFieldsQuoted(line)[1:]); err != nil {
			t.Fatalf("fixture %q 回放失败: %v", line, err)
		}
	}
	sub, err := navigateJSON(toJSONTree(cfg), []string{"virtual-machine-functions", "v1", "startup"})
	if err != nil {
		t.Fatalf("导航失败: %v", err)
	}
	lines, err := renderSetStatements(sub.(map[string]any), []string{"virtual-machine-functions", "v1", "startup"})
	if err != nil {
		t.Fatalf("层级反推失败: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "set virtual-machine-functions v1 startup cpu mode custom") {
		t.Fatalf("层级反推应含 startup cpu mode 语句:\n%s", joined)
	}
}

// TestDisplaySetVSwitchDescriptionNote（回归）：交换机带 description（Web 侧编辑的
// REST 写入形态，模型有字段、语句树与别名表都无关键字）时 display set 必须整体成功——
// 该字段以注释说明不入输出（同 acls 描述先例），不再照发语句被回放自校验打回
// 「未知语句: description」而把整条读命令拖垮（修复前的行为）。
func TestDisplaySetVSwitchDescriptionNote(t *testing.T) {
	var cfg model.Config
	if err := applyStatement(&cfg, splitFieldsQuoted("set virtual-switches vs-lan type l2")[1:]); err != nil {
		t.Fatalf("fixture 回放失败: %v", err)
	}
	cfg.VirtualSwitches[0].Description = "probe-desc"
	tree := toJSONTree(cfg)
	if _, ok := tree["virtual_switches"].([]any)[0].(map[string]any)["description"]; !ok {
		t.Fatalf("fixture 前置失效：description 应已落库")
	}
	lines, err := renderSetStatements(tree, nil)
	if err != nil {
		t.Fatalf("带 description 的配置应可反推（修复前整体失败）: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "probe-desc") || strings.Contains(joined, "description") {
		t.Fatalf("描述字段不得出现在 display set 输出: %v", lines)
	}
	if !strings.Contains(joined, "# virtual-switches vs-lan：描述字段无语句关键字，不入 display set") {
		t.Fatalf("描述字段应有「不入 display set」的注释说明: %v", lines)
	}
	if !strings.Contains(joined, "set virtual-switches vs-lan type l2") {
		t.Fatalf("其余语句应照常输出: %v", lines)
	}
	// 配置模式层级 show（`edit virtual-switches vs-lan`）同一行为：不再整体失败，
	// 注释说明照发（回放比较两侧按 stripNonEmittable 同剥）。
	sub, err := navigateJSON(tree, []string{"virtual-switches", "vs-lan"})
	if err != nil {
		t.Fatalf("导航失败: %v", err)
	}
	level, err := renderSetStatements(sub.(map[string]any), []string{"virtual-switches", "vs-lan"})
	if err != nil {
		t.Fatalf("层级反推应可成功（修复前整体失败）: %v", err)
	}
	for _, l := range level {
		if !strings.HasPrefix(l, "set virtual-switches vs-lan") && !strings.HasPrefix(l, "# ") {
			t.Fatalf("层级反推的语句应带绝对路径前缀: %q", l)
		}
	}
}

// TestDisplaySetNonEmittableFieldsNote（回归）：其余「发射器有意不输出」字段同口径——
// 出现时 display set 整体成功 + 注释说明，不因单个字段把整条读命令打回。
//   - VRF（L3 交换机数据落点）description：语句树无该叶子（仅 REST 可达）；
//   - gateway 空对象：多为 `delete … gateway ip` 的删除残留，无语句可还原；
//   - serial console 关闭态（`delete … serial console enable` 落库 false）：无 set 形态；
//   - SR-IOV vNIC 的 VF 编号：语句关键字与模型字段不一致，无可用 set 语句。
func TestDisplaySetNonEmittableFieldsNote(t *testing.T) {
	serialOff := false
	cases := []struct {
		name   string
		cfg    model.Config
		note   string
		absent string
	}{
		{"vrf-description", model.Config{
			VirtualSwitches: []model.VirtualSwitch{{Name: "vs-l3", Type: "l3"}},
			Vrfs:            []model.Vrf{{Name: "vs-l3", Description: "probe-desc"}},
		}, "# virtual-switches vs-l3：描述字段无语句关键字，不入 display set", "probe-desc"},
		{"gateway-empty", model.Config{
			VirtualSwitches: []model.VirtualSwitch{{Name: "vs1", Type: "l2", Gateway: &model.VSGateway{}}},
		}, "# virtual-switches vs1：gateway 为空对象（多为删除残留），不入 display set", ""},
		{"serial-console-off", model.Config{
			VirtualMachineFunctions: []model.VMFunction{{Name: "v1", Image: "img1", SerialConsole: &serialOff}},
		}, "# virtual-machine-functions v1：serial console 关闭态没有 set 语句形态，不入 display set", ""},
		{"sriov-vf-id", model.Config{
			VirtualMachineFunctions: []model.VMFunction{{Name: "v1", Image: "img1",
				Interfaces: []model.VnfInterface{{Name: "nic0", Type: "sriov-vf", Sriov: &model.SriovBind{PhysicalInterface: "ens192", VFID: 3}}}}},
		}, "# virtual-machine-functions 接口 nic0：SR-IOV VF 编号无可用 set 语句（语句关键字与模型字段不一致），不入 display set", ""},
	}
	for _, tc := range cases {
		lines, err := renderSetStatements(toJSONTree(tc.cfg), nil)
		if err != nil {
			t.Fatalf("%s: display set 应整体成功: %v", tc.name, err)
		}
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, tc.note) {
			t.Fatalf("%s: 应含注释 %q:\n%s", tc.name, tc.note, joined)
		}
		if tc.absent != "" && strings.Contains(joined, tc.absent) {
			t.Fatalf("%s: %q 不得出现在输出:\n%s", tc.name, tc.absent, joined)
		}
	}
}

// TestDisplaySetEmptyContainersPruned（R38-2 回归）：配置里存在**空容器**（`{}`/`[]`）+ null，
// 如 Web 侧清空字段留下的 `system.kernel = {}`（真机 rev 269 复现：display set **整体失败**，
// 删掉空壳立即恢复）——空容器不是有意义的数据（与「键不存在」对回放等价），发射器不发
// 语句、比较端两侧同剥（stripEmptyContainers），整条读命令必须成功，且空容器之外的语句
// 照常输出。修复前：空壳只在一侧（committed 树）、回放树里缺席，深比较不等即报内部错误。
func TestDisplaySetEmptyContainersPruned(t *testing.T) {
	cases := []struct {
		name    string
		cfg     model.Config
		absent  string // 输出中不得出现的空容器痕迹
		present string // 空容器之外的语句照常输出（可选）
	}{
		{"system-kernel（真机复现形态）", model.Config{
			System: &model.SystemConfig{Kernel: &model.KernelConfig{}, Hostname: "nfvis-lab"}},
			"kernel", "set system hostname nfvis-lab"},
		{"vpp-dpdk-dev（真机现场形态：dev 为值结构体恒现）", model.Config{
			Vpp: &model.VppConfig{Memory: &model.VppMemory{HugepagePreference: "2M"}, DPDK: &model.VppDPDK{}}},
			"dpdk", "set vpp memory hugepage-preference 2M"},
		{"metrics-history（两层空壳）", model.Config{
			System: &model.SystemConfig{Metrics: &model.MetricsConfig{History: &model.MetricsHistoryConfig{}}, Hostname: "probe"}},
			"metrics", "set system hostname probe"},
		{"management（空对象）", model.Config{
			System: &model.SystemConfig{Management: &model.MgmtConfig{}, Hostname: "probe"}},
			"management", "set system hostname probe"},
		{"storm-control（空对象）", model.Config{
			Interfaces: []model.InterfaceConfig{{Name: "ens192", Description: "sd", StormControl: &model.StormControl{}}}},
			"storm-control", "set interfaces ens192 description sd"},
		{"vm-startup（空对象）", model.Config{
			VirtualMachineFunctions: []model.VMFunction{{Name: "v1", Image: "img1", Startup: &model.VMStartupOverride{}}}},
			"startup", "set virtual-machine-functions v1 image img1"},
		{"lldp（空对象）", model.Config{Protocols: &model.ProtocolsConfig{LLDP: &model.LldpConfig{}}},
			"lldp", ""},
		{"nat（空对象）", model.Config{Nat: &model.NatConfig{}}, "nat", ""},
		{"acl-rules（空 list：回放侧为 null，口径等价）", model.Config{
			Acls: []model.Acl{{Name: "a1", Rules: []model.AclRule{}}}},
			"rule", "set acls a1"},
	}
	for _, tc := range cases {
		tree := toJSONTree(tc.cfg)
		lines, err := renderSetStatements(tree, nil)
		if err != nil {
			t.Fatalf("%s: 带空容器的配置 display set 必须整体成功（修复前整体失败）: %v", tc.name, err)
		}
		joined := strings.Join(lines, "\n")
		if tc.absent != "" && strings.Contains(joined, tc.absent) {
			t.Fatalf("%s: 空容器 %q 不得出现在输出:\n%s", tc.name, tc.absent, joined)
		}
		if tc.present != "" && !strings.Contains(joined, tc.present) {
			t.Fatalf("%s: 空容器之外的语句应照常输出（缺 %q）:\n%s", tc.name, tc.present, joined)
		}
	}
	// 主复现形态的前置确认：空壳确实落在树里（否则上表第一条测不到点）。
	kernelTree := toJSONTree(model.Config{System: &model.SystemConfig{Kernel: &model.KernelConfig{}}})
	if _, ok := kernelTree["system"].(map[string]any)["kernel"]; !ok {
		t.Fatalf("fixture 前置失效：system.kernel 空壳应已落树")
	}
}

// TestDisplaySetEmptyLevelShow（R38-2 回归）：配置模式层级 show 落在**空容器层级**时同样整体
// 成功——`edit system kernel` / `edit virtual-switches <n> gateway` 的空壳在回放里该层不存在，
// 但空 ≡ 缺失、本层没有数据可核对（修复前报「回放结果缺少层级」内部错误）。
// 负控：树在该层级**非空**（只是语句缺口）时，缺层的内部错误必须如实保留。
func TestDisplaySetEmptyLevelShow(t *testing.T) {
	lines, err := renderSetStatements(map[string]any{}, []string{"system", "kernel"})
	if err != nil {
		t.Fatalf("空容器层级（system kernel）应整体成功（修复前报缺少层级）: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("空容器层级应无语句输出: %v", lines)
	}
	// 空网关层级：edit 进入只补发交换机裸声明（无数据语句），不再整体失败。
	lines, err = renderSetStatements(map[string]any{}, []string{"virtual-switches", "vs1", "gateway"})
	if err != nil {
		t.Fatalf("空容器层级（virtual-switches vs1 gateway）应整体成功（修复前报缺少层级）: %v", err)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "set virtual-switches vs1") {
			t.Fatalf("空网关层级只应补发交换机裸声明: %q", l)
		}
	}
	// 负控：层级树非空但语句缺口 ⇒ 缺层仍旧如实报错（容忍只吃「空」）。
	if err := verifySetStatements(map[string]any{"hostname": "probe"}, []string{"system", "kernel"}, nil); err == nil {
		t.Fatalf("非空层级缺层不得被空容器容忍误吞（应报缺层内部错误）")
	}
}

// TestDisplaySetNumaEmptyCoresPruned（R38-2 同族回归）：空/缺核列表的 numa 节点没有语句可
// 还原——`numa node <n>` 不带 cores 回放即失败（core-list 是语句必需取值，解析报「核编号 ""
// 不合法」），修复前照发该语句把**整条 display set** 拖垮。修复后只注释说明、不发语句；
// 回放比较两侧同剥该元素（nonEmittableElem 与发射器同判据）。同数组里的正常节点照常发射。
func TestDisplaySetNumaEmptyCoresPruned(t *testing.T) {
	cfg := model.Config{ResourcePools: &model.ResourcePool{CPU: &model.CPUSetup{
		IsolatedCores: []int{4, 5},
		Numa: []model.NumaNode{
			{Node: 0, Cores: []int{}}, // 空 list（REST 形态）
			{Node: 1, Cores: nil},     // null（字段无 omitempty 的缺值形态）
			{Node: 2, Cores: []int{4, 5}},
		},
	}}}
	lines, err := renderSetStatements(toJSONTree(cfg), nil)
	if err != nil {
		t.Fatalf("空核列表 numa 节点应被跳过而不是整体失败（修复前回放失败）: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "set resource-pools cpu numa node 2 cores 4,5") {
		t.Fatalf("正常 numa 节点应照常发射:\n%s", joined)
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "set ") && (strings.Contains(l, "numa node 0") || strings.Contains(l, "numa node 1")) {
			t.Fatalf("空核列表节点不得发语句（回放即失败）: %q", l)
		}
	}
	for _, node := range []string{"0", "1"} {
		note := "# resource-pools cpu numa node " + node + "：核列表为空，无语句可还原（不入 display set）"
		if !strings.Contains(joined, note) {
			t.Fatalf("空核列表节点应有注释说明 %q:\n%s", note, joined)
		}
	}
	// 输出的每条语句必须可回放（修复前 `numa node 0` 语句回放即报「核编号 "" 不合法」）。
	var rebuilt model.Config
	for _, l := range lines {
		if !strings.HasPrefix(l, "set ") {
			continue
		}
		if err := applyStatement(&rebuilt, splitFieldsQuoted(l)[1:]); err != nil {
			t.Fatalf("输出语句 %q 回放失败: %v", l, err)
		}
	}
}
