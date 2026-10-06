package network

// 决策 #385：接口入向风暴抑制的单测（向量构造纯函数 + 建/改/删/重放的调用序 + 读视图 +
// 真机修复轮的两个场景：重建不重复建表、删除按实况解绑并清干净）。
//
// 假客户端是一台**小型状态机**（记表/会话/policer/绑定），因此除调用序外还能断言
// 「撤旧之后不留残渣」「登记丢失后按数据面实况重建」与「孤儿表清扫」。
//
// 与真机的口径对齐（真机实测，2026-10-06）：
//   - 绑定只能按 `classify_table_by_interface` 读（`policer_classify_dump` 返回不了绑定）；
//   - 接口 L2 槽挂表**不容错**（槽已有表时不覆盖——真机由此出现「登记说新、实况是旧」）；
//   - 解绑未挂在接口上的表 ⇒ `No such table (-65)`（客户端归一为 ErrStormAbsent）。

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
	calls       []string
	ifidx       map[string]uint32
	tables      map[uint32]StormTableInfo
	sessions    map[uint32]int
	pols        map[string]uint32
	attached    map[uint32]uint32
	nextTable   uint32
	nextPolicer uint32
	failOn      string // 命中该调用前缀即报错（失败注入）
	failListing string // 命中该「实况查询」调用即报错（孤儿清扫的安全边界用例）
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
		if _, ok := f.pols[name]; !ok {
			return 0, ErrStormAbsent // 本就不在（客户端归一给 Provider 决定）
		}
		delete(f.pols, name)
		return 0, nil
	}
	if _, ok := f.pols[name]; ok {
		return 0, fmt.Errorf("VALUE_EXIST: %s", name) // 新增方向不容错
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
		return ErrStormAbsent
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
	if _, ok := f.tables[tableIndex]; !ok {
		return ErrStormAbsent
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
		// 挂上方向不容错：槽已被别的表占用 ⇒ 报错（真机实测：VPP 不给覆盖；静默吞掉会留下
		// 「登记说新、实况是旧」的错位）
		if cur, ok := f.attached[swIfIndex]; ok && cur != l2TableIndex {
			return fmt.Errorf("VALUE_EXIST: 接口 %d 的 L2 槽已挂表 %d", swIfIndex, cur)
		}
		f.attached[swIfIndex] = l2TableIndex
		return nil
	}
	if cur, ok := f.attached[swIfIndex]; !ok || cur != l2TableIndex {
		return ErrStormAbsent // 真机实测：解绑未挂在接口上的表得到 No such table (-65)
	}
	delete(f.attached, swIfIndex)
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
	if f.failListing != "" && strings.HasPrefix(f.calls[len(f.calls)-1], f.failListing) {
		return 0, false, fmt.Errorf("注入失败: %s", f.calls[len(f.calls)-1])
	}
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

func (f *fakeStormClient) ClassifyTableIDs() ([]uint32, error) {
	f.calls = append(f.calls, "table-ids")
	if f.listingFails() {
		return nil, fmt.Errorf("注入失败: table-ids")
	}
	ids := make([]uint32, 0, len(f.tables))
	for id := range f.tables {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func (f *fakeStormClient) AllInterfaceIndexes() ([]uint32, error) {
	f.calls = append(f.calls, "iface-indexes")
	if f.listingFails() {
		return nil, fmt.Errorf("注入失败: iface-indexes")
	}
	seen := map[uint32]bool{}
	out := make([]uint32, 0, len(f.ifidx))
	for _, i := range f.ifidx {
		if !seen[i] {
			seen[i] = true
			out = append(out, i)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// listingFails 最后一次调用是否命中失败注入（孤儿清扫的安全边界用例用）。
func (f *fakeStormClient) listingFails() bool {
	if f.failListing == "" || len(f.calls) == 0 {
		return false
	}
	return strings.HasPrefix(f.calls[len(f.calls)-1], f.failListing)
}

func unsetU32(v uint32) string {
	if v == ^uint32(0) {
		return "unset"
	}
	return fmt.Sprint(v)
}

// seedStormTable 直接往「数据面」塞一张本产品形状的分类表（模拟历史缺陷/上次进程留下的现场；
// 修好之后 Provider 自己不会再产生这种状态）。
func (f *fakeStormClient) seedStormTable(kind string, next uint32) uint32 {
	idx := f.nextTable
	f.nextTable++
	f.tables[idx] = StormTableInfo{Index: idx, Mask: fmt.Sprintf("%x", stormMask(kind)), NextTableIndex: next}
	f.sessions[idx] = 1
	return idx
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

func (f *fakeStormClient) clean() bool {
	return len(f.tables) == 0 && len(f.pols) == 0 && len(f.attached) == 0
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
// （真机已按 show classify tables 核对：ff×6 与 01 两种掩码）。
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
			t.Fatalf("组播掩码/匹配的第 %d 字节应为 0: mask=%x match=%x", i, mMask, mMatch)
		}
	}
	// 两类掩码必须不同（一张表只有一个掩码，故必须两张表——文件头说明 2）
	if bytes.Equal(bMask, mMask) {
		t.Fatal("广播与组播掩码必须不同")
	}
	if !stormMaskKnown(fmt.Sprintf("%x", bMask)) || !stormMaskKnown(fmt.Sprintf("%x", mMask)) {
		t.Fatal("两类掩码都应被 stormMaskKnown 认作本产品形状")
	}
	if stormMaskKnown("deadbeef") {
		t.Fatal("外来掩码不得被认作本产品形状")
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
		// 撤旧：先查接口 L2 槽实况（干净现场：没挂）、再按名看 policer、再扫孤儿表（无表）
		"attached:7",
		"policer-dump",
		"table-ids",
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
		"table-ids",
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

// TestStormChangeReplacesOldFirst：改＝先撤旧（按实况解绑 → 删表（含链）→ 删 policer）后建新。
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
		// ① 实况：接口 L2 槽挂着表 0（掩码形状核对通过）⇒ 按实况解绑 + 带链删表
		"attached:7",
		"table-info:0",
		"detach:if=7:table=0",
		"table-del:0:chain=true",
		// ② 登记里的表（与实况同一张，删除幂等——已不在 ⇒ 按已达成容忍）
		"session-del:0:match=ffffffffffff00000000000000000000",
		"table-del:0:chain=false",
		// ③ 按名清 policer
		"policer-dump",
		"policer-del:nfvis-storm-ens192-broadcast:0:cb=0",
		// ④ 孤儿清扫（无孤儿）
		"table-ids",
		// 建新
		"policer-add:nfvis-storm-ens192-broadcast:9000:cb=9000000",
		"table-add:mask=ffffffffffff00000000000000000000:next=unset",
		"session-add:1:match=ffffffffffff00000000000000000000:policer=1",
		"attach:if=7:table=1",
	}
	if got != strings.Join(want, "\n") {
		t.Fatalf("改值的调用序不符（应先撤旧后建新）：\n got:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	// 旧对象清干净、只剩新建的一对（表 1 + policer 1 + 绑定 1）
	if len(c.tables) != 1 || len(c.pols) != 1 || c.sessions[1] != 1 || c.attached[7] != 1 {
		t.Fatalf("改值后应只剩新建的一对（无残渣）: %s", c.snapshot())
	}
}

// TestStormDeleteAll：声明清空＝按实况解绑 → 逐类删（幂等）→ 清 policer → 扫孤儿。
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
		"attached:7",
		"table-info:1",
		"detach:if=7:table=1",
		"table-del:1:chain=true", // 链上还有组播表 0，一并删
		"table-del:0:chain=true", // 链上那张的删除（由 DelChain 递归）
		"session-del:1:match=ffffffffffff00000000000000000000",
		"table-del:1:chain=false",
		"session-del:0:match=01000000000000000000000000000000",
		"table-del:0:chain=false",
		"policer-dump",
		"policer-del:nfvis-storm-ens192-broadcast:0:cb=0",
		"policer-del:nfvis-storm-ens192-multicast:0:cb=0",
		"table-ids",
	}
	if got != strings.Join(want, "\n") {
		t.Fatalf("清空的调用序不符：\n got:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	if !c.clean() {
		t.Fatalf("清空后不得留残渣: %s", c.snapshot())
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

// TestStormRebuildNoDuplicateTables（真机修复轮 ①）：**VPP 里已有本接口的一对表、进程登记为空**
// （nfvisd 重启而 VPP 未重启）时重建，必须**先按实况认出并清掉既有表**再建——否则会重复建表
// （真机实测：重放后 nfvis 重启 ⇒ 4 张表、policer 仍 2 个）。
//
// 红：把「实况解绑/删表」这一步摘掉（模拟旧实现按不可靠来源判实况）⇒ 表数变 4，本用例失败。
func TestStormRebuildNoDuplicateTables(t *testing.T) {
	c := newFakeStormClient()
	// 现场：上一轮重放建的一对表 + 两个 policer，接口槽挂着广播表；本进程登记为空
	mIdx := c.seedStormTable(StormKindMulticast, ^uint32(0))
	bIdx := c.seedStormTable(StormKindBroadcast, mIdx)
	c.attached[7] = bIdx
	c.pols["nfvis-storm-ens192-broadcast"] = 0
	c.pols["nfvis-storm-ens192-multicast"] = 1

	p := NewStormProvider(c)
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 20000)); err != nil {
		t.Fatalf("重建 Apply: %v", err)
	}
	if n := len(c.tables); n != 2 {
		t.Fatalf("重建后应恰有 2 张表（不重复建），实际 %d 张: %s", n, c.snapshot())
	}
	if len(c.pols) != 2 {
		t.Fatalf("policer 应仍为 2 个（按名幂等）: %s", c.snapshot())
	}
	if got := c.attached[7]; got == mIdx || got == bIdx {
		t.Fatalf("接口应挂新建的表，实际仍挂旧表 %d: %s", got, c.snapshot())
	}
	// 旧的一对表确实被清掉了（新的一对是新索引）
	if _, ok := c.tables[mIdx]; ok {
		t.Fatalf("旧组播表 %d 未被清理: %s", mIdx, c.snapshot())
	}
	if _, ok := c.tables[bIdx]; ok {
		t.Fatalf("旧广播表 %d 未被清理: %s", bIdx, c.snapshot())
	}
}

// TestStormDeleteByFactsCleansDuplicates（真机修复轮 ②）：残留 4 张表（历史重复对）+ 登记陈旧
// （登记说挂表 3、实况挂的是 1）时删除，必须：按**实况**解绑（不是登记）→ 陈旧解绑按已达成容忍
// （真机 `No such table (-65)`）→ 全部本接口的表/policer 清光（含重复对）。
//
// 红：不做实况解绑（用陈旧登记索引解绑）且不容错 ⇒ 真机那条 `No such table (-65)` 让整次提交失败。
func TestStormDeleteByFactsCleansDuplicates(t *testing.T) {
	c := newFakeStormClient()
	p := NewStormProvider(c)
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 20000)); err != nil {
		t.Fatalf("首次 Apply: %v", err)
	}
	// 复刻真机现场：历史缺陷留下第二对表（2=组播、3=广播，广播表链着组播表），且**登记指向
	// 第二对**（3）而接口 L2 槽实际仍挂着第一对的广播表（1）——即「attach 未生效但登记已推进」。
	m2 := c.seedStormTable(StormKindMulticast, ^uint32(0))
	b3 := c.seedStormTable(StormKindBroadcast, m2)
	p.mu.Lock()
	p.rt["ens192"].kinds[StormKindBroadcast].tableIdx = b3
	p.rt["ens192"].kinds[StormKindMulticast].tableIdx = m2
	p.rt["ens192"].attachTable = b3
	p.mu.Unlock()
	if len(c.tables) != 4 || c.attached[7] != 1 {
		t.Fatalf("现场构造失败: %s", c.snapshot())
	}
	c.calls = nil
	if err := p.ApplyInterface(context.Background(), stormCfg(0, 0)); err != nil {
		t.Fatalf("按实况解绑 + 容错的删除不应失败: %v", err)
	}
	if !c.clean() {
		t.Fatalf("删除后应清光全部表/policer/绑定（含重复对）: %s", c.snapshot())
	}
	got := strings.Join(c.calls, "\n")
	// 先按实况（表 1）解绑：这是真机失败点（旧实现按登记传 3 ⇒ No such table）
	if !strings.Contains(got, "detach:if=7:table=1") {
		t.Fatalf("应按实况表 1 解绑: %s", got)
	}
	if !strings.Contains(got, "detach:if=7:table=3") {
		t.Fatalf("陈旧登记的表 3 也要尝试解绑（拿到 -65 按已达成）: %s", got)
	}
	if idx := strings.Index(got, "detach:if=7:table=1"); idx > strings.Index(got, "detach:if=7:table=3") {
		t.Fatalf("实况解绑必须排在陈旧登记解绑之前: %s", got)
	}
}

// TestStormSweepProtectsOtherInterfacesTables：孤儿清扫只清「没挂在任何接口上」的表——
// 另一个接口仍挂着的表必须原样保留（宁可不扫，也不误删）。
func TestStormSweepProtectsOtherInterfacesTables(t *testing.T) {
	c := newFakeStormClient()
	c.ifidx["ens224"] = 9
	p := NewStormProvider(c)
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 0)); err != nil {
		t.Fatalf("首次 Apply: %v", err)
	}
	// 另一个接口（ens224）合法持有的一对表 + 一个孤儿对（没挂在任何接口上）
	otherM := c.seedStormTable(StormKindMulticast, ^uint32(0))
	otherB := c.seedStormTable(StormKindBroadcast, otherM)
	c.attached[9] = otherB
	orphanM := c.seedStormTable(StormKindMulticast, ^uint32(0))
	orphanB := c.seedStormTable(StormKindBroadcast, orphanM)

	if err := p.ApplyInterface(context.Background(), stormCfg(9000, 0)); err != nil {
		t.Fatalf("改值 Apply: %v", err)
	}
	if _, ok := c.tables[otherB]; !ok {
		t.Fatalf("别的接口挂着的表不得被清: %s", c.snapshot())
	}
	if _, ok := c.tables[otherM]; !ok {
		t.Fatalf("别的接口挂着的链上表不得被清: %s", c.snapshot())
	}
	if _, ok := c.tables[orphanB]; ok {
		t.Fatalf("孤儿表应被清: %s", c.snapshot())
	}
	if _, ok := c.tables[orphanM]; ok {
		t.Fatalf("孤儿链表应被清: %s", c.snapshot())
	}
	if c.attached[9] != otherB {
		t.Fatalf("别的接口的绑定不得被动: %s", c.snapshot())
	}
}

// TestStormSweepGivesUpWhenFactsUnavailable：孤儿清扫的安全边界——表清单/接口清单/绑定查询
// 任一不可得 ⇒ **放弃清扫**（宁可不扫，也不误删别的接口的表），本接口自身的清理照常完成。
func TestStormSweepGivesUpWhenFactsUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		failOn string
	}{
		{"表清单查询失败", "table-ids"},
		{"接口清单查询失败", "iface-indexes"},
		{"保护集查询失败（另一个接口）", "attached:9"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeStormClient()
			c.ifidx["ens224"] = 9
			p := NewStormProvider(c)
			if err := p.ApplyInterface(context.Background(), stormCfg(8000, 0)); err != nil {
				t.Fatalf("首次 Apply: %v", err)
			}
			// 另一个接口合法持有的一对表（清扫里要保护的对象）
			otherM := c.seedStormTable(StormKindMulticast, ^uint32(0))
			otherB := c.seedStormTable(StormKindBroadcast, otherM)
			c.attached[9] = otherB
			c.failListing = tc.failOn
			if err := p.ApplyInterface(context.Background(), stormCfg(0, 0)); err != nil {
				t.Fatalf("清扫放弃不应让删除失败: %v", err)
			}
			c.failListing = ""
			if _, ok := c.tables[otherB]; !ok {
				t.Fatalf("放弃清扫时不得误删别的接口的表: %s", c.snapshot())
			}
			if c.attached[9] != otherB {
				t.Fatalf("别的接口的绑定不得动: %s", c.snapshot())
			}
			// 本接口自身的清理照常完成：policer 清光、ens192 的绑定已摘
			if len(c.pols) != 0 {
				t.Fatalf("本接口自身的 policer 应已清理: %s", c.snapshot())
			}
			if _, ok := c.attached[7]; ok {
				t.Fatalf("本接口的绑定应已摘掉: %s", c.snapshot())
			}
		})
	}
}

