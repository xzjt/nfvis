package netkernel

// 内核数据面 LLDP 自研收发代理（决策 #440）单测：**不依赖真 socket / 真内核口**——收发底座注入
// 内存实现（SetLLDPLayer / newLLDPManager），校验：
//  ① TLV 编解码（合法/未知 TLV 跳过/截断/仅 END/非 0x88CC/尾部 NUL 由共享渲染器去掉）；
//  ② 发送报文**字节级断言**（目的 MAC、源＝接口 MAC、ethertype、TLV 顺序与取值、TTL＝4×interval、
//     END 与补零）；
//  ③ 邻居表（新增/刷新/同口多邻居/键去重/TTL 过期/TTL=0 立即过期/缺必选 TLV 丢弃）；
//  ④ Sync 开关收敛（未启用不开、启用只开 Enabled=true 的口并立即各发一帧、接口关/全局关收
//     socket、间隔变更不重建 socket）；
//  ⑤ 一个口起不来如实报错且不拖累其它口、底座恢复后同一声明重试收敛；
//  ⑥ Neighbors 确定性排序与 LastHeard 秒数；
//  ⑦ Close 关 socket、等收发协程退出（有界）且幂等；
//  ⑧ Provider 接线（读视图「空表而非不支持」、未声明未装配时巡检空操作、恢复重放幂等、
//     Provider.Close 关管理器）。
// 源码扫描守护（apply 路径不读配置发动机）的覆盖文件清单见 dhcpserver_test.go。

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

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

const lldpTestHostname = "nk-lldp-host"

// ---------- 内存收发底座（AF_PACKET 绑该口） ----------

// lldpFakeIO 一个接口的收发 socket（内存）：feed 注入收到的帧、sends 记录发出的帧、failRecv
// 注入收包错误；Close 后 Recv 返回 net.ErrClosed（照真实现的语义——收包协程据此退出）。
type lldpFakeIO struct {
	ifname string
	mac    net.HardwareAddr
	in     chan []byte
	errCh  chan error
	done   chan struct{}
	once   sync.Once

	mu      sync.Mutex
	sends   [][]byte
	closes  int
	sendErr error

	recvClosed atomic.Int32 // Recv 因 Close 返回 net.ErrClosed 的次数（协程退出的证据）
}

func (s *lldpFakeIO) Recv(buf []byte) (int, error) {
	select {
	case <-s.done:
		s.recvClosed.Add(1)
		return 0, net.ErrClosed
	case err := <-s.errCh:
		return 0, err
	case frame := <-s.in:
		return copy(buf, frame), nil
	}
}

func (s *lldpFakeIO) Send(frame []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return net.ErrClosed
	default:
	}
	if s.sendErr != nil {
		return s.sendErr
	}
	s.sends = append(s.sends, append([]byte{}, frame...))
	return nil
}

func (s *lldpFakeIO) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closes++
		s.mu.Unlock()
		close(s.done)
	})
	return nil
}

func (s *lldpFakeIO) closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *lldpFakeIO) sent() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.sends...)
}

func (s *lldpFakeIO) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closes
}

func (s *lldpFakeIO) setSendErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendErr = err
}

func (s *lldpFakeIO) feed(frame []byte) { s.in <- frame }

func (s *lldpFakeIO) failRecv(err error) { s.errCh <- err }

// lldpFakeLayer 收发底座假实现：按接口名登记 socket（重复打开如实报错，模拟真 socket）；
// 可注入某接口的打开失败与返回的 MAC。
type lldpFakeLayer struct {
	mu     sync.Mutex
	opened map[string]*lldpFakeIO
	opens  []string
	fail   map[string]error
	macs   map[string]net.HardwareAddr
}

func newLLDPFakeLayer() *lldpFakeLayer {
	return &lldpFakeLayer{
		opened: map[string]*lldpFakeIO{},
		fail:   map[string]error{},
		macs:   map[string]net.HardwareAddr{},
	}
}

func (l *lldpFakeLayer) Open(ifname string) (lldpIO, net.HardwareAddr, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.fail[ifname]; err != nil {
		return nil, nil, err
	}
	if cur := l.opened[ifname]; cur != nil && !cur.closed() {
		return nil, nil, fmt.Errorf("AF_PACKET %s 已在绑定状态", ifname)
	}
	mac := l.macs[ifname]
	if mac == nil {
		mac = lldpTestMAC(ifname)
	}
	s := &lldpFakeIO{
		ifname: ifname, mac: mac,
		in:    make(chan []byte, 8),
		errCh: make(chan error, 4),
		done:  make(chan struct{}),
	}
	l.opened[ifname] = s
	l.opens = append(l.opens, ifname)
	return s, mac, nil
}

func (l *lldpFakeLayer) socket(ifname string) *lldpFakeIO {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.opened[ifname]
}

func (l *lldpFakeLayer) openKeys() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.opens...)
}

func (l *lldpFakeLayer) setFail(ifname string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil {
		delete(l.fail, ifname)
		return
	}
	l.fail[ifname] = err
}

// lldpTestMAC 按接口名派生确定的测试 MAC（仅测试用；02:00 前缀＝本地管理地址段）。
func lldpTestMAC(ifname string) net.HardwareAddr {
	sum := 0
	for i := 0; i < len(ifname); i++ {
		sum = (sum*31 + int(ifname[i])) & 0xff
	}
	return net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, byte(sum)}
}

// ---------- 装配与断言助手 ----------

type lldpFixture struct {
	m   *lldpManager
	lay *lldpFakeLayer
}

func newLLDPFixture(t *testing.T) *lldpFixture {
	t.Helper()
	f := &lldpFixture{lay: newLLDPFakeLayer()}
	f.m = newLLDPManager(f.lay)
	f.m.SetHostnameSource(func() string { return lldpTestHostname })
	t.Cleanup(func() { _ = f.m.Close() })
	return f
}

