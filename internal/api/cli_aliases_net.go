package api

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// 网络类语句别名表（cli_aliases_net.go）。
//
// 背景（见 docs/reviews/2026-09-13.md 第三轮）：schema 命令树声明的语句里，
// 凡是「关键字后跟数组元素 / 嵌套对象」的形态，其 JSON 键名与模型字段并不一一对应
// （如 CLI 的 `gateway ip` 要写进 VSGateway.Addresses[]，`l3-interface … ip address`
// 要写进 Vrf.L3Interfaces[].Addresses[]）。通用树遍历只会按关键字名拼 JSON 键，
// 这些语句因此写不到模型：元素已存在时报「尚未映射到模型」，元素不存在时更糟——
// 变更落在模型不认识的键上被 encoding/json 静默丢弃，只靠元素新建这件事骗过
// commitTree 的 Diff 兜底而误报成功。
//
// 本表把这些语句显式映射到模型字段；每条规则同时处理 set 与 delete。

var statementAliasesNet = []aliasRule{
	// virtual-switches <n> type <l2|l3>：l3 需同步建立同名 Vrf 条目
	// （模型校验要求 l3 交换机必须有同名 VRF 承载 L3 配置，附录 B）
	{pattern: []string{"virtual-switches", "*", "type", "*"},
		apply: aliasVSType},

	// virtual-switches <n> gateway ip <prefix>：BVI 网关地址（可多条）
	{pattern: []string{"virtual-switches", "*", "gateway", "ip", "*"},
		apply: aliasVSGatewayIP},
	{pattern: []string{"virtual-switches", "*", "gateway", "ip"},
		apply: func(tree map[string]any, t []string, _ bool) error {
			vs, err := elemByID(tree, "virtual_switches", t[1])
			if err != nil {
				return err
			}
			if gw, ok := vs["gateway"].(map[string]any); ok {
				delete(gw, "addresses")
			}
			return nil
		}},
	{pattern: []string{"virtual-switches", "*", "gateway"},
		apply: func(tree map[string]any, t []string, _ bool) error {
			vs, err := elemByID(tree, "virtual_switches", t[1])
			if err != nil {
				return err
			}
			delete(vs, "gateway")
			return nil
		}},

	// virtual-switches <n> dhcp-relay server <ip>（决策 #335：DHCP 中继，模型单值字符串）
	{pattern: []string{"virtual-switches", "*", "dhcp-relay", "server", "*"},
		apply: aliasVSDhcpRelay},
	{pattern: []string{"virtual-switches", "*", "dhcp-relay"},
		apply: func(tree map[string]any, t []string, _ bool) error {
			vs, err := elemByID(tree, "virtual_switches", t[1])
			if err != nil {
				return err
			}
			delete(vs, "dhcp_relay_server")
			return nil
		}},

	// virtual-switches <n> dhcp-server pool <start> <end>（决策 #359：DHCP 服务器，模型 5 个平铺键）。
	// pool 两键是启用要件（必须同时给，与模型校验同口径——缺一个即明确报错，不落半套配置）；
	// **delete 的 pool 形态＝停用**：清全部 5 键并回收运行态（服务器不可无池运行），与裸 delete 等价。
	{pattern: []string{"virtual-switches", "*", "dhcp-server", "pool", "*", "*"},
		apply: aliasVSDhcpServerPool},
	{pattern: []string{"virtual-switches", "*", "dhcp-server", "pool", "*"},
		apply: aliasVSDhcpServerPoolIncomplete},
	{pattern: []string{"virtual-switches", "*", "dhcp-server", "pool"},
		apply: aliasVSDhcpServerPoolBare},
	// 可选叶子：lease-time / dns / domain-name（带值形态与无值形态各一条；delete 逐叶子）。
	{pattern: []string{"virtual-switches", "*", "dhcp-server", "lease-time", "*"},
		apply: aliasVSDhcpServerLeaseTime},
	{pattern: []string{"virtual-switches", "*", "dhcp-server", "lease-time"},
		apply: aliasVSDhcpServerLeafNoValue("lease-time", "dhcp_server_lease_time_seconds")},
	{pattern: []string{"virtual-switches", "*", "dhcp-server", "dns", "*"},
		apply: aliasVSDhcpServerLeaf("dns", "dhcp_server_dns")},
	{pattern: []string{"virtual-switches", "*", "dhcp-server", "dns"},
		apply: aliasVSDhcpServerLeafNoValue("dns", "dhcp_server_dns")},
	{pattern: []string{"virtual-switches", "*", "dhcp-server", "domain-name", "*"},
		apply: aliasVSDhcpServerLeaf("domain-name", "dhcp_server_domain_name")},
	{pattern: []string{"virtual-switches", "*", "dhcp-server", "domain-name"},
		apply: aliasVSDhcpServerLeafNoValue("domain-name", "dhcp_server_domain_name")},
	// 裸 dhcp-server：delete ＝整段停用（清 5 键）；set 缺取值明确报错。
	{pattern: []string{"virtual-switches", "*", "dhcp-server"},
		apply: aliasVSDhcpServerBare},

	// virtual-switches <n> dns proxy server <ip> [secondary <ip>]（决策 #345：按域上游，模型数组）。
	// 与 system 级同名语句对偶：system 落 vpp.dns_proxy_servers（全局），本语句落该交换机元素的
	// dns_proxy_servers（本域优先、回落全局）。更具体的形态须排在带通配的形态之前。
	{pattern: []string{"virtual-switches", "*", "dns", "proxy", "server", "*", "secondary", "*"},
		apply: aliasVSDNSProxyPair},
	{pattern: []string{"virtual-switches", "*", "dns", "proxy", "server", "secondary", "*"},
		apply: aliasVSDNSProxyOne},
	{pattern: []string{"virtual-switches", "*", "dns", "proxy", "server", "*"},
		apply: aliasVSDNSProxyOne},
	{pattern: []string{"virtual-switches", "*", "dns", "proxy", "server"},
		apply: aliasVSDNSProxyClear},

	// virtual-switches <n> learn-limit <n>（决策 #337：MAC 学习条数上限，模型单值整数）。
	// 走别名而非通用树：delete 无值形态在「键不存在」时须为幂等空操作（套件里 set 与 delete 各在
	// 独立会话、delete 时 committed 里本就没有该键），通用树遍历会回「无匹配配置」而中止脚本。
	{pattern: []string{"virtual-switches", "*", "learn-limit", "*"},
		apply: aliasVSLearnLimit},
	{pattern: []string{"virtual-switches", "*", "learn-limit"},
		apply: func(tree map[string]any, t []string, _ bool) error {
			vs, err := elemByID(tree, "virtual_switches", t[1])
			if err != nil {
				return err
			}
			delete(vs, "learn_limit")
			return nil
		}},

	// virtual-switches <n> cross-connect <port-a> <port-b>（FR-NET-012，两端口直通）
	// 模型只有 `cross_connect bool`（OpenAPI 亦为 boolean），两个端口的"身份"由该交换机的
	// ports 列表承担（applier 取前两个端口做 sw_interface_set_l2_xconnect）。
	// 故本语句：置位标志 + **校验**被引用端口已声明且恰为两个——避免 <2 个端口时 applier
	// 静默什么都不做（决策 #79）。
	{pattern: []string{"virtual-switches", "*", "cross-connect", "*", "*"},
		apply: aliasVSCrossConnect},
	{pattern: []string{"virtual-switches", "*", "cross-connect"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return aliasVSCrossConnect(tree, append(append([]string{}, t...), "", ""), isSet)
		}},

	// virtual-switches <n> l3-interface <if> ip address <prefix>（L3，落在同名 Vrf）
	{pattern: []string{"virtual-switches", "*", "l3-interface", "*", "ip", "address", "*"},
		apply: aliasL3InterfaceAddr},
	{pattern: []string{"virtual-switches", "*", "l3-interface", "*", "acl-in", "*"},
		apply: aliasL3InterfaceAclIn},
	{pattern: []string{"virtual-switches", "*", "l3-interface", "*"},
		apply: aliasL3InterfaceDel},

	// virtual-switches <n> static-routes <prefix> next-hop <ip> [distance <n>]（L3）
	{pattern: []string{"virtual-switches", "*", "static-routes", "*", "next-hop", "*"},
		apply: aliasStaticRoute},
	{pattern: []string{"virtual-switches", "*", "static-routes", "*", "distance", "*"},
		apply: aliasStaticRouteDistance},
	{pattern: []string{"virtual-switches", "*", "static-routes", "*", "next-hop", "*", "distance", "*"},
		apply: aliasStaticRouteBoth},
	{pattern: []string{"virtual-switches", "*", "static-routes", "*", "distance", "*", "next-hop", "*"},
		apply: aliasStaticRouteBothRev},
	{pattern: []string{"virtual-switches", "*", "static-routes", "*"},
		apply: aliasStaticRouteDel},

	// acls <name> rule <seq> <k> <v> …（不定长键值对）
	{pattern: []string{"acls", "*", "rule", "**"},
		apply: aliasAclRule},

	// nat source-pool <n> address-range <a> to <b>；nat rules <seq> …；nat static <in> to <out>
	{pattern: []string{"nat", "source-pool", "*", "address-range", "*", "to", "*"},
		apply: aliasNatPool},
	{pattern: []string{"nat", "source-pool", "*"},
		apply: aliasNatPoolDel},
	{pattern: []string{"nat", "rules", "**"},
		apply: aliasNatRule},
	{pattern: []string{"nat", "static", "*", "to", "*"},
		apply: aliasNatStatic},

	// port-mirroring <n> source interface <if> [direction <d>]
	// port-mirroring <n> source vnf <vm> interface <vnic> [direction <d>]
	// port-mirroring <n> analyzer interface <if>
	{pattern: []string{"port-mirroring", "*", "source", "**"},
		apply: aliasPMSource},
	{pattern: []string{"port-mirroring", "*", "analyzer", "interface", "*"},
		apply: aliasPMAnalyzer},

	// qos policies <n> cir <bps> cbs <bytes>（不定长，可只给其一）
	{pattern: []string{"qos", "policies", "**"},
		apply: aliasQosPolicy},

	// interfaces <if> ingress-policy <name> | egress-policy <name>（决策 #331：出向为同族语句）
	{pattern: []string{"interfaces", "*", "ingress-policy", "*"},
		apply: ifacePolicyAlias("ingress_policy")},
	{pattern: []string{"interfaces", "*", "ingress-policy"},
		apply: ifacePolicyClearAlias("ingress_policy")},
	{pattern: []string{"interfaces", "*", "egress-policy", "*"},
		apply: ifacePolicyAlias("egress_policy")},
	{pattern: []string{"interfaces", "*", "egress-policy"},
		apply: ifacePolicyClearAlias("egress_policy")},

	// interfaces <if> storm-control broadcast|multicast <kbps>（决策 #385：入向风暴抑制）。
	// 模型是单对象（storm_control.{broadcast_kbps,multicast_kbps}），逐叶子 set/delete；
	// 裸 delete 清两类；**接口须已声明**（elemByID 查不到即报错并给照做指引——风暴抑制只
	// 作用于已声明物理口，不允许由本语句顺带建出一个只有 storm_control 的口声明）。
	{pattern: []string{"interfaces", "*", "storm-control", "broadcast", "*"},
		apply: ifaceStormAlias("broadcast_kbps")},
	{pattern: []string{"interfaces", "*", "storm-control", "broadcast"},
		apply: ifaceStormClearAlias("broadcast_kbps")},
	{pattern: []string{"interfaces", "*", "storm-control", "multicast", "*"},
		apply: ifaceStormAlias("multicast_kbps")},
	{pattern: []string{"interfaces", "*", "storm-control", "multicast"},
		apply: ifaceStormClearAlias("multicast_kbps")},
	{pattern: []string{"interfaces", "*", "storm-control"},
		apply: ifaceStormBareAlias},

	// interfaces <if> port-security mac <mac>（决策 #389：端口安全白名单）。
	// set＝追加（模型 []string 数组）；delete mac <mac>＝按值删一条（大小写不敏感——
	// 解码层已归一小写，按值删仍做归一比较，容忍操作者用大写指认同一条）；
	// 裸 delete port-security＝清空整段＝停用。**接口须已声明**（elemByID 查不到即报错
	// 并给照做指引——白名单只作用于已声明物理口，不允许由本语句顺带建出口声明）。
	// MAC 合法性在语句层即时校验（给了可照做的报错）；重复/超限/前置（L2 成员等）
	// 由提交校验统一拒绝（提交期才看得到整份配置的成员关系）。
	{pattern: []string{"interfaces", "*", "port-security", "mac", "*"},
		apply: ifacePortSecMacAlias},
	{pattern: []string{"interfaces", "*", "port-security", "mac"},
		apply: ifacePortSecMacClearAlias},
	{pattern: []string{"interfaces", "*", "port-security"},
		apply: ifacePortSecBareAlias},

	// protocols lldp enable <bool> | advertisement-interval <n> | interface <if> enable <bool>
	{pattern: []string{"protocols", "lldp", "enable", "*"},
		apply: aliasLldpEnable},
	{pattern: []string{"protocols", "lldp", "enable"},
		apply: func(tree map[string]any, _ []string, _ bool) error {
			if l := lldpObj(tree, false); l != nil {
				delete(l, "enabled")
			}
			return nil
		}},
	{pattern: []string{"protocols", "lldp", "advertisement-interval", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			l := lldpObj(tree, isSet)
			if l == nil {
				return errNoLLDP
			}
			if !isSet {
				delete(l, "advertisement_interval")
				return nil
			}
			n, err := numField(t[3])
			if err != nil {
				return err
			}
			l["advertisement_interval"] = n
			return nil
		}},
	{pattern: []string{"protocols", "lldp", "interface", "*", "enable", "*"},
		apply: aliasLldpInterface},
}

