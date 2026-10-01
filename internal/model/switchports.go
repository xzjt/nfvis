package model

import "sort"

// 决策 #326（收口 R84-16 / v2 待做 二.8）：虚拟交换机成员端口的**读视图**。
//
// 现状（登记）：VNF/容器声明的 vNIC（`set virtual-machine-functions <vm> interfaces <nic>
// virtual-switch <vs>` 与 container-functions 同形）挂在某台交换机上，却**不出现在**
// 该交换机的端口读视图里——`show virtual-switches <vs> ports` 与 REST
// `GET /virtual-switches/{n}/ports` 只看配置里的静态 `ports`，于是操作者看不到「这台
// 交换机上其实还挂了几个 vNIC」。物化进 `vs.Ports` 会改配置库形状（schema 迁移 + 所有写
// 路径连带），故本决策定为**运行态派生、不改配置库**：读视图在读取时把 VNF/容器声明的
// vNIC 作为**派生条目**并入端口列表，并用 source 逐条区分来源。
//
// 语义边界：
//   - 派生条目**只读**：不写回配置库；CLI/REST/Web 三面同源（都调本函数）。
//   - 静态 `ports` 与派生条目按 **source** 区分（config|vnf|container）。
//   - 同一 vNIC 既在交换机 `ports` 里显式声明、又在 VNF 侧声明时，只出显式声明那条
//     （source=config），不重复。
//   - 派生的准据是**配置**（声明即派生，与运行态无关）：VNF 已从配置删除则派生条目随之
//     消失（不残留幽灵条目）；VNF 已停但仍声明则仍在列表里——是否在线由运行态列如实反映。

// 端口来源（读视图的 source 列/字段）。
const (
	// PortSourceConfig 交换机 `ports` 里静态声明的成员。
	PortSourceConfig = "config"
	// PortSourceVNF VNF 侧 `interfaces <nic> virtual-switch <vs>` 声明派生的成员。
	PortSourceVNF = "vnf"
	// PortSourceContainer 容器侧 `interfaces <nic> virtual-switch <vs>` 声明派生的成员。
	PortSourceContainer = "container"
	// PortSourceRuntime 仅在 VPP 运行态存在、配置未声明的成员（保留 #84 的「运行态也看得见」）。
	PortSourceRuntime = "runtime"
)

// SwitchPortView 交换机成员端口的读视图条目：静态声明与派生端口的并集，逐条标注来源。
// 字段与 VSwitchPort 对齐（便于客户端复用渲染），额外带 source 与 port（VPP 侧接口名，
// 供运行态列按名合并、CLI/Web 展示；SR-IOV VF 等无法确定时为空串）。
type SwitchPortView struct {
	Source             string `json:"source"`
	Port               string `json:"port,omitempty"`
	Seq                int    `json:"seq,omitempty"`
	Interface          string `json:"interface,omitempty"`
	VNF                string `json:"vnf,omitempty"`
	VNFInterface       string `json:"vnf_interface,omitempty"`
	Container          string `json:"container,omitempty"`
	ContainerInterface string `json:"container_interface,omitempty"`
	TrunkVlans         []int  `json:"trunk,omitempty"`
	NativeVlan         int    `json:"native,omitempty"`
	AclIn              string `json:"acl_in,omitempty"`
	AclOut             string `json:"acl_out,omitempty"`
}

// portDisplayName 端口在 VPP 中的接口名（物理口=其名；vNIC=确定性名，见 AttachedPortName）。
// 无法确定（SR-IOV VF）时返回空串——调用方退回 <owner>/<nic>，不编造不存在的接口名。
func (p SwitchPortView) portDisplayName(cfg Config) string {
	switch {
	case p.Interface != "":
		return p.Interface
	case p.VNF != "":
		return AttachedPortName(cfg, PortSourceVNF, p.VNF, p.VNFInterface)
	case p.Container != "":
		return AttachedPortName(cfg, PortSourceContainer, p.Container, p.ContainerInterface)
	}
	return ""
}

// portKey 派生去重键：能同时出现在静态 ports 与派生两处的只有 vnf/container 成员
// （按 (名, vNIC) 身份）；物理口条目用 `if|` 前缀，与派生键不同域，天然不冲突。
func portKey(src, owner, nic string) string { return src + "|" + owner + "|" + nic }

// staticPortKey 静态端口的去重键（按条目类型取身份，与派生键同域）。
func staticPortKey(p VSwitchPort) string {
	switch {
	case p.Vnf != "":
		return portKey(PortSourceVNF, p.Vnf, p.VnfInterface)
	case p.Container != "":
		return portKey(PortSourceContainer, p.Container, p.ContainerInterface)
	default:
		return "interface|" + p.Interface
	}
}

