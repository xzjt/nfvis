package api

// display set 家族逆映射发射器（决策 #155）：与 cli_aliases_*.go 的别名规则一一对照，
// 把「语句树关键字路径 ⇄ 模型 JSON 键」结构不一致的家族从 JSON 反推回规范 set 语句。
//
// 注册键 = 配置根起的关键字路径（emitMechanicalInner 按 jsonKeyOf 命中 JSON 键后才可能
// 进入，故注册键必须是「JSON 键与树关键字同名」的那一层）。各注册键互不为前缀——
// emitterFor 对注册表做前缀匹配且遍历顺序不定，重叠注册会导致命中不确定。
//
// 进入层级契约：发射器可能在注册层级（家族根）或其下任一层被进入——顶层逆走沿树
// 下钻（分支 5/7），配置模式 edit 层级经 navigateJSON 成功的路径也会进入。发射器按
// keyPath 判层：本文件每个发射器switch完整列出可达层级，未列出的交给
// emitMechanicalInner（纯机械子区域）。家族根为 map 的家族在顶层与 edit 进入时形状
// 相同；具名数组家族（interfaces/bonds/virtual-switches/acls/port-mirroring/
// virtual-machine-functions/container-functions）顶层逆走总是逐元素进入、且通用逆走
// 已先发过元素裸声明——这类家族的元素层发射器不再补裸声明，edit 进入时靠后续字段
// 语句经身份消费建出元素（身份取值即语句倒数第二段）。
//
// 结构性盲区（注册键不可能覆盖，回放自校验会如实报错而不是少输出）：
//   - vrfs 数组（L3 交换机的 l3-interface/static-routes 落点）：命令树没有 vrfs 关键字，
//     任何注册键都无法被 keyPath 命中。type l3 语句会同步建出同名 VRF 条目（别名 apply），
//     故「仅 type l3」的空壳可随该语句自复现；带 l3_interfaces/routes 数据的配置目前
//     无语句可还原。
//   - qos_policies 数组（qos policies 语句落点）：树顶层关键字为 qos、JSON 键为
//     qos_policies，通用逆走在根层查不到 "qos" 键即跳过，同上无注册键可达。

import (
	"sort"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/schema"
)

func init() {
	subtreeEmitters["system"] = emitSystemFamily
	subtreeEmitters["protocols lldp"] = emitLldpFamily
	subtreeEmitters["interfaces"] = emitInterfacesFamily
	subtreeEmitters["bonds"] = emitBondsFamily
	subtreeEmitters["virtual-switches"] = emitVirtualSwitchFamily
	subtreeEmitters["acls"] = emitAclsFamily
	subtreeEmitters["nat"] = emitNatFamily
	subtreeEmitters["port-mirroring"] = emitPortMirroringFamily
	subtreeEmitters["resource-pools"] = emitResourcePoolsFamily
	subtreeEmitters["vpp dpdk"] = emitVppDpdkFamily
	subtreeEmitters["virtual-machine-functions"] = emitVMFamily
	subtreeEmitters["container-functions"] = emitContainerFamily
}

// toks 语句 token 拼装：prefix 原样拷贝后追加（w.add 持有切片，禁止 append 共享底层数组）。
func toks(prefix []string, parts ...string) []string {
	out := make([]string, 0, len(prefix)+len(parts))
	out = append(out, prefix...)
	return append(out, parts...)
}

func kpOf(keyPath []string) string { return strings.Join(keyPath, " ") }

// joinScalars 标量数组 → 分隔串（core-list/vlan-list 的逆格式化，与 valueTransforms 对偶）。
func joinScalars(arr []any, sep string) string {
	parts := make([]string, 0, len(arr))
	for _, e := range arr {
		parts = append(parts, formatScalar(e))
	}
	return strings.Join(parts, sep)
}

// lastTok prefix 末位（身份取值或关键字），仅供注释文案。
func lastTok(prefix []string) string {
	if len(prefix) == 0 {
		return ""
	}
	return prefix[len(prefix)-1]
}

// withoutZeroScalars 浅拷贝并剥离空串/零值标量。模型里无 omitempty 的叶子（如 VM 的
// image:""、vcpu.count:0）在 JSON 树中恒存在，通用逆走会照常发语句——而该值回放到
// 已含零值的元素上是「值未变化」的空操作，被 Diff 兜底拒绝；缺席与零值在模型序列化
// 后逐字节相同（零值自复现），剥离不损失还原性。仅用于委托 emitMechanicalInner 之前。
func withoutZeroScalars(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch x := v.(type) {
		case string:
			if x == "" {
				continue
			}
		case float64:
			if x == 0 {
				continue
			}
		}
		out[k] = v
	}
	return out
}

// ---------- system（ntp/dns/login/management/health/syslog/api 家族统一接管） ----------
//
// system 子树里 JSON 键与树关键字同层的只有 hostname/timezone/idle-timeout/ntp/management/
// kernel/health/syslog/login/api；dns_servers 扁平在 system 下（树为 system→dns→server
// 两级），通用逆走查不到 "dns" 键即跳过，故必须注册到 system 层才能覆盖。

