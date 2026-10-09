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
	polCir      map[string]uint32 // policer 名 → CIR（kbps；dump 实测字段，自认领按名取回 kbps）
	attached    map[uint32]uint32
	nextTable   uint32
	nextPolicer uint32
	failOn      string // 命中该调用前缀即报错（失败注入）
	failListing string // 命中该「实况查询」调用即报错（孤儿清扫的安全边界用例）
	// blindAttached 模拟**本底座（VPP 26.06）的 policer-classify 绑定不可回读**：
	// classify_table_by_interface 恒回 NONE（阴性），而数据面内部仍挂着表（attached 照旧维护）。
	blindAttached bool
	// livePin 钉住「实况回读」的结果（解绑后仍读到绑定——真机上「删了表而槽仍指向它」的形态）。
	livePin map[uint32]uint32
}

func newFakeStormClient() *fakeStormClient {
	return &fakeStormClient{
		ifidx:    map[string]uint32{"ens192": 7},
		tables:   map[uint32]StormTableInfo{},
		sessions: map[uint32]int{},
		pols:     map[string]uint32{},
		polCir:   map[string]uint32{},
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
		delete(f.polCir, name)
		return 0, nil
	}
	if _, ok := f.pols[name]; ok {
		return 0, fmt.Errorf("VALUE_EXIST: %s", name) // 新增方向不容错
	}
	idx := f.nextPolicer
	f.nextPolicer++
	f.pols[name] = idx
	f.polCir[name] = cirKbps
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
		// 真机实测的 dump 字段：名字 + CIR（kbps）；自认领按名取回 CIR 写进登记。
		out = append(out, StormPolicer{Name: n, CirKbps: f.polCir[n]})
	}
	return out, nil
}

