package netkernel

// 内核数据面 DNS 代理（v3 决策 #439）单测：**不依赖真 socket / 真上游**——域落点 socket 底座与
// 上游查询都注入内存实现，校验：
//  ① 未启用不开任何落点、停用全关；
//  ② 落点按声明开在正确的域设备上，地址增/删、域删、域设备变更各自关旧开新（幂等不抖动）；
//  ③ 上游选择（按域优先、回落全局、皆空回 SERVFAIL），SERVFAIL 载荷断言（QR/RCODE/question 回显）；
//  ④ 上游失败回 SERVFAIL、成功应答原样写回客户端地址、回包失败计数；
//  ⑤ 起不来如实报错（点名域）且已开的落点不回滚，底座恢复后同一声明幂等收敛；
//  ⑥ State 快照按声明序报地址/上游/VRF 设备/未收敛原因与计数；
//  ⑧ 生命周期 Close 关 socket 并等收包协程退出（幂等）。
// 另：ApplyDNSProxy 空声明在从未装配过的 provider 上是空操作（不因巡检常驻构造管理器）。

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/orchestrator"
)

// ---------- 内存域落点底座（UDP/53 绑具体地址 + 域 VRF 设备） ----------

type dnsProxyFakeDatagram struct {
	payload []byte
	from    *net.UDPAddr
}

type dnsProxyFakeSend struct {
	payload []byte
	to      *net.UDPAddr
}

// dnsProxyFakeIO 一个域落点 socket（内存）。
type dnsProxyFakeIO struct {
	addr net.IP
	vrf  string
	in   chan dnsProxyFakeDatagram
	done chan struct{}
	once sync.Once

	mu      sync.Mutex
	sends   []dnsProxyFakeSend
	closes  int
	sendErr error

	recvClosed atomic.Int32 // Recv 因 Close 返回 net.ErrClosed 的次数（收包协程确实退出的证据）
}

func (s *dnsProxyFakeIO) Recv(buf []byte) (int, *net.UDPAddr, error) {
	select {
	case <-s.done:
		s.recvClosed.Add(1)
		return 0, nil, net.ErrClosed
	case d := <-s.in:
		n := copy(buf, d.payload)
		return n, d.from, nil
	}
}

func (s *dnsProxyFakeIO) Send(b []byte, to *net.UDPAddr) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	s.sends = append(s.sends, dnsProxyFakeSend{payload: append([]byte{}, b...), to: to})
	return nil
}

func (s *dnsProxyFakeIO) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closes++
		s.mu.Unlock()
		close(s.done)
	})
	return nil
}

func (s *dnsProxyFakeIO) closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *dnsProxyFakeIO) sent() []dnsProxyFakeSend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]dnsProxyFakeSend(nil), s.sends...)
}

func (s *dnsProxyFakeIO) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closes
}

func (s *dnsProxyFakeIO) setSendErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendErr = err
}

func (s *dnsProxyFakeIO) feed(payload []byte, from *net.UDPAddr) {
	s.in <- dnsProxyFakeDatagram{payload: payload, from: from}
}

// dnsProxyFakeLayer 域落点底座假实现：按「地址|VRF 设备」键登记 socket（同键重复绑定如实报
// EADDRINUSE，模拟真机）；可注入某地址的打开失败。
type dnsProxyFakeLayer struct {
	mu     sync.Mutex
	opened map[string]*dnsProxyFakeIO
	opens  []string
	fail   map[string]error
}

func newDNSProxyFakeLayer() *dnsProxyFakeLayer {
	return &dnsProxyFakeLayer{opened: map[string]*dnsProxyFakeIO{}, fail: map[string]error{}}
}

