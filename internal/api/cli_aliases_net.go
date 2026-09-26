package api

import (
	"fmt"
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

	// interfaces <if> ingress-policy <name>
	{pattern: []string{"interfaces", "*", "ingress-policy", "*"},
		apply: aliasIngressPolicy},
	{pattern: []string{"interfaces", "*", "ingress-policy"},
		apply: func(tree map[string]any, t []string, _ bool) error {
			ifc, err := elemByID(tree, "interfaces", t[1])
			if err != nil {
				return err
			}
			delete(ifc, "ingress_policy")
			return nil
		}},

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

// aliasIngressPolicy：interfaces <if> ingress-policy <name>。
func aliasIngressPolicy(tree map[string]any, t []string, isSet bool) error {
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
		delete(ifc, "ingress_policy")
		return nil
	}
	ifc["ingress_policy"] = t[3]
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