func emitSystemFamily(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error {
	switch kpOf(keyPath) {
	case "system":
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		// dns_servers：数组标量按声明序逐条发射（appendOrRemove 语义保序，顺序即还原序）
		if arr, ok := m["dns_servers"].([]any); ok {
			for _, ip := range arr {
				w.add(toks(prefix, "dns", "server", formatScalar(ip)))
			}
		}
		return emitMechanicalInner(w, node, val, prefix, keyPath)
	case "system ntp":
		arr, ok := val.([]any)
		if !ok {
			return nil
		}
		for _, e := range arr {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			srv, ok := em["server"]
			if !ok {
				continue
			}
			t := toks(prefix, "server", formatScalar(srv))
			if prefer, ok := em["prefer"].(bool); ok && prefer {
				t = append(t, "prefer") // 无值 flag
			}
			w.add(t)
		}
		return nil
	case "system login":
		return emitSystemLogin(w, node, val, prefix, keyPath)
	case "system api":
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		// 证书与私钥扁平在 api 对象（树多一层 tls）；一条语句同时给两件的 7 token 形态
		// 拆成两条单件语句回放等价
		if v, ok := m["cert_file"]; ok && v != "" {
			w.add(toks(prefix, "tls", "cert-file", formatScalar(v)))
		}
		if v, ok := m["key_file"]; ok && v != "" {
			w.add(toks(prefix, "tls", "key-file", formatScalar(v)))
		}
		if v, ok := m["tls_self_signed"].(bool); ok && v {
			w.add(toks(prefix, "tls", "self-signed", "regenerate"))
		}
		return emitMechanicalInner(w, node, val, prefix, keyPath) // port/token-ttl/max-sessions 机械可达
	case "system management":
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		// address：树为 management→ip→address 三段，JSON 键扁平为 management.address
		if v, ok := m["address"]; ok && v != "" {
			w.add(toks(prefix, "ip", "address", formatScalar(v)))
		}
		return emitMechanicalInner(w, node, val, prefix, keyPath) // interface（透明参数）/gateway 机械可达
	case "system health":
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		// 阈值扁平在 health 下（树多一层 thresholds），三个键均为 omitempty（0 不落库）
		for _, kv := range []struct{ key, kw string }{
			{"cpu_temp_celsius", "cpu-temp-celsius"},
			{"disk_temp_celsius", "disk-temp-celsius"},
			{"disk_used_percent", "disk-used-percent"},
		} {
			if v, ok := m[kv.key]; ok {
				w.add(toks(prefix, "thresholds", kv.kw, formatScalar(v)))
			}
		}
		return nil
	case "system syslog":
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		// 远端：host 一条语句尾随成对参数（port/facility/severity 顺序与别名解析对偶）
		if host, ok := m["remote_host"]; ok && host != "" {
			t := toks(prefix, "host", formatScalar(host))
			if v, ok := m["remote_port"].(float64); ok && v != 0 {
				t = append(t, "port", formatScalar(v))
			}
			if v, ok := m["facility"]; ok && v != "" {
				t = append(t, "facility", formatScalar(v))
			}
			if v, ok := m["severity"]; ok && v != "" {
				t = append(t, "severity", formatScalar(v))
			}
			w.add(t)
		}
		// 本地：level/retention-days/max-size-mb 扁平在 syslog 下（树多一层 local）
		if v, ok := m["level"]; ok && v != "" {
			w.add(toks(prefix, "local", "level", formatScalar(v)))
		}
		if v, ok := m["retention_days"].(float64); ok && v != 0 {
			w.add(toks(prefix, "local", "retention-days", formatScalar(v)))
		}
		if v, ok := m["max_size_mb"].(float64); ok && v != 0 {
			w.add(toks(prefix, "local", "max-size-mb", formatScalar(v)))
		}
		return nil
	default:
		// system kernel（全机械）、system login password-policy（值叶子机械）等
		return emitMechanicalInner(w, node, val, prefix, keyPath)
	}
}

// emitSystemLogin users/classes 数组（单复数不一致，机械不可达）+ password_policy 机械委托。
func emitSystemLogin(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error {
	m, ok := val.(map[string]any)
	if !ok {
		return nil
	}
	if arr, ok := m["users"].([]any); ok {
		for _, e := range arr {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			name, _ := em["name"].(string)
			if name == "" {
				continue
			}
			// 敏感键口径与通用逆走一致（model.IsSensitiveKey）：口令哈希不输出语句——
			// password 别名会把明文哈希成新口令，输出占位符或哈希都会造成静默凭据替换。
			for k := range em {
				if !model.IsSensitiveKey(k) {
					continue
				}
				w.note("system login user %s：口令哈希不入 display set（复制配置后需重新 set system login user %s password <口令>）", name, name)
				break
			}
			if cls, ok := em["class"]; ok && cls != "" {
				w.add(toks(prefix, "user", name, "class", formatScalar(cls)))
			}
			// 仅有名字的用户无语句可还原：`set system login user <name>` 被命令树判为
			// 不完整语句（不能单独成句），正常流程也建不出这种账号——交回放自校验报错。
		}
	}
	if arr, ok := m["classes"].([]any); ok {
		for _, e := range arr {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			name := formatScalar(em["name"])
			if name == "" {
				continue
			}
			base := toks(prefix, "class", name)
			emitted := false
			for _, kind := range []string{"allow", "deny"} {
				if paths, ok := em[kind].([]any); ok {
					for _, p := range paths {
						w.add(append(append([]string{}, base...), kind, formatScalar(p)))
						emitted = true
					}
				}
			}
			if !emitted {
				w.add(base) // 仅声明的 class：裸语句合法（与 user 不同，class 可单独成句）
			}
		}
	}
	return emitMechanicalInner(w, node, val, prefix, keyPath) // password_policy 机械可达
}

// ---------- protocols lldp ----------
//
// enabled/interfaces 与树关键字单复数、词形不一致（enable⇄enabled、interface⇄interfaces），
// 机械逆走均查不到；advertisement-interval 机械可达但一并显式发射，保持全显式免委托。

func emitLldpFamily(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error {
	m, ok := val.(map[string]any)
	if !ok {
		return nil
	}
	if v, ok := m["enabled"].(bool); ok && v {
		w.add(toks(prefix, "enable", "true")) // 关闭态 omitempty 不落库，无需 false 语句
	}
	if v, ok := m["advertisement_interval"].(float64); ok && v != 0 {
		w.add(toks(prefix, "advertisement-interval", formatScalar(v)))
	}
	if arr, ok := m["interfaces"].([]any); ok {
		for _, e := range arr {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			ifc, ok := em["interface"]
			if !ok {
				continue
			}
			en, ok := em["enabled"]
			if !ok { // 模型字段无 omitempty，正常恒存在；缺失时如实报错而非猜值
				w.note("protocols lldp interface %s：缺少启停状态，无法反推语句", formatScalar(ifc))
				continue
			}
			w.add(toks(prefix, "interface", formatScalar(ifc), "enable", formatScalar(en)))
		}
	}
	return nil
}

// ---------- interfaces ----------
//
// ingress_policy 是字符串而语句树按「具名数组容器」建模（ingress-policy <name>），
// 机械逆走按数组解容器失败即跳过；sriov 子对象机械可达（经委托再入本发射器后委托）。

func emitInterfacesFamily(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error {
	if kpOf(keyPath) == "interfaces" {
		if m, ok := val.(map[string]any); ok {
			if v, ok := m["ingress_policy"]; ok && v != "" {
				w.add(toks(prefix, "ingress-policy", formatScalar(v)))
			}
		}
	}
	return emitMechanicalInner(w, node, val, prefix, keyPath)
}

// ---------- bonds ----------
//
// members 是 []string 而语句树按具名数组建模（成员序号 + 接口名），机械逆走按
// map 元素解数组失败即跳过；members 为有序数组，按声明序逐条发射即还原原序。

func emitBondsFamily(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error {
	if kpOf(keyPath) == "bonds" {
		if m, ok := val.(map[string]any); ok {
			if arr, ok := m["members"].([]any); ok {
				for _, mem := range arr {
					w.add(toks(prefix, "members", formatScalar(mem)))
				}
			}
		}
		return emitMechanicalInner(w, node, val, prefix, keyPath) // mtu/description 机械；lacp 经再入全显式
	}
	if kpOf(keyPath) == "bonds lacp" {
		if m, ok := val.(map[string]any); ok {
			// interval 挂在 mode 取值关键字之下（值叶子分支不访问兄弟关键字），
			// 机械不可达；语句形态要求 interval 与 mode 同句
			mode, hasMode := m["mode"]
			interval, hasInterval := m["interval"]
			if hasMode && mode != "" {
				t := toks(prefix, "mode", formatScalar(mode))
				if hasInterval && interval != "" {
					t = append(t, "interval", formatScalar(interval))
				}
				w.add(t)
			}
		}
		return nil
	}
	return emitMechanicalInner(w, node, val, prefix, keyPath)
}

// ---------- virtual-switches（最大族，全显式） ----------
//
// 元素层全显式、不委托：ports 元素的 bare 声明由通用逆走（具名数组分支）发出，
// 若再委托 ports 子树，edit 进入端口层级时会与顶层逆走无法区分、bare 双发导致
// 回放报「语句未产生配置变更」。全显式后 ports/gateway 层级只在 edit 进入时触达，
// 各层自行补齐自身层级必需的裸声明。
//
// type l3 语句的别名 apply 会同步建出同名 VRF 空壳条目——vrfs 数组虽是注册盲区，
// 「仅 type l3」的配置仍可完整还原（自复现）；带 l3 数据的配置见文件头盲区说明。

func emitVirtualSwitchFamily(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error {
	switch kpOf(keyPath) {
	case "virtual-switches":
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		if v, ok := m["description"]; ok && v != "" {
			w.add(toks(prefix, "description", formatScalar(v)))
		}
		if v, ok := m["type"]; ok && v != "" {
			w.add(toks(prefix, "type", formatScalar(v)))
		}
		if v, ok := m["vlan_access"].(float64); ok && v != 0 {
			w.add(toks(prefix, "vlan", "access", formatScalar(v)))
		}
		// ports 先于 gateway/cross-connect：端口成员语句经身份消费建出交换机元素，
		// gateway 别名（elemByID）与 cross-connect 校验都要求元素与端口已存在
		var seqs []string
		if arr, ok := m["ports"].([]any); ok {
			for _, e := range arr {
				em, ok := e.(map[string]any)
				if !ok {
					continue
				}
				seqv, ok := em["seq"]
				if !ok {
					continue
				}
				seq := formatScalar(seqv)
				seqs = append(seqs, seq)
				emitVSPort(w, em, toks(prefix, "ports", seq))
			}
		}
		if gw, ok := m["gateway"].(map[string]any); ok {
			if len(gw) == 0 { // 删除残留的空对象：无语句可还原
				w.note("virtual-switches %s：gateway 为空对象（多为删除残留），不入 display set", lastTok(prefix))
			} else {
				emitVSGateway(w, gw, toks(prefix, "gateway"))
			}
		}
		if cc, ok := m["cross_connect"].(bool); ok && cc {
			if len(seqs) == 2 {
				w.add(toks(prefix, "cross-connect", seqs[0], seqs[1]))
			} else { // 别名 apply 校验端口恰两个且已声明，正常建不出此态
				w.note("virtual-switches %s：cross-connect 需要恰好两个已声明端口，无法反推语句", lastTok(prefix))
			}
		}
		return nil
	case "virtual-switches gateway":
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		if len(prefix) > 0 { // edit 进入：补交换机元素裸声明（gateway 别名要求元素已存在）
			w.add(append([]string{}, prefix[:len(prefix)-1]...))
		}
		emitVSGateway(w, m, prefix)
		return nil
	case "virtual-switches ports":
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		w.add(append([]string{}, prefix...)) // 端口序号身份可单独成句，edit 进入时在此补齐
		emitVSPortFields(w, m, prefix)
		return nil
	}
	return nil
}

// emitVSGateway BVI 网关：addresses 是数组而树为可重复的 gateway ip 值叶子；
// vrf/acl-in/acl-out 为透明标量参数（树多一层关键字）。
func emitVSGateway(w *stmtWriter, gw map[string]any, gwToks []string) {
	if arr, ok := gw["addresses"].([]any); ok {
		for _, a := range arr {
			w.add(append(append([]string{}, gwToks...), "ip", formatScalar(a)))
		}
	}
	if v, ok := gw["vrf"]; ok && v != "" {
		w.add(append(append([]string{}, gwToks...), "vrf", formatScalar(v)))
	}
	if v, ok := gw["acl_in"]; ok && v != "" {
		w.add(append(append([]string{}, gwToks...), "acl-in", formatScalar(v)))
	}
	if v, ok := gw["acl_out"]; ok && v != "" {
		w.add(append(append([]string{}, gwToks...), "acl-out", formatScalar(v)))
	}
}

// emitVSPort 端口元素：bare + 成员字段（顶层逆走与 edit 进入共用字段逻辑）。
func emitVSPort(w *stmtWriter, em map[string]any, portToks []string) {
	w.add(append([]string{}, portToks...)) // 端口序号身份可单独成句
	emitVSPortFields(w, em, portToks)
}

func emitVSPortFields(w *stmtWriter, em map[string]any, portToks []string) {
	label := lastTok(portToks)
	// 成员三选一：vnf / container / 物理口。vnf、container 的 6 token 形态在别名表里
	// 固定按 delete 语义处理，set 必须带 interface——缺 vNIC 名的配置无语句可还原。
	if vnf, ok := em["vnf"]; ok && vnf != "" {
		nic, _ := em["vnf_interface"].(string)
		if nic == "" {
			w.note("virtual-switches 端口 %s：vnf 成员缺少 vNIC 名（set 语法必须带 interface），无法反推", label)
			return
		}
		t := toks(portToks, "vnf", formatScalar(vnf), "interface", nic)
		if tr, ok := em["trunk"].([]any); ok && len(tr) > 0 {
			t = append(t, "trunk", "vlans", joinScalars(tr, ","))
		}
		w.add(t)
		return
	}
	if ct, ok := em["container"]; ok && ct != "" {
		nic, _ := em["container_interface"].(string)
		if nic == "" {
			w.note("virtual-switches 端口 %s：container 成员缺少 vNIC 名（set 语法必须带 interface），无法反推", label)
			return
		}
		w.add(toks(portToks, "container", formatScalar(ct), "interface", nic))
		return
	}
	if ifc, ok := em["interface"]; ok && ifc != "" {
		tr, hasTrunk := em["trunk"].([]any)
		nv, hasNative := em["native"].(float64)
		if hasTrunk && len(tr) > 0 {
			w.add(toks(portToks, "interface", formatScalar(ifc), "trunk", "vlans", joinScalars(tr, ",")))
		}
		if hasNative && nv != 0 {
			w.add(toks(portToks, "interface", formatScalar(ifc), "native", formatScalar(nv)))
		}
		if (!hasTrunk || len(tr) == 0) && (!hasNative || nv == 0) {
			w.add(toks(portToks, "interface", formatScalar(ifc)))
		}
		return
	}
	// 无成员的端口（仅序号）：bare 已发，合法形态
	for _, kv := range []struct{ key, why string }{
		{"trunk", "trunk 缺少成员接口"},
		{"native", "native 缺少成员接口"},
		{"acl_in", "端口级入向 ACL 绑定无语句关键字"},
		{"acl_out", "端口级出向 ACL 绑定无语句关键字"},
	} {
		if v, ok := em[kv.key]; ok && v != "" {
			w.note("virtual-switches 端口 %s：%s，无法反推", label, kv.why)
		}
	}
}

// ---------- acls ----------
//
// rule 关键字对应 JSON 键 rules（单复数不一致），机械逆走查不到 "rule" 键即跳过；
// 规则字段与树关键字同名，按固定字段序发射保证输出确定。

func emitAclsFamily(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error {
	m, ok := val.(map[string]any)
	if !ok {
		return nil
	}
	if d, ok := m["description"]; ok && d != "" { // 模型字段无语句关键字
		w.note("acls %s：描述字段无语句关键字，不入 display set", lastTok(prefix))
	}
	if arr, ok := m["rules"].([]any); ok {
		for _, e := range arr {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			seqv, ok := em["seq"]
			if !ok {
				continue
			}
			ruleToks := toks(prefix, "rule", formatScalar(seqv))
			w.add(append([]string{}, ruleToks...)) // seq 身份可单独成句（别名按「仅 seq」建规则）
			for _, kv := range []struct{ key, kw string }{
				{"source", "source"},
				{"destination", "destination"},
				{"protocol", "protocol"},
				{"source_port", "source-port"},
				{"destination_port", "destination-port"},
				{"action", "action"},
				{"direction", "direction"},
			} {
				if v, ok := em[kv.key]; ok && v != "" {
					w.add(append(append([]string{}, ruleToks...), kv.kw, formatScalar(v)))
				}
			}
		}
	}
	return nil
}

// ---------- nat ----------
//
// source_pools/rules/static 的身份字段与树建模均有出入（source_pool⇄source_pools、
// static 无 name 身份），全显式发射、不委托；rules/action 层级仅供 edit 进入。

func emitNatFamily(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error {
	switch kpOf(keyPath) {
	case "nat":
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		if arr, ok := m["source_pools"].([]any); ok {
			for _, e := range arr {
				em, ok := e.(map[string]any)
				if !ok {
					continue
				}
				name, _ := em["name"].(string)
				ar, _ := em["address_range"].(string)
				a, b, found := strings.Cut(ar, " to ")
				if !found { // 别名 apply 恒写「a to b」形态，缺失即异常配置
					w.note("nat source-pool %s：地址范围缺少「 to 」分隔，无法反推语句", name)
					continue
				}
				w.add(toks(prefix, "source-pool", name, "address-range", a, "to", b))
			}
		}
		if arr, ok := m["rules"].([]any); ok {
			for _, e := range arr {
				em, ok := e.(map[string]any)
				if !ok {
					continue
				}
				seqv, ok := em["seq"]
				if !ok {
					continue
				}
				emitNatRule(w, em, toks(prefix, "rules", formatScalar(seqv)))
			}
		}
		if arr, ok := m["static"].([]any); ok {
			for _, e := range arr {
				em, ok := e.(map[string]any)
				if !ok {
					continue
				}
				in, _ := em["inside_ip"].(string)
				out, _ := em["outside_ip"].(string)
				w.add(toks(prefix, "static", in, "to", out))
			}
		}
		return nil
	case "nat rules":
		if m, ok := val.(map[string]any); ok {
			emitNatRule(w, m, prefix) // prefix 已含 rules 与规则序号
		}
		return nil
	case "nat rules action":
		if m, ok := val.(map[string]any); ok {
			if len(prefix) >= 3 { // 补规则层裸声明（edit 深入 action 层时规则须先存在）
				w.add(append([]string{}, prefix[:len(prefix)-1]...))
			}
			emitNatActionFields(w, m, prefix)
		}
		return nil
	}
	return nil
}

// emitNatRule 单条转换规则：裸声明 + 匹配条件 + 动作。match_source/virtual_switch
// 按树语法合成一条 match 语句；action 空对象随裸声明落库（别名 apply 恒建空 action）。
func emitNatRule(w *stmtWriter, em map[string]any, ruleToks []string) {
	w.add(append([]string{}, ruleToks...))
	ms, hasMS := em["match_source"]
	vs, hasVS := em["virtual_switch"]
	switch {
	case hasMS && ms != "":
		t := toks(ruleToks, "match", "source", formatScalar(ms))
		if hasVS && vs != "" {
			t = append(t, "virtual-switch", formatScalar(vs))
		}
		w.add(t)
	case hasVS && vs != "":
		w.add(toks(ruleToks, "virtual-switch", formatScalar(vs)))
	}
	if act, ok := em["action"].(map[string]any); ok {
		emitNatActionFields(w, act, append(append([]string{}, ruleToks...), "action"))
	}
}

// emitNatActionFields 动作对象：source-pool/interface 为透明标量参数（可并存，
// 别名 apply 校验语义由 commit 承担）。
func emitNatActionFields(w *stmtWriter, act map[string]any, actToks []string) {
	if v, ok := act["source_pool"]; ok && v != "" {
		w.add(append(append([]string{}, actToks...), "source-pool", formatScalar(v)))
	}
	if v, ok := act["interface"]; ok && v != "" {
		w.add(append(append([]string{}, actToks...), "interface", formatScalar(v)))
	}
}

// ---------- port-mirroring ----------
//
// analyzer 是扁平字符串（树多一层 interface 关键字），source.direction 挂在取值
// 关键字之下机械不可达，全显式；source 层级仅供 edit 进入。

func emitPortMirroringFamily(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error {
	switch kpOf(keyPath) {
	case "port-mirroring":
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		if src, ok := m["source"].(map[string]any); ok {
			emitPMSource(w, src, toks(prefix, "source"))
		}
		if an, ok := m["analyzer"]; ok && an != "" {
			w.add(toks(prefix, "analyzer", "interface", formatScalar(an)))
		}
		return nil
	case "port-mirroring source":
		if m, ok := val.(map[string]any); ok {
			emitPMSource(w, m, prefix)
		}
		return nil
	}
	return nil
}

// emitPMSource 镜像源：物理口 / VNF vNIC 两形态 + 可选 direction（尾随同一语句）。
func emitPMSource(w *stmtWriter, src map[string]any, srcToks []string) {
	dir, _ := src["direction"].(string)
	if vnf, ok := src["vnf"]; ok && vnf != "" {
		t := toks(srcToks, "vnf", formatScalar(vnf))
		if nic, ok := src["vnf_interface"]; ok && nic != "" {
			t = append(t, "interface", formatScalar(nic))
		}
		if dir != "" {
			t = append(t, "direction", dir)
		}
		w.add(t)
		return
	}
	if ifc, ok := src["interface"]; ok && ifc != "" {
		t := toks(srcToks, "interface", formatScalar(ifc))
		if dir != "" {
			t = append(t, "direction", dir)
		}
		w.add(t)
		return
	}
	if dir != "" { // 仅方向的源（别名解析支持）
		w.add(toks(srcToks, "direction", dir))
		return
	}
	if _, ok := src["vnf_interface"]; ok { // vnf_interface 无 vnf：别名建不出此态
		w.note("port-mirroring %s：source 存在 vnf_interface 却无 vnf，无法反推", lastTok(srcToks))
	}
}

// ---------- resource-pools ----------
//
// hugepages（IVK 身份取值）与 isolated-cores 机械可达；numa 数组的 node 关键字下
// 才是具名数组建模、机械逆走查不到 "node" 键即跳过，须显式发射。

func emitResourcePoolsFamily(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error {
	switch kpOf(keyPath) {
	case "resource-pools", "resource-pools cpu":
		return emitMechanicalInner(w, node, val, prefix, keyPath)
	case "resource-pools cpu numa":
		switch v := val.(type) {
		case []any: // 顶层逆走：numa 关键字以整个数组进入（分支 7）
			for _, e := range v {
				if em, ok := e.(map[string]any); ok {
					emitNumaNode(w, em, prefix)
				}
			}
		case map[string]any: // edit 进入元素层级：prefix 末位是节点号取值（身份不入关键字路径）
			if len(prefix) > 0 {
				emitNumaNode(w, v, prefix[:len(prefix)-1])
			}
		}
		return nil
	default: // hugepages page-size 元素层级（edit 不可达、委托再入）：count 机械可达
		return emitMechanicalInner(w, node, val, prefix, keyPath)
	}
}

func emitNumaNode(w *stmtWriter, em map[string]any, numaToks []string) {
	node, ok := em["node"]
	if !ok {
		return
	}
	t := toks(numaToks, "node", formatScalar(node))
	if cores, ok := em["cores"].([]any); ok && len(cores) > 0 {
		t = append(t, "cores", joinScalars(cores, ",")) // core-list 逆格式化，expandCores 可还原
	}
	w.add(t)
}

// ---------- vpp dpdk ----------
//
// dev 全局默认在模型里是对象、树按「具名数组容器 + 透明参数」建模，机械逆走按数组
// 解对象失败即跳过；per_dev 无树关键字。uio-driver 机械可达（委托）。

func emitVppDpdkFamily(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error {
	switch kpOf(keyPath) {
	case "vpp dpdk":
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		if dev, ok := m["dev"].(map[string]any); ok {
			emitDpdkDevFields(w, dev, toks(prefix, "dev"))
		}
		if arr, ok := m["per_dev"].([]any); ok {
			for _, e := range arr {
				em, ok := e.(map[string]any)
				if !ok {
					continue
				}
				ifc, ok := em["interface"]
				if !ok {
					continue
				}
				// 先发创建语句本身（`vpp dpdk dev <if>` 4-token 即建 per_dev 条目）——
				// 真机配置里 {interface: ens192} 这种「只有身份」的条目就是它建的，
				// 漏发会让整个 dpdk 块在回放中消失（round81 真机实测抓到）。
				w.add(toks(prefix, "dev", formatScalar(ifc)))
				emitDpdkDevFields(w, em, toks(prefix, "dev", formatScalar(ifc)))
			}
		}
		return emitMechanicalInner(w, node, val, prefix, keyPath) // uio-driver
	default:
		// vpp dpdk dev（edit 进入的全局默认对象）：rx-queues 等值叶子机械可达
		return emitMechanicalInner(w, node, val, prefix, keyPath)
	}
}

func emitDpdkDevFields(w *stmtWriter, dev map[string]any, devToks []string) {
	for _, kv := range []struct{ key, kw string }{
		{"rx_queues", "rx-queues"},
		{"tx_queues", "tx-queues"},
		{"rx_descriptors", "rx-descriptors"},
		{"tx_descriptors", "tx-descriptors"},
	} {
		if v, ok := dev[kv.key]; ok && v != "" {
			w.add(append(append([]string{}, devToks...), kv.kw, formatScalar(v)))
		}
	}
}

// ---------- virtual-machine-functions ----------
//
// vcpu/disks/cloud-init/interfaces 机械可达（委托）；memory.numa（树为 memory→numa→node
// 三层、JSON 键扁平 numa_node）与 serial_console（树为 serial→console→enable、JSON 扁平
// 指针布尔）须显式；sriov.vf_id 的语句关键字落点为 "vf"、与模型键不一致，语句无法落到模型。

func emitVMFamily(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error {
	switch kpOf(keyPath) {
	case "virtual-machine-functions":
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		if sc, ok := m["serial_console"]; ok {
			if b, _ := sc.(bool); b {
				w.add(toks(prefix, "serial", "console", "enable"))
			} else { // set 语法只能置位（delete 置否），关闭态无语句可还原
				w.note("virtual-machine-functions %s：serial console 关闭态没有 set 语句形态，不入 display set", lastTok(prefix))
			}
		}
		return emitMechanicalInner(w, node, withoutZeroScalars(m), prefix, keyPath)
	case "virtual-machine-functions memory":
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		if v, ok := m["numa_node"]; ok && v != nil {
			w.add(toks(prefix, "numa", "node", formatScalar(v)))
		}
		return emitMechanicalInner(w, node, withoutZeroScalars(m), prefix, keyPath) // size-mb:0 等零值自复现
	case "virtual-machine-functions interfaces sriov":
		if m, ok := val.(map[string]any); ok {
			if vf, ok := m["vf_id"].(float64); ok && vf != 0 {
				w.note("virtual-machine-functions 接口 %s：SR-IOV VF 编号无可用 set 语句（语句关键字与模型字段不一致），不入 display set", lastTok(prefix[:len(prefix)-1]))
			}
			return emitMechanicalInner(w, node, withoutZeroScalars(m), prefix, keyPath) // physical-interface 机械可达
		}
		return nil
	default:
		if m, ok := val.(map[string]any); ok {
			// vcpu/disks/cloud-init/interfaces 元素等：零值剥离后全部机械可达
			// （如 vNIC 的 type:"" 无 omitempty，发空值语句回放即失败）
			return emitMechanicalInner(w, node, withoutZeroScalars(m), prefix, keyPath)
		}
		return nil
	}
}

// ---------- container-functions ----------
//
// env 是 map 而语句树按「具名数组」建模（键序无关、按名排序输出）；args 是 []string
// 而树是单个 string 值叶子——机械逆走会把序列化数组当一个值写出，故元素层剥掉 args
// 再委托其余机械子区域，args 按别名语义整组一条语句发射。vcpu 是扁平整数且 JSON 键
// 同名，经分支 7 以数值进入下方 case；memory_mb 键名不同（树为 memory→size-mb），
// 只能在元素层显式发射。

func emitContainerFamily(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error {
	switch kpOf(keyPath) {
	case "container-functions":
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		// memory_mb 扁平整数（树为 memory→size-mb 两层）：通用逆走查不到 "memory" 键
		// 即跳过，须在本层显式发射；vcpu 键同名，仍经分支 7 以数值进入下方 case
		if v, ok := m["memory_mb"].(float64); ok && v != 0 {
			w.add(toks(prefix, "memory", "size-mb", formatScalar(v)))
		}
		if env, ok := m["env"].(map[string]any); ok {
			for _, k := range sortedKeys(env) {
				w.add(toks(prefix, "env", k, formatScalar(env[k])))
			}
		}
		if arr, ok := m["args"].([]any); ok && len(arr) > 0 {
			t := toks(prefix, "args")
			for _, a := range arr {
				t = append(t, formatScalar(a))
			}
			w.add(t)
		}
		filtered := withoutZeroScalars(m)
		delete(filtered, "args") // 值叶子分支会把 []string 序列化成 JSON 数组字面量，必须剥离
		return emitMechanicalInner(w, node, filtered, prefix, keyPath)
	case "container-functions env":
		if m, ok := val.(map[string]any); ok {
			for _, k := range sortedKeys(m) {
				w.add(append(append([]string{}, prefix...), k, formatScalar(m[k])))
			}
		}
		return nil
	case "container-functions vcpu":
		if v, ok := val.(float64); ok && v != 0 {
			w.add(toks(prefix, "count", formatScalar(v)))
		}
		return nil
	default: // interfaces 元素等：零值剥离（vNIC type:"" 无 omitempty）后 type/virtual-switch/mac/vlan 机械可达
		if m, ok := val.(map[string]any); ok {
			return emitMechanicalInner(w, node, withoutZeroScalars(m), prefix, keyPath)
		}
		return nil
	}
}

// sortedKeys map 键升序（输出确定性）。
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------- 根级回落发射器（决策 #155 补充） ----------
//
// vrfs / qos_policies 的模型键在命令树里没有对应顶层关键字（VRF 由
// `virtual-switches <n> type l3` 派生、qos 由 `qos policies` 别名落库），
// 通用逆走与家族注册表都不可达，由根层未消费键的回落钩子接管。

func init() {
	rootFallbackEmitters["vrfs"] = emitVrfsFallback
	rootFallbackEmitters["qos_policies"] = emitQosFallback
}

// emitVrfsFallback 把 vrfs[]（L3 数据落点，VRF 名 = 同名虚拟交换机）反推为
// `virtual-switches <名> l3-interface …` / `static-routes …` 语句。回放依赖
// vs 侧已发出 `type l3`（家族发射器先于根层回落执行，顺序成立）。
func emitVrfsFallback(w *stmtWriter, val any) error {
	arr, ok := val.([]any)
	if !ok {
		return nil
	}
	for _, el := range arr {
		vrf, ok := el.(map[string]any)
		if !ok {
			continue
		}
		name, _ := vrf["name"].(string)
		if name == "" {
			continue
		}
		// description 仅 REST 可达（无语句形态），出现时由回放校验如实报错
		if lis, ok := vrf["l3_interfaces"].([]any); ok {
			for _, l := range lis {
				li, ok := l.(map[string]any)
				if !ok {
					continue
				}
				iface, _ := li["interface"].(string)
				if iface == "" {
					continue
				}
				p := []string{"virtual-switches", name, "l3-interface", iface}
				if acl, ok := li["acl_in"].(string); ok && acl != "" {
					w.add(append(append([]string{}, p...), "acl-in", acl))
				}
				addrs, _ := li["addresses"].([]any)
				for _, a := range addrs {
					w.add(append(append([]string{}, p...), "ip", "address", formatScalar(a)))
				}
			}
		}
		if rts, ok := vrf["routes"].([]any); ok {
			for _, r := range rts {
				rt, ok := r.(map[string]any)
				if !ok {
					continue
				}
				prefix, _ := rt["prefix"].(string)
				nh, _ := rt["next_hop"].(string)
				if prefix == "" || nh == "" {
					continue
				}
				w.add([]string{"virtual-switches", name, "static-routes", prefix, "next-hop", nh})
				if d, ok := rt["distance"].(float64); ok && d != 0 {
					w.add([]string{"virtual-switches", name, "static-routes", prefix, "distance", formatScalar(d)})
				}
			}
		}
	}
	return nil
}

// emitQosFallback 把 qos_policies[] 反推为 `qos policies <名> cir <bps> cbs <bytes>`
// （别名 ensureElem 自建元素；两值都发，零值不发——cir/cbs 为 0 时语句本就建不出该值）。
func emitQosFallback(w *stmtWriter, val any) error {
	arr, ok := val.([]any)
	if !ok {
		return nil
	}
	for _, el := range arr {
		pol, ok := el.(map[string]any)
		if !ok {
			continue
		}
		name, _ := pol["name"].(string)
		if name == "" {
			continue
		}
		cir, hasCir := pol["cir"].(float64)
		cbs, hasCbs := pol["cbs"].(float64)
		if !hasCir && !hasCbs {
			continue
		}
		toks := []string{"qos", "policies", name}
		if hasCir {
			toks = append(toks, "cir", formatScalar(cir))
		}
		if hasCbs {
			toks = append(toks, "cbs", formatScalar(cbs))
		}
		w.add(toks)
	}
	return nil
}
