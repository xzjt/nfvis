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
//     任何注册键都无法被 keyPath 命中，由根层回落发射器接管（emitVrfsFallback）。决策
//     #452 起该回落**自足**：每个 VRF 先发一条确定性的 `type l3` 创建语句（含空 VRF），
//     按 vrfs[] 顺序建表；顶层 `type l3`（emitVirtualSwitchFamily）让位到此处，不双发。
//   - qos_policies 数组（qos policies 语句落点）：树顶层关键字为 qos、JSON 键为
//     qos_policies，通用逆走在根层查不到 "qos" 键即跳过，同上无注册键可达。

import (
	"fmt"
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
	subtreeEmitters["vpp"] = emitVppFamily
	subtreeEmitters["virtual-machine-functions"] = emitVMFamily
	subtreeEmitters["container-functions"] = emitContainerFamily
	subtreeEmitters["vm-image-profiles"] = emitVMImageProfilesFamily
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
	case "system metrics", "system metrics history":
		// 历史时序存储（决策 #356）：模型键 interval_seconds/retention_days 与树叶子
		// interval/retention-days 词形不同、且多一层 history，机械逆走查不到，须显式发射。
		// 两字段均为 omitempty：0/未设置不发射（空 metrics 对象不产生任何语句）。
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		if kpOf(keyPath) == "system metrics" { // 顶层进入：val 是 metrics 对象，history 在下一层
			h, ok := m["history"].(map[string]any)
			if !ok {
				return nil
			}
			m, prefix = h, toks(prefix, "history")
		}
		if v, ok := m["interval_seconds"].(float64); ok && v != 0 {
			w.add(toks(prefix, "interval", formatScalar(v)))
		}
		if v, ok := m["retention_days"].(float64); ok && v != 0 {
			w.add(toks(prefix, "retention-days", formatScalar(v)))
		}
		return nil
	case "system firewall":
		// 管理面主机防火墙（决策 #388）：default_policy 与规则的字段名虽与树关键字同形，
		// 但规则数组的 JSON 键是 `rules`、树关键字是 `rule`（单复数不一致，机械逆走查不到）；
		// 显式发射。规则按**数组声明序**发射（不按 seq 排序）——回放按语句序重建数组，
		// 声明序才能还原原数组（seq 升序只是数据面生效序，见 RenderFirewallScript 的排序）。
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		if v, ok := m["default_policy"]; ok && v != "" {
			w.add(toks(prefix, "default-policy", formatScalar(v)))
		}
		arr, _ := m["rules"].([]any)
		for _, e := range arr {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if err := emitFirewallRuleStmt(w, em, prefix); err != nil {
				return err
			}
		}
		return nil
	default:
		// system kernel（全机械）、system login password-policy（值叶子机械）等
		return emitMechanicalInner(w, node, val, prefix, keyPath)
	}
}