func (f *fakeStormClient) AttachedL2Table(swIfIndex uint32) (uint32, bool, error) {
	f.calls = append(f.calls, "attached:"+fmt.Sprint(swIfIndex))
	if f.failListing != "" && strings.HasPrefix(f.calls[len(f.calls)-1], f.failListing) {
		return 0, false, fmt.Errorf("注入失败: %s", f.calls[len(f.calls)-1])
	}
	if f.blindAttached {
		return 0, false, nil // 本底座不可回读：阴性（不代表未挂）
	}
	if t, ok := f.livePin[swIfIndex]; ok {
		return t, true, nil
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
		// 自认领尝试（登记为空 + 实况不可回读）：查槽（没挂）→ 按名找本接口 policer（无）
		// ⇒ 没有可认的对象，按声明正常建
		"attached:7",
		"policer-dump",
		// 撤旧：再查接口 L2 槽实况（干净现场：没挂）、按名看 policer、扫孤儿表（无表）
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
		// 自认领尝试（登记为空）：查槽（没挂）→ 按名找 policer（无）⇒ 无可认对象
		"attached:7",
		"policer-dump",
		// 撤旧（现场干净）
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
		// ① 实况：接口 L2 槽挂着表 0（掩码形状核对通过）⇒ 按实况解绑 → **复核解绑已达成**
		// （第二次 attached:7 读到无绑定）→ 带链删表
		"attached:7",
		"table-info:0",
		"detach:if=7:table=0",
		"attached:7",
		"table-info:0", // 删前链核对（决策 #429①）：读被删表取链目标；无链 ⇒ 带链标志无对象可波及
		"table-del:0:chain=true",
		// ② 登记里的表（与实况同一张，已解绑过 ⇒ 不重复解绑）——删前形状复核读到「表已不在」
		//（刚由①带走）：**不删**（也没有可删的对象），按「本就不在」＝已达成推进登记
		//（决策 #429①；修复前会再发一次 session-del/table-del，靠容忍 -65 收场）
		"table-info:0",
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
		"attached:7", // 解散后复核解绑已达成（决策 #421③）
		// 删前链核对（决策 #429①）：读被删表取链目标 → 链目标是本产品形状（组播表）⇒ 允许带链删
		"table-info:1",
		"table-info:0",
		"table-del:1:chain=true", // 链上还有组播表 0，一并删
		"table-del:0:chain=true", // 链上那张的删除（由 DelChain 递归）
		// 登记里的逐类表：删前形状复核（决策 #429①）——两张表刚随①的带链删除消失 ⇒ 实测
		// 「表已不存在」：**不删**（也没有可删的对象），按已达成推进登记。
		"table-info:1",
		"detach:if=7:table=0", // 组播表先按它的索引解绑（未挂 ⇒ 已达成）——形状复核在其后
		"table-info:0",
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
	p.SetCountersReader(&fakeStormCounters{cnt: StormCounters{ConformPackets: 189, ViolatePackets: 9}, ok: true})
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

// TestStormDataplaneSlotHeldByForeignTable（决策 #401，R176-2）：进程内登记说两类表都已下发，
// 但接口 L2 槽实际挂着**别的**表（如 port-security 的 macip 表，掩码形状与两类风暴掩码都不符）
// ⇒ 读视图必须如实报两类「未挂」（Table==nil），不再以进程内登记充当在位证据。
// 修复前：`case e != nil` 直接按登记报 `e.tableIdx` 的表 ⇒ 报「在位」，限速静默不生效却显示已生效。
func TestStormDataplaneSlotHeldByForeignTable(t *testing.T) {
	c := newFakeStormClient()
	p := NewStormProvider(c)
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 20000)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// 同接口被 port-security 抢走 L2 槽：槽上换成一张非本产品形状的表（macip 白名单掩码），
	// 而进程内登记仍认为广播/组播表已下发（policer 也仍在数据面）。
	c.tables[24] = StormTableInfo{Index: 24, Mask: "000000000000ffffffffffffffff0000", NextTableIndex: ^uint32(0)}
	c.attached[7] = 24
	dp, err := p.Dataplane(context.Background(), "ens192")
	if err != nil {
		t.Fatalf("Dataplane: %v", err)
	}
	if !dp.Available || !dp.Attached || dp.AttachedL2Table != 24 {
		t.Fatalf("应实测到槽挂着外来表 24: %+v", dp)
	}
	if !dp.Kinds[StormKindBroadcast].PolicerPresent {
		t.Fatalf("前提：广播 policer 仍在数据面（本用例针对「在位」失真）: %+v", dp.Kinds[StormKindBroadcast])
	}
	for _, kind := range []string{StormKindBroadcast, StormKindMulticast} {
		if kd := dp.Kinds[kind]; kd.Table != nil {
			t.Fatalf("槽被外来表占用时 %s 不得报「在位」: %+v", kind, kd)
		}
	}
}

type fakeStormCounters struct {
	cnt    StormCounters
	ok     bool
	reason string
	// names 记录读数来源被调用的 policer 名（决策 #429②：计数一律按名读；名字不在数据面时
	// **根本不调用**读数来源——没有对象可读，也就不按索引猜）。
	names []string
}

func (f *fakeStormCounters) StormCounters(ctx context.Context, policerName string) (StormCounters, bool, string) {
	f.names = append(f.names, policerName)
	return f.cnt, f.ok, f.reason
}

// TestStormCountersFromDump：stats segment 文本解析——**按 policer 名**匹配，找不到 ok=false。
//
// 决策 #429②：policer 索引在 policer_dump 里不可回读（自认领态只有哨兵占位）、且可能被底座
// 复用，按索引匹配会把别的 policer 的计数读成本产品的计数 ⇒ 索引形状的路径一律不匹配。
// 修复前：`<id> == 索引的十进制串 || <id> == 名字` 两路都试（索引路会把外来对象算进来）。
func TestStormCountersFromDump(t *testing.T) {
	dump := strings.Join([]string{
		"9:430184.00:/buffer-pools/default-numa-0/available",
		"7:189.00:/net/policer/nfvis-storm-ens192-broadcast/conform_packets",
		"7:12345.00:/net/policer/nfvis-storm-ens192-broadcast/conform_bytes",
		"7:0.00:/net/policer/nfvis-storm-ens192-broadcast/exceed_packets",
		"7:9.00:/net/policer/nfvis-storm-ens192-broadcast/violate_packets",
		"7:784.00:/net/policer/nfvis-storm-ens192-broadcast/violate_bytes",
		"7:42.00:/net/policer/nfvis-storm-ens224-multicast/conform_packets",
		"7:999.00:/net/policer/11/conform_packets", // 索引形状的路径：不得被匹配
	}, "\n")
	c, ok := StormCountersFromDump(dump, "nfvis-storm-ens192-broadcast")
	if !ok || c.ConformPackets != 189 || c.ConformBytes != 12345 || c.ViolatePackets != 9 || c.ViolateBytes != 784 {
		t.Fatalf("按名应解析出计数（来自按名路径的 189，而不是索引路径的 999）: %+v ok=%v", c, ok)
	}
	c, ok = StormCountersFromDump(dump, "nfvis-storm-ens224-multicast")
	if !ok || c.ConformPackets != 42 {
		t.Fatalf("按名应解析出计数: %+v ok=%v", c, ok)
	}
	if _, ok := StormCountersFromDump(dump, "nfvis-storm-ens999-broadcast"); ok {
		t.Fatal("不存在的 policer 应 ok=false（不返回零值当真值）")
	}
	if _, ok := StormCountersFromDump(dump, ""); ok {
		t.Fatal("空名应 ok=false（不猜任何对象）")
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

// ---------- 绑定事实三态与清扫/删除安全化（决策 #421） ----------
//
// 真机由来：本底座（VPP 26.06）`classify_table_by_interface` 对 policer-classify 绑定恒回
// l2_table_id=NONE、`policer_classify_dump` 恒 0 条目（自建 govpp 探针实测），而 vppctl 里
// 绑定在场。把「实况阴性」当「未挂」的直接后果：孤儿清扫删掉接口 L2 槽正挂着的、正在生效的
// 风暴分类表 ⇒ 槽悬空指向已释放的表 ⇒ 首包在 policer 插件里空指针（VPP 崩溃循环，
// restart counter 实测到 23）；巡检又据阴性反复「清登记→重放」，是同一循环的放大器。

// TestStormSweepAbortsWhenBindingUnreadable（决策 #421②）：实况阴性（本底座读不到绑定）而
// 登记在位 ⇒ **整轮放弃清扫**（宁留孤儿不误删）——接口槽上正在生效的表绝不因「API 读不到」
// 被当孤儿删掉。
//
// 修复前：保护集只由实况阳性绑定构成（本底座恒空）⇒ 登记在位的一对表被当孤儿删除
// （接口槽悬空 ⇒ 首包空指针崩溃）。
func TestStormSweepAbortsWhenBindingUnreadable(t *testing.T) {
	c := newFakeStormClient()
	c.blindAttached = true // 本底座：两个 API 都读不到 policer-classify 绑定
	p := NewStormProvider(c)
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 20000)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(c.tables) != 2 || len(c.attached) != 1 {
		t.Fatalf("前提：接口 L2 槽上挂着一对表: %s", c.snapshot())
	}
	c.calls = nil
	if err := p.sweepOrphanTables(c); err != nil {
		t.Fatalf("清扫: %v", err)
	}
	if len(c.tables) != 2 || len(c.attached) != 1 {
		t.Fatalf("登记在位而实况不可回读时不得清扫任何表（宁留孤儿不误删）: %s", c.snapshot())
	}
}

// TestStormSweepProtectsRegisteredTables（决策 #421②）：保护集 = 全部接口的实况阳性绑定
// ∪ **全部接口的登记表及其链**（沿 NextTableIndex）——登记指向、实况读不到的表（本底座常态）
// 一律不动；只清「实况不属于任何接口、且无任何登记指向」的已知形状表。
//
// 修复前：保护集只看实况阳性 ⇒ 登记表（别的一对）被当孤儿清掉。
func TestStormSweepProtectsRegisteredTables(t *testing.T) {
	c := newFakeStormClient()
	c.ifidx["ens224"] = 9
	p := NewStormProvider(c)
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 0)); err != nil {
		t.Fatalf("Apply(ens192): %v", err)
	}
	ens224 := model.InterfaceConfig{Name: "ens224", StormControl: &model.StormControl{BroadcastKbps: 4000}}
	if err := p.ApplyInterface(context.Background(), ens224); err != nil {
		t.Fatalf("Apply(ens224): %v", err)
	}
	// ens224 的登记换成另一对表（上一次成功下发的坐标），实况仍挂在原来那张上（陈旧登记）。
	m2 := c.seedStormTable(StormKindMulticast, ^uint32(0))
	b3 := c.seedStormTable(StormKindBroadcast, m2)
	p.mu.Lock()
	p.rt["ens224"].kinds[StormKindBroadcast].tableIdx = b3
	p.rt["ens224"].attachTable = b3
	p.mu.Unlock()
	// 一对真正的孤儿：没有绑定，也没有任何登记指向。
	oM := c.seedStormTable(StormKindMulticast, ^uint32(0))
	oB := c.seedStormTable(StormKindBroadcast, oM)
	if err := p.sweepOrphanTables(c); err != nil {
		t.Fatalf("清扫: %v", err)
	}
	for _, id := range []uint32{m2, b3} {
		if _, ok := c.tables[id]; !ok {
			t.Fatalf("登记指向的表 %d（及其链）不得被清: %s", id, c.snapshot())
		}
	}
	for _, id := range []uint32{oB, oM} {
		if _, ok := c.tables[id]; ok {
			t.Fatalf("无绑定且无登记指向的孤儿表 %d 应被清: %s", id, c.snapshot())
		}
	}
}

