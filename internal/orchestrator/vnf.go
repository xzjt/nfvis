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
