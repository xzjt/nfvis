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
	"encoding/json"
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

	// failDelete 注入 TapDelete 失败（R142-4：tap 删不掉时租约文件必须保留）。
	// 错误文案不带 `-2`/`Invalid sw_if_index`（不得命中 isMissingIfaceErr 的「已达成」容错）。
	failDelete bool
	// failBridge 注入 SetL2Bridge 失败（R142-4 同族：tap 已建后任何一步失败都不得留下未登记 tap）。
	failBridge bool
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
	if c.failDelete {
		return fmt.Errorf("注入失败：删除 tap 未成功（VPP 不可用）")
	}
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
	if c.failBridge && enable {
		return fmt.Errorf("注入失败：加入 bridge-domain 未成功")
	}
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
	// 已关闭的旧 tap 不再复用（真实 AF_PACKET 重开会拿到新 fd；R142-4 的重放路径依赖这一点）。
	if t := f.taps[name]; t != nil && !t.isClosed() {
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
	// 租约目录给**尚不存在**的子路径：产品侧的 MkdirAll(0700) 才会真正创建它——权限断言
	// 只对「产品创建的目录」有意义。⚠️ 不能直接断言 t.TempDir() 本身：testing 的每调用子目录
	// 按 0777&^umask 创建（Linux 实测 0755），那是测试框架的语义、不是产品行为（round141 CI 假红）。
	dir := filepath.Join(t.TempDir(), "leases")
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

// ---------- R142-3 / R142-4 回归（round142 半程体检；决策 #360） ----------

// TestDHCPServerProviderIdempotentSyncKeepsTapMAC R142-3 回归（round142 真机复现）：
// 幂等 Sync（15s 巡检/重复提交走同一路径）不得清空应答的以太源——tapMAC 归运行态、与内核
// tap 同生命期。修复前每次 Sync 用配置规格覆盖 rt.spec（无 MAC 字段 ⇒ 零值），OFFER 的
// 以太源变 00:00:00:00:00:00（BD 学到 bogon 表项；做 L2 源过滤/端口安全的环境会丢帧）。
func TestDHCPServerProviderIdempotentSyncKeepsTapMAC(t *testing.T) {
	p, _, _, factory, _ := newEnabledProvider(t)
	ctx := context.Background()
	vs := vsDHCPServer()
	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	tapName := DHCPServerTapName(vs.Name)
	tapMAC := factory.get(tapName).MAC()
	if len(tapMAC) == 0 || tapMAC.String() == "00:00:00:00:00:00" {
		t.Fatalf("前置：假 tap 应给出非零 MAC: %v", tapMAC)
	}
	chaddr := mustMAC(t, "aa:bb:cc:dd:ee:01")

	// 先过一轮幂等 Sync（真机现场：启用后 15s 巡检先跑过一轮，正是复现条件）
	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("二次 Sync: %v", err)
	}
	p.handleTapFrame(vs.Name, dhcpClientFrame(chaddr, dhcpDiscover, nil, nil, nil))
	sent := factory.get(tapName).frames()
	if len(sent) != 1 {
		t.Fatalf("应回 1 条 OFFER，实得 %d", len(sent))
	}
	if off := parseReplyFrame(t, sent[0]); string(off.ethSrc) != string(tapMAC) {
		t.Fatalf("OFFER 以太源应为内核 tap MAC（幂等 Sync 后仍成立）: %s（期望 %s）", off.ethSrc, tapMAC)
	}

	// 再连续几轮（巡检节奏）：源 MAC 仍未漂移
	for i := 0; i < 3; i++ {
		if err := p.Sync(ctx, vs); err != nil {
			t.Fatalf("第 %d 轮幂等 Sync: %v", i+3, err)
		}
	}
	p.handleTapFrame(vs.Name, dhcpClientFrame(chaddr, dhcpDiscover, nil, nil, nil))
	sent = factory.get(tapName).frames()
	last := parseReplyFrame(t, sent[len(sent)-1])
	if last.ethSrc.String() == "00:00:00:00:00:00" {
		t.Fatal("多轮幂等 Sync 后 OFFER 以太源被清成零值（R142-3 回归）")
	}
	if string(last.ethSrc) != string(tapMAC) {
		t.Fatalf("多轮幂等 Sync 后 OFFER 以太源漂移: %s（期望 %s）", last.ethSrc, tapMAC)
	}
}

