package netkernel

// 内核数据面 DHCP 服务器（v3 决策 #438）单测：**不依赖真内核/真 socket**——内核命令走内存
// Runner（dhcpFakeHost）、内置 tap 的以太帧收发与单播接收 socket 都是可注入底座，本文件注入
// 内存实现，校验：tap 建/删/按名核对身份（含同名复用与索引被复用不误删）、**复用既有服务器
// 核心**的报文处理（DISCOVER→OFFER、REQUEST→ACK、池耗尽告警）+ 租约文件持久化、单播续租经
// UDP 底座到达、生命周期（起→停无残留、幂等 Sync、交换机删除、恢复重放重建）、起不来如实报错。

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// ---------- 内存内核（Runner 假实现） ----------

// dhcpFakeLink 一台内核接口（只覆盖 DHCP 服务器路径用到的事实）。
type dhcpFakeLink struct {
	index  uint32
	kind   string // bridge / vrf / tun / ""（物理口）
	master string
	up     bool
	mac    string
	addrs  []string
}

// dhcpFakeHost 内存内核：按命令改/读一份设备表，并记录执行过的命令。
// 覆盖内核 DHCP 服务器路径实际发出的命令（tuntap add/del、link set master/up/down、
// ip -d -j link show、bridge -j link show、link del）；其余命令返回空输出（与既有
// fakeRunner 同法，便于同一测试里混用别的族）。
type dhcpFakeHost struct {
	mu    sync.Mutex
	links map[string]*dhcpFakeLink
	next  uint32
	cmds  []string
	// failTuntap 注入 `ip tuntap add` 失败（tap 建不出：如实报错路径）。
	failTuntap bool
}

func newDHCPFakeHost() *dhcpFakeHost {
	return &dhcpFakeHost{links: map[string]*dhcpFakeLink{}, next: 100}
}

// addLink 预置一台设备（bridge/vrf/物理口/既有 tap），返回其 ifindex。
func (h *dhcpFakeHost) addLink(name, kind string, addrs ...string) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	mac := "02:00:00:00:00:01"
	h.links[name] = &dhcpFakeLink{index: h.next, kind: kind, mac: mac, addrs: addrs}
	return h.next
}

func (h *dhcpFakeHost) link(name string) *dhcpFakeLink {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.links[name]
}

func (h *dhcpFakeHost) has(name string) bool { return h.link(name) != nil }

// linkJSON 按 `ip -d -j link show` 的形状渲染设备表。
func (h *dhcpFakeHost) linkJSON() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	type row struct {
		Ifname   string `json:"ifname"`
		Ifindex  uint32 `json:"ifindex"`
		Address  string `json:"address"`
		Master   string `json:"master,omitempty"`
		LinkInfo *struct {
			InfoKind string `json:"info_kind"`
		} `json:"linkinfo,omitempty"`
	}
	rows := make([]row, 0, len(h.links))
	for name, l := range h.links {
		r := row{Ifname: name, Ifindex: l.index, Address: l.mac, Master: l.master}
		if l.kind != "" {
			r.LinkInfo = &struct {
				InfoKind string `json:"info_kind"`
			}{InfoKind: l.kind}
		}
		rows = append(rows, r)
	}
	b, _ := json.Marshal(rows)
	return string(b)
}

// bridgeMembersJSON 按 `bridge -j link show` 的形状渲染「成员口 → master」。
func (h *dhcpFakeHost) bridgeMembersJSON() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	type row struct {
		Ifname string `json:"ifname"`
		Master string `json:"master"`
	}
	rows := []row{}
	for name, l := range h.links {
		if l.master != "" {
			rows = append(rows, row{Ifname: name, Master: l.master})
		}
	}
	b, _ := json.Marshal(rows)
	return string(b)
}

func (h *dhcpFakeHost) lastCmds() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.cmds...)
}

func (h *dhcpFakeHost) hasCmd(sub string) bool {
	for _, c := range h.lastCmds() {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func (h *dhcpFakeHost) countCmd(sub string) int {
	n := 0
	for _, c := range h.lastCmds() {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func (h *dhcpFakeHost) Run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	h.mu.Lock()
	h.cmds = append(h.cmds, line)
	h.mu.Unlock()

	switch {
	case line == "ip -d -j link show":
		return h.linkJSON(), nil
	case strings.HasPrefix(line, "sysctl -n "):
		return "1", nil // 恢复收敛的转发开关读回（假内核一律「已开」）
	case strings.HasPrefix(line, "bridge -j link show"):
		return h.bridgeMembersJSON(), nil
	case strings.HasPrefix(line, "ip tuntap add dev "):
		fields := strings.Fields(line) // ip tuntap add dev <name> mode tap
		if len(fields) < 6 {
			return "", fmt.Errorf("bad tuntap add: %s", line)
		}
		dev := fields[4]
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.failTuntap {
			return "ioctl(TUNSETIFF): Operation not permitted", fmt.Errorf("ip tuntap add: 注入失败")
		}
		if _, ok := h.links[dev]; ok {
			return "ioctl(TUNSETIFF): Device or resource busy", fmt.Errorf("exists")
		}
		h.next++
		h.links[dev] = &dhcpFakeLink{index: h.next, kind: "tun", mac: "02:00:00:00:00:77"}
		return "", nil
	case strings.HasPrefix(line, "ip tuntap del dev "):
		fields := strings.Fields(line)
		if len(fields) < 6 {
			return "", fmt.Errorf("bad tuntap del: %s", line)
		}
		dev := fields[4]
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.links[dev]; !ok {
			return "ioctl(TUNSETIFF): No such device", fmt.Errorf("no such device")
		}
		delete(h.links, dev)
		return "", nil
	case strings.HasPrefix(line, "ip link del "):
		dev := strings.Fields(line)[3]
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.links[dev]; !ok {
			return fmt.Sprintf("Cannot find device %q", dev), fmt.Errorf("no such device")
		}
		delete(h.links, dev)
		return "", nil
	case strings.HasPrefix(line, "ip link set dev "):
		fields := strings.Fields(line) // ip link set dev <name> <verb> [arg]
		dev := fields[4]
		h.mu.Lock()
		defer h.mu.Unlock()
		l, ok := h.links[dev]
		if !ok {
			return fmt.Sprintf("Cannot find device %q", dev), fmt.Errorf("no such device")
		}
		switch {
		case len(fields) >= 6 && fields[5] == "up":
			l.up = true
		case len(fields) >= 6 && fields[5] == "down":
			l.up = false
		case len(fields) >= 6 && fields[5] == "nomaster":
			l.master = ""
		case len(fields) >= 7 && fields[5] == "master":
			master := fields[6]
			if _, ok := h.links[master]; !ok {
				return fmt.Sprintf("Cannot find device %q", master), fmt.Errorf("no such device")
			}
			l.master = master
		}
		return "", nil
	}
	return "", nil
}

// ---------- 内存内置 tap（**持有**与 fd 收发） ----------
//
// 内核数据面下内置 tap 由产品打开 /dev/net/tun 并 TUNSETIFF 认领后**长期持有**（carrier 由持有者
// 保证），收发直接读写该 fd——见 dhcpserver_tap.go 的文件头。本假实现替换的是**原始系统操作**
// （dhcpTunOps），于是被点测的是**真实现** tunTapTransport / kernelTapLayer 本身；测试因此能在
// 任意平台断言「持有步骤在场」与「收发走 fd」两条不变量，且不碰真设备。

// fakeTunOps 内存版：记录打开/认领/关闭与收发的帧，并作为「该 tap 的文件描述符」被读写。
type fakeTunOps struct {
	name string

	mu        sync.Mutex
	ifaceMAC  net.HardwareAddr
	nextFD    int
	opens     []string // OpenCharDevice 的路径
	claims    []string // ClaimIface 的设备名（＝被持有过的证据）
	closedFDs []int
	sent      [][]byte // 写出的帧（tap → bridge）
	queue     [][]byte // 待读的帧（bridge → tap）
	heldNow   bool     // 当前是否被持有（认领成功→真；CloseFD→假）
	wake      chan struct{}
	isClosed  bool
	failClaim bool
	failOpen  bool
	noSuchDev bool
}

func (o *fakeTunOps) IfaceMAC(name string) (net.HardwareAddr, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.noSuchDev {
		return nil, fmt.Errorf("no such interface %s", name)
	}
	return o.ifaceMAC, nil
}

func (o *fakeTunOps) OpenCharDevice(path string) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failOpen {
		return 0, fmt.Errorf("open %s: operation not permitted", path)
	}
	o.opens = append(o.opens, path)
	o.nextFD++
	return o.nextFD, nil
}

func (o *fakeTunOps) ClaimIface(fd int, name string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failClaim {
		return fmt.Errorf("ioctl(TUNSETIFF, %s): device or resource busy", name)
	}
	o.claims = append(o.claims, name)
	o.heldNow = true
	return nil
}

func (o *fakeTunOps) WaitReadable(_ int, timeout time.Duration) (bool, error) {
	o.mu.Lock()
	if len(o.queue) > 0 {
		o.mu.Unlock()
		return true, nil
	}
	if o.wake == nil {
		o.wake = make(chan struct{})
	}
	ch := o.wake
	o.mu.Unlock()
	select {
	case <-ch:
		return true, nil
	case <-time.After(timeout):
		return false, nil
	}
}

func (o *fakeTunOps) ReadFrame(_ int, buf []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.queue) == 0 {
		return 0, nil // 本次无可读帧（与真实现的 EAGAIN 口径一致）
	}
	f := o.queue[0]
	o.queue = o.queue[1:]
	return copy(buf, f), nil
}

func (o *fakeTunOps) WriteFrame(_ int, frame []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.isClosed {
		return 0, fmt.Errorf("write: bad file descriptor")
	}
	o.sent = append(o.sent, append([]byte(nil), frame...))
	return len(frame), nil
}

func (o *fakeTunOps) CloseFD(fd int) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closedFDs = append(o.closedFDs, fd)
	o.isClosed = true
	o.heldNow = false
	return nil
}

