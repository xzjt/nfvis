package api

// 内核数据面 DHCP 中继的运行态读视图注记（决策 #437）。
//
// 由来：内核数据面下 `dhcp-relay` 由 nfvisd 内的**用户态中继实例**承担（每台声明了中继的交换机
// 一个实例），与 VPP 数据面「中继在 VPP 内跑」不同——实例跑没跑、有没有丢应答，只有产品自己
// 知道。配置视图（dhcp_relay:{server}）两数据面共用、行为不变；内核侧另加 dhcp_relay_note
// 如实给出「机制 + 客户端寻址依据 + 运行态与计数」（与 learn_limit_note 同法：VPP 侧不出现）。

import (
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// DHCPRelayRuntime 内核数据面 DHCP 中继实例的运行态读物（Options.DHCPRelay）。
// ok=false = 该数据面/该交换机没有可报的中继实例（VPP 数据面恒如此，见 L2Network.DHCPRelayState）。
type DHCPRelayRuntime interface {
	DHCPRelayState(swName string) (network.DHCPRelayState, bool)
}

// kernelDHCPRelayNote 内核数据面下 DHCP 中继的读视图注记（CLI 详情与 REST 详情**同一实现、
// 同一措辞**）。如实给出四件事：
//  1. 实现形态——nfvisd 内的用户态中继实例（非 VPP、非外部守护进程）；
//  2. 报文口径——收 bridge 上的 DHCP 请求，源地址重写为 BVI 的 IPv4 网关地址后单播 server:67，
//     giaddr=0（与 VPP 侧同口径）；
//  3. **客户端寻址依据**（契约要求写明实际采用哪一种）——采用「请求期 xid → 客户端 MAC」的
//     短 TTL 登记表，应答按表回注以太帧；**不是**插 option 82 靠 server 回显（server 不回显
//     option 82 也能工作）；
//  4. 运行态与计数——未运行给出如实原因；「找不到客户端的应答」如实计数（不静默）。
func kernelDHCPRelayNote(st network.DHCPRelayState) string {
	const mech = "内核数据面下的中继由 nfvisd 内的用户态实例承担：收 bridge 上的 DHCP 请求，" +
		"源地址重写为 BVI 的 IPv4 网关地址后单播到 server:67（giaddr=0）；应答按" +
		"「请求期 xid → 客户端 MAC」登记表（短 TTL）回注以太帧给客户端"
	if !st.Running {
		if r := strings.TrimSpace(st.Reason); r != "" {
			return fmt.Sprintf("%s。当前：未运行（%s）", mech, r)
		}
		return fmt.Sprintf("%s。当前：未运行", mech)
	}
	return fmt.Sprintf("%s。当前：运行中；已转发 %d / 已回注 %d / 因找不到客户端丢弃应答 %d",
		mech, st.Forwarded, st.Injected, st.DroppedNoClient)
}