// TestDHCPServerTeardownKeepsLeaseFileWhenTapDeleteFails R142-4：停用回收顺序——
// tap 删除失败 ⇒ teardown 失败、提交回滚、服务器仍要服务（客户端仍持旧地址）：租约文件必须
// 保留，否则租约表被抹、地址可被重复分配（v1 无冲突检测）；修复后重试（tap 删除成功）即回收。
func TestDHCPServerTeardownKeepsLeaseFileWhenTapDeleteFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "leases")
	c := newFakeDHCPServerClient()
	p, _ := newTestDHCPServer(t, c, &fakePunt{}, newTapFactory(), dir)
	ctx := context.Background()
	vs := vsDHCPServer()
	name := vs.Name
	path := filepath.Join(dir, name+".json")

	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	p.handleTapFrame(name, dhcpClientFrame(mustMAC(t, "aa:bb:cc:dd:ee:09"), dhcpRequest,
		net.ParseIP("192.168.100.10"), net.ParseIP("192.168.100.1"), nil))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("前置：租约文件应存在: %v", err)
	}

	// tap 删除失败：停用必须失败并保留租约文件（错误里要说明原因）
	c.failDelete = true
	err := p.Sync(ctx, model.VirtualSwitch{Name: name})
	if err == nil {
		t.Fatal("tap 删除失败时停用应报错（提交将回滚、服务器仍要服务）")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("tap 未删成时租约文件必须保留（否则回滚后地址可被重复分配）: %v", statErr)
	}
	if !hasSub(err.Error(), "租约文件保留") {
		t.Fatalf("错误应说明租约文件保留: %v", err)
	}

	// 重试（VPP 恢复）：一次停用即回收
	c.failDelete = false
	if err := p.Sync(ctx, model.VirtualSwitch{Name: name}); err != nil {
		t.Fatalf("重试停用: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("重试成功后租约文件应被清除: %v", statErr)
	}
	if len(c.deleted) != 1 {
		t.Fatalf("重试应删掉 VPP 侧 tap: %v", c.deleted)
	}
}

// TestDHCPServerResetClearsTapIndex R142-4：VPP 重连 reset 后旧 sw_if_index 必须作废
// （可能已被 VPP 复用给别的接口——端口读视图不得再按它过滤、停用不得再按它删除）；
// 停用回收一律回到「按 HostIfName 核对身份」路径。
func TestDHCPServerResetClearsTapIndex(t *testing.T) {
	p, c, _, factory, _ := newEnabledProvider(t)
	ctx := context.Background()
	vs := vsDHCPServer()
	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	tapName := DHCPServerTapName(vs.Name)
	var idx uint32
	for _, ti := range mustDump(t, c) {
		if ti.HostIfName == tapName {
			idx = ti.SwIfIndex
		}
	}
	if idx == 0 || !p.TapIndexes()[idx] {
		t.Fatalf("前置：应登记 VPP 侧索引 %d: %v", idx, p.TapIndexes())
	}

	p.reset() // VPP 重连
	if got := p.TapIndexes(); len(got) != 0 {
		t.Fatalf("reset 应清空登记索引（旧索引可能被 VPP 复用）: %v", got)
	}
	if !factory.get(tapName).isClosed() {
		t.Fatal("reset 应关闭内核侧 tap")
	}

	// 停用：不按旧索引，按 HostIfName 找到存量再删（VPP 侧对象此时仍在）
	if err := p.Sync(ctx, model.VirtualSwitch{Name: vs.Name}); err != nil {
		t.Fatalf("reset 后停用: %v", err)
	}
	if len(c.deleted) != 1 || c.deleted[0] != idx {
		t.Fatalf("停用应按 HostIfName 删掉存量 tap %d: %v", idx, c.deleted)
	}
}