// TestStormTeardownConfirmsUnbindBeforeDelete（决策 #421③）：删表前必须按该表的索引解绑并拿到
// 确认——解绑拿不到确认（非「本就不在」的失败）时**一张表都不删**，如实返回错误（宁留孤儿不误删）。
//
// 修复前：登记里的逐类表直接删（没有任何解绑动作）⇒ 解绑失败也照删/照报成功。
func TestStormTeardownConfirmsUnbindBeforeDelete(t *testing.T) {
	c := newFakeStormClient()
	c.blindAttached = true
	p := NewStormProvider(c)
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 0)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// 现场：槽上的绑定在数据面侧消失（本底座实况本就不可回读），登记退化为「未声称在位」、
	// 只残留逐类表索引——正是旧实现「不先解绑就删表」的形态。
	delete(c.attached, 7)
	p.mu.Lock()
	p.rt["ens192"].attached = false
	p.mu.Unlock()
	c.calls = nil
	c.failOn = "detach:if=7:table=0" // 该表的解绑拿不到确认
	err := p.ApplyInterface(context.Background(), stormCfg(0, 0))
	if err == nil {
		t.Fatalf("解绑未获确认时应如实报错（不得继续删表）: %s", c.snapshot())
	}
	if len(c.tables) != 1 || c.sessions[0] != 1 {
		t.Fatalf("解绑未获确认的那张表及其会话不得被删: %s", c.snapshot())
	}
	if len(c.pols) != 1 {
		t.Fatalf("清理应中止在报错点，policer 不得被删: %s", c.snapshot())
	}
}

// TestStormTeardownDeclaredUnbindsByRegistration（决策 #421③）：实况不可回读时，解绑走**登记索引**
// （登记是「槽上挂着哪张表」的唯一线索），解绑确认后才删表——先解绑、后删表的顺序保持。
func TestStormTeardownDeclaredUnbindsByRegistration(t *testing.T) {
	c := newFakeStormClient()
	c.blindAttached = true
	p := NewStormProvider(c)
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 20000)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	c.calls = nil
	if err := p.ApplyInterface(context.Background(), stormCfg(0, 0)); err != nil {
		t.Fatalf("清空: %v", err)
	}
	got := strings.Join(c.calls, "\n")
	if !strings.Contains(got, "detach:if=7:table=1") {
		t.Fatalf("应按登记索引（槽上的广播表 1）解绑: %s", got)
	}
	if idx, del := strings.Index(got, "detach:if=7:table=1"), strings.Index(got, "table-del:1:chain=true"); idx < 0 || del < 0 || idx > del {
		t.Fatalf("顺序应为先解绑、后删表: %s", got)
	}
	if !c.clean() {
		t.Fatalf("解绑确认后应清干净: %s", c.snapshot())
	}
}