func (l *dnsProxyFakeLayer) Open(addr net.IP, vrfDevice string) (dnsProxyIO, error) {
	v4 := net.ParseIP(addr.String()).To4()
	if addr == nil || v4 == nil {
		return nil, fmt.Errorf("落点 %v 不是 IPv4", addr)
	}
	if vrfDevice == "" {
		return nil, fmt.Errorf("VRF 设备名为空")
	}
	key := v4.String() + "|" + vrfDevice
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.fail[v4.String()]; err != nil {
		return nil, err
	}
	if cur := l.opened[key]; cur != nil && !cur.closed() {
		return nil, fmt.Errorf("bind: address already in use（%s:%d 已绑定）", v4, dnsProxyPort)
	}
	s := &dnsProxyFakeIO{addr: v4, vrf: vrfDevice, in: make(chan dnsProxyFakeDatagram, 8), done: make(chan struct{})}
	l.opened[key] = s
	l.opens = append(l.opens, key)
	return s, nil
}

func (l *dnsProxyFakeLayer) socket(addr, vrf string) *dnsProxyFakeIO {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.opened[addr+"|"+vrf]
}

func (l *dnsProxyFakeLayer) openKeys() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.opens...)
}

func (l *dnsProxyFakeLayer) setFail(addr string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil {
		delete(l.fail, addr)
		return
	}
	l.fail[addr] = err
}

// ---------- 装配与断言助手 ----------

type dnsProxyFixture struct {
	m   *dnsProxyManager
	lay *dnsProxyFakeLayer
}

func newDNSProxyFixture(t *testing.T) *dnsProxyFixture {
	t.Helper()
	f := &dnsProxyFixture{lay: newDNSProxyFakeLayer()}
	f.m = newDNSProxyManager(f.lay)
	return f
}

// client 一个域内客户端地址（查询来源）。
func dnsProxyClient(ip string, port int) *net.UDPAddr {
	return &net.UDPAddr{IP: net.ParseIP(ip).To4(), Port: port}
}

// dnsProxyQuery 构造一条最小 DNS 查询（头 + 单个 question），用于断言 SERVFAIL 的 question 回显。
func dnsProxyQuery(id uint16, name string) []byte {
	msg := make([]byte, 12)
	binary.BigEndian.PutUint16(msg[0:2], id)
	msg[2] = 0x01 // RD
	binary.BigEndian.PutUint16(msg[4:6], 1)
	for _, label := range strings.Split(name, ".") {
		msg = append(msg, byte(len(label)))
		msg = append(msg, label...)
	}
	msg = append(msg, 0)    // 根
	msg = append(msg, 0, 1) // QTYPE A
	msg = append(msg, 0, 1) // QCLASS IN
	return msg
}

