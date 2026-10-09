package network

// 数据面 DNS 代理 v2 —— 自研域内转发器（决策 #345，FR-NET-010）。
//
// 形态（取代已撤回的 #338「VPP 内置 dns 插件」）：VPP 经 **punt socket** 把「目的地址 ∈ VPP
// 本机」的 UDP/53 交给 nfvisd，nfvisd 用**宿主网络栈**向上游解析后按原转发域回注
// （desc.Action = PUNT_IP4_ROUTED）。底座语义与真机结论见
// `docs/evidence/v2-round124-d345-dns-punt-spike.txt`，实现严格照此：
//
//   - 注册/注销：govpp binapi `punt`（`punt_socket_register/deregister`，见 dnsproxy_govpp.go）。
//     其效果是 `udp_register_dst_port`——只对「目的地址 ∈ VPP 本机」的 UDP/53 生效，转发中的
//     UDP 不受影响。
//   - 上行包（VPP → nfvisd）：`punt_packetdesc_t{u32 sw_if_index; u32 action}`（8 字节、小端、packed）
//     + 包体。`sw_if_index` = 包进入 IP 栈的接口（真实场景是 **BVI**，或 L3 交换机的 l3-interface）。
//   - 回注（nfvisd → VPP）：同 8 字节 desc + **裸 IP 包**（不带以太网头）；action = 1
//     (PUNT_IP4_ROUTED)，sw_if_index 取**上行 desc 的原值**（对称）⇒ VPP 按该接口所属的 FIB 表
//     路由回域内客户端。
//   - 传输：`AF_UNIX`/`SOCK_DGRAM`。nfvisd bind 自己的 client socket（`/run/vpp/nfvis-dns.sock`），
//     VPP 用 `sendmsg` 发到该地址；nfvisd 用 `sendto` 把回注包发到 VPP 的 server socket
//     （即 startup.conf 里 `punt { socket /run/vpp/punt.sock }` 配的路径）。
//
// 范围（决策 #345）：
//   - **纯 UDP 转发**：不缓存、不解析资源记录内容（只做最小 DNS 报文处理：定位事务 ID 与
//     question 段，用于 SERVFAIL 构造与应答匹配）；
//   - **应答所有**指向 VPP 本机地址的 UDP/53 查询（不设 sw_if_index 白名单）；
//   - 上游选择：按上行包所属交换机取**按域上游**，为空回落**全局**，两者皆空 ⇒ 对该查询回
//     **SERVFAIL**（快速失败，不得静默丢弃）；
//   - 上游由 nfvisd 用宿主网络栈发起 UDP 查询（不是 VPP FIB）；超时（建议 3s）/失败 ⇒ 回 SERVFAIL
//     并计数。
//
// 边界（如实登记，不伪造）：
//   - 客户端用 **TCP** 查 DNS 不生效——punt 只注册 UDP 53，TCP 查询不在覆盖内；
//   - 无缓存；
//   - 不预检上游可达性——运行期回 SERVFAIL 并计数；
//   - 启用期间指向产品地址的 UDP/53 由 nfvisd 独占：nfvisd 不在（崩溃/被停）时这些包被 VPP
//     punt 节点丢弃（该节点 flags=IS_DROP），域内 DNS 中断，直到 nfvisd 回来或停用注销。
//
// 生命周期口径（与 DHCP relay/learn-limit 同族）：
//   - 随提交编排收敛：全局或任一交换机非空 ⇒ 注册 + 起转发器；全空 ⇒ 注销（恢复 VPP 默认处理）；
//   - **恢复重放必须含它**：VPP 重启后 punt 注册丢失，recovery.go 按配置重放（独立记源）；
//   - nfvisd 重启也要重注册；优雅退出时注销（否则留一个指向已消失 socket 的注册＝域内 DNS 黑洞）。

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xzjt/nfvis/internal/orchestrator"
)

