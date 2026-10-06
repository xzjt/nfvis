package network

// 决策 #385：接口入向风暴抑制的单测（向量构造纯函数 + 建/改/删/重放的调用序 + 读视图）。
//
// 假客户端是一台**小型状态机**（记表/会话/policer/绑定），因此除调用序外还能断言
// 「撤旧之后不留残渣」与「登记丢失（nfvisd 重启）后按数据面实况重建」。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

type fakeStormClient struct {
	calls        []string
	ifidx        map[string]uint32
	tables       map[uint32]StormTableInfo
	sessions     map[uint32]int
	pols         map[string]uint32
	attached     map[uint32]uint32
	nextTable    uint32
	nextPolicer  uint32
	failOn       string // 命中该调用前缀即报错（失败注入）
	setIfaceFail bool
}

func newFakeStormClient() *fakeStormClient {
	return &fakeStormClient{
		ifidx:    map[string]uint32{"ens192": 7},
		tables:   map[uint32]StormTableInfo{},
		sessions: map[uint32]int{},
		pols:     map[string]uint32{},
		attached: map[uint32]uint32{},
	}
}

func (f *fakeStormClient) log(format string, args ...any) error {
	line := fmt.Sprintf(format, args...)
	f.calls = append(f.calls, line)
	if f.failOn != "" && strings.HasPrefix(line, f.failOn) {
		return fmt.Errorf("注入失败: %s", line)
	}
	return nil
}

func (f *fakeStormClient) Close() {}

func (f *fakeStormClient) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	f.calls = append(f.calls, "sw-index:"+ifname)
	if f.setIfaceFail {
		return 0, false, fmt.Errorf("索引查询失败")
	}
	idx, ok := f.ifidx[ifname]
	return idx, ok, nil
}

func (f *fakeStormClient) PolicerAddDel(name string, cirKbps uint32, cb uint64, add bool) (uint32, error) {
	verb := "policer-del"
	if add {
		verb = "policer-add"
	}
	if err := f.log("%s:%s:%d:cb=%d", verb, name, cirKbps, cb); err != nil {
		return 0, err
	}
	if !add {
		delete(f.pols, name) // 不存在即已达成（与适配器同口径）
		return 0, nil
	}
	if _, ok := f.pols[name]; ok {
		return 0, fmt.Errorf("VALUE_EXIST: %s", name) // 适配器不吞：新增时不该存在
	}
	idx := f.nextPolicer
	f.nextPolicer++
	f.pols[name] = idx
	return idx, nil
}

func (f *fakeStormClient) ClassifyAddTable(mask []byte, nextTableIndex uint32) (uint32, error) {
	if err := f.log("table-add:mask=%x:next=%s", mask, unsetU32(nextTableIndex)); err != nil {
		return 0, err
	}
	idx := f.nextTable
	f.nextTable++
	f.tables[idx] = StormTableInfo{Index: idx, Mask: fmt.Sprintf("%x", mask), NextTableIndex: nextTableIndex}
	return idx, nil
}

