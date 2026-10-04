package network

// 决策 #359：DHCP 服务器（域内租约池/状态机）的单元测试。
//
// 覆盖口径（附录 A #359 的测试清单）：
//   - tap 名生成（≤15 字符 IFNAMSIZ、按交换机名确定性）；
//   - 租约状态机纯函数（分配/同 MAC 优先续用/最小可用、commit、release、decline 隔离一个租期、
//     过期回收、setPool 裁剪、restore 只收池内未到期条目）；
//   - 报文解析/构造（DISCOVER→OFFER 字段逐项、NAK 带 server-id 不带租约选项、非 DHCP 帧忽略）；
//   - provider 生命周期（fake VPP client：tap 创建/删除/入 BD/置 up；punt **每次 Sync 都重申**
//     ——R140-1 硬口径；恢复重放按 HostIfName 复用存量 tap；池耗尽告警建/消；租约文件
//     原子持久化与损坏回退）。
//
// 底座交互全部走假实现（仓库纪律：govpp/内核藏在 Provider 接口后，单测用 mock）。

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
)

// ---------- 假实现 ----------

// fakeDHCPTap 内存 tap：Send 记录帧；Recv 阻塞到 Close（收包协程原地驻留，不空转）。
type fakeDHCPTap struct {
	mu     sync.Mutex
	sent   [][]byte
	closed chan struct{}
}

func (t *fakeDHCPTap) Recv() ([]byte, error) {
	<-t.closed
	return nil, net.ErrClosed
}
func (t *fakeDHCPTap) Send(f []byte) error {
	t.mu.Lock()
	t.sent = append(t.sent, append([]byte{}, f...))
	t.mu.Unlock()
	return nil
}
func (t *fakeDHCPTap) MAC() net.HardwareAddr { mac, _ := net.ParseMAC("02:fe:00:00:00:99"); return mac }
func (t *fakeDHCPTap) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed != nil {
		select {
		case <-t.closed:
		default:
			close(t.closed)
		}
	}
	return nil
}
func (t *fakeDHCPTap) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	select {
	case <-t.closed:
		return true
	default:
		return false
	}
}
func (t *fakeDHCPTap) frames() [][]byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([][]byte{}, t.sent...)
}

// fakeDHCPServerClient VPP 侧 tap 生命周期的内存实现（TapDump 只回 HostIfName——**不回 tag**，
// 与真机实测一致，钉住「恢复查找只能按 HostIfName」）。
type fakeDHCPServerClient struct {
	mu      sync.Mutex
	next    uint32
	taps    map[uint32]string // sw_if_index → host_if_name
	bridged map[string]bool   // "idx:bd" → 已入 bridge-domain
	up      map[uint32]bool
	deleted []uint32
}

func newFakeDHCPServerClient() *fakeDHCPServerClient {
	return &fakeDHCPServerClient{next: 100, taps: map[uint32]string{}, bridged: map[string]bool{}, up: map[uint32]bool{}}
}

func (c *fakeDHCPServerClient) TapCreate(hostIfName, _ string) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	c.taps[c.next] = hostIfName
	return c.next, nil
}
func (c *fakeDHCPServerClient) TapDelete(idx uint32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.taps[idx]; !ok {
		return fmt.Errorf("tap_delete_v2 retval=-2（No such interface）")
	}
	delete(c.taps, idx)
	c.deleted = append(c.deleted, idx)
	return nil
}
func (c *fakeDHCPServerClient) TapDump() ([]TapInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]TapInfo, 0, len(c.taps))
	for idx, h := range c.taps {
		out = append(out, TapInfo{SwIfIndex: idx, HostIfName: h})
	}
	return out, nil
}
func (c *fakeDHCPServerClient) SetL2Bridge(idx, bd uint32, enable bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := fmt.Sprintf("%d:%d", idx, bd)
	if enable {
		c.bridged[key] = true
	} else {
		delete(c.bridged, key)
	}
	return nil
}
func (c *fakeDHCPServerClient) SetState(idx uint32, up bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.up[idx] = up
	return nil
}
func (c *fakeDHCPServerClient) Close() {}
func (c *fakeDHCPServerClient) bridgedOK(idx, bd uint32) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bridged[fmt.Sprintf("%d:%d", idx, bd)]
}
func (c *fakeDHCPServerClient) wipe() { // 模拟 VPP 重启后一切运行态消失
	c.mu.Lock()
	defer c.mu.Unlock()
	c.taps = map[uint32]string{}
	c.bridged = map[string]bool{}
	c.up = map[uint32]bool{}
}

// fakeClock 可推进的时钟（租约到期/隔离回收的确定性）。
type fakeClock struct {
	mu   sync.Mutex
	curr time.Time
}

func newFakeClock(t0 time.Time) *fakeClock { return &fakeClock{curr: t0} }
func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.curr
}
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.curr = c.curr.Add(d)
	c.mu.Unlock()
}

// tapFactory 记录打开过的内核侧名（每台交换机一个独立内存 tap）。
type tapFactory struct {
	mu    sync.Mutex
	taps  map[string]*fakeDHCPTap
	fails int // 开头 N 次失败（tap 刚建、内核 netdev 未就绪的重试路径）
}

func newTapFactory() *tapFactory { return &tapFactory{taps: map[string]*fakeDHCPTap{}} }
func (f *tapFactory) open(name string) (dhcpTapTransport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails > 0 {
		f.fails--
		return nil, fmt.Errorf("内核侧接口 %s 不存在（注入失败）", name)
	}
	if t := f.taps[name]; t != nil {
		return t, nil
	}
	t := &fakeDHCPTap{closed: make(chan struct{})}
	f.taps[name] = t
	return t, nil
}
func (f *tapFactory) get(name string) *fakeDHCPTap {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.taps[name]
}