var errNoLLDP = errString("无匹配配置: lldp")

type errString string

func (e errString) Error() string { return string(e) }

// ---------- 通用小工具 ----------

// ensureElem 取具名数组元素，不存在则创建（仅 set 路径调用）。
func ensureElem(tree map[string]any, arrKey, ident string) map[string]any {
	arr, _ := tree[arrKey].([]any)
	em, _ := selectElement(arr, "name", ident)
	if em == nil {
		em = map[string]any{"name": ident}
		tree[arrKey] = append(arr, em)
	}
	return em
}

// elemByField 在 arrKey 数组内按自定义身份字段取元素，不存在则创建。
func elemByField(tree map[string]any, arrKey, field, ident string) map[string]any {
	arr, _ := tree[arrKey].([]any)
	em, _ := selectElement(arr, field, ident)
	if em == nil {
		em = map[string]any{field: typedIdentity(field, ident)}
		tree[arrKey] = append(arr, em)
	}
	return em
}

// typedIdentity 身份值按字段类型落 JSON：seq/node 等数值字段转 number，其余保持字符串。
func typedIdentity(field, ident string) any {
	switch field {
	case "seq", "node":
		if n, err := numField(ident); err == nil {
			return n
		}
	}
	return ident
}

// ensureObj 取（或建）map 子对象。
func ensureObj(m map[string]any, key string) map[string]any {
	if sub, ok := m[key].(map[string]any); ok {
		return sub
	}
	sub := map[string]any{}
	m[key] = sub
	return sub
}

