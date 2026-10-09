package network

// 决策 #345：数据面 DNS 代理（自研域内转发器）的单元测试。
// 覆盖：注册/注销生命周期、按域上游选择与回落、SERVFAIL（不静默丢弃）、
// 上行包解析（L2 前导容忍）、回注包构造（对称 desc + 反向五元组）。

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/xzjt/nfvis/internal/orchestrator"
)

type fakePunt struct {
	registers   int
	deregisters int
	registered  bool
	path        string
	port        uint16
	regErr      error
}

func (f *fakePunt) Register(path string, port uint16) error {
	if f.regErr != nil {
		return f.regErr
	}
	f.registers++
	f.registered = true
	f.path = path
	f.port = port
	return nil
}
func (f *fakePunt) Deregister(port uint16) error {
	f.deregisters++
	f.registered = false
	return nil
}
func (f *fakePunt) Close() {}

type fakePacket struct {
	desc PuntDesc
	pkt  []byte
}

type fakeTransport struct {
	mu     sync.Mutex
	sent   []fakePacket
	closed bool
}

func (t *fakeTransport) Recv() (PuntDesc, []byte, error) {
	return PuntDesc{}, nil, net.ErrClosed // 单测直接调 handle，不走 serve 循环
}
func (t *fakeTransport) Send(desc PuntDesc, pkt []byte) error {
	t.mu.Lock()
	t.sent = append(t.sent, fakePacket{desc, append([]byte{}, pkt...)})
	t.mu.Unlock()
	return nil
}
func (t *fakeTransport) Close() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	return nil
}
func (t *fakeTransport) last() (fakePacket, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.sent) == 0 {
		return fakePacket{}, false
	}
	return t.sent[len(t.sent)-1], true
}

// newTestProvider 构造一个可注入的 provider；switchOf 由用例按需接上。
func newTestProvider(fp *fakePunt, tr *fakeTransport, up func(server string, query []byte) ([]byte, error)) *DNSProxyProvider {
	return NewDNSProxyProvider(fp, tr, up, "/tmp/nfvis-test-dns.sock", "/tmp/nfvis-test-punt.sock")
}

func dnsQueryBytes(id uint16) []byte {
	q := make([]byte, 12)
	binary.BigEndian.PutUint16(q[0:2], id)
	binary.BigEndian.PutUint16(q[4:6], 1) // QDCOUNT=1
	q = append(q, 5, 'p', 'r', 'o', 'b', 'e', 4, 't', 'e', 's', 't', 0)
	q = append(q, 0, 1, 0, 1) // QTYPE=A QCLASS=IN
	return q
}

// mkUDPQuery 构造一个 IPv4/UDP 查询包（可选 14 字节 L2 前导）。
func mkUDPQuery(srcIP, dstIP [4]byte, srcPort, dstPort uint16, dns []byte, withL2 bool) []byte {
	var pkt []byte
	if withL2 {
		pkt = append(pkt, make([]byte, 14)...) // 内容无关紧要（读侧不依赖）
	}
	ip := make([]byte, 20)
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(20+8+len(dns)))
	ip[8] = 64
	ip[9] = 17
	copy(ip[12:16], srcIP[:])
	copy(ip[16:20], dstIP[:])
	binary.BigEndian.PutUint16(ip[10:12], ipChecksum(ip))
	udp := make([]byte, 8)
	binary.BigEndian.PutUint16(udp[0:2], srcPort)
	binary.BigEndian.PutUint16(udp[2:4], dstPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(8+len(dns)))
	pkt = append(pkt, ip...)
	pkt = append(pkt, udp...)
	pkt = append(pkt, dns...)
	return pkt
}

// -------- 生命周期 --------

