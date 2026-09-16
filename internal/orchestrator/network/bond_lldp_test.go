package network

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// ---------- M3-6：bond 与 LLDP（假客户端） ----------

type fakeBond struct {
	ifaces  map[string]uint32
	next    uint32
	created []bool // 每次创建的 lacp 标志
	names   map[uint32]string
	added   []string // "bondIdx:memberIdx:passive"
	detach  []uint32
	deleted []uint32
	mtu     map[uint32]uint32
	state   map[uint32]bool
	err     error
}

func newFakeBond() *fakeBond {
	return &fakeBond{ifaces: map[string]uint32{"ens192": 1, "ens224": 2}, next: 100,
		names: map[uint32]string{}, mtu: map[uint32]uint32{}, state: map[uint32]bool{}}
}

func (f *fakeBond) Close() {}

func (f *fakeBond) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	if idx, ok := f.ifaces[ifname]; ok {
		return idx, true, nil
	}
	for idx, n := range f.names {
		if n == ifname {
			return idx, true, nil
		}
	}
	return 0, false, nil
}

func (f *fakeBond) BondCreate(lacp bool) (uint32, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.next++
	f.created = append(f.created, lacp)
	return f.next, nil
}

func (f *fakeBond) SetInterfaceName(swIfIndex uint32, name string) error {
	if f.err != nil {
		return f.err
	}
	f.names[swIfIndex] = name
	return nil
}

func (f *fakeBond) BondAddMember(bondSwIfIndex, memberSwIfIndex uint32, passive bool) error {
	if f.err != nil {
		return f.err
	}
	f.added = append(f.added, string(rune('0'+bondSwIfIndex-100))+":"+string(rune('0'+memberSwIfIndex))+":"+boolStr(passive))
	return nil
}

func (f *fakeBond) BondDetachMember(memberSwIfIndex uint32) error {
	if f.err != nil {
		return f.err
	}
	f.detach = append(f.detach, memberSwIfIndex)
	return nil
}

func (f *fakeBond) BondDelete(bondSwIfIndex uint32) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, bondSwIfIndex)
	return nil
}

func (f *fakeBond) SetState(swIfIndex uint32, up bool) error {
	if f.err != nil {
		return f.err
	}
	f.state[swIfIndex] = up
	return nil
}

func (f *fakeBond) SetMTU(swIfIndex, mtu uint32) error {
	if f.err != nil {
		return f.err
	}
	f.mtu[swIfIndex] = mtu
	return nil
}

func boolStr(b bool) string {
	if b {
		return "passive"
	}
	return "active"
}

func TestBondStaticAndLacp(t *testing.T) {
	f := newFakeBond()
	p := NewBondProvider(f)
	static := model.Bond{Name: "bond0", Members: []string{"ens192", "ens224"}, MTU: 9000}
	if err := p.ApplyBond(context.Background(), static); err != nil {
		t.Fatalf("ApplyBond(static): %v", err)
	}
	if len(f.created) != 1 || f.created[0] {
		t.Fatalf("静态聚合应 lacp=false: %v", f.created)
	}
	if f.names[101] != "bond0" {
		t.Fatalf("bond 接口应改名为 bond0: %v", f.names)
	}
	if len(f.added) != 2 || !strings.HasSuffix(f.added[0], ":active") {
		t.Fatalf("成员添加不符: %v", f.added)
	}
	if f.mtu[101] != 9000 || !f.state[101] {
		t.Fatalf("MTU/状态: %v %v", f.mtu, f.state)
	}

	// 幂等
	f.added = nil
	if err := p.ApplyBond(context.Background(), static); err != nil {
		t.Fatalf("重复 ApplyBond: %v", err)
	}
	if len(f.added) != 0 {
		t.Fatalf("幂等重放不应重复加成员: %v", f.added)
	}

	// 减少成员 → 摘除 ens224(idx2)
	f.detach = nil
	static.Members = []string{"ens192"}
	if err := p.ApplyBond(context.Background(), static); err != nil {
		t.Fatalf("缩减成员: %v", err)
	}
	if len(f.detach) != 1 || f.detach[0] != 2 {
		t.Fatalf("应摘除 ens224: %v", f.detach)
	}

	// 切 LACP passive → 重建 + 成员被动
	lacp := model.Bond{Name: "bond0", Members: []string{"ens192"}, Lacp: &model.Lacp{Mode: "passive"}}
	f.added, f.deleted = nil, nil
	if err := p.ApplyBond(context.Background(), lacp); err != nil {
		t.Fatalf("切 LACP: %v", err)
	}
	if len(f.deleted) != 1 {
		t.Fatalf("模式变更应重建（删除旧实例）: %v", f.deleted)
	}
	if len(f.created) != 2 || !f.created[1] {
		t.Fatalf("应创建 LACP bond: %v", f.created)
	}
	if len(f.added) != 1 || !strings.HasSuffix(f.added[0], ":passive") {
		t.Fatalf("被动模式成员标记: %v", f.added)
	}

	// 删除
	f.detach = nil
	if err := p.DeleteBond(context.Background(), "bond0"); err != nil {
		t.Fatalf("DeleteBond: %v", err)
	}
	if len(f.deleted) != 2 {
		t.Fatalf("应删除 bond: %v", f.deleted)
	}
	// 删除前必须先摘除成员（旧实现先清登记，成员永远读成空列表）
	if len(f.detach) != 1 || f.detach[0] != 1 {
		t.Fatalf("删除前应摘除成员 ens192(idx 1): %v", f.detach)
	}
	if err := p.DeleteBond(context.Background(), "nope"); err != nil {
		t.Fatalf("删除不存在应无害: %v", err)
	}
}