// TestStormAttachOccupiedSlotFailsLoudly：接口 L2 槽被占且不是我们的形状 ⇒ 如实报错（不动它）。
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
	if !c.clean() {
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
	if b2.Table == nil || b2.Table.Mask != c.tables[1].Mask || b2.CountersReason == "" {
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

// 决策 #390③：L2Network.TeardownInterface 的接线——接口元素被删时，storm/portsec/QoS
// 三者的接口级绑定都要撤掉（复用各自「空声明＝teardown」的既有路径）。
func TestL2NetworkTeardownInterface(t *testing.T) {
	ctx := context.Background()

	sf := newFakeStormClient()
	sf.ifidx["ens224"] = 9
	storm := NewStormProvider(sf)
	if err := storm.ApplyInterface(ctx, model.InterfaceConfig{
		Name: "ens224", StormControl: &model.StormControl{BroadcastKbps: 1000}}); err != nil {
		t.Fatalf("前置 storm 下发: %v", err)
	}
	if _, ok := sf.pols["nfvis-storm-ens224-broadcast"]; !ok {
		t.Fatalf("前置：storm policer 应在: %v", sf.pols)
	}

	pf := newFakePortSec()
	portsec := NewPortSecProvider(pf)
	if err := portsec.ApplyInterface(ctx, model.InterfaceConfig{
		Name: "ens224", PortSecurity: []model.PortSecMAC{"b0:b0:00:00:00:01"}}); err != nil {
		t.Fatalf("前置 portsec 下发: %v", err)
	}
	if _, bound := pf.bound[4]; !bound {
		t.Fatalf("前置：portsec macip 应已绑: %v", pf.bound)
	}

	svcf := newFakeSvc()
	svc := NewServicesProvider(svcf)
	if err := svc.ApplyInterface(ctx, model.InterfaceConfig{Name: "ens224", IngressPolicy: "pin"}); err != nil {
		t.Fatalf("前置 QoS 绑定: %v", err)
	}

	n := NewL2Network(nil, nil)
	n.SetStorm(storm)
	n.SetPortSec(portsec)
	n.SetServices(svc)

	svcf.pins, svcf.pouts = nil, nil // 只保留 teardown 自身的调用
	if err := n.TeardownInterface(ctx, model.InterfaceConfig{Name: "ens224"}); err != nil {
		t.Fatalf("TeardownInterface: %v", err)
	}
	if _, ok := sf.pols["nfvis-storm-ens224-broadcast"]; ok {
		t.Fatalf("storm policer 应删除: %v", sf.pols)
	}
	if _, bound := pf.bound[4]; bound {
		t.Fatalf("portsec macip 应解绑: %v", pf.bound)
	}
	if len(svcf.pins) != 1 || svcf.pins[0] != "pin:off" {
		t.Fatalf("QoS 入向应解绑: %v", svcf.pins)
	}

	// 接口已从 VPP 消失（ErrIfaceUnavailable）按已达成，不阻断提交。
	sf3 := newFakeStormClient()
	sf3.ifidx["ens224"] = 9
	storm3 := NewStormProvider(sf3)
	if err := storm3.ApplyInterface(ctx, model.InterfaceConfig{
		Name: "ens224", StormControl: &model.StormControl{BroadcastKbps: 1000}}); err != nil {
		t.Fatal(err)
	}
	delete(sf3.ifidx, "ens224")
	n3 := NewL2Network(nil, nil)
	n3.SetStorm(storm3)
	if err := n3.TeardownInterface(ctx, model.InterfaceConfig{Name: "ens224"}); err != nil {
		t.Fatalf("接口已不在 VPP 时 teardown 应按已达成（不阻断）: %v", err)
	}
}

// TestStormReconcileReplaysAfterOutOfBandDelete（决策 #394①）：带外删除 policer 与接口 L2 槽上的
// 分类表后，巡检的按登记复核应在**一个周期内**按数据面实况重放——登记 `applyDone` 仍为真也不跳过。
func TestStormReconcileReplaysAfterOutOfBandDelete(t *testing.T) {
	ctx := context.Background()
	c := newFakeStormClient()
	p := NewStormProvider(c)
	if err := p.ApplyInterface(ctx, stormCfg(8000, 0)); err != nil {
		t.Fatalf("前置 Apply: %v", err)
	}
	// 带外删除：policer 与接口 L2 槽上的分类表都没了（数据面丢失），进程内登记仍称已下发。
	delete(c.pols, "nfvis-storm-ens192-broadcast")
	delete(c.attached, 7)
	c.calls = nil
	// 前提：幂等重跑 ApplyInterface 会因 applyDone 为真而早退（正是旧行为下永不恢复的根因）。
	if err := p.ApplyInterface(ctx, stormCfg(8000, 0)); err != nil {
		t.Fatalf("ApplyInterface(幂等): %v", err)
	}
	if len(c.calls) != 0 {
		t.Fatalf("登记一致时 ApplyInterface 应早退（前提校验）: %v", c.calls)
	}
	// 巡检复核：实况缺项 ⇒ 重放（清登记后按实况重建）。
	cfg := model.Config{Interfaces: []model.InterfaceConfig{stormCfg(8000, 0)}}
	if errs := p.Reconcile(ctx, cfg); len(errs) != 0 {
		t.Fatalf("Reconcile: %v", errs)
	}
	if _, ok := c.pols["nfvis-storm-ens192-broadcast"]; !ok {
		t.Fatalf("巡检应重放 policer: %s", c.snapshot())
	}
	tbl, ok := c.attached[7]
	if !ok || c.sessions[tbl] != 1 {
		t.Fatalf("巡检应重建分类表并挂接口: %s", c.snapshot())
	}
}

// TestStormReconcileNoopWhenPresent：数据面齐备时巡检只做实况核对、零重放（幂等，不重复建表）。
func TestStormReconcileNoopWhenPresent(t *testing.T) {
	ctx := context.Background()
	c := newFakeStormClient()
	p := NewStormProvider(c)
	if err := p.ApplyInterface(ctx, stormCfg(8000, 0)); err != nil {
		t.Fatalf("前置 Apply: %v", err)
	}
	c.calls = nil
	cfg := model.Config{Interfaces: []model.InterfaceConfig{stormCfg(8000, 0)}}
	if errs := p.Reconcile(ctx, cfg); len(errs) != 0 {
		t.Fatalf("Reconcile: %v", errs)
	}
	for _, line := range c.calls {
		if strings.HasPrefix(line, "policer-add") || strings.HasPrefix(line, "table-add") ||
			strings.HasPrefix(line, "attach:") || strings.HasPrefix(line, "session-add") {
			t.Fatalf("数据面齐备时不应重放: %v", c.calls)
		}
	}
	if _, ok := c.pols["nfvis-storm-ens192-broadcast"]; !ok || c.attached[7] != 0 || c.sessions[0] != 1 {
		t.Fatalf("巡检不应破坏既有数据面: %s", c.snapshot())
	}
}

// TestL2NetworkReconcileStorm：L2Network 接线（未注入 provider 时为空操作）。
func TestL2NetworkReconcileStorm(t *testing.T) {
	c := newFakeStormClient()
	n := NewL2Network(nil, nil)
	if errs := n.ReconcileStorm(context.Background(), model.Config{}); errs != nil {
		t.Fatalf("未注入 provider 应返回 nil: %v", errs)
	}
	n.SetStorm(NewStormProvider(c))
	cfg := model.Config{Interfaces: []model.InterfaceConfig{stormCfg(0, 0)}}
	if errs := n.ReconcileStorm(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("无声明应无事可做: %v", errs)
	}
}
