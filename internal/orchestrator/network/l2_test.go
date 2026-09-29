package network

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// ---------- M3-3：L2 编排（FR-NET-010~016）单测（假 L2Client） ----------

type fakeL2 struct {
	ifaces  map[string]uint32
	nameIdx map[string]uint32 // 接口名 → idx（子接口按名可查，与真实 VPP 一致）
	names   map[uint32]SwIfInfo
	nextSub uint32

	bds        map[uint32]bool
	bdRuntimes []BDRuntime       // BridgeDomains() 返回值（决策 #84）
	bridge     map[uint32]uint32 // swIfIndex → bdID
	xconn      map[uint32]uint32
	macs       map[uint32][]MACEntry
	calls      []string
	subifs     []CreateSubifReq
	err        error // 非 nil 时各方法返回该错误
	namesErr   error // 非 nil 时 SwInterfaceNames 返回该错误（链路告警「查询失败不清警」单测）
}

func newFakeL2() *fakeL2 {
	return &fakeL2{
		ifaces: map[string]uint32{"ens192": 1, "ens224": 2},
		// nameIdx 只由 CreateSubif 动态登记子接口（真实 VPP 里子接口是独立接口、可按名查）；
		// 物理口一律走 ifaces，避免「删掉 ifaces 端口但仍能按名查到」把缺失口用例变成假绿。
		nameIdx: map[string]uint32{},
		names: map[uint32]SwIfInfo{
			1:   {Name: "ens192"},
			2:   {Name: "ens224"},
			100: {Name: "ens192.100", OuterVlanID: 100},
		},
		nextSub: 100,
		bds:     map[uint32]bool{},
		bridge:  map[uint32]uint32{},
		xconn:   map[uint32]uint32{},
		macs:    map[uint32][]MACEntry{},
	}
}

func (f *fakeL2) log(s string) { f.calls = append(f.calls, s) }
func (f *fakeL2) Close()       {}

func (f *fakeL2) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	if idx, ok := f.ifaces[ifname]; ok {
		return idx, true, nil
	}
	idx, ok := f.nameIdx[ifname] // 子接口按名可查（真实 VPP 亦然）
	return idx, ok, nil
}

func (f *fakeL2) SwInterfaceNames() (map[uint32]SwIfInfo, error) {
	if f.namesErr != nil {
		return nil, f.namesErr
	}
	return f.names, nil
}

// BridgeDomains 假的 BD 运行态（决策 #84）。
func (f *fakeL2) BridgeDomains() ([]BDRuntime, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.bdRuntimes, nil
}

func (f *fakeL2) BridgeDomainExists(bdID uint32) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.bds[bdID], nil
}

func (f *fakeL2) BridgeDomainAddDel(bdID uint32, add, learn bool, tag string) error {
	f.log("bd:" + tag)
	if add {
		f.bds[bdID] = true
	} else {
		delete(f.bds, bdID)
	}
	return nil
}

func (f *fakeL2) SwInterfaceSetL2Bridge(swIfIndex, bdID uint32, _ L2PortType, _ uint8, enable bool) error {
	if enable {
		f.bridge[swIfIndex] = bdID
		f.log("attach")
	} else {
		delete(f.bridge, swIfIndex)
		f.log("detach")
	}
	return nil
}

func (f *fakeL2) SwInterfaceSetL2Xconnect(swIfIndex, bdID uint32, enable bool) error {
	if enable {
		f.xconn[swIfIndex] = bdID
		f.log("xconnect")
	} else {
		delete(f.xconn, swIfIndex)
		f.log("xdisconnect")
	}
	return nil
}

