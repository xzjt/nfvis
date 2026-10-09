// Package netkernel 是 Linux 内核网络数据面的编排实现（system.dataplane = kernel）。
//
// 与 internal/orchestrator/network（VPP 数据面）并列：二者实现同一个
// orchestrator.NetworkProvider 接口，由 nfvisd 按 committed 配置里的数据面选择装配其一
// （整机单数据面，切换需重启）。映射关系（契约见 docs/v3-数据面可切换-设计.md）：
//
//	L2 虚拟交换机 → 内核 bridge（vlan_filtering）
//	L3 虚拟交换机 → 内核 VRF（ip link type vrf，路由表号由名字确定性派生）
//	L3 接口/地址   → ip link / ip addr（vlan 子接口按需建）
//	静态路由       → ip route（vrf 表内）
//	NAT44          → nftables（table inet nfvis-nat；snat/masquerade/dnat）
//	VXLAN          → ip link type vxlan
//	VNF vNIC       → virtio + vhost-net + tap（宿主侧 tap 挂 bridge；VM 侧由计算编排下发）
//	容器 vNIC      → veth 对（宿主端由网络编排建、交换机段入 bridge；容器端在容器 start 后移入其 netns）
//	bond           → ip link type bond（内核 bonding 驱动）
//
// 数据面命令一律经 Runner 执行（不直接 exec），单测注入假 Runner 校验命令生成，
// 使本包在任意平台可测。族级能力大多已接通（ACL/QoS/SPAN/DHCP 中继与服务器/DNS 代理/
// 风暴抑制/端口安全/LLDP/容器 vNIC 都有内核侧真实现）；仍在内核数据面没有对应物的
// （NAT 会话等 VPP 专有读视图）一律**如实报不支持**（ErrUnsupported），不静默成功。
package netkernel

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrUnsupported 该能力在当前数据面（Linux 内核网络）没有实现。
//
// 一律由**提交期校验**提前拒绝（model.Validate 的 checkKernelDataPlane），
// 此处是纵深防御的第二道：装配或恢复重放若仍走到，如实报错而不是静默成功。
var ErrUnsupported = errors.New("该能力在 Linux 内核网络数据面下不受支持")

// ErrToolMissing 依赖的宿主工具不存在（iproute2 / nftables 未安装）。
var ErrToolMissing = errors.New("宿主缺少网络配置工具")

// Runner 执行一条宿主命令并返回合并输出。
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// execRunner 真实实现（宿主 ip / nft）。
type execRunner struct{}

// NewExecRunner 构造真实 Runner。
func NewExecRunner() Runner { return execRunner{} }

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return string(out), fmt.Errorf("%w: %s", ErrToolMissing, name)
		}
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