const (
	// DNSProxyClientSock nfvisd 侧 client socket（VPP 用 sendmsg 把上行包发到这里）。
	DNSProxyClientSock = "/run/vpp/nfvis-dns.sock"
	// DNSProxyServerSock VPP 侧 server socket（startup.conf 的 `punt { socket … }`；回注 sendto 目标）。
	DNSProxyServerSock = "/run/vpp/punt.sock"
	// dnsProxyUDPPort 注册/应答的 UDP 端口（仅 UDP 53；TCP 不在覆盖内）。
	dnsProxyUDPPort = 53
	// dnsProxyUpstreamTimeout 单个上游的查询超时（超时/失败 ⇒ 回 SERVFAIL 并计数）。
	dnsProxyUpstreamTimeout = 3 * time.Second
	// dnsPuntActionIP4Routed 回注 desc 的 action：VPP 置 sw_if_index[VLIB_RX] 后送 ip4-lookup。
	dnsPuntActionIP4Routed = 1
)

// PuntClient VPP punt socket 注册/注销的最小能力集（govpp 适配 / 单测假实现）。
type PuntClient interface {
	// Register 注册「目的地址 ∈ VPP 本机」的 UDP/<port> 到 clientPath（VPP 用 sendmsg 发到该地址）。
	Register(clientPath string, port uint16) error
	// Deregister 注销同一条注册（恢复 VPP 默认处理）。
	Deregister(port uint16) error
	Close()
}

// PuntDesc 上行/回注的 8 字节描述符（u32 sw_if_index + u32 action，小端）。
//
// 导出（v3 决策 #438）：内核数据面要在 netkernel 包里实现同一 seam（单播续租的接收面），
// 接口的方法签名里出现描述符类型，故类型与字段必须可命名；VPP 侧语义与实现**逐字不变**。
type PuntDesc struct {
	// SwIfIndex 上行包所属转发域（VPP 侧 = 域 BVI 的 sw_if_index；内核侧 = 该交换机内核
	// bridge 的 ifindex）——provider 用它经 SetSwitchResolver 派发到对应交换机。
	SwIfIndex uint32
	// Action 回注动作（VPP 侧语义；内核侧不用回注，恒 0）。
	Action uint32
}

// PuntTransport 上行收包与回注的传输层（真实实现 = AF_UNIX SOCK_DGRAM；单测注入内存实现；
// 内核数据面 = UDP/67 socket 汇聚，见 internal/orchestrator/netkernel 的同名实现）。
type PuntTransport interface {
	// Recv 收一个上行包：返回 desc 与**原始包体**（可能带 L2 前导；解析时按 IP 版本 nibble 定位）。
	Recv() (PuntDesc, []byte, error)
	// Send 回注一个包：8 字节 desc（含 action/sw_if_index）+ 裸 IP 包。
	Send(desc PuntDesc, ipPacket []byte) error
	Close() error
}

// DNSProxyProvider 数据面 DNS 代理（自研域内转发器）编排。
type DNSProxyProvider struct {
	client    func() (PuntClient, error)
	transport func() (PuntTransport, error)
	upstream  func(server string, query []byte) ([]byte, error)
	switchOf  func(uint32) (string, bool) // sw_if_index → 所属交换机名（按域上游选择）

	clientSock string
	serverSock string

	mu         sync.Mutex
	global     []string
	perSwitch  map[string][]string
	registered bool          // VPP 侧 punt 注册是否在场
	tr         PuntTransport // 运行中的转发器传输（nil = 未起）

	servfail uint64 // 回 SERVFAIL 计数（原子；如实计数，不预检上游可达性）
	answered uint64 // 成功转发计数（原子）
	sendFail uint64 // 回注失败计数（原子）：报文没送回 VPP 也要如实计数，不静默
}