// objOrNil 取 map 子对象，不存在返回 nil。
func objOrNil(m map[string]any, key string) map[string]any {
	sub, _ := m[key].(map[string]any)
	return sub
}

// appendUniqueStr 字符串数组去重追加。
func appendUniqueStr(m map[string]any, key, val string) {
	arr, _ := m[key].([]any)
	for _, e := range arr {
		if s, ok := e.(string); ok && s == val {
			return
		}
	}
	m[key] = append(arr, val)
}

// removeStr 从字符串数组移除（removeAll=true 时清空该键）。
func removeStr(m map[string]any, key, val string) {
	arr, _ := m[key].([]any)
	out := make([]any, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok && s == val {
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		delete(m, key)
		return
	}
	m[key] = out
}

// boolField 解析 true/false。
func boolField(s string) (bool, error) {
	switch s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, errString("取值须为 true|false: " + s)
}

// l3VrfOf 取（或建）L3 交换机的同名 Vrf 条目（与 resources_w6.go 的只读 vrfOf 区分）。
func l3VrfOf(tree map[string]any, vsName string, create bool) (map[string]any, error) {
	arr, _ := tree["vrfs"].([]any)
	em, _ := selectElement(arr, "name", vsName)
	if em == nil {
		if !create {
			return nil, errString("无匹配配置: " + vsName)
		}
		em = map[string]any{"name": vsName}
		tree["vrfs"] = append(arr, em)
	}
	return em, nil
}

// removeVrf 删除同名 Vrf 条目。
func removeVrf(tree map[string]any, vsName string) {
	arr, _ := tree["vrfs"].([]any)
	out := make([]any, 0, len(arr))
	for _, e := range arr {
		if em, ok := e.(map[string]any); ok {
			if n, _ := em["name"].(string); n == vsName {
				continue
			}
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		delete(tree, "vrfs")
		return
	}
	tree["vrfs"] = out
}

// vswitchNamesOf 取 JSON 树里全部虚拟交换机的名字（身份字段与建元素口径一致）。
func vswitchNamesOf(tree map[string]any) map[string]bool {
	arr, _ := tree["virtual_switches"].([]any)
	out := make(map[string]bool, len(arr))
	ident := identityFields["virtual_switches"]
	if ident == "" {
		ident = "name"
	}
	for _, e := range arr {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if n, _ := em[ident].(string); n != "" {
			out[n] = true
		}
	}
	return out
}

// pruneVSwitchVrf 本次 delete 移除掉的虚拟交换机，其同名 Vrf 条目一并删除。
//
// L3 交换机与同名 VRF 条目互为映射（附录 B）：REST `DELETE /virtual-switches/{name}`
// 即一并删除（resources.go handleDeleteVSwitch），CLI 侧此前只有 `type` 一类别名规则
// 会调 removeVrf，**整节点**删除留下的空 VRF 条目操作者看不见（display set 反推不出）、
// 数据面 VRF 表跨重启滞留，且没有任何命令能清掉（round84 走查 R84-2）。
// before 是删除前的交换机名单（名字集合），after 是删除后的 JSON 树。
func pruneVSwitchVrf(before map[string]bool, after map[string]any) {
	kept := vswitchNamesOf(after)
	for name := range before {
		if !kept[name] {
			removeVrf(after, name)
		}
	}
}

// lldpObj 取 protocols.lldp 对象；create=false 且不存在时返回 nil。
func lldpObj(tree map[string]any, create bool) map[string]any {
	p := objOrNil(tree, "protocols")
	if p == nil {
		if !create {
			return nil
		}
		p = ensureObj(tree, "protocols")
	}
	l := objOrNil(p, "lldp")
	if l == nil {
		if !create {
			return nil
		}
		l = ensureObj(p, "lldp")
	}
	return l
}

// ---------- 各语句实现 ----------

// aliasVSType：virtual-switches <n> type <l2|l3>，并同步 vrfs 条目。
func aliasVSType(tree map[string]any, t []string, isSet bool) error {
	if !isSet {
		vs, err := elemByID(tree, "virtual_switches", t[1])
		if err != nil {
			return err
		}
		delete(vs, "type")
		removeVrf(tree, t[1])
		return nil
	}
	vs := ensureElem(tree, "virtual_switches", t[1])
	vs["type"] = t[3]
	if t[3] == "l3" {
		if _, err := l3VrfOf(tree, t[1], true); err != nil {
			return err
		}
	} else {
		removeVrf(tree, t[1])
	}
	return nil
}

// aliasVSDhcpRelay：virtual-switches <n> dhcp-relay server <ip>（决策 #335）。
// 模型是单值字符串（dhcp_relay_server）；delete 由无值形态的注册键处理（整键删除）。
func aliasVSDhcpRelay(tree map[string]any, t []string, isSet bool) error {
	vs, err := elemByID(tree, "virtual_switches", t[1])
	if err != nil {
		return err
	}
	if !isSet {
		delete(vs, "dhcp_relay_server")
		return nil
	}
	vs["dhcp_relay_server"] = t[4]
	return nil
}

// ---------- DHCP 服务器（决策 #359） ----------
//
// 模型是 VirtualSwitch 上的 5 个平铺键（dhcp_server_pool_start/pool_end/lease_time_seconds/
// dns/domain_name），与 dhcp-relay 同族；pool 两键是启用要件。删除语义按契约：
// **delete pool（与裸 delete 等价）＝整段停用**——把 5 个键一并删掉（服务器不可无池运行）。

// vsDhcpServerClear 清掉 DHCP 服务器的全部 5 个键（整段停用）。
// 平铺键不留空壳（不存在 `dhcp_server` 嵌套对象，与 #356 的教训同口径）。
func vsDhcpServerClear(vs map[string]any) {
	delete(vs, "dhcp_server_pool_start")
	delete(vs, "dhcp_server_pool_end")
	delete(vs, "dhcp_server_lease_time_seconds")
	delete(vs, "dhcp_server_dns")
	delete(vs, "dhcp_server_domain_name")
}

// aliasVSDhcpServerPool：virtual-switches <n> dhcp-server pool <start> <end>（决策 #359）。
// set：写入两个池键（同时给，缺一即报错）；delete：整段停用（清 5 键，幂等）。
func aliasVSDhcpServerPool(tree map[string]any, t []string, isSet bool) error {
	vs, err := elemByID(tree, "virtual_switches", t[1])
	if err != nil {
		return err
	}
	if !isSet {
		vsDhcpServerClear(vs)
		return nil
	}
	if t[4] == "" || t[5] == "" {
		return errString("配置不完整，缺少租约池的起始/结束地址: set virtual-switches " + t[1] +
			" dhcp-server pool <start> <end>")
	}
	vs["dhcp_server_pool_start"] = t[4]
	vs["dhcp_server_pool_end"] = t[5]
	return nil
}

// aliasVSDhcpServerPoolIncomplete：只给了一个池端点（）——set 明确报错（不落半套配置），
// delete 视同停用（与 delete pool 等价）。
func aliasVSDhcpServerPoolIncomplete(tree map[string]any, t []string, isSet bool) error {
	if isSet {
		return errString("配置不完整，缺少租约池的起始/结束地址: set virtual-switches " + t[1] +
			" dhcp-server pool <start> <end>")
	}
	return aliasVSDhcpServerPool(tree, append(append([]string{}, t...), ""), isSet)
}

// aliasVSDhcpServerPoolBare：`… dhcp-server pool`（无取值）。delete ＝停用；set 缺取值报错。
func aliasVSDhcpServerPoolBare(tree map[string]any, t []string, isSet bool) error {
	if isSet {
		return errString("配置不完整，缺少租约池的起始/结束地址: set virtual-switches " + t[1] +
			" dhcp-server pool <start> <end>")
	}
	return aliasVSDhcpServerPool(tree, append(append([]string{}, t...), "", ""), isSet)
}

// aliasVSDhcpServerBare：裸 `… dhcp-server`（无取值）。delete ＝整段停用；set 缺取值报错。
func aliasVSDhcpServerBare(tree map[string]any, t []string, isSet bool) error {
	if isSet {
		return errString("配置不完整，缺少取值: set virtual-switches " + t[1] +
			" dhcp-server pool <start> <end> [lease-time <seconds>] [dns <ip>] [domain-name <name>]")
	}
	return aliasVSDhcpServerPool(tree, append(append([]string{}, t...), "", ""), isSet)
}

// aliasVSDhcpServerLeaseTime：`… dhcp-server lease-time <seconds>`（模型单值整数，秒）。
// set 取值必须为十进制正整数（与模型口径同口径，避免写出模型不认的值）；delete 清该键。
func aliasVSDhcpServerLeaseTime(tree map[string]any, t []string, isSet bool) error {
	vs, err := elemByID(tree, "virtual_switches", t[1])
	if err != nil {
		return err
	}
	if !isSet {
		delete(vs, "dhcp_server_lease_time_seconds")
		return nil
	}
	n, err := strconv.Atoi(t[4])
	if err != nil || n < 1 {
		return errString("租约时长须为正整数（秒）: " + t[4])
	}
	vs["dhcp_server_lease_time_seconds"] = float64(n)
	return nil
}

// aliasVSDhcpServerLeaf：其它可选叶子（dns/domain-name）的 set/delete。
// set 写入取值；delete 清该键（幂等）。
func aliasVSDhcpServerLeaf(stmt, key string) func(map[string]any, []string, bool) error {
	return func(tree map[string]any, t []string, isSet bool) error {
		vs, err := elemByID(tree, "virtual_switches", t[1])
		if err != nil {
			return err
		}
		if !isSet {
			delete(vs, key)
			return nil
		}
		if t[4] == "" {
			return errString("缺少取值: set virtual-switches " + t[1] + " dhcp-server " + stmt + " <value>")
		}
		vs[key] = t[4]
		return nil
	}
}

// aliasVSDhcpServerLeafNoValue：可选叶子的无值形态（delete 逐叶子清键；set 明确报错）。
func aliasVSDhcpServerLeafNoValue(stmt, key string) func(map[string]any, []string, bool) error {
	f := aliasVSDhcpServerLeaf(stmt, key)
	return func(tree map[string]any, t []string, isSet bool) error {
		if isSet {
			return errString("缺少取值: set virtual-switches " + t[1] + " dhcp-server " + stmt + " <value>")
		}
		return f(tree, append(append([]string{}, t...), ""), isSet)
	}
}

// aliasVSDNSProxyPair：virtual-switches <n> dns proxy server <ip> secondary <ip>（决策 #345）。
// t[5] 与 t[7] 是两个上游地址，一并追加或移除。
func aliasVSDNSProxyPair(tree map[string]any, t []string, isSet bool) error {
	vs, err := elemByID(tree, "virtual_switches", t[1])
	if err != nil {
		return err
	}
	for _, ip := range []string{t[5], t[7]} {
		if err := appendOrRemove(vs, "dns_proxy_servers", ip, isSet); err != nil {
			return err
		}
	}
	return nil
}

// aliasVSDNSProxyOne：virtual-switches <n> dns proxy server [secondary] <ip>（决策 #345）。
func aliasVSDNSProxyOne(tree map[string]any, t []string, isSet bool) error {
	vs, err := elemByID(tree, "virtual_switches", t[1])
	if err != nil {
		return err
	}
	return appendOrRemove(vs, "dns_proxy_servers", t[len(t)-1], isSet)
}

// aliasVSDNSProxyClear：delete virtual-switches <n> dns proxy server（清空本域上游，回落全局）。
// set 形态缺取值即明确报错（与 system 级同口径，避免误把「忘了填地址」当清空）。
func aliasVSDNSProxyClear(tree map[string]any, t []string, isSet bool) error {
	if isSet {
		return errString("缺少上游地址：set virtual-switches " + t[1] + " dns proxy server <ip> [secondary <ip>]")
	}
	vs, err := elemByID(tree, "virtual_switches", t[1])
	if err != nil {
		return err
	}
	delete(vs, "dns_proxy_servers")
	return nil
}

// aliasVSLearnLimit：virtual-switches <n> learn-limit <n>（决策 #337）。// 模型是单值整数（learn_limit）；delete 由无值形态的注册键处理（整键删除，幂等）。
// 取值必须为十进制正整数——负数/0/非数一律拒绝（与模型校验同口径，避免写出模型不认的值）。
func aliasVSLearnLimit(tree map[string]any, t []string, isSet bool) error {
	vs, err := elemByID(tree, "virtual_switches", t[1])
	if err != nil {
		return err
	}
	if !isSet {
		delete(vs, "learn_limit")
		return nil
	}
	n, err := strconv.Atoi(t[3])
	if err != nil || n < 1 {
		return errString("学习上限须为正整数: " + t[3])
	}
	vs["learn_limit"] = float64(n)
	return nil
}

// aliasVSGatewayIP：virtual-switches <n> gateway ip <prefix>（addresses 追加/移除）。
func aliasVSGatewayIP(tree map[string]any, t []string, isSet bool) error {
	vs, err := elemByID(tree, "virtual_switches", t[1])
	if err != nil {
		return err
	}
	gw := objOrNil(vs, "gateway")
	if gw == nil {
		if !isSet {
			return errString("无匹配配置: gateway")
		}
		gw = ensureObj(vs, "gateway")
	}
	if isSet {
		appendUniqueStr(gw, "addresses", t[4])
		return nil
	}
	removeStr(gw, "addresses", t[4])
	return nil
}

// aliasL3InterfaceAddr：virtual-switches <n> l3-interface <if> ip address <prefix>。
func aliasL3InterfaceAddr(tree map[string]any, t []string, isSet bool) error {
	vrf, err := l3VrfOf(tree, t[1], isSet)
	if err != nil {
		return err
	}
	li := elemByField(vrf, "l3_interfaces", "interface", t[3])
	if isSet {
		appendUniqueStr(li, "addresses", t[6])
		return nil
	}
	removeStr(li, "addresses", t[6])
	return nil
}

// aliasL3InterfaceDel：delete virtual-switches <n> l3-interface <if>。
func aliasL3InterfaceDel(tree map[string]any, t []string, isSet bool) error {
	if isSet {
		return errString("配置不完整，缺少取值: " + joinTokens(t))
	}
	vrf, err := l3VrfOf(tree, t[1], false)
	if err != nil {
		return err
	}
	arr, _ := vrf["l3_interfaces"].([]any)
	out := make([]any, 0, len(arr))
	hit := false
	for _, e := range arr {
		if em, ok := e.(map[string]any); ok {
			if s, _ := em["interface"].(string); s == t[3] {
				hit = true
				continue
			}
		}
		out = append(out, e)
	}
	if !hit {
		return errString("无匹配配置: " + t[3])
	}
	if len(out) == 0 {
		delete(vrf, "l3_interfaces")
	} else {
		vrf["l3_interfaces"] = out
	}
	return nil
}

// aliasStaticRoute：virtual-switches <n> static-routes <prefix|default> next-hop <ip>。
func aliasStaticRoute(tree map[string]any, t []string, isSet bool) error {
	vrf, err := l3VrfOf(tree, t[1], isSet)
	if err != nil {
		return err
	}
	prefix := t[3]
	if prefix == "default" {
		prefix = "0.0.0.0/0"
	}
	rt := elemByField(vrf, "routes", "prefix", prefix)
	if isSet {
		rt["next_hop"] = t[5]
		return nil
	}
	if _, ok := rt["next_hop"]; !ok {
		return errString("无匹配配置: " + prefix)
	}
	delete(rt, "next_hop")
	return nil
}

// aliasStaticRouteDistance：… static-routes <prefix> distance <n>。
func aliasStaticRouteDistance(tree map[string]any, t []string, isSet bool) error {
	vrf, err := l3VrfOf(tree, t[1], isSet)
	if err != nil {
		return err
	}
	prefix := t[3]
	if prefix == "default" {
		prefix = "0.0.0.0/0"
	}
	rt := elemByField(vrf, "routes", "prefix", prefix)
	if !isSet {
		delete(rt, "distance")
		return nil
	}
	n, err := numField(t[5])
	if err != nil {
		return err
	}
	rt["distance"] = n
	return nil
}

// aliasStaticRouteDel：delete virtual-switches <n> static-routes <prefix>。
func aliasStaticRouteDel(tree map[string]any, t []string, isSet bool) error {
	if isSet {
		return errString("配置不完整，缺少取值: " + joinTokens(t))
	}
	vrf, err := l3VrfOf(tree, t[1], false)
	if err != nil {
		return err
	}
	prefix := t[3]
	if prefix == "default" {
		prefix = "0.0.0.0/0"
	}
	arr, _ := vrf["routes"].([]any)
	out := make([]any, 0, len(arr))
	hit := false
	for _, e := range arr {
		if em, ok := e.(map[string]any); ok {
			if s, _ := em["prefix"].(string); s == prefix {
				hit = true
				continue
			}
		}
		out = append(out, e)
	}
	if !hit {
		return errString("无匹配配置: " + prefix)
	}
	if len(out) == 0 {
		delete(vrf, "routes")
	} else {
		vrf["routes"] = out
	}
	return nil
}

// aliasAclRule：acls <name> rule <seq> <k> <v> …（k ∈ source/destination/protocol/
// action/direction/source-port/destination-port；端口用 - 连接，落模型时转 _）。
func aliasAclRule(tree map[string]any, t []string, isSet bool) error {
	var acl map[string]any
	if isSet {
		acl = ensureElem(tree, "acls", t[1])
	} else {
		var err error
		if acl, err = elemByID(tree, "acls", t[1]); err != nil {
			return err
		}
	}
	rest := t[3:]
	if len(rest) == 0 {
		return errString("配置不完整，缺少取值: " + joinTokens(t))
	}
	seq := rest[0]
	if !isSet {
		arr, _ := acl["rules"].([]any)
		out := make([]any, 0, len(arr))
		hit := false
		for _, e := range arr {
			if em, ok := e.(map[string]any); ok && scalarEq(em["seq"], seq) {
				hit = true
				continue
			}
			out = append(out, e)
		}
		if !hit {
			return errString("无匹配配置: rule " + seq)
		}
		acl["rules"] = out
		return nil
	}
	rule := elemByField(acl, "rules", "seq", seq)
	for i := 1; i < len(rest); i += 2 {
		if i+1 >= len(rest) {
			return errString("语句不完整: " + rest[i] + " 缺少取值")
		}
		key := strings.ReplaceAll(rest[i], "-", "_")
		switch key {
		case "source", "destination", "protocol", "action", "direction",
			"source_port", "destination_port":
			rule[key] = rest[i+1]
		default:
			return errString("未知语句: \"" + rest[i] + "\"")
		}
	}
	return nil
}

// aliasNatPool：nat source-pool <n> address-range <a> to <b>。
func aliasNatPool(tree map[string]any, t []string, isSet bool) error {
	if !isSet {
		return aliasNatPoolDel(tree, t, isSet)
	}
	nat := ensureObj(tree, "nat")
	pool := elemByField(nat, "source_pools", "name", t[2])
	pool["address_range"] = t[4] + " to " + t[6]
	return nil
}

// aliasNatPoolDel：delete nat source-pool <n>。
func aliasNatPoolDel(tree map[string]any, t []string, _ bool) error {
	nat := objOrNil(tree, "nat")
	if nat == nil {
		return errString("无匹配配置: nat")
	}
	arr, _ := nat["source_pools"].([]any)
	out := make([]any, 0, len(arr))
	hit := false
	for _, e := range arr {
		if em, ok := e.(map[string]any); ok {
			if s, _ := em["name"].(string); s == t[2] {
				hit = true
				continue
			}
		}
		out = append(out, e)
	}
	if !hit {
		return errString("无匹配配置: " + t[2])
	}
	nat["source_pools"] = out
	return nil
}

// aliasNatRule：nat rules <seq> [match source <prefix> [virtual-switch <n>]]
// [action source-pool <n> | action interface <if>]，键值对可任意组合。
//
// delete 两种形态（R84-25）：
//   - `delete nat rules <seq>`：整条规则删除（无尾随 token）；
//   - `delete nat rules <seq> <键值对…>`：只清对应叶子，规则与其余叶子保留——
//     此前不分形态一律整条删，操作者 `delete nat rules 10 action source-pool pool-a`
//     想摘掉池引用，结果连 match 与 action interface 一起消失。
func aliasNatRule(tree map[string]any, t []string, isSet bool) error {
	nat := objOrNil(tree, "nat")
	rest := t[2:]
	if len(rest) == 0 {
		return errString("配置不完整，缺少取值: " + joinTokens(t))
	}
	seq := rest[0]
	if !isSet {
		if nat == nil {
			return errString("无匹配配置: nat rules")
		}
		arr, _ := nat["rules"].([]any)
		// 无尾随 token：整条规则删除（`delete nat rules <seq>`）——重复 seq 一并删，
		// 既有行为不变（该状态本身非法，操作者要能一次清干净）
		if len(rest) == 1 {
			out := make([]any, 0, len(arr))
			hit := false
			for _, e := range arr {
				if em, ok := e.(map[string]any); ok && scalarEq(em["seq"], seq) {
					hit = true
					continue
				}
				out = append(out, e)
			}
			if !hit {
				return errString("无匹配配置: rule " + seq)
			}
			nat["rules"] = out
			return nil
		}
		// 带尾随 token：只清对应叶子，规则与其余叶子保留（R84-25）
		rule, _ := selectElement(arr, "seq", seq)
		if rule == nil {
			return errString("无匹配配置: rule " + seq)
		}
		return clearNatRuleLeaves(rule, rest)
	}
	nat = ensureObj(tree, "nat")
	rule := elemByField(nat, "rules", "seq", seq)
	action := ensureObj(rule, "action")
	for i := 1; i < len(rest); i += 2 {
		if i+1 >= len(rest) {
			return errString("语句不完整: " + rest[i] + " 缺少取值")
		}
		switch rest[i] {
		case "match":
			// "match source <prefix>"
			if rest[i+1] != "source" {
				return errString("未知语句: \"" + rest[i+1] + "\"")
			}
			if i+2 >= len(rest) {
				return errString("语句不完整: match source 缺少取值")
			}
			rule["match_source"] = rest[i+2]
			i++
		case "source":
			if i+2 >= len(rest) {
				return errString("语句不完整: source 缺少取值")
			}
			rule["match_source"] = rest[i+1]
			i++
		case "virtual-switch":
			rule["virtual_switch"] = rest[i+1]
		case "action":
			switch rest[i+1] {
			case "source-pool":
				if i+2 >= len(rest) {
					return errString("语句不完整: action source-pool 缺少取值")
				}
				action["source_pool"] = rest[i+2]
				i++
			case "interface":
				if i+2 >= len(rest) {
					return errString("语句不完整: action interface 缺少取值")
				}
				action["interface"] = rest[i+2]
				i++
			default:
				return errString("未知语句: \"" + rest[i+1] + "\"")
			}
		case "source-pool":
			action["source_pool"] = rest[i+1]
		case "interface":
			action["interface"] = rest[i+1]
		default:
			return errString("未知语句: \"" + rest[i] + "\"")
		}
	}
	return nil
}

// clearNatRuleLeaves 清规则内的叶子（R84-25；与 aliasNatRule 的 set 键值对解析镜像）。
//
// 取值 token 容忍：操作者常把 set 行原样换成 delete，取值与现值不一致也照删
// （与 applyTokens 的值叶子删除同口径）。叶子本就不在场（键缺失或空串）则报
// 「无匹配配置」——否则会由 commitTree 的 Diff 兜底报「值未变化」，对 delete 场景
// 指错了方向（applyTokens 删不存在的叶子同样报「无匹配配置」）。
func clearNatRuleLeaves(rule map[string]any, rest []string) error {
	action, _ := rule["action"].(map[string]any)
	clear := func(container map[string]any, key, label string) error {
		if container == nil {
			return errString("无匹配配置: " + label)
		}
		if v, ok := container[key]; !ok || v == "" {
			return errString("无匹配配置: " + label)
		}
		delete(container, key)
		return nil
	}
	for i := 1; i < len(rest); i += 2 {
		if i+1 >= len(rest) {
			return errString("语句不完整: " + rest[i] + " 缺少取值")
		}
		switch rest[i] {
		case "match":
			// "match source <prefix> [virtual-switch <n>]"：逐键清，virtual-switch 另给才清
			if rest[i+1] != "source" {
				return errString("未知语句: \"" + rest[i+1] + "\"")
			}
			if i+2 >= len(rest) {
				return errString("语句不完整: match source 缺少取值")
			}
			if err := clear(rule, "match_source", "match source"); err != nil {
				return err
			}
			i++
		case "source":
			if i+2 >= len(rest) {
				return errString("语句不完整: source 缺少取值")
			}
			if err := clear(rule, "match_source", "match source"); err != nil {
				return err
			}
			i++
		case "virtual-switch":
			if err := clear(rule, "virtual_switch", "virtual-switch"); err != nil {
				return err
			}
		case "action":
			switch rest[i+1] {
			case "source-pool":
				if i+2 >= len(rest) {
					return errString("语句不完整: action source-pool 缺少取值")
				}
				if err := clear(action, "source_pool", "action source-pool"); err != nil {
					return err
				}
				i++
			case "interface":
				if i+2 >= len(rest) {
					return errString("语句不完整: action interface 缺少取值")
				}
				if err := clear(action, "interface", "action interface"); err != nil {
					return err
				}
				i++
			default:
				return errString("未知语句: \"" + rest[i+1] + "\"")
			}
		case "source-pool":
			if err := clear(action, "source_pool", "action source-pool"); err != nil {
				return err
			}
		case "interface":
			if err := clear(action, "interface", "action interface"); err != nil {
				return err
			}
		default:
			return errString("未知语句: \"" + rest[i] + "\"")
		}
	}
	return nil
}

// aliasNatStatic：nat static <inside> to <outside>。
func aliasNatStatic(tree map[string]any, t []string, isSet bool) error {
	if isSet {
		nat := ensureObj(tree, "nat")
		st := elemByField(nat, "static", "inside_ip", t[2])
		st["outside_ip"] = t[4]
		return nil
	}
	nat := objOrNil(tree, "nat")
	if nat == nil {
		return errString("无匹配配置: nat")
	}
	arr, _ := nat["static"].([]any)
	out := make([]any, 0, len(arr))
	hit := false
	for _, e := range arr {
		if em, ok := e.(map[string]any); ok {
			if s, _ := em["inside_ip"].(string); s == t[2] {
				hit = true
				continue
			}
		}
		out = append(out, e)
	}
	if !hit {
		return errString("无匹配配置: " + t[2])
	}
	nat["static"] = out
	return nil
}

// aliasPMSource：port-mirroring <n> source interface <if> [direction <d>]
//
//	port-mirroring <n> source vnf <vm> interface <vnic> [direction <d>]
func aliasPMSource(tree map[string]any, t []string, isSet bool) error {
	if !isSet {
		pm, err := elemByID(tree, "port_mirroring", t[1])
		if err != nil {
			return err
		}
		delete(pm, "source")
		return nil
	}
	pm := ensureElem(tree, "port_mirroring", t[1])
	src := ensureObj(pm, "source")
	rest := t[3:]
	if len(rest) == 0 {
		return errString("配置不完整，缺少取值: " + joinTokens(t))
	}
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "interface":
			if i+1 >= len(rest) {
				return errString("语句不完整: interface 缺少取值")
			}
			if _, ok := src["vnf"]; ok {
				src["vnf_interface"] = rest[i+1]
			} else {
				src["interface"] = rest[i+1]
			}
			i++
		case "vnf":
			if i+1 >= len(rest) {
				return errString("语句不完整: vnf 缺少取值")
			}
			src["vnf"] = rest[i+1]
			i++
		case "direction":
			if i+1 >= len(rest) {
				return errString("语句不完整: direction 缺少取值")
			}
			src["direction"] = rest[i+1]
			i++
		default:
			return errString("未知语句: \"" + rest[i] + "\"")
		}
	}
	return nil
}