// DerivedSwitchPorts 返回交换机 vsName 的端口读视图：静态 `ports`（source=config）
// 在前、按其 seq 声明序，随后是 VNF 声明（source=vnf）与容器声明（source=container）
// 派生出的 vNIC 成员（各按 (名, vNIC) 升序，确定性输出）。同一成员只出一次。
//
// 交换机未在配置中声明时仍会返回挂在它名下的派生条目（调用方可据此给出「该交换机
// 未在配置中」的说明，而不是把派生条目一并丢掉）。
func DerivedSwitchPorts(cfg Config, vsName string) []SwitchPortView {
	out := make([]SwitchPortView, 0, 4)
	seen := map[string]bool{}

	for _, vs := range cfg.VirtualSwitches {
		if vs.Name != vsName {
			continue
		}
		for _, p := range vs.Ports {
			seen[staticPortKey(p)] = true
			out = append(out, SwitchPortView{
				Source: PortSourceConfig, Seq: p.Seq, Interface: p.Interface,
				VNF: p.Vnf, VNFInterface: p.VnfInterface,
				Container: p.Container, ContainerInterface: p.ContainerInterface,
				TrunkVlans: p.TrunkVlans, NativeVlan: p.NativeVlan,
				AclIn: p.AclIn, AclOut: p.AclOut,
			}.withPortName(cfg))
		}
		break
	}

	// 收集派生成员并按 (名, vNIC) 排序，保证同配置两次读取逐字相同。
	var vnfs, cts [][2]string
	for _, vm := range cfg.VirtualMachineFunctions {
		for _, ifc := range vm.Interfaces {
			if ifc.VirtualSwitch != vsName || ifc.Name == "" {
				continue
			}
			if seen[portKey(PortSourceVNF, vm.Name, ifc.Name)] {
				continue // 已在静态 ports 里显式声明：只出那一条（source=config）
			}
			seen[portKey(PortSourceVNF, vm.Name, ifc.Name)] = true
			vnfs = append(vnfs, [2]string{vm.Name, ifc.Name})
		}
	}
	for _, ct := range cfg.ContainerFunctions {
		for _, ifc := range ct.Interfaces {
			if ifc.VirtualSwitch != vsName || ifc.Name == "" {
				continue
			}
			if seen[portKey(PortSourceContainer, ct.Name, ifc.Name)] {
				continue
			}
			seen[portKey(PortSourceContainer, ct.Name, ifc.Name)] = true
			cts = append(cts, [2]string{ct.Name, ifc.Name})
		}
	}
	sortMembers(vnfs)
	sortMembers(cts)
	for _, m := range vnfs {
		out = append(out, SwitchPortView{Source: PortSourceVNF, VNF: m[0], VNFInterface: m[1]}.withPortName(cfg))
	}
	for _, m := range cts {
		out = append(out, SwitchPortView{
			Source: PortSourceContainer, Container: m[0], ContainerInterface: m[1]}.withPortName(cfg))
	}
	return out
}

// withPortName 填上 VPP 侧接口名（确定性，见 ifacename.go）；无法确定时保持空串。
func (p SwitchPortView) withPortName(cfg Config) SwitchPortView {
	p.Port = p.portDisplayName(cfg)
	return p
}

// sortMembers 按 (名, vNIC) 升序就地排序（确定性输出）。
func sortMembers(m [][2]string) {
	sort.Slice(m, func(i, j int) bool {
		if m[i][0] != m[j][0] {
			return m[i][0] < m[j][0]
		}
		return m[i][1] < m[j][1]
	})
}

// VNICAttachedTo 报告 <owner, nic> 这个 VNF/容器 vNIC 是否声明挂到交换机 vsName。
// 供 API 层在「删除派生端口」时给出去 VNF 侧删除的明确指引（决策 #326 的边界）。
func VNICAttachedTo(cfg Config, kind, owner, nic, vsName string) bool {
	switch kind {
	case PortSourceVNF:
		for _, vm := range cfg.VirtualMachineFunctions {
			if vm.Name != owner {
				continue
			}
			for _, ifc := range vm.Interfaces {
				if ifc.VirtualSwitch == vsName && (nic == "" || ifc.Name == nic) {
					return true
				}
			}
		}
	case PortSourceContainer:
		for _, ct := range cfg.ContainerFunctions {
			if ct.Name != owner {
				continue
			}
			for _, ifc := range ct.Interfaces {
				if ifc.VirtualSwitch == vsName && (nic == "" || ifc.Name == nic) {
					return true
				}
			}
		}
	}
	return false
}

// AttachedPortName 返回 VNF/容器 vNIC 在 VPP 中的接口名（vhost-user/memif，命名规则与
// 建接口侧同源——见 ifacename.go）。SR-IOV VF 的 VPP 名取决于绑定关系、此处无法确定，
// 返回 ""（调用方据此退回 `<owner>/<nic>`，不编造一个不存在的接口名）。
func AttachedPortName(cfg Config, kind, owner, nic string) string {
	if kind == PortSourceContainer {
		return MemifIfaceName(owner, nic)
	}
	for _, vm := range cfg.VirtualMachineFunctions {
		if vm.Name != owner {
			continue
		}
		for _, ifc := range vm.Interfaces {
			if ifc.Name != nic {
				continue
			}
			switch ifc.Type {
			case "memif":
				return MemifIfaceName(owner, nic)
			case "sriov-vf":
				return ""
			default: // vhost-user（缺省）
				return VnfIfaceName(owner, nic)
			}
		}
	}
	return ""
}