// rt 取某接口的运行态（测试内部访问；按管理器锁）。
func (f *lldpFixture) rt(name string) *lldpIfaceRT {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	return f.m.ifaces[name]
}

// lldpCfg 组一份声明：on 里的口启用；interval ≤0 表示不写间隔（走缺省 30s）。
func lldpCfg(interval int, on ...string) *model.LldpConfig {
	cfg := &model.LldpConfig{Enabled: true}
	if interval > 0 {
		cfg.AdvertisementInterval = interval
	}
	for _, n := range on {
		cfg.Interfaces = append(cfg.Interfaces, model.LldpInterface{Interface: n, Enabled: true})
	}
	return cfg
}

// waitLLDPSend 等某口发出第 n 帧（有界轮询；超时即失败）。
func waitLLDPSend(t *testing.T, s *lldpFakeIO, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.sent(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等 %s 的第 %d 帧广告超时（现有 %d 帧）", s.ifname, n, len(s.sent()))
	return nil
}

// waitLLDPNeighbors 等邻居表达到 n 条（有界轮询）。
func waitLLDPNeighbors(t *testing.T, m *lldpManager, n int) []network.LldpNeighbor {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := m.Neighbors(time.Now()); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等邻居表达到 %d 条超时（现有 %d 条）", n, len(m.Neighbors(time.Now())))
	return nil
}

// waitLLDPMalformed 等畸形/非 LLDP 帧计数达到 n（有界轮询）。
func waitLLDPMalformed(t *testing.T, m *lldpManager, n uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadUint64(&m.malformed) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等畸形帧计数达到 %d 超时（现有 %d）", n, atomic.LoadUint64(&m.malformed))
}

// lldpStateOf 取某接口的运行态快照条目。
func lldpStateOf(m *lldpManager, name string) (lldpIfaceState, bool) {
	for _, st := range m.State() {
		if st.Name == name {
			return st, true
		}
	}
	return lldpIfaceState{}, false
}

// ---------- 手拼帧助手（解析用例的输入独立可核，不借生产编码器） ----------

type lldpTestTLV struct {
	typ   uint16
	value []byte
}

// lldpTestFrame 手拼一条以太帧：目的＝LLDP 组播、源＝固定测试 MAC、ethertype 0x88CC，
// 然后逐条 TLV（9 位类型 + 9 位长度，大端）。
func lldpTestFrame(tlvs ...lldpTestTLV) []byte {
	frame := []byte{0x01, 0x80, 0xc2, 0x00, 0x00, 0x0e, 0x02, 0x00, 0x00, 0x00, 0x00, 0x11, 0x88, 0xcc}
	for _, tlv := range tlvs {
		head := tlv.typ<<9 | uint16(len(tlv.value))
		frame = append(frame, byte(head>>8), byte(head&0xff))
		frame = append(frame, tlv.value...)
	}
	return frame
}

// lldpTestIDTLV 一条标识 TLV 的值：subtype + 原始字节。
func lldpTestIDTLV(subtype uint8, id []byte) []byte {
	return append([]byte{subtype}, id...)
}

// lldpTestTLVSeq 按序取出帧里的原始 TLV（到 END 为止），并返回 END TLV 之后的偏移（补零起点）。
func lldpTestTLVSeq(t *testing.T, frame []byte) ([]lldpTestTLV, int) {
	t.Helper()
	if len(frame) < 14 {
		t.Fatalf("帧短于以太头：%d", len(frame))
	}
	body := frame[14:]
	var out []lldpTestTLV
	for i := 0; i+2 <= len(body); {
		head := binary.BigEndian.Uint16(body[i : i+2])
		typ, length := head>>9, int(head&0x1ff)
		if i+2+length > len(body) {
			t.Fatalf("TLV 值越过帧尾：type=%d len=%d", typ, length)
		}
		out = append(out, lldpTestTLV{typ: typ, value: append([]byte(nil), body[i+2:i+2+length]...)})
		i += 2 + length
		if typ == lldpTlvEnd {
			return out, 14 + i
		}
	}
	t.Fatalf("帧里没有 END TLV：%x", frame)
	return nil, 0
}

// lldpTestFindTLV 取第一条指定类型的 TLV。
func lldpTestFindTLV(t *testing.T, frame []byte, typ uint16) (lldpTestTLV, bool) {
	t.Helper()
	tlvs, _ := lldpTestTLVSeq(t, frame)
	for _, tlv := range tlvs {
		if tlv.typ == typ {
			return tlv, true
		}
	}
	return lldpTestTLV{}, false
}

// lldpTestTTLOf 取广告帧的 TTL 值（秒）。
func lldpTestTTLOf(t *testing.T, frame []byte) int {
	t.Helper()
	tlv, ok := lldpTestFindTLV(t, frame, lldpTlvTTL)
	if !ok || len(tlv.value) != 2 {
		t.Fatalf("帧里应有 2 字节 TTL TLV：%x", frame)
	}
	return int(binary.BigEndian.Uint16(tlv.value))
}

// lldpTestSystemNameOf 取广告帧的 system name（无该 TLV 返回 ""）。
func lldpTestSystemNameOf(t *testing.T, frame []byte) string {
	t.Helper()
	tlv, ok := lldpTestFindTLV(t, frame, lldpTlvSystemName)
	if !ok {
		return ""
	}
	return string(tlv.value)
}

// ---------- ① TLV 编解码 ----------

func TestLLDPParseBasicTLVsSkipsUnknownAndTrimsNULs(t *testing.T) {
	frame := lldpTestFrame(
		// 文本型 chassis（subtype 7）+ 尾部 NUL：由共享渲染器去掉。
		lldpTestTLV{typ: lldpTlvChassisID, value: lldpTestIDTLV(lldpChassisSubtypeLocallyAssigned, []byte("sw1\x00\x00"))},
		lldpTestTLV{typ: 127, value: []byte{0x00, 0x12, 0x0f, 0x01}}, // 组织自定义（未知）：跳过
		lldpTestTLV{typ: lldpTlvPortID, value: lldpTestIDTLV(lldpPortSubtypeInterfaceName, []byte("Gi0/1"))},
		lldpTestTLV{typ: lldpTlvTTL, value: []byte{0x00, 0x78}}, // 120 秒
		lldpTestTLV{typ: lldpTlvSystemName, value: []byte("sw1")},
		lldpTestTLV{typ: lldpTlvEnd},
	)
	tlvs, ok := parseLLDPDU(frame)
	if !ok {
		t.Fatal("合法 LLDPDU 应解析成功")
	}
	if !tlvs.hasChassis || tlvs.chassis.subtype != lldpChassisSubtypeLocallyAssigned {
		t.Fatalf("chassis TLV 应解析出 subtype=7：%+v", tlvs)
	}
	if got := network.LldpIDBySubtype(uint32(tlvs.chassis.subtype), lldpChassisSubtypeMAC, tlvs.chassis.value); got != "sw1" {
		t.Fatalf("尾部 NUL 应由共享渲染器去掉：%q", got)
	}
	if !tlvs.hasPort || tlvs.port.subtype != lldpPortSubtypeInterfaceName {
		t.Fatalf("port TLV 应解析出 subtype=5：%+v", tlvs)
	}
	if got := network.LldpIDBySubtype(uint32(tlvs.port.subtype), lldpPortSubtypeMAC, tlvs.port.value); got != "Gi0/1" {
		t.Fatalf("port ID 应渲染为接口名：%q", got)
	}
	if !tlvs.hasTTL || tlvs.ttl != 120 || tlvs.systemName != "sw1" {
		t.Fatalf("TTL/system name 应解析正确：%+v", tlvs)
	}

	// MAC 型标识（chassis MAC = 4、port MAC = 3）走同一份渲染器 ⇒ MAC 串而不是乱码。
	macFrame := lldpTestFrame(
		lldpTestTLV{typ: lldpTlvChassisID, value: lldpTestIDTLV(lldpChassisSubtypeMAC, []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff})},
		lldpTestTLV{typ: lldpTlvPortID, value: lldpTestIDTLV(lldpPortSubtypeMAC, []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55})},
		lldpTestTLV{typ: lldpTlvEnd},
	)
	tmac, ok := parseLLDPDU(macFrame)
	if !ok {
		t.Fatal("MAC 型标识的帧应解析成功")
	}
	if got := network.LldpIDBySubtype(uint32(tmac.chassis.subtype), lldpChassisSubtypeMAC, tmac.chassis.value); got != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("MAC 型 chassis ID 应渲染为 MAC 串：%q", got)
	}
	if got := network.LldpIDBySubtype(uint32(tmac.port.subtype), lldpPortSubtypeMAC, tmac.port.value); got != "00:11:22:33:44:55" {
		t.Fatalf("MAC 型 port ID 应渲染为 MAC 串：%q", got)
	}
}