// TestStormTeardownRefusesDeleteWhenUnbindNotAchieved（决策 #421③）：实况阳性时解绑后必须**复核**——
// 仍读到阳性绑定说明解绑未达成，此时一张表都不删（真机上正是「表被删而接口槽仍指向它」的崩溃形态）。
//
// 修复前：解绑发出即当已达成（不看复核）⇒ 表被删而槽悬空。
func TestStormTeardownRefusesDeleteWhenUnbindNotAchieved(t *testing.T) {
	c := newFakeStormClient()
	p := NewStormProvider(c)
	if err := p.ApplyInterface(context.Background(), stormCfg(8000, 0)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	c.livePin = map[uint32]uint32{7: 0} // 解绑后实况仍读到同一张表（解绑没真正达成）
	err := p.ApplyInterface(context.Background(), stormCfg(0, 0))
	if err == nil {
		t.Fatalf("解绑未达成时应如实报错（不得继续删表）: %s", c.snapshot())
	}
	if len(c.tables) != 1 || c.sessions[0] != 1 {
		t.Fatalf("解绑未达成时不得删表: %s", c.snapshot())
	}
}

// TestStormReconcileKeepsDeclaredBinding（决策 #421③）：实况阴性（本底座读不到绑定）而登记在位、
// 表仍在 ⇒ 巡检必须判「在场」，零重放——不得据阴性反复拆建（那是真机崩溃循环的放大器）。
//
// 修复前：巡检只据实况阴性就清登记重放，每次重放都伴随删表/重建（接口槽一度悬空 ⇒ 崩溃）。
func TestStormReconcileKeepsDeclaredBinding(t *testing.T) {
	ctx := context.Background()
	c := newFakeStormClient()
	c.blindAttached = true
	p := NewStormProvider(c)
	if err := p.ApplyInterface(ctx, stormCfg(8000, 20000)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	before := c.snapshot()
	c.calls = nil
	cfg := model.Config{Interfaces: []model.InterfaceConfig{stormCfg(8000, 20000)}}
	if errs := p.Reconcile(ctx, cfg); len(errs) != 0 {
		t.Fatalf("Reconcile: %v", errs)
	}
	for _, line := range c.calls {
		for _, bad := range []string{"policer-del", "policer-add", "table-del", "table-add", "session-add", "attach:", "detach:"} {
			if strings.HasPrefix(line, bad) {
				t.Fatalf("登记在位且表仍在时应视为在场，不得重放: %v", c.calls)
			}
		}
	}
	if c.snapshot() != before {
		t.Fatalf("巡检不得改动数据面: before=%s after=%s", before, c.snapshot())
	}
}

// TestStormBindingFactFourStates（决策 #421①，收口后 +自认领）：绑定事实四态——实况回读
// （权威）/按登记/自认领/未挂；「按登记」「自认领」各自给出对应表（TableByRegistration /
// TableAdopted）且绝不冒充实况（Table 保持空）。
func TestStormBindingFactFourStates(t *testing.T) {
	ctx := context.Background()

	// ① 实况回读：API 阳性 ⇒ 权威态
	c := newFakeStormClient()
	p := NewStormProvider(c)
	if err := p.ApplyInterface(ctx, stormCfg(8000, 0)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	dp, err := p.Dataplane(ctx, "ens192")
	if err != nil {
		t.Fatalf("Dataplane: %v", err)
	}
	if dp.Binding != StormBindingLive || !dp.Attached || dp.AttachedL2Table != 0 {
		t.Fatalf("实况回读态不符: %+v", dp)
	}
	if kd := dp.Kinds[StormKindBroadcast]; kd.Table == nil || kd.TableByRegistration != nil {
		t.Fatalf("实况回读态应给实测表、不给按登记表: %+v", kd)
	}

	// ② 按登记：实况阴性（本底座读不到该绑定）但登记在位、登记的表仍在
	c2 := newFakeStormClient()
	c2.blindAttached = true
	p2 := NewStormProvider(c2)
	if err := p2.ApplyInterface(ctx, stormCfg(8000, 0)); err != nil {
		t.Fatalf("Apply(blind): %v", err)
	}
	dp2, err := p2.Dataplane(ctx, "ens192")
	if err != nil {
		t.Fatalf("Dataplane(blind): %v", err)
	}
	if dp2.Binding != StormBindingDeclared || dp2.Attached || dp2.DeclaredTable != 0 {
		t.Fatalf("按登记态不符: %+v", dp2)
	}
	kd2 := dp2.Kinds[StormKindBroadcast]
	if kd2.Table != nil || kd2.TableByRegistration == nil || kd2.TableByRegistration.Index != 0 {
		t.Fatalf("按登记态应在 TableByRegistration 给出登记表（不冒充实况）: %+v", kd2)
	}

	// ③ 未挂：实况阴性且登记的表已不在数据面（如被带外删除）
	delete(c2.tables, 0)
	dp3, err := p2.Dataplane(ctx, "ens192")
	if err != nil {
		t.Fatalf("Dataplane(表已删): %v", err)
	}
	if dp3.Binding != StormBindingNone || dp3.Attached {
		t.Fatalf("未挂态不符: %+v", dp3)
	}
	kd3 := dp3.Kinds[StormKindBroadcast]
	if kd3.Table != nil || kd3.TableByRegistration != nil {
		t.Fatalf("未挂态不得给任何「在位」表: %+v", kd3)
	}

	// ④ 自认领：登记为空（进程重启）但数据面对象在位、绑定不可回读 ⇒ 认回来后判在位，
	// 归属与依据（policer 按名在场 + 表链匹配）与前三态分列。
	_, p4 := adoptedFixture(t)
	if errs := p4.Reconcile(ctx, model.Config{Interfaces: []model.InterfaceConfig{stormCfg(8000, 20000)}}); len(errs) != 0 {
		t.Fatalf("自认领应先成立: %v", errs)
	}
	dp4, err := p4.Dataplane(ctx, "ens192")
	if err != nil {
		t.Fatalf("Dataplane(自认领): %v", err)
	}
	if dp4.Binding != StormBindingAdopted || dp4.Attached || dp4.AdoptedTable != 1 || dp4.DeclaredTable != 0 {
		t.Fatalf("自认领态不符: %+v", dp4)
	}
	kd4 := dp4.Kinds[StormKindBroadcast]
	if kd4.Table != nil || kd4.TableByRegistration != nil || kd4.TableAdopted == nil || kd4.TableAdopted.Index != 1 {
		t.Fatalf("自认领态应在 TableAdopted 给出表（不冒充其它态）: %+v", kd4)
	}
}

// ---------- 自认领（adoption）：登记丢失后从数据面把本产品的对象认回来（决策 #421 收口） ----------

// stormDestructiveCalls 列出「改动数据面」的调用（认领/在场复核的用例里断言零改动用）。
func stormDestructiveCalls(c *fakeStormClient) []string {
	var out []string
	for _, line := range c.calls {
		for _, p := range []string{"policer-del", "policer-add", "table-del", "table-add", "session-del", "session-add", "attach:", "detach:"} {
			if strings.HasPrefix(line, p) {
				out = append(out, line)
				break
			}
		}
	}
	return out
}

// adoptedFixture 造「数据面已有一对本产品对象、进程登记为空」的现场（等价 nfvisd 重启：
// VPP 存活、槽上仍挂着我们的表、绑定不可回读）。返回新进程的 Provider（与旧进程共用数据面）。
func adoptedFixture(t *testing.T) (*fakeStormClient, *StormProvider) {
	t.Helper()
	c := newFakeStormClient()
	c.blindAttached = true // 本底座：绑定不可回读
	seed := NewStormProvider(c)
	if err := seed.ApplyInterface(context.Background(), stormCfg(8000, 20000)); err != nil {
		t.Fatalf("前置下发: %v", err)
	}
	// 新进程：登记为空（旧进程的登记随进程消失）
	return c, NewStormProvider(c)
}

// TestStormAdoptsAfterRegistrationLoss（决策 #421 收口①）：绑定不可回读 + 登记为空时，
// 按「policer 按名在场 + 表链自洽」自认领已存在的数据面对象——**不删不建**，巡检判在场。
func TestStormAdoptsAfterRegistrationLoss(t *testing.T) {
	ctx := context.Background()
	c, p := adoptedFixture(t)
	before := c.snapshot()
	c.calls = nil
	cfg := model.Config{Interfaces: []model.InterfaceConfig{stormCfg(8000, 20000)}}
	if errs := p.Reconcile(ctx, cfg); len(errs) != 0 {
		t.Fatalf("登记为空但对象在位时应自认领判在场，实际报未收敛: %v", errs)
	}
	if bad := stormDestructiveCalls(c); len(bad) != 0 {
		t.Fatalf("自认领不得删/建任何对象，实际: %v", bad)
	}
	if c.snapshot() != before {
		t.Fatalf("自认领不得改动数据面: before=%s after=%s", before, c.snapshot())
	}
	// 认领后不得再判缺项：重复巡检仍判在场、仍零改动。
	c.calls = nil
	if errs := p.Reconcile(ctx, cfg); len(errs) != 0 {
		t.Fatalf("认领后重复巡检应判在场: %v", errs)
	}
	if bad := stormDestructiveCalls(c); len(bad) != 0 {
		t.Fatalf("认领后巡检不得再改动数据面: %v", bad)
	}
	// 读视图四态（决策 #421 收口③）：自认领在位 + 归属（挂广播表 1、链上组播表 0）。
	dp, err := p.Dataplane(ctx, "ens192")
	if err != nil {
		t.Fatalf("Dataplane: %v", err)
	}
	if dp.Binding != StormBindingAdopted || dp.Attached || dp.AdoptedTable != 1 {
		t.Fatalf("读视图应报自认领（挂广播表 1）: %+v", dp)
	}
	b, m := dp.Kinds[StormKindBroadcast], dp.Kinds[StormKindMulticast]
	if b.Table != nil || b.TableAdopted == nil || b.TableAdopted.Index != 1 {
		t.Fatalf("广播类应按自认领给表 1: %+v", b)
	}
	if m.Table != nil || m.TableAdopted == nil || m.TableAdopted.Index != 0 {
		t.Fatalf("组播类应按自认领给表 0: %+v", m)
	}

	// 恢复重放路径（EnsureConsistent → ApplyInterface）：登记为空时同样先自认领，认回来且
	// 与声明一致（CIR 与配置一致）即零改动返回。
	c.calls = nil
	p2 := NewStormProvider(c)
	if err := p2.ApplyInterface(ctx, stormCfg(8000, 20000)); err != nil {
		t.Fatalf("恢复重放: %v", err)
	}
	if bad := stormDestructiveCalls(c); len(bad) != 0 {
		t.Fatalf("恢复重放认出既有对象后不得删/建: %v", bad)
	}
	if c.snapshot() != before {
		t.Fatalf("恢复重放不得改动数据面: before=%s after=%s", before, c.snapshot())
	}
}

// TestStormAdoptionRefusedWhenPatternMismatch（决策 #421 收口①）：认领条件（policer 按名在场、
// 掩码形状、表链自洽、会话数 ≥1）任一不满足即**放弃认领**：不删、不建，按未收敛如实报出。
func TestStormAdoptionRefusedWhenPatternMismatch(t *testing.T) {
	ctx := context.Background()
	cfg := model.Config{Interfaces: []model.InterfaceConfig{stormCfg(8000, 20000)}}
	for _, tc := range []struct {
		name   string
		break_ func(c *fakeStormClient)
	}{
		{"policer 按名缺失", func(c *fakeStormClient) { delete(c.pols, "nfvis-storm-ens192-multicast") }},
		{"表链断裂", func(c *fakeStormClient) { ti := c.tables[1]; ti.NextTableIndex = ^uint32(0); c.tables[1] = ti }},
		{"掩码形状不符", func(c *fakeStormClient) { ti := c.tables[1]; ti.Mask = "deadbeef"; c.tables[1] = ti }},
		{"表上无会话", func(c *fakeStormClient) { c.sessions[1] = 0 }},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c, p := adoptedFixture(t)
			tc.break_(c)
			before := c.snapshot()
			c.calls = nil
			errs := p.Reconcile(ctx, cfg)
			if len(errs) == 0 {
				t.Fatalf("形态不自洽时不得认领，应如实报未收敛: %s", c.snapshot())
			}
			if bad := stormDestructiveCalls(c); len(bad) != 0 {
				t.Fatalf("放弃认领时不得删/建任何对象: %v", bad)
			}
			if c.snapshot() != before {
				t.Fatalf("放弃认领不得改动数据面: before=%s after=%s", before, c.snapshot())
			}
		})
	}
}

// TestStormSweepOnlyIdentifiesOnBlindBase（决策 #421 收口②）：绑定不可回读的底座上，
// 孤儿清扫**只识别不删**——形状属本产品但不被绑定/登记覆盖的表，一张都不许删。
func TestStormSweepOnlyIdentifiesOnBlindBase(t *testing.T) {
	c := newFakeStormClient()
	c.blindAttached = true
	p := NewStormProvider(c)
	// 一对本产品形状的表：既没有绑定、也没有登记指向（“候选”）
	oM := c.seedStormTable(StormKindMulticast, ^uint32(0))
	oB := c.seedStormTable(StormKindBroadcast, oM)
	c.calls = nil
	if err := p.sweepOrphanTables(c); err != nil {
		t.Fatalf("只识别不删不应报错: %v", err)
	}
	if bad := stormDestructiveCalls(c); len(bad) != 0 {
		t.Fatalf("绑定不可回读时清扫不得执行任何删表: %v", bad)
	}
	if _, ok := c.tables[oB]; !ok {
		t.Fatalf("候选表 %d 不得被删: %s", oB, c.snapshot())
	}
	if _, ok := c.tables[oM]; !ok {
		t.Fatalf("候选链上表 %d 不得被删: %s", oM, c.snapshot())
	}
}

// TestStormAdoptThenVPPRestartStillRebuilds（决策 #421 收口④）：认领过之后数据面整体重启
// （对象全无、连接重连触发 reset）仍能正常按声明重建——认领不改变「无对象即建」的路径。
func TestStormAdoptThenVPPRestartStillRebuilds(t *testing.T) {
	ctx := context.Background()
	c, p := adoptedFixture(t)
	cfg := model.Config{Interfaces: []model.InterfaceConfig{stormCfg(8000, 20000)}}
	if errs := p.Reconcile(ctx, cfg); len(errs) != 0 {
		t.Fatalf("认领应先成立: %v", errs)
	}
	// VPP 重启：对象全失 + 重连触发 reset（登记清空）
	for id := range c.tables {
		delete(c.tables, id)
		delete(c.sessions, id)
	}
	c.pols = map[string]uint32{}
	c.attached = map[uint32]uint32{}
	p.reset()
	c.calls = nil
	if errs := p.Reconcile(ctx, cfg); len(errs) != 0 {
		t.Fatalf("对象全无时应按声明重建: %v", errs)
	}
	got := strings.Join(c.calls, "\n")
	for _, want := range []string{"policer-add:nfvis-storm-ens192-broadcast:8000", "policer-add:nfvis-storm-ens192-multicast:20000", "table-add:", "attach:if=7"} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺少重建步骤 %q:\n%s", want, got)
		}
	}
	if len(c.tables) != 2 || len(c.pols) != 2 || len(c.attached) != 1 {
		t.Fatalf("重建后数据面不齐: %s", c.snapshot())
	}
}