func (f *fakeL2) CreateSubif(req CreateSubifReq) (uint32, error) {
	// 复刻 VPP 的 create_subif 语义：同父口 + 同 sub-id 已存在 → -56（Value already exists），
	// 且子接口名按真实规则命名（`<父口名>.<sub-id>`）——R88-3 的现场正是这个返回码把整次
	// 提交打回滚，幂等复用则按这个名字查回来。
	parentName := ""
	if info, ok := f.names[req.ParentSwIfIndex]; ok {
		parentName = info.Name
	}
	if parentName == "" {
		parentName = fmt.Sprintf("%d", req.ParentSwIfIndex)
	}
	key := fmt.Sprintf("%s.%d", parentName, req.SubID)
	if _, ok := f.nameIdx[key]; ok {
		f.log("subif-exists")
		return 0, fmt.Errorf("create_subif(parent=%d,sub=%d) retval=-56", req.ParentSwIfIndex, req.SubID)
	}
	f.subifs = append(f.subifs, req)
	f.nextSub++
	idx := f.nextSub
	f.nameIdx[key] = idx
	f.names[idx] = SwIfInfo{Name: key, OuterVlanID: req.OuterVlanID}
	f.log("subif")
	return idx, nil
}

func (f *fakeL2) DeleteSubif(swIfIndex uint32) error {
	if _, ok := f.names[swIfIndex]; !ok {
		return fmt.Errorf("delete_subif(if=%d) retval=-2", swIfIndex)
	}
	for k, v := range f.nameIdx {
		if v == swIfIndex {
			delete(f.nameIdx, k)
		}
	}
	delete(f.names, swIfIndex)
	f.log("delsubif")
	return nil
}

func (f *fakeL2) L2InterfaceVlanTagRewrite(VlanTagRewriteReq) error { return nil }

func (f *fakeL2) MACTable(bdID uint32) ([]MACEntry, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.macs[bdID], nil
}

func l2vs(name string, ports ...model.VSwitchPort) model.VirtualSwitch {
	return model.VirtualSwitch{Name: name, Type: "l2", Ports: ports}
}

func TestL2ApplyBridgeDomainAndDetach(t *testing.T) {
	f := newFakeL2()
	p := NewL2Provider(f)
	vs := l2vs("vs-app",
		model.VSwitchPort{Seq: 0, Interface: "ens192"},
		model.VSwitchPort{Seq: 1, Interface: "ens224"})

	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("ApplyBridgeDomain: %v", err)
	}
	bd := BDID("vs-app")
	if !f.bds[bd] || f.bridge[1] != bd || f.bridge[2] != bd {
		t.Fatalf("BD 与端口挂接不符: bds=%v bridge=%v", f.bds, f.bridge)
	}

	// 删除端口 1 → 端口 1 摘除、端口 2 保持
	vs.Ports = vs.Ports[1:]
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("再次 ApplyBridgeDomain: %v", err)
	}
	if _, ok := f.bridge[1]; ok {
		t.Fatalf("端口 ens192 应被摘除: %v", f.bridge)
	}
	if f.bridge[2] != bd {
		t.Fatalf("端口 ens224 应保持挂接: %v", f.bridge)
	}

	// L3 交换机不由本 provider 处理
	if err := p.ApplyBridgeDomain(context.Background(), model.VirtualSwitch{Name: "x", Type: "l3"}); err != nil {
		t.Fatalf("L3 应跳过: %v", err)
	}
}

func TestL2DeleteBridgeDomainDetaches(t *testing.T) {
	f := newFakeL2()
	p := NewL2Provider(f)
	vs := l2vs("vs-del", model.VSwitchPort{Seq: 0, Interface: "ens192"})
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := p.DeleteBridgeDomain(context.Background(), "vs-del"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if f.bds[BDID("vs-del")] || len(f.bridge) != 0 {
		t.Fatalf("删除后应无 BD/成员: bds=%v bridge=%v", f.bds, f.bridge)
	}
}

func TestL2CrossConnect(t *testing.T) {
	f := newFakeL2()
	p := NewL2Provider(f)
	vs := l2vs("vs-xc",
		model.VSwitchPort{Seq: 0, Interface: "ens192"},
		model.VSwitchPort{Seq: 1, Interface: "ens224"})
	vs.CrossConnect = true
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("cross-connect: %v", err)
	}
	if f.xconn[1] != 2 || f.xconn[2] != 1 {
		t.Fatalf("cross-connect 应对称挂接: %v", f.xconn)
	}

	vs.Ports = append(vs.Ports, model.VSwitchPort{Seq: 2, Interface: "ens192"})
	if err := p.ApplyBridgeDomain(context.Background(), vs); err == nil {
		t.Fatalf("三个端口应报错")
	}
}