func (f *fakeStormClient) ClassifyDelTable(tableIndex uint32, delChain bool) error {
	if err := f.log("table-del:%d:chain=%v", tableIndex, delChain); err != nil {
		return err
	}
	ti, ok := f.tables[tableIndex]
	if !ok {
		return nil
	}
	delete(f.tables, tableIndex)
	delete(f.sessions, tableIndex)
	if delChain && ti.NextTableIndex != ^uint32(0) {
		if _, ok := f.tables[ti.NextTableIndex]; ok {
			if err := f.ClassifyDelTable(ti.NextTableIndex, true); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *fakeStormClient) ClassifyAddSession(tableIndex uint32, match []byte, policerIndex uint32) error {
	if err := f.log("session-add:%d:match=%x:policer=%d", tableIndex, match, policerIndex); err != nil {
		return err
	}
	f.sessions[tableIndex]++
	return nil
}

func (f *fakeStormClient) ClassifyDelSession(tableIndex uint32, match []byte) error {
	if err := f.log("session-del:%d:match=%x", tableIndex, match); err != nil {
		return err
	}
	if f.sessions[tableIndex] > 0 {
		f.sessions[tableIndex]--
	}
	return nil
}

func (f *fakeStormClient) PolicerClassifySetInterface(swIfIndex, l2TableIndex uint32, add bool) error {
	verb := "detach"
	if add {
		verb = "attach"
	}
	if err := f.log("%s:if=%d:table=%d", verb, swIfIndex, l2TableIndex); err != nil {
		return err
	}
	if add {
		f.attached[swIfIndex] = l2TableIndex
	} else {
		delete(f.attached, swIfIndex)
	}
	return nil
}

func (f *fakeStormClient) PolicerDump() ([]StormPolicer, error) {
	f.calls = append(f.calls, "policer-dump")
	names := make([]string, 0, len(f.pols))
	for n := range f.pols {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]StormPolicer, 0, len(names))
	for _, n := range names {
		out = append(out, StormPolicer{Name: n}) // dump 实测字段：名字（实测 CIR 由真机给出）
	}
	return out, nil
}

func (f *fakeStormClient) AttachedL2Table(swIfIndex uint32) (uint32, bool, error) {
	f.calls = append(f.calls, "attached:"+fmt.Sprint(swIfIndex))
	t, ok := f.attached[swIfIndex]
	return t, ok, nil
}

func (f *fakeStormClient) ClassifyTableInfo(tableIndex uint32) (StormTableInfo, bool, error) {
	f.calls = append(f.calls, "table-info:"+fmt.Sprint(tableIndex))
	ti, ok := f.tables[tableIndex]
	if !ok {
		return StormTableInfo{}, false, nil
	}
	ti.Sessions = uint32(f.sessions[tableIndex])
	return ti, true, nil
}

func unsetU32(v uint32) string {
	if v == ^uint32(0) {
		return "unset"
	}
	return fmt.Sprint(v)
}

// snapshot 数据面当前状态（断言「不留残渣」用）。
func (f *fakeStormClient) snapshot() string {
	var b strings.Builder
	fmt.Fprintf(&b, "tables=%d sessions=", len(f.tables))
	keys := make([]int, 0, len(f.tables))
	for k := range f.tables {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%d:%d ", k, f.sessions[uint32(k)])
	}
	fmt.Fprintf(&b, "pols=%d", len(f.pols))
	for _, n := range sortedKeys(f.pols) {
		fmt.Fprintf(&b, " %s", n)
	}
	fmt.Fprintf(&b, " attached=%v", f.attached)
	return b.String()
}

func sortedKeys(m map[string]uint32) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------- 向量构造（纯函数） ----------

// TestStormVectorsMaskAndMatch：两类掩码/匹配向量的形状是本实现的**契约形状**
//（真机按 show classify tables 核对）。
func TestStormVectorsMaskAndMatch(t *testing.T) {
	bMask := stormMask(StormKindBroadcast)
	bMatch := stormMatch(StormKindBroadcast)
	if len(bMask) != 16 || len(bMatch) != 16 {
		t.Fatalf("向量须为 16 字节: %d/%d", len(bMask), len(bMatch))
	}
	if !bytes.Equal(bMask[:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) ||
		!bytes.Equal(bMatch[:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) {
		t.Fatalf("广播应精确匹配前 6 字节 ff: mask=%x match=%x", bMask, bMatch)
	}
	for i := 6; i < 16; i++ {
		if bMask[i] != 0 || bMatch[i] != 0 {
			t.Fatalf("广播掩码/匹配的第 %d 字节应为 0: mask=%x match=%x", i, bMask, bMatch)
		}
	}
	mMask := stormMask(StormKindMulticast)
	mMatch := stormMatch(StormKindMulticast)
	if mMask[0] != 0x01 || mMatch[0] != 0x01 {
		t.Fatalf("组播应按「匹配位」写法置 I/G 位: mask=%x match=%x", mMask, mMatch)
	}
	for i := 1; i < 16; i++ {
		if mMask[i] != 0 || mMatch[i] != 0 {
			t.Fatalf("组播掩码/匹配的其余字节应为 0: mask=%x match=%x", mMask, mMatch)
		}
	}
	// 两类掩码必须不同（一张表只有一个掩码，故必须两张表——文件头说明 2）
	if bytes.Equal(bMask, mMask) {
		t.Fatal("广播与组播掩码必须不同")
	}
}

// TestStormCbNonZero：Cb 必须非 0（真机实测 Cb=0 报 Invalid value 被拒），且随速率增长。
func TestStormCbNonZero(t *testing.T) {
	for _, kbps := range []uint32{1, 8000, 100000000} {
		if cb := stormCbBytes(kbps); cb == 0 {
			t.Fatalf("kbps=%d 的 Cb 不得为 0（VPP 会拒绝）", kbps)
		}
	}
	if stormCbBytes(8000) <= stormCbBytes(4000) {
		t.Fatal("Cb 应随速率单调增")
	}
	if got, want := stormCbBytes(8000), uint64(8000*125*8); got != want { // 8 秒窗口
		t.Fatalf("Cb 口径应为 8 秒 CIR：got=%d want=%d", got, want)
	}
}

// ---------- 建/改/删/重放的调用序 ----------

func stormCfg(bcast, mcast int) model.InterfaceConfig {
	ifc := model.InterfaceConfig{Name: "ens192"}
	if bcast > 0 || mcast > 0 {
		ifc.StormControl = &model.StormControl{BroadcastKbps: bcast, MulticastKbps: mcast}
	}
	return ifc
}

// TestStormApplyBroadcastOnly：建＝policer → 表 → session → attach（单类，与 spike 形状一致）。
func TestStormApplyBroadcastOnly(t *testing.T) {
	c := newFakeStormClient()
	p := NewStormProvider(c)
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 0)); err != nil {
		t.Fatalf("ApplyInterface: %v", err)
	}
	want := []string{
		"sw-index:ens192",
		// 登记为空 ⇒ 按数据面实况先撤旧（干净现场：查了但什么都没删）
		"attached:7",
		"policer-dump",
		// 建新
		"policer-add:nfvis-storm-ens192-broadcast:8000:cb=8000000",
		"table-add:mask=ffffffffffff00000000000000000000:next=unset",
		"session-add:0:match=ffffffffffff00000000000000000000:policer=0",
		"attach:if=7:table=0",
	}
	if got := strings.Join(c.calls, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("调用序不符：\n got:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	if c.attached[7] != 0 || c.sessions[0] != 1 || len(c.pols) != 1 {
		t.Fatalf("数据面状态不符: %s", c.snapshot())
	}
}

// TestStormApplyBothKindsChain：两类并存：组播表先建（作表链目标），广播表的 NextTableIndex
// 指向它；挂接口挂广播表（接口 L2 槽只有一张表，见文件头说明 1）。
func TestStormApplyBothKindsChain(t *testing.T) {
	c := newFakeStormClient()
	p := NewStormProvider(c)
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 20000)); err != nil {
		t.Fatalf("ApplyInterface: %v", err)
	}
	got := strings.Join(c.calls, "\n")
	want := []string{
		"sw-index:ens192",
		"attached:7",
		"policer-dump",
		// 组播表先建（表 0），广播表引用它（表 1，next=0）
		"policer-add:nfvis-storm-ens192-multicast:20000:cb=20000000",
		"table-add:mask=01000000000000000000000000000000:next=unset",
		"session-add:0:match=01000000000000000000000000000000:policer=0",
		"policer-add:nfvis-storm-ens192-broadcast:8000:cb=8000000",
		"table-add:mask=ffffffffffff00000000000000000000:next=0",
		"session-add:1:match=ffffffffffff00000000000000000000:policer=1",
		"attach:if=7:table=1",
	}
	if got != strings.Join(want, "\n") {
		t.Fatalf("两类并存的调用序不符：\n got:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	if c.attached[7] != 1 || c.sessions[0] != 1 || c.sessions[1] != 1 || len(c.pols) != 2 {
		t.Fatalf("数据面状态不符: %s", c.snapshot())
	}
}

// TestStormChangeReplacesOldFirst：改＝先撤旧（detach → 删 session/表/policer）后建新。
func TestStormChangeReplacesOldFirst(t *testing.T) {
	c := newFakeStormClient()
	p := NewStormProvider(c)
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 0)); err != nil {
		t.Fatalf("首次 Apply: %v", err)
	}
	c.calls = nil
	if err := p.ApplyInterface(context.Background(), stormCfg(9000, 0)); err != nil {
		t.Fatalf("改值 Apply: %v", err)
	}
	got := strings.Join(c.calls, "\n")
	want := []string{
		"sw-index:ens192",
		// 撤旧（登记路径：不查数据面，按登记精确删）
		"detach:if=7:table=0",
		"session-del:0:match=ffffffffffff00000000000000000000",
		"table-del:0:chain=false",
		"policer-del:nfvis-storm-ens192-broadcast:0:cb=0",
		// 建新
		"policer-add:nfvis-storm-ens192-broadcast:9000:cb=9000000",
		"table-add:mask=ffffffffffff00000000000000000000:next=unset",
		"session-add:1:match=ffffffffffff00000000000000000000:policer=1",
		"attach:if=7:table=1",
	}
	if got != strings.Join(want, "\n") {
		t.Fatalf("改值的调用序不符（应先撤旧后建新）：\n got:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	if len(c.tables) != 1 || len(c.pols) != 1 {
		t.Fatalf("改值后不得有残渣: %s", c.snapshot())
	}
}