// waitDNSProxySend 等某个落点收到第 n 个应答（有界轮询；超时即失败）。
func waitDNSProxySend(t *testing.T, s *dnsProxyFakeIO, n int) []dnsProxyFakeSend {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.sent(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等落点 %s 的第 %d 个应答超时（现有 %d 个）", s.addr, n, len(s.sent()))
	return nil
}

// assertDNSProxyServfail 断言应答是「回显 question 的 SERVFAIL」：ID 原样、QR=1、RCODE=2、
// question 段逐字节回显（共享实现 network.DNSServfail 的契约）。
func assertDNSProxyServfail(t *testing.T, query, resp []byte) {
	t.Helper()
	if len(resp) < 12 {
		t.Fatalf("SERVFAIL 应答不应短于 12 字节头：%d", len(resp))
	}
	if got, want := binary.BigEndian.Uint16(resp[0:2]), binary.BigEndian.Uint16(query[0:2]); got != want {
		t.Fatalf("事务 ID 应原样回显：got %#x want %#x", got, want)
	}
	flags := binary.BigEndian.Uint16(resp[2:4])
	if flags&0x8000 == 0 {
		t.Fatalf("QR 位应为 1（应答）：flags=%#x", flags)
	}
	if rcode := flags & 0x000f; rcode != 2 {
		t.Fatalf("RCODE 应为 SERVFAIL(2)：flags=%#x rcode=%d", flags, rcode)
	}
	if len(resp) != len(query) || !bytes.Equal(resp[12:], query[12:]) {
		t.Fatalf("question 段应逐字节回显：query=%x resp=%x", query, resp)
	}
}

// ---------- ① 启用/停用生命周期 ----------

// 未启用（全局与按域皆空）⇒ 一个落点也不开——**哪怕声明里带了落点地址**（判据是上游，
// 「配了交换机网关」不等于启用了 DNS 代理）。
func TestDNSProxyDisabledOpensNothing(t *testing.T) {
	f := newDNSProxyFixture(t)
	ctx := context.Background()
	want := orchestrator.DNSProxyUpstreams{
		Domains: []orchestrator.DNSProxyDomain{
			{Name: "vs-a", Addresses: []string{"192.168.99.1"}, VRFDevice: "vr-vs-a"},
		},
	}
	if err := f.m.Sync(ctx, want); err != nil {
		t.Fatalf("未启用应收敛为空操作：%v", err)
	}
	if got := f.lay.openKeys(); len(got) != 0 {
		t.Fatalf("未启用不该开任何落点：%v", got)
	}
	if st := f.m.State(); len(st.Domains) != 0 || st.Answered != 0 || st.Servfail != 0 || st.SendFail != 0 {
		t.Fatalf("未启用时运行态应为空：%+v", st)
	}
}

// 启用 → 停用：全部落点关闭、收包协程退出、状态清空；重复停用幂等。
func TestDNSProxyEnabledThenDisabledClosesAll(t *testing.T) {
	f := newDNSProxyFixture(t)
	ctx := context.Background()
	want := orchestrator.DNSProxyUpstreams{
		Global: []string{"8.8.8.8"},
		Domains: []orchestrator.DNSProxyDomain{
			{Name: "vs-a", Addresses: []string{"192.168.99.1", "10.0.0.1"}, VRFDevice: "vr-vs-a"},
		},
	}
	if err := f.m.Sync(ctx, want); err != nil {
		t.Fatalf("启用应收敛成功：%v", err)
	}
	s1 := f.lay.socket("192.168.99.1", "vr-vs-a")
	s2 := f.lay.socket("10.0.0.1", "vr-vs-a")
	if s1 == nil || s2 == nil {
		t.Fatalf("启用后两个落点都应在绑定状态：%v", f.lay.openKeys())
	}
	if err := f.m.Sync(ctx, orchestrator.DNSProxyUpstreams{}); err != nil {
		t.Fatalf("停用应收敛成功：%v", err)
	}
	if !s1.closed() || !s2.closed() {
		t.Fatal("停用后全部落点 socket 应关闭")
	}
	if s1.recvClosed.Load() == 0 || s2.recvClosed.Load() == 0 {
		t.Fatal("停用应等收包协程退出（Recv 以 net.ErrClosed 返回）")
	}
	if st := f.m.State(); len(st.Domains) != 0 {
		t.Fatalf("停用后不该再有域落点：%+v", st)
	}
	if err := f.m.Sync(ctx, orchestrator.DNSProxyUpstreams{}); err != nil {
		t.Fatalf("重复停用应幂等：%v", err)
	}
}

// ---------- ② 地址/域增删与域设备变更 ----------

func TestDNSProxySyncOpensRebindsAndCloses(t *testing.T) {
	f := newDNSProxyFixture(t)
	ctx := context.Background()
	two := func() orchestrator.DNSProxyUpstreams {
		return orchestrator.DNSProxyUpstreams{
			Global: []string{"8.8.8.8"},
			Domains: []orchestrator.DNSProxyDomain{
				{Name: "vs-a", Addresses: []string{"192.168.99.1", "10.0.0.1"}, VRFDevice: "vr-vs-a"},
				{Name: "vs-b", Addresses: []string{"192.168.100.1"}, VRFDevice: "vr-vs-b"},
			},
		}
	}
	if err := f.m.Sync(ctx, two()); err != nil {
		t.Fatalf("收敛失败：%v", err)
	}
	wantKeys := []string{"192.168.99.1|vr-vs-a", "10.0.0.1|vr-vs-a", "192.168.100.1|vr-vs-b"}
	if got := f.lay.openKeys(); !reflect.DeepEqual(got, wantKeys) {
		t.Fatalf("落点应按声明序开在各自的域设备上：got %v want %v", got, wantKeys)
	}
	s1 := f.lay.socket("192.168.99.1", "vr-vs-a")
	s2 := f.lay.socket("10.0.0.1", "vr-vs-a")
	sb := f.lay.socket("192.168.100.1", "vr-vs-b")

	// 幂等：同一份声明再收敛一次不重开、不关闭（socket 的打开次数与关闭次数都不变）。
	if err := f.m.Sync(ctx, two()); err != nil {
		t.Fatalf("幂等收敛失败：%v", err)
	}
	if got := len(f.lay.openKeys()); got != len(wantKeys) {
		t.Fatalf("声明未变不该重开落点：%v", f.lay.openKeys())
	}
	if s1.closed() || s2.closed() || sb.closed() {
		t.Fatal("声明未变不该关闭已绑定的落点")
	}

	// 明确断言：落点开在该域的 VRF 设备上（不是别的域/空设备）。
	if s1.vrf != "vr-vs-a" || sb.vrf != "vr-vs-b" {
		t.Fatalf("落点应绑在各自域的 VRF 设备上：%q / %q", s1.vrf, sb.vrf)
	}

	// 去掉一个地址 ⇒ 它关闭，同域另一个保留。
	one := two()
	one.Domains[0].Addresses = []string{"192.168.99.1"}
	if err := f.m.Sync(ctx, one); err != nil {
		t.Fatalf("收敛失败：%v", err)
	}
	if !s2.closed() {
		t.Fatal("不再声明的地址应关闭")
	}
	if s1.closed() {
		t.Fatal("仍声明的地址不该被关")
	}

	// 域设备变更 ⇒ 旧 socket 关闭、按新设备重开。
	reb := two()
	reb.Domains[0].VRFDevice = "vr-other"
	reb.Domains[1].Addresses = nil
	if err := f.m.Sync(ctx, reb); err != nil {
		t.Fatalf("收敛失败：%v", err)
	}
	if !s1.closed() {
		t.Fatal("域设备变更后旧 socket 应关闭（它绑在旧 VRF 上）")
	}
	ns := f.lay.socket("192.168.99.1", "vr-other")
	if ns == nil || ns.closed() {
		t.Fatal("域设备变更后应按新设备重开落点")
	}

	// 域删除 ⇒ 该域全部落点关闭（这里 vs-b 的地址已被上一步清空，先补回来再删域）。
	withB := two()
	withB.Domains[0].VRFDevice = "vr-other"
	if err := f.m.Sync(ctx, withB); err != nil {
		t.Fatalf("收敛失败：%v", err)
	}
	sb2 := f.lay.socket("192.168.100.1", "vr-vs-b")
	if sb2 == nil || sb2.closed() {
		t.Fatal("前提不成立：vs-b 的落点应已重开")
	}
	onlyA := two()
	onlyA.Domains = onlyA.Domains[:1]
	if err := f.m.Sync(ctx, onlyA); err != nil {
		t.Fatalf("收敛失败：%v", err)
	}
	if !sb2.closed() {
		t.Fatal("已删除的域应关闭其全部落点")
	}
	if st := f.m.State(); len(st.Domains) != 1 || st.Domains[0].Name != "vs-a" {
		t.Fatalf("删除域后运行态应只剩 vs-a：%+v", st)
	}
}

// ---------- ③ 上游选择与 SERVFAIL ----------

// 按域上游优先于全局；无按域上游的域回落全局；上游失败 ⇒ SERVFAIL 并计数。
func TestDNSProxyUpstreamSelectionPerDomainOverGlobal(t *testing.T) {
	f := newDNSProxyFixture(t)
	ctx := context.Background()
	var mu sync.Mutex
	var asked []string
	f.m.SetUpstream(func(server string, _ []byte) ([]byte, error) {
		mu.Lock()
		asked = append(asked, server)
		mu.Unlock()
		if server == "1.1.1.1" {
			return []byte{0xBE, 0xEF}, nil
		}
		return nil, fmt.Errorf("上游 %s 不可达", server)
	})
	askedServers := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), asked...)
	}
	want := orchestrator.DNSProxyUpstreams{
		Global:    []string{"9.9.9.9"},
		PerSwitch: map[string][]string{"vs-a": {"1.1.1.1"}},
		Domains: []orchestrator.DNSProxyDomain{
			{Name: "vs-a", Addresses: []string{"192.168.99.1"}, VRFDevice: "vr-vs-a"},
			{Name: "vs-b", Addresses: []string{"192.168.100.1"}, VRFDevice: "vr-vs-b"},
		},
	}
	if err := f.m.Sync(ctx, want); err != nil {
		t.Fatalf("收敛失败：%v", err)
	}
	sa := f.lay.socket("192.168.99.1", "vr-vs-a")
	sb := f.lay.socket("192.168.100.1", "vr-vs-b")

	// vs-a 有按域上游 ⇒ 只问 1.1.1.1，全局 9.9.9.9 不被问；应答原样写回客户端地址与端口。
	qa := dnsProxyQuery(0x1111, "a.test")
	sa.feed(qa, dnsProxyClient("192.168.99.50", 40000))
	gotA := waitDNSProxySend(t, sa, 1)
	if !bytes.Equal(gotA[0].payload, []byte{0xBE, 0xEF}) {
		t.Fatalf("上游成功应答应原样写回：%x", gotA[0].payload)
	}
	if gotA[0].to.String() != "192.168.99.50:40000" {
		t.Fatalf("应答应写回发起查询的客户端地址：%v", gotA[0].to)
	}
	if got := askedServers(); len(got) != 1 || got[0] != "1.1.1.1" {
		t.Fatalf("按域上游应优先于全局（不该问全局）：%v", got)
	}

	// vs-b 无按域上游 ⇒ 回落全局；注入的上游对 9.9.9.9 报错 ⇒ SERVFAIL。
	qb := dnsProxyQuery(0x2222, "b.test")
	sb.feed(qb, dnsProxyClient("192.168.100.50", 40001))
	gotB := waitDNSProxySend(t, sb, 1)
	assertDNSProxyServfail(t, qb, gotB[0].payload)
	if got := askedServers(); len(got) != 2 || got[1] != "9.9.9.9" {
		t.Fatalf("无按域上游应回落全局：%v", got)
	}
	if len(sa.sent()) != 1 {
		t.Fatalf("应答不该写到别的域的 socket 上：%v", sa.sent())
	}
	if st := f.m.State(); st.Answered != 1 || st.Servfail != 1 || st.SendFail != 0 {
		t.Fatalf("计数应如实：answered=1 servfail=1 send_fail=0，得到 %+v", st)
	}
}