// TestStormOrphanCandidatesReportedNotDeleted（决策 #421 收口②③）：形状属本产品却不被绑定/
// 登记覆盖的表在巡检里**如实回报**（未收敛项 + 读视图候选），但**一张都不删**。
func TestStormOrphanCandidatesReportedNotDeleted(t *testing.T) {
	ctx := context.Background()
	cfg := model.Config{Interfaces: []model.InterfaceConfig{stormCfg(8000, 20000)}}
	c, p := adoptedFixture(t)
	if errs := p.Reconcile(ctx, cfg); len(errs) != 0 {
		t.Fatalf("自认领应先成立: %v", errs)
	}
	// 历史遗留的一对表（未被任何绑定/登记覆盖）——巡检应只识别不删。
	oM := c.seedStormTable(StormKindMulticast, ^uint32(0))
	oB := c.seedStormTable(StormKindBroadcast, oM)
	before := c.snapshot()
	c.calls = nil
	errs := p.Reconcile(ctx, cfg)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "只识别不删") ||
		!strings.Contains(errs[0].Error(), "#2") || !strings.Contains(errs[0].Error(), "#3") {
		t.Fatalf("候选应如实回报（未收敛项含候选索引）: %v", errs)
	}
	dp, err := p.Dataplane(ctx, "ens192")
	if err != nil {
		t.Fatalf("Dataplane: %v", err)
	}
	if len(dp.OrphanCandidates) != 2 || dp.OrphanCandidates[0] != oM || dp.OrphanCandidates[1] != oB {
		t.Fatalf("读视图应给候选（只识别）: %+v", dp.OrphanCandidates)
	}
	if dp.Binding != StormBindingAdopted {
		t.Fatalf("前提：本接口应处于自认领态: %+v", dp)
	}
	if bad := stormDestructiveCalls(c); len(bad) != 0 {
		t.Fatalf("只识别不删：不得有任何改动调用: %v", bad)
	}
	if _, ok := c.tables[oB]; !ok {
		t.Fatalf("候选表不得被删: %s", c.snapshot())
	}
	if c.snapshot() != before {
		t.Fatalf("巡检不得改动数据面: before=%s after=%s", before, c.snapshot())
	}
	// 候选随数据面消失（如数据面重启）而消解：识别结果同源刷新，不再虚报。
	delete(c.tables, oB)
	delete(c.tables, oM)
	if errs := p.Reconcile(ctx, cfg); len(errs) != 0 {
		t.Fatalf("候选消失后不应再报未收敛: %v", errs)
	}
	if dp, _ := p.Dataplane(ctx, "ens192"); len(dp.OrphanCandidates) != 0 {
		t.Fatalf("候选消解后读视图不得再列: %+v", dp.OrphanCandidates)
	}
}