// TestStormDeleteAll：声明清空＝detach → 逐类删 session/表/policer（顺序固定：广播先）。
func TestStormDeleteAll(t *testing.T) {
	c := newFakeStormClient()
	p := NewStormProvider(c)
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 20000)); err != nil {
		t.Fatalf("首次 Apply: %v", err)
	}
	c.calls = nil
	if err := p.ApplyInterface(context.Background(), stormCfg(0, 0)); err != nil {
		t.Fatalf("清空 Apply: %v", err)
	}
	got := strings.Join(c.calls, "\n")
	want := []string{
		"sw-index:ens192",
		"detach:if=7:table=1",
		"session-del:1:match=ffffffffffff00000000000000000000",
		"table-del:1:chain=false",
		"policer-del:nfvis-storm-ens192-broadcast:0:cb=0",
		"session-del:0:match=01000000000000000000000000000000",
		"table-del:0:chain=false",
		"policer-del:nfvis-storm-ens192-multicast:0:cb=0",
	}
	if got != strings.Join(want, "\n") {
		t.Fatalf("清空的调用序不符：\n got:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	if s := c.snapshot(); !strings.Contains(s, "tables=0") || !strings.Contains(s, "pols=0") ||
		!strings.Contains(s, "attached=map[]") {
		t.Fatalf("清空后不得留残渣: %s", s)
	}
	// 清空后收到同样的清空声明：零调用（幂等）
	c.calls = nil
	if err := p.ApplyInterface(context.Background(), stormCfg(0, 0)); err != nil {
		t.Fatalf("重复清空: %v", err)
	}
	if len(c.calls) != 0 {
		t.Fatalf("清空后重复 Apply 不应再调 VPP: %v", c.calls)
	}
}

// TestStormIdempotentNoCalls：值未变时零 VPP 调用（幂等重跑；含 SwInterfaceIndex 都不查）。
func TestStormIdempotentNoCalls(t *testing.T) {
	c := newFakeStormClient()
	p := NewStormProvider(c)
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 20000)); err != nil {
		t.Fatalf("首次 Apply: %v", err)
	}
	c.calls = nil
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 20000)); err != nil {
		t.Fatalf("重复 Apply: %v", err)
	}
	if len(c.calls) != 0 {
		t.Fatalf("幂等重跑不应调 VPP: %v", c.calls)
	}
}