// 全局与按域都没有该域的上游（全局为空、该域无按域条目）⇒ 查询回 SERVFAIL（不静默丢弃、
// 不问任何上游），servfail 计数增长。
func TestDNSProxyNoUpstreamsForDomainServfail(t *testing.T) {
	f := newDNSProxyFixture(t)
	ctx := context.Background()
	var mu sync.Mutex
	var asked []string
	f.m.SetUpstream(func(server string, _ []byte) ([]byte, error) {
		mu.Lock()
		asked = append(asked, server)
		mu.Unlock()
		return nil, fmt.Errorf("不该被问到")
	})
	want := orchestrator.DNSProxyUpstreams{
		PerSwitch: map[string][]string{"vs-a": {"1.1.1.1"}},
		Domains: []orchestrator.DNSProxyDomain{
			{Name: "vs-a", Addresses: []string{"192.168.99.1"}, VRFDevice: "vr-vs-a"},
			{Name: "vs-b", Addresses: []string{"192.168.100.1"}, VRFDevice: "vr-vs-b"},
		},
	}
	if err := f.m.Sync(ctx, want); err != nil {
		t.Fatalf("收敛失败：%v", err)
	}
	sb := f.lay.socket("192.168.100.1", "vr-vs-b")
	q := dnsProxyQuery(0x3333, "no-upstream.test")
	sb.feed(q, dnsProxyClient("192.168.100.50", 40002))
	got := waitDNSProxySend(t, sb, 1)
	assertDNSProxyServfail(t, q, got[0].payload)
	mu.Lock()
	n := len(asked)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("没有上游就不该问任何上游：%v", asked)
	}
	if st := f.m.State(); st.Servfail != 1 || st.Answered != 0 {
		t.Fatalf("无上游应计 servfail：%+v", st)
	}
	if st := f.m.State(); len(st.Domains[1].Upstreams) != 0 {
		t.Fatalf("State 应如实报该域没有生效上游：%+v", st.Domains[1])
	}
}