func TestDNSProxyRegisterLifecycle(t *testing.T) {
	fp := &fakePunt{}
	tr := &fakeTransport{}
	p := newTestProvider(fp, tr, func(string, []byte) ([]byte, error) { return nil, fmt.Errorf("no upstream") })
	ctx := context.Background()

	// 初始（全空）：不注册、不起转发器
	if err := p.Sync(ctx, orchestrator.DNSProxyUpstreams{}); err != nil {
		t.Fatalf("空声明 Sync: %v", err)
	}
	if fp.registered {
		t.Fatal("全空不应注册 punt")
	}

	// 启用（全局非空）：注册一次、端口 53
	if err := p.Sync(ctx, orchestrator.DNSProxyUpstreams{Global: []string{"8.8.8.8"}}); err != nil {
		t.Fatalf("启用 Sync: %v", err)
	}
	if !fp.registered || fp.registers != 1 || fp.port != dnsProxyUDPPort {
		t.Fatalf("启用应注册一次 ipv4/udp/53: %+v", fp)
	}

	// 再次启用（声明变化）：不重复注册
	if err := p.Sync(ctx, orchestrator.DNSProxyUpstreams{Global: []string{"8.8.8.8", "1.1.1.1"}}); err != nil {
		t.Fatalf("二次启用 Sync: %v", err)
	}
	if fp.registers != 1 {
		t.Fatalf("已注册时不应重复注册: registers=%d", fp.registers)
	}

	// 按域上游也算启用
	fp2 := &fakePunt{}
	p2 := newTestProvider(fp2, &fakeTransport{}, func(string, []byte) ([]byte, error) { return nil, nil })
	if err := p2.Sync(ctx, orchestrator.DNSProxyUpstreams{PerSwitch: map[string][]string{"vs-a": {"9.9.9.9"}}}); err != nil {
		t.Fatalf("按域启用 Sync: %v", err)
	}
	if !fp2.registered {
		t.Fatal("按域非空也应注册 punt")
	}

	// 停用：注销 + 关转发器 socket
	if err := p.Sync(ctx, orchestrator.DNSProxyUpstreams{}); err != nil {
		t.Fatalf("停用 Sync: %v", err)
	}
	if fp.registered || fp.deregisters != 1 {
		t.Fatalf("停用应注销一次: %+v", fp)
	}
	if !tr.closed {
		t.Fatal("停用应关闭转发器 socket")
	}
}

// reset 模拟 VPP 重启：注册丢失，下一次 Sync 应重注册。
func TestDNSProxyResetReRegisters(t *testing.T) {
	fp := &fakePunt{}
	tr := &fakeTransport{}
	p := newTestProvider(fp, tr, func(string, []byte) ([]byte, error) { return nil, nil })
	ctx := context.Background()
	if err := p.Sync(ctx, orchestrator.DNSProxyUpstreams{Global: []string{"8.8.8.8"}}); err != nil {
		t.Fatal(err)
	}
	p.reset()
	if err := p.Sync(ctx, orchestrator.DNSProxyUpstreams{Global: []string{"8.8.8.8"}}); err != nil {
		t.Fatal(err)
	}
	if fp.registers != 2 {
		t.Fatalf("reset 后应重注册: registers=%d", fp.registers)
	}
	// Close（进程退出）：注销
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if fp.registered {
		t.Fatal("Close 应注销 punt")
	}
}

// 注册失败（如 startup.conf 未含 punt 段）必须如实报错，不静默放行。
func TestDNSProxyRegisterErrorSurfaces(t *testing.T) {
	fp := &fakePunt{regErr: fmt.Errorf("socket is not configured")}
	p := newTestProvider(fp, &fakeTransport{}, func(string, []byte) ([]byte, error) { return nil, nil })
	err := p.Sync(context.Background(), orchestrator.DNSProxyUpstreams{Global: []string{"8.8.8.8"}})
	if err == nil {
		t.Fatal("注册失败应返回错误")
	}
}

// -------- 上游选择 + 回注 --------

