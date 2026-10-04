package network

// DHCP 报文的最小解析与构造（决策 #359，DHCP 服务器 v1）。
//
// 只覆盖**域内服务器**所需的部分（round140 spike 已证的能力边界内）：
//   - 入向：以太帧（tap 路径）或裸 IP 包（punt 路径，可能带 14 字节以太头，先经 extractIP）；
//     只处理 IPv4/UDP 目的端口 67 且 BOOTP op=REQUEST(1) 的报文；
//   - 出向：一律构造**完整以太帧**（写回内核侧 tap，由 bridge-domain 按目的 MAC 交换/洪泛），
//     不使用 punt 回注（round140 实证回注会被 ip4 spoofed local-address 拒绝）。
//
// 如实边界（不做的部分，不猜测、不静默改写）：
//   - 不做中继入向（giaddr≠0 的报文交回调用方忽略）；
//   - 不做 802.1Q 标签解析（access/trunk 子接口在 BD 内已是无标签帧；带标签的帧一律忽略）；
//   - 不做 option 82/扩展选项（NTP/PXE/租约查询等），v1 只下发 53/51/1/3/6/15 与 54。

import (
	"encoding/binary"
	"net"
)

// DHCP 常量（端口与消息类型；RFC 2131）。
const (
	dhcpServerPort  = 67
	dhcpClientPort  = 68
	dhcpMagicCookie = 0x63825363

	dhcpDiscover = 1
	dhcpOffer    = 2
	dhcpRequest  = 3
	dhcpDecline  = 4
	dhcpAck      = 5
	dhcpNak      = 6
	dhcpRelease  = 7
)

// DHCP 选项编码（本版解析/下发的部分）。
const (
	dhcpOptSubnetMask  = 1
	dhcpOptRouter      = 3
	dhcpOptDNS         = 6
	dhcpOptDomainName  = 15
	dhcpOptRequestedIP = 50
	dhcpOptLeaseTime   = 51
	dhcpOptMsgType     = 53
	dhcpOptServerID    = 54
	dhcpOptEnd         = 255
)

// dhcpMessage 一条解析后的入向 DHCP 报文（op=REQUEST、htype=Ethernet、hlen=6）。
type dhcpMessage struct {
	xid         uint32
	flags       uint16
	ciaddr      net.IP // 已配置地址（RENEW/RELEASE 时非零）
	chaddr      net.HardwareAddr
	giaddr      net.IP // 非零＝经中继转来（v1 不支持，调用方忽略）
	msgType     byte
	requestedIP net.IP // option 50（SELECTING/INIT-REBOOT 的请求地址）
	serverID    net.IP // option 54（客户端选定的 server-id）
}

// parseDHCPEtherFrame 解析内核 tap 路径收到的以太帧：仅 IPv4 + UDP/67。
func parseDHCPEtherFrame(frame []byte) (dhcpMessage, bool) {
	var m dhcpMessage
	if len(frame) < 14+20+8 {
		return m, false
	}
	if binary.BigEndian.Uint16(frame[12:14]) != 0x0800 {
		return m, false // 非 IPv4（含 VLAN 标签 0x8100、ARP、IPv6）：不在覆盖内
	}
	return parseDHCPIP(frame[14:])
}

// parseDHCPIP 解析裸 IPv4 包（punt 路径；调用方已用 extractIP 去 L2 头）。目的端口须为 67。
func parseDHCPIP(ipPkt []byte) (dhcpMessage, bool) {
	var m dhcpMessage
	if len(ipPkt) < 20 || ipPkt[0]>>4 != 4 {
		return m, false
	}
	ihl := int(ipPkt[0]&0x0f) * 4
	if ihl < 20 || len(ipPkt) < ihl+8 {
		return m, false
	}
	if ipPkt[9] != 17 { // 仅 UDP
		return m, false
	}
	total := int(binary.BigEndian.Uint16(ipPkt[2:4]))
	if total < ihl+8 || total > len(ipPkt) {
		total = len(ipPkt)
	}
	udp := ipPkt[ihl:total]
	if binary.BigEndian.Uint16(udp[2:4]) != dhcpServerPort {
		return m, false
	}
	udpLen := int(binary.BigEndian.Uint16(udp[4:6]))
	if udpLen < 8 || udpLen > len(udp) {
		udpLen = len(udp)
	}
	return parseDHCPMessage(udp[8:udpLen])
}

// parseDHCPMessage 解析 BOOTP/DHCP 报文主体（236 字节头 + magic cookie + 选项）。
func parseDHCPMessage(b []byte) (dhcpMessage, bool) {
	var m dhcpMessage
	if len(b) < 240 || b[0] != 1 || b[1] != 1 || b[2] != 6 {
		return m, false // 非 BOOTREQUEST / 非以太网 6 字节硬件地址
	}
	if binary.BigEndian.Uint32(b[236:240]) != dhcpMagicCookie {
		return m, false
	}
	m.xid = binary.BigEndian.Uint32(b[4:8])
	m.flags = binary.BigEndian.Uint16(b[10:12])
	m.ciaddr = net.IPv4(b[12], b[13], b[14], b[15])
	m.giaddr = net.IPv4(b[24], b[25], b[26], b[27])
	m.chaddr = append(net.HardwareAddr{}, b[28:34]...)
	for i := 240; i < len(b); {
		code := b[i]
		if code == 0 { // PAD
			i++
			continue
		}
		if code == dhcpOptEnd {
			break
		}
		if i+1 >= len(b) {
			break
		}
		l := int(b[i+1])
		if i+2+l > len(b) {
			break
		}
		v := b[i+2 : i+2+l]
		switch code {
		case dhcpOptMsgType:
			if l == 1 {
				m.msgType = v[0]
			}
		case dhcpOptRequestedIP:
			if l == 4 {
				m.requestedIP = net.IPv4(v[0], v[1], v[2], v[3])
			}
		case dhcpOptServerID:
			if l == 4 {
				m.serverID = net.IPv4(v[0], v[1], v[2], v[3])
			}
		}
		i += 2 + l
	}
	return m, m.msgType != 0
}