// ---------- ④ 上游失败 / 回包失败 ----------

// 上游全部失败 ⇒ SERVFAIL；回包失败（Send 报错）如实计数、不静默。
func TestDNSProxyUpstreamAndSendFailuresAreHonest(t *testing.T) {
	f := newDNSProxyFixture(t)
	ctx := context.Background()
	f.m.SetUpstream(func(server string, _ []byte) ([]byte, error) {
		return nil, fmt.Errorf("上游 %s 不可达", server)
	})
	want := orchestrator.DNSProxyUpstreams{
		Global: []string{"8.8.8.8", "9.9.9.9"}, // 双上游：都失败也要如实回 SERVFAIL
		Domains: []orchestrator.DNSProxyDomain{
			{Name: "vs-a", Addresses: []string{"192.168.99.1"}, VRFDevice: "vr-vs-a"},
		},
	}
	if err := f.m.Sync(ctx, want); err != nil {
		t.Fatalf("收敛失败：%v", err)
	}
	s := f.lay.socket("192.168.99.1", "vr-vs-a")
	q := dnsProxyQuery(0x4444, "fail.test")
	s.feed(q, dnsProxyClient("192.168.99.60", 40003))
	got := waitDNSProxySend(t, s, 1)
	assertDNSProxyServfail(t, q, got[0].payload)
	if st := f.m.State(); st.Servfail != 1 {
		t.Fatalf("上游失败应计 servfail：%+v", st)
	}

	// 回包失败：Send 报错 ⇒ send_fail 计数（应答没送达也要如实反映）。
	s.setSendErr(fmt.Errorf("网络不可达（注入）"))
	q2 := dnsProxyQuery(0x5555, "sendfail.test")
	s.feed(q2, dnsProxyClient("192.168.99.61", 40004))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.m.State().SendFail == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st := f.m.State(); st.SendFail != 1 || st.Servfail != 2 {
		t.Fatalf("回包失败应如实计数：%+v", st)
	}
}