// ---------- 登记态表的删前形状复核 + 计数按名（决策 #429） ----------
//
// 由来（#421 收口后如实登记的残余）：① `teardown` 对「登记态」的表只做解绑尝试、**删前不核对
// 形状**——若某登记索引被 VPP 复用为外来表（别的插件/手工建的分类表），理论上仍可能误删；
// ② 自认领态的 policer **索引**不可回读（`policer_dump` 无索引字段），登记里用哨兵占位，计数
// 读取必须**按 policer 名**才不会错配。

// TestStormTeardownSkipsDeleteWhenRegisteredTableShapeMismatch（决策 #429①）：登记指向的表被
// 换成**形状不符**的表（索引被复用给外来对象 / 换成了另一类的表）时，删登记态的表之前必须读
// 实测掩码并与**本类**风暴掩码核对——不符即**一张表都不删**，并把「登记指向的表形状不符
// （疑似索引被复用），已跳过删除」如实报进未收敛项。
//
// 修复前：解绑拿到确认（或按「本就不在」容忍）后直接按登记索引删表 ⇒ 删到外来对象。
func TestStormTeardownSkipsDeleteWhenRegisteredTableShapeMismatch(t *testing.T) {
	ctx := context.Background()
	const skipMsg = "登记指向的表形状不符（疑似索引被复用），已跳过删除"

	t.Run("登记索引被复用为外来形状的表", func(t *testing.T) {
		c := newFakeStormClient()
		c.blindAttached = true // 本底座：绑定不可回读（登记是「槽上挂着哪张表」的唯一线索）
		p := NewStormProvider(c)
		if err := p.ApplyInterface(ctx, stormCfg(8000, 0)); err != nil {
			t.Fatalf("前置下发: %v", err)
		}
		// 带外把登记指向的索引换成外来表（掩码与本产品两类都不符）。
		foreign := "deadbeef000000000000000000000000"
		c.tables[0] = StormTableInfo{Index: 0, Mask: foreign, NextTableIndex: ^uint32(0)}
		c.calls = nil
		err := p.ApplyInterface(ctx, stormCfg(9000, 0))
		if err == nil || !strings.Contains(err.Error(), skipMsg) {
			t.Fatalf("形状不符时应如实报「已跳过删除」: %v", err)
		}
		for _, line := range c.calls {
			if strings.HasPrefix(line, "table-del") || strings.HasPrefix(line, "session-del") {
				t.Fatalf("形状不符时不得删表/删会话: %v", c.calls)
			}
		}
		if ti, ok := c.tables[0]; !ok || ti.Mask != foreign {
			t.Fatalf("外来表不得被删: %s", c.snapshot())
		}
		if _, ok := c.pols["nfvis-storm-ens192-broadcast"]; !ok {
			t.Fatalf("如实报错即中止，后续清理不得继续（policer 应仍在）: %s", c.snapshot())
		}
		// 未收敛项（15s 巡检与提交编排同源）：复核把同一句如实报出来，且仍不做任何删除。
		c.calls = nil
		errs := p.Reconcile(ctx, model.Config{Interfaces: []model.InterfaceConfig{stormCfg(9000, 0)}})
		if len(errs) == 0 || !strings.Contains(errs[0].Error(), skipMsg) {
			t.Fatalf("巡检应把该未收敛项如实报出: %v", errs)
		}
		for _, line := range c.calls {
			if strings.HasPrefix(line, "table-del") {
				t.Fatalf("巡检同样不得按陈旧登记删表: %v", c.calls)
			}
		}
		if _, ok := c.tables[0]; !ok {
			t.Fatalf("外来表不得被删（巡检）: %s", c.snapshot())
		}
	})

	t.Run("登记挂表被换成另一类的表", func(t *testing.T) {
		// 形状仍是本产品（stormMaskKnown 通过），但**不是本类**——核对必须按本类掩码，
		// 否则会把另一类的表（可能是别的接口/上一轮的对象）当自己的删掉。
		c := newFakeStormClient()
		c.blindAttached = true
		p := NewStormProvider(c)
		if err := p.ApplyInterface(ctx, stormCfg(8000, 0)); err != nil {
			t.Fatalf("前置下发: %v", err)
		}
		ti := c.tables[0]
		ti.Mask = fmt.Sprintf("%x", stormMask(StormKindMulticast))
		c.tables[0] = ti
		c.calls = nil
		err := p.ApplyInterface(ctx, stormCfg(0, 0))
		if err == nil || !strings.Contains(err.Error(), skipMsg) || !strings.Contains(err.Error(), "broadcast") {
			t.Fatalf("挂表形状不是本类时应如实报「已跳过删除」并点名本类: %v", err)
		}
		for _, line := range c.calls {
			if strings.HasPrefix(line, "table-del") || strings.HasPrefix(line, "session-del") {
				t.Fatalf("形状不符时不得删表/删会话: %v", c.calls)
			}
		}
		if _, ok := c.tables[0]; !ok {
			t.Fatalf("形状不符的表不得被删: %s", c.snapshot())
		}
	})

	t.Run("链上那张被换成外来形状的表", func(t *testing.T) {
		// 两类并存：广播表（挂表）用 NextTableIndex 链着组播表，带链删会**顺着链删掉链目标**——
		// 链目标同样是登记态表、索引同样可能被复用，故链目标也要按形状核对：不符即**不带链删**
		//（只删本类那张），并由逐类核对如实报「已跳过删除」。
		//
		// 修复前：带链删直接删掉链上的外来对象，且逐类核对再也读不到它（表已不存在）⇒ 静默无报。
		c := newFakeStormClient()
		p := NewStormProvider(c)
		if err := p.ApplyInterface(ctx, stormCfg(8000, 20000)); err != nil {
			t.Fatalf("前置下发: %v", err)
		}
		// 现场：链目标（表 0，组播）被换成外来形状；挂表（表 1，广播）仍挂在接口上。
		foreign := "00112233445566778899aabbccddeeff"
		c.tables[0] = StormTableInfo{Index: 0, Mask: foreign, NextTableIndex: ^uint32(0)}
		c.calls = nil
		err := p.ApplyInterface(ctx, stormCfg(0, 0))
		if err == nil || !strings.Contains(err.Error(), skipMsg) || !strings.Contains(err.Error(), StormKindMulticast) {
			t.Fatalf("链目标形状不符时应如实报「已跳过删除」并点名组播类: %v", err)
		}
		got := strings.Join(c.calls, "\n")
		if strings.Contains(got, "table-del:1:chain=true") {
			t.Fatalf("链目标形状不符时不得带链删（会波及外来对象）:\n%s", got)
		}
		if !strings.Contains(got, "table-del:1:chain=false") {
			t.Fatalf("本类那张仍应照常删（只是不带链）:\n%s", got)
		}
		if _, ok := c.tables[0]; !ok {
			t.Fatalf("链上的外来表不得被删: %s", c.snapshot())
		}
		if _, ok := c.tables[1]; ok {
			t.Fatalf("本类表（广播表 1）应被删: %s", c.snapshot())
		}
	})
}