// macString 客户端 MAC 的小写冒号分隔形式（租约表/读视图键，与 openapi DhcpLease.mac 同形）。
func macString(mac net.HardwareAddr) string { return mac.String() }

// dhcpReplySpec 构造应答所需的服务器侧参数（生效值，取自已收敛的规格）。
type dhcpReplySpec struct {
	serverMAC net.HardwareAddr // 内核侧 tap 的 MAC（服务器以太源）
	bvi       net.IP           // server-id / IP 源 / 下发网关
	mask      net.IPMask       // BVI 前缀（option 1）
	dns       net.IP           // option 6（配置缺省＝BVI）
	domain    string           // option 15（配置了才发）
	lease     int              // 租约时长秒（option 51）
}

// buildDHCPReply 构造一条完整以太帧应答（OFFER/ACK/NAK）。
//
// 收/发口径（与 round140 实测一致）：
//   - 以太目的 = 客户端 chaddr（**单播**；BD 按目的 MAC 交换到成员口，已实证可达）；
//   - 以太源 = 内核侧 tap 的 MAC；IP 源 = BVI 地址；UDP 67 → 68；
//   - IP 目的 = 255.255.255.255、DHCP flags 置 broadcast 位 0x8000：客户端尚未配置地址
//     时也能被 IP 栈接受（不依赖它已 ARP/收敛）。
func buildDHCPReply(m dhcpMessage, msgType byte, yiaddr net.IP, spec dhcpReplySpec) []byte {
	body := make([]byte, 240)
	body[0] = 2 // BOOTREPLY
	body[1] = 1 // htype Ethernet
	body[2] = 6 // hlen
	binary.BigEndian.PutUint32(body[4:8], m.xid)
	binary.BigEndian.PutUint16(body[10:12], 0x8000) // broadcast 位
	if msgType != dhcpNak && yiaddr != nil {
		copy(body[16:20], yiaddr.To4())
	}
	copy(body[28:34], m.chaddr)
	binary.BigEndian.PutUint32(body[236:240], dhcpMagicCookie)

	opts := []byte{dhcpOptMsgType, 1, msgType}
	if msgType == dhcpOffer || msgType == dhcpAck {
		var b4 [4]byte
		// option 54 server-id：两种消息（含 NAK）都带，客户端据此识别服务器。
		opts = append(opts, dhcpOptServerID, 4)
		opts = append(opts, spec.bvi.To4()...)
		binary.BigEndian.PutUint32(b4[:], uint32(spec.lease))
		opts = append(opts, dhcpOptLeaseTime, 4)
		opts = append(opts, b4[:]...)
		if v4 := spec.mask; len(v4) == 4 {
			opts = append(opts, dhcpOptSubnetMask, 4)
			opts = append(opts, v4...)
		}
		if spec.bvi.To4() != nil {
			opts = append(opts, dhcpOptRouter, 4)
			opts = append(opts, spec.bvi.To4()...)
		}
		if dns := spec.dns.To4(); dns != nil {
			opts = append(opts, dhcpOptDNS, 4)
			opts = append(opts, dns...)
		}
		if spec.domain != "" && len(spec.domain) <= 255 {
			opts = append(opts, dhcpOptDomainName, byte(len(spec.domain)))
			opts = append(opts, spec.domain...)
		}
	} else { // NAK：带 server-id，其余选项不发（RFC 2131 §4.3.2）
		opts = append(opts, dhcpOptServerID, 4)
		opts = append(opts, spec.bvi.To4()...)
	}
	opts = append(opts, dhcpOptEnd)
	body = append(body, opts...)

	// UDP + IPv4（校验和如实计算，与 dnsproxy.go 同一套核心）。
	udpLen := 8 + len(body)
	ipLen := 20 + udpLen
	pkt := make([]byte, ipLen)
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(ipLen))
	pkt[8] = 64 // TTL
	pkt[9] = 17 // UDP
	copy(pkt[12:16], spec.bvi.To4())
	copy(pkt[16:20], net.IPv4bcast.To4())
	binary.BigEndian.PutUint16(pkt[10:12], ipChecksum(pkt[:20]))
	udp := pkt[20:]
	binary.BigEndian.PutUint16(udp[0:2], dhcpServerPort)
	binary.BigEndian.PutUint16(udp[2:4], dhcpClientPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(udpLen))
	copy(udp[8:], body)
	binary.BigEndian.PutUint16(udp[6:8], udpChecksum(pkt[12:16], pkt[16:20], udp))

	// 以太帧：dst=chaddr、src=tap MAC。
	frame := make([]byte, 14+len(pkt))
	copy(frame[0:6], m.chaddr)
	copy(frame[6:12], spec.serverMAC)
	binary.BigEndian.PutUint16(frame[12:14], 0x0800)
	copy(frame[14:], pkt)
	return frame
}