// ---------- ⑤ 起不来如实报错 + 恢复后幂等收敛 ----------

func TestDNSProxyOpenFailureIsHonestAndRetryConverges(t *testing.T) {
	f := newDNSProxyFixture(t)
	ctx := context.Background()
	f.lay.setFail("192.168.100.1", errors.New("bind: cannot assign requested address"))
	want := orchestrator.DNSProxyUpstreams{
		Global: []string{"8.8.8.8"},
		Domains: []orchestrator.DNSProxyDomain{
			{Name: "vs-a", Addresses: []string{"192.168.99.1", "192.168.100.1"}, VRFDevice: "vr-vs-a"},
		},
	}
	err := f.m.Sync(ctx, want)
	if err == nil {
		t.Fatal("一个落点起不来必须如实报错（不得静默）")
	}
	if !strings.Contains(err.Error(), "vs-a") || !strings.Contains(err.Error(), "192.168.100.1") {
		t.Fatalf("错误应点名域与落点地址：%v", err)
	}
	// 同域已开的落点不回滚（下次 Sync 幂等重试即补齐）。
	if s := f.lay.socket("192.168.99.1", "vr-vs-a"); s == nil || s.closed() {
		t.Fatal("同一域里已绑定的落点不该被回滚")
	}
	st := f.m.State()
	if len(st.Domains) != 1 || !strings.Contains(st.Domains[0].Error, "192.168.100.1") {
		t.Fatalf("State 应如实报该域未收敛原因：%+v", st.Domains)
	}
	if got := st.Domains[0].Addresses; !reflect.DeepEqual(got, []string{"192.168.99.1"}) {
		t.Fatalf("State 的地址应只报已绑定（服务中）的落点：%v", got)
	}
	// 底座恢复 ⇒ 同一声明（声明未变）幂等收敛到位、错误清空。
	f.lay.setFail("192.168.100.1", nil)
	if err := f.m.Sync(ctx, want); err != nil {
		t.Fatalf("底座恢复后应收敛成功：%v", err)
	}
	if s := f.lay.socket("192.168.100.1", "vr-vs-a"); s == nil || s.closed() {
		t.Fatal("恢复后应补起落点")
	}
	st2 := f.m.State()
	if st2.Domains[0].Error != "" {
		t.Fatalf("恢复后不该再报该域未收敛：%q", st2.Domains[0].Error)
	}
	if got := st2.Domains[0].Addresses; !reflect.DeepEqual(got, []string{"192.168.99.1", "192.168.100.1"}) {
		t.Fatalf("恢复后两个落点都应在服务：%v", got)
	}
}