// aliasPMAnalyzer：port-mirroring <n> analyzer interface <if>（模型 analyzer 为字符串）。
func aliasPMAnalyzer(tree map[string]any, t []string, isSet bool) error {
	var pm map[string]any
	if isSet {
		pm = ensureElem(tree, "port_mirroring", t[1])
	} else {
		var err error
		if pm, err = elemByID(tree, "port_mirroring", t[1]); err != nil {
			return err
		}
	}
	if !isSet {
		delete(pm, "analyzer")
		return nil
	}
	pm["analyzer"] = t[4]
	return nil
}

// aliasQosPolicy：qos policies <n> [cir <bps>] [cbs <bytes>]。
func aliasQosPolicy(tree map[string]any, t []string, isSet bool) error {
	rest := t[2:]
	if len(rest) == 0 {
		return errString("配置不完整，缺少取值: " + joinTokens(t))
	}
	name := rest[0]
	if !isSet {
		if len(rest) == 1 { // delete qos policies <n>
			arr, _ := tree["qos_policies"].([]any)
			out := make([]any, 0, len(arr))
			hit := false
			for _, e := range arr {
				if em, ok := e.(map[string]any); ok {
					if s, _ := em["name"].(string); s == name {
						hit = true
						continue
					}
				}
				out = append(out, e)
			}
			if !hit {
				return errString("无匹配配置: " + name)
			}
			tree["qos_policies"] = out
			return nil
		}
		pol, _ := selectElement(anySlice(tree["qos_policies"]), "name", name)
		if pol == nil {
			return errString("无匹配配置: " + name)
		}
		for i := 1; i < len(rest); i += 2 {
			delete(pol, rest[i])
		}
		return nil
	}
	pol := ensureElem(tree, "qos_policies", name)
	for i := 1; i < len(rest); i += 2 {
		if i+1 >= len(rest) {
			return errString("语句不完整: " + rest[i] + " 缺少取值")
		}
		switch rest[i] {
		case "cir", "cbs":
			n, err := numField(rest[i+1])
			if err != nil {
				return err
			}
			pol[rest[i]] = n
		default:
			return errString("未知语句: \"" + rest[i] + "\"")
		}
	}
	return nil
}

