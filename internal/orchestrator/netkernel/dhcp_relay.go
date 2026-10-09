package netkernel

// 内核数据面 DHCP 中继（v3 决策 #437）：nfvisd 内的**用户态中继实例**，零外部依赖
// （不引入 dnsmasq/isc-dhcp-relay 一类外部守护）。每台「L2 交换机 + BVI **v4** 网关 +
// `dhcp-relay server <ip>`」的声明一个实例。
//
// 报文流（与 VPP 侧 dhcp proxy 同口径：**源地址重写、giaddr=0**）：
//  1. **收**：在交换机的内核 bridge（既有 `LinkName(vs.Name)`）上收客户端 DHCP 请求（UDP/67）；
//  2. **转**：源地址重写为中继源 = BVI 的第一个 IPv4 网关地址（socket 绑定该地址 : 67），
//     单播到 `server:67`；载荷**字节保真**、不插 option 82（见下）。上行 socket 同时**绑到该域的
//     VRF 设备**（`SO_BINDTODEVICE`，与既有 `ip vrf exec` 同一口径）——BVI 地址在 VRF 里，只绑源
//     地址的话本地发起的路由查找会落在**主表**（域内路由不在主表），connect 阶段就会 ENETUNREACH；
//     绑定 VRF 设备后查找走该 VRF 的表，收包侧由内核按「入向设备是该 VRF 的从属」匹配到本 socket。
//  3. **回**：收 server 的应答，回注到 bridge 交给客户端。客户端此刻无 IP，**不能靠路由**——
//     用 AF_PACKET 底座按客户端 MAC 构造完整以太帧写回 bridge（由 bridge 查 fdb 单播/洪泛，
//     与产品自带 DHCP 服务器的回程同法）。
//
// **客户端寻址依据采用 (a)（契约给定的二选一）**：请求期登记「xid → 客户端 MAC」的**短 TTL 表**
// （relayPendingTTL = 30s），应答按该表回注。理由：
//   - 中继两侧都是单播、不依赖 server 回显任何东西——(b) 的 option 82 要求 server 回显，
//     不回显时应答被静默丢弃（VPP proxy 真机实证的坑，手册 §8.3）；(a) 让「不回显 option 82
//     的 server」也能工作，且**不改动**客户端 payload（载荷字节保真）；
//   - 登记表同时是**回注白名单**：只有我们转出去过的 xid 的应答才会被回注（来源还由连接式
//     UDP 限死为配置的 server:67，内核按对端过滤）。
//   **表里找不到客户端的应答一律丢弃并计数**（droppedNoClient；进读视图注记），不静默。
//
// 生命周期：见 relayManager 的说明（提交 / 交换机删除 / 恢复重放 / 15s 巡检 / 进程退出）。
//
// 如实边界（v1）：
//   - 只处理**无 802.1Q 标签的 IPv4**（与产品自带 DHCP 服务器同口径：BD 内 access/trunk 已是无标签帧；
//     带标签的帧一律不处理）；
//   - 收包面 = AF_PACKET 绑 bridge 的**本地端口**：域内广播/多播与目的 MAC 是 bridge 自身的单播
//     都能收到；成员口之间直接交换的单播（同域内 server 直答客户端的帧）不经过本套接字——
//     中继部署本就不需要它（server 与客户端同 L2 时应直连、不经中继）；
//   - 只中继 DHCP 报文（须带 option 53；纯 BOOTP 无消息类型的老报文不中继）与 BOOTREQUEST
//     （giaddr≠0 的报文不中继——已经过其它中继，防环）；
//   - 不做端口级限速/防泛洪（DHCP 速率极低）；不做 option 82 插入与回显依赖；
//   - server 须在该转发域内可达（同子网或域内静态路由）——与 VPP 侧 relay 的既有范围一致。

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

const (
	relayServerPort  = 67 // 客户端请求的目的端口 / 中继的上行源端口
	relayClientPort  = 68 // 客户端端口（回注帧的 UDP 目的端口）
	relayMagicCookie = 0x63825363
	relayBootRequest = 1
	relayBootReply   = 2
	relayOptMsgType  = 53

	// relayPendingTTL 请求登记（xid → 客户端 MAC）的存活期。
	// 30s：覆盖「server 稍慢才应答」的窗口；客户端重传（RFC 2131 首重试 ≥4s、指数退避）会重新
	// 登记，故不会因过期而丢正常应答。过期条目在插入/查表时顺带清理。
	relayPendingTTL = 30 * time.Second
	// relayPendingMax 登记表条目上限（有界：超出时挤掉最早到期的一条并计数，不无限增长）。
	relayPendingMax = 1024
	// relayStopTimeout 停止实例时等两个收发协程退出的上界（正常路径 ≤ 一个读超时周期即返回；
	// 超时即如实报错——「socket/goroutine 不残留」是契约要求，不糊过去）。
	relayStopTimeout = 3 * time.Second
)

