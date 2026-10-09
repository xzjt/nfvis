package model

// 内核数据面派生设备名的**导出入口**（决策 #439）：编排层需要按模型的命名规则算出内核
// VRF 设备名（DNS 代理的域落点派生于提交编排内），而规则本体（kernelDerivedLinkName /
// kernelDerivedGatewayVRFName）留在 validate.go、保持不导出——这里只做薄封装，保证
// 「模型内部校验」与「编排层派生」用同一份实现（不各抄一份而漂移）。
// 与 netkernel 侧同一映射的同步由 netkernel 的跨包一致性测试
// （internal/orchestrator/netkernel/naming_validate_sync_test.go）钉住。

// KernelGatewayVRFDevice L2 交换机网关域的内核 VRF 设备名（显式 gateway vrf 优先，否则派生 vr-<交换机名>）。
func KernelGatewayVRFDevice(swName, explicitVRF string) string {
	if explicitVRF != "" {
		return kernelDerivedLinkName(explicitVRF)
	}
	return kernelDerivedGatewayVRFName(swName)
}

// KernelVRFDevice 一个 VRF 条目（含 type=l3 交换机同名条目）的内核设备名。
func KernelVRFDevice(vrfName string) string { return kernelDerivedLinkName(vrfName) }