// ---- 测试面向的小助手（与旧内存 tap 同名，便于既有用例复用）----

func (o *fakeTunOps) feed(frame []byte) {
	o.mu.Lock()
	o.queue = append(o.queue, append([]byte(nil), frame...))
	if o.wake != nil {
		close(o.wake)
		o.wake = nil
	}
	o.mu.Unlock()
}

func (o *fakeTunOps) frames() [][]byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([][]byte(nil), o.sent...)
}

func (o *fakeTunOps) closed() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.isClosed
}

// held 该 tap 是否曾被**认领**（＝有持有者 ⇒ carrier 由持有者保证）。
func (o *fakeTunOps) held() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.claims) > 0
}

func (o *fakeTunOps) openedPaths() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.opens...)
}

func (o *fakeTunOps) claimedNames() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.claims...)
}

func (o *fakeTunOps) closeCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.closedFDs)
}

// fakeTapLayer 每台交换机一个内存 tap 底座；返回的是**真实现** tunTapTransport（见文件头）。
// 同一 tap 未关闭前再次 Open 按真机语义拒绝（单队列设备不允许两个持有者：EBUSY）。
type fakeTapLayer struct {
	mu    sync.Mutex
	devs  map[string]*fakeTunOps
	opens int
}

func newFakeTapLayer() *fakeTapLayer { return &fakeTapLayer{devs: map[string]*fakeTunOps{}} }

func (l *fakeTapLayer) Open(name string) (network.TapTransport, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.opens++
	dev := l.devs[name]
	if dev == nil {
		mac, _ := net.ParseMAC("02:fe:00:00:00:99")
		dev = &fakeTunOps{name: name, ifaceMAC: mac}
		l.devs[name] = dev
	}
	dev.mu.Lock()
	if dev.heldNow {
		dev.mu.Unlock()
		return nil, fmt.Errorf("认领内核 tap %s（TUNSETIFF，%s）: device or resource busy（同一设备已被持有）", name, tunDevicePath)
	}
	dev.isClosed = false
	dev.mu.Unlock()
	return kernelTapLayer{ops: dev}.Open(name)
}

func (l *fakeTapLayer) get(name string) *fakeTunOps {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.devs[name]
}

func (l *fakeTapLayer) openCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.opens
}

// ---------- 内存单播底座（UDP/67 绑 BVI 地址） ----------

type fakeUnicastDatagram struct {
	payload []byte
	from    net.IP
}

// fakeUnicastIO 一台交换机的单播接收 socket（内存）。
type fakeUnicastIO struct {
	bvi    net.IP
	vrf    string
	in     chan fakeUnicastDatagram
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	closes int
}

func (s *fakeUnicastIO) Recv(buf []byte) (int, *net.UDPAddr, error) {
	select {
	case <-s.done:
		return 0, nil, net.ErrClosed
	case d := <-s.in:
		n := copy(buf, d.payload)
		return n, &net.UDPAddr{IP: d.from, Port: 68}, nil
	}
}

func (s *fakeUnicastIO) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closes++
		s.mu.Unlock()
		close(s.done)
	})
	return nil
}

func (s *fakeUnicastIO) closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *fakeUnicastIO) feed(payload []byte, from net.IP) {
	s.in <- fakeUnicastDatagram{payload: payload, from: from}
}

// fakeUnicastLayer 单播底座假实现：Open 校验 BVI 地址确实在内核里（模拟真机 EADDRNOTAVAIL）。
type fakeUnicastLayer struct {
	host   *dhcpFakeHost
	mu     sync.Mutex
	opened map[string]*fakeUnicastIO
	opens  []string
}

func newFakeUnicastLayer(host *dhcpFakeHost) *fakeUnicastLayer {
	return &fakeUnicastLayer{host: host, opened: map[string]*fakeUnicastIO{}}
}

func (l *fakeUnicastLayer) Open(bvi net.IP, vrfDevice string) (dhcpUnicastIO, error) {
	if bvi.To4() == nil {
		return nil, fmt.Errorf("BVI %v 不是 IPv4", bvi)
	}
	if vrfDevice == "" {
		return nil, fmt.Errorf("VRF 设备名为空")
	}
	if !l.host.has(vrfDevice) {
		return nil, fmt.Errorf("bind: cannot assign requested address（内核里没有 %s）", vrfDevice)
	}
	key := bvi.String() + "|" + vrfDevice
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur := l.opened[key]; cur != nil && !cur.closed() {
		return nil, fmt.Errorf("bind: address already in use（%s:%d 已绑定）", bvi, dhcpUnicastPort)
	}
	s := &fakeUnicastIO{bvi: bvi, vrf: vrfDevice, in: make(chan fakeUnicastDatagram, 8), done: make(chan struct{})}
	l.opened[key] = s
	l.opens = append(l.opens, key)
	return s, nil
}

func (l *fakeUnicastLayer) socket(bvi string, vrf string) *fakeUnicastIO {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.opened[bvi+"|"+vrf]
}

func (l *fakeUnicastLayer) openCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.opens)
}

// ---------- 装配与报文构造 ----------

// vsKernelDHCPServer 一台已配 DHCP 服务器的测试交换机（L2 + IPv4 BVI + 3 地址小池）。
func vsKernelDHCPServer(name string) model.VirtualSwitch {
	return model.VirtualSwitch{
		Name: name, Type: "l2",
		Gateway:                    &model.VSGateway{Addresses: []string{"192.168.99.1/24"}},
		DhcpServerPoolStart:        "192.168.99.10",
		DhcpServerPoolEnd:          "192.168.99.12",
		DhcpServerLeaseTimeSeconds: 7200,
	}
}

// kernelDHCPServerFixture 一套装配：内存内核 + 可注入的两个传输面 + 临时租约目录 + 告警表。
type kernelDHCPServerFixture struct {
	p      *Provider
	host   *dhcpFakeHost
	taps   *fakeTapLayer
	uni    *fakeUnicastLayer
	alarms *network.AlarmStore
	cfg    model.Config
}