// ---------- 中继规格（由交换机声明派生） ----------

// relayTarget 一台交换机的中继规格：bridge 名、server 与中继源地址。规格变化 ⇒ 实例重建。
type relayTarget struct {
	switchName string
	bridge     string // 内核 bridge 名（既有 LinkName(vs.Name) 口径）
	server     net.IP // DHCP 服务器地址（严格 IPv4；配置层已保证）
	src        net.IP // 中继源地址 = BVI 的第一个 IPv4 网关地址
	vrfDevice  string // 该域 VRF 的内核设备名（上行 socket 的作用域，同 applyGateway 的派生口径）
}

func (t relayTarget) equal(o relayTarget) bool {
	return t.switchName == o.switchName && t.bridge == o.bridge && t.vrfDevice == o.vrfDevice &&
		t.server.Equal(o.server) && t.src.Equal(o.src)
}

// relayTargetOf 由交换机声明派生中继规格。
//
// 口径与校验层 gatewayHasV4、VPP 侧 network.relayTargetOf **逐字同源**：中继源取
// gateway.addresses 里**第一个 IPv4**（顺序敏感，与 VPP 侧一致）。不满足时如实报错——
// 配置层（model.Validate）已拦下这些形态，这里是纵深防御（恢复重放与直接调用编排的路径仍会走到）。
func relayTargetOf(vs model.VirtualSwitch) (relayTarget, error) {
	if vs.Type == "l3" {
		return relayTarget{}, fmt.Errorf(
			"交换机 %s 是 type=l3（没有 BVI 网关），DHCP 中继仅支持已配置网关的 L2 交换机", vs.Name)
	}
	server := net.ParseIP(strings.TrimSpace(vs.DhcpRelayServer)).To4()
	if server == nil {
		return relayTarget{}, fmt.Errorf("交换机 %s 的 DHCP 中继服务器地址 %q 不是 IPv4 地址", vs.Name, vs.DhcpRelayServer)
	}
	if vs.Gateway == nil {
		return relayTarget{}, fmt.Errorf(
			"交换机 %s 未配置网关，无法作 DHCP 中继（先 set virtual-switches %s gateway ip <ip-prefix>）", vs.Name, vs.Name)
	}
	// 该域 VRF 的内核设备名：与 applyGateway 逐字同源（显式 gateway.vrf 也过 LinkName）。
	vrfDevice := GatewayVRFName(vs.Name)
	if vs.Gateway.Vrf != "" {
		vrfDevice = LinkName(vs.Gateway.Vrf)
	}
	for _, a := range vs.Gateway.Addresses {
		ip, _, err := net.ParseCIDR(a)
		if err != nil {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			return relayTarget{
				switchName: vs.Name,
				bridge:     LinkName(vs.Name),
				server:     append(net.IP(nil), server...),
				src:        append(net.IP(nil), v4...),
				vrfDevice:  vrfDevice,
			}, nil
		}
	}
	return relayTarget{}, fmt.Errorf(
		"交换机 %s 的网关没有 IPv4 地址，无法作 DHCP 中继源（中继源地址自动取 BVI 的 IPv4 网关地址）", vs.Name)
}

// ---------- 可注入的收发底座 ----------

// relayFrameIO 中继在**交换机 bridge** 侧的以太帧收发底座。
//
// 真实现 = AF_PACKET(SOCK_RAW) 绑该 bridge（dhcp_relay_frame_linux.go，内核专有）；
// 单测注入内存实现（不依赖真 socket），校验报文处理与生命周期。语义：
//   - Recv 每次返回**一个完整以太帧**；Close 后返回 net.ErrClosed；
//   - Send 写一个完整以太帧（帧自带以太头）——相当于宿主以 bridge 本地端口发包，
//     由 bridge 按目的 MAC 查 fdb 直接交换（查不到则洪泛）；
//   - MAC 是 bridge 自身的 MAC（回注帧的以太源）。
type relayFrameIO interface {
	Recv() ([]byte, error)
	Send(frame []byte) error
	MAC() net.HardwareAddr
	Close() error
}

