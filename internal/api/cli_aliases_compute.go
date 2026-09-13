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
func aliasPortVnfMember(tree map[string]any, t []string, isSet bool) error {
	port, err := portElem(tree, t[1], t[3])
	if err != nil {
		return err
	}
	if !isSet {
		delete(port, "vnf")
		delete(port, "vnf_interface")
		return nil
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
func aliasPortContainerMember(tree map[string]any, t []string, isSet bool) error {
	port, err := portElem(tree, t[1], t[3])
	if err != nil {
		return err
	}
	if !isSet {
		delete(port, "container")
		delete(port, "container_interface")
		return nil
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