// NewDNSProxyProviderFunc 以 punt 客户端工厂构造（连接可重连）；传输与上游查询用真实实现。
func NewDNSProxyProviderFunc(f func() (PuntClient, error)) *DNSProxyProvider {
	return &DNSProxyProvider{
		client:     f,
		transport:  func() (PuntTransport, error) { return newUnixPuntTransport(DNSProxyClientSock, DNSProxyServerSock) },
		upstream:   hostUDPQuery,
		clientSock: DNSProxyClientSock,
		serverSock: DNSProxyServerSock,
	}
}

// NewDNSProxyProvider 以固定组件构造（单测）：punt 客户端/传输/上游查询/路径均可注入。
func NewDNSProxyProvider(c PuntClient, tr PuntTransport, up func(server string, query []byte) ([]byte, error), clientSock, serverSock string) *DNSProxyProvider {
	return &DNSProxyProvider{
		client:     func() (PuntClient, error) { return c, nil },
		transport:  func() (PuntTransport, error) { return tr, nil },
		upstream:   up,
		clientSock: clientSock,
		serverSock: serverSock,
	}
}

// SetSwitchResolver 注入「sw_if_index → 所属交换机」反查（由 L2Network 接到 L3Provider）。
func (p *DNSProxyProvider) SetSwitchResolver(fn func(uint32) (string, bool)) { p.switchOf = fn }

// SetTransport/SetUpstream 覆盖传输与上游查询实现（单测用；生产走默认）。
func (p *DNSProxyProvider) SetTransport(fn func() (PuntTransport, error)) { p.transport = fn }
func (p *DNSProxyProvider) SetUpstream(fn func(server string, query []byte) ([]byte, error)) {
	p.upstream = fn
}

// SERVFAILCount/ServedCount 转发器运行期计数（供诊断/单测；读视图不依赖它）。
func (p *DNSProxyProvider) SERVFAILCount() uint64 { return atomic.LoadUint64(&p.servfail) }
func (p *DNSProxyProvider) ServedCount() uint64   { return atomic.LoadUint64(&p.answered) }

// SendFailCount 回注失败计数（如实计数：回注失败不静默）。
func (p *DNSProxyProvider) SendFailCount() uint64 { return atomic.LoadUint64(&p.sendFail) }

// reset 标记 VPP 侧 punt 注册已丢失（连接（重）建立时由 resetProviders 调用）：VPP 重启后注册
// 不再在场，但 transport（nfvisd 自己的 socket）保留，下一次 Sync 只需重注册即可，声明非空会
// 重新注册（幂等）。不在此注销——VPP 已重启，注销只会失败。
func (p *DNSProxyProvider) reset() {
	p.mu.Lock()
	p.registered = false
	p.mu.Unlock()
}

// Close 优雅停用（nfvisd 退出调用）：注销 punt 并关掉转发器 socket。未停用即退出会把一个指向
// 已消失 socket 的注册留在 VPP 里 ⇒ 域内 DNS 黑洞（该节点 IS_DROP），故必须注销。
func (p *DNSProxyProvider) Close() error {
	return p.applyState(false)
}

// Sync 把数据面 DNS 代理声明收敛到运行态（决策 #345）：
//   - 全局或任一交换机非空 ⇒ 注册 punt socket 并起域内转发器；
//   - 全空 ⇒ 注销（恢复 VPP 默认处理）。
//
// 声明内容每次更新（转发器按最新声明选上游）；注册/传输只在「启用↔停用」切换时动作（幂等）。
func (p *DNSProxyProvider) Sync(ctx context.Context, want orchestrator.DNSProxyUpstreams) error {
	global := dedupeServers(want.Global)
	perSwitch := map[string][]string{}
	for name, s := range want.PerSwitch {
		if d := dedupeServers(s); len(d) > 0 {
			perSwitch[name] = d
		}
	}
	enabled := len(global) > 0 || len(perSwitch) > 0

	p.mu.Lock()
	p.global = global
	p.perSwitch = perSwitch
	tr, registered := p.tr, p.registered
	p.mu.Unlock()

	if enabled {
		if tr == nil {
			return p.applyState(true)
		}
		if registered {
			return nil // 已启用且已注册：声明内容已更新，转发器按最新声明选上游
		}
	}
	return p.applyState(enabled)
}

