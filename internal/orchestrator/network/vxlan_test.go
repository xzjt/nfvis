package network

// VXLAN 编排单测（决策 #383）：纯函数（元组键/instance 分配/元组匹配）+ 假客户端下的
// 建隧/变更撤旧/删除/恢复重放（按 dump 元组匹配，不依赖进程内登记与接口名）。

import (
	"context"
	"fmt"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// fakeVxlanClient 记录调用序列的假客户端：tunnels 即 VPP 里"实际"的隧道表。
type fakeVxlanClient struct {
	lastIdx    uint32
	tunnels    []VxlanTunnelInfo
	calls      []string
	bdState    map[string]bool // "if|bd" → 是否在 BD 里
	dumpFail   bool
	addFail    bool
	bdFail     bool
	instanceOf uint32 // 记录最近一次建隧分配的 instance
}

func newFakeVxlanClient(existing ...VxlanTunnelInfo) *fakeVxlanClient {
	f := &fakeVxlanClient{bdState: map[string]bool{}}
	for _, t := range existing {
		if t.SwIfIndex > f.lastIdx {
			f.lastIdx = t.SwIfIndex
		}
		f.tunnels = append(f.tunnels, t)
	}
	return f
}

func (f *fakeVxlanClient) Close() {}

func (f *fakeVxlanClient) TunnelAddDel(isAdd bool, instance uint32, vni uint32, src, dst string, dstPort uint16) (uint32, error) {
	f.calls = append(f.calls, fmt.Sprintf("tunnel:%v:%d:%d:%s:%s:%d", isAdd, instance, vni, src, dst, dstPort))
	if f.addFail {
		return 0, fmt.Errorf("模拟建隧失败")
	}
	if isAdd {
		if t, ok := vxlanFindTuple(f.tunnels, vni, src, dst, dstPort); ok {
			return t.SwIfIndex, nil // 与 VPP 语义一致：元组已存在即返回既有接口
		}
		f.lastIdx++
		f.instanceOf = instance
		f.tunnels = append(f.tunnels, VxlanTunnelInfo{
			Instance: instance, Vni: vni, Src: src, Dst: dst, DstPort: dstPort, SwIfIndex: f.lastIdx,
		})
		return f.lastIdx, nil
	}
	out := make([]VxlanTunnelInfo, 0, len(f.tunnels))
	for _, t := range f.tunnels {
		if t.Vni == vni && t.Src == src && t.Dst == dst && t.DstPort == dstPort {
			continue
		}
		out = append(out, t)
	}
	f.tunnels = out
	return 0, nil
}

func (f *fakeVxlanClient) TunnelDump() ([]VxlanTunnelInfo, error) {
	if f.dumpFail {
		return nil, fmt.Errorf("模拟 dump 失败")
	}
	return append([]VxlanTunnelInfo{}, f.tunnels...), nil
}

func (f *fakeVxlanClient) SetInterfaceUp(swIfIndex uint32) error {
	f.calls = append(f.calls, fmt.Sprintf("up:%d", swIfIndex))
	return nil
}

func (f *fakeVxlanClient) SetL2Bridge(swIfIndex, bdID uint32, enable bool) error {
	f.calls = append(f.calls, fmt.Sprintf("bd:%d:%d:%v", swIfIndex, bdID, enable))
	if f.bdFail {
		return fmt.Errorf("模拟入 BD 失败")
	}
	f.bdState[fmt.Sprintf("%d|%d", swIfIndex, bdID)] = enable
	return nil
}

func vxlanOf(name string, vni int, local, remote string) model.VxlanTunnel {
	return model.VxlanTunnel{Name: name, Vni: vni, Local: local, Remote: remote}
}

// 纯函数：instance 分配取最小可用值（含空洞）。
func TestAllocateVxlanInstance(t *testing.T) {
	cases := []struct {
		used map[uint32]bool
		want uint32
	}{
		{map[uint32]bool{}, 0},
		{map[uint32]bool{0: true}, 1},
		{map[uint32]bool{0: true, 1: true}, 2},
		{map[uint32]bool{0: true, 1: true, 3: true}, 2}, // 空洞复用
		{map[uint32]bool{1: true, 2: true}, 0},
	}
	for i, c := range cases {
		if got := AllocateVxlanInstance(c.used); got != c.want {
			t.Fatalf("case %d: 分配 instance = %d，期望 %d", i, got, c.want)
		}
	}
}

// 纯函数：元组匹配按 (vni, src, dst, dst_port)——端口不同即不同隧道；instance 不参与匹配。
func TestVxlanFindTuple(t *testing.T) {
	infos := []VxlanTunnelInfo{
		{Instance: 7, Vni: 100, Src: "10.99.0.1", Dst: "10.99.0.2", DstPort: 4789, SwIfIndex: 6},
		{Instance: 8, Vni: 100, Src: "10.99.0.1", Dst: "10.99.0.2", DstPort: 5789, SwIfIndex: 7},
	}
	for _, c := range []struct {
		vni       uint32
		src, dst  string
		port      uint16
		wantFound bool
		wantIdx   uint32
	}{
		{100, "10.99.0.1", "10.99.0.2", 4789, true, 6},
		{100, "10.99.0.1", "10.99.0.2", 5789, true, 7}, // 端口是元组的一部分
		{100, "10.99.0.1", "10.99.0.9", 4789, false, 0},
		{101, "10.99.0.1", "10.99.0.2", 4789, false, 0},
	} {
		got, ok := vxlanFindTuple(infos, c.vni, c.src, c.dst, c.port)
		if ok != c.wantFound || (ok && got.SwIfIndex != c.wantIdx) {
			t.Fatalf("匹配 (%d,%s→%s,%d) = (%v,%d)，期望 (%v,%d)",
				c.vni, c.src, c.dst, c.port, ok, got.SwIfIndex, c.wantFound, c.wantIdx)
		}
	}
}

// 元组键由四要素构成（vni/src/dst/dst_port），任一不同即不同键。
func TestVxlanTupleKey(t *testing.T) {
	base := VxlanTupleKey(100, "10.0.0.1", "10.0.0.2", 4789)
	if base == VxlanTupleKey(100, "10.0.0.1", "10.0.0.2", 5789) {
		t.Fatal("dst_port 不同应为不同键")
	}
	if base == VxlanTupleKey(100, "10.0.0.9", "10.0.0.2", 4789) {
		t.Fatal("src 不同应为不同键")
	}
	if base == VxlanTupleKey(101, "10.0.0.1", "10.0.0.2", 4789) {
		t.Fatal("vni 不同应为不同键")
	}
}

// 建隧 + 入 BD + 置 up；重复 Apply 幂等（不再建、不新增隧道）。
func TestVxlanProviderApplyCreatesAndJoinsBD(t *testing.T) {
	f := newFakeVxlanClient()
	p := NewVxlanProvider(f)
	tun := vxlanOf("t1", 100, "10.99.0.1", "10.99.0.2")
	tun.VirtualSwitch = "vs1"

	if err := p.ApplyVxlan(context.Background(), tun); err != nil {
		t.Fatalf("ApplyVxlan: %v", err)
	}
	if len(f.tunnels) != 1 || f.tunnels[0].Vni != 100 || f.tunnels[0].DstPort != 4789 {
		t.Fatalf("隧道未按元组建成: %+v", f.tunnels)
	}
	if f.instanceOf != 0 {
		t.Fatalf("首条隧道应分配 instance 0，实际 %d", f.instanceOf)
	}
	if !f.bdState[fmt.Sprintf("%d|%d", f.tunnels[0].SwIfIndex, BDID("vs1"))] {
		t.Fatalf("隧道口未加入 vs1 的 BD: %v", f.bdState)
	}
	upCalls := 0
	for _, c := range f.calls {
		if c == fmt.Sprintf("up:%d", f.tunnels[0].SwIfIndex) {
			upCalls++
		}
	}
	if upCalls != 1 {
		t.Fatalf("应恰好置 up 一次: %v", f.calls)
	}

	// 幂等重放（恢复收敛同路径）：元组已存在 ⇒ 不重复建
	f.calls = nil
	if err := p.ApplyVxlan(context.Background(), tun); err != nil {
		t.Fatalf("重复 ApplyVxlan: %v", err)
	}
	for _, c := range f.calls {
		if len(c) >= 10 && c[:10] == "tunnel:tr" { // tunnel:true:…
			t.Fatalf("元组已存在时不应重复建隧: %v", f.calls)
		}
	}
	if len(f.tunnels) != 1 {
		t.Fatalf("不应产生第二条隧道: %+v", f.tunnels)
	}
}

// 改 remote ⇒ 先按旧元组撤、再建新（决策 #380 的教训），且旧隧道消失、新隧道在。
func TestVxlanProviderChangeRemoteWithdrawsOld(t *testing.T) {
	f := newFakeVxlanClient()
	p := NewVxlanProvider(f)
	old := vxlanOf("t1", 100, "10.99.0.1", "10.99.0.2")
	if err := p.ApplyVxlan(context.Background(), old); err != nil {
		t.Fatalf("首次 ApplyVxlan: %v", err)
	}
	f.calls = nil

	next := vxlanOf("t1", 100, "10.99.0.1", "10.99.0.3")
	if err := p.ApplyVxlan(context.Background(), next); err != nil {
		t.Fatalf("变更 ApplyVxlan: %v", err)
	}
	if len(f.calls) < 2 {
		t.Fatalf("变更应有撤旧+建新两次调用: %v", f.calls)
	}
	if f.calls[0] != "tunnel:false:0:100:10.99.0.1:10.99.0.2:4789" {
		t.Fatalf("第一步应为按旧元组撤旧，实际 %v", f.calls)
	}
	if f.calls[1] != "tunnel:true:0:100:10.99.0.1:10.99.0.3:4789" {
		t.Fatalf("第二步应为按新元组建新（沿用 instance 0），实际 %v", f.calls)
	}
	if len(f.tunnels) != 1 || f.tunnels[0].Dst != "10.99.0.3" {
		t.Fatalf("旧隧道应消失、只剩新隧道: %+v", f.tunnels)
	}
}

// 删除：摘 BD 归属 + 按元组撤条目；重复删除幂等（条目不在 = 已达成）。
func TestVxlanProviderDeleteRemovesTunnelAndBD(t *testing.T) {
	f := newFakeVxlanClient()
	p := NewVxlanProvider(f)
	tun := vxlanOf("t1", 100, "10.99.0.1", "10.99.0.2")
	tun.VirtualSwitch = "vs1"
	if err := p.ApplyVxlan(context.Background(), tun); err != nil {
		t.Fatalf("ApplyVxlan: %v", err)
	}
	bdKey := fmt.Sprintf("%d|%d", f.tunnels[0].SwIfIndex, BDID("vs1"))
	f.calls = nil

	if err := p.DeleteVxlan(context.Background(), tun); err != nil {
		t.Fatalf("DeleteVxlan: %v", err)
	}
	if f.bdState[bdKey] {
		t.Fatalf("BD 归属应已摘除: %v", f.bdState)
	}
	if len(f.tunnels) != 0 {
		t.Fatalf("隧道应已删除: %+v", f.tunnels)
	}
	// 幂等：再删一次不报错、无新调用
	f.calls = nil
	if err := p.DeleteVxlan(context.Background(), tun); err != nil {
		t.Fatalf("重复 DeleteVxlan: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("条目已不在时不应再发调用: %v", f.calls)
	}
}

// 恢复重放：VPP 里已有该元组（进程内登记为空）⇒ 按 dump 匹配复用、不重复建。
// 这是「不靠进程内登记/接口名」的正面证据（与 #359 tap 的教训同族）。
func TestVxlanProviderRecoveryMatchesDump(t *testing.T) {
	f := newFakeVxlanClient(VxlanTunnelInfo{Instance: 3, Vni: 200, Src: "10.0.0.1", Dst: "10.0.0.2", DstPort: 4789, SwIfIndex: 9})
	p := NewVxlanProvider(f) // 空登记 = 模拟 nfvisd 重启后
	tun := vxlanOf("t9", 200, "10.0.0.1", "10.0.0.2")
	tun.VirtualSwitch = "vs2"

	errs := p.EnsureConsistent(context.Background(), model.Config{VxlanTunnels: []model.VxlanTunnel{tun}})
	if len(errs) != 0 {
		t.Fatalf("恢复重放不应报错: %v", errs)
	}
	for _, c := range f.calls {
		if len(c) >= 10 && c[:10] == "tunnel:tr" {
			t.Fatalf("存量隧道不应重复建: %v", f.calls)
		}
	}
	if !f.bdState[fmt.Sprintf("9|%d", BDID("vs2"))] {
		t.Fatalf("存量隧道应按声明重新入 BD: %v", f.bdState)
	}
	if err := p.ApplyVxlan(context.Background(), tun); err != nil { // 再次重放仍幂等
		t.Fatalf("重复重放: %v", err)
	}
	if len(f.tunnels) != 1 {
		t.Fatalf("不应重复建: %+v", f.tunnels)
	}
}

// 读视图：按元组键索引实际条目（配置侧匹配由 API 层做；这里钉住键的形状）。
func TestVxlanProviderStates(t *testing.T) {
	f := newFakeVxlanClient(VxlanTunnelInfo{Instance: 1, Vni: 100, Src: "10.0.0.1", Dst: "10.0.0.2", DstPort: 4789, SwIfIndex: 6})
	p := NewVxlanProvider(f)
	states, err := p.VxlanStates(context.Background())
	if err != nil {
		t.Fatalf("VxlanStates: %v", err)
	}
	st, ok := states[VxlanTupleKey(100, "10.0.0.1", "10.0.0.2", 4789)]
	if !ok || st.SwIfIndex != 6 || st.Instance != 1 {
		t.Fatalf("读视图未按元组命中: %v", states)
	}
	if _, ok := states[VxlanTupleKey(100, "10.0.0.1", "10.0.0.2", 5789)]; ok {
		t.Fatal("端口不同不应命中")
	}
}

// 失败补偿（登记＝已成功下发的状态，两步推进）：变更时「新隧道已建成、入 BD 失败」——
// 登记里已记新元组，提交编排按旧声明补偿（ApplyVxlan(old)）必须撤掉新隧道并恢复旧隧道，
// 不留残渣（与 #363 的登记语义同族；若登记只在全成功后才写，新隧道会永久残留）。
func TestVxlanProviderFailedApplyCompensationRemovesNewTunnel(t *testing.T) {
	f := newFakeVxlanClient()
	p := NewVxlanProvider(f)
	old := vxlanOf("t1", 100, "10.99.0.1", "10.99.0.2")
	if err := p.ApplyVxlan(context.Background(), old); err != nil {
		t.Fatalf("首次 ApplyVxlan: %v", err)
	}
	next := vxlanOf("t1", 100, "10.99.0.1", "10.99.0.3")
	next.VirtualSwitch = "vs1"

	f.bdFail = true
	if err := p.ApplyVxlan(context.Background(), next); err == nil {
		t.Fatal("入 BD 失败应上报错误")
	}
	if _, ok := vxlanFindTuple(f.tunnels, 100, "10.99.0.1", "10.99.0.3", 4789); !ok {
		t.Fatal("新隧道此刻已在数据面（失败点在入 BD 一步）")
	}

	// 提交编排的补偿路径：按旧声明收敛
	f.bdFail = false
	if err := p.ApplyVxlan(context.Background(), old); err != nil {
		t.Fatalf("补偿 ApplyVxlan(old): %v", err)
	}
	if _, ok := vxlanFindTuple(f.tunnels, 100, "10.99.0.1", "10.99.0.3", 4789); ok {
		t.Fatal("补偿后新隧道不应残留")
	}
	if _, ok := vxlanFindTuple(f.tunnels, 100, "10.99.0.1", "10.99.0.2", 4789); !ok {
		t.Fatal("补偿后旧隧道应恢复")
	}
}