// TestStormRegistryLostRebuildByFacts：**登记丢失但数据面对象还在**（nfvisd 重启而 VPP 未重启的
// 形态）：按数据面实况撤旧（摘绑定 + DelChain 删表 + 按名删 policer）再重建，不留残渣。
func TestStormRegistryLostRebuildByFacts(t *testing.T) {
	c := newFakeStormClient()
	p := NewStormProvider(c)
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 20000)); err != nil {
		t.Fatalf("首次 Apply: %v", err)
	}
	p.reset() // = resetProviders（VPP 重连/进程重启后的登记失效）
	c.calls = nil
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 20000)); err != nil {
		t.Fatalf("重建 Apply: %v", err)
	}
	got := strings.Join(c.calls, "\n")
	want := []string{
		"sw-index:ens192",
		// 实况撤旧：广播表挂在口上（掩码核对通过）⇒ 摘绑定 + 带链删表（组播表随之删）
		"attached:7",
		"table-info:1",
		"detach:if=7:table=1",
		"table-del:1:chain=true",
		"table-del:0:chain=true",
		// 按名清两类 policer
		"policer-dump",
		"policer-del:nfvis-storm-ens192-broadcast:0:cb=0",
		"policer-del:nfvis-storm-ens192-multicast:0:cb=0",
		// 重建（组播表先建）
		"policer-add:nfvis-storm-ens192-multicast:20000:cb=20000000",
		"table-add:mask=01000000000000000000000000000000:next=unset",
		"session-add:2:match=01000000000000000000000000000000:policer=2",
		"policer-add:nfvis-storm-ens192-broadcast:8000:cb=8000000",
		"table-add:mask=ffffffffffff00000000000000000000:next=2",
		"session-add:3:match=ffffffffffff00000000000000000000:policer=3",
		"attach:if=7:table=3",
	}
	if got != strings.Join(want, "\n") {
		t.Fatalf("登记丢失后的重建调用序不符：\n got:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	if s := c.snapshot(); !strings.Contains(s, "tables=2") || !strings.Contains(s, "pols=2") {
		t.Fatalf("重建后应为「两张表 + 两个 policer」且无孤儿: %s", s)
	}
}