// relayUplinkIO 中继到 server 的**单播 UDP** 底座：本地地址 = 中继源（BVI 的 v4 网关地址）、
// 本地端口 67，对端固定 server:67（连接式 UDP——内核按对端过滤，只收配置 server 从 67 来的应答）。
type relayUplinkIO interface {
	Send(payload []byte) error    // 一次一个 DHCP 载荷（UDP 层重写由 socket 的本地地址承担）
	Recv(buf []byte) (int, error) // 阻塞；Close 后返回 net.ErrClosed
	Close() error
}

// relaySocketLayer 打开中继的两种底座。真实现见 dhcp_relay_frame_{linux,other}.go；
// 单测用 SetRelaySocketLayer 注入假实现。
type relaySocketLayer interface {
	OpenBridge(bridge string) (relayFrameIO, error)
	// OpenUplink 打开到 server:67 的连接式 UDP：本地 = src（BVI 的 v4 网关地址）: 67，
	// 作用域 = vrfDevice（该域 VRF 的内核设备名；真实现经 SO_BINDTODEVICE 绑定，见 [linux] 文件）。
	OpenUplink(src, server net.IP, vrfDevice string) (relayUplinkIO, error)
}

// ---------- DHCP 报文（只读字段 + 字节保真转发/回注） ----------

// relayMsg 中继需要读的 BOOTP/DHCP 字段（载荷本身按字节保真，不重编码）。
type relayMsg struct {
	op      byte
	xid     uint32
	giaddr  net.IP
	chaddr  net.HardwareAddr
	msgType byte
}

// parseRelayMsg 解析 BOOTP/DHCP 报文主体（236 字节头 + magic cookie + 选项 53）。
// 只认以太网 6 字节硬件地址（htype=1/hlen=6）——与产品其余 DHCP 解析同口径。
func parseRelayMsg(b []byte) (relayMsg, bool) {
	var m relayMsg
	if len(b) < 240 || b[1] != 1 || b[2] != 6 {
		return m, false
	}
	if b[0] != relayBootRequest && b[0] != relayBootReply {
		return m, false
	}
	if binary.BigEndian.Uint32(b[236:240]) != relayMagicCookie {
		return m, false
	}
	m.op = b[0]
	m.xid = binary.BigEndian.Uint32(b[4:8])
	m.giaddr = net.IPv4(b[24], b[25], b[26], b[27])
	m.chaddr = append(net.HardwareAddr{}, b[28:34]...)
	for i := 240; i < len(b); {
		code := b[i]
		if code == 0 { // PAD
			i++
			continue
		}
		if code == 255 { // END
			break
		}
		if i+1 >= len(b) {
			break
		}
		l := int(b[i+1])
		if i+2+l > len(b) {
			break
		}
		if code == relayOptMsgType && l == 1 {
			m.msgType = b[i+2]
		}
		i += 2 + l
	}
	return m, m.msgType != 0
}

// parseRelayFrame 从 bridge 上收到的一帧里取出客户端 DHCP 请求：客户端源 MAC、**原始 UDP 载荷**
// （按字节保真转发）与解析出的 BOOTP 字段。
//
// 只处理**无 802.1Q 标签的 IPv4**（ethertype 0x0800）+ UDP 目的端口 67 + BOOTREQUEST：
// VLAN 标签帧（0x8100）、ARP/IPv6、非 DHCP 一律不处理（如实边界）。giaddr≠0 的报文由调用方
// 丢弃（已经过其它中继，不再中继——防环）。
func parseRelayFrame(frame []byte) (clientMAC net.HardwareAddr, payload []byte, msg relayMsg, ok bool) {
	if len(frame) < 14+20+8 {
		return nil, nil, msg, false
	}
	if binary.BigEndian.Uint16(frame[12:14]) != 0x0800 {
		return nil, nil, msg, false
	}
	ipPkt := frame[14:]
	if ipPkt[0]>>4 != 4 || ipPkt[9] != 17 {
		return nil, nil, msg, false
	}
	ihl := int(ipPkt[0]&0x0f) * 4
	if ihl < 20 || len(ipPkt) < ihl+8 {
		return nil, nil, msg, false
	}
	total := int(binary.BigEndian.Uint16(ipPkt[2:4]))
	if total < ihl+8 || total > len(ipPkt) {
		total = len(ipPkt)
	}
	udp := ipPkt[ihl:total]
	if binary.BigEndian.Uint16(udp[2:4]) != relayServerPort {
		return nil, nil, msg, false
	}
	udpLen := int(binary.BigEndian.Uint16(udp[4:6]))
	if udpLen < 8 || udpLen > len(udp) {
		udpLen = len(udp)
	}
	payload = udp[8:udpLen]
	msg, ok = parseRelayMsg(payload)
	if !ok || msg.op != relayBootRequest {
		return nil, nil, msg, false
	}
	mac := net.HardwareAddr(append([]byte(nil), frame[6:12]...))
	if !validClientMAC(mac) {
		return nil, nil, msg, false
	}
	return mac, payload, msg, true
}