// newTestDHCPServer 注入全部假实现的 provider。
func newTestDHCPServer(t *testing.T, c *fakeDHCPServerClient, fp *fakePunt, factory *tapFactory, leaseDir string) (*DHCPServerProvider, *fakeClock) {
	t.Helper()
	p := NewDHCPServerProviderFunc(
		func() (DHCPServerClient, error) { return c, nil },
		func() (PuntClient, error) { return fp, nil },
	)
	p.SetPuntTransport(func() (dnsPuntTransport, error) {
		return &fakeTransport{}, nil // Recv 立即 ErrClosed：serve 协程直接退出，单测直调 handle*
	})
	p.SetTapOpen(factory.open)
	p.SetLeaseDir(leaseDir)
	fc := newFakeClock(time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	p.SetClock(fc.now)
	return p, fc
}

// vsDHCPServer 一台已配 DHCP 服务器的测试交换机（L2 + IPv4 BVI + 3 地址小池）。
func vsDHCPServer() model.VirtualSwitch {
	return model.VirtualSwitch{
		Name: "vs-x", Type: "l2",
		Gateway:                    &model.VSGateway{Addresses: []string{"192.168.100.1/24"}},
		DhcpServerPoolStart:        "192.168.100.10",
		DhcpServerPoolEnd:          "192.168.100.12",
		DhcpServerLeaseTimeSeconds: 7200,
	}
}

// dhcpClientFrame 构造一条客户端 → 服务器的以太帧（IPv4/UDP/67 + BOOTREQUEST）。
// opt50=option 50（requested IP）、opt54=option 54（server-id）、ciaddr=已配置地址（RENEW 形态）。
// testFrameXID 让每条测试帧带**不同的 xid**：真实客户端每个交换（DISCOVER/每次续租）用新 xid，
// 服务器的双入径去重（dhcpDedupeWindow）按 (chaddr,xid,类型) 判重——固定 xid 会让「同一测试里
// 先 SELECTING REQUEST 再 punt 续租」这类序列被误判为重复（round141 实测后补的口径）。
var testFrameXID atomic.Uint32

func dhcpClientFrame(chaddr net.HardwareAddr, msgType byte, opt50, opt54, ciaddr net.IP) []byte {
	body := make([]byte, 240)
	body[0], body[1], body[2] = 1, 1, 6 // BOOTREQUEST / Ethernet / 6 字节硬件地址
	binary.BigEndian.PutUint32(body[4:8], 0x12345678+testFrameXID.Add(1))
	copy(body[28:34], chaddr)
	if ciaddr != nil {
		copy(body[12:16], ciaddr.To4())
	}
	binary.BigEndian.PutUint32(body[236:240], dhcpMagicCookie)
	opts := []byte{dhcpOptMsgType, 1, msgType}
	if opt50 != nil {
		opts = append(opts, dhcpOptRequestedIP, 4)
		opts = append(opts, opt50.To4()...)
	}
	if opt54 != nil {
		opts = append(opts, dhcpOptServerID, 4)
		opts = append(opts, opt54.To4()...)
	}
	opts = append(opts, dhcpOptEnd)
	body = append(body, opts...)

	udp := make([]byte, 8)
	binary.BigEndian.PutUint16(udp[0:2], dhcpClientPort)
	binary.BigEndian.PutUint16(udp[2:4], dhcpServerPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(8+len(body)))

	ip := make([]byte, 20)
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(20+8+len(body)))
	ip[8], ip[9] = 64, 17
	copy(ip[12:16], net.IPv4zero.To4())
	copy(ip[16:20], net.IPv4bcast.To4())
	binary.BigEndian.PutUint16(ip[10:12], ipChecksum(ip))

	frame := make([]byte, 14)
	copy(frame[0:6], net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	binary.BigEndian.PutUint16(frame[12:14], 0x0800)
	frame = append(frame, ip...)
	frame = append(frame, udp...)
	return append(frame, body...)
}

// findOption 从应答报文主体里取指定选项（测试断言用）。
func findOption(t *testing.T, body []byte, code byte) []byte {
	t.Helper()
	for i := 240; i < len(body); {
		if body[i] == 0 {
			i++
			continue
		}
		if body[i] == dhcpOptEnd {
			return nil
		}
		if i+1 >= len(body) {
			return nil
		}
		l := int(body[i+1])
		if body[i] == code {
			return body[i+2 : i+2+l]
		}
		i += 2 + l
	}
	return nil
}

// replyFields 一条应答以太帧的关键字段（以太地址/IP 源/BOOTP 主体）。
type replyFields struct {
	ethDst, ethSrc net.HardwareAddr
	ipSrc          net.IP
	body           []byte
}

func parseReplyFrame(t *testing.T, f []byte) replyFields {
	t.Helper()
	if len(f) < 14+20+8+240 {
		t.Fatalf("应答帧太短: %d", len(f))
	}
	var r replyFields
	r.ethDst, r.ethSrc = net.HardwareAddr(f[0:6]), net.HardwareAddr(f[6:12])
	r.ipSrc = net.IP(f[26:30]).To4()
	r.body = f[14+20+8:]
	if binary.BigEndian.Uint32(r.body[236:240]) != dhcpMagicCookie {
		t.Fatal("应答缺 magic cookie")
	}
	return r
}

func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	m, err := net.ParseMAC(s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// ---------- tap 名 ----------

func TestDHCPServerTapName(t *testing.T) {
	for _, n := range []string{"vs-x", "sem-vs", "a-very-long-switch-name-0123456789", "vs.core-01"} {
		got := DHCPServerTapName(n)
		if len(got) != 15 || got[:7] != "nfvisdh" {
			t.Fatalf("tap 名 %q 不合规（须 nfvisdh+8 十六进制，共 15 字符 = IFNAMSIZ 上限）", got)
		}
		if DHCPServerTapName(n) != got {
			t.Fatal("tap 名必须确定性（恢复期按 HostIfName 找存量）")
		}
	}
	if DHCPServerTapName("a") == DHCPServerTapName("b") {
		t.Fatal("不同交换机名应得到不同 tap 名")
	}
}

// ---------- 租约表（纯函数） ----------

// poolOf 点分地址 → 32 位区间（表内算法与 model.DHCPServerPoolRange 同一事实源）。
func poolOf(t *testing.T, lo, hi string) (uint32, uint32) {
	t.Helper()
	lv, ok1 := ipToU32(lo)
	hv, ok2 := ipToU32(hi)
	if !ok1 || !ok2 {
		t.Fatalf("测试池地址非法: %s-%s", lo, hi)
	}
	return lv, hv
}

func TestDHCPLeaseTableAllocate(t *testing.T) {
	fc := newFakeClock(time.Unix(1_000_000, 0))
	lo, hi := poolOf(t, "192.168.100.10", "192.168.100.12")
	tt := newDHCPLeaseTable(lo, hi, fc.now)

	// 同 MAC 优先续用原地址（两次 DISCOVER 拿到同一个）
	ip1, ok := tt.allocate("aa:aa:aa:aa:aa:01")
	if !ok || ip1 != "192.168.100.10" {
		t.Fatalf("首个 DISCOVER 应得最小地址 192.168.100.10，实得 %v/%v", ip1, ok)
	}
	if ip2, _ := tt.allocate("aa:aa:aa:aa:aa:01"); ip2 != ip1 {
		t.Fatalf("同 MAC 应续用原地址：%s vs %s", ip1, ip2)
	}
	// 不同 MAC → 下一最小地址
	if ip, _ := tt.allocate("aa:aa:aa:aa:aa:02"); ip != "192.168.100.11" {
		t.Fatalf("第二个客户端应得 192.168.100.11，实得 %s", ip)
	}
	// OFFER 保持到期后地址回池（其他客户端可拿最小地址）
	fc.advance(3 * time.Minute)
	if ip, _ := tt.allocate("aa:aa:aa:aa:aa:03"); ip != "192.168.100.10" {
		t.Fatalf("OFFER 保持到期应回池（最小地址），实得 %s", ip)
	}
	// 池耗尽
	tt.allocate("aa:aa:aa:aa:aa:04")
	tt.allocate("aa:aa:aa:aa:aa:05")
	if _, ok := tt.allocate("aa:aa:aa:aa:aa:06"); ok {
		t.Fatal("3 地址池全占后不应再分配")
	}
	if tt.free() {
		t.Fatal("耗尽后 free() 应为 false")
	}
}

func TestDHCPLeaseTableCommitReleaseDecline(t *testing.T) {
	fc := newFakeClock(time.Unix(1_000_000, 0))
	lo, hi := poolOf(t, "192.168.100.10", "192.168.100.20")
	tt := newDHCPLeaseTable(lo, hi, fc.now)

	// commit 后 active；换地址时旧地址立即回池
	tt.commit("aa:00:00:00:00:01", "192.168.100.10", time.Hour)
	if l := tt.lookupMAC("aa:00:00:00:00:01"); l == nil || l.State != dhcpLeaseActive {
		t.Fatalf("commit 后应 active: %+v", l)
	}
	tt.commit("aa:00:00:00:00:01", "192.168.100.11", time.Hour)
	if l := tt.ownerOf("192.168.100.10"); l != nil {
		t.Fatalf("换地址后旧地址应回池: %+v", l)
	}
	// release 立即回池
	tt.release("aa:00:00:00:00:01")
	if l := tt.lookupMAC("aa:00:00:00:00:01"); l != nil {
		t.Fatalf("release 后应无租约: %+v", l)
	}

	// decline 隔离一个租期：隔离期内不可分配、到期即回池
	tt.commit("aa:00:00:00:00:02", "192.168.100.12", time.Hour)
	tt.release("aa:00:00:00:00:02")
	if !tt.decline("192.168.100.12", time.Hour) {
		t.Fatal("decline 空闲地址也应记录（防声明冲突后立即复用回环）")
	}
	if l := tt.ownerOf("192.168.100.12"); l == nil || l.State != dhcpLeaseDeclined {
		t.Fatalf("192.168.100.12 应处于 declined: %+v", l)
	}
	fc.advance(2 * time.Hour)
	if ip, ok := tt.allocate("aa:00:00:00:00:03"); !ok || ip != "192.168.100.10" {
		t.Fatalf("隔离到期后最小地址应可用，实得 %s/%v", ip, ok)
	}

	// 隔离期内重复 decline：不再变化（不刷新隔离期，如实记录第一次判定）
	fc2 := newFakeClock(time.Unix(2_000_000, 0))
	lo2, hi2 := poolOf(t, "192.168.100.10", "192.168.100.20")
	t2 := newDHCPLeaseTable(lo2, hi2, fc2.now)
	if !t2.decline("192.168.100.10", time.Hour) {
		t.Fatal("首次 decline 应有变化")
	}
	if t2.decline("192.168.100.10", time.Hour) {
		t.Fatal("隔离期内重复 decline 不应有变化")
	}
}

func TestDHCPLeaseTableSetPoolAndRestore(t *testing.T) {
	fc := newFakeClock(time.Unix(1_000_000, 0))
	lo, hi := poolOf(t, "192.168.100.10", "192.168.100.20")
	tt := newDHCPLeaseTable(lo, hi, fc.now)
	tt.commit("aa:00:00:00:00:01", "192.168.100.11", time.Hour)
	tt.commit("aa:00:00:00:00:02", "192.168.100.30", time.Hour) // 池外（模拟换池前遗留）

	// 换小池：池外条目直接移除（已不可能续租），池内保留
	lo2, hi2 := poolOf(t, "192.168.100.10", "192.168.100.12")
	tt.setPool(lo2, hi2)
	if tt.ownerOf("192.168.100.30") != nil {
		t.Fatal("换池后池外条目应移除")
	}
	if tt.ownerOf("192.168.100.11") == nil {
		t.Fatal("换池后池内条目应保留")
	}

	// restore：只收池内、未到期、状态合法的条目
	fc.advance(time.Hour) // 上面两条已到期
	freshLo, freshHi := poolOf(t, "192.168.100.10", "192.168.100.12")
	fresh := newDHCPLeaseTable(freshLo, freshHi, fc.now)
	now := fc.now()
	fresh.restore([]dhcpLease{
		{MAC: "aa:00:00:00:00:01", IP: "192.168.100.10", State: dhcpLeaseActive, ExpiresAt: now.Add(time.Hour)},
		{MAC: "aa:00:00:00:00:02", IP: "192.168.100.11", State: dhcpLeaseActive, ExpiresAt: now.Add(-time.Minute)}, // 已到期
		{MAC: "aa:00:00:00:00:03", IP: "192.168.100.99", State: dhcpLeaseActive, ExpiresAt: now.Add(time.Hour)},    // 池外
		{MAC: "aa:00:00:00:00:04", IP: "192.168.100.12", State: dhcpLeaseActive, ExpiresAt: now.Add(time.Hour)},    // 合法
		{MAC: "aa:00:00:00:00:05", IP: "192.168.100.30", State: dhcpLeaseActive, ExpiresAt: now.Add(time.Hour)},    // 池外
	})
	if l := fresh.ownerOf("192.168.100.10"); l == nil || l.MAC != "aa:00:00:00:00:01" {
		t.Fatalf("restore 应收下池内未到期条目: %+v", l)
	}
	if fresh.ownerOf("192.168.100.99") != nil {
		t.Fatal("restore 不应收池外条目")
	}
	if fresh.lookupMAC("aa:00:00:00:00:05") != nil {
		t.Fatal("restore 不应收池外条目（30）")
	}
	if n := len(fresh.snapshot()); n != 2 {
		t.Fatalf("快照应含 2 条（10/12），实得 %d: %+v", n, fresh.snapshot())
	}
}

// ---------- 报文 ----------

func TestDHCPDiscoverOfferRoundTrip(t *testing.T) {
	chaddr := mustMAC(t, "aa:bb:cc:dd:ee:01")
	tapMAC := mustMAC(t, "02:fe:00:00:00:01")
	bvi := net.ParseIP("192.168.100.1")
	spec := dhcpReplySpec{
		serverMAC: tapMAC, bvi: bvi,
		mask: net.CIDRMask(24, 32), dns: net.ParseIP("192.168.100.1"),
		domain: "lab.local", lease: 7200,
	}
	msg, ok := parseDHCPEtherFrame(dhcpClientFrame(chaddr, dhcpDiscover, nil, nil, nil))
	if !ok || msg.msgType != dhcpDiscover {
		t.Fatalf("DISCOVER 解析失败: %+v/%v", msg, ok)
	}
	if string(msg.chaddr) != string(chaddr) {
		t.Fatalf("chaddr 不符: %s", msg.chaddr)
	}
	frame := buildDHCPReply(msg, dhcpOffer, net.ParseIP("192.168.100.10"), spec)
	r := parseReplyFrame(t, frame)
	if string(r.ethDst) != string(chaddr) {
		t.Fatalf("以太目的应为客户端 chaddr（单播，BD 按 MAC 交换）: %s", r.ethDst)
	}
	if string(r.ethSrc) != string(tapMAC) {
		t.Fatalf("以太源应为内核 tap MAC: %s", r.ethSrc)
	}
	if !r.ipSrc.Equal(bvi) {
		t.Fatalf("IP 源应为 BVI 地址: %s", r.ipSrc)
	}
	if mt := findOption(t, r.body, dhcpOptMsgType); len(mt) != 1 || mt[0] != dhcpOffer {
		t.Fatal("应为 OFFER（option 53=2）")
	}
	if sid := findOption(t, r.body, dhcpOptServerID); sid == nil || !net.IP(sid).Equal(bvi) {
		t.Fatalf("server-id 应为 BVI: %v", sid)
	}
	if lt := findOption(t, r.body, dhcpOptLeaseTime); lt == nil || binary.BigEndian.Uint32(lt) != 7200 {
		t.Fatalf("租约时长应为 7200: %v", lt)
	}
	if m := findOption(t, r.body, dhcpOptSubnetMask); m == nil || !net.IP(m).Equal(net.ParseIP("255.255.255.0")) {
		t.Fatalf("子网掩码应为 BVI 前缀: %v", m)
	}
	if gw := findOption(t, r.body, dhcpOptRouter); gw == nil || !net.IP(gw).Equal(bvi) {
		t.Fatalf("router 应为 BVI: %v", gw)
	}
	if d := findOption(t, r.body, dhcpOptDNS); d == nil || !net.IP(d).Equal(net.ParseIP("192.168.100.1")) {
		t.Fatalf("DNS 应为配置值: %v", d)
	}
	if dn := findOption(t, r.body, dhcpOptDomainName); dn == nil || string(dn) != "lab.local" {
		t.Fatalf("域名应为配置值: %v", dn)
	}

	// NAK：带 server-id、不发租约/掩码/网关等选项（RFC 2131 §4.3.2）
	nak := buildDHCPReply(msg, dhcpNak, nil, spec)
	nr := parseReplyFrame(t, nak)
	if mt := findOption(t, nr.body, dhcpOptMsgType); len(mt) != 1 || mt[0] != dhcpNak {
		t.Fatal("应为 NAK")
	}
	if sid := findOption(t, nr.body, dhcpOptServerID); sid == nil {
		t.Fatal("NAK 也要带 server-id")
	}
	if findOption(t, nr.body, dhcpOptLeaseTime) != nil || findOption(t, nr.body, dhcpOptSubnetMask) != nil {
		t.Fatal("NAK 不应带租约/掩码等选项")
	}
}

func TestParseDHCPRejectsNonDHCP(t *testing.T) {
	// ARP（ethertype 0x0806）与带 VLAN 标签的帧不在覆盖内：一律忽略
	f := make([]byte, 42)
	binary.BigEndian.PutUint16(f[12:14], 0x0806)
	if _, ok := parseDHCPEtherFrame(f); ok {
		t.Fatal("ARP 应被忽略")
	}
	vlan := make([]byte, 18)
	binary.BigEndian.PutUint16(vlan[12:14], 0x8100)
	if _, ok := parseDHCPEtherFrame(vlan); ok {
		t.Fatal("VLAN 标签帧应被忽略")
	}
}

// ---------- provider 生命周期 ----------

func newEnabledProvider(t *testing.T) (*DHCPServerProvider, *fakeDHCPServerClient, *fakePunt, *tapFactory, *fakeClock) {
	t.Helper()
	c := newFakeDHCPServerClient()
	fp := &fakePunt{}
	factory := newTapFactory()
	p, fc := newTestDHCPServer(t, c, fp, factory, t.TempDir())
	return p, c, fp, factory, fc
}

func TestDHCPServerProviderSyncLifecycle(t *testing.T) {
	p, c, fp, factory, _ := newEnabledProvider(t)
	ctx := context.Background()
	vs := vsDHCPServer()

	// 启用：建 tap（按 HostIfName 查存量 → 无则建）、入 BD、置 up、注册 punt、打开内核侧
	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	tapName := DHCPServerTapName(vs.Name)
	if factory.get(tapName) == nil {
		t.Fatalf("应打开内核侧 tap %s", tapName)
	}
	if fp.registers != 1 || !fp.registered || fp.port != dhcpServerPort {
		t.Fatalf("首次 Sync 应注册 UDP/67: %+v", fp)
	}
	if fp.path != DHCPServerClientSock {
		t.Fatalf("注册路径应为产品 client socket: %s", fp.path)
	}
	var idx uint32
	for _, ti := range mustDump(t, c) {
		if ti.HostIfName == tapName {
			idx = ti.SwIfIndex
		}
	}
	if idx == 0 {
		t.Fatal("VPP 侧应有该 tap")
	}
	bdID := BDID(vs.Name)
	if !c.bridgedOK(idx, bdID) || !c.up[idx] {
		t.Fatalf("tap 应入 BD %d 且置 up", bdID)
	}
	if _, ok := p.Leases(vs.Name); !ok {
		t.Fatal("收敛后租约读视图应可用")
	}
	if n, ok := p.ActiveLeases(vs.Name); !ok || n != 0 {
		t.Fatalf("初始在租数应为 0: %d/%v", n, ok)
	}
	if !p.TapIndexes()[idx] {
		t.Fatalf("TapIndexes 应含 %d: %v", idx, p.TapIndexes())
	}

	// 重复 Sync（幂等）：不重复建 tap；**punt 每次都重申**（R140-1：relay 的 proxy 会夺走注册）
	nBefore := len(mustDump(t, c))
	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("二次 Sync: %v", err)
	}
	if len(mustDump(t, c)) != nBefore {
		t.Fatalf("重复 Sync 不应再建 tap: %d → %d", nBefore, len(mustDump(t, c)))
	}
	if fp.registers != 2 || fp.deregisters != 2 {
		t.Fatalf("每次 Sync 都应重申注册（先撤再注，两次 Sync 各一对）: %+v", fp)
	}

	// 停用（声明无池＝teardown）：删 tap（VPP 侧 + 内核侧）、清租约文件；最后一台停用注销 punt
	if err := p.Sync(ctx, model.VirtualSwitch{Name: vs.Name}); err != nil {
		t.Fatalf("停用 Sync: %v", err)
	}
	if !factory.get(tapName).isClosed() {
		t.Fatal("停用应关闭内核侧 tap")
	}
	if len(c.deleted) != 1 || c.deleted[0] != idx {
		t.Fatalf("停用应删 VPP 侧 tap: %v", c.deleted)
	}
	if fp.registered || fp.deregisters != 3 {
		t.Fatalf("最后一台停用应注销 punt: %+v", fp)
	}
	if _, ok := p.Leases(vs.Name); ok {
		t.Fatal("停用后租约读视图应不可用")
	}
}

func mustDump(t *testing.T, c *fakeDHCPServerClient) []TapInfo {
	t.Helper()
	dumps, err := c.TapDump()
	if err != nil {
		t.Fatal(err)
	}
	return dumps
}

func TestDHCPServerProviderRestoresExistingTapByHostIfName(t *testing.T) {
	// 恢复重放：VPP 里已有同名 HostIfName 的 tap ⇒ 按 HostIfName 找到并复用（不新建）；
	// tag 只写不读（真机实测 dump 不回 tag），复用的 tap 也要重新入 BD/置 up。
	c := newFakeDHCPServerClient()
	fp := &fakePunt{}
	factory := newTapFactory()
	p, _ := newTestDHCPServer(t, c, fp, factory, t.TempDir())
	ctx := context.Background()
	vs := vsDHCPServer()
	tapName := DHCPServerTapName(vs.Name)

	existing, _ := c.TapCreate(tapName, "whatever-tag")
	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	dumps := mustDump(t, c)
	if len(dumps) != 1 || dumps[0].SwIfIndex != existing {
		t.Fatalf("应复用存量 tap 而非新建: %+v（期望 idx=%d）", dumps, existing)
	}
	if !c.bridgedOK(existing, BDID(vs.Name)) || !c.up[existing] {
		t.Fatal("复用的 tap 也要重新入 BD 并置 up")
	}
}

func TestDHCPServerProviderResetThenReplay(t *testing.T) {
	p, c, fp, factory, _ := newEnabledProvider(t)
	ctx := context.Background()
	vs := vsDHCPServer()
	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	tapName := DHCPServerTapName(vs.Name)
	var firstIdx uint32
	for _, ti := range mustDump(t, c) {
		if ti.HostIfName == tapName {
			firstIdx = ti.SwIfIndex
		}
	}
	if firstIdx == 0 {
		t.Fatal("VPP 侧应有该 tap")
	}

	// VPP 重启：连接重置 ⇒ 内核侧 tap 关闭、注册标记失效（VPP 侧注册本身已随重启消失，
	// 无需真发注销）；**租约表保留**（服务器自己的状态）
	p.reset()
	if !factory.get(tapName).isClosed() {
		t.Fatal("reset 应关闭内核侧 tap 传输")
	}
	if _, ok := p.Leases(vs.Name); !ok {
		t.Fatal("租约表应随 reset 保留")
	}
	// VPP 侧对象已随重启消失（模拟：清掉 fake 的 tap/BD 运行态）
	c.wipe()

	// 恢复重放：重建 tap（新索引）、重入 BD/置 up、重申注册
	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("重放 Sync: %v", err)
	}
	var newIdx uint32
	for idx := range p.TapIndexes() {
		newIdx = idx // 重放后 provider 自持的集合里应恰有新建 tap 的索引
	}
	if newIdx == 0 || newIdx == firstIdx {
		t.Fatalf("重放应拿到新建 tap 的索引: %d → %d", firstIdx, newIdx)
	}
	if !c.bridgedOK(newIdx, BDID(vs.Name)) {
		t.Fatal("重放后 tap 应重新入 BD")
	}
	if fp.registers != 2 {
		t.Fatalf("重放应重申注册: %+v", fp)
	}
}