func TestLLDPParseMalformedIsRejected(t *testing.T) {
	head := []byte{0x01, 0x80, 0xc2, 0x00, 0x00, 0x0e, 0x02, 0x00, 0x00, 0x00, 0x00, 0x11, 0x88, 0xcc}
	cases := map[string][]byte{
		"帧短于以太头": []byte{0x01, 0x80, 0xc2},
		"以太类型不是 0x88CC": func() []byte {
			f := append([]byte(nil), head...)
			f[12], f[13] = 0x08, 0x06 // ARP 等链路噪声：不是 LLDP
			return f
		}(),
		"TLV 头不全（尾随 1 字节）": append(append([]byte(nil), head...), 0x02),
		// 声明 16 字节值、实际只有 4 字节：帧被截断。
		"TLV 值长度越过帧尾": append(append([]byte(nil), head...), 0x02, 0x10, 0x07, 0x01, 0x00, 0x00),
	}
	for name, frame := range cases {
		if _, ok := parseLLDPDU(frame); ok {
			t.Fatalf("%s：畸形/非 LLDP 帧必须判 false", name)
		}
	}

	// 仅 END 的帧是合法空 LLDPDU：解析成功、没有任何基础 TLV（不是错误）。
	onlyEnd := append(append([]byte(nil), head...), lldpTLV(lldpTlvEnd, nil)...)
	tlvs, ok := parseLLDPDU(onlyEnd)
	if !ok {
		t.Fatal("仅 END 的帧应解析成功")
	}
	if tlvs.hasChassis || tlvs.hasPort || tlvs.hasTTL || tlvs.systemName != "" {
		t.Fatalf("仅 END 的帧不该解析出基础 TLV：%+v", tlvs)
	}
}

// ---------- ② 发送报文：字节级断言 ----------