func TestL2AccessVlanCreatesSubif(t *testing.T) {
	f := newFakeL2()
	p := NewL2Provider(f)
	vs := l2vs("vs-vlan", model.VSwitchPort{Seq: 0, Interface: "ens192"})
	vs.VlanAccess = 100
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("access vlan: %v", err)
	}
	if len(f.subifs) != 1 || f.subifs[0].OuterVlanID != 100 || f.subifs[0].ParentSwIfIndex != 1 {
		t.Fatalf("应创建 vlan 100 子接口: %+v", f.subifs)
	}
	if f.bridge[101] != BDID("vs-vlan") {
		t.Fatalf("子接口应挂接到 BD: %v", f.bridge)
	}
}

// R88-3 回归：BD 重放必须幂等——已有 access 子接口不得再建一次。
//
// 真机 round88 现场：先建好 vs-l2（access vlan 100，成员 ens224），之后把 vnf-a 的 vNIC
// 加进同一台交换机 → ApplyBridgeDomain 全量重放 → 对 ens224 再 create_subif → VPP 返回
// -56（already exists）→ **整次提交失败并回滚**（把交换机加个口都做不到）。BD 自身早有
// 存在性判断，子接口这一层当年漏了。
func TestL2AccessVlanSubifIdempotentOnReplay(t *testing.T) {
	f := newFakeL2()
	p := NewL2Provider(f)
	vs := l2vs("vs-vlan", model.VSwitchPort{Seq: 0, Interface: "ens224"})
	vs.VlanAccess = 100
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("首次下发: %v", err)
	}
	// 模拟「给同一台交换机新增一个端口」（VNF vNIC 已在 VPP 里）后的重放
	f.ifaces["vh-vnf-a-eth0"] = 7
	f.names[7] = SwIfInfo{Name: "vh-vnf-a-eth0"}
	vs.Ports = append(vs.Ports, model.VSwitchPort{Seq: 1, Vnf: "vnf-a", VnfInterface: "eth0"})

	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("重放（新增 VNF 端口）不应失败（旧实现在此撞 -56 并整体回滚）: %v", err)
	}
	// access VLAN 是交换机级的：每个端口各一个 <父口>.100 子接口。这里要断言的是
	// **已有端口（ens224）不得被再建一次**，而不是子接口总数。
	recreated := 0
	for _, req := range f.subifs {
		if req.ParentSwIfIndex == 2 {
			recreated++
		}
	}
	if recreated != 1 {
		t.Fatalf("ens224 的 access 子接口只应创建一次，实际 %d 次: %+v", recreated, f.subifs)
	}
	if _, ok := f.nameIdx["ens224.100"]; !ok {
		t.Fatalf("ens224 的子接口应存在并可按名查到: %v", f.nameIdx)
	}
	if _, ok := f.nameIdx["vh-vnf-a-eth0.100"]; !ok {
		t.Fatalf("新增端口的子接口应已创建: %v", f.nameIdx)
	}
	if f.bridge[101] != BDID("vs-vlan") {
		t.Fatalf("子接口应仍挂接在 BD 上: %v", f.bridge)
	}
	// access VLAN 下端口是经其子接口挂接的（不是裸 vNIC）
	subVnic := f.nameIdx["vh-vnf-a-eth0.100"]
	if subVnic == 0 || f.bridge[subVnic] != BDID("vs-vlan") {
		t.Fatalf("新增 vNIC 端口的子接口应挂接在 BD 上: %v（idx=%d）", f.bridge, subVnic)
	}
}