// ifacePolicyAlias：`interfaces <if> <ingress-policy|egress-policy> <name>`（决策 #331：
// 两个方向的绑定是同一族语句，仅模型字段不同，故用同一实现参数化字段名）。
func ifacePolicyAlias(field string) func(map[string]any, []string, bool) error {
	return func(tree map[string]any, t []string, isSet bool) error {
		var ifc map[string]any
		if isSet {
			ifc = ensureElem(tree, "interfaces", t[1])
		} else {
			var err error
			if ifc, err = elemByID(tree, "interfaces", t[1]); err != nil {
				return err
			}
		}
		if !isSet {
			delete(ifc, field)
			return nil
		}
		ifc[field] = t[3]
		return nil
	}
}

// ifacePolicyClearAlias：`delete interfaces <if> <ingress-policy|egress-policy>`（不带策略名）。
func ifacePolicyClearAlias(field string) func(map[string]any, []string, bool) error {
	return func(tree map[string]any, t []string, _ bool) error {
		ifc, err := elemByID(tree, "interfaces", t[1])
		if err != nil {
			return err
		}
		delete(ifc, field)
		return nil
	}
}

// ---------- 接口入向风暴抑制（决策 #385） ----------

// ifaceStormMissing 接口未声明时的统一报错（set 与 delete 同一条）：给能照做的一步，
// 而不是 elemByID 的「无匹配配置」（契约口径：storm-control 只配在**已声明**的物理口上，
// 本语句不代建声明）。
func ifaceStormMissing(ifname string) error {
	return errString("接口 " + ifname + " 未在配置中声明：先执行 set interfaces " + ifname +
		" description <说明> 声明该口，再配 storm-control")
}

