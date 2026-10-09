package netkernel

// 内核数据面 DHCP 中继（决策 #437）单测：**不依赖真 socket**——收发底座是可注入接口
// （relaySocketLayer / relayFrameIO / relayUplinkIO），本文件注入内存实现，校验
// 报文处理（源地址重写/载荷保真/按登记表回注/丢弃计数）与生命周期（起停幂等/重建/恢复重放）。

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"syscall"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// hasRelayAlarm 告警表里是否有该交换机中继的未收敛告警（source = virtual-switches/<交换机>/dhcp-relay）。
func hasRelayAlarm(store *network.AlarmStore) bool {
	for _, a := range store.List("active") {
		if a.Source == "virtual-switches/vs-r/dhcp-relay" {
			return true
		}
	}
	return false
}

// ---------- 内存收发底座 ----------

type fakeFrameIO struct {
	mac net.HardwareAddr

	mu      sync.Mutex
	in      chan []byte
	sent    [][]byte
	sendErr error
	closed  bool
	done    chan struct{}
}

func newFakeFrameIO(mac string) *fakeFrameIO {
	m, err := net.ParseMAC(mac)
	if err != nil {
		panic(err)
	}
	return &fakeFrameIO{mac: m, in: make(chan []byte, 16), done: make(chan struct{})}
}

func (f *fakeFrameIO) Recv() ([]byte, error) {
	select {
	case <-f.done:
		return nil, net.ErrClosed
	case frame := <-f.in:
		return frame, nil
	}
}

func (f *fakeFrameIO) Send(frame []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return net.ErrClosed
	}
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, append([]byte(nil), frame...))
	return nil
}

func (f *fakeFrameIO) MAC() net.HardwareAddr { return f.mac }

func (f *fakeFrameIO) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	close(f.done)
	return nil
}

func (f *fakeFrameIO) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeFrameIO) sentFrames() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.sent...)
}

func (f *fakeFrameIO) feed(frame []byte) { f.in <- frame }

type fakeUplinkIO struct {
	mu      sync.Mutex
	in      chan []byte
	errs    []error // 先于 in 逐条返回的读错误（模拟连接式 UDP 的 ICMP 一次性差错）
	sent    [][]byte
	sendErr error
	closed  bool
	done    chan struct{}
}

func newFakeUplinkIO() *fakeUplinkIO {
	return &fakeUplinkIO{in: make(chan []byte, 16), done: make(chan struct{})}
}

func (f *fakeUplinkIO) Send(payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return net.ErrClosed
	}
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, append([]byte(nil), payload...))
	return nil
}

func (f *fakeUplinkIO) Recv(buf []byte) (int, error) {
	for {
		f.mu.Lock()
		if len(f.errs) > 0 {
			err := f.errs[0]
			f.errs = f.errs[1:]
			f.mu.Unlock()
			return 0, err
		}
		f.mu.Unlock()
		select {
		case <-f.done:
			return 0, net.ErrClosed
		case p := <-f.in:
			return copy(buf, p), nil
		case <-time.After(5 * time.Millisecond): // 轮询注入的读错误（真实现里由内核直接唤醒）
		}
	}
}

// feedErr 让下一次 Recv 先返回给定错误（模拟 ICMP 差错上报）。
func (f *fakeUplinkIO) feedErr(err error) {
	f.mu.Lock()
	f.errs = append(f.errs, err)
	f.mu.Unlock()
}

func (f *fakeUplinkIO) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	close(f.done)
	return nil
}

func (f *fakeUplinkIO) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeUplinkIO) sentPayloads() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.sent...)
}

func (f *fakeUplinkIO) feed(payload []byte) { f.in <- payload }

// fakeRelayLayer 记录打开过的底座与参数（供断言"源地址 = BVI 的 v4 网关地址"等）。
type fakeRelayLayer struct {
	mu        sync.Mutex
	bridgeErr error
	uplinkErr error

	frames  []*fakeFrameIO
	ups     []*fakeUplinkIO
	srcs    []net.IP
	servers []net.IP
	vrfs    []string
}

func (l *fakeRelayLayer) OpenBridge(bridge string) (relayFrameIO, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.bridgeErr != nil {
		return nil, l.bridgeErr
	}
	f := newFakeFrameIO("02:00:00:00:00:aa")
	l.frames = append(l.frames, f)
	return f, nil
}