// R88-3 回归：删 L2 交换机时回收本交换机建的 VLAN 子接口（数据面不留 `ens224.100` 残留）。
func TestDeleteBridgeDomainReclaimsSubif(t *testing.T) {
	f := newFakeL2()
	p := NewL2Provider(f)
	vs := l2vs("vs-vlan", model.VSwitchPort{Seq: 0, Interface: "ens224"})
	vs.VlanAccess = 100
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("下发: %v", err)
	}
	if _, ok := f.names[101]; !ok {
		t.Fatalf("前置：子接口应已创建: %v", f.names)
	}
	if err := p.DeleteBridgeDomain(context.Background(), "vs-vlan"); err != nil {
		t.Fatalf("删交换机: %v", err)
	}
	if _, ok := f.names[101]; ok {
		t.Fatalf("子接口应随交换机删除被回收（旧实现留在数据面，真机显示「未声明」残留）: %v", f.names)
	}
	if f.bridge[101] != 0 {
		t.Fatalf("子接口应从 BD 摘除: %v", f.bridge)
	}
	if f.bds[BDID("vs-vlan")] {
		t.Fatal("BD 应已删除")
	}
}

// M4-4：VNF 端口在 VPP 侧按确定性接口名解析（ApplyVnfInterface 先建）；
// 接口不存在时给出可诊断错误（ErrIfaceUnavailable），不再以「M4 未支持」拒绝。
func TestL2VnfPortResolvesByIfaceName(t *testing.T) {
	f := newFakeL2()
	p := NewL2Provider(f)
	vs := l2vs("vs-vnf", model.VSwitchPort{Seq: 0, Vnf: "fw-vm", VnfInterface: "eth1"})
	if err := p.ApplyBridgeDomain(context.Background(), vs); err == nil || !errors.Is(err, ErrIfaceUnavailable) {
		t.Fatalf("vhost 接口未下发应报 ErrIfaceUnavailable: %v", err)
	}

	// 预置 vhost-user 接口（名 = orchestrator.VnfIfaceName）后应挂接成功。
	f.ifaces[orchestrator.VnfIfaceName("fw-vm", "eth1")] = 77
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("vhost 接口存在时应挂接: %v", err)
	}
	if f.bridge[77] != BDID("vs-vnf") {
		t.Fatalf("vhost 接口应挂接 BD: %v", f.bridge)
	}
}

// 决策 #170 回归：VNF 侧声明 `interfaces <nic> virtual-switch <vs>`（手册 §9.2 的写法）
// 经 SwitchMembersOf 合流后，其 vhost-user 口必须真的出现在该 BD 的成员里——
// 成员口接口名即确定性接口名 vh-<vm>-<nic>（此前该口从不进 BD，guest 帧被 100% 丢弃）。
func TestL2VnfNicDeclarationJoinsBridgeDomain(t *testing.T) {
	f := newFakeL2()
	p := NewL2Provider(f)
	cfg := model.Config{
		VirtualSwitches: []model.VirtualSwitch{l2vs("vs-vnf", model.VSwitchPort{Seq: 1, Interface: "ens192"})},
		VirtualMachineFunctions: []model.VMFunction{{Name: "vm-a", Image: "img",
			Interfaces: []model.VnfInterface{{Name: "eth0", Type: "vhost-user", VirtualSwitch: "vs-vnf"}}}},
		ContainerFunctions: []model.ContainerFunction{{Name: "ct-a", Image: "img",
			Interfaces: []model.VnfInterface{{Name: "eth0", Type: "memif", VirtualSwitch: "vs-vnf"}}}},
	}
	// vNIC 接口由 ApplyVnfInterface 先行建立（此处预置为 VPP 侧已存在）
	vh := orchestrator.VnfIfaceName("vm-a", "eth0")
	mf := orchestrator.MemifIfaceName("ct-a", "eth0")
	f.ifaces[vh], f.ifaces[mf] = 77, 78
	f.names[77], f.names[78] = SwIfInfo{Name: vh}, SwIfInfo{Name: mf}

	switches, errs := orchestrator.SwitchMembersOf(cfg, orchestrator.DefaultVhostDir, orchestrator.DefaultMemifDir)
	if len(errs) != 0 {
		t.Fatalf("声明应全部归位: %v", errs)
	}
	for _, vs := range switches {
		if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
			t.Fatalf("ApplyBridgeDomain(%s): %v", vs.Name, err)
		}
	}
	if f.bridge[77] != BDID("vs-vnf") {
		t.Fatalf("vhost 口 %s 应为 BD 成员: %v", vh, f.bridge)
	}
	if f.bridge[78] != BDID("vs-vnf") {
		t.Fatalf("memif 口 %s 应为 BD 成员: %v", mf, f.bridge)
	}
	if f.bridge[1] != BDID("vs-vnf") {
		t.Fatalf("交换机侧端口应仍挂接: %v", f.bridge)
	}
}