// TestStormForeignTableInSlot：接口 L2 槽被非本产品对象占用（掩码形状不符）⇒ 如实报错且不动它。
func TestStormForeignTableInSlot(t *testing.T) {
	c := newFakeStormClient()
	c.tables[5] = StormTableInfo{Index: 5, Mask: "deadbeef", NextTableIndex: ^uint32(0)}
	c.attached[7] = 5
	p := NewStormProvider(c)
	err := p.ApplyInterface(context.Background(), stormCfg(8000, 0))
	if err == nil || !strings.Contains(err.Error(), "占用") {
		t.Fatalf("应如实报「槽被占用」: %v", err)
	}
	if _, ok := c.tables[5]; !ok || c.attached[7] != 5 {
		t.Fatalf("不得删除/摘除别人的表: %s", c.snapshot())
	}
	if len(c.pols) != 0 {
		t.Fatalf("报错路径不得建 policer: %s", c.snapshot())
	}
}

// TestStormIfaceMissing：接口不在数据面 ⇒ ErrIfaceUnavailable（提交编排据此判延后收敛）。
func TestStormIfaceMissing(t *testing.T) {
	c := newFakeStormClient()
	c.ifidx = map[string]uint32{}
	p := NewStormProvider(c)
	err := p.ApplyInterface(context.Background(), stormCfg(8000, 0))
	if !errors.Is(err, ErrIfaceUnavailable) {
		t.Fatalf("应返回 ErrIfaceUnavailable: %v", err)
	}
}

// TestStormBuildFailureRollsBack：建会话失败 ⇒ 回滚本次已建对象（不留孤儿表/policer），
// 登记停在最后成功态；重试能建齐。
func TestStormBuildFailureRollsBack(t *testing.T) {
	c := newFakeStormClient()
	c.failOn = "session-add"
	p := NewStormProvider(c)
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 0)); err == nil {
		t.Fatal("建会话失败应报错")
	}
	if c.snapshot(); !strings.Contains(c.snapshot(), "tables=0") || !strings.Contains(c.snapshot(), "pols=0") {
		t.Fatalf("失败后应回滚已建对象: %s", c.snapshot())
	}
	c.failOn, c.calls = "", nil
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 0)); err != nil {
		t.Fatalf("重试: %v", err)
	}
	if !strings.Contains(strings.Join(c.calls, "\n"), "attach:if=7") || len(c.tables) != 1 {
		t.Fatalf("重试应建齐: %v", c.calls)
	}
}