func TestLLDPBuildFrameBytesTLVOrderAndTTL(t *testing.T) {
	src := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x01}
	frame := buildLLDPFrame(src, lldpTestHostname, "eth0", 30)

	if !bytes.Equal(frame[:6], []byte{0x01, 0x80, 0xc2, 0x00, 0x00, 0x0e}) {
		t.Fatalf("目的 MAC 必须是 LLDP 保留组播 01:80:c2:00:00:0e：%x", frame[:6])
	}
	if !bytes.Equal(frame[6:12], src) {
		t.Fatalf("源 MAC 必须是接口 MAC：%x", frame[6:12])
	}
	if got := binary.BigEndian.Uint16(frame[12:14]); got != lldpEtherType {
		t.Fatalf("ethertype 必须是 0x88cc：%#x", got)
	}
	tlvs, end := lldpTestTLVSeq(t, frame)
	want := []lldpTestTLV{
		{typ: lldpTlvChassisID, value: lldpTestIDTLV(lldpChassisSubtypeLocallyAssigned, []byte(lldpTestHostname))},
		{typ: lldpTlvPortID, value: lldpTestIDTLV(lldpPortSubtypeInterfaceName, []byte("eth0"))},
		{typ: lldpTlvTTL, value: []byte{0x00, 0x78}}, // 4 × 30
		{typ: lldpTlvSystemName, value: []byte(lldpTestHostname)},
		{typ: lldpTlvEnd, value: nil},
	}
	if !reflect.DeepEqual(tlvs, want) {
		t.Fatalf("TLV 顺序/取值不符：\n got %+v\nwant %+v", tlvs, want)
	}
	// 补到以太最小帧长；END 之后只有补零。
	if len(frame) < lldpMinFrameLen {
		t.Fatalf("短帧应补到以太最小帧长：%d", len(frame))
	}
	if pad := frame[end:]; len(bytes.Trim(pad, "\x00")) != 0 {
		t.Fatalf("END 之后应只有补零：%x", pad)
	}

	// 间隔＝4 × interval；缺省/非法间隔按 30s（TTL 120）。
	if got := lldpTestTTLOf(t, buildLLDPFrame(src, "h", "eth0", 1)); got != 4 {
		t.Fatalf("interval=1 的 TTL 应为 4：%d", got)
	}
	if got := lldpTestTTLOf(t, buildLLDPFrame(src, "h", "eth0", 0)); got != 4*lldpDefaultIntervalSeconds {
		t.Fatalf("非法间隔应按缺省 30s（TTL 120）：%d", got)
	}
	// 主机名为空 ⇒ 不发 system name TLV（必选的三条仍在）。
	if _, ok := lldpTestFindTLV(t, buildLLDPFrame(src, "", "eth0", 30), lldpTlvSystemName); ok {
		t.Fatal("主机名为空不该发 system name TLV")
	}
	// 接口 MAC 取不到 ⇒ 源 MAC 如实填全 0（不借别的口、不猜）。
	if !bytes.Equal(buildLLDPFrame(nil, "h", "eth0", 30)[6:12], make([]byte, 6)) {
		t.Fatal("接口 MAC 取不到时应如实填全 0")
	}
}

// 系统名**每次发送现读**来源（主机名可运行期变更，进程不重启）；来源读不到（空/空白）时
// 回落构造时的宿主主机名——不把「读不到」发成空系统名。
func TestLLDPHostnameIsReadPerAdvertisement(t *testing.T) {
	f := newLLDPFixture(t)
	f.m.hostname = "fallback-host" // 构造时的宿主主机名（来源读不到时的回落值）
	ctx := context.Background()
	var mu sync.Mutex
	cur := "host-a"
	f.m.SetHostnameSource(func() string {
		mu.Lock()
		defer mu.Unlock()
		return cur
	})
	if err := f.m.Sync(ctx, lldpCfg(30, "eth0")); err != nil {
		t.Fatalf("收敛失败：%v", err)
	}
	s := f.lay.socket("eth0")
	if got := lldpTestSystemNameOf(t, waitLLDPSend(t, s, 1)[0]); got != "host-a" {
		t.Fatalf("首帧系统名应取来源现值：%q", got)
	}
	mu.Lock()
	cur = "host-b"
	mu.Unlock()
	before := len(s.sent())
	if !f.m.sendAdvertisement(f.rt("eth0")) {
		t.Fatal("广告发送应成功")
	}
	sent := waitLLDPSend(t, s, before+1)
	if got := lldpTestSystemNameOf(t, sent[len(sent)-1]); got != "host-b" {
		t.Fatalf("主机名变更后应现读新值：%q", got)
	}
	// 来源读不到 ⇒ 回落构造值（而不是空串/空白）。
	f.m.SetHostnameSource(func() string { return "   " })
	before = len(s.sent())
	f.m.sendAdvertisement(f.rt("eth0"))
	sent = waitLLDPSend(t, s, before+1)
	if got := lldpTestSystemNameOf(t, sent[len(sent)-1]); got != "fallback-host" {
		t.Fatalf("来源读不到应回落构造时的宿主主机名：%q", got)
	}
}

// ---------- ③ 邻居表 ----------

// lldpTestID 一条标识 TLV 的解析形态（测试直调 hear 用）。
func lldpTestID(subtype uint8, v string) lldpID {
	return lldpID{subtype: subtype, value: []byte(v)}
}

func lldpTestTlvs(chassis, port string, ttl int) lldpTlvs {
	return lldpTlvs{
		chassis:    lldpTestID(lldpChassisSubtypeLocallyAssigned, chassis),
		hasChassis: true,
		port:       lldpTestID(lldpPortSubtypeInterfaceName, port),
		hasPort:    true,
		ttl:        ttl,
		hasTTL:     true,
	}
}