func TestL2MissingInterface(t *testing.T) {
	f := newFakeL2()
	p := NewL2Provider(f)
	vs := l2vs("vs-miss", model.VSwitchPort{Seq: 0, Interface: "ens999"})
	if err := p.ApplyBridgeDomain(context.Background(), vs); err == nil || !strings.Contains(err.Error(), "DPDK") {
		t.Fatalf("缺失接口应提示 DPDK: %v", err)
	}
}

func TestL2MACTableResolvesPort(t *testing.T) {
	f := newFakeL2()
	p := NewL2Provider(f)
	bd := BDID("vs-mac")
	f.macs[bd] = []MACEntry{{MAC: "00:11:22:33:44:55", SwIfIndex: 100}}
	rows, err := p.MACTable(context.Background(), "vs-mac")
	if err != nil {
		t.Fatalf("MACTable: %v", err)
	}
	if len(rows) != 1 || rows[0].Port != "ens192.100" || rows[0].VLAN != 100 {
		t.Fatalf("MAC 表应解析出端口/VLAN: %+v", rows)
	}
}

func TestBDIDDeterministic(t *testing.T) {
	if BDID("vs-a") != BDID("vs-a") {
		t.Fatalf("同名 BD ID 应恒定")
	}
	if BDID("vs-a") == BDID("vs-b") {
		t.Fatalf("不同名 BD ID 应不同")
	}
	if BDID("") == 0 {
		t.Fatalf("BD ID 不应为 0")
	}
}

func TestL2TrunkVlanSubifs(t *testing.T) {
	f := newFakeL2()
	p := NewL2Provider(f)
	vs := l2vs("vs-trunk", model.VSwitchPort{Seq: 0, Interface: "ens192",
		TrunkVlans: []int{10, 20}, NativeVlan: 1})
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("trunk: %v", err)
	}
	if len(f.subifs) != 2 {
		t.Fatalf("应为每个 tagged VID 建子接口: %+v", f.subifs)
	}
	// native 物理口挂接
	if f.bridge[1] != BDID("vs-trunk") {
		t.Fatalf("native 物理口应挂接: %v", f.bridge)
	}
}

func TestL2ContainerPortUnsupportedAndNoInterface(t *testing.T) {
	f := newFakeL2()
	p := NewL2Provider(f)
	if err := p.ApplyBridgeDomain(context.Background(),
		l2vs("vs-c", model.VSwitchPort{Seq: 0, Container: "c1", ContainerInterface: "eth0"})); err == nil {
		t.Fatalf("容器端口应报 M4")
	}
	if err := p.ApplyBridgeDomain(context.Background(),
		l2vs("vs-empty", model.VSwitchPort{Seq: 0})); err == nil || !strings.Contains(err.Error(), "未指定接口") {
		t.Fatalf("空端口应报未指定接口: %v", err)
	}
}

func TestL2ClientFactoryError(t *testing.T) {
	p := NewL2ProviderFunc(func() (L2Client, error) { return nil, ErrL2Unavailable })
	if err := p.ApplyBridgeDomain(context.Background(), l2vs("x")); err == nil {
		t.Fatalf("工厂错误应上抛")
	}
	if err := p.DeleteBridgeDomain(context.Background(), "x"); err == nil {
		t.Fatalf("工厂错误应上抛（删除）")
	}
	if _, err := p.MACTable(context.Background(), "x"); err == nil {
		t.Fatalf("工厂错误应上抛（MAC 表）")
	}
}

