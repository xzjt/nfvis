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