// validClientMAC 客户端 MAC 是否可用作回注目的（非全零、非组播——组播地址不能作单播目的）。
func validClientMAC(mac net.HardwareAddr) bool {
	if len(mac) != 6 || mac[0]&1 == 1 {
		return false
	}
	for _, b := range mac {
		if b != 0 {
			return true
		}
	}
	return false
}

// buildRelayReplyFrame 把 server 的应答（UDP 载荷）包成回注给客户端的完整以太帧。
//
// 口径（客户端此刻无 IP、不能靠路由；与产品自带 DHCP 服务器的回程同法）：
//   - 以太：dst = 登记表里记录的**客户端 MAC**（bridge 按 fdb 交换到它的口；未学到则洪泛），
//     src = bridge 自身的 MAC；ethertype 0x0800；
//   - IP：src = **server 地址**（应答来自谁就写谁：客户端从 IP 源与 option 54 看到真实 server）、
//     dst = 255.255.255.255 广播（客户端未配置地址也能被自己的协议栈接受），TTL 64、不分片；
//   - UDP：67 → 68（server 的应答发给中继的 67，回注时必须改到客户端端口）；载荷**字节保真**；
//   - IP/UDP 校验和按新头重算（改了目的地址与端口就必须重算，UDP 含伪首部）。
func buildRelayReplyFrame(payload []byte, src net.IP, clientMAC, bridgeMAC net.HardwareAddr) []byte {
	udpLen := 8 + len(payload)
	ipLen := 20 + udpLen
	pkt := make([]byte, ipLen)
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(ipLen))
	pkt[8] = 64 // TTL
	pkt[9] = 17 // UDP
	copy(pkt[12:16], src.To4())
	copy(pkt[16:20], net.IPv4bcast.To4())
	binary.BigEndian.PutUint16(pkt[10:12], relayIPChecksum(pkt[:20]))
	udp := pkt[20:]
	binary.BigEndian.PutUint16(udp[0:2], relayServerPort)
	binary.BigEndian.PutUint16(udp[2:4], relayClientPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(udpLen))
	copy(udp[8:], payload)
	binary.BigEndian.PutUint16(udp[6:8], relayUDPChecksum(pkt[12:16], pkt[16:20], udp))

	frame := make([]byte, 14+len(pkt))
	copy(frame[0:6], clientMAC)
	copy(frame[6:12], bridgeMAC)
	binary.BigEndian.PutUint16(frame[12:14], 0x0800)
	copy(frame[14:], pkt)
	return frame
}