// ---------- 消息处理（状态机接入） ----------

func TestDHCPServerProviderDORA(t *testing.T) {
	p, _, _, factory, _ := newEnabledProvider(t)
	alarms := NewAlarmStore()
	p.SetAlarms(alarms)
	ctx := context.Background()
	vs := vsDHCPServer()
	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	name := vs.Name
	tapName := DHCPServerTapName(name)
	chaddr := mustMAC(t, "aa:bb:cc:dd:ee:01")
	bvi := net.ParseIP("192.168.100.1")
	tap := factory.get(tapName)

	// DISCOVER（广播入径）→ OFFER 写回 tap（以太单播到 chaddr、yiaddr=最小可用地址）
	p.handleTapFrame(name, dhcpClientFrame(chaddr, dhcpDiscover, nil, nil, nil))
	sent := tap.frames()
	if len(sent) != 1 {
		t.Fatalf("DISCOVER 应回 1 条 OFFER，实得 %d", len(sent))
	}
	off := parseReplyFrame(t, sent[0])
	if mt := findOption(t, off.body, dhcpOptMsgType); mt[0] != dhcpOffer {
		t.Fatal("第一条应答应为 OFFER")
	}
	offered := net.IP(off.body[16:20]).To4()
	if !offered.Equal(net.ParseIP("192.168.100.10")) {
		t.Fatalf("OFFER 应给最小可用地址: %s", offered)
	}
	rows, _ := p.Leases(name)
	if len(rows) != 1 || rows[0].State != "offered" || rows[0].MAC != chaddr.String() {
		t.Fatalf("DISCOVER 后应有 1 条 offered 租约: %+v", rows)
	}

	// REQUEST（SELECTING，option 50+54=BVI）→ ACK，租约转 active
	p.handleTapFrame(name, dhcpClientFrame(chaddr, dhcpRequest, offered, bvi, nil))
	sent = tap.frames()
	if len(sent) != 2 {
		t.Fatalf("REQUEST 应回 1 条 ACK，实得累计 %d", len(sent))
	}
	ack := parseReplyFrame(t, sent[1])
	if mt := findOption(t, ack.body, dhcpOptMsgType); mt[0] != dhcpAck {
		t.Fatal("第二条应答应为 ACK")
	}
	rows, _ = p.Leases(name)
	if rows[0].State != "active" || rows[0].IP != offered.String() {
		t.Fatalf("ACK 后租约应为 active@%s: %+v", offered, rows[0])
	}
	if n, _ := p.ActiveLeases(name); n != 1 {
		t.Fatalf("在租数应为 1: %d", n)
	}

	// 续租（单播 punt 入径，ciaddr 形态、无 option 50/54）→ ACK；反查 miss 时忽略（不猜测）
	ipPkt := dhcpClientFrame(chaddr, dhcpRequest, nil, nil, offered)[14:]
	p.handlePuntPacket(dnsPuntDesc{swIfIndex: 42}, ipPkt)
	if got := len(tap.frames()); got != 2 {
		t.Fatalf("反查未接线时 punt 上行应被忽略，实得 %d 条应答", got)
	}
	p.SetSwitchResolver(func(idx uint32) (string, bool) {
		if idx == 42 {
			return name, true
		}
		return "", false
	})
	p.handlePuntPacket(dnsPuntDesc{swIfIndex: 42}, ipPkt)
	sent = tap.frames()
	if len(sent) != 3 {
		t.Fatalf("punt 续租应回 1 条 ACK，实得累计 %d", len(sent))
	}
	rows, _ = p.Leases(name)
	if rows[0].ExpiresInSeconds < 7100 {
		t.Fatalf("续租应刷新到期时间: %+v", rows[0])
	}

	// DECLINE → 该地址隔离一个租期；此后该 MAC 的 DISCOVER 拿别的地址
	p.handleTapFrame(name, dhcpClientFrame(chaddr, dhcpDecline, offered, nil, nil))
	p.handleTapFrame(name, dhcpClientFrame(chaddr, dhcpDiscover, nil, nil, nil))
	sent = tap.frames()
	off2 := parseReplyFrame(t, sent[len(sent)-1])
	if got := net.IP(off2.body[16:20]).To4(); got.Equal(offered) {
		t.Fatalf("declined 地址不应再分配给同一客户端: %s", got)
	}

	// RELEASE → 该客户端的活动租约清除；被 DECLINE 的地址仍在隔离期（隔离是池的卫生状态，
	// 不随客户端释放而消失——这正是「防立即复用回环」的本意）
	p.handleTapFrame(name, dhcpClientFrame(chaddr, dhcpRelease, nil, nil, nil))
	if n, _ := p.ActiveLeases(name); n != 0 {
		t.Fatalf("RELEASE 后不应还有 active 租约: %d", n)
	}
	rows, _ = p.Leases(name)
	for _, l := range rows {
		if l.State != "declined" {
			t.Fatalf("RELEASE 后残留的应只有 declined 隔离条目: %+v", rows)
		}
	}
}

