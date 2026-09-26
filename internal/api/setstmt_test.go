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
	// qos_policies 落点（根级回落发射器）
	{"set qos policies p1 cir 100000000 cbs 2000"},
	{"set virtual-switches vs1 type l2", "set virtual-switches vs1 vlan access 100"},
	{"set virtual-switches vs1 type l2", "set virtual-switches vs1 gateway ip 192.168.100.1/24"},
	{"set virtual-switches vs1 type l2", "set virtual-switches vs1 ports 1 interface ens192 trunk vlans 100,200"},
	{"set virtual-switches vs1 type l2", "set virtual-switches vs1 ports 1 vnf vnf-a interface eth0"},
	{"set virtual-switches vs1 type l2", "set virtual-switches vs1 ports 1 container ct1 interface memif0"},
	{"set virtual-switches vs1 type l2", "set virtual-switches vs1 ports 1 interface ens192", "set virtual-switches vs1 ports 2 interface ens224", "set virtual-switches vs1 cross-connect 1 2"},
	// virtual-machine-functions（vcpu/memory/interfaces/serial）
	{"set virtual-machine-functions vnf-a vcpu count 2", "set virtual-machine-functions vnf-a vcpu count 2 pin true"},
	{"set virtual-machine-functions vnf-a vcpu count 2", "set virtual-machine-functions vnf-a memory size-mb 512", "set virtual-machine-functions vnf-a memory numa node 0"},
	{"set virtual-machine-functions vnf-a vcpu count 2", "set virtual-machine-functions vnf-a interfaces eth0 virtual-switch vs1"},
	{"set virtual-machine-functions vnf-a vcpu count 2", "set virtual-machine-functions vnf-a serial console enable"},
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
