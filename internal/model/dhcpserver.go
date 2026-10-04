package model

// DHCP 服务器（决策 #359）的**纯函数**：启用判定、生效取值与租约池区间。
//
// 校验（validate.go）与数据面（orchestrator/network 的 dhcpserver*.go）共同依赖本文件，
// 避免「池区间算法 / 生效缺省」在两处各写一份而漂移（单一事实源，同 dhcp-relay 的先例）。

import (
	"net"
)

// DHCP 服务器取值域与缺省（决策 #359；校验与数据面共用）。
const (
	// DefaultDHCPServerLeaseSeconds 租约时长缺省值（未配置时的**生效值**，不写进配置）。
	DefaultDHCPServerLeaseSeconds = 86400
	// MinDHCPServerLeaseSeconds/MaxDHCPServerLeaseSeconds 租约时长取值域（秒）。
	MinDHCPServerLeaseSeconds = 60
	MaxDHCPServerLeaseSeconds = 2592000
	// MaxDHCPServerPoolSize 池内地址数上限（end-start+1 ≤ 4096）——读视图/租约表与
	// 用户态状态机都是内存结构，v1 以显式上限换取可预测性。
	MaxDHCPServerPoolSize = 4096
)

// DHCPServerEnabled 报告交换机是否**启用了** DHCP 服务器：pool 是启用要件
// （pool_start/pool_end 两键齐备才启用）。可选叶子（租约时长/DNS/域名）单独存在
// 不等于启用——它们随 pool 一起生效（见 validate.go 的口径说明）。
func (s VirtualSwitch) DHCPServerEnabled() bool {
	return s.DhcpServerPoolStart != "" && s.DhcpServerPoolEnd != ""
}

// DHCPServerLeaseSeconds 生效租约时长（配置 0/缺失回落缺省值）。
func (s VirtualSwitch) DHCPServerLeaseSeconds() int {
	if s.DhcpServerLeaseTimeSeconds > 0 {
		return s.DhcpServerLeaseTimeSeconds
	}
	return DefaultDHCPServerLeaseSeconds
}

// GatewayIPv4 取网关声明的**第一个 IPv4 地址**及其网段（决策 #359：DHCP 服务器以 BVI
// 作 server-id、下发网关与缺省 DNS）。ok=false 表示该交换机没有 IPv4 网关。
// 与 validate.go 的 gatewayHasV4 同一口径（逐条解析 CIDR 取首个 v4）。
func (s VirtualSwitch) GatewayIPv4() (net.IP, *net.IPNet, bool) {
	if s.Gateway == nil {
		return nil, nil, false
	}
	for _, a := range s.Gateway.Addresses {
		ip, ipnet, err := net.ParseCIDR(a)
		if err != nil {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			return v4, ipnet, true
		}
	}
	return nil, nil, false
}

// IPv4ToUint32 把 IPv4 地址转 32 位整数（非法地址返回 0,false）。
func IPv4ToUint32(s string) (uint32, bool) {
	ip := net.ParseIP(s)
	if ip == nil {
		return 0, false
	}
	v4 := ip.To4()
	if v4 == nil {
		return 0, false
	}
	return uint32(v4[0])<<24 | uint32(v4[1])<<16 | uint32(v4[2])<<8 | uint32(v4[3]), true
}

// Uint32ToIPv4 32 位整数 → 点分 IPv4。
func Uint32ToIPv4(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// DHCPServerPoolRange 解析租约池为 [start,end] 的 32 位整数区间（含两端）。
// 两端都必须是合法 IPv4 且 start ≤ end，否则 ok=false（取值域校验在 validate.go，
// 本函数只做纯解析，供校验与数据面共用同一套边界）。
func DHCPServerPoolRange(start, end string) (uint32, uint32, bool) {
	lo, ok1 := IPv4ToUint32(start)
	hi, ok2 := IPv4ToUint32(end)
	if !ok1 || !ok2 || lo > hi {
		return 0, 0, false
	}
	return lo, hi, true
}

// DHCPServerPoolSize 池内地址数（end-start+1）；非法时返回 0。
func DHCPServerPoolSize(start, end string) int {
	lo, hi, ok := DHCPServerPoolRange(start, end)
	if !ok {
		return 0
	}
	return int(hi-lo) + 1
}