func TestDHCPServerRequestNAKRules(t *testing.T) {
	p, _, _, factory, _ := newEnabledProvider(t)
	ctx := context.Background()
	vs := vsDHCPServer()
	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	name := vs.Name
	bvi := net.ParseIP("192.168.100.1")
	mac1 := mustMAC(t, "aa:bb:cc:dd:ee:01")
	mac2 := mustMAC(t, "aa:bb:cc:dd:ee:02")
	tap := factory.get(DHCPServerTapName(name))

	// mac1 先租下 192.168.100.10
	p.handleTapFrame(name, dhcpClientFrame(mac1, dhcpRequest, net.ParseIP("192.168.100.10"), bvi, nil))
	n0 := len(tap.frames())

	// 请求池外地址 → NAK（带 server-id、不带 yiaddr）
	p.handleTapFrame(name, dhcpClientFrame(mac2, dhcpRequest, net.ParseIP("192.168.199.9"), bvi, nil))
	sent := tap.frames()
	if len(sent) != n0+1 {
		t.Fatal("池外请求应回 1 条 NAK")
	}
	nr := parseReplyFrame(t, sent[n0])
	if mt := findOption(t, nr.body, dhcpOptMsgType); mt[0] != dhcpNak {
		t.Fatal("池外请求应 NAK")
	}
	if sid := findOption(t, nr.body, dhcpOptServerID); sid == nil || !net.IP(sid).Equal(bvi) {
		t.Fatal("NAK 应带 server-id=BVI")
	}
	if yi := net.IP(nr.body[16:20]).To4(); !yi.IsUnspecified() {
		t.Fatalf("NAK 不应带 yiaddr: %s", yi)
	}

	// 请求他人持有的地址（INIT-REBOOT：无 server-id）→ NAK
	p.handleTapFrame(name, dhcpClientFrame(mac2, dhcpRequest, net.ParseIP("192.168.100.10"), nil, nil))
	if got := len(tap.frames()); got != n0+2 {
		t.Fatal("他人地址请求应回 1 条 NAK")
	}

	// 客户端选了别的 server-id → 静默忽略（不抢答，也不 NAK 干扰他方服务器）
	p.handleTapFrame(name, dhcpClientFrame(mac2, dhcpRequest, net.ParseIP("192.168.100.11"), net.ParseIP("10.9.9.9"), nil))
	if got := len(tap.frames()); got != n0+2 {
		t.Fatalf("他方 server-id 的 REQUEST 应静默忽略，实得 %d 条应答", got-n0)
	}
}