// relayIPChecksum IPv4 头校验和（RFC 1071 反码求和）。
func relayIPChecksum(h []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(h); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(h[i:]))
	}
	if len(h)%2 == 1 {
		sum += uint32(h[len(h)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + sum>>16
	}
	return ^uint16(sum)
}

// relayUDPChecksum UDP 校验和（含 IPv4 伪首部；调用方须把 udp[6:8] 的校验和字段先置 0）。
func relayUDPChecksum(src, dst, udp []byte) uint16 {
	var sum uint32
	for _, p := range [][]byte{src, dst} {
		for i := 0; i+1 < len(p); i += 2 {
			sum += uint32(binary.BigEndian.Uint16(p[i:]))
		}
	}
	sum += 17 // 协议号
	sum += uint32(len(udp))
	for i := 0; i+1 < len(udp); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(udp[i:]))
	}
	if len(udp)%2 == 1 {
		sum += uint32(udp[len(udp)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + sum>>16
	}
	c := ^uint16(sum)
	if c == 0 {
		c = 0xffff // 全零结果按 RFC 768 写 0xffff（0 表示"未计算"）
	}
	return c
}

// ---------- 中继实例 ----------

// relayPending 请求登记表的一条（短 TTL）。
type relayPending struct {
	mac     net.HardwareAddr
	expires time.Time
}

// relayInstance 一台交换机的中继实例：两个收发协程（bridge 帧 / server 应答）+ 请求登记表。
type relayInstance struct {
	target    relayTarget
	frames    relayFrameIO
	uplink    relayUplinkIO
	bridgeMAC net.HardwareAddr
	now       func() time.Time

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	done     chan struct{} // 两个收发协程都退出后关闭

	mu      sync.Mutex
	err     error  // 首个致命错误（实例已停；读视图/巡检如实上报）
	lastErr string // 最近一次非致命失败（单包发送/回注失败等；不静默）
	pending map[uint32]relayPending

	forwarded atomic.Uint64 // 已转发的客户端请求
	injected  atomic.Uint64 // 已回注给客户端的应答
	dropped   atomic.Uint64 // 应答在登记表里找不到客户端而丢弃（如实计数）
	evicted   atomic.Uint64 // 登记表满而挤掉最早条目的次数
}

func newRelayInstance(t relayTarget, frames relayFrameIO, uplink relayUplinkIO, now func() time.Time) *relayInstance {
	if now == nil {
		now = time.Now
	}
	return &relayInstance{
		target: t, frames: frames, uplink: uplink, bridgeMAC: frames.MAC(), now: now,
		stop: make(chan struct{}), done: make(chan struct{}), pending: map[uint32]relayPending{},
	}
}

// start 起两个收发协程（每个实例只调用一次）。
func (r *relayInstance) start() {
	r.wg.Add(2)
	go r.serveFrames()
	go r.serveUplink()
	go func() { r.wg.Wait(); close(r.done) }()
}

// alive 实例是否仍在运行（两个收发协程都退出即 false）。
func (r *relayInstance) alive() bool {
	select {
	case <-r.done:
		return false
	default:
		return true
	}
}

// stopAndWait 停止实例并等两个收发协程退出（有界）：先关两个底座（阻塞读随之返回），
// 再等 done。超时如实报错（不得声称"已停"而留下后台消费者）。
func (r *relayInstance) stopAndWait(timeout time.Duration) error {
	r.stopOnce.Do(func() {
		close(r.stop)
		_ = r.frames.Close()
		_ = r.uplink.Close()
	})
	select {
	case <-r.done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("交换机 %s 的 DHCP 中继实例在 %s 内未停止（收发协程未退出）", r.target.switchName, timeout)
	}
}

// fail 记录首个致命错误并关闭两个底座（收发协程随之退出；实例转为"已停"，
// 由 15s 巡检按声明重试启动——起不来继续如实报错）。
func (r *relayInstance) fail(err error) {
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
	r.stopOnce.Do(func() {
		close(r.stop)
		_ = r.frames.Close()
		_ = r.uplink.Close()
	})
}

// setLastErr 记录最近一次非致命失败（读视图如实呈现；不静默）。
func (r *relayInstance) setLastErr(err error) {
	r.mu.Lock()
	if err != nil {
		r.lastErr = err.Error()
	}
	r.mu.Unlock()
}

func (r *relayInstance) isStopped() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

// serveFrames 收 bridge 帧（阻塞直到 Close/致命错误）。
func (r *relayInstance) serveFrames() {
	defer r.wg.Done()
	for {
		frame, err := r.frames.Recv()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || r.isStopped() {
				return
			}
			r.fail(fmt.Errorf("交换机 %s 的 DHCP 中继在 bridge %s 上收帧失败: %w", r.target.switchName, r.target.bridge, err))
			return
		}
		r.handleFrame(frame)
	}
}

// serveUplink 收 server 应答（阻塞直到 Close/致命错误）。
//
// 读错误分两类：**连接式 UDP 会把 ICMP 差错（server 未监听时的 port unreachable、路由不可达等）
// 作为一次性读错误上报**——那不代表 socket 坏了，据实记录（lastErr，进读视图）后继续读；
// 其余（fd 失效等）如实致命：实例转「已停」，由 15s 巡检按声明重试启动。
func (r *relayInstance) serveUplink() {
	defer r.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, err := r.uplink.Recv(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || r.isStopped() {
				return
			}
			if relayTransientRecvErr(err) {
				r.setLastErr(fmt.Errorf("交换机 %s 的 DHCP 中继读取 server %s 的应答时报错（已继续读）: %w",
					r.target.switchName, r.target.server, err))
				continue
			}
			r.fail(fmt.Errorf("交换机 %s 的 DHCP 中继收 server %s 的应答失败: %w", r.target.switchName, r.target.server, err))
			return
		}
		if n <= 0 {
			continue
		}
		r.handleReply(buf[:n])
	}
}

