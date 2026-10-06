package api

// VXLAN 隧道语句别名（决策 #383）：`set/delete vxlan tunnels <name> …`。
//
// 走别名而不是通用树遍历的原因与 nat/qos 同族：树顶层关键字是 `vxlan`（含一层
// `tunnels` 关键字），而模型落在 Config.VxlanTunnels（JSON 键 `vxlan_tunnels`）——
// 关键字名与 JSON 键不是机械双射，通用逆走器到不了模型。本表显式映射：
// set 建/改各叶子，delete 逐叶子（值 token 容错）或整条删除。
//
// 语义边界（与模型校验同源，不在本层重复判定）：取值合法性（vni 范围/local/remote 为
// IPv4 且不相等/dst-port 范围/virtual-switch 必须存在且为 L2）由提交期校验给出明确报错；
// 本层只做「整数叶子须为整数」这类落库前的形状检查，避免把非法取值写进模型。

var statementAliasesVxlan = []aliasRule{
	// vxlan tunnels <name> …（其余 token 由 apply 解析：set 键值对；delete 裸删/逐叶子）
	{pattern: []string{"vxlan", "tunnels", "**"}, apply: aliasVxlanTunnel},
}

// vxlanLeafKey 树叶子关键字 → 模型 JSON 键（只此一处映射，set/delete 共用）。
func vxlanLeafKey(kw string) (string, bool) {
	switch kw {
	case "vni":
		return "vni", true
	case "local":
		return "local", true
	case "remote":
		return "remote", true
	case "dst-port":
		return "dst_port", true
	case "virtual-switch":
		return "virtual_switch", true
	}
	return "", false
}

// aliasVxlanTunnel：vxlan tunnels <name> [<叶子> <值> …]（set）/ vxlan tunnels <name> [<叶子> [值] …]（delete）。
//
// 裸 delete（`delete vxlan tunnels <name>`）＝删整条隧道——数据面归属（BD 成员/隧道本身）
// 由提交编排的删除计划回收（DeleteVxlan），本层只管配置树。
func aliasVxlanTunnel(tree map[string]any, t []string, isSet bool) error {
	rest := t[2:]
	if len(rest) == 0 {
		return errString("配置不完整，缺少取值: " + joinTokens(t))
	}
	name := rest[0]
	if !isSet {
		if len(rest) == 1 {
			return deleteVxlanElement(tree, name)
		}
		em, err := elemByID(tree, "vxlan_tunnels", name)
		if err != nil {
			return err
		}
		// 逐叶子：值 token 容错（操作者常把 set 行原样换成 delete）——叶子关键字后跟的
		// 非关键字 token 视为它的值并跳过。
		for i := 1; i < len(rest); i++ {
			key, ok := vxlanLeafKey(rest[i])
			if !ok {
				return errString("未知语句: \"" + rest[i] + "\"")
			}
			delete(em, key)
			if i+1 < len(rest) {
				if _, next := vxlanLeafKey(rest[i+1]); !next {
					i++
				}
			}
		}
		return nil
	}
	em := ensureElem(tree, "vxlan_tunnels", name)
	for i := 1; i < len(rest); i += 2 {
		if i+1 >= len(rest) {
			return errString("语句不完整: " + rest[i] + " 缺少取值")
		}
		key, ok := vxlanLeafKey(rest[i])
		if !ok {
			return errString("未知语句: \"" + rest[i] + "\"")
		}
		switch key {
		case "vni", "dst_port":
			n, err := numField(rest[i+1])
			if err != nil {
				return err
			}
			em[key] = n
		default:
			em[key] = rest[i+1]
		}
	}
	return nil
}

// deleteVxlanElement 按名字删整条隧道（不存在报「无匹配配置」，与其它别名的删除口径一致）。
func deleteVxlanElement(tree map[string]any, name string) error {
	arr, _ := tree["vxlan_tunnels"].([]any)
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
	if len(out) == 0 {
		delete(tree, "vxlan_tunnels")
	} else {
		tree["vxlan_tunnels"] = out
	}
	return nil
}
