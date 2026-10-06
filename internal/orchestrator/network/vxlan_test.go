package network

// VXLAN 编排单测（决策 #383）：纯函数（元组比较）+ 假客户端下的「按 tag 判存量 / 按旧元组
// 撤旧 / 打标 / 入 BD / 删除 / 恢复重放」——真机实证 VPP 26.06 的 vxlan dump 恒空，故身份
// 走接口 tag（fake 里也照此建模：没有 dump 路径可走）。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
)

// fakeVxlanClient 假客户端：ifaces/tags/tunnels 模拟 VPP 侧实际状态（无 vxlan dump 可读）。
type fakeVxlanClient struct {
	nextIdx       uint32
	ifaces        map[uint32]*fakeTaggedIface // sw_if_index → 接口
	tags          map[string]uint32           // tag → sw_if_index
	tunnels       map[string]uint32           // 元组键 → sw_if_index
	conflictTuple map[string]bool             // 预置「带外建的同元组隧道」→ 建时返回 ErrVxlanExists
	calls         []string
	bdState       map[string]bool // "if|bd" → 是否在 BD 里
	failAdd       bool
	failBD        bool
	failTag       bool
	dumpFail      bool // FindTagged 失败（数据面不可用）
}

type fakeTaggedIface struct {
	Name string
	Tag  string
}

func newFakeVxlanClient() *fakeVxlanClient {
	return &fakeVxlanClient{
		ifaces: map[uint32]*fakeTaggedIface{}, tags: map[string]uint32{},
		tunnels: map[string]uint32{}, conflictTuple: map[string]bool{}, bdState: map[string]bool{},
	}
}

// presetTunnel 预置一条「VPP 里已有」的隧道（模拟 nfvisd 重启而 VPP 未重启的场景）：
// 带平台标记、接口名由假 VPP 分配。
func (f *fakeVxlanClient) presetTunnel(instance uint32, vni uint32, src, dst string, port uint16, tag string) uint32 {
	f.nextIdx++
	idx := f.nextIdx
	_ = instance
	f.ifaces[idx] = &fakeTaggedIface{Name: fmt.Sprintf("vxlan_tunnel%d", idx-1), Tag: tag}
	f.tunnels[tupleKey(vni, src, dst, port)] = idx
	f.tags[tag] = idx
	return idx
}

func tupleKey(vni uint32, src, dst string, port uint16) string {
	return fmt.Sprintf("%d|%s|%s|%d", vni, src, dst, port)
}

func (f *fakeVxlanClient) Close() {}

func (f *fakeVxlanClient) TunnelAddDel(isAdd bool, instance uint32, vni uint32, src, dst string, dstPort uint16) (uint32, error) {
	f.calls = append(f.calls, fmt.Sprintf("tunnel:%v:%d:%s:%s:%d", isAdd, vni, src, dst, dstPort))
	key := tupleKey(vni, src, dst, dstPort)
	if isAdd {
		_ = instance
		if f.failAdd {
			return 0, fmt.Errorf("模拟建隧失败")
		}
		if _, dup := f.tunnels[key]; dup || f.conflictTuple[key] {
			return 0, ErrVxlanExists // 与 govpp 适配层翻出的哨兵错误同口径
		}
		f.nextIdx++
		idx := f.nextIdx
		f.ifaces[idx] = &fakeTaggedIface{Name: fmt.Sprintf("vxlan_tunnel%d", idx-1)}
		f.tunnels[key] = idx
		return idx, nil
	}
	idx, ok := f.tunnels[key]
	if !ok {
		return 0, fmt.Errorf("模拟删隧失败：同元组隧道不存在")
	}
	delete(f.tunnels, key)
	delete(f.ifaces, idx)
	for tag, i := range f.tags {
		if i == idx {
			delete(f.tags, tag)
		}
	}
	return 0, nil
}

func (f *fakeVxlanClient) SetTag(swIfIndex uint32, tag string) error {
	f.calls = append(f.calls, fmt.Sprintf("tag:%d:%s", swIfIndex, tag))
	if f.failTag {
		return fmt.Errorf("模拟打标失败")
	}
	f.tags[tag] = swIfIndex
	if ifc := f.ifaces[swIfIndex]; ifc != nil {
		ifc.Tag = tag
	}
	return nil
}

func (f *fakeVxlanClient) FindTagged(prefix string) (map[string]TaggedIface, error) {
	if f.dumpFail {
		return nil, fmt.Errorf("模拟接口清单不可用")
	}
	out := map[string]TaggedIface{}
	for tag, idx := range f.tags {
		if !strings.HasPrefix(tag, prefix) {
			continue
		}
		name := ""
		if ifc := f.ifaces[idx]; ifc != nil {
			name = ifc.Name
		}
		out[tag] = TaggedIface{SwIfIndex: idx, InterfaceName: name}
	}
	return out, nil
}