// TestDHCPServerDedupeDualPath 双入径去重（round141 真机实测后补的口径）：同一条客户端广播
// 报文会经「BD 洪泛到内置 tap」与「经 BVI 进 UDP/67 的 punt」**各达一次**（真机一 REQUEST
// 两 ACK），服务器须在去重窗内只应答一次；窗外的真重传（新 xid）照常应答。
func TestDHCPServerDedupeDualPath(t *testing.T) {
	p, _, _, factory, clk := newEnabledProvider(t)
	ctx := context.Background()
	vs := vsDHCPServer()
	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	name := vs.Name
	tap := factory.get(DHCPServerTapName(name))
	chaddr := mustMAC(t, "aa:bb:cc:dd:ee:09")

	// 同一条 DISCOVER 的两个副本（同一 xid，tap 与 punt 两路）→ 只回 1 条 OFFER
	frame := dhcpClientFrame(chaddr, dhcpDiscover, nil, nil, nil)
	p.SetSwitchResolver(func(idx uint32) (string, bool) { return name, true })
	p.handleTapFrame(name, frame)
	p.handlePuntPacket(dnsPuntDesc{swIfIndex: 42}, frame[14:])
	if got := len(tap.frames()); got != 1 {
		t.Fatalf("双入径副本应只回 1 条 OFFER，实得 %d", got)
	}

	// 窗内的相同报文再投递（模拟巡检间隙的重复）→ 仍只此一次，不多答
	p.handleTapFrame(name, frame)
	if got := len(tap.frames()); got != 1 {
		t.Fatalf("去重窗内的重复报文不应再应答，实得累计 %d", got)
	}

	// 时钟推过去重窗 + 新 xid（真重传/新交换）→ 正常应答
	clk.advance(2 * dhcpDedupeWindow)
	p.handleTapFrame(name, dhcpClientFrame(chaddr, dhcpDiscover, nil, nil, nil))
	if got := len(tap.frames()); got != 2 {
		t.Fatalf("越窗后的新交换应正常应答，实得累计 %d", got)
	}
}