// applyState 收敛「注册 + 转发器」到指定启用态（幂等）。
func (p *DNSProxyProvider) applyState(enabled bool) error {
	p.mu.Lock()
	tr, registered := p.tr, p.registered
	p.mu.Unlock()

	if enabled {
		if tr == nil {
			t, err := p.transport()
			if err != nil {
				return fmt.Errorf("启动域内 DNS 转发器（绑定 %s）: %w", p.clientSock, err)
			}
			p.mu.Lock()
			p.tr = t
			p.mu.Unlock()
			go p.serve(t)
			tr = t
		}
		if !registered {
			c, err := p.client()
			if err != nil {
				return err
			}
			defer c.Close()
			if err := c.Register(p.clientSock, dnsProxyUDPPort); err != nil {
				return fmt.Errorf("注册 DNS punt socket（UDP/%d → %s）: %w；"+
					"若刚升级，请先执行 request vpp restart 让 startup.conf 的 punt 段生效", dnsProxyUDPPort, p.clientSock, err)
			}
			p.mu.Lock()
			p.registered = true
			p.mu.Unlock()
		}
		return nil
	}

	// 停用：先注销（恢复 VPP 默认处理），再关转发器 socket。
	if registered {
		c, err := p.client()
		if err != nil {
			return err
		}
		defer c.Close()
		if err := c.Deregister(dnsProxyUDPPort); err != nil {
			return fmt.Errorf("注销 DNS punt socket（UDP/%d）: %w", dnsProxyUDPPort, err)
		}
		p.mu.Lock()
		p.registered = false
		p.mu.Unlock()
	}
	if tr != nil {
		_ = tr.Close()
		p.mu.Lock()
		p.tr = nil
		p.mu.Unlock()
	}
	return nil
}

// serve 收包循环（转发器主循环）。传输被 Close 即退出；单次收包错误不静默中止（计数后继续）。
func (p *DNSProxyProvider) serve(tr PuntTransport) {
	for {
		desc, raw, err := tr.Recv()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// 收包错误：不静默中止，稍候重试（如实计数由 handle 承担）。
			time.Sleep(20 * time.Millisecond)
			continue
		}
		p.handle(tr, desc, raw)
	}
}

// handle 处理一个上行包：解析 → 选上游 → 转发 → 回注（失败回 SERVFAIL）。
func (p *DNSProxyProvider) handle(tr PuntTransport, desc PuntDesc, raw []byte) {
	ipPkt, ok := extractIP(raw)
	if !ok {
		return // 非 IP 包（不该出现）：不构造应答
	}
	q, ok := parseDNSQuery(ipPkt)
	if !ok {
		return // 非「指向 :53 的 IPv4/UDP」：不在覆盖内（注册只该收到这类包）
	}
	servers := p.serversFor(desc.SwIfIndex)
	var resp []byte
	if len(servers) == 0 {
		resp = buildServfail(q.payload)
		atomic.AddUint64(&p.servfail, 1)
	} else {
		resp = p.queryUpstreams(servers, q.payload)
		if resp == nil {
			resp = buildServfail(q.payload)
			atomic.AddUint64(&p.servfail, 1)
		} else {
			atomic.AddUint64(&p.answered, 1)
		}
	}
	reply := buildReplyIP4(q, resp)
	// 对称回注：action=PUNT_IP4_ROUTED、sw_if_index 取上行 desc 原值 ⇒ 按该接口所属表路由回客户端。
	// 回注失败如实计数（不静默吞掉）。
	if err := tr.Send(PuntDesc{SwIfIndex: desc.SwIfIndex, Action: dnsPuntActionIP4Routed}, reply); err != nil {
		atomic.AddUint64(&p.sendFail, 1)
	}
}