// TestDHCPServerPuntRegisterFailureKeepsTapRegistered R142-4：punt 注册失败不得留
// 「未登记 tap」——tap 已建、已入 BD 就必须在 TapIndexes 过滤集内（否则泄漏进用户端口视图，
// source=runtime 且用户删不掉，与 #359 ⑦ 相反）；注册失败如实报错、巡检重试重申成功后自愈。
func TestDHCPServerPuntRegisterFailureKeepsTapRegistered(t *testing.T) {
	p, c, fp, factory, _ := newEnabledProvider(t)
	ctx := context.Background()
	vs := vsDHCPServer()
	tapName := DHCPServerTapName(vs.Name)

	fp.regErr = fmt.Errorf("注入失败：punt 注册失败")
	if err := p.Sync(ctx, vs); err == nil {
		t.Fatal("punt 注册失败应报错（本机 DHCP 未收敛）")
	}
	dumps := mustDump(t, c)
	if len(dumps) != 1 {
		t.Fatalf("tap 应已建: %+v", dumps)
	}
	idx := dumps[0].SwIfIndex
	if !p.TapIndexes()[idx] {
		t.Fatalf("punt 失败时 tap 仍须登记（否则泄漏进用户端口视图）: %v", p.TapIndexes())
	}
	if !c.bridgedOK(idx, BDID(vs.Name)) {
		t.Fatal("tap 应已入 BD")
	}
	if factory.get(tapName) != nil {
		t.Fatal("punt 未成功时不应打开内核侧 tap（半功能不运行）")
	}

	// 巡检重试（注册恢复）：一次 Sync 收敛、打开内核侧 tap、不重复建 tap
	fp.regErr = nil
	if errs := p.Reconcile(ctx, model.Config{VirtualSwitches: []model.VirtualSwitch{vs}}); len(errs) != 0 {
		t.Fatalf("重试应成功: %v", errs)
	}
	if !fp.registered {
		t.Fatal("重试应完成 punt 注册")
	}
	if factory.get(tapName) == nil || factory.get(tapName).isClosed() {
		t.Fatal("收敛后应打开内核侧 tap")
	}
	if len(mustDump(t, c)) != 1 {
		t.Fatal("重试不应重复建 tap")
	}
}

// TestDHCPServerBridgeJoinFailureKeepsTapRegistered R142-4（同族加固）：入 BD 失败同样不得
// 留下未登记 tap（登记先于入 BD/置 up）；停用即按 HostIfName 回收。
func TestDHCPServerBridgeJoinFailureKeepsTapRegistered(t *testing.T) {
	p, c, _, _, _ := newEnabledProvider(t)
	ctx := context.Background()
	vs := vsDHCPServer()

	c.failBridge = true
	if err := p.Sync(ctx, vs); err == nil {
		t.Fatal("入 BD 失败应报错")
	}
	dumps := mustDump(t, c)
	if len(dumps) != 1 {
		t.Fatalf("tap 应已建: %+v", dumps)
	}
	if !p.TapIndexes()[dumps[0].SwIfIndex] {
		t.Fatalf("入 BD 失败时 tap 仍须登记（否则泄漏进用户端口视图）: %v", p.TapIndexes())
	}

	// 停用即回收（按 HostIfName；不依赖入 BD 是否成功过）
	c.failBridge = false
	if err := p.Sync(ctx, model.VirtualSwitch{Name: vs.Name}); err != nil {
		t.Fatalf("停用: %v", err)
	}
	if len(c.deleted) != 1 {
		t.Fatalf("停用应删掉 tap: %v", c.deleted)
	}
}

// TestDHCPServerProviderClose Close 单测（此前无覆盖）：注销 UDP/67 注册 + 关内核侧 tap +
// 关接收 socket；重复 Close 幂等（不得再发注销）。
func TestDHCPServerProviderClose(t *testing.T) {
	p, _, fp, factory, _ := newEnabledProvider(t)
	ctx := context.Background()
	vs := vsDHCPServer()
	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !fp.registered {
		t.Fatal("前置：应已注册 punt")
	}
	deregBefore := fp.deregisters

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !factory.get(DHCPServerTapName(vs.Name)).isClosed() {
		t.Fatal("Close 应关闭内核侧 tap")
	}
	if fp.registered {
		t.Fatal("Close 应注销 punt 注册（否则留一个指向已消失 socket 的注册＝域内 DHCP 黑洞）")
	}
	if fp.deregisters != deregBefore+1 {
		t.Fatalf("Close 应注销恰好一次: %d → %d", deregBefore, fp.deregisters)
	}

	if err := p.Close(); err != nil { // 幂等
		t.Fatalf("重复 Close: %v", err)
	}
	if fp.deregisters != deregBefore+1 {
		t.Fatalf("重复 Close 不应再注销: %d", fp.deregisters)
	}
}