func TestBondErrors(t *testing.T) {
	f := newFakeBond()
	p := NewBondProvider(f)
	if err := p.ApplyBond(context.Background(), model.Bond{Name: "b"}); err == nil {
		t.Fatalf("无成员应报错")
	}
	if err := p.ApplyBond(context.Background(), model.Bond{Name: "b", Members: []string{"ens999"}}); err == nil ||
		!strings.Contains(err.Error(), "DPDK") {
		t.Fatalf("成员缺失应提示 DPDK: %v", err)
	}
	fe := newFakeBond()
	fe.err = errors.New("boom")
	if err := NewBondProvider(fe).ApplyBond(context.Background(),
		model.Bond{Name: "b", Members: []string{"ens192"}}); err == nil {
		t.Fatalf("客户端错误应上抛")
	}
}

type fakeLldp struct {
	config   []string
	ifaces   []string // "iface:on|off"
	neigh    []LldpNeighbor
	err      error
	requests int
}

func (f *fakeLldp) Close() {}

func (f *fakeLldp) LldpConfig(txInterval, txHold uint32, systemName string) error {
	if f.err != nil {
		return f.err
	}
	f.config = append(f.config, string(rune('0'+txInterval)))
	return nil
}

func (f *fakeLldp) LldpSetInterface(ifname string, enable bool) error {
	if f.err != nil {
		return f.err
	}
	state := "off"
	if enable {
		state = "on"
	}
	f.ifaces = append(f.ifaces, ifname+":"+state)
	return nil
}

func (f *fakeLldp) LldpNeighbors() ([]LldpNeighbor, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.neigh, nil
}

func TestLldpApplyAndDisable(t *testing.T) {
	f := &fakeLldp{}
	p := NewLldpProvider(f)
	cfg := &model.LldpConfig{Enabled: true, AdvertisementInterval: 30,
		Interfaces: []model.LldpInterface{{Interface: "ens192", Enabled: true}, {Interface: "ens224", Enabled: false}}}
	if err := p.ApplyLLDP(context.Background(), cfg); err != nil {
		t.Fatalf("ApplyLLDP: %v", err)
	}
	if len(f.config) != 1 || len(f.ifaces) != 2 {
		t.Fatalf("全局/接口配置: %v %v", f.config, f.ifaces)
	}
	joined := strings.Join(f.ifaces, ",")
	if !strings.Contains(joined, "ens192:on") || !strings.Contains(joined, "ens224:off") {
		t.Fatalf("接口开关不符: %v", f.ifaces)
	}
	// 全局关闭 → 已启用接口被关闭
	f.ifaces = nil
	if err := p.ApplyLLDP(context.Background(), &model.LldpConfig{Enabled: false}); err != nil {
		t.Fatalf("关闭 LLDP: %v", err)
	}
	if len(f.ifaces) != 1 || f.ifaces[0] != "ens192:off" {
		t.Fatalf("应关闭曾启用的接口: %v", f.ifaces)
	}
	// nil 配置亦关闭
	f.ifaces = nil
	if err := p.ApplyLLDP(context.Background(), nil); err != nil {
		t.Fatalf("nil 配置: %v", err)
	}
	if len(f.ifaces) != 0 && f.ifaces[0] != "ens192:off" {
		t.Fatalf("nil 应关闭: %v", f.ifaces)
	}
	// 邻居运行态与标识解码
	f2 := &fakeLldp{neigh: []LldpNeighbor{{Interface: "ens192", ChassisID: "sw1", PortID: "Gi0/1", TTL: 120}}}
	rows, err := NewLldpProvider(f2).Neighbors(context.Background())
	if err != nil || len(rows) != 1 || rows[0].ChassisID != "sw1" {
		t.Fatalf("Neighbors: %v %+v", err, rows)
	}
	// 错误传播
	if err := NewLldpProvider(&fakeLldp{err: errors.New("boom")}).ApplyLLDP(context.Background(),
		&model.LldpConfig{Enabled: true}); err == nil {
		t.Fatalf("客户端错误应上抛")
	}
	if !strings.HasSuffix(lldpIDString([]byte("sw1\x00\x00")), "sw1") {
		t.Fatalf("标识解码应去尾部 NUL")
	}
}