// TestStormTeardownSkipsDeleteWhenRegisteredTableGone（决策 #429①）：登记指向的索引上已无表
// （带外删除等）时，删前形状复核**读不到属性** ⇒ **不发删除调用**（不按陈旧坐标删——这正是
// 「读取与删除之间索引被复用」的窗口），并按「本就不在」＝已是目标状态推进登记（stormAbsentOK
// 口径：不报错），随后的重建照常把声明补回。
//
// 修复前：仍按陈旧索引发一次删表（靠容忍 No such table 收场；索引若在这两步之间被复用即误删）。
func TestStormTeardownSkipsDeleteWhenRegisteredTableGone(t *testing.T) {
	ctx := context.Background()
	c := newFakeStormClient()
	c.blindAttached = true
	p := NewStormProvider(c)
	if err := p.ApplyInterface(ctx, stormCfg(8000, 0)); err != nil {
		t.Fatalf("前置下发: %v", err)
	}
	delete(c.tables, 0) // 带外删除登记指向的表（登记仍是旧坐标）
	c.calls = nil
	if err := p.ApplyInterface(ctx, stormCfg(9000, 0)); err != nil {
		t.Fatalf("表已不存在时应按已达成（不报错）: %v", err)
	}
	for _, line := range c.calls {
		if strings.HasPrefix(line, "table-del") {
			t.Fatalf("属性读不到时不得发删除调用: %v", c.calls)
		}
	}
	if !strings.Contains(strings.Join(c.calls, "\n"), "table-info:0") {
		t.Fatalf("应留下删前形状复核的实测读取: %v", c.calls)
	}
	// 重建照常：新表 + 会话 + 挂接口 + policer 都在（改值声明已收敛）
	if len(c.tables) != 1 || len(c.pols) != 1 {
		t.Fatalf("表已不存在后应正常重建: %s", c.snapshot())
	}
	tbl, ok := c.attached[7]
	if !ok {
		t.Fatalf("重建后应挂上接口: %s", c.snapshot())
	}
	if _, ok := c.tables[tbl]; !ok || c.sessions[tbl] != 1 {
		t.Fatalf("重建后的表/会话应在位: %s", c.snapshot())
	}
}