// TestDHCPServerProviderReconcile Reconcile 单测（此前无覆盖）：幂等重收敛（不重建在场
// tap）、VPP 重启后按 HostIfName 补齐并保留租约、到期租约回收并落盘。
func TestDHCPServerProviderReconcile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "leases")
	c := newFakeDHCPServerClient()
	fp := &fakePunt{}
	factory := newTapFactory()
	p, fc := newTestDHCPServer(t, c, fp, factory, dir)
	ctx := context.Background()
	vs := vsDHCPServer()
	name := vs.Name
	tapName := DHCPServerTapName(name)
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{vs}}

	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// 一条真实租约（REQUEST→commit→落盘）
	p.handleTapFrame(name, dhcpClientFrame(mustMAC(t, "aa:bb:cc:dd:ee:09"), dhcpRequest,
		net.ParseIP("192.168.100.10"), net.ParseIP("192.168.100.1"), nil))
	if n, _ := p.ActiveLeases(name); n != 1 {
		t.Fatalf("前置：应有 1 条在租: %d", n)
	}

	// ① 幂等重收敛：在场 tap 不重建（身份与成员关系保持）、注册重申
	if errs := p.Reconcile(ctx, cfg); len(errs) != 0 {
		t.Fatalf("Reconcile: %v", errs)
	}
	dumps := mustDump(t, c)
	if len(dumps) != 1 {
		t.Fatalf("在场 tap 不应被重建: %+v", dumps)
	}
	if dumps[0].HostIfName != tapName || !c.bridgedOK(dumps[0].SwIfIndex, BDID(name)) {
		t.Fatalf("重收敛后 tap 身份/成员关系应保持: %+v", dumps)
	}
	if fp.registers != 2 {
		t.Fatalf("巡检应重申注册: %+v", fp)
	}

	// ② VPP 重启后补齐：reset + 数据面清空 ⇒ Reconcile 按 HostIfName 重建 tap，租约保留
	p.reset()
	c.wipe()
	if errs := p.Reconcile(ctx, cfg); len(errs) != 0 {
		t.Fatalf("VPP 重启后 Reconcile: %v", errs)
	}
	dumps = mustDump(t, c)
	if len(dumps) != 1 || dumps[0].HostIfName != tapName {
		t.Fatalf("VPP 重启后应按声明重建 tap: %+v", dumps)
	}
	if n, ok := p.ActiveLeases(name); !ok || n != 1 {
		t.Fatalf("租约表应随 reset 保留: %d/%v", n, ok)
	}
	if got := factory.get(tapName); got == nil || got.isClosed() {
		t.Fatal("重放应重开内核侧 tap")
	}

	// ③ 到期回收：推进时钟过租期 ⇒ Reconcile 清租约并落盘
	fc.advance(time.Duration(vs.DhcpServerLeaseTimeSeconds)*time.Second + time.Minute)
	if errs := p.Reconcile(ctx, cfg); len(errs) != 0 {
		t.Fatalf("到期后 Reconcile: %v", errs)
	}
	if n, _ := p.ActiveLeases(name); n != 0 {
		t.Fatalf("到期租约应被回收: %d", n)
	}
	b, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if err != nil {
		t.Fatalf("回收后应重写租约文件: %v", err)
	}
	var f dhcpLeaseFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("租约文件应可解析: %v", err)
	}
	if len(f.Leases) != 0 {
		t.Fatalf("到期条目不应留在文件里: %+v", f.Leases)
	}
}

