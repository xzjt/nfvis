package api

import "fmt"

// 计算/容器类语句别名表（cli_aliases_compute.go，M4-12）。
//
// 背景：与网络语句同类问题（见 cli_aliases_net.go 头部说明）——M4 配置层声明的
// VM/容器语句中，凡「嵌套关键字 ⇄ 扁平模型字段」或「关键字后跟具名取值」的形态，
// 通用树遍历写不到模型。典型：
//
//   - `container-functions <n> memory size-mb <m>`：CLI 嵌套 memory{size-mb}，
//     模型是扁平 ContainerFunction.MemoryMB → 变更落在模型不认识的 memory 键上被
//     encoding/json 静默丢弃，且元素同时新建会骗过 commitTree 的 Diff 兜底（误报成功）。
//   - `container-functions <n> vcpu count <n>`：同上，模型为扁平 VCPU（还会直接报类型不符）。
//   - `<vm|ct> … interfaces <vnic> virtual-switch <name>`：取值关键字 virtual-switch
//     指向 VnfInterface.VirtualSwitch（通用遍历误当作数组容器）。
//
// 本表把这些语句显式映射到模型字段；每条规则同时处理 set 与 delete。

var statementAliasesCompute = []aliasRule{
	// container-functions <n> args <arg> [<arg> ...]：树为标量叶子但模型是 Args []string，
	// 通用遍历会把整行当作一个字符串 → encoding/json 反序列化失败/静默丢字段，
	// 故按「剩余 token 全部为参数」显式映射（set 覆盖、delete 清空）。
	{pattern: []string{"container-functions", "*", "args", "**"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			ct, err := elemByID(tree, "container_functions", t[1])
			if err != nil {
				return err
			}
			if !isSet || len(t) == 3 {
				delete(ct, "args")
				return nil
			}
			args := make([]any, 0, len(t)-3)
			for _, a := range t[3:] {
				args = append(args, a)
			}
			ct["args"] = args
			return nil
		}},

	// virtual-machine-functions <n> vcpu count <c> pin <bool>（§2.7 单条语句含可选 pin）：
	// 通用遍历在 count 取值后把 pin 当兄弟关键字失败（pin 是 vcpu 容器的兄弟取值），
	// 故整条映射到 VMCpu.{Count,Pin}。
	{pattern: []string{"virtual-machine-functions", "*", "vcpu", "count", "*", "pin", "*"},
		apply: func(tree map[string]any, t []string, _ bool) error {
			vm, err := elemByID(tree, "virtual_machine_functions", t[1])
			if err != nil {
				return err
			}
			cpu, _ := vm["vcpu"].(map[string]any)
			if cpu == nil {
				cpu = map[string]any{}
				vm["vcpu"] = cpu
			}
			n, err := numField(t[4])
			if err != nil {
				return err
			}
			cpu["count"] = n
			switch t[6] {
			case "true":
				cpu["pin"] = true
			case "false":
				cpu["pin"] = false
			default:
				return fmt.Errorf("pin 取值须为 true|false: %q", t[6])
			}
			return nil
		}},

	// container-functions <n> memory size-mb <m> → ContainerFunction.MemoryMB
	{pattern: []string{"container-functions", "*", "memory", "size-mb", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			ct, err := elemByID(tree, "container_functions", t[1])
			if err != nil {
				return err
			}
			if !isSet {
				delete(ct, "memory_mb")
				return nil
			}
			n, err := numField(t[4])
			if err != nil {
				return err
			}
			ct["memory_mb"] = n
			return nil
		}},
	{pattern: []string{"container-functions", "*", "memory", "size-mb"},
		apply: func(tree map[string]any, t []string, _ bool) error {
			ct, err := elemByID(tree, "container_functions", t[1])
			if err != nil {
				return err
			}
			delete(ct, "memory_mb")
			return nil
		}},

	// container-functions <n> vcpu count <n> → ContainerFunction.VCPU
	{pattern: []string{"container-functions", "*", "vcpu", "count", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			ct, err := elemByID(tree, "container_functions", t[1])
			if err != nil {
				return err
			}
			if !isSet {
				delete(ct, "vcpu")
				return nil
			}
			n, err := numField(t[4])
			if err != nil {
				return err
			}
			ct["vcpu"] = n
			return nil
		}},
	{pattern: []string{"container-functions", "*", "vcpu", "count"},
		apply: func(tree map[string]any, t []string, _ bool) error {
			ct, err := elemByID(tree, "container_functions", t[1])
			if err != nil {
				return err
			}
			delete(ct, "vcpu")
			return nil
		}},

	// container-functions <n> env <key> <value> → ContainerFunction.Env（**map**，非数组）
	// 语句树里的 env 是「具名数组」形态（PT<key> + 值），与模型的 map 不符 → 原先报
	// 「未知语句」；改由别名直接落 map（决策 #79）。
	{pattern: []string{"container-functions", "*", "env", "*", "*"},
		apply: containerEnvApply},
	{pattern: []string{"container-functions", "*", "env", "*"},
		apply: containerEnvApply},

	// <vm|ct> <n> interfaces <vnic> virtual-switch <name> → VnfInterface.VirtualSwitch
	{pattern: []string{"virtual-machine-functions", "*", "interfaces", "*", "virtual-switch", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return aliasVnicVirtualSwitch(tree, "virtual_machine_functions", t, isSet)
		}},
	{pattern: []string{"container-functions", "*", "interfaces", "*", "virtual-switch", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return aliasVnicVirtualSwitch(tree, "container_functions", t, isSet)
		}},

	// virtual-switches <n> ports <seq> vnf <vm> interface <vnic>
	// → VSwitchPort.{Vnf,VnfInterface}（M4-4 vNIC 挂接 BD 的必要语句，交接文档 §4 坑 6）
	{pattern: []string{"virtual-switches", "*", "ports", "*", "vnf", "*", "interface", "*"},
		apply: aliasPortVnfMember},
	{pattern: []string{"virtual-switches", "*", "ports", "*", "vnf", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return aliasPortVnfMember(tree, append(append([]string{}, t...), ""), false)
		}},

	// virtual-switches <n> ports <seq> container <ct> interface <vnic>
	// → VSwitchPort.{Container,ContainerInterface}（M4-7 容器 memif 挂接）
	{pattern: []string{"virtual-switches", "*", "ports", "*", "container", "*", "interface", "*"},
		apply: aliasPortContainerMember},
	{pattern: []string{"virtual-switches", "*", "ports", "*", "container", "*"},
		apply: func(tree map[string]any, t []string, _ bool) error {
			return aliasPortContainerMember(tree, append(append([]string{}, t...), ""), false)
		}},
}