// ifaceStormObj 取（或按需建）接口元素里的 storm_control 对象（删除路径的「空壳」清理
// 由 ifaceStormPrune 承担）：留下的空对象既反推不出语句（display set 回放自校验会报内部
// 错误），又让「配过又删光」与「从未配置」不可区分（与 pruneEmptySingleton 的
// management/nat/metrics 同因）。
func ifaceStormObj(ifc map[string]any) map[string]any {
	obj, _ := ifc["storm_control"].(map[string]any)
	if obj == nil {
		obj = map[string]any{}
		ifc["storm_control"] = obj
	}
	return obj
}

// ifaceStormPrune 两类皆空时删掉 storm_control 键（空壳不留）。
func ifaceStormPrune(ifc map[string]any) {
	obj, _ := ifc["storm_control"].(map[string]any)
	if obj == nil {
		return
	}
	for _, v := range obj {
		if n, ok := v.(float64); !ok || n != 0 {
			return // 还有非零取值（或未知键）：保留，宁可不剪
		}
	}
	delete(ifc, "storm_control")
}

// ifaceStormAlias：`set interfaces <if> storm-control <broadcast|multicast> <kbps>`（决策 #385）。
// 取值须为正整数（上界由模型校验在提交期拒绝，与 learn-limit 同一分工）。
func ifaceStormAlias(field string) func(map[string]any, []string, bool) error {
	return func(tree map[string]any, t []string, isSet bool) error {
		ifc, err := elemByID(tree, "interfaces", t[1])
		if err != nil {
			return ifaceStormMissing(t[1])
		}
		if !isSet {
			if obj, ok := ifc["storm_control"].(map[string]any); ok {
				delete(obj, field)
				ifaceStormPrune(ifc)
			}
			return nil
		}
		n, err := strconv.Atoi(t[4])
		if err != nil || n < 1 {
			return errString("风暴抑制速率须为大于 0 的整数（kbps）: " + t[4])
		}
		ifaceStormObj(ifc)[field] = float64(n)
		return nil
	}
}