// 决策 #371（R142 C7）：**探测不改生效租约**——active 条目经 DISCOVER（allocate）后
// 状态与到期都不变；旧实现会降级为 offered+2 分钟保持窗（随后被 sweep 回收 ⇒ 重复地址风险）。
func TestDHCPLeaseTableAllocateKeepsActive(t *testing.T) {
	fc := newFakeClock(time.Unix(1_000_000, 0))
	lo, hi := poolOf(t, "192.168.100.10", "192.168.100.12")
	tt := newDHCPLeaseTable(lo, hi, fc.now)

	const mac = "aa:aa:aa:aa:aa:01"
	ip, ok := tt.allocate(mac)
	if !ok {
		t.Fatal("首个 DISCOVER 应分配成功")
	}
	tt.commit(mac, ip, time.Hour) // REQUEST→ACK：active，租期 1h
	activeExpiry := tt.byMAC[mac].ExpiresAt

	fc.advance(10 * time.Minute)
	if ip2, ok2 := tt.allocate(mac); !ok2 || ip2 != ip {
		t.Fatalf("active 客户端的 DISCOVER 应得同一地址，实得 %v/%v", ip2, ok2)
	}
	l := tt.byMAC[mac]
	if l.State != dhcpLeaseActive {
		t.Fatalf("DISCOVER 不得把 active 降级（旧实现 offered）：%s", l.State)
	}
	if !l.ExpiresAt.Equal(activeExpiry) {
		t.Fatalf("DISCOVER 不得改动 active 的到期（旧实现缩到 2 分钟保持窗）：%v vs %v", l.ExpiresAt, activeExpiry)
	}
	// 对照：offered（未 active）经 DISCOVER 仍按保持窗刷新
	mac2 := "aa:aa:aa:aa:aa:02"
	ipB, _ := tt.allocate(mac2)
	fc.advance(30 * time.Second)
	before := tt.byMAC[mac2].ExpiresAt
	if _, ok := tt.allocate(mac2); !ok {
		t.Fatal("第二个客户端应能拿到地址")
	}
	if !tt.byMAC[mac2].ExpiresAt.After(before) {
		t.Fatalf("offered 条目经 DISCOVER 应刷新保持窗：%v vs %v", tt.byMAC[mac2].ExpiresAt, before)
	}
	_ = ipB
}

// 决策 #371（R142 C6）：declined 地址在隔离期内**任何客户端**（含声明者本人）都不得取得——
// 旧实现同一 MAC 的 REQUEST 会落进默认分支被 commit 夺回。
func TestDHCPServerDeclinedNotReclaimableBySameMAC(t *testing.T) {
	p, _, _, factory, _ := newEnabledProvider(t)
	ctx := context.Background()
	vs := vsDHCPServer()
	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	name := vs.Name
	bvi := net.ParseIP("192.168.100.1")
	mac1 := mustMAC(t, "aa:bb:cc:dd:ee:01")
	tap := factory.get(DHCPServerTapName(name))

	// mac1 租下 .10（active）
	p.handleTapFrame(name, dhcpClientFrame(mac1, dhcpRequest, net.ParseIP("192.168.100.10"), bvi, nil))
	n0 := len(tap.frames())

	// mac1 声明该地址冲突（DECLINE：option 50 = .10）
	p.handleTapFrame(name, dhcpClientFrame(mac1, dhcpDecline, net.ParseIP("192.168.100.10"), bvi, nil))
	if got := len(tap.frames()); got != n0 {
		t.Fatalf("DECLINE 不应产生应答，实得 %d 条", got-n0)
	}

	// mac1 再用 REQUEST 要回同一地址 ⇒ 必须 NAK（隔离期内任何人不得取得）
	p.handleTapFrame(name, dhcpClientFrame(mac1, dhcpRequest, net.ParseIP("192.168.100.10"), bvi, nil))
	sent := tap.frames()
	if len(sent) != n0+1 {
		t.Fatal("隔离期内同 MAC 的 REQUEST 应回 1 条 NAK")
	}
	nr := parseReplyFrame(t, sent[n0])
	if mt := findOption(t, nr.body, dhcpOptMsgType); mt[0] != dhcpNak {
		t.Fatalf("隔离期内同 MAC REQUEST 应 NAK（旧实现 ACK 夺回），实得 msgType=%d", mt[0])
	}
	// 另一台客户端同样 NAK（隔离对所有人成立）
	mac2 := mustMAC(t, "aa:bb:cc:dd:ee:02")
	p.handleTapFrame(name, dhcpClientFrame(mac2, dhcpRequest, net.ParseIP("192.168.100.10"), nil, nil))
	if got := len(tap.frames()); got != n0+2 {
		t.Fatal("他人请求隔离地址也应 NAK")
	}
}