// aliasPortVnfMember：交换机端口的 VM vNIC 成员（VSwitchPort.Vnf/VnfInterface）。
// t = [virtual-switches, <vs>, ports, <seq>, "vnf"(, <vm>[, "interface", <vnic>])]。
//
// 删除边界（决策 #326）：派生端口（VNF 侧声明挂到本交换机、**不在**静态 ports 里）不可在此删。
// 此前 portElem 在序号不存在时会**凭空建一个空端口元素**再删其 vnf 字段（无字段可删），
// 于是「删一个派生端口」静默留下 {"seq":N} 空壳——既没删掉自认的对象、又污染了配置。
// 现改为：元素不存在时不创建，若该 VM 确有此 vNIC 声明挂到本交换机则给出「去 VNF 侧删」的指引。
func aliasPortVnfMember(tree map[string]any, t []string, isSet bool) error {
	if !isSet {
		if _, err := elemByID(tree, "virtual_switches", t[1]); err != nil {
			return err
		}
		port := existingPortElem(tree, t[1], t[3])
		if port == nil {
			owner := ""
			if len(t) >= 6 {
				owner = t[5]
			}
			if owner != "" && vnicDeclaredOnVSwitch(tree, "virtual_machine_functions", owner, t[1]) {
				return fmt.Errorf("端口 %s 来自 VNF %s 的 vNIC 声明（读视图派生条目，source=vnf），不能在交换机侧删除；"+
					"请在 VNF 侧删除该接口的挂接：delete virtual-machine-functions %s interfaces %s virtual-switch %s",
					t[3], owner, owner, deleteNicHint(t), t[1])
			}
			return fmt.Errorf("端口序号 %s 在交换机 %s 的静态 ports 中不存在（不创建空端口）", t[3], t[1])
		}
		delete(port, "vnf")
		delete(port, "vnf_interface")
		return nil
	}
	port, err := portElem(tree, t[1], t[3])
	if err != nil {
		return err
	}
	if len(t) < 6 {
		return fmt.Errorf("配置不完整: ports %s vnf 缺少 VNF 名", t[3])
	}
	port["vnf"] = t[5]
	if len(t) >= 8 {
		port["vnf_interface"] = t[7]
	}
	return nil
}