// ifaceStormClearAlias：`delete interfaces <if> storm-control <broadcast|multicast>`
// （该叶子形态不带取值）。set 缺取值明确报错，不落半套配置。
func ifaceStormClearAlias(field string) func(map[string]any, []string, bool) error {
	f := ifaceStormAlias(field)
	return func(tree map[string]any, t []string, isSet bool) error {
		if isSet {
			return errString("缺少取值: set interfaces " + t[1] + " storm-control " +
				strings.TrimSuffix(field, "_kbps") + " <kbps>")
		}
		return f(tree, append(append([]string{}, t...), ""), isSet)
	}
}

// ifaceStormBareAlias：`interfaces <if> storm-control`（无取值）。delete ＝两类都清；
// set 缺取值明确报错并给出两类写法。
func ifaceStormBareAlias(tree map[string]any, t []string, isSet bool) error {
	ifc, err := elemByID(tree, "interfaces", t[1])
	if err != nil {
		return ifaceStormMissing(t[1])
	}
	if isSet {
		return errString("缺少取值: set interfaces " + t[1] +
			" storm-control <broadcast|multicast> <kbps>（单位 kbps；两类可分别配置）")
	}
	delete(ifc, "storm_control")
	return nil
}

// unsupportedStormKindMsg 本版本**有意不支持**的 storm-control 类别的执行期报错（决策 #385）。
//
// unknown-unicast 要判「目的 MAC 是否已在转发表中学到」，而以太头里没有这个可匹配位——
// 产品的数据面按目的 MAC 掩码分类（VPP classify），表达不了它，故语句树里不建该叶子。
// 但「未知命令」给不出原因，用户只会反复试写法；这里在派发前识别该类别，给一条能照做的
// 报错（说明为什么不支持 + 可用的替代）。返回 ""＝不涉及。
func unsupportedStormKindMsg(op string, tokens []string) string {
	if len(tokens) < 3 || tokens[0] != "interfaces" {
		return ""
	}
	// 关键字允许无歧义前缀（storm-c / storm-cont…），按前缀识别（≥ "storm"）
	if !strings.HasPrefix("storm-control", tokens[2]) || len(tokens[2]) < 5 {
		return ""
	}
	if len(tokens) < 4 || tokens[3] != "unknown-unicast" {
		return ""
	}
	return op + " interfaces " + tokens[1] + " storm-control unknown-unicast …：本版本不支持" +
		" unknown-unicast 风暴抑制——判定「目的 MAC 未学习」不是以太头里的可匹配位，" +
		"按目的 MAC 掩码只能表达广播（broadcast，ff:ff:ff:ff:ff:ff）与组播（multicast，I/G 位=1）两类"
}

// ---------- 接口端口安全（决策 #389） ----------

