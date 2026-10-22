package api

import (
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

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

	// virtual-machine-functions <n> pci-device <bdf> → VMFunction.PCIDevices（FR-CMP-023）：
	// 树为具名数组容器（P<vnic> 同形），通用遍历会写出 [{"name": …}] 与模型的 []string
	// 不符 → 显式映射为**追加**（同值幂等；已声明含省略 domain/大小写不同写法即视为同值）。
	{pattern: []string{"virtual-machine-functions", "*", "pci-device", "*"},
		apply: aliasVMPCIDevice},
	// delete virtual-machine-functions <n> pci-device（不带值）→ 清空该 VM 的全部直通设备；
	// set 形态缺取值是**语法不完整**（不能借清空语义静默得逞）。
	{pattern: []string{"virtual-machine-functions", "*", "pci-device"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			if isSet {
				return fmt.Errorf("配置不完整，缺少取值: virtual-machine-functions %s pci-device <bdf>", t[1])
			}
			vm, err := elemByID(tree, "virtual_machine_functions", t[1])
			if err != nil {
				return err
			}
			delete(vm, "pci_devices")
			return nil
		}},

	// vm-image-profiles <img> cpu mode|model <v>（决策 #485）：树里 cpu 是**对象容器**、
	// mode/model 是其子关键字（两层），而模型是**平铺**的 cpu_mode/cpu_model。通用遍历会把
	// 取值写成模型不认识的 cpu:{mode:…}（fromJSONTree 报「配置中不存在字段 "cpu"」），
	// 故显式映射到平铺字段（set 赋值 / delete 清字段）。删除形态不带取值（4 token），
	// 与既有 `memory numa node` 的两形态同例。
	{pattern: []string{"vm-image-profiles", "*", "cpu", "mode", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return aliasProfileCPUScalar(tree, t, isSet, "cpu_mode")
		}},
	{pattern: []string{"vm-image-profiles", "*", "cpu", "mode"},
		apply: func(tree map[string]any, t []string, _ bool) error {
			return aliasProfileCPUScalar(tree, t, false, "cpu_mode")
		}},
	{pattern: []string{"vm-image-profiles", "*", "cpu", "model", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return aliasProfileCPUScalar(tree, t, isSet, "cpu_model")
		}},
	{pattern: []string{"vm-image-profiles", "*", "cpu", "model"},
		apply: func(tree map[string]any, t []string, _ bool) error {
			return aliasProfileCPUScalar(tree, t, false, "cpu_model")
		}},

	// virtual-machine-functions <n> startup cpu mode|model <v>（决策 #485）：与镜像画像同族的
	// 层级差异，落在元素**内嵌的 startup 对象**上（VMStartupOverride 的平铺 cpu_mode/cpu_model）。
	{pattern: []string{"virtual-machine-functions", "*", "startup", "cpu", "mode", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return aliasStartupCPUScalar(tree, t, isSet, "cpu_mode")
		}},
	{pattern: []string{"virtual-machine-functions", "*", "startup", "cpu", "mode"},
		apply: func(tree map[string]any, t []string, _ bool) error {
			return aliasStartupCPUScalar(tree, t, false, "cpu_mode")
		}},
	{pattern: []string{"virtual-machine-functions", "*", "startup", "cpu", "model", "*"},
		apply: func(tree map[string]any, t []string, isSet bool) error {
			return aliasStartupCPUScalar(tree, t, isSet, "cpu_model")
		}},
	{pattern: []string{"virtual-machine-functions", "*", "startup", "cpu", "model"},
		apply: func(tree map[string]any, t []string, _ bool) error {
			return aliasStartupCPUScalar(tree, t, false, "cpu_model")
		}},
}