func TestLLDPNeighborTableAddRefreshDedupeAndExpire(t *testing.T) {
	f := newLLDPFixture(t)
	t0 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	// 新增：一条邻居，键＝（本地口, chassis, port）。
	f.m.hear("eth0", lldpTestTlvs("sw1", "Gi0/1", 4), t0)
	got := f.m.Neighbors(t0)
	if len(got) != 1 || got[0].Interface != "eth0" || got[0].ChassisID != "sw1" ||
		got[0].PortID != "Gi0/1" || got[0].TTL != 4 || got[0].LastHeard != 0 {
		t.Fatalf("新增邻居不符：%+v", got)
	}
	// LastHeard＝距上次收到的秒数。
	if got = f.m.Neighbors(t0.Add(3 * time.Second)); got[0].LastHeard != 3 {
		t.Fatalf("LastHeard 应为距上次收到的秒数：%v", got[0].LastHeard)
	}
	// 同键再收（TTL 变 10）⇒ 仍是 1 条（键去重、原地刷新）：LastHeard 归零、TTL 更新。
	f.m.hear("eth0", lldpTestTlvs("sw1", "Gi0/1", 10), t0.Add(3*time.Second))
	got = f.m.Neighbors(t0.Add(3 * time.Second))
	if len(got) != 1 || got[0].TTL != 10 || got[0].LastHeard != 0 {
		t.Fatalf("同键应原地刷新（不新增条目）：%+v", got)
	}
	// 同一本地口的第二个对端（chassis/port 不同）：另一条。
	f.m.hear("eth0", lldpTestTlvs("sw2", "Gi0/2", 4), t0.Add(4*time.Second))
	if got = f.m.Neighbors(t0.Add(4 * time.Second)); len(got) != 2 {
		t.Fatalf("同口不同对端应是两条邻居：%+v", got)
	}
	// TTL 过期（边界：恰好 TTL 仍在，超出才删）。sw1 于 t0+3s 刷新、TTL 10 ⇒ t0+13s 仍在。
	got = f.m.Neighbors(t0.Add(13 * time.Second))
	if len(got) != 1 || got[0].ChassisID != "sw1" {
		t.Fatalf("恰好到 TTL 不该过期（判据是 > TTL）；sw2（TTL 4）应已过期：%+v", got)
	}
	if got = f.m.Neighbors(t0.Add(13*time.Second + time.Nanosecond)); len(got) != 0 {
		t.Fatalf("超过对端 TTL 应移除：%+v", got)
	}
	// TTL=0（缺 TTL TLV 的不合规帧同理）⇒ 立即过期，不显示为邻居。
	f.m.hear("eth0", lldpTestTlvs("sw3", "Gi0/3", 0), t0.Add(20*time.Second))
	if got = f.m.Neighbors(t0.Add(20 * time.Second)); len(got) != 0 {
		t.Fatalf("TTL=0 应视为立即过期：%+v", got)
	}
	// 缺必选 TLV（chassis/port）的帧没法定键：按畸形丢弃、不建条目。
	bad := lldpTestTlvs("sw4", "Gi0/4", 30)
	bad.hasPort = false
	before := atomic.LoadUint64(&f.m.malformed)
	f.m.hear("eth0", bad, t0.Add(21*time.Second))
	if got = f.m.Neighbors(t0.Add(21 * time.Second)); len(got) != 0 {
		t.Fatalf("缺必选 TLV 的帧不该建邻居：%+v", got)
	}
	if atomic.LoadUint64(&f.m.malformed) != before+1 {
		t.Fatalf("缺必选 TLV 的帧应计畸形：%d → %d", before, atomic.LoadUint64(&f.m.malformed))
	}
}