// emitFirewallRuleStmt 发射单条主机防火墙规则（决策 #388）：字段序固定
// （action → source → protocol → port），回放由别名解析、与数组声明序一一对应。
func emitFirewallRuleStmt(w *stmtWriter, em map[string]any, prefix []string) error {
	seq, ok := em["seq"]
	if !ok {
		return nil
	}
	action, ok := em["action"]
	if !ok || action == "" {
		return fmt.Errorf("display set 内部错误：防火墙规则 seq=%v 缺少 action（提交校验本应拦下）", seq)
	}
	t := toks(prefix, "rule", formatScalar(seq), "action", formatScalar(action))
	if v, ok := em["source"]; ok && v != "" {
		t = append(t, "source", formatScalar(v))
	}
	if v, ok := em["protocol"]; ok && v != "" {
		t = append(t, "protocol", formatScalar(v))
	}
	if v, ok := em["port"]; ok {
		t = append(t, "port", formatScalar(v))
	}
	w.add(t)
	return nil
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
// ingress_policy / egress_policy 是字符串而语句树按「具名数组容器」建模
// （ingress-policy <name> / egress-policy <name>，决策 #331 加出向），机械逆走按数组解容器
// 失败即跳过；storm_control 是单对象而模型键是 broadcast_kbps/multicast_kbps（关键字
// broadcast/multicast 机械逆走查不到，决策 #385），同样显式发射；sriov 子对象机械可达
// （经委托再入本发射器后委托）。

func emitInterfacesFamily(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error {
	if kpOf(keyPath) == "interfaces" {
		if m, ok := val.(map[string]any); ok {
			for _, p := range []struct{ key, kw string }{
				{"ingress_policy", "ingress-policy"},
				{"egress_policy", "egress-policy"},
			} {
				if v, ok := m[p.key]; ok && v != "" {
					w.add(toks(prefix, p.kw, formatScalar(v)))
				}
			}
			// 风暴抑制（决策 #385）：每类一条语句；固定顺序 broadcast → multicast
			//（与声明序无关，保证同配置两次生成逐字相同）。
			if sc, ok := m["storm_control"].(map[string]any); ok {
				for _, p := range []struct{ key, kw string }{
					{"broadcast_kbps", "broadcast"},
					{"multicast_kbps", "multicast"},
				} {
					if v, ok := sc[p.key].(float64); ok && v != 0 {
						w.add(toks(prefix, "storm-control", p.kw, formatScalar(v)))
					}
				}
			}
			// 端口安全（决策 #389）：port_security 是 []string 数组而语句树按
			// `port-security mac <mac>` 关键字两层建模，机械逆走解不出，显式发射；
			// 按声明序逐条发射即还原原序（回放经别名表按序追加，往返自校验据此对账）。
			if ps, ok := m["port_security"].([]any); ok {
				for _, v := range ps {
					if s, ok := v.(string); ok && s != "" {
						w.add(toks(prefix, "port-security", "mac", s))
					}
				}
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
// type l3 语句的别名 apply 会同步建出同名 VRF 空壳条目——但顶层整配置逆走时 `type l3`
// 的创建语句**让位给根层 vrfs 回落发射器**（emitVrfsFallback 按 vrfs[] 顺序自足发出，
// 决策 #452），本发射器只在层级 show（无前置裸声明）时就地发出，避免双发/顺序错乱。

func emitVirtualSwitchFamily(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error {
	switch kpOf(keyPath) {
	case "virtual-switches":
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		// 描述字段无语句关键字：命令树与别名表都没有 `virtual-switches <n> description <v>`
		// 的形态（该关键字仅 interfaces/bonds/VM/容器有），照发语句会被回放自校验打回
		// 「未知语句」、整条 display set 读不出来——只发注释说明（同 acls 描述先例），
		// 回放比较两侧按 stripNonEmittable 同剥。
		if v, ok := m["description"]; ok && v != "" {
			w.note("virtual-switches %s：描述字段无语句关键字，不入 display set", lastTok(prefix))
		}
		if v, ok := m["type"]; ok && v != "" {
			// 决策 #452：顶层整配置逆走时，`type l3` 的创建语句让位给根层 vrfs 回落发射器
			//（emitVrfsFallback 按 vrfs[] 顺序发出、且自足），此处不双发——既避免与回落重复，
			// 又让回放后的 vrfs[] 顺序与 committed 一致（此前依赖 `type l3` 别名的副作用建
			// VRF，顺序由 virtual_switches[] 决定，与 vrfs[] 声明序不一致即回放不等）。
			// 层级 show（edit 层级直接进入本发射器，没有顶层具名数组分支的前置裸声明）仍就地
			// 发出 type l3，保证 `edit virtual-switches <n>` 下的 display set 可还原。
			if v == "l3" && lastStmtIs(w, prefix) {
				// 让位（创建语句见 emitVrfsFallback）
			} else {
				w.add(toks(prefix, "type", formatScalar(v)))
			}
		}
		if v, ok := m["vlan_access"].(float64); ok && v != 0 {
			w.add(toks(prefix, "vlan", "access", formatScalar(v)))
		}
		// ports 先于 gateway：端口成员语句经身份消费建出交换机元素（若该元素在裸声明里
		// 还没建出来），gateway 的别名发射器（elemByID）要求元素已存在
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
				emitVSPort(w, em, toks(prefix, "ports", formatScalar(seqv)))
			}
		}
		if gw, ok := m["gateway"].(map[string]any); ok {
			if len(gw) == 0 { // 删除残留的空对象：无语句可还原
				w.note("virtual-switches %s：gateway 为空对象（多为删除残留），不入 display set", lastTok(prefix))
			} else {
				emitVSGateway(w, gw, toks(prefix, "gateway"))
			}
		}
		// DHCP 中继（决策 #335）：模型单值字符串，语句树是 dhcp-relay server <ip> 两层关键字
		if v, ok := m["dhcp_relay_server"]; ok && v != "" {
			w.add(toks(prefix, "dhcp-relay", "server", formatScalar(v)))
		}
		// DHCP 服务器（决策 #359）：模型 5 个平铺键，语句树是 dhcp-server <leaf> 两层关键字。
		// pool 必须两键齐备才启用，反推为一行两值的 pool 语句；可选叶子各自一行。
		// （平铺键删空后不留 `dhcp_server` 空壳，此处无需剪壳。）
		poolStart, okStart := m["dhcp_server_pool_start"].(string)
		poolEnd, okEnd := m["dhcp_server_pool_end"].(string)
		if okStart && okEnd && poolStart != "" && poolEnd != "" {
			w.add(toks(prefix, "dhcp-server", "pool", poolStart, poolEnd))
		} else if (okStart && poolStart != "") || (okEnd && poolEnd != "") {
			// 模型校验要求两键同时给，正常建不出此态；只给一个时如实说明无法反推。
			w.note("virtual-switches %s：DHCP 服务器租约池只配置了一个端点（须同时给），无法反推语句", lastTok(prefix))
		}
		if v, ok := m["dhcp_server_lease_time_seconds"].(float64); ok && v != 0 {
			w.add(toks(prefix, "dhcp-server", "lease-time", formatScalar(v)))
		}
		if v, ok := m["dhcp_server_dns"]; ok && v != "" {
			w.add(toks(prefix, "dhcp-server", "dns", formatScalar(v)))
		}
		if v, ok := m["dhcp_server_domain_name"]; ok && v != "" {
			w.add(toks(prefix, "dhcp-server", "domain-name", formatScalar(v)))
		}
		// MAC 学习条数上限（决策 #337）：模型单值整数，语句树是单层关键字 learn-limit <n>
		if v, ok := m["learn_limit"].(float64); ok && v != 0 {
			w.add(toks(prefix, "learn-limit", formatScalar(v)))
		}
		// 数据面 DNS 代理的按域上游（决策 #345）：模型数组，语句树是 dns→proxy→server <ip> 三层
		// （与别名 aliasVSDNSProxy* 对偶）。元素此时已由上面的 type 语句建出（别名 elemByID 要求）。
		if arr, ok := m["dns_proxy_servers"].([]any); ok {
			for _, ip := range arr {
				w.add(toks(prefix, "dns", "proxy", "server", formatScalar(ip)))
			}
		}
		// 两端口直通开关：模型 bool，语句树同形（`cross-connect <true|false>` 显式取值叶子）。
		// 端口不入语句——它们由该交换机的 ports 列表承担（VPP 侧取前两个），
		// 因此这里既有形态可直接机械反推，无需端口就绪前置。
		if cc, ok := m["cross_connect"].(bool); ok && cc {
			w.add(toks(prefix, "cross-connect", "true"))
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
	cores, _ := em["cores"].([]any)
	if len(cores) == 0 {
		// 空/缺核列表：`numa node <n>` 不带 cores 回放即失败（core-list 是语句必需取值），
		// 该元素无语句可还原——只注释说明，回放比较两侧同剥该元素（nonEmittableElem）。
		w.note("resource-pools cpu numa node %s：核列表为空，无语句可还原（不入 display set）", formatScalar(node))
		return
	}
	t := toks(numaToks, "node", formatScalar(node))
	t = append(t, "cores", joinScalars(cores, ",")) // core-list 逆格式化，expandCores 可还原
	w.add(t)
}

// ---------- vpp（dns-proxy 显式；cpu/memory/plugins 机械，dpdk 委托 dpdk 发射器） ----------
//
// 决策 #345 后注册键为 "vpp"（原先只注册 "vpp dpdk"，与 "vpp" 前缀重叠会导致命中不确定，
// 故合并为一处）：dns_proxy_servers 扁平在 vpp 下、而语句树在 system→dns→proxy 下，
// 机械逆走查不到 "dns_proxy_servers" 键即跳过，须在本层显式发射为 `set system dns proxy server <ip>`
// （绝对语句前缀，与别名 cli_aliases_array.go 的 dnsProxyApply* 对偶；回放自校验据此验证还原性）。
func emitVppFamily(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error {
	switch kpOf(keyPath) {
	case "vpp":
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		if arr, ok := m["dns_proxy_servers"].([]any); ok {
			for _, ip := range arr {
				// 语句落在 system 族（与既有 dns server 同形），故用绝对前缀而非 prefix。
				w.add(append([]string{"system", "dns", "proxy", "server"}, formatScalar(ip)))
			}
		}
		// dpdk 子族显式接管（原 "vpp dpdk" 注册键的处理器）：dev 全局默认为对象而树按数组建模，
		// 机械逆走会失败，必须在此调用 dpdk 发射器；其余键（cpu/memory/plugins）机械可达。
		if dm, ok := m["dpdk"].(map[string]any); ok {
			if dn := childKeyword(node, "dpdk"); dn != nil {
				if err := emitVppDpdkFamily(w, dn, dm, toks(prefix, "dpdk"),
					append(append([]string{}, keyPath...), "dpdk")); err != nil {
					return err
				}
			}
			filtered := make(map[string]any, len(m))
			for k, v := range m {
				if k != "dpdk" {
					filtered[k] = v
				}
			}
			return emitMechanicalInner(w, node, filtered, prefix, keyPath)
		}
		return emitMechanicalInner(w, node, m, prefix, keyPath)
	case "vpp dpdk":
		return emitVppDpdkFamily(w, node, val, prefix, keyPath)
	default:
		// vpp cpu / memory / plugins（edit 进入的层级）与 dpdk 内层级：机械可达
		return emitMechanicalInner(w, node, val, prefix, keyPath)
	}
}

// childKeyword 在 n 的直接子节点里按名取关键字节点（供发射器显式接管子族）。
func childKeyword(n *schema.Node, name string) *schema.Node {
	if n == nil {
		return nil
	}
	for _, c := range n.Children {
		if c.Kind == schema.Keyword && c.Name == name {
			return c
		}
	}
	return nil
}

// emitVppDpdkFamily：vpp dpdk 子族（dev 全局默认对象 / per_dev 无树关键字；uio-driver 机械可达）。
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
		if err := emitMechanicalInner(w, node, withoutZeroScalars(m), prefix, keyPath); err != nil {
			return err
		}
		// PCIDevices：JSON 键 pci_devices、树关键字 pci-device，机械逆走按同名键查不到，
		// 这里**末尾追加**按声明序逐条发射——回放走别名追加语义，保持同一顺序，
		// 深比较才不会因数组顺序不同而失败（FR-CMP-023）。
		if arr, ok := m["pci_devices"].([]any); ok {
			for _, el := range arr {
				s, ok := el.(string)
				if !ok || s == "" {
					continue
				}
				w.add(toks(prefix, "pci-device", s))
			}
		}
		return nil
	case "virtual-machine-functions memory":
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		if v, ok := m["numa_node"]; ok && v != nil {
			w.add(toks(prefix, "numa", "node", formatScalar(v)))
		}
		return emitMechanicalInner(w, node, withoutZeroScalars(m), prefix, keyPath) // size-mb:0 等零值自复现
	case "virtual-machine-functions startup":
		// 实例级启动画像覆盖（决策 #485）：与镜像画像同族——模型平铺 cpu_mode/cpu_model，
		// 语句树是 startup→cpu→mode/model 两层，机械逆走查不到 "cpu" 键即跳过，须显式渲染。
		// 其余平铺叶子（machine/firmware/disk-interface/boot-order/accel/console/extra-args）
		// 与树关键字同形，委托通用逆走即可。
		m, ok := val.(map[string]any)
		if !ok {
			return nil
		}
		// 配置模式层级 show（`edit virtual-machine-functions <n> startup`）无前置裸声明，
		// 而 startup 的 cpu 别名（elemByID）要求属主元素已存在——就地补发 VM 创建语句；
		// 顶层逆走已由具名数组分支先行发出（不双发）。
		if len(prefix) >= 2 {
			emitElemIfAbsent(w, prefix[:len(prefix)-1])
		}
		emitCPUModeModel(w, m, prefix)
		return emitMechanicalInner(w, node, withoutZeroScalars(m), prefix, keyPath)
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

// ---------- vm-image-profiles（镜像启动画像，决策 #485） ----------
//
// cpu 在语句树里是**对象容器**（cpu→mode/model 两层），模型却是平铺的
// cpu_mode/cpu_model；通用逆走按 jsonKeyOf 查 "cpu" 键查不到即整棵子树跳过（回放
// 自校验会如实报「未能完整还原」）。故本族显式把平铺字段渲染成 `cpu mode …` /
// `cpu model …`，其余平铺叶子（machine/firmware/disk-interface/boot-order/accel/
// console/extra-args）与树关键字同形（横线转下划线），委托通用逆走即可。

func emitVMImageProfilesFamily(w *stmtWriter, node *schema.Node, val any, prefix, keyPath []string) error {
	m, ok := val.(map[string]any)
	if !ok {
		return nil
	}
	// 配置模式层级 show（`edit vm-image-profiles <img>`）：没有通用逆走具名数组分支的
	// 前置裸声明，而 cpu 别名（elemByID）要求元素已存在——就地补发创建语句；顶层逆走已
	// 由前置裸声明建出（不双发，见 elemEmitted）。
	emitElemIfAbsent(w, prefix)
	emitCPUModeModel(w, m, prefix)
	return emitMechanicalInner(w, node, withoutZeroScalars(m), prefix, keyPath)
}

// emitElemIfAbsent 在 writer 尚未输出任何以 prefix 开头的语句时补发元素创建语句（裸声明）。
// 顶层逆走由通用逆走器的具名数组分支先行发出（不补，避免回放撞「值未变化」）；
// 配置模式层级 show 直接进入家族发射器、无此前置，须就地补发（cpu 等别名要求元素已存在）。
func emitElemIfAbsent(w *stmtWriter, prefix []string) {
	if len(prefix) == 0 || elemEmitted(w, prefix) {
		return
	}
	w.add(append([]string{}, prefix...))
}

// elemEmitted 报告 writer 输出里是否已有以 prefix 开头的语句（元素已被前置语句建出/引用）。
func elemEmitted(w *stmtWriter, prefix []string) bool {
	for _, s := range w.out {
		if len(s.toks) < len(prefix) {
			continue
		}
		match := true
		for i := range prefix {
			if s.toks[i] != prefix[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// emitCPUModeModel 把平铺 cpu_mode/cpu_model 渲染为两层语句 `cpu mode <v>` / `cpu model <v>`
// （镜像画像与 VNF startup 覆盖共用；空值不发射——模型字段 omitempty，缺席与空串同义）。
func emitCPUModeModel(w *stmtWriter, m map[string]any, prefix []string) {
	if v, ok := m["cpu_mode"]; ok && v != "" {
		w.add(toks(prefix, "cpu", "mode", formatScalar(v)))
	}
	if v, ok := m["cpu_model"]; ok && v != "" {
		w.add(toks(prefix, "cpu", "model", formatScalar(v)))
	}
}

// ---------- 根级回落发射器（决策 #155 补充） ----------
//
// vrfs / qos_policies 的模型键在命令树里没有对应顶层关键字（VRF 由
// `virtual-switches <n> type l3` / l3-interface / static-routes 落库、qos 由
// `qos policies` 别名落库），通用逆走与家族注册表都不可达，由根层未消费键的回落钩子接管。

func init() {
	rootFallbackEmitters["vrfs"] = emitVrfsFallback
	rootFallbackEmitters["qos_policies"] = emitQosFallback
	rootFallbackEmitters["vxlan_tunnels"] = emitVxlanFallback
}

// emitVxlanFallback 把 vxlan_tunnels[] 反推为 `vxlan tunnels <名> vni <n> local <ip> remote <ip>
// [dst-port <n>] [virtual-switch <vs>]`（决策 #383）。
//
// 与 qos_policies 同族：树顶层关键字是 `vxlan`、模型键是 vxlan_tunnels，通用逆走在根层
// 查不到 "vxlan" 键即跳过，故由根层回落钩子接管（别名 cli_aliases_vxlan.go 的对偶）。
// vni/local/remote 是模型必填（校验要求齐备），一行给全；可选叶子仅在非零值时输出
// （零值＝未配置，回放后序列化逐字节相同）。virtual-switch 语句回放要求该交换机已存在
// ——交换机关键字先于根层回落发射，顺序与本文件既有约定一致。
func emitVxlanFallback(w *stmtWriter, val any) error {
	arr, ok := val.([]any)
	if !ok {
		return nil
	}
	for _, el := range arr {
		t, ok := el.(map[string]any)
		if !ok {
			continue
		}
		name, _ := t["name"].(string)
		vni, hasVni := t["vni"]
		local, _ := t["local"].(string)
		remote, _ := t["remote"].(string)
		if name == "" || !hasVni || local == "" || remote == "" {
			// vni/local/remote 是校验要求的必填项，正常建不出缺项配置；缺项时如实跳过
			// （回放自校验会把缺语报告出来，而不是猜一个值）。
			continue
		}
		line := []string{"vxlan", "tunnels", name, "vni", formatScalar(vni), "local", local, "remote", remote}
		if p, ok := t["dst_port"].(float64); ok && p != 0 {
			line = append(line, "dst-port", formatScalar(p))
		}
		if vs, ok := t["virtual_switch"].(string); ok && vs != "" {
			line = append(line, "virtual-switch", vs)
		}
		w.add(line)
	}
	return nil
}

// emitVrfsFallback 把 vrfs[]（L3 数据落点，VRF 名 = 同名虚拟交换机）反推为
// `virtual-switches <名> type l3` / `l3-interface …` / `static-routes …` 语句。
//
// 决策 #452：**发射自足**——每个 VRF 先发一条确定性的创建语句（`type l3`），即便该
// VRF 没有 l3_interfaces/routes 也能自复现；顶层 `type l3`（emitVirtualSwitchFamily）
// 已让位到此处，故不双发。按 vrfs[] 顺序逐条创建，使回放后的 vrfs[] 顺序与 committed
// 逐项一致（此前空 VRF 只能靠 `type l3` 别名副作用重建，顺序/重复即破）。
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
		// 确定性创建语句：仅在**存在同名 type=l3 虚拟交换机**时发出（见 vrfSwitchIsL3）——
		// VRF 没有同名交换机（仅由 l3-interface/static-routes 建出的形态）或同名交换机
		// 显式为 l2 时，`type l3` 会凭空建/改交换机而破坏回放等价；那两种形态的 VRF 由
		// 下面的 l3-interface/static-routes 语句自身建出（仍按 vrfs[] 顺序）。
		if vrfSwitchIsL3(w, name) {
			w.add([]string{"virtual-switches", name, "type", "l3"})
		}
		// 描述字段仅 REST 可达（无语句形态）：只发注释说明（同交换机/ACL 描述先例），
		// 不再让回放自校验把整条 display set 打回；回放比较两侧按 stripNonEmittable 同剥。
		if v, ok := vrf["description"]; ok && v != "" {
			w.note("virtual-switches %s：描述字段无语句关键字，不入 display set", name)
		}
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
				// 决策 #472：无地址条目（`delete … ip address` 只删地址叶留下的残形，
				// 条目本身有数据面语义——接口仍属该 VRF/表）只能经裸形态语句表达；裸声明
				// 须排在该条目的叶语句**之前**（回放时空配置从裸声明建出条目；条目已被
				// 叶语句建出后再发裸声明会撞「值未变化」而回放失败）。有地址的条目保持
				// 原输出：首条叶语句即建出条目，不掺裸声明（逐字不变）。
				addrs, _ := li["addresses"].([]any)
				if len(addrs) == 0 {
					w.add(append([]string{}, p...))
				}
				if acl, ok := li["acl_in"].(string); ok && acl != "" {
					w.add(append(append([]string{}, p...), "acl-in", acl))
				}
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

// lastStmtIs 报告 w 最近一条语句是否恰为 toks。用于识别**顶层整配置逆走**：通用逆走器的
// 具名数组分支在进入家族发射器前会先发出元素裸声明（= 该元素的 prefix），故「最近一条恰为
// 本 prefix」＝当前在顶层逆走；配置模式层级 show（generateSetStmts 带 prefix 直接进入本
// 发射器）没有这条前置裸声明。决策 #452 据此让顶层 `type l3` 让位给 vrfs 回落、层级 show
// 仍就地发出。
func lastStmtIs(w *stmtWriter, toks []string) bool {
	if len(w.out) == 0 {
		return false
	}
	last := w.out[len(w.out)-1].toks
	if len(last) != len(toks) {
		return false
	}
	for i := range toks {
		if last[i] != toks[i] {
			return false
		}
	}
	return true
}

// vrfSwitchIsL3 报告该 VRF 名是否对应一个 **type=l3** 的虚拟交换机（据此决定回落发射器
// 要不要补发 `virtual-switches <名> type l3` 创建语句，决策 #452）。顶层逆走里交换机的
// 裸声明已先行发出，且非 l3 的交换机会带自己的 `type` 语句；l3 交换机的 `type l3` 已让位
// 给本回落（见 emitVirtualSwitchFamily），故此处看不到它的 type 语句。三种形态：
//   - 同名交换机是 l3：有裸声明、无 type 语句 ⇒ true（补发创建语句）；
//   - 同名交换机显式为 l2：有裸声明、且有 type 语句 ⇒ false（补发会把它改型为 l3）；
//   - 无同名交换机（VRF 仅由 l3-interface/static-routes 建出）：无裸声明 ⇒ false
//     （补发会凭空建出一个交换机）。
//
// 后两种形态的 VRF 由 l3-interface/static-routes 语句自身建出，顺序仍随 vrfs[] 迭代。
func vrfSwitchIsL3(w *stmtWriter, name string) bool {
	hasSwitch := false
	for _, s := range w.out {
		t := s.toks
		if len(t) < 2 || t[0] != "virtual-switches" || t[1] != name {
			continue
		}
		if len(t) == 2 {
			hasSwitch = true
			continue
		}
		if t[2] == "type" {
			return false // 交换机已带 type 语句（非 l3——l3 已让位）⇒ 不是 l3 交换机
		}
	}
	return hasSwitch
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

// ---------- 回放自校验的「不发射字段」剥离 ----------

// 口径（R38-2）：**空容器不是有意义的数据**——`{}` / `[]` / null 与「键不存在」对回放等价。
// 发射器对空容器本就发不出语句（无内容可逆走），回放侧也不会凭空建出空壳——于是
// **一侧的空容器与另一侧的缺席**会让深比较不等，把整条 `display set` 打回内部错误
// （真机实例：Web 侧清空字段留下的 `system.kernel = {}`、`vpp.dpdk.dev = {}`）。
// 修法与 stripNonEmittable 同路：比较端**两侧同剥**（stripEmptyContainers）——
// 不是提交期拒绝（界面清空字段是正常操作，不该变报错），也不动前端。

// stripEmptyContainers 递归剪除空容器（比较端两侧同口径）。剪除是递归的：只含空容器的
// 父容器同样被剪（`{a:{}}` ≡ 缺席）；数组里的空元素一并剔除（`[{},x]` ≡ `[x]`）。
// 判据是**值形态**而非字段名——任何字段的空容器都不参与比较，故无需 w.note 说明
// （空容器本身没有可说明的数据；字段级清单仍走 stripNonEmittable 的注释原则）。
func stripEmptyContainers(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			sv := stripEmptyContainers(vv)
			if isEmptyContainer(sv) {
				continue
			}
			out[k] = sv
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, vv := range x {
			sv := stripEmptyContainers(vv)
			if isEmptyContainer(sv) {
				continue
			}
			out = append(out, sv)
		}
		return out
	default:
		return v
	}
}

// isEmptyContainer 空容器判据：null（模型无 omitempty 的 slice/map 字段在空值时的
// 序列化形态）、空 dict、空 list——三者对语句反推都是「无内容」。
func isEmptyContainer(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	}
	return false
}

// stripNonEmittable 剥离发射器**有意不输出**、无语句可还原的字段——回放自校验两侧同剥，
// 使 display set 不再因这些字段把**整条读命令**打回（修复「一条 Web 侧编辑让整条
// display set 读不出来」：此前 virtual-switches 的 description 照发语句，回放撞
// 「未知语句: description」整条失败）。每一项在对应发射器处都有 w.note 说明，剥的就是
// 这些**已注释**的字段——任何未注释的差异仍如实报错：
//   - virtual_switches[].description / vrfs[].description / acls[].description：
//     描述字段无语句关键字（发射器只发注释）；
//   - virtual_switches[].gateway：空对象（删除残留，非空照常由语句还原、照常比较）；
//   - virtual_machine_functions[].serial_console=false：关闭态无 set 语句形态
//     （true 由 `serial console enable` 发射）；
//   - virtual_machine_functions[].interfaces[].sriov.vf_id：VF 编号无可用 set 语句
//     （语句关键字与模型字段不一致）。
//
// prefix 为展示层级（顶层 show 为空、配置模式层级 show 为绝对语句路径）——层级 show 的
// 子树根就是元素对象本身，须按 prefix 折算元素归属的 JSON 键路径（身份取值不占键段）。
func stripNonEmittable(v any, prefix []string) any {
	return stripNonEmittableAt(v, nonEmittableKeyPath(prefix))
}

// nonEmittableKeyPath 语句层级路径 → 元素所属 JSON 键路径（只认发射器有「不输出」字段的
// 四个家族；关键字 token 经 jsonKeyOf 折算，身份取值 token 跳过）。顶层与其它层级返回
// 空路径（顶层逆走的数组规则在空路径下照常生效）。
func nonEmittableKeyPath(prefix []string) []string {
	if len(prefix) == 0 {
		return nil
	}
	switch prefix[0] {
	case "virtual-switches", "vrfs", "acls", "virtual-machine-functions":
		out := make([]string, 0, len(prefix)/2+1)
		for i := 0; i < len(prefix); i += 2 { // 「关键字 身份取值」逐对推进
			out = append(out, strings.ReplaceAll(prefix[i], "-", "_")) // 与 jsonKeyOf 同口径
		}
		return out
	}
	return nil
}

// stripNonEmittableAt 递归剥离（数组元素不增加键路径段：元素的 family 即其数组键路径）。
func stripNonEmittableAt(v any, jk []string) any {
	switch x := v.(type) {
	case map[string]any:
		family := strings.Join(jk, " ")
		out := make(map[string]any, len(x))
		for k, vv := range x {
			if nonEmittableLeaf(family, k, vv) {
				continue
			}
			out[k] = stripNonEmittableAt(vv, append(append([]string{}, jk...), k))
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, vv := range x {
			if nonEmittableElem(jk, vv) {
				continue
			}
			out = append(out, stripNonEmittableAt(vv, jk))
		}
		return out
	default:
		return v
	}
}

// nonEmittableLeaf 报告键路径 family 下的字段 k 是否属「发射器有意不输出」之列——
// 与各发射器的 w.note 站点一一对应，改一处须连另一处（回归测试钉住 display set 行为）。
func nonEmittableLeaf(family, k string, v any) bool {
	switch family + "|" + k {
	case "virtual_switches|description", "vrfs|description", "acls|description":
		s, ok := v.(string)
		return ok && s != ""
	case "virtual_switches|gateway":
		gw, ok := v.(map[string]any)
		return ok && len(gw) == 0 // 删除残留的空对象；非空网关照常比较
	case "virtual_machine_functions|serial_console":
		b, ok := v.(bool)
		return ok && !b // 关闭态；true 由语句还原、照常比较
	case "virtual_machine_functions interfaces sriov|vf_id":
		return true // 无可用 set 语句（vpp/内核均不依赖 display set 还原该值）
	}
	return false
}

// nonEmittableElem 报告数组元素**整体**属「发射器有意不输出」之列（字段级判据
// nonEmittableLeaf 表达不了的元素级规则）。判据与发射器逐字同源：
//   - resource-pools cpu numa 的空核列表节点：核列表是语句必需取值（`numa node <n>`
//     不带 cores 回放即失败），空/缺 cores 的元素无语句可还原（见 emitNumaNode）。
func nonEmittableElem(jk []string, v any) bool {
	if strings.Join(jk, " ") != "resource_pools cpu numa" {
		return false
	}
	em, ok := v.(map[string]any)
	if !ok {
		return false
	}
	cores, _ := em["cores"].([]any)
	return len(cores) == 0
}