func (f *fakeVxlanClient) SetInterfaceUp(swIfIndex uint32) error {
	f.calls = append(f.calls, fmt.Sprintf("up:%d", swIfIndex))
	return nil
}

func (f *fakeVxlanClient) SetL2Bridge(swIfIndex, bdID uint32, enable bool) error {
	f.calls = append(f.calls, fmt.Sprintf("bd:%d:%d:%v", swIfIndex, bdID, enable))
	if f.failBD {
		return fmt.Errorf("模拟入 BD 失败")
	}
	f.bdState[fmt.Sprintf("%d|%d", swIfIndex, bdID)] = enable
	return nil
}

func (f *fakeVxlanClient) hasCall(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func vxlanOf(name string, vni int, local, remote string) model.VxlanTunnel {
	return model.VxlanTunnel{Name: name, Vni: vni, Local: local, Remote: remote}
}

// 纯函数：元组比较只认 vni/local/remote/生效端口；dst_port 0 与 4789 等价；name 不参与。
func TestVxlanSameTuple(t *testing.T) {
	base := model.VxlanTunnel{Vni: 100, Local: "10.0.0.1", Remote: "10.0.0.2"}
	for _, c := range []struct {
		o    model.VxlanTunnel
		want bool
	}{
		{model.VxlanTunnel{Vni: 100, Local: "10.0.0.1", Remote: "10.0.0.2", DstPort: 4789}, true}, // 0 与 4789 等价
		{model.VxlanTunnel{Vni: 100, Local: "10.0.0.1", Remote: "10.0.0.3"}, false},
		{model.VxlanTunnel{Vni: 101, Local: "10.0.0.1", Remote: "10.0.0.2"}, false},
		{model.VxlanTunnel{Vni: 100, Local: "10.0.0.9", Remote: "10.0.0.2"}, false},
		{model.VxlanTunnel{Vni: 100, Local: "10.0.0.1", Remote: "10.0.0.2", DstPort: 5789}, false},
	} {
		if got := base.SameTuple(c.o); got != c.want {
			t.Fatalf("SameTuple(%+v) = %v，期望 %v", c.o, got, c.want)
		}
	}
}

// 建隧 → 打平台标记 → 置 up → 入 BD；同元组重放（prev 同值）不重复建、仍按声明校正归属。
func TestVxlanProviderApplyCreatesTagsAndJoinsBD(t *testing.T) {
	f := newFakeVxlanClient()
	p := NewVxlanProvider(f)
	tun := vxlanOf("t1", 100, "10.99.0.1", "10.99.0.2")
	tun.VirtualSwitch = "vs1"

	if err := p.ApplyVxlan(context.Background(), tun, nil); err != nil {
		t.Fatalf("ApplyVxlan: %v", err)
	}
	if len(f.tunnels) != 1 {
		t.Fatalf("隧道未建成: %+v", f.tunnels)
	}
	if _, ok := f.tags[tun.DataPlaneTag()]; !ok {
		t.Fatalf("建隧后应打平台标记 %q: %v", tun.DataPlaneTag(), f.tags)
	}
	idx := f.tunnels[tupleKey(100, "10.99.0.1", "10.99.0.2", 4789)]
	if !f.hasCall(fmt.Sprintf("up:%d", idx)) {
		t.Fatalf("应置接口 up: %v", f.calls)
	}
	if !f.bdState[fmt.Sprintf("%d|%d", idx, BDID("vs1"))] {
		t.Fatalf("隧道口未加入 vs1 的 BD: %v", f.bdState)
	}
	joined := strings.Join(f.calls, " ")
	if strings.Index(joined, "tag:") > strings.Index(joined, "up:") {
		t.Fatalf("打标应在置 up 之前（任何一步失败后身份都可识别）: %v", f.calls)
	}

	// 重放（同元组、prev 同值）：不重复建，仍按声明把归属校正一遍（幂等）
	f.calls = nil
	prev := tun
	if err := p.ApplyVxlan(context.Background(), tun, &prev); err != nil {
		t.Fatalf("重放 ApplyVxlan: %v", err)
	}
	if f.hasCall("tunnel:true") {
		t.Fatalf("元组未变时不应重复建: %v", f.calls)
	}
	if len(f.tunnels) != 1 {
		t.Fatalf("不应产生第二条隧道: %+v", f.tunnels)
	}
}

// 改 remote ⇒ 先按**旧声明的元组**撤、再按新元组建（撤旧不依赖 dump，旧元组来自提交 diff）。
func TestVxlanProviderChangeRemoteWithdrawsOld(t *testing.T) {
	f := newFakeVxlanClient()
	p := NewVxlanProvider(f)
	old := vxlanOf("t1", 100, "10.99.0.1", "10.99.0.2")
	if err := p.ApplyVxlan(context.Background(), old, nil); err != nil {
		t.Fatalf("首次 ApplyVxlan: %v", err)
	}
	f.calls = nil

	next := vxlanOf("t1", 100, "10.99.0.1", "10.99.0.3")
	if err := p.ApplyVxlan(context.Background(), next, &old); err != nil {
		t.Fatalf("变更 ApplyVxlan: %v", err)
	}
	if len(f.calls) < 2 {
		t.Fatalf("变更应有撤旧+建新两次调用: %v", f.calls)
	}
	if f.calls[0] != "tunnel:false:100:10.99.0.1:10.99.0.2:4789" {
		t.Fatalf("第一步应为按旧元组撤旧，实际 %v", f.calls)
	}
	if f.calls[1] != "tunnel:true:100:10.99.0.1:10.99.0.3:4789" {
		t.Fatalf("第二步应为按新元组建新，实际 %v", f.calls)
	}
	if _, ok := f.tunnels[tupleKey(100, "10.99.0.1", "10.99.0.2", 4789)]; ok {
		t.Fatal("旧元组不应残留")
	}
	if _, ok := f.tunnels[tupleKey(100, "10.99.0.1", "10.99.0.3", 4789)]; !ok {
		t.Fatal("新元组应在数据面")
	}
	if got, want := f.tags[next.DataPlaneTag()], f.tunnels[tupleKey(100, "10.99.0.1", "10.99.0.3", 4789)]; got != want {
		t.Fatalf("新隧道应带平台标记: %v", f.tags)
	}
}

// tag 已在（nfvisd 重启而 VPP 未重启）⇒ **不重复建**；EnsureConsistent 同路径（按 tag 判存量）。
func TestVxlanProviderTagExistsSkipCreate(t *testing.T) {
	f := newFakeVxlanClient()
	tun := vxlanOf("t1", 100, "10.99.0.1", "10.99.0.2")
	tun.VirtualSwitch = "vs1"
	idx := f.presetTunnel(0, 100, "10.99.0.1", "10.99.0.2", 4789, tun.DataPlaneTag())
	p := NewVxlanProvider(f)

	// 恢复重放（prev=nil）：tag 已在 ⇒ 不重复建，只按声明入 BD
	errs := p.EnsureConsistent(context.Background(), model.Config{VxlanTunnels: []model.VxlanTunnel{tun}})
	if len(errs) != 0 {
		t.Fatalf("恢复重放不应报错: %v", errs)
	}
	if f.hasCall("tunnel:true") {
		t.Fatalf("tag 已在时不应重复建: %v", f.calls)
	}
	if !f.bdState[fmt.Sprintf("%d|%d", idx, BDID("vs1"))] {
		t.Fatalf("存量隧道应按声明重新入 BD: %v", f.bdState)
	}
	if len(f.tunnels) != 1 {
		t.Fatalf("不应重复建: %+v", f.tunnels)
	}

	// 恢复重放对**缺失**的隧道：按配置建 + 打标
	f2 := newFakeVxlanClient()
	p2 := NewVxlanProvider(f2)
	errs = p2.EnsureConsistent(context.Background(), model.Config{VxlanTunnels: []model.VxlanTunnel{tun}})
	if len(errs) != 0 {
		t.Fatalf("缺失项恢复应成功: %v", errs)
	}
	if len(f2.tunnels) != 1 || f2.tags[tun.DataPlaneTag()] == 0 {
		t.Fatalf("缺失的隧道应按配置重建并打标: tunnels=%v tags=%v", f2.tunnels, f2.tags)
	}
}

// 删除：按旧声明的元组撤 + 按它的 virtual-switch 摘 BD 归属；tag 不在＝已达成（幂等）。
func TestVxlanProviderDeleteByTag(t *testing.T) {
	f := newFakeVxlanClient()
	p := NewVxlanProvider(f)
	tun := vxlanOf("t1", 100, "10.99.0.1", "10.99.0.2")
	tun.VirtualSwitch = "vs1"
	if err := p.ApplyVxlan(context.Background(), tun, nil); err != nil {
		t.Fatalf("ApplyVxlan: %v", err)
	}
	idx := f.tunnels[tupleKey(100, "10.99.0.1", "10.99.0.2", 4789)]
	f.calls = nil

	if err := p.DeleteVxlan(context.Background(), tun); err != nil {
		t.Fatalf("DeleteVxlan: %v", err)
	}
	if f.bdState[fmt.Sprintf("%d|%d", idx, BDID("vs1"))] {
		t.Fatalf("BD 归属应已摘除: %v", f.bdState)
	}
	if len(f.tunnels) != 0 || len(f.tags) != 0 {
		t.Fatalf("隧道与平台标记应一并消失: tunnels=%v tags=%v", f.tunnels, f.tags)
	}
	// 幂等：再删一次不报错、无新调用（tag 不在＝已达成）
	f.calls = nil
	if err := p.DeleteVxlan(context.Background(), tun); err != nil {
		t.Fatalf("重复 DeleteVxlan: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("tag 不在时不应再发调用: %v", f.calls)
	}
}

// 读视图：按**隧道名**索引实际存在的隧道口（值为接口索引与数据面接口名）。
func TestVxlanProviderStates(t *testing.T) {
	f := newFakeVxlanClient()
	idx := f.presetTunnel(0, 100, "10.0.0.1", "10.0.0.2", 4789, "nfvis-vxlan:t1")
	f.tags["other-prefix:x"] = idx // 非平台前缀不出现
	f.tags["nfvis-vxlan:"] = idx   // 只有前缀的畸形标记：不猜名字，不出现

	p := NewVxlanProvider(f)
	states, err := p.VxlanStates(context.Background())
	if err != nil {
		t.Fatalf("VxlanStates: %v", err)
	}
	st, ok := states["t1"]
	if !ok || st.SwIfIndex != idx || st.InterfaceName == "" {
		t.Fatalf("读视图未按隧道名命中: %v", states)
	}
	if _, ok := states["x"]; ok {
		t.Fatalf("非平台前缀不应出现: %v", states)
	}
	if _, ok := states[""]; ok {
		t.Fatalf("空名字不应出现: %v", states)
	}
	// 运行态不可用：如实返回错误（读视图据此报「运行态不可用」）
	f.dumpFail = true
	if _, err := p.VxlanStates(context.Background()); err == nil {
		t.Fatal("数据面不可用应返回错误")
	}
}

// 同元组已存在于数据面（带外命令建的）：建隧失败翻成 ErrVxlanExists 且文案给出照做路径。
func TestVxlanProviderConflictGivesActionableError(t *testing.T) {
	f := newFakeVxlanClient()
	tun := vxlanOf("t1", 100, "10.99.0.1", "10.99.0.2")
	f.conflictTuple[tupleKey(100, "10.99.0.1", "10.99.0.2", 4789)] = true
	p := NewVxlanProvider(f)
	err := p.ApplyVxlan(context.Background(), tun, nil)
	if err == nil {
		t.Fatal("同元组冲突应报错")
	}
	if !strings.Contains(err.Error(), "带外") || !strings.Contains(err.Error(), "request vpp restart") {
		t.Fatalf("冲突文案应给出照做路径: %v", err)
	}
}

// 失败补偿：变更时新隧道已建、入 BD 失败——按旧声明补偿（ApplyVxlan(old, &new)）必须撤掉
// 新元组并恢复旧元组（撤旧依据是传进来的旧/新声明，不靠进程内登记）。
func TestVxlanProviderFailedApplyCompensationRemovesNewTunnel(t *testing.T) {
	f := newFakeVxlanClient()
	p := NewVxlanProvider(f)
	old := vxlanOf("t1", 100, "10.99.0.1", "10.99.0.2")
	if err := p.ApplyVxlan(context.Background(), old, nil); err != nil {
		t.Fatalf("首次 ApplyVxlan: %v", err)
	}
	next := vxlanOf("t1", 100, "10.99.0.1", "10.99.0.3")
	next.VirtualSwitch = "vs1"

	f.failBD = true
	if err := p.ApplyVxlan(context.Background(), next, &old); err == nil {
		t.Fatal("入 BD 失败应上报错误")
	}
	if _, ok := f.tunnels[tupleKey(100, "10.99.0.1", "10.99.0.3", 4789)]; !ok {
		t.Fatal("新隧道此刻已在数据面（失败点在入 BD 一步）")
	}

	// 提交编排的补偿路径：把隧道收敛回旧声明（prev=新声明 ⇒ 撤新元组、建旧元组）
	f.failBD = false
	if err := p.ApplyVxlan(context.Background(), old, &next); err != nil {
		t.Fatalf("补偿 ApplyVxlan(old): %v", err)
	}
	if _, ok := f.tunnels[tupleKey(100, "10.99.0.1", "10.99.0.3", 4789)]; ok {
		t.Fatal("补偿后新元组不应残留")
	}
	if _, ok := f.tunnels[tupleKey(100, "10.99.0.1", "10.99.0.2", 4789)]; !ok {
		t.Fatal("补偿后旧元组应恢复")
	}
}