// serversFor 按上行包所属交换机选上游：按域优先，回落全局，皆空返回 nil（调用方回 SERVFAIL）。
func (p *DNSProxyProvider) serversFor(swIfIndex uint32) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.switchOf != nil {
		if name, ok := p.switchOf(swIfIndex); ok {
			if s := p.perSwitch[name]; len(s) > 0 {
				return s
			}
		}
	}
	return p.global
}

// queryUpstreams 按声明序尝试各上游（宿主网络栈）；全部失败返回 nil（调用方回 SERVFAIL）。
func (p *DNSProxyProvider) queryUpstreams(servers []string, query []byte) []byte {
	for _, s := range servers {
		resp, err := p.upstream(s, query)
		if err == nil && len(resp) > 0 {
			return resp
		}
	}
	return nil
}

// ---------- 传输层（AF_UNIX SOCK_DGRAM） ----------

type unixPuntTransport struct {
	conn       *net.UnixConn
	serverPath string
}

func newUnixPuntTransport(clientPath, serverPath string) (*unixPuntTransport, error) {
	if dir := filepath.Dir(clientPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建 %s: %w", dir, err)
		}
	}
	// 清陈旧 socket 文件：上次进程异常退出/重启会留下它，bind 会报 address already in use。
	_ = os.Remove(clientPath)
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: clientPath, Net: "unixgram"})
	if err != nil {
		return nil, fmt.Errorf("绑定 %s: %w", clientPath, err)
	}
	return &unixPuntTransport{conn: conn, serverPath: serverPath}, nil
}

func (t *unixPuntTransport) Recv() (PuntDesc, []byte, error) {
	buf := make([]byte, 65535)
	n, _, err := t.conn.ReadFromUnix(buf)
	if err != nil {
		return PuntDesc{}, nil, err
	}
	if n < 8 {
		return PuntDesc{}, nil, fmt.Errorf("上行包过短（%d 字节，缺 8 字节描述符）", n)
	}
	desc := PuntDesc{
		SwIfIndex: binary.LittleEndian.Uint32(buf[0:4]),
		Action:    binary.LittleEndian.Uint32(buf[4:8]),
	}
	return desc, append([]byte{}, buf[8:n]...), nil
}

func (t *unixPuntTransport) Send(desc PuntDesc, ipPacket []byte) error {
	buf := make([]byte, 8+len(ipPacket))
	binary.LittleEndian.PutUint32(buf[0:4], desc.SwIfIndex)
	binary.LittleEndian.PutUint32(buf[4:8], desc.Action)
	copy(buf[8:], ipPacket)
	if _, err := t.conn.WriteToUnix(buf, &net.UnixAddr{Name: t.serverPath, Net: "unixgram"}); err != nil {
		return fmt.Errorf("回注到 %s: %w", t.serverPath, err)
	}
	return nil
}

func (t *unixPuntTransport) Close() error { return t.conn.Close() }

// hostUDPQuery 用宿主网络栈向上游发一次 UDP 查询（不是 VPP FIB）。超时/失败返回错误。
func hostUDPQuery(server string, query []byte) ([]byte, error) {
	conn, err := net.DialTimeout("udp", net.JoinHostPort(server, "53"), dnsProxyUpstreamTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(dnsProxyUpstreamTimeout))
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return append([]byte{}, buf[:n]...), nil
}

// ---------- 最小 DNS/IP 报文处理 ----------

// dnsQuery 一次上行 UDP/53 查询的关键字段（只做转发所需的解析，不解析资源记录内容）。
type dnsQuery struct {
	srcIP   net.IP
	dstIP   net.IP
	srcPort uint16
	dstPort uint16
	payload []byte // DNS 报文（发往/来自上游的字节流）
}