// relayTransientRecvErr 连接式 UDP socket 的**一次性读错误**（ICMP 差错、被信号打断、暂不可读）：
// 不是 socket 失效，继续读即可（调用方记 lastErr 如实呈现，不静默也不误判实例已死）。
func relayTransientRecvErr(err error) bool {
	for _, e := range []error{
		syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EHOSTUNREACH, syscall.ENETUNREACH,
		syscall.EAGAIN, syscall.EWOULDBLOCK, syscall.EINTR, syscall.ENOBUFS, syscall.EMSGSIZE,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// handleFrame 处理一个从 bridge 收到的帧：登记客户端 → 源地址重写转发（giaddr 保持 0、载荷保真）。
func (r *relayInstance) handleFrame(frame []byte) {
	clientMAC, payload, msg, ok := parseRelayFrame(frame)
	if !ok {
		return // 不是本中继要处理的报文（非 IPv4/UDP67/BOOTREQUEST，或源 MAC 不可用）
	}
	if !msg.giaddr.IsUnspecified() {
		return // 已经过其它中继（giaddr≠0）：不再中继（防环）
	}
	// (a) 请求期登记：xid → 客户端 MAC（短 TTL）。应答按这张表回注。
	r.remember(msg.xid, clientMAC)
	// 转：源地址重写由 socket 的本地地址（= BVI 的 v4 网关地址）承担，目的固定 server:67，
	// giaddr 保持 0；payload 按字节保真（不插 option 82）。
	if err := r.uplink.Send(payload); err != nil {
		// 单包发送失败不杀实例（下一个包可能成功）；如实记数、最近错误进读视图，不静默。
		r.setLastErr(fmt.Errorf("交换机 %s 的 DHCP 中继向 server %s 转发失败: %w", r.target.switchName, r.target.server, err))
		return
	}
	r.forwarded.Add(1)
}

// handleReply 处理一个从 server 收到的应答：按登记表找回客户端并回注（找不到即丢弃并计数）。
func (r *relayInstance) handleReply(payload []byte) {
	msg, ok := parseRelayMsg(payload)
	if !ok || msg.op != relayBootReply {
		// 不是 BOOTREPLY（例如客户端直接单播到 BVI:67 的续租请求经内核递到了本 socket）：
		// 不属于"应答"，不参与回注；本帧若确需中继，另有 bridge 侧收包路径处理（见文件头边界）。
		return
	}
	clientMAC, ok := r.lookup(msg.xid)
	if !ok {
		// (a) 的固有代价：登记表里没有这个 xid —— 如实丢弃并计数（不静默；计数进读视图注记）。
		r.dropped.Add(1)
		return
	}
	frame := buildRelayReplyFrame(payload, r.target.server, clientMAC, r.bridgeMAC)
	if err := r.frames.Send(frame); err != nil {
		r.setLastErr(fmt.Errorf("交换机 %s 的 DHCP 中继向客户端 %s 回注应答失败: %w", r.target.switchName, clientMAC, err))
		return
	}
	r.injected.Add(1)
}

// remember 登记一条「xid → 客户端 MAC」（顺带清理过期条目；表长有界）。
func (r *relayInstance) remember(xid uint32, mac net.HardwareAddr) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range r.pending {
		if !v.expires.After(now) {
			delete(r.pending, k)
		}
	}
	if len(r.pending) >= relayPendingMax {
		var (
			oldest  uint32
			oldestT time.Time
			first   = true
		)
		for k, v := range r.pending {
			if first || v.expires.Before(oldestT) {
				oldest, oldestT, first = k, v.expires, false
			}
		}
		delete(r.pending, oldest)
		r.evicted.Add(1)
	}
	r.pending[xid] = relayPending{mac: append(net.HardwareAddr(nil), mac...), expires: now.Add(relayPendingTTL)}
}

// lookup 按 xid 查客户端 MAC（过期条目顺带清理）。
func (r *relayInstance) lookup(xid uint32) (net.HardwareAddr, bool) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.pending[xid]
	if !ok {
		return nil, false
	}
	if !v.expires.After(now) {
		delete(r.pending, xid)
		return nil, false
	}
	return v.mac, true
}

