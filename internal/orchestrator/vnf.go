package orchestrator

import (
	"fmt"
	"hash/fnv"
	"path"
)

// VNF vNIC 接入的中立描述与确定性命名（FR-NET-020~023）。
//
// 命名/路径确定性派生（同 BD ID/VRF TableID 思路，决策 #31/#40）：compute（domain XML）
// 与 network（VPP 侧 vhost-user 接口）必须算出同一路径与接口名，不得各写一份。

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

// VnfIfaceName vhost-user 接口在 VPP 中的名字（交换机端口按名引用；≤63 字节，
// 超长时用 FNV-1a 哈希后缀保证确定且不截断碰撞）。
func VnfIfaceName(vmName, ifaceName string) string {
	full := "vh-" + vmName + "-" + ifaceName
	if len(full) <= 63 {
		return full
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(vmName + "/" + ifaceName))
	return fmt.Sprintf("vh-%08x", h.Sum32())
}

// VnfPortTag VPP 接口 tag：用于跨进程反查该接口归属（恢复收敛用，附录 A #35 思路）。
func VnfPortTag(vmName, ifaceName string) string {
	return "nfvis:vnf:" + vmName + ":" + ifaceName
}

// DefaultMemifDir memif socket 缺省目录（容器 vNIC，FR-NET-022）。
const DefaultMemifDir = "/run/nfvis/memif"

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

// MemifIfaceName memif 接口在 VPP 中的名字（容器端口按名引用；≤63 字节，超长用哈希）。
func MemifIfaceName(owner, ifaceName string) string {
	full := "mf-" + owner + "-" + ifaceName
	if len(full) <= 63 {
		return full
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte("memif/" + owner + "/" + ifaceName))
	return fmt.Sprintf("mf-%08x", h.Sum32())
}