// TestStormDataplaneFacts：读视图取实测事实（policer/分类表/绑定/计数），取不到给原因不猜。
func TestStormDataplaneFacts(t *testing.T) {
	c := newFakeStormClient()
	p := NewStormProvider(c)
	p.SetCountersReader(fakeStormCounters{cnt: StormCounters{ConformPackets: 189, ViolatePackets: 9}, ok: true})
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 20000)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	dp, err := p.Dataplane(context.Background(), "ens192")
	if err != nil {
		t.Fatalf("Dataplane: %v", err)
	}
	if !dp.Available || !dp.Attached || dp.AttachedL2Table != 1 {
		t.Fatalf("应实测到挂在广播表（1）: %+v", dp)
	}
	b := dp.Kinds[StormKindBroadcast]
	if !b.PolicerPresent || b.Table == nil || b.Counters == nil || b.Counters.ConformPackets != 189 {
		t.Fatalf("广播类实测事实不齐: %+v", b)
	}
	if b.Table.NextTableIndex != 0 { // 广播表链着组播表（表 0）
		t.Fatalf("广播表应链向组播表: %+v", b.Table)
	}
	m := dp.Kinds[StormKindMulticast]
	if !m.PolicerPresent || m.Table == nil || m.Table.NextTableIndex != ^uint32(0) {
		t.Fatalf("组播类实测事实不齐（无后续链）: %+v", m)
	}
	// 登记丢失：仍能从「口上挂着的表 + 掩码」认出广播类（实测），并如实说明无法读该口重放状态
	p.reset()
	dp2, _ := p.Dataplane(context.Background(), "ens192")
	b2 := dp2.Kinds[StormKindBroadcast]
	if b2.Table == nil || b2.Table.Mask != string(c.tables[1].Mask) || b2.CountersReason == "" {
		t.Fatalf("登记丢失后应按掩码认表并如实给计数原因: %+v", b2)
	}
	// 未接入计数读数 ⇒ 如实给原因（不是 0）
	p2 := NewStormProvider(c)
	if err := p2.ApplyInterface(context.Background(), stormCfg(8000, 0)); err != nil {
		t.Fatalf("Apply(p2): %v", err)
	}
	dp3, _ := p2.Dataplane(context.Background(), "ens192")
	if got := dp3.Kinds[StormKindBroadcast].CountersReason; got == "" {
		t.Fatalf("未接入计数来源应给原因: %+v", dp3.Kinds[StormKindBroadcast])
	}
}

type fakeStormCounters struct {
	cnt    StormCounters
	ok     bool
	reason string
}

func (f fakeStormCounters) StormCounters(ctx context.Context, idx uint32, name string) (StormCounters, bool, string) {
	return f.cnt, f.ok, f.reason
}

// TestStormCountersFromDump：stats segment 文本解析（按索引或名字匹配；找不到 ok=false）。
func TestStormCountersFromDump(t *testing.T) {
	dump := strings.Join([]string{
		"9:430184.00:/buffer-pools/default-numa-0/available",
		"7:189.00:/net/policer/11/conform_packets",
		"7:12345.00:/net/policer/11/conform_bytes",
		"7:0.00:/net/policer/11/exceed_packets",
		"7:9.00:/net/policer/11/violate_packets",
		"7:784.00:/net/policer/11/violate_bytes",
		"7:42.00:/net/policer/nfvis-storm-ens224-multicast/conform_packets",
	}, "\n")
	c, ok := StormCountersFromDump(dump, 11, "nfvis-storm-ens192-broadcast")
	if !ok || c.ConformPackets != 189 || c.ConformBytes != 12345 || c.ViolatePackets != 9 || c.ViolateBytes != 784 {
		t.Fatalf("按索引应解析出计数: %+v ok=%v", c, ok)
	}
	c, ok = StormCountersFromDump(dump, 99, "nfvis-storm-ens224-multicast")
	if !ok || c.ConformPackets != 42 {
		t.Fatalf("按名字应解析出计数: %+v ok=%v", c, ok)
	}
	if _, ok := StormCountersFromDump(dump, 77, "nfvis-storm-ens999-broadcast"); ok {
		t.Fatal("不存在的 policer 应 ok=false（不返回零值当真值）")
	}
}
