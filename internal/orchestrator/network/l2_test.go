package network

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// ---------- M3-3：L2 编排（FR-NET-010~016）单测（假 L2Client） ----------

type fakeL2 struct {
	ifaces  map[string]uint32
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
}

func newFakeL2() *fakeL2 {
	return &fakeL2{
		ifaces: map[string]uint32{"ens192": 1, "ens224": 2},
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
	idx, ok := f.ifaces[ifname]
	return idx, ok, nil
}

func (f *fakeL2) SwInterfaceNames() (map[uint32]SwIfInfo, error) { return f.names, nil }

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
	f.subifs = append(f.subifs, req)
	f.nextSub++
	idx := f.nextSub
	f.names[idx] = SwIfInfo{Name: "sub", OuterVlanID: req.OuterVlanID}
	f.log("subif")
	return idx, nil
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