func TestL2NetworkDecorator(t *testing.T) {
	f := newFakeL2()
	bd := BDID("vs-dec")
	f.macs[bd] = []MACEntry{{MAC: "aa:bb:cc:dd:ee:ff", SwIfIndex: 1}}
	n := NewL2Network(nil, NewL2Provider(f)) // base=nil → noop
	vs := l2vs("vs-dec", model.VSwitchPort{Seq: 0, Interface: "ens192"})
	if err := n.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("装饰器 Apply: %v", err)
	}
	rows, err := n.MACTable(context.Background(), "vs-dec")
	if err != nil || len(rows) != 1 || rows[0].Port != "ens192" {
		t.Fatalf("装饰器 MACTable: %v %+v", err, rows)
	}
	if err := n.DeleteBridgeDomain(context.Background(), "vs-dec"); err != nil {
		t.Fatalf("装饰器 Delete: %v", err)
	}
	// 其余方法沿用 noop
	if err := n.ApplyACL(context.Background(), model.Acl{Name: "a"}); err != nil {
		t.Fatalf("noop 透传: %v", err)
	}
	if errs := n.EnsureConsistent(context.Background(), model.Config{}); len(errs) != 0 {
		t.Fatalf("noop EnsureConsistent 应为空")
	}
}

func TestManagerAPIChannelUnavailable(t *testing.T) {
	m := NewManager(testConfig(), &fakeDialer{})
	if _, err := m.APIChannel(); err == nil {
		t.Fatalf("未连接时 APIChannel 应报错")
	}
	if _, err := m.L2ClientFunc()(); err == nil {
		t.Fatalf("未连接时 L2 客户端应报错")
	}
}

// 幂等：BD 已存在时重复 Apply 不报错；删除不存在的 BD 亦不报错（FR-OPS-010 重放友好）。
func TestL2ApplyIdempotent(t *testing.T) {
	f := newFakeL2()
	p := NewL2Provider(f)
	vs := l2vs("vs-idem", model.VSwitchPort{Seq: 0, Interface: "ens192"})
	for i := 0; i < 2; i++ {
		if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
			t.Fatalf("第 %d 次 Apply 应幂等: %v", i+1, err)
		}
	}
	if !f.bds[BDID("vs-idem")] || f.bridge[1] != BDID("vs-idem") {
		t.Fatalf("幂等 Apply 后状态不符: %v %v", f.bds, f.bridge)
	}
	if err := p.DeleteBridgeDomain(context.Background(), "vs-nonexistent"); err != nil {
		t.Fatalf("删除不存在的 BD 应无害: %v", err)
	}
}

func TestL2ClientErrors(t *testing.T) {
	sentinel := errors.New("boom")
	f := newFakeL2()
	f.err = sentinel
	p := NewL2Provider(f)
	vs := l2vs("vs-err", model.VSwitchPort{Seq: 0, Interface: "ens192"})
	if err := p.ApplyBridgeDomain(context.Background(), vs); err == nil {
		t.Fatalf("BD 查询错误应上抛")
	}
	if err := p.DeleteBridgeDomain(context.Background(), "vs-err"); err == nil {
		t.Fatalf("删除时查询错误应上抛")
	}
	if _, err := p.MACTable(context.Background(), "vs-err"); err == nil {
		t.Fatalf("MAC 表错误应上抛")
	}
}

// cross-connect 配置缩减后应解除旧端口的 xconnect。
func TestL2CrossConnectDetach(t *testing.T) {
	f := newFakeL2()
	p := NewL2Provider(f)
	vs := l2vs("vs-xc2",
		model.VSwitchPort{Seq: 0, Interface: "ens192"},
		model.VSwitchPort{Seq: 1, Interface: "ens224"})
	vs.CrossConnect = true
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("xconnect: %v", err)
	}
	if len(f.xconn) != 2 {
		t.Fatalf("应两端 xconnect: %v", f.xconn)
	}
	vs.Ports = vs.Ports[:1]
	if err := p.ApplyBridgeDomain(context.Background(), vs); err != nil {
		t.Fatalf("缩减: %v", err)
	}
	if len(f.xconn) != 0 {
		t.Fatalf("缩减后应解除 xconnect: %v", f.xconn)
	}
}