// ---------- V1 收尾（决策 #70）：LLDP 标识须按 subtype 解码 ----------
//
// 此前把 ChassisID/PortID 原始字节直接当字符串（仅去尾部 NUL），忽略了 subtype。
// 真实交换机绝大多数用 MAC 型 chassis-ID（subtype 4，6 字节二进制），
// 旧实现会输出乱码（且 JSON 序列化会把非法 UTF-8 变成 U+FFFD）。

func TestLldpIDBySubtype(t *testing.T) {
	mac := []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}

	// chassis MAC = subtype 4
	if got := lldpIDBySubtype(4, 4, mac); got != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("chassis MAC 型标识应格式化为 MAC，得到 %q", got)
	}
	// port MAC = subtype 3（编号与 chassis 不同）
	if got := lldpIDBySubtype(3, 3, mac); got != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("port MAC 型标识应格式化为 MAC，得到 %q", got)
	}
	// 文本型（interface name，subtype 6）仍按字符串输出并去尾部 NUL
	if got := lldpIDBySubtype(6, 4, []byte("sw1\x00\x00")); got != "sw1" {
		t.Fatalf("文本型标识解码错误: %q", got)
	}
	// 二进制但非 MAC（如 network address，subtype 5）→ 十六进制，不得输出乱码
	if got := lldpIDBySubtype(5, 4, []byte{0x0a, 0x00, 0x00, 0x01}); got != "0x0a000001" {
		t.Fatalf("非 MAC 二进制标识应为十六进制: %q", got)
	}
	// 空标识
	if got := lldpIDBySubtype(4, 4, nil); got != "" {
		t.Fatalf("空标识应为空串: %q", got)
	}
	// 声明为 MAC 型但长度不是 6 → 回退为十六进制（不猜测）
	if got := lldpIDBySubtype(4, 4, []byte{0xaa, 0xbb}); got != "0xaabb" {
		t.Fatalf("长度不符的 MAC 型应回退十六进制: %q", got)
	}
}

// 回归：MAC 型标识绝不能按旧实现（裸转字符串）输出不可打印内容。
func TestLldpIDMacNotRawString(t *testing.T) {
	mac := []byte{0x02, 0xfe, 0x83, 0xb5, 0x2e, 0x5e}
	got := lldpIDBySubtype(4, 4, mac)
	for _, r := range got {
		if r < 0x20 || r > 0x7e {
			t.Fatalf("输出含不可打印字符（旧缺陷复现）: %q", got)
		}
	}
	if got != "02:fe:83:b5:2e:5e" {
		t.Fatalf("MAC 格式化错误: %q", got)
	}
}

// ---------- bond 并发回归 ----------

// syncFakeBond 线程安全假客户端（并发回归测试专用）。
type syncFakeBond struct {
	mu     sync.Mutex
	ifaces map[string]uint32
	next   uint32
}

func newSyncFakeBond() *syncFakeBond {
	return &syncFakeBond{ifaces: map[string]uint32{"ens192": 1, "ens224": 2}, next: 100}
}

func (f *syncFakeBond) Close() {}

func (f *syncFakeBond) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx, ok := f.ifaces[ifname]
	return idx, ok, nil
}

func (f *syncFakeBond) BondCreate(lacp bool) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	return f.next, nil
}

func (f *syncFakeBond) SetInterfaceName(swIfIndex uint32, name string) error { return nil }

func (f *syncFakeBond) BondAddMember(bondSwIfIndex, memberSwIfIndex uint32, passive bool) error {
	return nil
}

func (f *syncFakeBond) BondDetachMember(memberSwIfIndex uint32) error { return nil }

func (f *syncFakeBond) BondDelete(swIfIndex uint32) error { return nil }

func (f *syncFakeBond) SetState(swIfIndex uint32, up bool) error { return nil }

func (f *syncFakeBond) SetMTU(swIfIndex, mtu uint32) error { return nil }

// 并发回归：模式重建路径的 map 删除曾在锁外执行，与并发 DeleteBond 构成
// 并发 map 读写——Go 运行时直接 fatal 崩溃整个 nfvisd（-race 必报）。
func TestBondConcurrentApplyDelete(t *testing.T) {
	f := newSyncFakeBond()
	p := NewBondProvider(f)
	lacp := model.Bond{Name: "bond0", Members: []string{"ens192"}, Lacp: &model.Lacp{Mode: "active"}}
	if err := p.ApplyBond(context.Background(), lacp); err != nil {
		t.Fatalf("初始 ApplyBond: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			b := lacp
			if n%2 == 0 { // 交替 静态/LACP，持续触发模式重建路径
				b.Lacp = nil
			}
			_ = p.ApplyBond(context.Background(), b)
		}(i)
		go func() {
			defer wg.Done()
			_ = p.DeleteBond(context.Background(), "bond0")
		}()
	}
	wg.Wait()
}