// aliasPortContainerMember：交换机端口的容器 memif 成员（VSwitchPort.Container/ContainerInterface）。
// 删除边界同 aliasPortVnfMember（决策 #326）。
func aliasPortContainerMember(tree map[string]any, t []string, isSet bool) error {
	if !isSet {
		if _, err := elemByID(tree, "virtual_switches", t[1]); err != nil {
			return err
		}
		port := existingPortElem(tree, t[1], t[3])
		if port == nil {
			owner := ""
			if len(t) >= 6 {
				owner = t[5]
			}
			if owner != "" && vnicDeclaredOnVSwitch(tree, "container_functions", owner, t[1]) {
				return fmt.Errorf("端口 %s 来自容器 %s 的 vNIC 声明（读视图派生条目，source=container），不能在交换机侧删除；"+
					"请在容器侧删除该接口的挂接：delete container-functions %s interfaces %s virtual-switch %s",
					t[3], owner, owner, deleteNicHint(t), t[1])
			}
			return fmt.Errorf("端口序号 %s 在交换机 %s 的静态 ports 中不存在（不创建空端口）", t[3], t[1])
		}
		delete(port, "container")
		delete(port, "container_interface")
		return nil
	}
	port, err := portElem(tree, t[1], t[3])
	if err != nil {
		return err
	}
	if len(t) < 6 {
		return fmt.Errorf("配置不完整: ports %s container 缺少容器名", t[3])
	}
	port["container"] = t[5]
	if len(t) >= 8 {
		port["container_interface"] = t[7]
	}
	return nil
}

// deleteNicHint 删除指引里的 vNIC 名：8-token 形态（含 `interface <vnic>`）给出实际名，
// 6-token 形态未给 vNIC 时用占位符（指引仍可照做，只是要补上接口名）。
func deleteNicHint(t []string) string {
	if len(t) >= 8 && t[7] != "" {
		return t[7]
	}
	return "<vnic>"
}

// existingPortElem 取交换机静态 ports 里序号为 seq 的元素；不存在返回 nil（**不创建**）。
func existingPortElem(tree map[string]any, vsName, seq string) map[string]any {
	vs, err := elemByID(tree, "virtual_switches", vsName)
	if err != nil {
		return nil
	}
	arr, _ := vs["ports"].([]any)
	em, _ := selectElement(arr, "seq", seq)
	return em
}

// vnicDeclaredOnVSwitch 报告 JSON 树里 owner（VM/容器）是否有 vNIC 声明挂到交换机 vs
// （branch 为 virtual_machine_functions 或 container_functions）。用于删除派生端口的指引。
func vnicDeclaredOnVSwitch(tree map[string]any, branch, owner, vs string) bool {
	arr, _ := tree[branch].([]any)
	for _, e := range arr {
		em, _ := e.(map[string]any)
		if em == nil {
			continue
		}
		if name, _ := em["name"].(string); name != owner {
			continue
		}
		ifaces, _ := em["interfaces"].([]any)
		for _, ie := range ifaces {
			im, _ := ie.(map[string]any)
			if im == nil {
				continue
			}
			if v, _ := im["virtual_switch"].(string); v == vs {
				return true
			}
		}
	}
	return false
}

// aliasVnicVirtualSwitch：vNIC 的所属 L2 交换机（VnfInterface.VirtualSwitch）。
// t = [branch, <name>, "interfaces", <vnic>, "virtual-switch"(, <vs>)]。
func aliasVnicVirtualSwitch(tree map[string]any, branch string, t []string, isSet bool) error {
	owner, err := elemByID(tree, branch, t[1])
	if err != nil {
		return err
	}
	nic := elemByField(owner, "interfaces", "name", t[3])
	if !isSet {
		delete(nic, "virtual_switch")
		return nil
	}
	if len(t) < 6 {
		return fmt.Errorf("配置不完整: interfaces %s virtual-switch 缺少交换机名", t[3])
	}
	nic["virtual_switch"] = t[5]
	return nil
}

// containerEnvApply：容器环境变量（模型 ContainerFunction.Env map[string]string）。
// t = ["container-functions", <name>, "env", <key>(, <value>)]。
func containerEnvApply(tree map[string]any, t []string, isSet bool) error {
	ct, err := elemByID(tree, "container_functions", t[1])
	if err != nil {
		return err
	}
	env, _ := ct["env"].(map[string]any)
	if !isSet {
		if env == nil {
			return fmt.Errorf("无匹配配置: container-functions %s env %s", t[1], t[3])
		}
		if _, ok := env[t[3]]; !ok {
			return fmt.Errorf("无匹配配置: container-functions %s env %s", t[1], t[3])
		}
		delete(env, t[3])
		return nil
	}
	if len(t) < 5 {
		return fmt.Errorf("配置不完整，缺少取值: container-functions %s env %s <value>", t[1], t[3])
	}
	if env == nil {
		env = map[string]any{}
		ct["env"] = env
	}
	env[t[3]] = t[4]
	return nil
}