func newKernelDHCPServerFixture(t *testing.T, host *dhcpFakeHost, vss ...model.VirtualSwitch) *kernelDHCPServerFixture {
	t.Helper()
	if host == nil {
		host = newDHCPFakeHost()
	}
	f := &kernelDHCPServerFixture{
		host:   host,
		taps:   newFakeTapLayer(),
		uni:    newFakeUnicastLayer(host),
		alarms: network.NewAlarmStore(),
		cfg:    model.Config{VirtualSwitches: vss},
	}
	f.p = New(host)
	f.p.SetConfig(f.cfg)
	f.p.SetAlarms(f.alarms)
	f.p.SetDHCPServerTapLayer(f.taps)
	f.p.SetDHCPServerSocketLayer(f.uni)
	f.p.SetDHCPServerLeaseDir(t.TempDir())
	return f
}

// seedBridge 在内核里预置该交换机的 bridge（含 BVI 地址）与该域 VRF（产品收敛后的形态）。
func (f *kernelDHCPServerFixture) seedBridge(vs model.VirtualSwitch) {
	if vs.Gateway == nil || len(vs.Gateway.Addresses) == 0 {
		return
	}
	vrf := GatewayVRFName(vs.Name)
	if vs.Gateway.Vrf != "" {
		vrf = LinkName(vs.Gateway.Vrf)
	}
	f.host.addLink(vrf, "vrf")
	f.host.addLink(LinkName(vs.Name), "bridge", vs.Gateway.Addresses...)
}

// tapName 该交换机的内置 tap 名（内核侧名）。
func tapNameOf(vs model.VirtualSwitch) string { return network.DHCPServerTapName(vs.Name) }

// dhcpTestXID 每条测试帧一个不同 xid：服务器核心的双入径去重按 (chaddr,xid,类型) 判重，
// 固定 xid 会让「先 tap 上的 DISCOVER，再 UDP 上的续租」这类序列被误判为重复。
var dhcpTestXID atomic.Uint32

// kernelDHCPBody 构造 BOOTP/DHCP 报文主体（客户端 → 服务器）。
func kernelDHCPBody(chaddr net.HardwareAddr, msgType byte, opt50, opt54, ciaddr net.IP) []byte {
	body := make([]byte, 240)
	body[0], body[1], body[2] = 1, 1, 6 // BOOTREQUEST / Ethernet / hlen 6
	binary.BigEndian.PutUint32(body[4:8], 0x22334455+dhcpTestXID.Add(1))
	copy(body[28:34], chaddr)
	if ciaddr != nil {
		copy(body[12:16], ciaddr.To4())
	}
	binary.BigEndian.PutUint32(body[236:240], 0x63825363)
	opts := []byte{53, 1, msgType}
	if opt50 != nil {
		opts = append(opts, 50, 4)
		opts = append(opts, opt50.To4()...)
	}
	if opt54 != nil {
		opts = append(opts, 54, 4)
		opts = append(opts, opt54.To4()...)
	}
	opts = append(opts, 255)
	return append(body, opts...)
}

// kernelDHCPClientFrame 以太帧形态（tap 入径：广播）。
func kernelDHCPClientFrame(chaddr net.HardwareAddr, msgType byte, opt50, opt54, ciaddr net.IP) []byte {
	body := kernelDHCPBody(chaddr, msgType, opt50, opt54, ciaddr)
	udp := make([]byte, 8)
	binary.BigEndian.PutUint16(udp[0:2], 68)
	binary.BigEndian.PutUint16(udp[2:4], 67)
	binary.BigEndian.PutUint16(udp[4:6], uint16(8+len(body)))
	ip := make([]byte, 20)
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(20+8+len(body)))
	ip[8], ip[9] = 64, 17
	copy(ip[12:16], net.IPv4zero.To4())
	copy(ip[16:20], net.IPv4bcast.To4())
	frame := make([]byte, 14)
	copy(frame[0:6], net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	binary.BigEndian.PutUint16(frame[12:14], 0x0800)
	frame = append(frame, ip...)
	frame = append(frame, udp...)
	return append(frame, body...)
}

// kernelReplyFields 一条应答帧的关键字段。
type kernelReplyFields struct {
	ethDst, ethSrc net.HardwareAddr
	ipSrc          net.IP
	body           []byte
}

func parseKernelReply(t *testing.T, f []byte) kernelReplyFields {
	t.Helper()
	if len(f) < 14+20+8+240 {
		t.Fatalf("应答帧太短: %d", len(f))
	}
	var r kernelReplyFields
	r.ethDst, r.ethSrc = net.HardwareAddr(f[0:6]), net.HardwareAddr(f[6:12])
	r.ipSrc = net.IP(f[26:30]).To4()
	r.body = f[14+20+8:]
	if binary.BigEndian.Uint32(r.body[236:240]) != 0x63825363 {
		t.Fatal("应答缺 magic cookie")
	}
	return r
}

// replyMsgType 应答的 option 53。
func replyMsgType(body []byte) byte { return kernelFindOption(body, 53)[0] }

// kernelFindOption 从报文主体里取指定选项值。
func kernelFindOption(body []byte, code byte) []byte {
	for i := 240; i < len(body); {
		if body[i] == 0 {
			i++
			continue
		}
		if body[i] == 255 || i+1 >= len(body) {
			return nil
		}
		l := int(body[i+1])
		if i+2+l > len(body) {
			return nil
		}
		if body[i] == code {
			return body[i+2 : i+2+l]
		}
		i += 2 + l
	}
	return nil
}