func TestDHCPServerPoolExhaustedAlarm(t *testing.T) {
	p, _, _, factory, _ := newEnabledProvider(t)
	alarms := NewAlarmStore()
	p.SetAlarms(alarms)
	ctx := context.Background()
	vs := vsDHCPServer() // 3 地址池
	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	name := vs.Name
	tap := factory.get(DHCPServerTapName(name))
	code := AlarmDHCPPoolExhausted

	assertAlarm := func(want bool, why string) {
		t.Helper()
		found := false
		for _, a := range alarms.ActiveOf(dhcpServerScope) {
			if a.Code == code && a.Source == name {
				found = true
			}
		}
		if found != want {
			t.Fatalf("%s：DHCP_POOL_EXHAUSTED 在场=%v，期望 %v", why, found, want)
		}
	}

	// 占满 3 个地址（各 DISCOVER→REQUEST）
	for i := 1; i <= 3; i++ {
		mac := mustMAC(t, fmt.Sprintf("aa:bb:cc:dd:ee:%02d", i))
		p.handleTapFrame(name, dhcpClientFrame(mac, dhcpDiscover, nil, nil, nil))
		p.handleTapFrame(name, dhcpClientFrame(mac, dhcpRequest,
			net.ParseIP(fmt.Sprintf("192.168.100.%d", 9+i)), net.ParseIP("192.168.100.1"), nil))
	}
	if n, _ := p.ActiveLeases(name); n != 3 {
		t.Fatalf("应租出 3 个地址: %d", n)
	}
	// 3 地址全部在租 = 池内已无可用地址可应答新的 DISCOVER ⇒ 告警即时在场（由
	// refreshPoolAlarmLocked 在每次变更后复核，非只靠第 4 个 DISCOVER 触发）
	assertAlarm(true, "池满即无可用地址（应答不了新的 DISCOVER）")

	// 第 4 个 DISCOVER：不发 OFFER（告警维持）
	mac4 := mustMAC(t, "aa:bb:cc:dd:ee:04")
	n0 := len(tap.frames())
	p.handleTapFrame(name, dhcpClientFrame(mac4, dhcpDiscover, nil, nil, nil))
	if len(tap.frames()) != n0 {
		t.Fatal("池耗尽时不应回 OFFER")
	}
	assertAlarm(true, "池耗尽")
	foundMsg := false
	for _, a := range alarms.List("active") {
		if a.Code != code {
			continue
		}
		foundMsg = true
		if !hasSub(a.Message, "192.168.100.10") || !hasSub(a.Message, "192.168.100.12") || !hasSub(a.Message, "在租 3 个") {
			t.Fatalf("告警文案应带池范围与在租数: %s", a.Message)
		}
	}
	if !foundMsg {
		t.Fatal("应有活动告警可核对文案")
	}

	// 释放一个 → 有可用地址即自动消解（handleMessage 内即时复核）
	p.handleTapFrame(name, dhcpClientFrame(mustMAC(t, "aa:bb:cc:dd:ee:01"), dhcpRelease, nil, nil, nil))
	assertAlarm(false, "有地址释放即自动消解")
	// 消解后再 DISCOVER 能拿到地址
	p.handleTapFrame(name, dhcpClientFrame(mac4, dhcpDiscover, nil, nil, nil))
	if len(tap.frames()) == n0 {
		t.Fatal("释放后 DISCOVER 应能拿到 OFFER")
	}
}

func hasSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ---------- 租约持久化 ----------

func TestDHCPServerLeaseFileRoundTripAndCorruption(t *testing.T) {
	dir := t.TempDir()
	c := newFakeDHCPServerClient()
	fp := &fakePunt{}
	factory := newTapFactory()
	p, _ := newTestDHCPServer(t, c, fp, factory, dir)
	ctx := context.Background()
	vs := vsDHCPServer()
	name := vs.Name
	bvi := net.ParseIP("192.168.100.1")
	mac := mustMAC(t, "aa:bb:cc:dd:ee:01")

	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	p.handleTapFrame(name, dhcpClientFrame(mac, dhcpRequest, net.ParseIP("192.168.100.10"), bvi, nil))

	// 原子落盘：文件存在、0600；目录 0700
	path := filepath.Join(dir, name+".json")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("租约文件应存在: %v", err)
	}
	if runtime.GOOS == "linux" { // Windows 的文件模式映射不出 POSIX 权限位，断言只在 Linux 有意义
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("租约文件权限应为 0600: %v", fi.Mode().Perm())
		}
		if sti, err := os.Stat(dir); err != nil || sti.Mode().Perm() != 0o700 {
			t.Fatalf("租约目录权限应为 0700: %v/%v", sti, err)
		}
	}

	// 新 provider（模拟进程重启）：从文件恢复；同 MAC 的 DISCOVER 优先续用原地址
	factory2 := newTapFactory()
	p2, _ := newTestDHCPServer(t, newFakeDHCPServerClient(), &fakePunt{}, factory2, dir)
	if err := p2.Sync(ctx, vs); err != nil {
		t.Fatalf("重建 Sync: %v", err)
	}
	rows, ok := p2.Leases(name)
	if !ok || len(rows) != 1 || rows[0].IP != "192.168.100.10" || rows[0].State != "active" {
		t.Fatalf("重启后应按文件恢复租约: %v/%v", rows, ok)
	}
	p2.handleTapFrame(name, dhcpClientFrame(mac, dhcpDiscover, nil, nil, nil))
	sent := factory2.get(DHCPServerTapName(name)).frames()
	if len(sent) != 1 {
		t.Fatal("恢复后应能继续应答")
	}
	off := parseReplyFrame(t, sent[0])
	if got := net.IP(off.body[16:20]).To4(); !got.Equal(net.ParseIP("192.168.100.10")) {
		t.Fatalf("恢复后同 MAC 应优先续用原地址: %s", got)
	}

	// 损坏文件：空表起步（不崩），原文件保留供排查
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	p3, _ := newTestDHCPServer(t, newFakeDHCPServerClient(), &fakePunt{}, newTapFactory(), dir)
	if err := p3.Sync(ctx, vs); err != nil {
		t.Fatalf("损坏文件下 Sync 不应失败: %v", err)
	}
	rows, ok = p3.Leases(name)
	if !ok || len(rows) != 0 {
		t.Fatalf("损坏文件应空表起步: %v/%v", rows, ok)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("损坏文件不应被删除（供排查）")
	}
}