// 收路径端到端：经 runReader 收对端广告建邻居；非 LLDP/畸形帧静默丢弃但不断循环；
// 收包错误如实记该口未收敛，恢复收帧后转回就绪。
func TestLLDPReaderUpdatesTableAndSurvivesNoise(t *testing.T) {
	f := newLLDPFixture(t)
	ctx := context.Background()
	if err := f.m.Sync(ctx, lldpCfg(30, "eth0")); err != nil {
		t.Fatalf("收敛失败：%v", err)
	}
	s := f.lay.socket("eth0")
	s.feed(lldpTestFrame(
		lldpTestTLV{typ: lldpTlvChassisID, value: lldpTestIDTLV(lldpChassisSubtypeMAC, []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff})},
		lldpTestTLV{typ: lldpTlvPortID, value: lldpTestIDTLV(lldpPortSubtypeInterfaceName, []byte("Gi0/9"))},
		lldpTestTLV{typ: lldpTlvTTL, value: []byte{0x00, 0x3c}}, // 60s
		lldpTestTLV{typ: lldpTlvEnd},
	))
	got := waitLLDPNeighbors(t, f.m, 1)
	if got[0].ChassisID != "aa:bb:cc:dd:ee:ff" || got[0].PortID != "Gi0/9" || got[0].TTL != 60 {
		t.Fatalf("收到的广告应建邻居（ID 与 VPP 侧同一渲染器）：%+v", got[0])
	}

	// 链路噪声：ARP 帧与截断帧 ⇒ 计数、丢弃、不断循环。
	arp := lldpTestFrame()
	arp[12], arp[13] = 0x08, 0x06
	s.feed(arp)
	s.feed([]byte{0x01, 0x02, 0x03})
	waitLLDPMalformed(t, f.m, 2)

	// 收包错误：如实记录该口未收敛（不静默失聪），小睡后重试。
	s.failRecv(errors.New("网卡收包失败（注入）"))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := lldpStateOf(f.m, "eth0"); ok && !st.Converged {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st, _ := lldpStateOf(f.m, "eth0"); st.Converged {
		t.Fatalf("收包失败应如实报该口未收敛：%+v", st)
	}
	// 恢复收帧（新对端）⇒ 邻居增加、该口转回就绪（错误清空）。
	s.feed(lldpTestFrame(
		lldpTestTLV{typ: lldpTlvChassisID, value: lldpTestIDTLV(lldpChassisSubtypeLocallyAssigned, []byte("sw-again"))},
		lldpTestTLV{typ: lldpTlvPortID, value: lldpTestIDTLV(lldpPortSubtypeInterfaceName, []byte("p1"))},
		lldpTestTLV{typ: lldpTlvTTL, value: []byte{0x00, 0x1e}},
		lldpTestTLV{typ: lldpTlvEnd},
	))
	waitLLDPNeighbors(t, f.m, 2)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := lldpStateOf(f.m, "eth0"); ok && st.Converged {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	st, _ := lldpStateOf(f.m, "eth0")
	t.Fatalf("恢复收帧后该口应转回就绪：%+v", st)
}

// ---------- ④ Sync 开关收敛 ----------

func TestLLDPSyncSwitchLifecycle(t *testing.T) {
	f := newLLDPFixture(t)
	ctx := context.Background()

	// 未声明 / 全局关闭：一个口都不开——哪怕接口条目标了启用（判据是全局开关）。
	if err := f.m.Sync(ctx, nil); err != nil {
		t.Fatalf("未声明应收敛为空操作：%v", err)
	}
	off := &model.LldpConfig{Enabled: false, AdvertisementInterval: 10,
		Interfaces: []model.LldpInterface{{Interface: "eth0", Enabled: true}}}
	if err := f.m.Sync(ctx, off); err != nil {
		t.Fatalf("全局关闭应收敛为空操作：%v", err)
	}
	if got := f.lay.openKeys(); len(got) != 0 {
		t.Fatalf("未启用不该开任何口：%v", got)
	}

	// 启用：只开 Enabled=true 的口（eth1 显式关、eth3 未声明），并**立即各发一帧**。
	cfg := &model.LldpConfig{Enabled: true, AdvertisementInterval: 30, Interfaces: []model.LldpInterface{
		{Interface: "eth0", Enabled: true},
		{Interface: "eth1", Enabled: false},
		{Interface: "eth2", Enabled: true},
		{Interface: "eth3", Enabled: false},
	}}
	if err := f.m.Sync(ctx, cfg); err != nil {
		t.Fatalf("启用应收敛成功：%v", err)
	}
	if got := f.lay.openKeys(); !reflect.DeepEqual(got, []string{"eth0", "eth2"}) {
		t.Fatalf("只应开 Enabled=true 的口（按声明序）：%v", got)
	}
	if f.lay.socket("eth1") != nil || f.lay.socket("eth3") != nil {
		t.Fatal("显式关掉的口不该开 socket")
	}
	s0, s2 := f.lay.socket("eth0"), f.lay.socket("eth2")
	frame0 := waitLLDPSend(t, s0, 1)[0]
	if got := lldpTestTTLOf(t, frame0); got != 120 {
		t.Fatalf("启用即发的首帧 TTL 应＝4×30：%d", got)
	}
	if !bytes.Equal(frame0[6:12], s0.mac) {
		t.Fatalf("源 MAC 应＝该口 MAC：%x want %s", frame0[6:12], s0.mac)
	}
	waitLLDPSend(t, s2, 1)
	// State 按声明序报就绪与 MAC。
	st := f.m.State()
	if len(st) != 2 || st[0].Name != "eth0" || st[1].Name != "eth2" ||
		!st[0].Converged || !st[1].Converged || st[0].MAC != s0.mac.String() {
		t.Fatalf("State 应按声明序报就绪与 MAC：%+v", st)
	}

	// 幂等：同一份声明再收敛不重开、不换 socket。
	opensBefore := len(f.lay.openKeys())
	if err := f.m.Sync(ctx, cfg); err != nil {
		t.Fatalf("幂等收敛失败：%v", err)
	}
	if got := len(f.lay.openKeys()); got != opensBefore {
		t.Fatalf("声明未变不该重开 socket：%v", f.lay.openKeys())
	}
	if f.lay.socket("eth0") != s0 {
		t.Fatal("声明未变不该换 socket")
	}

	// 间隔变更 ⇒ **不重建 socket**（Open 次数不变），下一次发送的 TTL 立刻是新间隔的 4 倍。
	if err := f.m.Sync(ctx, lldpCfg(10, "eth0", "eth2")); err != nil {
		t.Fatalf("间隔变更收敛失败：%v", err)
	}
	if got := len(f.lay.openKeys()); got != opensBefore {
		t.Fatalf("间隔变更不该重建 socket：%v", f.lay.openKeys())
	}
	if f.lay.socket("eth0") != s0 {
		t.Fatal("间隔变更不该换 socket")
	}
	before := len(s0.sent())
	if !f.m.sendAdvertisement(f.rt("eth0")) {
		t.Fatal("广告发送应成功")
	}
	sent := waitLLDPSend(t, s0, before+1)
	if got := lldpTestTTLOf(t, sent[len(sent)-1]); got != 4*10 {
		t.Fatalf("间隔变更后 TTL 应＝4×10：%d", got)
	}

	// 两个口都先建邻居，再关掉 eth2：该口 socket 关闭、收发协程退出、邻居条目作废；
	// eth0 与其邻居保持不动。
	peer := func(chassis, port string) []byte {
		return lldpTestFrame(
			lldpTestTLV{typ: lldpTlvChassisID, value: lldpTestIDTLV(lldpChassisSubtypeLocallyAssigned, []byte(chassis))},
			lldpTestTLV{typ: lldpTlvPortID, value: lldpTestIDTLV(lldpPortSubtypeInterfaceName, []byte(port))},
			lldpTestTLV{typ: lldpTlvTTL, value: []byte{0x00, 0x78}},
			lldpTestTLV{typ: lldpTlvEnd},
		)
	}
	s0.feed(peer("sw-a", "p1"))
	s2.feed(peer("sw-b", "p1"))
	waitLLDPNeighbors(t, f.m, 2)
	one := &model.LldpConfig{Enabled: true, AdvertisementInterval: 10, Interfaces: []model.LldpInterface{
		{Interface: "eth0", Enabled: true},
		{Interface: "eth2", Enabled: false},
	}}
	if err := f.m.Sync(ctx, one); err != nil {
		t.Fatalf("关掉单口收敛失败：%v", err)
	}
	if !s2.closed() || s2.closeCount() != 1 {
		t.Fatal("关掉的口应关闭其 socket（一次）")
	}
	if s2.recvClosed.Load() == 0 {
		t.Fatal("关掉的口应等收包协程退出（Recv 以 net.ErrClosed 返回）")
	}
	if s0.closed() {
		t.Fatal("仍启用的口不该被关")
	}
	if got := f.m.Neighbors(time.Now()); len(got) != 1 || got[0].ChassisID != "sw-a" {
		t.Fatalf("关掉的口的邻居条目应作废、另一口的保持：%+v", got)
	}
	if st := f.m.State(); len(st) != 1 || st[0].Name != "eth0" {
		t.Fatalf("State 应只剩仍启用的口：%+v", st)
	}

	// 全局关 ⇒ 全部关闭、邻居表清空、State 清空。
	if err := f.m.Sync(ctx, nil); err != nil {
		t.Fatalf("全局关收敛失败：%v", err)
	}
	if !s0.closed() {
		t.Fatal("全局关应答关闭全部 socket")
	}
	if got := f.m.Neighbors(time.Now()); len(got) != 0 {
		t.Fatalf("全局关应清空邻居表：%+v", got)
	}
	if st := f.m.State(); len(st) != 0 {
		t.Fatalf("全局关后 State 应为空：%+v", st)
	}
}

// ---------- ⑤ 起不来如实报错 + 恢复后重试 ----------

func TestLLDPSyncOpenFailureIsHonestAndRetries(t *testing.T) {
	f := newLLDPFixture(t)
	ctx := context.Background()
	f.lay.setFail("eth-bad", errors.New("内核接口不存在（注入）"))
	err := f.m.Sync(ctx, lldpCfg(30, "eth-bad", "eth-good"))
	if err == nil {
		t.Fatal("一个口起不来必须如实报错（不得静默）")
	}
	if !strings.Contains(err.Error(), "eth-bad") {
		t.Fatalf("错误应点名接口：%v", err)
	}
	// 另一个口照常收敛：socket 在位且已发广告。
	sg := f.lay.socket("eth-good")
	if sg == nil || sg.closed() {
		t.Fatal("一条声明失败不该拖累其它接口")
	}
	waitLLDPSend(t, sg, 1)
	// State：坏口如实未收敛 + 原因；好口就绪。
	bad, ok := lldpStateOf(f.m, "eth-bad")
	if !ok || bad.Converged || !strings.Contains(bad.Error, "不存在") {
		t.Fatalf("State 应如实报坏口未收敛：%+v ok=%v", bad, ok)
	}
	if good, _ := lldpStateOf(f.m, "eth-good"); !good.Converged {
		t.Fatalf("好口应就绪：%+v", good)
	}
	// 底座恢复 ⇒ 同一声明重试即收敛（只为坏口补开一次）。
	opensBefore := len(f.lay.openKeys())
	f.lay.setFail("eth-bad", nil)
	if err := f.m.Sync(ctx, lldpCfg(30, "eth-bad", "eth-good")); err != nil {
		t.Fatalf("底座恢复后应重试收敛成功：%v", err)
	}
	if got := len(f.lay.openKeys()); got != opensBefore+1 {
		t.Fatalf("恢复后应为坏口补开一次 socket：%v", f.lay.openKeys())
	}
	if st, _ := lldpStateOf(f.m, "eth-bad"); !st.Converged {
		t.Fatalf("恢复后坏口应就绪：%+v", st)
	}
	// 幂等：两个口都在位，再收敛不重开。
	opens := len(f.lay.openKeys())
	if err := f.m.Sync(ctx, lldpCfg(30, "eth-bad", "eth-good")); err != nil {
		t.Fatalf("幂等收敛失败：%v", err)
	}
	if got := len(f.lay.openKeys()); got != opens {
		t.Fatalf("已在位的口不该重开：%v", f.lay.openKeys())
	}
}

// ---------- ⑥ 排序与 LastHeard ----------

func TestLLDPNeighborsDeterministicOrderAndLastHeard(t *testing.T) {
	f := newLLDPFixture(t)
	base := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	// 乱序加入四条（含同一本地口的不同对端）。
	f.m.hear("eth1", lldpTestTlvs("sw-b", "p1", 30), base.Add(4*time.Second))
	f.m.hear("eth0", lldpTestTlvs("sw-x", "p2", 30), base.Add(1*time.Second))
	f.m.hear("eth0", lldpTestTlvs("sw-x", "p1", 30), base)
	f.m.hear("eth0", lldpTestTlvs("sw-a", "p9", 30), base.Add(2*time.Second))
	want := []network.LldpNeighbor{
		{Interface: "eth0", ChassisID: "sw-a", PortID: "p9", TTL: 30, LastHeard: 3},
		{Interface: "eth0", ChassisID: "sw-x", PortID: "p1", TTL: 30, LastHeard: 5},
		{Interface: "eth0", ChassisID: "sw-x", PortID: "p2", TTL: 30, LastHeard: 4},
		{Interface: "eth1", ChassisID: "sw-b", PortID: "p1", TTL: 30, LastHeard: 1},
	}
	got := f.m.Neighbors(base.Add(5 * time.Second))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("邻居表应按〔接口、chassis、port〕确定性排序：\n got %+v\nwant %+v", got, want)
	}
	// 重复取快照结果一致（不受 map 迭代顺序影响）。
	if again := f.m.Neighbors(base.Add(5 * time.Second)); !reflect.DeepEqual(again, want) {
		t.Fatalf("同一时刻两次快照应一致：%+v", again)
	}
}

