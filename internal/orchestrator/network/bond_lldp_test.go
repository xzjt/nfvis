package network

import (
	"context"
	"errors"
	"strconv"
	"strings"
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
	seq     []string            // 调用顺序（"detach:<idx>" / "delete:<idx>"）
	bonds   []BondRuntime       // 数据面现存 bond（Bonds 返回）
	members map[uint32][]uint32 // bond sw_if_index → 成员 sw_if_index
	err     error
	memErr  error // 仅成员枚举失败（Bonds 正常）
}

func newFakeBond() *fakeBond {
	return &fakeBond{ifaces: map[string]uint32{"ens192": 1, "ens224": 2}, next: 100,
		names: map[uint32]string{}, mtu: map[uint32]uint32{}, state: map[uint32]bool{},
		members: map[uint32][]uint32{}}
}

// addBond 预置数据面现存 bond（撤销收敛用例）。
func (f *fakeBond) addBond(idx uint32, name string, members ...uint32) {
	f.bonds = append(f.bonds, BondRuntime{SwIfIndex: idx, Name: name})
	f.members[idx] = members
	f.names[idx] = name
}

func (f *fakeBond) Bonds() ([]BondRuntime, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.bonds, nil
}

func (f *fakeBond) BondMembers(bondSwIfIndex uint32) ([]uint32, error) {
	if f.memErr != nil {
		return nil, f.memErr
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.members[bondSwIfIndex], nil
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
	f.seq = append(f.seq, "detach:"+u32str(memberSwIfIndex))
	return nil
}

func (f *fakeBond) BondDelete(bondSwIfIndex uint32) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, bondSwIfIndex)
	f.seq = append(f.seq, "delete:"+u32str(bondSwIfIndex))
	// 与 VPP 一致：bond 接口消失后不再出现在现存 bond 列表
	kept := f.bonds[:0]
	for _, b := range f.bonds {
		if b.SwIfIndex != bondSwIfIndex {
			kept = append(kept, b)
		}
	}
	f.bonds = kept
	delete(f.members, bondSwIfIndex)
	delete(f.names, bondSwIfIndex)
	return nil
}

func u32str(v uint32) string { return strconv.FormatUint(uint64(v), 10) }

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
	if err := p.DeleteBond(context.Background(), "bond0"); err != nil {
		t.Fatalf("DeleteBond: %v", err)
	}
	if len(f.deleted) != 2 {
		t.Fatalf("应删除 bond: %v", f.deleted)
	}
	if err := p.DeleteBond(context.Background(), "nope"); err != nil {
		t.Fatalf("删除不存在应无害: %v", err)
	}
}

// ---------- 撤销收敛：声明集之外的 bond 必须被拆除 ----------

// 声明仍在的 bond 不被动；未声明的 bond 先摘成员再删接口（顺序不可颠倒）。
func TestPruneBondsRemovesUndeclaredOnly(t *testing.T) {
	f := newFakeBond()
	f.addBond(101, "bond0", 1) // 仍在声明中
	f.addBond(102, "bond9", 2) // 配置已不再声明
	p := NewBondProvider(f)

	declared := []model.Bond{{Name: "bond0", Members: []string{"ens192"}}}
	if errs := p.PruneBonds(context.Background(), declared); len(errs) != 0 {
		t.Fatalf("拆除应成功: %v", errs)
	}
	if len(f.deleted) != 1 || f.deleted[0] != 102 {
		t.Fatalf("只应删除未声明的 bond9(102): %v", f.deleted)
	}
	if len(f.detach) != 1 || f.detach[0] != 2 {
		t.Fatalf("应摘除 bond9 的成员 ens224(2): %v", f.detach)
	}
	want := []string{"detach:2", "delete:102"} // 先摘成员、再删接口
	if len(f.seq) != len(want) {
		t.Fatalf("调用序列不符: %v", f.seq)
	}
	for i, w := range want {
		if f.seq[i] != w {
			t.Fatalf("第 %d 步应为 %s，实际 %s（全部: %v）", i, w, f.seq[i], f.seq)
		}
	}
	// 登记表里的 bond0 未被触碰（再次收敛不会重建它）
	if errs := p.PruneBonds(context.Background(), declared); len(errs) != 0 {
		t.Fatalf("重复收敛应成功: %v", errs)
	}
	if len(f.deleted) != 1 {
		t.Fatalf("重复收敛不应再删（含声明中的 bond）: %v", f.deleted)
	}
}

// 无残留 bond 时收敛不产生任何调用（避免误删与噪声）。
func TestPruneBondsNoopWhenNothingStale(t *testing.T) {
	f := newFakeBond()
	f.addBond(101, "bond0", 1)
	p := NewBondProvider(f)
	if errs := p.PruneBonds(context.Background(), []model.Bond{{Name: "bond0"}}); len(errs) != 0 {
		t.Fatalf("应收敛成功: %v", errs)
	}
	if len(f.detach) != 0 || len(f.deleted) != 0 {
		t.Fatalf("声明中的 bond 不应被触碰: detach=%v deleted=%v", f.detach, f.deleted)
	}
}

// 数据面查询/成员枚举失败按未收敛上报，不静默吞掉。
func TestPruneBondsReportsErrors(t *testing.T) {
	fe := newFakeBond()
	fe.err = errors.New("boom")
	if errs := NewBondProvider(fe).PruneBonds(context.Background(), nil); len(errs) != 1 {
		t.Fatalf("查询失败应上报: %v", errs)
	}

	// 成员枚举失败：该项报错且不冒进删除（状态未知）
	f := newFakeBond()
	f.addBond(102, "bond9", 2)
	p := NewBondProvider(f)
	f.memErr = errors.New("dump failed")
	errs := p.PruneBonds(context.Background(), nil)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "bond9") {
		t.Fatalf("成员枚举失败应上报且指明对象: %v", errs)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("状态未知时不应删除: %v", f.deleted)
	}
}

// nfvisd 重启后登记表为空：按接口名反查数据面后仍能删除（成员以数据面为准）。
func TestDeleteBondFallsBackToDataplane(t *testing.T) {
	f := newFakeBond()
	f.addBond(101, "bond0", 1, 2)
	p := NewBondProvider(f) // 未经过 ApplyBond：登记表为空
	if err := p.DeleteBond(context.Background(), "bond0"); err != nil {
		t.Fatalf("DeleteBond: %v", err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != 101 {
		t.Fatalf("应按名反查后删除: %v", f.deleted)
	}
	if len(f.detach) != 2 || f.detach[0] != 1 || f.detach[1] != 2 {
		t.Fatalf("应摘除数据面上的全部成员: %v", f.detach)
	}
	if f.seq[0] != "detach:1" || f.seq[len(f.seq)-1] != "delete:101" {
		t.Fatalf("应先摘成员再删接口: %v", f.seq)
	}
	// 二次删除幂等
	if err := p.DeleteBond(context.Background(), "bond0"); err != nil {
		t.Fatalf("重复删除应无害: %v", err)
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