// extractIP 从上行包体定位 IP 头。round124 实测：真实上行经 bridge-domain → BVI 进 IP 栈，
// 包体带 **14 字节以太网头**；而注入方不带 L2 时包体直接是 IP（前 14 字节可能是残留垃圾）。
// 三级判据：
//  1. 先认**以太网类型**（raw[12:14] = 0x0800/0x86dd）——上行包的目的 MAC 是产品自己的 BVI MAC，
//     其首字节不可假定；ethertype 是与 MAC 取值无关的可信判据；
//  2. 再退回「首字节版本 nibble」（无 L2 的包直接命中）；
//  3. 最后按 14 字节偏移试「版本 nibble」（ethertype 非标准时的兜底）。
func extractIP(raw []byte) ([]byte, bool) {
	if len(raw) > 14 {
		if et := binary.BigEndian.Uint16(raw[12:14]); et == 0x0800 || et == 0x86dd {
			if v := raw[14] >> 4; v == 4 || v == 6 {
				return raw[14:], true
			}
		}
	}
	if len(raw) > 0 {
		if v := raw[0] >> 4; v == 4 || v == 6 {
			return raw, true
		}
	}
	if len(raw) > 14 {
		if v := raw[14] >> 4; v == 4 || v == 6 {
			return raw[14:], true
		}
	}
	return nil, false
}

// parseDNSQuery 解析一个 IPv4/UDP 且目的端口为 53 的查询。非该类（含 IPv6/TCP/非 53）返回 false。
func parseDNSQuery(ipPkt []byte) (dnsQuery, bool) {
	var q dnsQuery
	if len(ipPkt) < 20 || ipPkt[0]>>4 != 4 {
		return q, false
	}
	ihl := int(ipPkt[0]&0x0f) * 4
	if ihl < 20 || len(ipPkt) < ihl+8 {
		return q, false
	}
	if ipPkt[9] != 17 { // 仅 UDP
		return q, false
	}
	total := int(binary.BigEndian.Uint16(ipPkt[2:4]))
	if total < ihl+8 || total > len(ipPkt) {
		total = len(ipPkt)
	}
	q.srcIP = net.IP(append([]byte{}, ipPkt[12:16]...))
	q.dstIP = net.IP(append([]byte{}, ipPkt[16:20]...))
	udp := ipPkt[ihl:total]
	q.srcPort = binary.BigEndian.Uint16(udp[0:2])
	q.dstPort = binary.BigEndian.Uint16(udp[2:4])
	udpLen := int(binary.BigEndian.Uint16(udp[4:6]))
	if udpLen < 8 || udpLen > len(udp) {
		udpLen = len(udp)
	}
	if q.dstPort != dnsProxyUDPPort {
		return q, false
	}
	q.payload = append([]byte{}, udp[8:udpLen]...)
	return q, true
}

// buildReplyIP4 由「查询 + DNS 应答」构造回注的裸 IPv4/UDP 包：源/目的取查询的反向（网关→客户端）。
// VLAN/以太头由 VPP 回注时自行重写，故此处不带 L2。
func buildReplyIP4(q dnsQuery, dnsResp []byte) []byte {
	total := 20 + 8 + len(dnsResp)
	pkt := make([]byte, total)
	pkt[0] = 0x45 // IPv4, IHL=5
	binary.BigEndian.PutUint16(pkt[2:4], uint16(total))
	pkt[8] = 64 // TTL
	pkt[9] = 17 // UDP
	copy(pkt[12:16], q.dstIP.To4())
	copy(pkt[16:20], q.srcIP.To4())
	binary.BigEndian.PutUint16(pkt[10:12], ipChecksum(pkt[:20]))
	udp := pkt[20:]
	binary.BigEndian.PutUint16(udp[0:2], dnsProxyUDPPort) // 源端口 53（来自网关地址）
	binary.BigEndian.PutUint16(udp[2:4], q.srcPort)       // 目的端口 = 客户端源端口
	binary.BigEndian.PutUint16(udp[4:6], uint16(8+len(dnsResp)))
	copy(udp[8:], dnsResp)
	// UDP 校验和按 IPv4 伪头**如实计算**：IPv4 虽允许置 0（RFC 768），但回注包经 vhost-user 送到
	// guest，取包方的校验和语义不应依赖「0 = 未计算」的宽容，故算真值（结 round124 spike 的形态）。
	binary.BigEndian.PutUint16(udp[6:8], udpChecksum(pkt[12:16], pkt[16:20], udp))
	return pkt
}