// TestStormDataplaneCountersByName（决策 #429②）：计数读取一律按 policer **名**——
//  1. 自认领态（policer 索引不可回读，登记里是哨兵）按名命中并给出计数；
//  2. 名字在数据面查不到 ⇒ 如实报「计数不可读（policer 不在数据面）」，且**不调用**读数来源
//     （没有对象可读，也就不按索引猜）。
//
// 修复前：读数来源收到的是登记里的索引（自认领态是哨兵）——索引既不可回读、又可能被复用，
// 按索引匹配会把别的 policer 的计数读成本产品的计数。
func TestStormDataplaneCountersByName(t *testing.T) {
	ctx := context.Background()
	c, p := adoptedFixture(t)
	cr := &fakeStormCounters{cnt: StormCounters{ConformPackets: 77, ViolatePackets: 3}, ok: true}
	p.SetCountersReader(cr)
	if errs := p.Reconcile(ctx, model.Config{Interfaces: []model.InterfaceConfig{stormCfg(8000, 20000)}}); len(errs) != 0 {
		t.Fatalf("自认领应先成立: %v", errs)
	}
	reg := p.regSnapshot("ens192")
	if e := reg.kinds[StormKindBroadcast]; e == nil || e.policerIdx != stormPolicerIdxUnknown {
		t.Fatalf("前提：自认领登记的 policer 索引应是「不可回读」哨兵: %+v", reg.kinds)
	}
	dp, err := p.Dataplane(ctx, "ens192")
	if err != nil {
		t.Fatalf("Dataplane: %v", err)
	}
	if dp.Binding != StormBindingAdopted {
		t.Fatalf("前提：应为自认领态: %+v", dp)
	}
	for _, kind := range []string{StormKindBroadcast, StormKindMulticast} {
		kd := dp.Kinds[kind]
		if kd.Counters == nil || kd.Counters.ConformPackets != 77 || kd.Counters.ViolatePackets != 3 {
			t.Fatalf("%s 应按名读到计数: %+v", kind, kd)
		}
		if kd.PolicerIndex != stormPolicerIdxUnknown {
			t.Fatalf("%s 的 policer 索引不可回读，读视图应如实保留哨兵: %+v", kind, kd)
		}
	}
	want := "nfvis-storm-ens192-broadcast,nfvis-storm-ens192-multicast"
	if got := strings.Join(cr.names, ","); got != want {
		t.Fatalf("读数来源应按 policer 名调用: got=%s want=%s", got, want)
	}

	// 名字在数据面查不到：如实报不可读，且**不去问**读数来源（不按索引猜）。
	delete(c.pols, "nfvis-storm-ens192-broadcast")
	cr.names = nil
	dp2, err := p.Dataplane(ctx, "ens192")
	if err != nil {
		t.Fatalf("Dataplane(policer 缺失): %v", err)
	}
	kd := dp2.Kinds[StormKindBroadcast]
	if kd.Counters != nil || kd.CountersReason != "计数不可读（policer 不在数据面）" {
		t.Fatalf("名字不在数据面时应如实报不可读（不给计数、不改口）: %+v", kd)
	}
	if got := strings.Join(cr.names, ","); got != "nfvis-storm-ens192-multicast" {
		t.Fatalf("名字不在数据面时不得调用读数来源: %v", cr.names)
	}
}

// TestStormTeardownReclaimsAdopted（决策 #429① 的正向用例）：自认领态（归属是推断来的、policer
// 索引为哨兵）的形状与本类一致 ⇒ 删前形状复核放行，回收照常：先解绑（拿到明确应答）→ 删表 →
// 按名清 policer → 三空。形状复核不得把合法的自认领对象挡在门外。
func TestStormTeardownReclaimsAdopted(t *testing.T) {
	ctx := context.Background()
	c, p := adoptedFixture(t)
	if errs := p.Reconcile(ctx, model.Config{Interfaces: []model.InterfaceConfig{stormCfg(8000, 20000)}}); len(errs) != 0 {
		t.Fatalf("自认领应先成立: %v", errs)
	}
	if dp, _ := p.Dataplane(ctx, "ens192"); dp.Binding != StormBindingAdopted {
		t.Fatalf("前提：应为自认领态: %+v", dp)
	}
	c.calls = nil
	if err := p.ApplyInterface(ctx, stormCfg(0, 0)); err != nil {
		t.Fatalf("自认领态回收: %v", err)
	}
	got := strings.Join(c.calls, "\n")
	if idx, del := strings.Index(got, "detach:if=7:table=1"), strings.Index(got, "table-del:1:chain=true"); idx < 0 || del < 0 || idx > del {
		t.Fatalf("自认领态应先解绑（明确应答）后删表: %s", got)
	}
	if !c.clean() {
		t.Fatalf("回收后应三空（表/policer/绑定）: %s", c.snapshot())
	}
}