// aliasProfileCPUScalar：`vm-image-profiles <img> cpu mode|model <v>` → 平铺 VMImageProfile
// 字段（field = cpu_mode|cpu_model）。t = [vm-image-profiles, <img>, cpu, mode|model(, <v>)]。
//
// set 时按需建出画像元素（与兄弟语句 `vm-image-profiles <img> accel|machine …` 的通用遍历
// 产出**同一模型**）：此前一律走 elemByID，全新画像上 `set … cpu mode` 直接报
// 「无匹配配置」——同一族语句一半能建元素、一半不能，纯属实现疏漏（真机复现）。
// 元素身份字段取 identityFields 的单一真源（image），不在这里另写一份缺省 name。
//
// delete 保持「不凭空建元素」：删一个不存在的画像若建出只有 image 的空壳，读视图会从
// 「没有这个画像」变成「多了一条空画像」，且空壳无语句可还原（display set 自校验失败）。
func aliasProfileCPUScalar(tree map[string]any, t []string, isSet bool, field string) error {
	if !isSet {
		e, err := elemByID(tree, "vm_image_profiles", t[1])
		if err != nil {
			return err
		}
		delete(e, field)
		return nil
	}
	e := elemByField(tree, "vm_image_profiles", identityFieldOf("vm_image_profiles"), t[1])
	e[field] = t[4]
	return nil
}

// aliasStartupCPUScalar：`virtual-machine-functions <n> startup cpu mode|model <v>` → 元素
// startup 对象上的平铺字段（field = cpu_mode|cpu_model）。
// t = [virtual-machine-functions, <n>, startup, cpu, mode|model(, <v>)]。
//
// set 时按需建 startup 对象（与 memory numa node 同例）；delete 时 startup 不在则不动作
// （交由 commitTree 的 Diff 兜底报「未产生变更」，不凭空建壳）。
func aliasStartupCPUScalar(tree map[string]any, t []string, isSet bool, field string) error {
	vm, err := elemByID(tree, "virtual_machine_functions", t[1])
	if err != nil {
		return err
	}
	st, _ := vm["startup"].(map[string]any)
	if !isSet {
		if st != nil {
			delete(st, field)
		}
		return nil
	}
	if st == nil {
		st = map[string]any{}
		vm["startup"] = st
	}
	st[field] = t[5]
	return nil
}

// aliasVMPCIDevice：VM 的通用 PCI 直通设备列表（VMFunction.PCIDevices，FR-CMP-023）。
// t = ["virtual-machine-functions", <vm>, "pci-device", <bdf>]。
//
// 语法即时报错（model.NormalizeBDF 与提交期校验同源）；删除按**归一 BDF** 匹配
// （`03:00.0` 与 `0000:03:00.0` 是同一设备）；删除不存在的一条如实报「无匹配配置」。
func aliasVMPCIDevice(tree map[string]any, t []string, isSet bool) error {
	vm, err := elemByID(tree, "virtual_machine_functions", t[1])
	if err != nil {
		return err
	}
	raw := t[3]
	norm, err := model.NormalizeBDF(raw)
	if err != nil {
		return err
	}
	arr, _ := vm["pci_devices"].([]any)
	sameBDF := func(el any) bool {
		s, ok := el.(string)
		if !ok {
			return false
		}
		n, err := model.NormalizeBDF(s)
		return err == nil && n == norm
	}
	if !isSet {
		out := make([]any, 0, len(arr))
		for _, el := range arr {
			if !sameBDF(el) {
				out = append(out, el)
			}
		}
		if len(out) == len(arr) {
			return fmt.Errorf("无匹配配置: virtual-machine-functions %s pci-device %s", t[1], raw)
		}
		if len(out) == 0 {
			delete(vm, "pci_devices")
		} else {
			vm["pci_devices"] = out
		}
		return nil
	}
	for _, el := range arr {
		if sameBDF(el) {
			// 同一设备（含不同写法）已在声明中：空操作提示，不重复追加。
			return &noChangeError{stmt: strings.Join(t, " ")}
		}
	}
	vm["pci_devices"] = append(arr, raw)
	return nil
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
		// 回写（决策 #455）：解绑交换机端口即解引用该 vNIC——若其 virtual_switch 正指向本交换机
		// 则一并清掉，与 VM 侧 `delete … interfaces <vnic> virtual-switch` 等价（两形态同一字段）。
		owner, _ := port["vnf"].(string)
		vnic, _ := port["vnf_interface"].(string)
		clearVnicVirtualSwitch(tree, "virtual_machine_functions", owner, vnic, t[1])
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
		// 回写被引用 vNIC 的 virtual_switch（决策 #455：交换机侧接入与 VM 侧接入产出同一模型）。
		setVnicVirtualSwitch(tree, "virtual_machine_functions", t[5], t[7], t[1])
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
		// 回写（决策 #455）：与 VM 侧同口径——解绑即解引用该容器 vNIC。
		owner, _ := port["container"].(string)
		vnic, _ := port["container_interface"].(string)
		clearVnicVirtualSwitch(tree, "container_functions", owner, vnic, t[1])
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
		// 回写被引用 vNIC 的 virtual_switch（决策 #455：交换机侧接入与容器侧接入产出同一模型）。
		setVnicVirtualSwitch(tree, "container_functions", t[5], t[7], t[1])
	}
	return nil
}