// state 实例运行态（读视图注记的来源）。
func (r *relayInstance) state() network.DHCPRelayState {
	st := network.DHCPRelayState{
		Running:         r.alive(),
		Forwarded:       r.forwarded.Load(),
		Injected:        r.injected.Load(),
		DroppedNoClient: r.dropped.Load(),
		Evicted:         r.evicted.Load(),
	}
	r.mu.Lock()
	switch {
	case r.err != nil:
		st.Reason = r.err.Error()
	case r.lastErr != "":
		st.Reason = r.lastErr
	}
	r.mu.Unlock()
	return st
}

// ---------- 中继实例管理器 ----------

// relayManager 内核数据面 DHCP 中继实例的持有者（每台声明了中继的交换机一个实例）。
//
// 生命周期口径（与 VPP 侧 dhcp proxy 同族）：
//   - **提交**：随交换机事务收敛（bridge-domain 之后的伴随操作，见 orchestrator/apply.go）——
//     新配/改 server/换网关地址重建实例，清声明停实例；起不来如实返回（提交失败/回滚）；
//   - **交换机删除**：DeleteBridgeDomain 先停实例（先解引用、后删被引用）；
//   - **恢复重放**：EnsureConsistent 按 committed 重放（中继实例活在 nfvisd 进程内，进程重启后
//     必然不在——不重放即静默丢中继，与 VPP 重启后 proxy 消失同族教训）；
//   - **巡检**：ReconcileProxy（15s）对账——起失败被停的补启、已不声明的停掉；
//   - **进程退出**：Provider.Close（socket/goroutine 不残留）。
type relayManager struct {
	layer relaySocketLayer
	// now 时钟（单测注入假时钟校验登记表的短 TTL；nil = time.Now）。
	now   func() time.Time
	mu    sync.Mutex
	insts map[string]*relayInstance
	// lastFail 最近一次启动失败的如实原因（交换机名 → 错误文案）。启动失败没有实例可查，
	// 但读视图必须能看见"为什么没跑"（不静默）；启动成功/停止时清除。
	lastFail map[string]string
}

func newRelayManager(layer relaySocketLayer) *relayManager {
	return &relayManager{layer: layer, insts: map[string]*relayInstance{}, lastFail: map[string]string{}}
}

// Sync 收敛一台交换机的中继声明：声明未变且实例在跑 ⇒ 幂等空操作；否则按声明起（重建）实例；
// 未声明 ⇒ 停实例。起不来如实返回错误（调用方按未收敛项处理，不静默）。
func (m *relayManager) Sync(ctx context.Context, vs model.VirtualSwitch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if vs.DhcpRelayServer == "" {
		return m.Stop(vs.Name)
	}
	t, err := relayTargetOf(vs)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur := m.insts[vs.Name]; cur != nil {
		if cur.target.equal(t) && cur.alive() {
			return nil // 幂等：声明未变、实例在跑
		}
		if err := m.stopLocked(vs.Name, cur); err != nil {
			return err
		}
	}
	inst, err := m.open(t)
	if err != nil {
		m.lastFail[vs.Name] = err.Error()
		return err
	}
	delete(m.lastFail, vs.Name)
	m.insts[vs.Name] = inst
	return nil
}

// Stop 停掉一台交换机的中继实例（交换机删除/teardown；无实例即幂等空操作）。
func (m *relayManager) Stop(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst := m.insts[name]
	delete(m.lastFail, name)
	if inst == nil {
		return nil
	}
	return m.stopLocked(name, inst)
}

// stopLocked 停掉并移除一台交换机的实例（调用方须持 m.mu）。
func (m *relayManager) stopLocked(name string, inst *relayInstance) error {
	delete(m.insts, name)
	delete(m.lastFail, name)
	return inst.stopAndWait(relayStopTimeout)
}

// open 按规格打开两个底座并启动实例。任何一步失败都**先把已开的部分关掉**（不残留 socket）。
func (m *relayManager) open(t relayTarget) (*relayInstance, error) {
	frames, err := m.layer.OpenBridge(t.bridge)
	if err != nil {
		return nil, fmt.Errorf("启动交换机 %s 的 DHCP 中继：打开内核 bridge %s 的收发套接字失败: %w",
			t.switchName, t.bridge, err)
	}
	up, err := m.layer.OpenUplink(t.src, t.server, t.vrfDevice)
	if err != nil {
		_ = frames.Close()
		return nil, fmt.Errorf("启动交换机 %s 的 DHCP 中继：绑定中继源地址（BVI 网关地址）%s 的 UDP %d（VRF %s）失败: %w",
			t.switchName, t.src, relayServerPort, t.vrfDevice, err)
	}
	inst := newRelayInstance(t, frames, up, m.now)
	inst.start()
	return inst, nil
}

