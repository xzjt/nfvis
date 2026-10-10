package model

import (
	"fmt"
	"hash/fnv"
)

// VNF vNIC 在 VPP 中的**确定性接口名**（FR-NET-020~023，附录 A #41）。
//
// 命名规则下沉到 model 的理由：同一份派生规则有三个使用者——compute 侧建 domain XML、
// network 侧建 vhost-user/memif 接口、以及校验层判定「配置声明的 vNIC 能否作 l3-interface」
// （round84 证据 §14：guest 的网关地址必须落在 guest 自己的口上，否则 VPP 不为同 VRF 的
// 另一接口代答 ARP，guest 100% Destination Host Unreachable）。三处若各写一份必然漂移，
// 故以本文件为唯一真源，`internal/orchestrator` 侧的 VnfIfaceName/MemifIfaceName 只作转发。
//
// model 是无内部依赖的叶子包（internal/system 等已 import 它），反向 import 会成环。

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

// ContainerVethHostPrefix / ContainerVethPeerPrefix 内核数据面下容器 vNIC 的 veth 对两端前缀
// （决策 #441）。命名总形如 `<前缀>+8 位小写十六进制` = 15 字符 = IFNAMSIZ 上限，与内核侧
// DHCP 内置 tap（network.DHCPServerTapName）同一命名法：确定性派生、只由哈希区分实例，
// 故恢复重放/巡检能按名核对与复用（长度也逐字对齐）。
const (
	ContainerVethHostPrefix = "nfvisct" // 宿主端：网络编排创建、由 bridge-domain 段入交换机内核 bridge
	ContainerVethPeerPrefix = "nfviscp" // 容器端：容器编排在容器 start 后移入其 netns
)

// ContainerVethNames 内核数据面下容器 vNIC 的 veth 对命名（决策 #441）。
//
// 规则下沉到 model 的理由与 VnfIfaceName/MemifIfaceName 同源（本文件头）：**读视图**也要按同一
// 份规则给派生端口条目命名——内核数据面下端口列表里可核对、可排障的实际设备是 veth **宿主端**
// （决策 #442，收口 R5-1：此前两种数据面都显示 VPP 逻辑名 mf-<容器>-<vNIC>，内核侧照名去
// `ip link` 找不到设备）。编排层（internal/orchestrator）的同名函数只作转发。
//
// 哈希取 FNV-1a 32 位全宽、渲染成 8 位小写十六进制；两端**同哈希不同前缀**，产品据此区分
// 宿主端/容器端。哈希不可逆（派生只为**同一性**），归属另按声明/簿记集合判定
// （见 internal/orchestrator/netkernel/container_veth.go）。
func ContainerVethNames(containerName, ifaceName string) (hostEnd, ctEnd string) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(containerName + "/" + ifaceName))
	sum := fmt.Sprintf("%08x", h.Sum32())
	return ContainerVethHostPrefix + sum, ContainerVethPeerPrefix + sum
}