// setVnicVirtualSwitch 回写被交换机端口引用的 vNIC 的 virtual_switch（决策 #455）。
//
// branch = virtual_machine_functions|container_functions，owner = VM/容器名，vnic = vNIC 名，
// vs = 接入的交换机名。由来（round13 真机 R13-B）：交换机侧接入
// （`set virtual-switches <n> ports <seq> vnf <vm> interface <vnic>`）此前只写 VSwitchPort，
// 不写 VnfInterface.VirtualSwitch，而内核数据面的域生成据该字段定位内核 bridge——同一份配置
// 在 vpp 可用、切 kernel 生成域失败（报「vNIC 缺少接入的虚拟交换机」）。此处回写使两形态
// 产出**同一模型**（单一真源）。
//
// 仅当宿主元素与 vNIC 都已声明时写入（**不凭空创建**）：引用不存在由提交期校验报错，
// 凭空造一个无 type 的残缺 vNIC 只会把问题推得更远。
func setVnicVirtualSwitch(tree map[string]any, branch, owner, vnic, vs string) {
	if owner == "" || vnic == "" {
		return
	}
	ownerElem, err := elemByID(tree, branch, owner)
	if err != nil {
		return
	}
	arr, _ := ownerElem["interfaces"].([]any)
	nic, _ := selectElement(arr, "name", vnic)
	if nic == nil {
		return
	}
	nic["virtual_switch"] = vs
}

// clearVnicVirtualSwitch 与 setVnicVirtualSwitch 对称：解绑交换机端口 = 解引用该 vNIC——
// 仅当其 virtual_switch 正指向本交换机时清掉（不误清挂到别的交换机的声明）。
func clearVnicVirtualSwitch(tree map[string]any, branch, owner, vnic, vs string) {
	if owner == "" || vnic == "" {
		return
	}
	ownerElem, err := elemByID(tree, branch, owner)
	if err != nil {
		return
	}
	arr, _ := ownerElem["interfaces"].([]any)
	nic, _ := selectElement(arr, "name", vnic)
	if nic == nil {
		return
	}
	if cur, _ := nic["virtual_switch"].(string); cur == vs {
		delete(nic, "virtual_switch")
	}
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
//
// 决策 #455：交换机侧接入（静态 ports）也会回写 vNIC 的 virtual_switch——此时该 vNIC **不是**
// 派生条目（它在交换机侧有静态端口、可在交换机侧删），故若交换机已有静态端口引用该 vNIC 则
// 不算「派生声明」，避免把静态端口误报成派生条目。
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
			if v, _ := im["virtual_switch"].(string); v != vs {
				continue
			}
			if vnic, _ := im["name"].(string); vnic != "" && staticPortRefsVnic(tree, vs, branch, owner, vnic) {
				continue
			}
			return true
		}
	}
	return false
}

// staticPortRefsVnic 交换机 vs 的静态 ports 里是否有引用 (owner, vnic) 的成员
// （branch 决定成员字段：virtual_machine_functions → vnf/vnf_interface，
// container_functions → container/container_interface）。
func staticPortRefsVnic(tree map[string]any, vs, branch, owner, vnic string) bool {
	ownerField, ifaceField := "vnf", "vnf_interface"
	if branch == "container_functions" {
		ownerField, ifaceField = "container", "container_interface"
	}
	vsElem, err := elemByID(tree, "virtual_switches", vs)
	if err != nil {
		return false
	}
	ports, _ := vsElem["ports"].([]any)
	for _, pe := range ports {
		pm, _ := pe.(map[string]any)
		if pm == nil {
			continue
		}
		if o, _ := pm[ownerField].(string); o != owner {
			continue
		}
		if i, _ := pm[ifaceField].(string); i == vnic {
			return true
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
