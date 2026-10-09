package network

// 内核数据面 DNS 代理的**运行态读视图**类型（决策 #439）。定义在 network 包（而非 netkernel）：
// API/CLI/Web 三面同源地渲染内核侧运行态块，而读视图装配层只依赖本包的类型（与
// network.DHCPServerLeases 等读视图同一依赖方向）。VPP 侧不产生本类型（行为不变）。

// DNSProxyDomainState 内核数据面 DNS 代理一个域落点的运行态（决策 #439）。
type DNSProxyDomainState struct {
	Name      string   `json:"name"`            // 交换机/VRF 名
	Addresses []string `json:"addresses"`       // 已绑定的落点地址（服务中）
	Upstreams []string `json:"upstreams"`       // 该域生效的上游（按域优先、回落全局；空 = 回 SERVFAIL）
	VRFDevice string   `json:"vrf_device"`      // socket 绑定的内核 VRF 设备（作用域）
	Error     string   `json:"error,omitempty"` // 该域未收敛的如实原因（空 = 就绪）
}

// DNSProxyState 内核数据面 DNS 代理的运行态读数（决策 #439）。
type DNSProxyState struct {
	Domains  []DNSProxyDomainState `json:"domains"`   // 按声明序（确定性）
	Answered uint64                `json:"answered"`  // 已应答（成功转发）
	Servfail uint64                `json:"servfail"`  // 已回 SERVFAIL（无上游或上游失败）
	SendFail uint64                `json:"send_fail"` // 回包失败（如实计数，不静默）
}