// checksum16 一补数求和取反（IPv4 头 / UDP 伪头共用的核心）。
func checksum16(data []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(data); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(data[i : i+2]))
	}
	if len(data)%2 == 1 {
		sum += uint32(data[len(data)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// ipChecksum 计算 IPv4 头校验和。
func ipChecksum(hdr []byte) uint16 { return checksum16(hdr) }

// udpChecksum 计算 UDP 校验和（含 IPv4 伪头：src/dst/零/协议/长度）。段内校验和字段须先为 0。
// 结果为 0 时按 RFC 768 取 0xffff 传输（0 在 UDP 里表示「未计算」，不能与真值 0 混淆）。
func udpChecksum(src, dst, seg []byte) uint16 {
	buf := make([]byte, 0, 12+len(seg))
	buf = append(buf, src...)
	buf = append(buf, dst...)
	buf = append(buf, 0, 17)
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(seg)))
	buf = append(buf, l[:]...)
	buf = append(buf, seg...)
	if s := checksum16(buf); s != 0 {
		return s
	}
	return 0xffff
}

// buildServfail 由查询报文构造一个 SERVFAIL 应答（快速失败，不静默丢弃）：
// 翻转 QR、置 RCODE=SERVFAIL，清空 AN/NS/AR 计数，回显 question 段（transaction ID 原样）。
func buildServfail(query []byte) []byte {
	resp := make([]byte, 12)
	if len(query) >= 12 {
		copy(resp, query[:12])
	}
	flags := binary.BigEndian.Uint16(resp[2:4])
	flags |= 0x8000  // QR=1（应答）
	flags &^= 0x000f // 清 RCODE
	flags |= 0x0002  // RCODE=SERVFAIL
	binary.BigEndian.PutUint16(resp[2:4], flags)
	if end, ok := dnsQuestionEnd(query); ok {
		resp = append(resp, query[12:end]...)
	} else {
		// question 段解析不出（畸形报文）：只回 12 字节头并清 QDCOUNT，仍是合法 SERVFAIL。
		binary.BigEndian.PutUint16(resp[4:6], 0)
	}
	binary.BigEndian.PutUint16(resp[6:8], 0)  // ANCOUNT
	binary.BigEndian.PutUint16(resp[8:10], 0) // NSCOUNT
	binary.BigEndian.PutUint16(resp[10:12], 0)
	return resp
}

// dnsQuestionEnd 返回 question 段结束的偏移（用于 SERVFAIL 构造与应答匹配的最小解析）。
func dnsQuestionEnd(msg []byte) (int, bool) {
	if len(msg) < 12 {
		return 0, false
	}
	qd := int(binary.BigEndian.Uint16(msg[4:6]))
	pos := 12
	for i := 0; i < qd; i++ {
		for {
			if pos >= len(msg) {
				return 0, false
			}
			b := msg[pos]
			if b == 0 {
				pos++
				break
			}
			if b&0xc0 == 0xc0 { // 压缩指针
				pos += 2
				break
			}
			pos += 1 + int(b)
		}
		pos += 4 // qtype + qclass
		if pos > len(msg) {
			return 0, false
		}
	}
	return pos, true
}

// dedupeServers 去重并剔除空串（保持声明序）。
func dedupeServers(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