// ---------- ⑦ Close ----------

func TestLLDPCloseStopsGoroutinesAndIsIdempotent(t *testing.T) {
	f := newLLDPFixture(t)
	ctx := context.Background()
	if err := f.m.Sync(ctx, lldpCfg(30, "eth0", "eth2")); err != nil {
		t.Fatalf("收敛失败：%v", err)
	}
	s0, s2 := f.lay.socket("eth0"), f.lay.socket("eth2")
	if err := f.m.Close(); err != nil {
		t.Fatalf("Close 应正常返回（收发协程应在期限内退出）：%v", err)
	}
	if !s0.closed() || !s2.closed() {
		t.Fatal("Close 应关闭全部 socket")
	}
	if s0.recvClosed.Load() == 0 || s2.recvClosed.Load() == 0 {
		t.Fatal("Close 返回前收包协程必须已退出（Recv 以 net.ErrClosed 返回）")
	}
	if s0.closeCount() != 1 || s2.closeCount() != 1 {
		t.Fatalf("Close 应只关一次（幂等）：%d/%d", s0.closeCount(), s2.closeCount())
	}
	if st := f.m.State(); len(st) != 0 {
		t.Fatalf("Close 后不该再有接口运行态：%+v", st)
	}
	if err := f.m.Close(); err != nil {
		t.Fatalf("Close 应幂等：%v", err)
	}
}

