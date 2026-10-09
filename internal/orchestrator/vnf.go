package orchestrator

import (
	"fmt"
	"hash/fnv"
	"path"
	"sort"

	"github.com/xzjt/nfvis/internal/model"
)

// VNF vNIC 接入的中立描述与确定性命名（FR-NET-020~023）。
//
// 命名/路径确定性派生（同 BD ID/VRF TableID 思路，决策 #31/#40）：compute（domain XML）
// 与 network（VPP 侧 vhost-user 接口）必须算出同一路径与接口名，不得各写一份。
// **接口名规则的真源在 model（internal/model/ifacename.go）**——校验层也要按同一份规则
// 判定「配置声明的 vNIC 能否作 l3-interface」，故本文件的 VnfIfaceName/MemifIfaceName 只作转发。

// VnfPort 一台 VNF 的一个虚拟网卡接入点。
type VnfPort struct {
	VM            string
	Interface     string // vNIC 名（配置 interfaces[].name）
	Type          string // vhost-user | sriov-vf | memif
	VirtualSwitch string
	MAC           string
	VLAN          int
	Socket        string // vhost-user：VPP 侧监听 socket 路径（QEMU 作 client）
	VRF           string // 非空 = 该 vNIC 挂在 L3 交换机（值为同名 VRF），仅入表不配 IP
}

// VnfSocketPath vhost-user socket 路径：<vhostDir>/<vm>-<vnic>.sock。
func VnfSocketPath(vhostDir, vmName, ifaceName string) string {
	return path.Join(vhostDir, vmName+"-"+ifaceName+".sock")
}

// DefaultVhostDir vhost-user socket 缺省目录（nfvisd 启动期确保存在）。
const DefaultVhostDir = "/run/nfvis/vhost"

// VnfIfaceName vhost-user 接口在 VPP 中的名字（交换机端口按名引用）。
// 规则已下沉到 model（校验层要按同一份规则解析 vNIC 名），此处只作转发，签名不变。
func VnfIfaceName(vmName, ifaceName string) string { return model.VnfIfaceName(vmName, ifaceName) }

// VnfPortTag VPP 接口 tag：用于跨进程反查该接口归属（恢复收敛用，附录 A #35 思路）。
func VnfPortTag(vmName, ifaceName string) string {
	return "nfvis:vnf:" + vmName + ":" + ifaceName
}

// DefaultMemifDir memif socket 缺省目录（容器 vNIC，FR-NET-022）。
const DefaultMemifDir = "/run/nfvis/memif"

// ContainerVethHostPrefix / ContainerVethPeerPrefix 内核数据面下容器 vNIC 的 veth 对两端前缀
// （决策 #441）。命名总形如 `<前缀>+8 位小写十六进制` = 15 字符 = IFNAMSIZ 上限，与内核侧
// DHCP 内置 tap（network.DHCPServerTapName）同一命名法：确定性派生、只由哈希区分实例，
// 故恢复重放/巡检能按名核对与复用（长度也逐字对齐）。
const (
	ContainerVethHostPrefix = "nfvisct" // 宿主端：网络编排创建并 enslave 到交换机内核 bridge
	ContainerVethPeerPrefix = "nfviscp" // 容器端：容器编排在容器 start 后移入其 netns
)

// ContainerVethNames 内核数据面下容器 vNIC 的 veth 对命名（决策 #441 单一真源）：
// 宿主端 nfvisct+8 位十六进制、容器端 nfviscp+同一 8 位十六进制（按「容器名/nic 名」派生，
// 各 15 字符 = IFNAMSIZ 上限；哈希取法与 DHCP 内置 tap 同族）。
//
// 哈希取 FNV-1a 32 位全宽（与 LinkName / deriveUint32 同一族），渲染成 8 位小写十六进制
// 与 DHCP 内置 tap 的落形一致（前缀 + 8hex = 15 字符）。两端**同哈希不同前缀**：产品据此把
// 「宿主端」与「容器端」区分开（容器端由容器编排按此名找到后移入容器 netns）。
//
// 派生只为**同一性**（同一个 vNIC 每次都得到同一对名字），不承载归属信息——哈希不可逆，
// 名字反查不出「哪个容器的哪个 nic」，故内核侧另按声明/簿记集合判定归属
// （见 internal/orchestrator/netkernel/container_veth.go）。
func ContainerVethNames(containerName, ifaceName string) (hostEnd, ctEnd string) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(containerName + "/" + ifaceName))
	sum := fmt.Sprintf("%08x", h.Sum32())
	return ContainerVethHostPrefix + sum, ContainerVethPeerPrefix + sum
}