func TestDNSProxyForwardUsesPerDomainAndReplies(t *testing.T) {
	fp := &fakePunt{}
	tr := &fakeTransport{}
	var gotServer string
	var gotQuery []byte
	p := newTestProvider(fp, tr, func(server string, query []byte) ([]byte, error) {
		gotServer = server
		gotQuery = query
		return []byte("UPSTREAM-ANSWER"), nil
	})
	p.SetSwitchResolver(func(idx uint32) (string, bool) {
		if idx == 5 {
			return "vs-a", true
		}
		return "", false
	})
	p.Sync(context.Background(), orchestrator.DNSProxyUpstreams{
		Global:    []string{"8.8.8.8"},
		PerSwitch: map[string][]string{"vs-a": {"10.0.0.53"}},
	})

	q := mkUDPQuery([4]byte{192, 168, 99, 2}, [4]byte{192, 168, 99, 1}, 34567, 53, dnsQueryBytes(0x1234), true)
	p.handle(tr, PuntDesc{SwIfIndex: 5, Action: 0}, q)

	if gotServer != "10.0.0.53" {
		t.Fatalf("应优先用按域上游: got %q", gotServer)
	}
	if len(gotQuery) == 0 || gotQuery[0] != 0x12 || gotQuery[1] != 0x34 {
		t.Fatalf("转发给上游的应是原查询报文: %x", gotQuery)
	}
	out, ok := tr.last()
	if !ok {
		t.Fatal("应回注一个包")
	}
	if out.desc.SwIfIndex != 5 || out.desc.Action != dnsPuntActionIP4Routed {
		t.Fatalf("回注 desc 应为 action=1 且 sw_if_index 取上行原值: %+v", out.desc)
	}
	// 回注包应为裸 IPv4：源=网关(99.1:53)、目的=客户端(99.2:34567)、载荷=上游应答
	if out.pkt[0]>>4 != 4 {
		t.Fatalf("回注应为裸 IPv4: %x", out.pkt[:1])
	}
	if !net.IP(out.pkt[12:16]).Equal(net.ParseIP("192.168.99.1")) {
		t.Fatalf("回注源地址应为网关: %v", net.IP(out.pkt[12:16]))
	}
	if !net.IP(out.pkt[16:20]).Equal(net.ParseIP("192.168.99.2")) {
		t.Fatalf("回注目的地址应为客户端: %v", net.IP(out.pkt[16:20]))
	}
	if sp := binary.BigEndian.Uint16(out.pkt[20:22]); sp != 53 {
		t.Fatalf("回注源端口应为 53: %d", sp)
	}
	if dp := binary.BigEndian.Uint16(out.pkt[22:24]); dp != 34567 {
		t.Fatalf("回注目的端口应为客户端源端口: %d", dp)
	}
	if string(out.pkt[28:]) != "UPSTREAM-ANSWER" {
		t.Fatalf("回注载荷应为上游应答: %q", out.pkt[28:])
	}
}

// 交换机无按域上游 ⇒ 回落全局。
func TestDNSProxyFallbackToGlobal(t *testing.T) {
	tr := &fakeTransport{}
	var used string
	p := newTestProvider(&fakePunt{}, tr, func(server string, query []byte) ([]byte, error) {
		used = server
		return []byte("OK"), nil
	})
	p.SetSwitchResolver(func(uint32) (string, bool) { return "vs-unknown", true })
	p.Sync(context.Background(), orchestrator.DNSProxyUpstreams{
		Global:    []string{"8.8.8.8"},
		PerSwitch: map[string][]string{"vs-a": {"10.0.0.53"}},
	})
	q := mkUDPQuery([4]byte{10, 0, 0, 2}, [4]byte{10, 0, 0, 1}, 40000, 53, dnsQueryBytes(1), false)
	p.handle(tr, PuntDesc{SwIfIndex: 9}, q)
	if used != "8.8.8.8" {
		t.Fatalf("无按域上游应回落全局: got %q", used)
	}
}