// 非 IPv4 / 非 IP 的落点声明：如实报该域的错误，且不为它开 socket（其余地址照常收敛）。
func TestDNSProxyNonIPv4AddressIsHonest(t *testing.T) {
	f := newDNSProxyFixture(t)
	ctx := context.Background()
	want := orchestrator.DNSProxyUpstreams{
		Global: []string{"8.8.8.8"},
		Domains: []orchestrator.DNSProxyDomain{
			{Name: "vs-v6", Addresses: []string{"2001:db8::1", "不是地址"}, VRFDevice: "vr-v6"},
		},
	}
	err := f.m.Sync(ctx, want)
	if err == nil {
		t.Fatal("非 IPv4 落点应如实报错")
	}
	if !strings.Contains(err.Error(), "vs-v6") {
		t.Fatalf("错误应点名域：%v", err)
	}
	if got := f.lay.openKeys(); len(got) != 0 {
		t.Fatalf("非 IPv4 落点不该开 socket：%v", got)
	}
}

// ---------- ⑥ State 快照 ----------

func TestDNSProxyStateSnapshotOrderAndFields(t *testing.T) {
	f := newDNSProxyFixture(t)
	ctx := context.Background()
	want := orchestrator.DNSProxyUpstreams{
		Global:    []string{"8.8.8.8", "8.8.8.8", ""}, // 去重 + 剔空（共享实现）
		PerSwitch: map[string][]string{"vs-b": {"1.1.1.1", "1.1.1.1"}},
		Domains: []orchestrator.DNSProxyDomain{
			{Name: "vs-b", Addresses: []string{"192.168.100.1"}, VRFDevice: "vr-vs-b"},
			{Name: "vs-a", Addresses: []string{"192.168.99.1"}, VRFDevice: "vr-vs-a"},
		},
	}
	if err := f.m.Sync(ctx, want); err != nil {
		t.Fatalf("收敛失败：%v", err)
	}
	st := f.m.State()
	if len(st.Domains) != 2 || st.Domains[0].Name != "vs-b" || st.Domains[1].Name != "vs-a" {
		t.Fatalf("State 应按**声明序**报域：%+v", st.Domains)
	}
	b, a := st.Domains[0], st.Domains[1]
	if b.VRFDevice != "vr-vs-b" || !reflect.DeepEqual(b.Addresses, []string{"192.168.100.1"}) ||
		!reflect.DeepEqual(b.Upstreams, []string{"1.1.1.1"}) || b.Error != "" {
		t.Fatalf("vs-b 快照不符：%+v", b)
	}
	if a.VRFDevice != "vr-vs-a" || !reflect.DeepEqual(a.Addresses, []string{"192.168.99.1"}) ||
		!reflect.DeepEqual(a.Upstreams, []string{"8.8.8.8"}) || a.Error != "" {
		t.Fatalf("vs-a 应回落去重后的全局上游：%+v", a)
	}
	if st.Answered != 0 || st.Servfail != 0 || st.SendFail != 0 {
		t.Fatalf("未处理任何查询时计数应为 0：%+v", st)
	}
}