// 决策 #371（R142 C10）：应答按 BOOTP 下限补零到 300 字节（RFC 2131 §2）。
func TestDHCPReplyPaddedTo300(t *testing.T) {
	p, _, _, factory, _ := newEnabledProvider(t)
	ctx := context.Background()
	vs := vsDHCPServer()
	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	name := vs.Name
	bvi := net.ParseIP("192.168.100.1")
	mac := mustMAC(t, "aa:bb:cc:dd:ee:09")
	tap := factory.get(DHCPServerTapName(name))

	// DISCOVER → OFFER；REQUEST → ACK；两者都须 ≥300 字节
	p.handleTapFrame(name, dhcpClientFrame(mac, dhcpDiscover, nil, nil, nil))
	p.handleTapFrame(name, dhcpClientFrame(mac, dhcpRequest, net.ParseIP("192.168.100.10"), bvi, nil))
	frames := tap.frames()
	if len(frames) < 2 {
		t.Fatalf("应有 OFFER 与 ACK，实得 %d 条", len(frames))
	}
	for i, f := range frames {
		nr := parseReplyFrame(t, f)
		if len(nr.body) < dhcpMinMessageBytes {
			t.Fatalf("第 %d 条应答载荷 %d 字节 < %d（未补零）", i+1, len(nr.body), dhcpMinMessageBytes)
		}
	}
}

// 决策 #373（R142 C11）：池耗尽告警的建/消在**解锁后**执行——告警 I/O（通知器）进行中
// provider 锁必须空闲（旧实现持 p.mu 调 Raise ⇒ 收包路径持锁做 I/O，此处会阻塞）。
// 设计：前两次通知（建告警 / 消解）放行，第 3 次起阻塞——触发调用放 goroutine（否则主 goroutine 自身被卡）。
func TestPoolAlarmIODoesNotHoldProviderLock(t *testing.T) {
	p, _, _, _, _ := newEnabledProvider(t)
	ctx := context.Background()
	vs := vsDHCPServer() // 3 地址小池
	if err := p.Sync(ctx, vs); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	alarms := NewAlarmStore()
	var calls int32
	entered := make(chan struct{})
	release := make(chan struct{})
	alarms.SetNotifier(func(Alarm) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			return // 建告警 / 消解：放行
		}
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
	})
	p.SetAlarms(alarms)

	name := vs.Name
	bvi := net.ParseIP("192.168.100.1")
	macOf := func(i int) net.HardwareAddr { return mustMAC(t, fmt.Sprintf("aa:bb:cc:00:00:%02x", i)) }
	// 占满池（.10/.11/.12）：第 3 条触发建告警（通知 #1，放行）
	for i, ip := range []string{"192.168.100.10", "192.168.100.11", "192.168.100.12"} {
		p.handleTapFrame(name, dhcpClientFrame(macOf(i+1), dhcpRequest, net.ParseIP(ip), bvi, nil))
	}
	// 释放一个（消解告警，通知 #2，放行）→ 再 DISCOVER 占回（重新建告警，通知 #3：阻塞）
	p.handleTapFrame(name, dhcpClientFrame(macOf(3), dhcpRelease, nil, bvi, net.ParseIP("192.168.100.12")))
	done := make(chan struct{})
	go func() {
		p.handleTapFrame(name, dhcpClientFrame(macOf(4), dhcpDiscover, nil, nil, nil))
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("告警通知器未被触发（池耗尽告警应已 raise）")
	}
	// 告警 I/O 进行中：provider 锁应空闲——Leases 立即返回
	leaseDone := make(chan struct{})
	go func() { _, _ = p.Leases(name); close(leaseDone) }()
	select {
	case <-leaseDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("告警 I/O 期间 provider 锁被持有（C11 回归：收包路径持锁做 I/O）")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("告警应用后收包路径应正常返回")
	}
	// 行为不回归：告警确在册
	found := false
	for _, a := range alarms.List("active") {
		if a.Code == AlarmDHCPPoolExhausted && a.Source == name {
			found = true
		}
	}
	if !found {
		t.Fatal("池耗尽告警应在册（C11 只改持锁范围，行为不变）")
	}
}