// MemifSocketPath 容器 vNIC 的 VPP 侧 memif socket 路径。
func MemifSocketPath(memifDir, owner, ifaceName string) string {
	return path.Join(memifDir, owner+"-"+ifaceName+".sock")
}

// MemifSocketID / MemifID 由 (容器名, vNIC 名) 确定性派生（FNV-1a，非零）。
// VPP memif 的 socket-id 与 memif-id 命名空间独立，同一容器多 vNIC 需各自确定。
func MemifSocketID(owner, ifaceName string) uint32 {
	return deriveUint32("sock:" + owner + "/" + ifaceName)
}
func MemifID(owner, ifaceName string) uint32 { return deriveUint32("memif:" + owner + "/" + ifaceName) }

func deriveUint32(key string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	v := h.Sum32()
	if v == 0 {
		v = 1
	}
	return v
}

// MemifIfaceName memif 接口在 VPP 中的名字（容器端口按名引用）。同样转发 model。
func MemifIfaceName(owner, ifaceName string) string { return model.MemifIfaceName(owner, ifaceName) }

// VnfPortsOf 由配置派生全部需 VPP 接入的 vNIC 端口（VM vhost-user + 容器 memif），
// 按 (属主名, vNIC 名) 升序，保证操作序列与恢复收敛重放确定。
// socket 路径与 VRF 归属（L3 交换机同名 VRF）在此统一派生，供事务 apply 与恢复收敛共用。
func VnfPortsOf(cfg model.Config, vhostDir, memifDir string) []VnfPort {
	var out []VnfPort
	for _, vm := range cfg.VirtualMachineFunctions {
		for _, nic := range vm.Interfaces {
			if nic.Type != "vhost-user" {
				continue
			}
			out = append(out, VnfPort{
				VM: vm.Name, Interface: nic.Name, Type: nic.Type,
				VirtualSwitch: nic.VirtualSwitch, MAC: nic.MAC, VLAN: nic.Vlan,
				Socket: VnfSocketPath(vhostDir, vm.Name, nic.Name),
				VRF:    vrfForSwitch(cfg, nic.VirtualSwitch),
			})
		}
	}
	for _, ct := range cfg.ContainerFunctions {
		for _, nic := range ct.Interfaces {
			if nic.Type != "memif" {
				continue
			}
			out = append(out, VnfPort{
				VM: ct.Name, Interface: nic.Name, Type: nic.Type,
				VirtualSwitch: nic.VirtualSwitch, MAC: nic.MAC, VLAN: nic.Vlan,
				Socket: MemifSocketPath(memifDir, ct.Name, nic.Name),
				VRF:    vrfForSwitch(cfg, nic.VirtualSwitch),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].VM != out[j].VM {
			return out[i].VM < out[j].VM
		}
		return out[i].Interface < out[j].Interface
	})
	return out
}

// SwitchMembersOf 把「VNF/容器侧声明的 vNIC 接入」合流进交换机端口集合（FR-NET-020~023）。
//
// 两条声明看似等价、实则不同源：
//   - 交换机侧 `virtual-switches <vs> ports <n> vnf <vm> interface <nic>` 直接写 vs.Ports；
//   - VNF 侧 `virtual-machine-functions <n> interfaces <nic> virtual-switch <vs>` 只把交换机
//     名记在 VNF 对象上（VnfInterface.VirtualSwitch）。
//
// 而 bridge-domain 的成员口只由 vs.Ports 决定，故 VNF 侧声明若不合流，vhost-user 口永远
// 不会被挂进 BD：guest 的帧在 vhost 口被全部丢弃（rx 有计数、drops 同步涨），L2FIB 学不到
// guest MAC，DHCP 拿不到地址——但全程零报错（决策 #170）。
//
// 此处以 VnfPortsOf 为唯一真源，投影出「带全部成员口的交换机副本」，供事务 apply 与恢复
// 收敛共用（不在 l2.go 里另造一份遍历）。交换机侧已显式声明的同一 vNIC 以显式声明为准
// （可带 trunk/native/acl 属性）。
//
// errors 列出无法归位的声明（vNIC 声明的交换机在配置中不存在）：调用方必须使其可见
// （提交失败或进未收敛清单），**不得静默跳过**——静默正是该缺陷长期未被发现的原因。
func SwitchMembersOf(cfg model.Config, vhostDir, memifDir string) ([]model.VirtualSwitch, []error) {
	out := make([]model.VirtualSwitch, len(cfg.VirtualSwitches))
	copy(out, cfg.VirtualSwitches)
	idxOf := make(map[string]int, len(out))
	declared := make(map[string]bool, len(out)) // 交换机侧已声明的 vNIC（属主/vNIC）
	nextSeq := make(map[string]int, len(out))   // 合成端口序号：接在配置已用序号之后
	for i := range out {
		// 端口切片须独立复制：后续 append 不得写回调用方的配置（cap 可能大于 len）。
		out[i].Ports = append([]model.VSwitchPort(nil), out[i].Ports...)
		idxOf[out[i].Name] = i
		for _, p := range out[i].Ports {
			if key := vnicPortKey(p.Vnf, p.VnfInterface, p.Container, p.ContainerInterface); key != "" {
				declared[out[i].Name+"/"+key] = true
			}
			if p.Seq >= nextSeq[out[i].Name] {
				nextSeq[out[i].Name] = p.Seq + 1
			}
		}
	}

	var errs []error
	for _, p := range VnfPortsOf(cfg, vhostDir, memifDir) {
		if p.VirtualSwitch == "" {
			continue // vNIC 未接入交换机：只建接口、不入 bridge-domain（合法声明）
		}
		i, ok := idxOf[p.VirtualSwitch]
		if !ok {
			errs = append(errs, fmt.Errorf("%s %s 的 vNIC %s 声明了虚拟交换机 %s，但该交换机不在配置中，"+
				"该 vNIC 无法挂入任何 bridge-domain", vnicOwnerCN(p.Type), p.VM, p.Interface, p.VirtualSwitch))
			continue
		}
		if out[i].Type != "l2" {
			continue // L3 交换机经同名 VRF 编排（附录 B），不进 bridge-domain
		}
		key := p.VirtualSwitch + "/" + vnicKeyOfPort(p)
		if declared[key] {
			continue // 交换机侧已显式声明同一 vNIC
		}
		port := model.VSwitchPort{Seq: nextSeq[p.VirtualSwitch], Vnf: p.VM, VnfInterface: p.Interface}
		if p.Type == "memif" {
			port = model.VSwitchPort{Seq: nextSeq[p.VirtualSwitch], Container: p.VM, ContainerInterface: p.Interface}
		}
		out[i].Ports = append(out[i].Ports, port)
		nextSeq[p.VirtualSwitch]++
	}
	return out, errs
}

// vnicPortKey 同一 vNIC 端口的同一性键（属主类别:属主/vNIC）；未指定属主时返回空串。
// 带类别前缀是为了让同名的 VM 与容器不互相冒认。
func vnicPortKey(vnf, vnfIface, container, ctIface string) string {
	if vnf != "" {
		return "vnf:" + vnf + "/" + vnfIface
	}
	if container != "" {
		return "ct:" + container + "/" + ctIface
	}
	return ""
}

// vnicKeyOfPort 由 vNIC 接入点派生与 vnicPortKey 同构的同一性键。
func vnicKeyOfPort(p VnfPort) string {
	if p.Type == "memif" {
		return vnicPortKey("", "", p.VM, p.Interface)
	}
	return vnicPortKey(p.VM, p.Interface, "", "")
}

// vnicOwnerCN vNIC 属主的中文类别（错误文案用）。
func vnicOwnerCN(portType string) string {
	if portType == "memif" {
		return "容器"
	}
	return "VNF"
}

// vrfForSwitch 若虚拟交换机为 L3 类型则返回同名 VRF（附录 B），否则空。
func vrfForSwitch(cfg model.Config, name string) string {
	if name == "" {
		return ""
	}
	for _, vs := range cfg.VirtualSwitches {
		if vs.Name == name {
			if vs.Type == "l3" {
				return name
			}
			return ""
		}
	}
	return ""
}