// ifacePortSecMissing 接口未声明时的统一报错（set 与 delete 同一条）：给能照做的一步，
// 而不是 elemByID 的「无匹配配置」（契约口径：端口安全只配在**已声明**的物理口上，
// 本语句不代建声明——与 storm-control 同一分工）。
func ifacePortSecMissing(ifname string) error {
	return errString("接口 " + ifname + " 未在配置中声明：先执行 set interfaces " + ifname +
		" description <说明> 声明该口，再配 port-security")
}

// ifacePortSecMacAlias：`set interfaces <if> port-security mac <mac>`（追加）/ 同形 delete（按值删）。
//
// 追加语义：同一 MAC 写两遍不会覆盖——重复由提交校验按「重复（大小写不敏感）」拒绝，
// 不在语句层静默去重（静默去重会让「值未变化」吞掉操作者的笔误）。
// MAC 合法性在语句层即时校验并归一小写（PortSecMAC 解码层还会再归一一次，两处同形；
// 语句层先做是为了给「哪个 token 错了」的可操作报错）。
func ifacePortSecMacAlias(tree map[string]any, t []string, isSet bool) error {
	ifc, err := elemByID(tree, "interfaces", t[1])
	if err != nil {
		return ifacePortSecMissing(t[1])
	}
	hw, perr := net.ParseMAC(strings.TrimSpace(t[4]))
	if perr != nil {
		return errString("白名单 MAC \"" + t[4] + "\" 非法: " + perr.Error() + "（形如 b0:b0:00:00:00:01）")
	}
	mac := strings.ToLower(hw.String())
	cur, _ := ifc["port_security"].([]any)
	if !isSet {
		out := make([]any, 0, len(cur))
		for _, el := range cur {
			if s, ok := el.(string); ok && strings.EqualFold(s, mac) {
				continue // 按值删（大小写不敏感）
			}
			out = append(out, el)
		}
		if len(out) == len(cur) {
			return errString("无匹配配置: port_security " + mac)
		}
		if len(out) == 0 {
			delete(ifc, "port_security") // 清空后不留空壳（display set 反推不出空数组）
			return nil
		}
		ifc["port_security"] = out
		return nil
	}
	ifc["port_security"] = append(cur, mac)
	return nil
}

// ifacePortSecMacClearAlias：`delete interfaces <if> port-security mac`（缺取值）。
// set 缺取值明确报错，不落半套配置。
func ifacePortSecMacClearAlias(tree map[string]any, t []string, isSet bool) error {
	if isSet {
		return errString("缺少取值: set interfaces " + t[1] + " port-security mac <mac>")
	}
	return errString("缺少取值: delete interfaces " + t[1] + " port-security mac <mac>（按值删一条；" +
		"清空整段用 delete interfaces " + t[1] + " port-security）")
}

// ifacePortSecBareAlias：`interfaces <if> port-security`（无取值）。delete ＝清空整段
// （＝停用端口安全）；set 缺取值明确报错并给出正确写法。
func ifacePortSecBareAlias(tree map[string]any, t []string, isSet bool) error {
	ifc, err := elemByID(tree, "interfaces", t[1])
	if err != nil {
		return ifacePortSecMissing(t[1])
	}
	if isSet {
		return errString("缺少取值: set interfaces " + t[1] + " port-security mac <mac>（追加一条白名单 MAC）")
	}
	_, ok := ifc["port_security"]
	if !ok {
		return errString("无匹配配置: port_security")
	}
	delete(ifc, "port_security")
	return nil
}

// aliasLldpEnable：protocols lldp enable <bool>。
func aliasLldpEnable(tree map[string]any, t []string, isSet bool) error {
	l := lldpObj(tree, isSet)
	if l == nil {
		return errNoLLDP
	}
	if !isSet {
		delete(l, "enabled")
		return nil
	}
	b, err := boolField(t[3])
	if err != nil {
		return err
	}
	l["enabled"] = b
	return nil
}

// aliasLldpInterface：protocols lldp interface <if> enable <bool>。
func aliasLldpInterface(tree map[string]any, t []string, isSet bool) error {
	l := lldpObj(tree, isSet)
	if l == nil {
		return errNoLLDP
	}
	if !isSet {
		arr, _ := l["interfaces"].([]any)
		out := make([]any, 0, len(arr))
		for _, e := range arr {
			if em, ok := e.(map[string]any); ok {
				if s, _ := em["interface"].(string); s == t[3] {
					continue
				}
			}
			out = append(out, e)
		}
		l["interfaces"] = out
		return nil
	}
	em := elemByField(l, "interfaces", "interface", t[3])
	b, err := boolField(t[5])
	if err != nil {
		return err
	}
	em["enabled"] = b
	return nil
}

// anySlice 取数组（不存在返回 nil）。
func anySlice(v any) []any {
	arr, _ := v.([]any)
	return arr
}

// joinTokens 便于错误信息复用。
func joinTokens(t []string) string { return strings.Join(t, " ") }

// aliasL3InterfaceAclIn：virtual-switches <n> l3-interface <if> acl-in <acl>（§2.5 绑定）。
func aliasL3InterfaceAclIn(tree map[string]any, t []string, isSet bool) error {
	vrf, err := l3VrfOf(tree, t[1], isSet)
	if err != nil {
		return err
	}
	li := elemByField(vrf, "l3_interfaces", "interface", t[3])
	if !isSet {
		delete(li, "acl_in")
		return nil
	}
	li["acl_in"] = t[5]
	return nil
}

// aliasStaticRouteBoth：… static-routes <prefix> next-hop <ip> distance <n>。
func aliasStaticRouteBoth(tree map[string]any, t []string, isSet bool) error {
	if err := aliasStaticRoute(tree, append([]string{}, t[:6]...), isSet); err != nil {
		return err
	}
	return aliasStaticRouteDistance(tree, []string{t[0], t[1], t[2], t[3], t[6], t[7]}, isSet)
}

// aliasStaticRouteBothRev：… static-routes <prefix> distance <n> next-hop <ip>。
func aliasStaticRouteBothRev(tree map[string]any, t []string, isSet bool) error {
	if err := aliasStaticRouteDistance(tree, t[:6], isSet); err != nil {
		return err
	}
	return aliasStaticRoute(tree, []string{t[0], t[1], t[2], t[3], t[6], t[7]}, isSet)
}

// aliasVSCrossConnect：cross-connect 交换机（FR-NET-012）。
// t = ["virtual-switches", <name>, "cross-connect"(, <port-a>, <port-b>)]。
func aliasVSCrossConnect(tree map[string]any, t []string, isSet bool) error {
	vs, err := elemByID(tree, "virtual_switches", t[1])
	if err != nil {
		return err
	}
	if !isSet {
		delete(vs, "cross_connect")
		return nil
	}
	if len(t) < 5 || t[3] == "" || t[4] == "" {
		return fmt.Errorf("配置不完整，缺少取值: virtual-switches %s cross-connect <port-a> <port-b>", t[1])
	}
	a, b := t[3], t[4]
	if a == b {
		return fmt.Errorf("cross-connect 的两个端口不能相同（%s）", a)
	}
	ports, _ := vs["ports"].([]any)
	declared := func(seq string) bool {
		for _, p := range ports {
			if m, ok := p.(map[string]any); ok && scalarEq(m["seq"], seq) {
				return true
			}
		}
		return false
	}
	if !declared(a) || !declared(b) {
		return fmt.Errorf("cross-connect 引用的端口未声明，请先 set virtual-switches %s ports %s interface <ifname>（另需端口 %s）", t[1], a, b)
	}
	if len(ports) != 2 {
		return fmt.Errorf("cross-connect 交换机仅支持两个端口，实际 %d 个", len(ports))
	}
	vs["cross_connect"] = true
	return nil
}
