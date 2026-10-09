package api

// 内核数据面 DNS 代理的运行态读视图注记（决策 #439）。
//
// 由来：内核数据面下数据面 DNS 代理由 nfvisd 内的**域内转发器**承担——每个服务落点
// （L2 交换机的 IPv4 网关地址 / L3 交换机同名 VRF 的 l3-interface IPv4 地址）一个绑具体
// 地址的 UDP/53 socket；与 VPP 数据面「punt socket + 按 sw_if_index 选域」形态不同——
// socket 起没起、回了多少 SERVFAIL，只有产品自己知道。配置视图（enabled/servers/switches）
// 两数据面共用、行为不变；内核侧另加运行态块（机制 + 每落点一行 + 计数，与 dhcp_relay_note
// 同法：VPP 侧不出现，CLI 与 REST 同一实现、同一措辞）。

import (
	"fmt"
	"strings"

	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// DNSProxyRuntime 内核数据面 DNS 代理转发器的运行态读物（Options.DNSProxy；nil = 运行态块
// 不出现，VPP 数据面恒如此）。ok=false = 没有可报的运行态（从未启用/读不到），读视图据此
// 省略运行态块——不编造。
type DNSProxyRuntime interface {
	DNSProxyState() (network.DNSProxyState, bool)
}

// kernelDNSProxyNote 内核数据面下 DNS 代理的运行态注记（CLI 与 REST 同一实现、同一措辞）。
// 如实给出三件事：
//  1. 机制——nfvisd 内的域内转发器：在各域 IPv4 落点（L2 网关 / L3 l3-interface 地址）的
//     UDP/53 上收查询，按域优先、回落全局上游经宿主网络栈转发；无可用上游回 SERVFAIL；
//  2. 每个落点一行——域、内核 VRF 设备、绑定地址、生效上游；未收敛的如实给出原因；
//  3. 计数——已应答 / 已回 SERVFAIL / 回包失败。
//
// 落点集为空时如实说「当前无服务落点」，不编造空块。
func kernelDNSProxyNote(st network.DNSProxyState) string {
	const mech = "内核数据面下的 DNS 代理由 nfvisd 内的转发器承担：在各域 IPv4 落点（L2 网关 / " +
		"L3 l3-interface 地址）的 UDP/53 上收查询，按域优先、回落全局上游经宿主网络栈转发；" +
		"无可用上游回 SERVFAIL"
	var b strings.Builder
	b.WriteString(mech + "。\n")
	if len(st.Domains) == 0 {
		b.WriteString("当前无服务落点。\n")
	} else {
		for _, d := range st.Domains {
			addrs := strings.Join(d.Addresses, "、")
			if addrs == "" {
				addrs = "（无）"
			}
			up := strings.Join(d.Upstreams, "、")
			if up == "" {
				up = "无（回 SERVFAIL）"
			}
			fmt.Fprintf(&b, "域 %s（%s）：%s ← 上游 %s", d.Name, d.VRFDevice, addrs, up)
			if e := strings.TrimSpace(d.Error); e != "" {
				b.WriteString("；未收敛：" + e)
			}
			b.WriteString("\n")
		}
	}
	fmt.Fprintf(&b, "已应答 %d / 已回 SERVFAIL %d / 回包失败 %d", st.Answered, st.Servfail, st.SendFail)
	return b.String()
}

// dnsProxyState 注入读物可读时的运行态（CLI 侧；未注入/无运行态 ⇒ ok=false，读视图省略该块）。
func (x *cliExecutor) dnsProxyState() (network.DNSProxyState, bool) {
	if x.dnsProxy == nil {
		return network.DNSProxyState{}, false
	}
	return x.dnsProxy.DNSProxyState()
}

// dnsProxyState 注入读物可读时的运行态（REST 侧；与 CLI 同一读法与口径）。
func (s *Server) dnsProxyState() (network.DNSProxyState, bool) {
	if s.dnsProxy == nil {
		return network.DNSProxyState{}, false
	}
	return s.dnsProxy.DNSProxyState()
}