func TestDHCPServerTeardownAfterRestartReclaimsOrphans(t *testing.T) {
	// nfvisd 重启后停用/删交换机：新 provider 的 servers 表为空（rt=nil），但磁盘上的租约
	// 文件与 VPP 里的存量 tap 都要回收（契约：停用即回收；tap 按 HostIfName dump 找存量）。
	ctx := context.Background()
	vs := vsDHCPServer()
	dir := t.TempDir()

	// 第一段：正常运行，落一份租约文件 + 建一个 VPP tap。
	c1 := newFakeDHCPServerClient()
	p1, _ := newTestDHCPServer(t, c1, &fakePunt{}, newTapFactory(), dir)
	if err := p1.Sync(ctx, vs); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	tapName := DHCPServerTapName(vs.Name)
	var idx uint32
	for _, ti := range mustDump(t, c1) {
		if ti.HostIfName == tapName {
			idx = ti.SwIfIndex
		}
	}
	if idx == 0 {
		t.Fatal("前置：VPP 侧应有该 tap")
	}
	// 发一条 REQUEST 产生真实租约（租约文件只在租约变更时原子落盘）。
	bvi := net.ParseIP("192.168.100.1")
	p1.handleTapFrame(vs.Name, dhcpClientFrame(mustMAC(t, "aa:bb:cc:dd:ee:09"), dhcpRequest,
		net.ParseIP("192.168.100.10"), bvi, nil))
	if _, err := os.Stat(filepath.Join(dir, vs.Name+".json")); err != nil {
		t.Fatalf("前置：租约文件应存在: %v", err)
	}

	// 第二段：模拟 nfvisd 重启——新 provider（无任何进程内登记）+ 同一份磁盘/VPP 现场。
	c2 := newFakeDHCPServerClient()
	if _, err := c2.TapCreate(tapName, "nfvis-dhcp:"+vs.Name); err != nil { // 存量 tap（HostIfName 相同、索引任意）
		t.Fatal(err)
	}
	p2, _ := newTestDHCPServer(t, c2, &fakePunt{}, newTapFactory(), dir)
	if err := p2.Sync(ctx, model.VirtualSwitch{Name: vs.Name}); err != nil { // 停用（声明无池）＝teardown
		t.Fatalf("重启后停用 Sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, vs.Name+".json")); !os.IsNotExist(err) {
		t.Fatalf("重启后停用应清掉租约文件: %v", err)
	}
	if len(c2.deleted) != 1 || c2.deleted[0] == 0 {
		t.Fatalf("重启后停用应按 HostIfName 删掉存量 tap: %v", c2.deleted)
	}
}

func TestDHCPServerPuntPathIgnoresForeignPackets(t *testing.T) {
	// punt 上行的忽略路径：反查 miss（不属任何已知交换机）→ 静默；IPv6/非 DHCP 帧 → 解析拒绝。
	p, _, _, factory, _ := newEnabledProvider(t)
	p.SetSwitchResolver(func(uint32) (string, bool) { return "", false })
	ctx := context.Background()
	vs := vsDHCPServer()
	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	name := vs.Name
	mac := mustMAC(t, "aa:bb:cc:dd:ee:01")
	tap := factory.get(DHCPServerTapName(name))
	n0 := len(tap.frames())

	p.handlePuntPacket(dnsPuntDesc{swIfIndex: 7}, dhcpClientFrame(mac, dhcpDiscover, nil, nil, nil)[14:])
	if got := len(tap.frames()); got != n0 {
		t.Fatal("反查 miss 的 punt 上行应被忽略")
	}
	v6 := make([]byte, 60)
	binary.BigEndian.PutUint16(v6[12:14], 0x86dd)
	if _, ok := parseDHCPEtherFrame(v6); ok {
		t.Fatal("IPv6 帧不应解析为 DHCP")
	}
}