// StopUndeclared 停掉**已不在给定配置里声明**的实例（15s 巡检的兜底：交换机删除走
// DeleteBridgeDomain、清 relay 走 ApplyDhcpRelay，这里的职责是不让任何实例在声明消失后
// 还活着）。逐条如实返回错误。
func (m *relayManager) StopUndeclared(cfg model.Config) []error {
	declared := map[string]bool{}
	for _, vs := range cfg.VirtualSwitches {
		if vs.Type != "l3" && vs.DhcpRelayServer != "" {
			declared[vs.Name] = true
		}
	}
	var errs []error
	for _, name := range m.names() {
		if declared[name] {
			continue
		}
		if err := m.Stop(name); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// names 当前持有实例的交换机名（排序，便于错误/日志顺序确定）。
func (m *relayManager) names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.insts))
	for n := range m.insts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// State 一台交换机中继实例的运行态（读视图注记的来源）。
// ok=false 只表示「没有任何可报的实例信息」（未声明/从未启动过）——读视图此时不出现注记；
// 启动失败过的交换机由 lastFail 如实给出「未运行 + 原因」。
func (m *relayManager) State(name string) (network.DHCPRelayState, bool) {
	m.mu.Lock()
	inst, failReason := m.insts[name], m.lastFail[name]
	m.mu.Unlock()
	if inst != nil {
		return inst.state(), true
	}
	if failReason != "" {
		return network.DHCPRelayState{Running: false, Reason: failReason}, true
	}
	return network.DHCPRelayState{}, false
}

// Close 停掉全部实例（进程退出；幂等）。
func (m *relayManager) Close() error {
	var errs []error
	for _, name := range m.names() {
		m.mu.Lock()
		inst := m.insts[name]
		var err error
		if inst != nil {
			err = m.stopLocked(name, inst)
		}
		m.mu.Unlock()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ---------- Provider 接线 ----------

// relayMgr 惰性构造中继管理器（真实现 = AF_PACKET + 连接式 UDP）。
func (p *Provider) relayMgr() *relayManager {
	p.relayMu.Lock()
	defer p.relayMu.Unlock()
	if p.relay == nil {
		p.relay = newRelayManager(defaultRelaySocketLayer())
	}
	return p.relay
}

// SetRelaySocketLayer 注入中继的收发底座（单测专用：不依赖真 socket 校验报文处理与生命周期；
// 须在任何 Sync/State 之前调用）。nil = 恢复真实现（等价于重新构造管理器）。
func (p *Provider) SetRelaySocketLayer(l relaySocketLayer) {
	p.relayMu.Lock()
	defer p.relayMu.Unlock()
	if l == nil {
		p.relay = nil
		return
	}
	p.relay = newRelayManager(l)
}

// DHCPRelayState 一台交换机 DHCP 中继实例的运行态（读视图注记的来源；VPP 数据面无此概念，
// 见 network.L2Network 的同名方法）。ok=false = 该交换机没有可报的中继实例信息。
func (p *Provider) DHCPRelayState(name string) (network.DHCPRelayState, bool) {
	return p.relayMgr().State(name)
}

// Close 进程优雅退出：停掉全部 DHCP 中继实例，并关闭 DHCP 服务器的单播接收 socket 与内置 tap
// 的收发协程（决策 #438；socket/goroutine 不残留——tap 是内核对象，保留给下次启动按名复用）。
// 幂等。
func (p *Provider) Close() error {
	var errs []error
	if err := p.relayMgr().Close(); err != nil {
		errs = append(errs, err)
	}
	p.dhcpMu.Lock()
	srv, hub := p.dhcpSrv, p.dhcpHub
	p.dhcpMu.Unlock()
	if srv != nil {
		// 关内核侧 tap 的 AF_PACKET 收发并注销（内核侧注册是空操作）——与 main 里 VPP 侧的
		// `defer dhcpServer.Close()` 同义。
		if err := srv.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if hub != nil {
		if err := hub.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