// ---------- ⑧ 生命周期 Close ----------

func TestDNSProxyCloseStopsReaders(t *testing.T) {
	f := newDNSProxyFixture(t)
	ctx := context.Background()
	want := orchestrator.DNSProxyUpstreams{
		Global: []string{"8.8.8.8"},
		Domains: []orchestrator.DNSProxyDomain{
			{Name: "vs-a", Addresses: []string{"192.168.99.1", "10.0.0.1"}, VRFDevice: "vr-vs-a"},
		},
	}
	if err := f.m.Sync(ctx, want); err != nil {
		t.Fatalf("收敛失败：%v", err)
	}
	s1 := f.lay.socket("192.168.99.1", "vr-vs-a")
	s2 := f.lay.socket("10.0.0.1", "vr-vs-a")
	if err := f.m.Close(); err != nil {
		t.Fatalf("Close 应正常返回（收包协程已在期限内退出）：%v", err)
	}
	if !s1.closed() || !s2.closed() {
		t.Fatal("Close 应关闭全部落点 socket")
	}
	if s1.recvClosed.Load() == 0 || s2.recvClosed.Load() == 0 {
		t.Fatal("Close 返回前收包协程必须已退出（Recv 以 net.ErrClosed 返回）")
	}
	if s1.closeCount() != 1 {
		t.Fatalf("Close 应只关一次（幂等）：%d", s1.closeCount())
	}
	if st := f.m.State(); len(st.Domains) != 0 {
		t.Fatalf("Close 后不该再有域落点：%+v", st)
	}
	if err := f.m.Close(); err != nil {
		t.Fatalf("Close 应幂等：%v", err)
	}
}

// ---------- Provider 接线（空声明在从未装配过的 provider 上是空操作） ----------

func TestApplyDNSProxyEmptyWantIsNoopOnUnusedProvider(t *testing.T) {
	host := &fakeRunner{}
	p := New(host)
	lay := newDNSProxyFakeLayer()
	p.SetDNSProxyLayer(lay)
	ctx := context.Background()
	if _, ok := p.DNSProxyState(); ok {
		t.Fatal("从未装配过时不该有运行态读数（ok=false）")
	}
	if err := p.ApplyDNSProxy(ctx, orchestrator.DNSProxyUpstreams{}); err != nil {
		t.Fatalf("空声明应空操作：%v", err)
	}
	if got := lay.openKeys(); len(got) != 0 {
		t.Fatalf("空声明不该开任何落点：%v", got)
	}
	st, ok := p.DNSProxyState()
	if !ok {
		t.Fatal("收敛过（哪怕空声明）后运行态读数应可用（ok=true）")
	}
	if len(st.Domains) != 0 {
		t.Fatalf("空声明的运行态应为空：%+v", st)
	}
}