func (l *fakeRelayLayer) OpenUplink(src, server net.IP, vrfDevice string) (relayUplinkIO, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.uplinkErr != nil {
		return nil, l.uplinkErr
	}
	u := newFakeUplinkIO()
	l.ups = append(l.ups, u)
	l.srcs = append(l.srcs, append(net.IP(nil), src...))
	l.servers = append(l.servers, append(net.IP(nil), server...))
	l.vrfs = append(l.vrfs, vrfDevice)
	return u, nil
}

func (l *fakeRelayLayer) counts() (frames, ups int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.frames), len(l.ups)
}

func (l *fakeRelayLayer) lastFrame() *fakeFrameIO {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.frames) == 0 {
		return nil
	}
	return l.frames[len(l.frames)-1]
}

func (l *fakeRelayLayer) lastUplink() *fakeUplinkIO {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.ups) == 0 {
		return nil
	}
	return l.ups[len(l.ups)-1]
}

func (l *fakeRelayLayer) openArgs() (srcs, servers []net.IP) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]net.IP(nil), l.srcs...), append([]net.IP(nil), l.servers...)
}

// openedVRFs 每次打开上行 socket 时传入的 VRF 设备名（按打开顺序）。
func (l *fakeRelayLayer) openedVRFs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.vrfs...)
}

// ---------- 报文构造（与产品实现同一套头/校验和口径，互为独立事实源） ----------

func mustMAC(s string) net.HardwareAddr {
	m, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return m
}

// testDHCPPayload 造一条 DHCP 载荷（BOOTP 头 + option 53 + 附加选项 + END + 补零到 300）。
func testDHCPPayload(op byte, xid uint32, msgType byte, giaddr net.IP, chaddr net.HardwareAddr, extra []byte) []byte {
	body := make([]byte, 240)
	body[0] = op
	body[1] = 1
	body[2] = 6
	binary.BigEndian.PutUint32(body[4:8], xid)
	binary.BigEndian.PutUint16(body[10:12], 0x8000)
	if giaddr != nil {
		copy(body[24:28], giaddr.To4())
	}
	copy(body[28:34], chaddr)
	binary.BigEndian.PutUint32(body[236:240], relayMagicCookie)
	opts := []byte{relayOptMsgType, 1, msgType}
	opts = append(opts, extra...)
	opts = append(opts, 255)
	body = append(body, opts...)
	if len(body) < 300 {
		body = append(body, make([]byte, 300-len(body))...)
	}
	return body
}

// testClientFrame 把 DHCP 载荷包成客户端请求的完整以太帧（eth 广播 + IPv4 0.0.0.0→255.255.255.255 + UDP 68→67）。
func testClientFrame(srcMAC net.HardwareAddr, payload []byte) []byte {
	udpLen := 8 + len(payload)
	ipLen := 20 + udpLen
	pkt := make([]byte, ipLen)
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(ipLen))
	pkt[8] = 64
	pkt[9] = 17
	copy(pkt[12:16], net.IPv4zero.To4())
	copy(pkt[16:20], net.IPv4bcast.To4())
	binary.BigEndian.PutUint16(pkt[10:12], relayIPChecksum(pkt[:20]))
	udp := pkt[20:]
	binary.BigEndian.PutUint16(udp[0:2], relayClientPort)
	binary.BigEndian.PutUint16(udp[2:4], relayServerPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(udpLen))
	copy(udp[8:], payload)
	binary.BigEndian.PutUint16(udp[6:8], relayUDPChecksum(pkt[12:16], pkt[16:20], udp))
	frame := make([]byte, 14+len(pkt))
	for i := 0; i < 6; i++ {
		frame[i] = 0xff
	}
	copy(frame[6:12], srcMAC)
	binary.BigEndian.PutUint16(frame[12:14], 0x0800)
	copy(frame[14:], pkt)
	return frame
}