// waitFrames 等 tap 上出现第 n 帧（服务器应答是异步的）。
func waitFrames(t *testing.T, tap *fakeTunOps, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fs := tap.frames(); len(fs) >= n {
			return fs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待第 %d 帧超时（当前 %d 帧）", n, len(tap.frames()))
	return nil
}

func mustMACT(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	m, err := net.ParseMAC(s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func poolAlarmActive(store *network.AlarmStore, source string) bool {
	for _, a := range store.List("active") {
		if a.Code == network.AlarmDHCPPoolExhausted && a.Source == source {
			return true
		}
	}
	return false
}

// ---------- tap 建/删/身份核对 ----------

func TestKernelDHCPServerCreateTapAndJoinBridge(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d")
	f := newKernelDHCPServerFixture(t, nil, vs)
	f.seedBridge(vs)

	if err := f.p.ApplyDHCPServer(context.Background(), vs); err != nil {
		t.Fatalf("收敛 DHCP 服务器失败: %v", err)
	}
	tap := f.host.link(tapNameOf(vs))
	if tap == nil {
		t.Fatalf("应创建内核内置 tap %s；实际命令：\n%s", tapNameOf(vs), strings.Join(f.host.lastCmds(), "\n"))
	}
	if tap.kind != "tun" {
		t.Fatalf("内置 tap 应是 TUN/TAP 设备，得到 %q", tap.kind)
	}
	if tap.master != LinkName(vs.Name) {
		t.Fatalf("内置 tap 应 enslave 到交换机内核 bridge %s，得到 %q", LinkName(vs.Name), tap.master)
	}
	if !tap.up {
		t.Fatal("内置 tap 应置 up（down 的口不转发也不收发帧）")
	}
	if !f.host.hasCmd(fmt.Sprintf("ip tuntap add dev %s mode tap", tapNameOf(vs))) {
		t.Fatalf("应经 ip tuntap add 创建；实际：\n%s", strings.Join(f.host.lastCmds(), "\n"))
	}
	// 单播接收 socket：绑 BVI 地址 + 该域 VRF。
	sock := f.uni.socket("192.168.99.1", GatewayVRFName(vs.Name))
	if sock == nil {
		t.Fatalf("应绑定 BVI 地址的单播接收 socket；实际开过的：%v", f.uni.opens)
	}
	// 读视图接通：租约表可用（尚未有租约）、内置 tap 的 ifindex 可查。
	if _, ok := f.p.DHCPServerLeases(vs.Name); !ok {
		t.Fatal("收敛后租约读视图应可用")
	}
	if !f.p.DHCPTapIndexes()[tap.index] {
		t.Fatal("内置 tap 的 ifindex 应进产品自持集合（端口读视图按它过滤）")
	}
	// 端口读视图：内置 tap 不泄漏给用户。
	bds, err := f.p.BridgeDomains()
	if err != nil {
		t.Fatal(err)
	}
	for _, bd := range bds {
		for _, port := range bd.Ports {
			if isProductDHCPTapName(port.Name) {
				t.Fatalf("内置 tap 不应出现在用户端口视图：%+v", bd)
			}
		}
	}
}

func TestKernelDHCPServerReusesExistingTapByName(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d")
	f := newKernelDHCPServerFixture(t, nil, vs)
	f.seedBridge(vs)
	idx := f.host.addLink(tapNameOf(vs), "tun") // 存量 tap（恢复重放/重装现场）

	if err := f.p.ApplyDHCPServer(context.Background(), vs); err != nil {
		t.Fatalf("收敛失败: %v", err)
	}
	if n := f.host.countCmd("ip tuntap add"); n != 0 {
		t.Fatalf("同名 tap 已存在时不应重建（幂等复用）；实际 %d 次 add：\n%s", n, strings.Join(f.host.lastCmds(), "\n"))
	}
	if got := f.host.link(tapNameOf(vs)); got == nil || got.index != idx {
		t.Fatalf("应复用存量 tap（索引 %d 不变），得到 %+v", idx, got)
	}
	if !f.p.DHCPTapIndexes()[idx] {
		t.Fatal("复用的 tap 也应进产品自持集合")
	}
}

func TestKernelDHCPServerTapDeleteByIdentity(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d")
	f := newKernelDHCPServerFixture(t, nil, vs)
	f.seedBridge(vs)
	client := &kernelDHCPServerClient{p: f.p}

	// 1) 索引被复用给别的接口（非内置 tap 名）：**不误删**。
	foreign := f.host.addLink("ens192", "")
	if err := client.TapDelete(foreign); err != nil {
		t.Fatalf("索引指向非内置 tap 时应按已达成处理，得到 %v", err)
	}
	if !f.host.has("ens192") {
		t.Fatal("绝不能按旧索引删掉用户接口")
	}
	if f.host.countCmd("ip tuntap del") != 0 {
		t.Fatal("身份不符时不应删任何设备")
	}
	// 2) 索引已不存在（带外删/内核重启）：按已达成处理、不报错。
	if err := client.TapDelete(999999); err != nil {
		t.Fatalf("索引不存在时应按已达成处理，得到 %v", err)
	}
	// 3) 名字合规的内置 tap：按名删。
	tapIdx := f.host.addLink(tapNameOf(vs), "tun")
	if err := client.TapDelete(tapIdx); err != nil {
		t.Fatalf("删除内置 tap 失败: %v", err)
	}
	if f.host.has(tapNameOf(vs)) {
		t.Fatal("内置 tap 应已删除")
	}
	if !f.host.hasCmd("ip tuntap del dev " + tapNameOf(vs)) {
		t.Fatalf("应按名删除；实际：\n%s", strings.Join(f.host.lastCmds(), "\n"))
	}
}

func TestKernelDHCPServerTapCreateRejectsForeignDeviceWithSameName(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d")
	f := newKernelDHCPServerFixture(t, nil, vs)
	f.host.addLink(tapNameOf(vs), "bridge") // 同名但不是 tap：不冒认
	client := &kernelDHCPServerClient{p: f.p}
	if _, err := client.TapCreate(tapNameOf(vs), "tag"); err == nil {
		t.Fatal("同名设备不是 TUN/TAP 时应如实报错（不冒认）")
	}
	// TapDump 也不把它当成内置 tap。
	dump, err := client.TapDump()
	if err != nil {
		t.Fatal(err)
	}
	if len(dump) != 0 {
		t.Fatalf("非 tun 的同名设备不应进内置 tap 清单：%+v", dump)
	}
}

// VLAN 交换机（vlan_filtering=1）：内置 tap 必须钉到服务 VLAN，否则客户端的 DISCOVER
// 到不了 tap（命令全成功、客户端拿不到地址——典型静默失效）。
func TestKernelDHCPServerTapJoinsServiceVlan(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d")
	vs.VlanAccess = 100
	f := newKernelDHCPServerFixture(t, nil, vs)
	f.seedBridge(vs)
	if err := f.p.ApplyDHCPServer(context.Background(), vs); err != nil {
		t.Fatalf("收敛失败: %v", err)
	}
	want := fmt.Sprintf("bridge vlan add dev %s vid 100 pvid untagged", tapNameOf(vs))
	if !f.host.hasCmd(want) {
		t.Fatalf("应把内置 tap 钉到服务 VLAN；期望命令 %q，实际：\n%s", want, strings.Join(f.host.lastCmds(), "\n"))
	}
	// 未声明 VLAN 的交换机不应产生任何 bridge vlan 命令（不引入新命令、零风险）。
	plain := vsKernelDHCPServer("vs-p")
	g := newKernelDHCPServerFixture(t, nil, plain)
	g.seedBridge(plain)
	if err := g.p.ApplyDHCPServer(context.Background(), plain); err != nil {
		t.Fatalf("收敛失败: %v", err)
	}
	if g.host.countCmd("bridge vlan add") != 0 {
		t.Fatalf("未声明 VLAN 时不应动 VLAN 表:\n%s", strings.Join(g.host.lastCmds(), "\n"))
	}
}

// 纯 trunk（声明了 VLAN 却没有 access/native VLAN）：服务域无法确定 ⇒ 如实报错，不静默。
func TestKernelDHCPServerPureTrunkIsHonestError(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d")
	vs.Ports = []model.VSwitchPort{{Seq: 1, Interface: "ens224", TrunkVlans: []int{100, 200}}}
	f := newKernelDHCPServerFixture(t, nil, vs)
	f.seedBridge(vs)
	err := f.p.ApplyDHCPServer(context.Background(), vs)
	if err == nil {
		t.Fatal("纯 trunk（无 access/native VLAN）时应如实报错")
	}
	if !strings.Contains(err.Error(), "native VLAN") {
		t.Fatalf("错误应说明为什么无法确定服务域并给照做路径，得到 %v", err)
	}
	// 这是**确定性配置错误**（与内核状态无关、重试也不会好）：必须在**建 tap 之前**挡下，
	// 不留孤儿 tap（真机实证过中间态：tap 先建出来、随后步骤失败——提交没落地却留下对象）。
	if f.host.has(tapNameOf(vs)) {
		t.Fatalf("确定性配置错误不得留下孤儿 tap；实际命令：\n%s", strings.Join(f.host.lastCmds(), "\n"))
	}
	if f.host.countCmd("ip tuntap add") != 0 {
		t.Fatalf("应在建 tap 之前就失败（不受内核状态影响的配置错误）：\n%s", strings.Join(f.host.lastCmds(), "\n"))
	}
	if f.p.DHCPTapIndexes()[0] {
		t.Fatal("未建 tap 的产品自持集合应为空")
	}
}

// ---------- 自死锁回归（真机 SIGQUIT 实证：一次提交把整个管理面挂死） ----------

// blockingConfigSource **读取即阻塞**的配置源：模拟「提交期间发动机锁由本次提交自己持有」
// （`Engine.Committed()` 在提交中被重入读取就永久阻塞——真机现场：nfvisd 卡死、CLI 全无响应，
// 直到 SIGQUIT 才恢复）。apply/Sync 路径只要回读它，本文件的用例就会超时失败。
type blockingConfigSource struct {
	reads   atomic.Int32
	release chan struct{}
}

func newBlockingConfigSource() *blockingConfigSource {
	return &blockingConfigSource{release: make(chan struct{})}
}

func (s *blockingConfigSource) Committed() (model.Config, error) {
	s.reads.Add(1)
	<-s.release
	return model.Config{}, nil
}

func (s *blockingConfigSource) close() { close(s.release) }

// runNoBlock 在超时内跑完 fn：读取即阻塞的配置源在场时，回读会表现为**超时**（＝死锁）。
func runNoBlock(t *testing.T, what string, src *blockingConfigSource, fn func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("%s 阻塞：apply/Sync 路径回读了 committed 配置（提交期发动机锁由提交自己持有，"+
			"重入即自死锁）；已读 %d 次", what, src.reads.Load())
	}
}

// 内核 DHCP 服务器的**整条应用/巡检/回收路径**都不得回读配置引擎：交换机事实只从调用点的
// vs 传下去（bridge 名 / 服务 VLAN），读视图只用进程内状态，巡检只用入参 cfg。
func TestKernelDHCPServerApplyNeverReadsCommittedConfig(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d")
	f := newKernelDHCPServerFixture(t, nil, vs)
	f.seedBridge(vs)
	src := newBlockingConfigSource()
	defer src.close()
	f.p.SetConfigSource(src.Committed) // 现场等价：来源在，读取即阻塞

	runNoBlock(t, "ApplyDHCPServer（启用/提交路径）", src, func() error {
		return f.p.ApplyDHCPServer(context.Background(), vs)
	})
	runNoBlock(t, "DHCPServerLeases/ActiveLeases（读视图）", src, func() error {
		if _, ok := f.p.DHCPServerLeases(vs.Name); !ok {
			return fmt.Errorf("收敛后租约读视图应可用")
		}
		if _, ok := f.p.DHCPServerActiveLeases(vs.Name); !ok {
			return fmt.Errorf("收敛后生效租约读视图应可用")
		}
		return nil
	})
	runNoBlock(t, "ReconcileDHCPServer（15s 巡检，cfg 由入参给出）", src, func() error {
		if errs := f.p.ReconcileDHCPServer(context.Background(), f.cfg); len(errs) != 0 {
			return fmt.Errorf("巡检应无错误: %v", errs)
		}
		return nil
	})
	runNoBlock(t, "ApplyDHCPServer（停用/teardown）", src, func() error {
		return f.p.ApplyDHCPServer(context.Background(), model.VirtualSwitch{Name: vs.Name})
	})
	runNoBlock(t, "DeleteBridgeDomain（先停服务器再删 bridge）", src, func() error {
		return f.p.DeleteBridgeDomain(context.Background(), vs.Name)
	})
	if n := src.reads.Load(); n != 0 {
		t.Fatalf("apply/Sync/巡检/回收路径不得读 committed 配置（自死锁根源），实际读 %d 次", n)
	}
}

// 源码级第二道守护：**应用路径上不得再引入 `p.config()`**（＝读 committed 配置 ⇒ 提交期自死锁）。
// 覆盖内核 DHCP 服务器与其巡检、L3 接口/ACL 绑定所在的族文件、DNS 代理与 LLDP 自研收发代理
// （R3-11：`aclByName` 的兜底曾回落读活配置，与 #438 同型）。解码为「去掉行注释后逐行扫」，
// 故注释里提到该调用不会误报。
func TestKernelApplyPathSourceHasNoEngineConfigRead(t *testing.T) {
	for _, file := range []string{"dhcpserver.go", "dhcp_relay.go", "families.go", "dnsproxy.go", "lldp.go"} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("读取 %s: %v", file, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			code := line
			if j := strings.Index(code, "//"); j >= 0 {
				code = code[:j]
			}
			if strings.Contains(code, ".config()") {
				t.Fatalf("%s:%d 在 apply/Sync 路径上读 committed 配置（提交期自死锁根源）：%s",
					file, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// ---------- tap 的持有（carrier 不变量；真机实证的静默失效） ----------

// 真机症状：`ip tuntap add` 建出的**持久化但没有持有者**的 tap 是 `NO-CARRIER, state DOWN`，
// bridge 不会把洪泛帧投给它 ⇒ 客户端 DISCOVER 到不了服务器、租约表恒空（命令全成功）。
// 故本用例钉住两条不变量：① tap 被**打开 /dev/net/tun + TUNSETIFF 认领**（＝有持有者，
// carrier 由持有者保证）；② 收发走该 fd（tap 的「线」就是持有它的 fd）。
func TestKernelDHCPServerTapIsHeldByProduct(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d")
	f := newKernelDHCPServerFixture(t, nil, vs)
	f.seedBridge(vs)
	ctx := context.Background()
	if err := f.p.ApplyDHCPServer(ctx, vs); err != nil {
		t.Fatalf("收敛失败: %v", err)
	}
	dev := f.taps.get(tapNameOf(vs))
	if dev == nil {
		t.Fatal("内置 tap 的底座应已被打开")
	}
	// ① 持有步骤：打开 /dev/net/tun 并 TUNSETIFF 认领该设备名。
	if paths := dev.openedPaths(); len(paths) != 1 || paths[0] != tunDevicePath {
		t.Fatalf("应打开 %s 持有 tap（carrier 由持有者保证），实际 %v", tunDevicePath, paths)
	}
	if claims := dev.claimedNames(); len(claims) != 1 || claims[0] != tapNameOf(vs) {
		t.Fatalf("应用 TUNSETIFF 认领 %s（＝有持有者），实际 %v", tapNameOf(vs), claims)
	}
	if !dev.held() {
		t.Fatal("tap 必须有持有者（没有持有者 = NO-CARRIER，bridge 不投递）")
	}
	// ② 收发走该 fd：DISCOVER 经 fd 队列进来，OFFER 经同一个 fd 写回（以太源 = 该 tap 的 MAC）。
	client := mustMACT(t, "02:11:22:33:44:55")
	dev.feed(kernelDHCPClientFrame(client, 1, nil, nil, nil))
	fs := waitFrames(t, dev, 1)
	offer := parseKernelReply(t, fs[0])
	if got := replyMsgType(offer.body); got != 2 {
		t.Fatalf("DISCOVER 应回 OFFER，得到 %d", got)
	}
	if offer.ethSrc.String() != dev.ifaceMAC.String() {
		t.Fatalf("应答以太源应是持有中的 tap 的 MAC（%s），得到 %s", dev.ifaceMAC, offer.ethSrc)
	}
	// ③ 停用＝释放持有：fd 关闭（carrier 落下、设备保留复用），不留持有者。
	if err := f.p.ApplyDHCPServer(ctx, model.VirtualSwitch{Name: vs.Name}); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	if !dev.closed() || dev.closeCount() != 1 {
		t.Fatalf("停用应关闭持有的 fd（实际 closed=%v closeCount=%d）", dev.closed(), dev.closeCount())
	}
	if f.host.has(tapNameOf(vs)) {
		t.Fatal("停用应删除内置 tap 设备（无残留）")
	}
}

// 装配守护（任意平台可跑）：内核数据面默认的 tap 底座必须是**持有**实现（/dev/net/tun + TUNSETIFF），
// 不能退回「建了设备就不管」的用法。文案里点名 /dev/net/tun 是这条断言的依据。
func TestKernelDHCPServerDefaultTapLayerHoldsDevice(t *testing.T) {
	_, err := defaultDHCPServerTapLayer().Open("nfvisdh00000000")
	if err == nil {
		t.Skip("本平台能真正打开 tap（真机集成场景）：由真机用例断言 carrier")
	}
	if !strings.Contains(err.Error(), tunDevicePath) {
		t.Fatalf("默认 tap 底座应是持有实现（文案须点名 %s），得到 %v", tunDevicePath, err)
	}
}

// 持有失败如实报错且不泄漏 fd：TUNSETIFF 失败（真机 EBUSY/权限）清掉已打开的 fd、不留下半成品。
func TestKernelDHCPServerTapHolderFailuresAreHonest(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d")
	f := newKernelDHCPServerFixture(t, nil, vs)
	f.seedBridge(vs)
	// 预置底座：认领失败。
	f.taps.mu.Lock()
	mac, _ := net.ParseMAC("02:fe:00:00:00:99")
	f.taps.devs[tapNameOf(vs)] = &fakeTunOps{name: tapNameOf(vs), ifaceMAC: mac, failClaim: true}
	f.taps.mu.Unlock()
	err := f.p.ApplyDHCPServer(context.Background(), vs)
	if err == nil {
		t.Fatal("TUNSETIFF 认领失败时应如实报错")
	}
	if !strings.Contains(err.Error(), "TUNSETIFF") {
		t.Fatalf("错误应点名失败的持有步骤，得到 %v", err)
	}
	dev := f.taps.get(tapNameOf(vs))
	if dev.closeCount() != 1 {
		t.Fatalf("认领失败必须关掉已打开的 fd（不泄漏），实际 closeCount=%d", dev.closeCount())
	}
	// 接口不存在（设备未建）：如实报错并给出「为什么要持有」。
	f2 := newKernelDHCPServerFixture(t, nil, vs)
	f2.seedBridge(vs)
	f2.taps.mu.Lock()
	f2.taps.devs[tapNameOf(vs)] = &fakeTunOps{name: tapNameOf(vs), noSuchDev: true}
	f2.taps.mu.Unlock()
	err = f2.p.ApplyDHCPServer(context.Background(), vs)
	if err == nil || !strings.Contains(err.Error(), tunDevicePath) {
		t.Fatalf("接口读不到时应如实报错并说明持有语义，得到 %v", err)
	}
}

// 单队列语义：同一 tap 已被持有时不得再打开第二个持有者（真机 TUNSETIFF 会 EBUSY）。
func TestKernelDHCPServerTapRejectsSecondHolder(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d")
	f := newKernelDHCPServerFixture(t, nil, vs)
	f.seedBridge(vs)
	if err := f.p.ApplyDHCPServer(context.Background(), vs); err != nil {
		t.Fatalf("收敛失败: %v", err)
	}
	if _, err := f.taps.Open(tapNameOf(vs)); err == nil {
		t.Fatal("同一 tap 已被持有时不应允许第二个持有者")
	}
}

// ---------- 复用核心的报文处理（tap 入径 + 单播 UDP 入径） ----------

func TestKernelDHCPServerDORAAndLeaseFile(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d")
	f := newKernelDHCPServerFixture(t, nil, vs)
	f.seedBridge(vs)
	ctx := context.Background()
	if err := f.p.ApplyDHCPServer(ctx, vs); err != nil {
		t.Fatalf("收敛失败: %v", err)
	}
	tap := f.taps.get(tapNameOf(vs))
	if tap == nil {
		t.Fatal("内置 tap 的以太帧收发应已打开")
	}
	client := mustMACT(t, "02:11:22:33:44:55")
	bvi := net.ParseIP("192.168.99.1").To4()

	// DISCOVER（tap 广播入径）→ OFFER。
	tap.feed(kernelDHCPClientFrame(client, 1, nil, nil, nil))
	fs := waitFrames(t, tap, 1)
	offer := parseKernelReply(t, fs[0])
	if got := replyMsgType(offer.body); got != 2 {
		t.Fatalf("DISCOVER 应回 OFFER（53=2），得到 %d", got)
	}
	if offer.ethDst.String() != client.String() {
		t.Fatalf("应答以太目的应是客户端 MAC，得到 %s", offer.ethDst)
	}
	if !offer.ipSrc.Equal(bvi) {
		t.Fatalf("应答 IP 源应是 BVI 地址，得到 %s", offer.ipSrc)
	}
	if got := net.IP(offer.body[16:20]); !got.Equal(net.ParseIP("192.168.99.10")) {
		t.Fatalf("OFFER 应给池内第一个地址，得到 %s", got)
	}
	if got := net.IP(kernelFindOption(offer.body, 54)); !got.Equal(bvi) {
		t.Fatalf("option 54（server-id）应是 BVI，得到 %s", got)
	}
	leases, ok := f.p.DHCPServerLeases(vs.Name)
	if !ok || len(leases) != 1 || leases[0].State != "offered" {
		t.Fatalf("offer 后应有 1 条 offered 租约，得到 %+v ok=%v", leases, ok)
	}

	// REQUEST（SELECTING，带 option 50/54）→ ACK。
	tap.feed(kernelDHCPClientFrame(client, 3, net.ParseIP("192.168.99.10"), bvi, nil))
	fs = waitFrames(t, tap, 2)
	ack := parseKernelReply(t, fs[1])
	if got := replyMsgType(ack.body); got != 5 {
		t.Fatalf("REQUEST 应回 ACK（53=5），得到 %d", got)
	}
	if got := int(binary.BigEndian.Uint32(kernelFindOption(ack.body, 51))); got != 7200 {
		t.Fatalf("option 51 应是配置的租约时长 7200，得到 %d", got)
	}
	if n, ok := f.p.DHCPServerActiveLeases(vs.Name); !ok || n != 1 {
		t.Fatalf("ACK 后应有 1 条生效租约，得到 %d ok=%v", n, ok)
	}
	// 租约文件持久化（独立目录、原子写）。
	b, err := os.ReadFile(filepath.Join(f.p.dhcpLeaseDir, vs.Name+".json"))
	if err != nil {
		t.Fatalf("租约文件应已持久化: %v", err)
	}
	if !strings.Contains(string(b), "192.168.99.10") || !strings.Contains(string(b), client.String()) {
		t.Fatalf("租约文件内容不符: %s", b)
	}
}

func TestKernelDHCPServerUnicastRenewalOverUDP(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d")
	f := newKernelDHCPServerFixture(t, nil, vs)
	f.seedBridge(vs)
	ctx := context.Background()
	if err := f.p.ApplyDHCPServer(ctx, vs); err != nil {
		t.Fatalf("收敛失败: %v", err)
	}
	tap := f.taps.get(tapNameOf(vs))
	client := mustMACT(t, "02:11:22:33:44:55")
	bvi := net.ParseIP("192.168.99.1").To4()
	leased := net.ParseIP("192.168.99.10").To4()

	// 先经 tap 完成一次 DORA（拿到地址）。
	tap.feed(kernelDHCPClientFrame(client, 1, nil, nil, nil))
	waitFrames(t, tap, 1)
	tap.feed(kernelDHCPClientFrame(client, 3, leased, bvi, nil))
	waitFrames(t, tap, 2)

	// **单播续租**：经 UDP/67 socket（UDP 载荷，无以太/IP 头）——REQUEST 带 ciaddr、无 option 54。
	sock := f.uni.socket("192.168.99.1", GatewayVRFName(vs.Name))
	if sock == nil {
		t.Fatal("单播接收 socket 应已打开")
	}
	sock.feed(kernelDHCPBody(client, 3, nil, nil, leased), leased)
	fs := waitFrames(t, tap, 3)
	renew := parseKernelReply(t, fs[2])
	if got := replyMsgType(renew.body); got != 5 {
		t.Fatalf("单播续租应回 ACK（53=5），得到 %d", got)
	}
	if renew.ethDst.String() != client.String() || !renew.ipSrc.Equal(bvi) {
		t.Fatalf("续租应答字段不符：dst=%s src=%s", renew.ethDst, renew.ipSrc)
	}
	// 续租必须**投递到对应交换机**（描述符按 bridge ifindex 派发）：租约仍在原池内。
	leases, _ := f.p.DHCPServerLeases(vs.Name)
	if len(leases) != 1 || leases[0].State != "active" || leases[0].IP != "192.168.99.10" {
		t.Fatalf("续租后租约应仍是 active 且地址不变，得到 %+v", leases)
	}
	// 不是本域的包（描述符索引对不上任何交换机）应被静默丢弃——构造第二个交换机前先验证：
	// 直接把载荷投到一个未登记的 socket 上不存在，故这里以「未知索引」单测 switchOf。
	if _, ok := f.p.dhcpHub.switchOf(424242); ok {
		t.Fatal("未知 bridge ifindex 不应派发到任何交换机")
	}
}

func TestKernelDHCPServerPoolExhaustedAlarm(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d") // 3 地址池（.10-.12）
	f := newKernelDHCPServerFixture(t, nil, vs)
	f.seedBridge(vs)
	ctx := context.Background()
	if err := f.p.ApplyDHCPServer(ctx, vs); err != nil {
		t.Fatalf("收敛失败: %v", err)
	}
	tap := f.taps.get(tapNameOf(vs))
	bvi := net.ParseIP("192.168.99.1").To4()

	// 占满池：三个客户端各完成一次 DORA（池满＝无可用地址可应答新 DISCOVER ⇒ 告警即时在场，
	// 与 VPP 侧 refreshPoolAlarmLocked 的口径一致）。
	done := 0
	for i := 1; i <= 3; i++ {
		if i < 3 && poolAlarmActive(f.alarms, vs.Name) {
			t.Fatal("池未耗尽时不应有池耗尽告警")
		}
		mac := mustMACT(t, fmt.Sprintf("02:11:22:33:44:%02d", i))
		leased := net.ParseIP(fmt.Sprintf("192.168.99.%d", 9+i))
		tap.feed(kernelDHCPClientFrame(mac, 1, nil, nil, nil))
		waitFrames(t, tap, done+1)
		done++
		tap.feed(kernelDHCPClientFrame(mac, 3, leased, bvi, nil))
		waitFrames(t, tap, done+1)
		done++
	}
	if n, _ := f.p.DHCPServerActiveLeases(vs.Name); n != 3 {
		t.Fatalf("应租出 3 个地址，得到 %d", n)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !poolAlarmActive(f.alarms, vs.Name) {
		time.Sleep(5 * time.Millisecond)
	}
	if !poolAlarmActive(f.alarms, vs.Name) {
		t.Fatal("池满应建 DHCP_POOL_EXHAUSTED 告警（source=交换机名）")
	}
	// 第 4 个客户端：不发 OFFER（告警维持）。
	before := len(tap.frames())
	tap.feed(kernelDHCPClientFrame(mustMACT(t, "02:11:22:33:44:99"), 1, nil, nil, nil))
	time.Sleep(100 * time.Millisecond)
	if got := len(tap.frames()); got != before {
		t.Fatalf("池耗尽时不应发 OFFER（帧数 %d → %d）", before, got)
	}
	// 释放一个 ⇒ 地址回池、告警自动消解。
	tap.feed(kernelDHCPClientFrame(mustMACT(t, "02:11:22:33:44:01"), 7, nil, nil,
		net.ParseIP("192.168.99.10").To4()))
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && poolAlarmActive(f.alarms, vs.Name) {
		time.Sleep(5 * time.Millisecond)
	}
	if poolAlarmActive(f.alarms, vs.Name) {
		t.Fatal("地址释放后池耗尽告警应自动消解")
	}
}

// ---------- 生命周期 ----------

func TestKernelDHCPServerSyncIdempotentAndRecreatesLostTap(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d")
	f := newKernelDHCPServerFixture(t, nil, vs)
	f.seedBridge(vs)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := f.p.ApplyDHCPServer(ctx, vs); err != nil {
			t.Fatalf("第 %d 次收敛失败: %v", i+1, err)
		}
	}
	if n := f.host.countCmd("ip tuntap add"); n != 1 {
		t.Fatalf("重复收敛应幂等（只建一次 tap），实际 %d 次：\n%s", n, strings.Join(f.host.lastCmds(), "\n"))
	}
	if n := f.uni.openCount(); n != 1 {
		t.Fatalf("重复收敛应复用单播 socket（只绑一次），实际 %d 次：%v", n, f.uni.opens)
	}
	tap := f.taps.get(tapNameOf(vs))
	if tap == nil || tap.closed() {
		t.Fatal("重复收敛不应重开内核侧 tap 收发")
	}
	if n := f.taps.openCount(); n != 1 {
		t.Fatalf("重复收敛不应重开 AF_PACKET，实际打开 %d 次", n)
	}

	// 带外删掉内置 tap（模拟「tap 丢了」）：巡检应重建 tap 并重开收发（索引即使被复用也不误判）。
	f.host.mu.Lock()
	delete(f.host.links, tapNameOf(vs))
	f.host.mu.Unlock()
	if errs := f.p.ReconcileDHCPServer(ctx, f.cfg); len(errs) != 0 {
		t.Fatalf("巡检应自愈被带外删除的 tap，得到 %v", errs)
	}
	if !f.host.has(tapNameOf(vs)) {
		t.Fatal("巡检应重建内置 tap")
	}
	if n := f.host.countCmd("ip tuntap add"); n != 2 {
		t.Fatalf("重建应再执行一次 ip tuntap add，实际 %d 次", n)
	}
	if n := f.taps.openCount(); n != 2 {
		t.Fatalf("tap 重建后应重开 AF_PACKET（旧 fd 绑的是已消失的设备），实际打开 %d 次", n)
	}
	if f.uni.openCount() != 1 {
		t.Fatal("网桥/地址未变时不应重绑单播 socket（socket 与 tap 生命期解耦）")
	}
}

func TestKernelDHCPServerTeardownRemovesTapSocketAndLeaseFile(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d")
	f := newKernelDHCPServerFixture(t, nil, vs)
	f.seedBridge(vs)
	ctx := context.Background()
	if err := f.p.ApplyDHCPServer(ctx, vs); err != nil {
		t.Fatalf("收敛失败: %v", err)
	}
	tap := f.taps.get(tapNameOf(vs))
	sock := f.uni.socket("192.168.99.1", GatewayVRFName(vs.Name))
	// 造一条租约（同时验证租约文件被清）。
	client := mustMACT(t, "02:11:22:33:44:55")
	tap.feed(kernelDHCPClientFrame(client, 1, nil, nil, nil))
	waitFrames(t, tap, 1)
	leaseDir := f.p.dhcpLeaseDir
	leaseFile := filepath.Join(leaseDir, vs.Name+".json")
	if _, err := os.Stat(leaseFile); err != nil {
		t.Fatalf("租约文件应已落盘: %v", err)
	}

	// 停用（声明里无池）＝teardown：删内置 tap + 关单播 socket + 清租约文件。
	if err := f.p.ApplyDHCPServer(ctx, model.VirtualSwitch{Name: vs.Name}); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	if f.host.has(tapNameOf(vs)) {
		t.Fatal("停用应删除内置 tap（无残留）")
	}
	if !sock.closed() {
		t.Fatal("停用应关闭单播接收 socket（无残留）")
	}
	if !tap.closed() {
		t.Fatal("停用应关闭内核侧 tap 的收发协程")
	}
	if _, err := os.Stat(leaseFile); !os.IsNotExist(err) {
		t.Fatalf("停用应清租约文件（got err=%v）", err)
	}
	if _, ok := f.p.DHCPServerLeases(vs.Name); ok {
		t.Fatal("停用后租约读视图应如实报不可用（未收敛）")
	}
	// 幂等：再停一次不报错。
	if err := f.p.ApplyDHCPServer(ctx, model.VirtualSwitch{Name: vs.Name}); err != nil {
		t.Fatalf("重复停用应幂等，得到 %v", err)
	}
}

func TestKernelDHCPServerDeleteBridgeStopsServer(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d")
	f := newKernelDHCPServerFixture(t, nil, vs)
	f.seedBridge(vs)
	ctx := context.Background()
	if err := f.p.ApplyDHCPServer(ctx, vs); err != nil {
		t.Fatalf("收敛失败: %v", err)
	}
	sock := f.uni.socket("192.168.99.1", GatewayVRFName(vs.Name))
	// 交换机删除：先解引用（停服务器）→ 再删 bridge。
	if err := f.p.DeleteBridgeDomain(ctx, vs.Name); err != nil {
		t.Fatalf("删除交换机失败: %v", err)
	}
	if f.host.has(tapNameOf(vs)) {
		t.Fatal("交换机删除应一并删掉内置 tap")
	}
	if !sock.closed() {
		t.Fatal("交换机删除应关闭单播接收 socket")
	}
	if f.host.has(LinkName(vs.Name)) {
		t.Fatal("bridge 应已删除")
	}
}

func TestKernelDHCPServerReconcileStopsUndeclared(t *testing.T) {
	vsA := vsKernelDHCPServer("vs-a")
	vsB := vsKernelDHCPServer("vs-b")
	vsB.Gateway = &model.VSGateway{Addresses: []string{"192.168.100.1/24"}}
	vsB.DhcpServerPoolStart = "192.168.100.10"
	vsB.DhcpServerPoolEnd = "192.168.100.12"
	f := newKernelDHCPServerFixture(t, nil, vsA, vsB)
	f.seedBridge(vsA)
	f.seedBridge(vsB)
	ctx := context.Background()
	for _, vs := range []model.VirtualSwitch{vsA, vsB} {
		if err := f.p.ApplyDHCPServer(ctx, vs); err != nil {
			t.Fatalf("收敛 %s 失败: %v", vs.Name, err)
		}
	}
	sockA := f.uni.socket("192.168.99.1", GatewayVRFName(vsA.Name))
	sockB := f.uni.socket("192.168.100.1", GatewayVRFName(vsB.Name))
	if sockA == nil || sockB == nil {
		t.Fatal("两台交换机都应有单播 socket")
	}
	// 声明里只剩 A：B 的 socket 应被停掉（巡检兜底），A 保持。
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{vsA}}
	if errs := f.p.ReconcileDHCPServer(ctx, cfg); len(errs) != 0 {
		t.Fatalf("巡检对账应无错误，得到 %v", errs)
	}
	if !sockB.closed() {
		t.Fatal("已不声明的交换机应停掉单播 socket")
	}
	if sockA.closed() {
		t.Fatal("仍声明的交换机不应受影响")
	}
}

// ---------- 起不来如实报错 + 恢复重放 ----------

func TestKernelDHCPServerBridgeMissingIsHonestError(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d")
	f := newKernelDHCPServerFixture(t, nil, vs) // **不**预置 bridge
	err := f.p.ApplyDHCPServer(context.Background(), vs)
	if err == nil {
		t.Fatal("bridge 不存在时应如实报错（不静默）")
	}
	if !strings.Contains(err.Error(), LinkName(vs.Name)) {
		t.Fatalf("错误应点名缺失的 bridge，得到 %v", err)
	}
	if f.host.has(tapNameOf(vs)) {
		t.Fatal("收敛失败时不应留下内置 tap")
	}
	if _, ok := f.p.DHCPServerLeases(vs.Name); ok {
		t.Fatal("收敛失败时租约读视图应如实报不可用")
	}
}

func TestKernelDHCPServerTapCreateFailureIsHonest(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d")
	f := newKernelDHCPServerFixture(t, nil, vs)
	f.seedBridge(vs)
	f.host.mu.Lock()
	f.host.failTuntap = true
	f.host.mu.Unlock()
	err := f.p.ApplyDHCPServer(context.Background(), vs)
	if err == nil {
		t.Fatal("tap 建不出时应如实报错")
	}
	if !strings.Contains(err.Error(), "ip tuntap add") {
		t.Fatalf("错误应点名失败的命令，得到 %v", err)
	}
}

func TestKernelDHCPServerWithoutGatewayIsHonest(t *testing.T) {
	vs := model.VirtualSwitch{Name: "vs-d", Type: "l2",
		DhcpServerPoolStart: "192.168.99.10", DhcpServerPoolEnd: "192.168.99.12"}
	f := newKernelDHCPServerFixture(t, nil, vs)
	err := f.p.ApplyDHCPServer(context.Background(), vs)
	if err == nil {
		t.Fatal("没有 IPv4 网关时应如实报错（server-id/单播接收地址都取 BVI）")
	}
	if strings.Contains(err.Error(), "不受支持") || strings.Contains(err.Error(), "尚未实现") {
		t.Fatalf("内核侧已支持 DHCP 服务器，错误应是配置不完整而非「未实现」：%v", err)
	}
}

func TestKernelDHCPServerRecoveryReplay(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d")
	host := newDHCPFakeHost()
	host.addLink(GatewayVRFName(vs.Name), "vrf")
	host.addLink(LinkName(vs.Name), "bridge", "192.168.99.1/24")
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{vs}}
	ctx := context.Background()

	// 第一次恢复收敛（模拟首启/进程重启）：建 tap + 绑 socket。
	f1 := newKernelDHCPServerFixture(t, host, vs)
	f1.cfg = cfg
	f1.p.SetConfig(cfg)
	if errs := f1.p.EnsureConsistent(ctx, cfg); len(errs) != 0 {
		t.Fatalf("恢复收敛应无未收敛项，得到 %v", errs)
	}
	if !host.has(tapNameOf(vs)) {
		t.Fatal("恢复重放应重建内置 tap")
	}
	if f1.uni.socket("192.168.99.1", GatewayVRFName(vs.Name)) == nil {
		t.Fatal("恢复重放应重建单播接收 socket")
	}
	// 进程退出（socket/收发协程不残留；tap 保留给下次启动复用）。
	if err := f1.p.Close(); err != nil {
		t.Fatalf("退出失败: %v", err)
	}
	if !f1.uni.socket("192.168.99.1", GatewayVRFName(vs.Name)).closed() {
		t.Fatal("退出应关闭单播 socket")
	}
	if !host.has(tapNameOf(vs)) {
		t.Fatal("退出不应删除内置 tap（内核对象，下次启动按名复用）")
	}

	// 第二次恢复收敛（重装/重启后）：按名复用存量 tap（不再 add），socket 重建，报文处理可用。
	f2 := newKernelDHCPServerFixture(t, host, vs)
	f2.cfg = cfg
	f2.p.SetConfig(cfg)
	if errs := f2.p.EnsureConsistent(ctx, cfg); len(errs) != 0 {
		t.Fatalf("第二次恢复收敛应无未收敛项，得到 %v", errs)
	}
	if n := host.countCmd("ip tuntap add"); n != 1 {
		t.Fatalf("重启后应按名复用存量 tap（全程只建一次），实际 %d 次 add", n)
	}
	tap := f2.taps.get(tapNameOf(vs))
	if tap == nil {
		t.Fatal("重启后应重开内核侧 tap 收发")
	}
	tap.feed(kernelDHCPClientFrame(mustMACT(t, "02:11:22:33:44:55"), 1, nil, nil, nil))
	fs := waitFrames(t, tap, 1)
	if got := replyMsgType(parseKernelReply(t, fs[0]).body); got != 2 {
		t.Fatalf("重启后 DISCOVER 应照常回 OFFER，得到 %d", got)
	}
}

func TestKernelDHCPServerRecoveryFailureEntersUnconverged(t *testing.T) {
	vs := vsKernelDHCPServer("vs-d")
	host := newDHCPFakeHost() // 不预置 bridge ⇒ 收敛必然失败
	cfg := model.Config{VirtualSwitches: []model.VirtualSwitch{vs}}
	f := newKernelDHCPServerFixture(t, host, vs)
	f.cfg = cfg
	f.p.SetConfig(cfg)

	errs := f.p.EnsureConsistent(context.Background(), cfg)
	if len(errs) == 0 {
		t.Fatal("恢复收敛应如实记未收敛项")
	}
	path := "virtual-switches/" + vs.Name + "/dhcp-server"
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), path) {
			found = true
		}
	}
	if !found {
		t.Fatalf("未收敛项应带对象路径 %s，得到 %v", path, errs)
	}
	// 进告警（事后可查：`show alarms`/Web/诊断包）。
	raised := false
	for _, a := range f.alarms.List("active") {
		if a.Source == path {
			raised = true
		}
	}
	if !raised {
		t.Fatalf("未收敛项应落告警（source=%s）", path)
	}
}

// 单播收包合成包：IPv4/UDP/67 形状正确（与 VPP punt 上行同一解析入口的前提）。
func TestKernelDHCPUnicastIPSynthesis(t *testing.T) {
	payload := kernelDHCPBody(mustMACT(t, "02:11:22:33:44:55"), 3, nil, nil, net.ParseIP("192.168.99.10"))
	pkt := buildDHCPUnicastIP(net.ParseIP("192.168.99.1"), &net.UDPAddr{IP: net.ParseIP("192.168.99.10"), Port: 68}, payload)
	if pkt[0]>>4 != 4 || pkt[9] != 17 {
		t.Fatalf("应是 IPv4/UDP：ver=%d proto=%d", pkt[0]>>4, pkt[9])
	}
	if got := int(binary.BigEndian.Uint16(pkt[2:4])); got != len(pkt) {
		t.Fatalf("IP 总长应为 %d，得到 %d", len(pkt), got)
	}
	if got := net.IP(pkt[12:16]); !got.Equal(net.ParseIP("192.168.99.10")) {
		t.Fatalf("源应是客户端地址，得到 %s", got)
	}
	if got := net.IP(pkt[16:20]); !got.Equal(net.ParseIP("192.168.99.1")) {
		t.Fatalf("目的应是 BVI 地址，得到 %s", got)
	}
	udp := pkt[20:]
	if binary.BigEndian.Uint16(udp[2:4]) != 67 {
		t.Fatalf("UDP 目的端口应是 67")
	}
	if binary.BigEndian.Uint16(udp[4:6]) != uint16(8+len(payload)) {
		t.Fatalf("UDP 长度不符")
	}
	if string(udp[8:]) != string(payload) {
		t.Fatal("载荷必须字节保真")
	}
	if relayIPChecksum(pkt[:20]) != 0 {
		t.Fatal("IP 头校验和应正确（反码求和为 0）")
	}
}