// 两者皆空 ⇒ SERVFAIL（快速失败，不静默丢弃），且计数。
func TestDNSProxyServfailWhenNoUpstream(t *testing.T) {
	tr := &fakeTransport{}
	p := newTestProvider(&fakePunt{}, tr, func(string, []byte) ([]byte, error) { return nil, fmt.Errorf("x") })
	// 只有全局 8.8.8.8 声明？这里故意全空：但全空时 Sync 不注册；直接调 handle 验证行为。
	p.Sync(context.Background(), orchestrator.DNSProxyUpstreams{Global: []string{"8.8.8.8"}})
	// 清空声明但保留（模拟「该域按域与全局都空」态）：直接改内部声明。
	p.mu.Lock()
	p.global = nil
	p.perSwitch = nil
	p.mu.Unlock()

	q := mkUDPQuery([4]byte{10, 0, 0, 2}, [4]byte{10, 0, 0, 1}, 40000, 53, dnsQueryBytes(0xBEEF), false)
	p.handle(tr, PuntDesc{SwIfIndex: 3}, q)
	out, ok := tr.last()
	if !ok {
		t.Fatal("无上游也应回 SERVFAIL（不得静默丢弃）")
	}
	dns := out.pkt[28:]
	if len(dns) < 12 {
		t.Fatalf("SERVFAIL 报文过短: %x", dns)
	}
	flags := binary.BigEndian.Uint16(dns[2:4])
	if flags&0x8000 == 0 || flags&0x000f != 2 {
		t.Fatalf("应为 QR=1 RCODE=SERVFAIL: flags=%#x", flags)
	}
	if p.SERVFAILCount() != 1 {
		t.Fatalf("SERVFAIL 应计数: %d", p.SERVFAILCount())
	}
}

// -------- 报文解析/构造 --------

func TestExtractIPHandlesL2Prefix(t *testing.T) {
	bare := mkUDPQuery([4]byte{1, 2, 3, 4}, [4]byte{5, 6, 7, 8}, 1, 53, dnsQueryBytes(1), false)
	if _, ok := extractIP(bare); !ok {
		t.Fatal("裸 IP 包应解析成功")
	}
	withL2 := mkUDPQuery([4]byte{1, 2, 3, 4}, [4]byte{5, 6, 7, 8}, 1, 53, dnsQueryBytes(1), true)
	ip, ok := extractIP(withL2)
	if !ok || ip[0]>>4 != 4 {
		t.Fatal("带 L2 前导的包应能定位到 IP 头")
	}
	if _, ok := extractIP([]byte{0xde, 0xad, 0xbe, 0xef}); ok {
		t.Fatal("非 IP 包不应被当 IP")
	}
}

func TestParseDNSQueryRejectsNon53(t *testing.T) {
	// 目的端口 1234 不是 53 ⇒ 不在覆盖内
	q := mkUDPQuery([4]byte{1, 2, 3, 4}, [4]byte{5, 6, 7, 8}, 1, 1234, dnsQueryBytes(1), false)
	if _, ok := parseDNSQuery(q); ok {
		t.Fatal("非 :53 查询不应处理")
	}
}

func TestBuildServfailEchoesQuestion(t *testing.T) {
	query := dnsQueryBytes(0x0102)
	sf := buildServfail(query)
	end, ok := dnsQuestionEnd(query)
	if !ok {
		t.Fatal("question 段应可解析")
	}
	if len(sf) != end {
		t.Fatalf("SERVFAIL 应回显 question（长度 %d，期望 %d）", len(sf), end)
	}
	if binary.BigEndian.Uint16(sf[0:2]) != 0x0102 {
		t.Fatal("事务 ID 应原样")
	}
	if binary.BigEndian.Uint16(sf[6:8]) != 0 {
		t.Fatal("ANCOUNT 应为 0")
	}
}