// relayedSwitch 一台可用的中继声明（L2 + v4 网关）。
func relayedSwitch(server string) model.VirtualSwitch {
	return model.VirtualSwitch{
		Name: "vs-r", Type: "l2",
		Gateway:         &model.VSGateway{Addresses: []string{"2001:db8::1/64", "192.168.99.1/24"}},
		DhcpRelayServer: server,
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

// ---------- 规格派生 ----------

func TestRelayTargetOfDerivesBridgeServerAndBVISource(t *testing.T) {
	vs := relayedSwitch("192.168.99.10")
	// 网关里 IPv6 在前：中继源取**第一个 IPv4**（与校验层 gatewayHasV4、VPP 侧 relayTargetOf 同口径）。
	tgt, err := relayTargetOf(vs)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.bridge != "vs-r" || !tgt.server.Equal(net.ParseIP("192.168.99.10")) || !tgt.src.Equal(net.ParseIP("192.168.99.1")) {
		t.Fatalf("规格派生不符：%+v", tgt)
	}
	// 上行 socket 的作用域 = 该域 VRF 设备（无显式 gateway.vrf 时为派生的 vr-<交换机名>）。
	if tgt.vrfDevice != "vr-vs-r" {
		t.Fatalf("应派生网关 VRF 设备名 vr-vs-r，得到 %q", tgt.vrfDevice)
	}
	explicit := vs
	explicit.Gateway = &model.VSGateway{Vrf: "myvrf", Addresses: []string{"192.168.99.1/24"}}
	if tgt2, err := relayTargetOf(explicit); err != nil || tgt2.vrfDevice != "myvrf" {
		t.Fatalf("显式 gateway.vrf 应作为上行作用域，得到 %+v err=%v", tgt2, err)
	}

	// 各非法形态如实报错（配置层已拦，这里是纵深防御）。
	l3 := vs
	l3.Type = "l3"
	if _, err := relayTargetOf(l3); err == nil || !strings.Contains(err.Error(), "l3") {
		t.Fatalf("type=l3 应如实报错，得到 %v", err)
	}
	noGW := vs
	noGW.Gateway = nil
	if _, err := relayTargetOf(noGW); err == nil || !strings.Contains(err.Error(), "未配置网关") {
		t.Fatalf("无网关应如实报错，得到 %v", err)
	}
	v6only := vs
	v6only.Gateway = &model.VSGateway{Addresses: []string{"2001:db8::1/64"}}
	if _, err := relayTargetOf(v6only); err == nil || !strings.Contains(err.Error(), "IPv4") {
		t.Fatalf("无 v4 网关地址应如实报错，得到 %v", err)
	}
	badServer := vs
	badServer.DhcpRelayServer = "2001:db8::10"
	if _, err := relayTargetOf(badServer); err == nil || !strings.Contains(err.Error(), "IPv4") {
		t.Fatalf("非 IPv4 的 server 应如实报错，得到 %v", err)
	}
}

// ---------- 请求 → 转发 ----------

func TestRelayForwardsRequestWithBVISourceAndFaithfulPayload(t *testing.T) {
	layer := &fakeRelayLayer{}
	m := newRelayManager(layer)
	vs := relayedSwitch("192.168.99.10")
	if err := m.Sync(context.Background(), vs); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	client := mustMAC("02:00:00:00:00:01")
	// 载荷里带一条自定义选项（option 60 vendor class）——转发必须**字节保真**、不插 option 82。
	extra := []byte{60, 4, 'n', 'f', 'v', 's'}
	req := testDHCPPayload(relayBootRequest, 0xdeadbeef, 1 /*DISCOVER*/, nil, client, extra)
	layer.lastFrame().feed(testClientFrame(client, req))

	up := layer.lastUplink()
	waitFor(t, "请求被转发到 server", func() bool { return len(up.sentPayloads()) == 1 })
	got := up.sentPayloads()[0]
	if string(got) != string(req) {
		t.Fatalf("转发载荷应字节保真（不重编码、不插 option 82）：发出 %d 字节，收到 %d 字节", len(req), len(got))
	}
	// 源地址重写 = socket 绑定到 BVI 的 v4 网关地址；目的 = 声明的 server:67（连接式 UDP）。
	srcs, servers := layer.openArgs()
	if len(srcs) != 1 || !srcs[0].Equal(net.ParseIP("192.168.99.1")) {
		t.Fatalf("中继源地址应为 BVI 的第一个 v4 网关地址，得到 %v", srcs)
	}
	if len(servers) != 1 || !servers[0].Equal(net.ParseIP("192.168.99.10")) {
		t.Fatalf("上行对端应为声明的 server，得到 %v", servers)
	}
	if vrfs := layer.openedVRFs(); len(vrfs) != 1 || vrfs[0] != "vr-vs-r" {
		t.Fatalf("上行 socket 应绑到该域 VRF 设备，得到 %v", vrfs)
	}
	// giaddr 保持 0（与 VPP 侧同口径：源地址重写、不填 giaddr）。
	msg, ok := parseRelayMsg(got)
	if !ok || !msg.giaddr.IsUnspecified() {
		t.Fatalf("转发的载荷必须保持 giaddr=0，得到 %v", msg.giaddr)
	}
	if st, ok := m.State(vs.Name); !ok || !st.Running || st.Forwarded != 1 {
		t.Fatalf("运行态应显示运行中、已转发 1，得到 %+v ok=%v", st, ok)
	}
}

func TestRelaySkipsUnexpectedFrames(t *testing.T) {
	layer := &fakeRelayLayer{}
	m := newRelayManager(layer)
	if err := m.Sync(context.Background(), relayedSwitch("192.168.99.10")); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	client := mustMAC("02:00:00:00:00:01")
	up := layer.lastUplink()
	fr := layer.lastFrame()

	// ① 802.1Q 标签帧（0x8100）：如实边界内不处理（BD 内 access/trunk 已是无标签帧）。
	tagged := testClientFrame(client, testDHCPPayload(relayBootRequest, 1, 1, nil, client, nil))
	binary.BigEndian.PutUint16(tagged[12:14], 0x8100)
	fr.feed(tagged)
	// ② 已经过其它中继（giaddr≠0）：不再中继（防环）。
	relayed := testDHCPPayload(relayBootRequest, 2, 1, net.ParseIP("10.9.9.9"), client, nil)
	fr.feed(testClientFrame(client, relayed))
	// ③ 非 67 端口：不是 DHCP。
	other := testClientFrame(client, testDHCPPayload(relayBootRequest, 3, 1, nil, client, nil))
	binary.BigEndian.PutUint16(other[14+20+2:14+20+4], 53)
	fr.feed(other)
	// ④ 源 MAC 组播/全零：不能作单播回注目的，不处理。
	bogus := testClientFrame(client, testDHCPPayload(relayBootRequest, 4, 1, nil, client, nil))
	bogus[6] = 0x01
	fr.feed(bogus)

	// 有效请求照常转发（排除"被前面喂坏状态"）。
	fr.feed(testClientFrame(client, testDHCPPayload(relayBootRequest, 5, 1, nil, client, nil)))
	waitFor(t, "有效请求仍被转发", func() bool { return len(up.sentPayloads()) == 1 })
	if n := len(up.sentPayloads()); n != 1 {
		t.Fatalf("边界外的帧不得被转发，实际转发 %d 条", n)
	}
}

// ---------- 应答 → 回注 ----------

func TestRelayInjectsReplyToRegisteredClient(t *testing.T) {
	layer := &fakeRelayLayer{}
	m := newRelayManager(layer)
	vs := relayedSwitch("192.168.99.10")
	if err := m.Sync(context.Background(), vs); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	client := mustMAC("02:00:00:00:00:01")
	fr, up := layer.lastFrame(), layer.lastUplink()
	const xid = 0x11223344
	up.feed(nil) // 占位：保证 fakeUplink 的 channel 有消费方（下方才是真应答）
	fr.feed(testClientFrame(client, testDHCPPayload(relayBootRequest, xid, 1, nil, client, nil)))
	waitFor(t, "请求已转发", func() bool { return len(up.sentPayloads()) == 1 })

	// server 的应答（BOOTREPLY + OFFER）：按 (a) 登记表找回客户端并回注。
	reply := testDHCPPayload(relayBootReply, xid, 2 /*OFFER*/, nil, client, nil)
	up.feed(reply)
	waitFor(t, "应答被回注", func() bool { return len(fr.sentFrames()) == 1 })
	frame := fr.sentFrames()[0]

	if len(frame) < 14+20+8 {
		t.Fatalf("回注帧过短：%d", len(frame))
	}
	// 以太：dst = 登记表里的客户端 MAC、src = bridge 的 MAC、ethertype IPv4。
	if got := net.HardwareAddr(frame[0:6]); got.String() != client.String() {
		t.Fatalf("回注以太目的应为登记的客户端 MAC，得到 %s", got)
	}
	if got := net.HardwareAddr(frame[6:12]); got.String() != fr.MAC().String() {
		t.Fatalf("回注以太源应为 bridge 的 MAC，得到 %s", got)
	}
	if binary.BigEndian.Uint16(frame[12:14]) != 0x0800 {
		t.Fatalf("回注帧应为 IPv4 以太类型")
	}
	ip := frame[14:]
	if ip[9] != 17 {
		t.Fatalf("回注帧应为 UDP")
	}
	if !net.IP(ip[12:16]).Equal(net.ParseIP("192.168.99.10")) {
		t.Fatalf("回注 IP 源应为 server 地址，得到 %s", net.IP(ip[12:16]))
	}
	if !net.IP(ip[16:20]).Equal(net.IPv4bcast) {
		t.Fatalf("回注 IP 目的应为广播（客户端尚无地址），得到 %s", net.IP(ip[16:20]))
	}
	// 校验和按新头重算（改目的地址与端口后必须重算）：把校验和字段归零后重算应等于帧里的值。
	ipZeroed := append([]byte(nil), ip[:20]...)
	binary.BigEndian.PutUint16(ipZeroed[10:12], 0)
	if got, want := binary.BigEndian.Uint16(ip[10:12]), relayIPChecksum(ipZeroed); got != want {
		t.Fatalf("IP 头校验和未按新头重算：%#x != %#x", got, want)
	}
	ihl := int(ip[0]&0x0f) * 4
	udp := ip[ihl:]
	if binary.BigEndian.Uint16(udp[0:2]) != relayServerPort || binary.BigEndian.Uint16(udp[2:4]) != relayClientPort {
		t.Fatalf("回注 UDP 端口应为 67→68，得到 %d→%d",
			binary.BigEndian.Uint16(udp[0:2]), binary.BigEndian.Uint16(udp[2:4]))
	}
	zeroed := append([]byte(nil), udp...)
	binary.BigEndian.PutUint16(zeroed[6:8], 0)
	if got, want := binary.BigEndian.Uint16(udp[6:8]), relayUDPChecksum(ip[12:16], ip[16:20], zeroed); got != want {
		t.Fatalf("UDP 校验和未按新头重算：%#x != %#x", got, want)
	}
	// 载荷字节保真（含 chaddr/xid/选项）。
	if got := udp[8:]; string(got) != string(reply) {
		t.Fatalf("回注载荷应字节保真")
	}
	if st, ok := m.State(vs.Name); !ok || st.Injected != 1 || st.Forwarded != 1 {
		t.Fatalf("运行态计数应为 转发1/回注1，得到 %+v", st)
	}
}

func TestRelayDropsUnknownReplyAndCounts(t *testing.T) {
	layer := &fakeRelayLayer{}
	m := newRelayManager(layer)
	vs := relayedSwitch("192.168.99.10")
	if err := m.Sync(context.Background(), vs); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	client := mustMAC("02:00:00:00:00:01")
	fr, up := layer.lastFrame(), layer.lastUplink()
	// 没有任何请求登记的 xid：如实丢弃并计数（不静默）。
	up.feed(testDHCPPayload(relayBootReply, 0x9999, 2, nil, client, nil))
	waitFor(t, "丢弃计数出现", func() bool {
		st, _ := m.State(vs.Name)
		return st.DroppedNoClient == 1
	})
	if n := len(fr.sentFrames()); n != 0 {
		t.Fatalf("找不到客户端的应答不得回注，实际回注 %d 条", n)
	}
	// 非 BOOTREPLY（客户端单播到 BVI:67 的报文经内核递到本 socket）：不属于应答，不计数也不回注。
	up.feed(testDHCPPayload(relayBootRequest, 0x9999, 3, nil, client, nil))
	time.Sleep(20 * time.Millisecond)
	st, _ := m.State(vs.Name)
	if st.DroppedNoClient != 1 || st.Injected != 0 {
		t.Fatalf("非应答报文不得计为丢弃/回注，得到 %+v", st)
	}
}

// 连接式 UDP 的 ICMP 一次性差错（server 未监听/路由不可达）**不得**杀死实例：如实记入
// 读视图的原因，随后照常收发。
func TestRelayUplinkTransientErrorKeepsRunning(t *testing.T) {
	layer := &fakeRelayLayer{}
	m := newRelayManager(layer)
	vs := relayedSwitch("192.168.99.10")
	if err := m.Sync(context.Background(), vs); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	client := mustMAC("02:00:00:00:00:01")
	fr, up := layer.lastFrame(), layer.lastUplink()
	// ICMP 差错：读返回一次 ECONNREFUSED。
	up.feedErr(syscall.ECONNREFUSED)
	waitFor(t, "差错记入运行态原因", func() bool {
		st, _ := m.State(vs.Name)
		// 平台无关的标记：差错被记入运行态原因且实例仍在跑（Windows 的 Errno 文案与 Linux 不同）。
		return st.Running && strings.Contains(st.Reason, "已继续读")
	})
	// 差错之后照常中继：请求已转发、应答能回注。
	const xid = 0x5150
	fr.feed(testClientFrame(client, testDHCPPayload(relayBootRequest, xid, 3, nil, client, nil)))
	waitFor(t, "请求仍被转发", func() bool { return len(up.sentPayloads()) == 1 })
	up.feed(testDHCPPayload(relayBootReply, xid, 5, nil, client, nil))
	waitFor(t, "应答仍被回注", func() bool { return len(fr.sentFrames()) == 1 })
	if st, _ := m.State(vs.Name); !st.Running || st.Injected != 1 {
		t.Fatalf("差错不应影响回注计数与运行态，得到 %+v", st)
	}
}

func TestRelayPendingEntryExpires(t *testing.T) {
	layer := &fakeRelayLayer{}
	m := newRelayManager(layer)
	now := time.Now()
	m.now = func() time.Time { return now }
	vs := relayedSwitch("192.168.99.10")
	if err := m.Sync(context.Background(), vs); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	client := mustMAC("02:00:00:00:00:01")
	fr, up := layer.lastFrame(), layer.lastUplink()
	fr.feed(testClientFrame(client, testDHCPPayload(relayBootRequest, 0x42, 1, nil, client, nil)))
	waitFor(t, "请求已转发", func() bool { return len(up.sentPayloads()) == 1 })

	// 短 TTL 过期：应答按表查不到客户端 ⇒ 丢弃并计数（不静默），不回注。
	now = now.Add(relayPendingTTL + time.Second)
	up.feed(testDHCPPayload(relayBootReply, 0x42, 2, nil, client, nil))
	waitFor(t, "过期后丢弃计数出现", func() bool {
		st, _ := m.State(vs.Name)
		return st.DroppedNoClient == 1
	})
	if n := len(fr.sentFrames()); n != 0 {
		t.Fatalf("登记过期的应答不得回注，实际回注 %d 条", n)
	}
}

// ---------- 生命周期 ----------

func TestRelaySyncIsIdempotentAndTeardownReleasesSockets(t *testing.T) {
	layer := &fakeRelayLayer{}
	m := newRelayManager(layer)
	vs := relayedSwitch("192.168.99.10")
	for i := 0; i < 3; i++ {
		if err := m.Sync(context.Background(), vs); err != nil {
			t.Fatal(err)
		}
	}
	if f, u := layer.counts(); f != 1 || u != 1 {
		t.Fatalf("重复 Sync 必须幂等（不得重复开 socket），实际 frames=%d uplinks=%d", f, u)
	}
	// 实例在跑（State 可见）。停：清声明（server 为空）⇒ 两个底座关闭、收发协程退出。
	inst := m.insts[vs.Name]
	if inst == nil {
		t.Fatal("实例应在册")
	}
	if err := m.Sync(context.Background(), model.VirtualSwitch{Name: vs.Name}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inst.done:
	default:
		t.Fatalf("停止后收发协程应已退出（socket/goroutine 不残留）")
	}
	if !layer.lastFrame().isClosed() || !layer.lastUplink().isClosed() {
		t.Fatalf("停止后两个底座都应关闭")
	}
	if _, ok := m.State(vs.Name); ok {
		t.Fatalf("停止后不应再报运行态（无实例）")
	}
	// 再停一次：幂等空操作。
	if err := m.Stop(vs.Name); err != nil {
		t.Fatalf("重复停止应幂等，得到 %v", err)
	}
}

func TestRelayRestartsOnTargetChange(t *testing.T) {
	layer := &fakeRelayLayer{}
	m := newRelayManager(layer)
	old := relayedSwitch("192.168.99.10")
	if err := m.Sync(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	oldFr, oldUp := layer.lastFrame(), layer.lastUplink()

	// 改 server：按新规格重建（旧底座关闭，不残留）。
	next := relayedSwitch("192.168.99.11")
	if err := m.Sync(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if !oldFr.isClosed() || !oldUp.isClosed() {
		t.Fatalf("改 server 时旧实例的两个底座都应关闭")
	}
	if f, u := layer.counts(); f != 2 || u != 2 {
		t.Fatalf("改 server 应重建实例（再开一套底座），实际 frames=%d uplinks=%d", f, u)
	}
	_, servers := layer.openArgs()
	if len(servers) != 2 || !servers[1].Equal(net.ParseIP("192.168.99.11")) {
		t.Fatalf("新实例应指向新 server，得到 %v", servers)
	}
	// 换网关地址（同 server）：同样重建。
	lastFr := layer.lastFrame()
	moved := relayedSwitch("192.168.99.11")
	moved.Gateway = &model.VSGateway{Addresses: []string{"192.168.99.2/24"}}
	if err := m.Sync(context.Background(), moved); err != nil {
		t.Fatal(err)
	}
	if !lastFr.isClosed() {
		t.Fatalf("换中继源地址时应重建实例")
	}
	srcs, _ := layer.openArgs()
	if !srcs[len(srcs)-1].Equal(net.ParseIP("192.168.99.2")) {
		t.Fatalf("重建后的中继源应为新网关地址，得到 %v", srcs[len(srcs)-1])
	}
}

func TestRelayStartFailureIsHonestAndRetried(t *testing.T) {
	layer := &fakeRelayLayer{bridgeErr: net.UnknownNetworkError("不存在")}
	m := newRelayManager(layer)
	vs := relayedSwitch("192.168.99.10")
	err := m.Sync(context.Background(), vs)
	if err == nil {
		t.Fatal("bridge 打不开时必须如实报错（不得静默）")
	}
	if !strings.Contains(err.Error(), vs.Name) || !strings.Contains(err.Error(), "vs-r") {
		t.Fatalf("报错应点名交换机与 bridge，得到 %v", err)
	}
	// 起不来也要在读视图可见（"为什么没跑"）——未运行 + 原因。
	st, ok := m.State(vs.Name)
	if !ok || st.Running || st.Reason == "" {
		t.Fatalf("启动失败应给出运行态（未运行 + 原因），得到 %+v ok=%v", st, ok)
	}
	// 条件恢复后重试成功（15s 巡检的补启路径）。
	layer.mu.Lock()
	layer.bridgeErr = nil
	layer.mu.Unlock()
	if err := m.Sync(context.Background(), vs); err != nil {
		t.Fatalf("恢复后重试应成功，得到 %v", err)
	}
	if st, ok := m.State(vs.Name); !ok || !st.Running || st.Reason != "" {
		t.Fatalf("重试成功后应报运行中且无原因，得到 %+v", st)
	}
	_ = m.Close()
}

func TestRelayUplinkBindFailureReleasesFrameSocket(t *testing.T) {
	layer := &fakeRelayLayer{uplinkErr: net.UnknownNetworkError("地址不可绑定")}
	m := newRelayManager(layer)
	if err := m.Sync(context.Background(), relayedSwitch("192.168.99.10")); err == nil {
		t.Fatal("上行 socket 打不开时必须如实报错")
	}
	if f, u := layer.counts(); f != 1 || u != 0 {
		t.Fatalf("上行失败时不应留下上行 socket，frames=%d uplinks=%d", f, u)
	}
	if !layer.lastFrame().isClosed() {
		t.Fatalf("上行失败时已开的 bridge 底座必须关掉（不残留 socket）")
	}
}

func TestRelayManagerCloseIsIdempotent(t *testing.T) {
	layer := &fakeRelayLayer{}
	m := newRelayManager(layer)
	if err := m.Sync(context.Background(), relayedSwitch("192.168.99.10")); err != nil {
		t.Fatal(err)
	}
	inst := m.insts["vs-r"]
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close 应幂等，得到 %v", err)
	}
	select {
	case <-inst.done:
	default:
		t.Fatalf("Close 后收发协程应已退出")
	}
	if !layer.lastUplink().isClosed() {
		t.Fatalf("Close 后底座应关闭")
	}
}

// ---------- Provider 接线 ----------

func TestProviderApplyDhcpRelayRunsInstanceAndDeleteStopsIt(t *testing.T) {
	layer := &fakeRelayLayer{}
	p := New(&fakeRunner{})
	p.SetRelaySocketLayer(layer)
	ctx := context.Background()
	vs := relayedSwitch("192.168.99.10")

	if err := p.ApplyDhcpRelay(ctx, vs); err != nil {
		t.Fatal(err)
	}
	if st, ok := p.DHCPRelayState(vs.Name); !ok || !st.Running {
		t.Fatalf("提交后中继实例应在运行，得到 %+v ok=%v", st, ok)
	}
	// 未声明（提交编排对每台 L2 交换机都调用一次）：不报错、不产生实例。
	if err := p.ApplyDhcpRelay(ctx, model.VirtualSwitch{Name: "vs-plain"}); err != nil {
		t.Fatalf("未声明中继的交换机应空操作，得到 %v", err)
	}
	if _, ok := p.DHCPRelayState("vs-plain"); ok {
		t.Fatalf("未声明的交换机不应有中继运行态")
	}
	// 交换机删除：DeleteBridgeDomain 必须先停中继（先解引用、后删被引用）。
	if err := p.DeleteBridgeDomain(ctx, vs.Name); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.DHCPRelayState(vs.Name); ok {
		t.Fatalf("交换机删除后中继实例应已停止")
	}
	if !layer.lastFrame().isClosed() || !layer.lastUplink().isClosed() {
		t.Fatalf("交换机删除后中继底座应关闭")
	}
}

func TestProviderEnsureConsistentReplaysRelay(t *testing.T) {
	layer := &fakeRelayLayer{}
	// 转发开关读回 1（否则恢复重放的第一个未收敛项是宿主转发前置，会掩盖本用例关心的中继重放）。
	p := New(&fakeRunner{replies: []fakeReply{
		{prefix: "sysctl -n net.ipv4.ip_forward", out: "1"},
		{prefix: "sysctl -n net.ipv6.conf.all.forwarding", out: "1"},
	}})
	p.SetRelaySocketLayer(layer)
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{relayedSwitch("192.168.99.10")}}

	// 恢复重放（进程重启后的路径）：声明了 relay 就必须把实例起回来。
	if errs := p.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("恢复重放不应有未收敛项，得到 %v", errs)
	}
	if st, ok := p.DHCPRelayState("vs-r"); !ok || !st.Running {
		t.Fatalf("恢复重放后中继实例应在运行，得到 %+v ok=%v", st, ok)
	}
	if f, u := layer.counts(); f != 1 || u != 1 {
		t.Fatalf("恢复重放应起一套实例，frames=%d uplinks=%d", f, u)
	}
	// 幂等：再重放一次不重复开 socket。
	if errs := p.EnsureConsistent(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("重复重放不应有未收敛项，得到 %v", errs)
	}
	if f, u := layer.counts(); f != 1 || u != 1 {
		t.Fatalf("重复恢复重放应幂等，frames=%d uplinks=%d", f, u)
	}
}

func TestProviderReconcileProxyHealsAndAlarmsHonestly(t *testing.T) {
	layer := &fakeRelayLayer{bridgeErr: net.UnknownNetworkError("bridge 不在")}
	p := New(&fakeRunner{})
	p.SetRelaySocketLayer(layer)
	alarms := network.NewAlarmStore()
	p.SetAlarms(alarms)
	ctx := context.Background()
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{relayedSwitch("192.168.99.10")}}

	// 起不来：如实返回错误 + 进未收敛项（recovery scope 的告警，事后可查）。
	errs := p.ReconcileProxy(ctx, cfg)
	if len(errs) == 0 {
		t.Fatal("中继起不来时巡检必须如实报错（不静默）")
	}
	if !hasRelayAlarm(alarms) {
		t.Fatalf("中继未收敛应进告警表（show alarms 事后可查）")
	}

	// 条件恢复：下一轮补启成功并自动消解该告警。
	layer.mu.Lock()
	layer.bridgeErr = nil
	layer.mu.Unlock()
	if errs := p.ReconcileProxy(ctx, cfg); len(errs) != 0 {
		t.Fatalf("恢复后巡检应无未收敛项，得到 %v", errs)
	}
	if hasRelayAlarm(alarms) {
		t.Fatalf("中继恢复运行后告警应自动消解")
	}

	// 已不声明的实例：巡检兜底停掉（交换机删除走 DeleteBridgeDomain，这里防漏）。
	if errs := p.ReconcileProxy(ctx, model.Config{}); len(errs) != 0 {
		t.Fatalf("对空配置对账应无错误，得到 %v", errs)
	}
	if _, ok := p.DHCPRelayState("vs-r"); ok {
		t.Fatalf("声明消失后实例应被停掉")
	}
}