// ---------- ⑧ Provider 接线 ----------

func TestLLDPProviderWiringReadViewReplayAndClose(t *testing.T) {
	p := New(&fakeRunner{})
	lay := newLLDPFakeLayer()
	p.SetLLDPLayer(lay)
	ctx := context.Background()

	// 未装配：邻居读视图是**空表 + nil**（内核数据面支持 LLDP；「没装配」≠「能力缺失」）。
	rows, err := p.LldpNeighbors(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("未装配时邻居读视图应为空表且不报错：%v / %v", rows, err)
	}
	// 未声明且从未装配：巡检空操作（不因巡检常驻构造管理器）。
	if errs := p.ReconcileLLDP(ctx, model.Config{}); len(errs) != 0 {
		t.Fatalf("未声明且未装配时巡检应空操作：%v", errs)
	}
	if p.lldp != nil {
		t.Fatal("未声明且从未装配时不该构造管理器")
	}

	// 声明并启用 ⇒ 收敛到注入的底座；读视图经 Provider 读到邻居（三面同源的服务端一侧）。
	cfg := lldpCfg(30, "eth0")
	if err := p.ApplyLLDP(ctx, cfg); err != nil {
		t.Fatalf("收敛失败：%v", err)
	}
	s := lay.socket("eth0")
	if s == nil {
		t.Fatal("应打开 eth0 的收发 socket")
	}
	waitLLDPSend(t, s, 1)
	s.feed(lldpTestFrame(
		lldpTestTLV{typ: lldpTlvChassisID, value: lldpTestIDTLV(lldpChassisSubtypeLocallyAssigned, []byte("sw-peer"))},
		lldpTestTLV{typ: lldpTlvPortID, value: lldpTestIDTLV(lldpPortSubtypeInterfaceName, []byte("Gi1/0"))},
		lldpTestTLV{typ: lldpTlvTTL, value: []byte{0x00, 0x78}},
		lldpTestTLV{typ: lldpTlvEnd},
	))
	waitLLDPNeighbors(t, p.lldp, 1)
	rows, err = p.LldpNeighbors(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("读视图应给出邻居：%v / %v", rows, err)
	}
	if rows[0].Interface != "eth0" || rows[0].ChassisID != "sw-peer" || rows[0].PortID != "Gi1/0" || rows[0].TTL != 120 {
		t.Fatalf("读视图条目不符：%+v", rows[0])
	}

	// 恢复重放必须含 LLDP（socket/协程活在进程内）：EnsureConsistent 后仍是同一个 socket（幂等）。
	rcfg := model.Config{Protocols: &model.ProtocolsConfig{LLDP: lldpCfg(30, "eth0")}}
	_ = p.EnsureConsistent(ctx, rcfg)
	if got := lay.openKeys(); len(got) != 1 {
		t.Fatalf("恢复重放应幂等复用同一 socket（不重开）：%v", got)
	}
	if lay.socket("eth0") != s {
		t.Fatal("恢复重放不该换 socket")
	}

	// 未声明（提交编排的 undo/删除路径）⇒ teardown。
	if err := p.ApplyLLDP(ctx, nil); err != nil {
		t.Fatalf("未声明应收敛为空操作：%v", err)
	}
	if !s.closed() {
		t.Fatal("未声明应关闭 socket")
	}
	// 巡检在已装配过之后走 Sync（把带外残留收干净）：空声明 ⇒ 无未收敛项。
	if errs := p.ReconcileLLDP(ctx, model.Config{}); len(errs) != 0 {
		t.Fatalf("装配过后巡检空声明应无未收敛项：%v", errs)
	}

	// 再次启用后 Provider.Close 应带上 LLDP（进程退出路径：socket 关、协程退出）。
	if err := p.ApplyLLDP(ctx, cfg); err != nil {
		t.Fatalf("再收敛失败：%v", err)
	}
	s2 := lay.socket("eth0")
	if s2 == nil || s2 == s {
		t.Fatal("teardown 后应重开 socket")
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Provider.Close 应正常返回：%v", err)
	}
	if !s2.closed() || s2.recvClosed.Load() == 0 {
		t.Fatal("Provider.Close 应关闭 LLDP 的 socket 并等收包协程退出")
	}
}
