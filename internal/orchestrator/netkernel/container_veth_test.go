package netkernel

// 内核数据面容器 vNIC 接入（v3 决策 #441）单测：**不依赖真内核/真网络命名空间**——veth 操作面
// 注入内存底座（SetContainerVethLayer），内核命令走内存 Runner（dhcpFakeHost，veth 操作面把
// 建/删/up/master 镜像进它——同一台内核的两个视图），校验：
//  ① 命名单一真源（确定性、15 字符、两端只差前缀、不同输入不撞名、严格字符集识别）；
//  ② 宿主端接口收敛（**只建对 + up**；入桥与 VLAN 不在本段——与 VPP 侧「先建 memif 接口、
//     BD 段按名挂端口」同构：**先建接口、后入域**）；
//  ③ attach（幂等 ensure + 把容器端以声明的 vNIC 名与 MAC 接进容器 netns；一个失败不遮其它）；
//  ④ 删除（按名删宿主端、幂等、DeleteContainerVeths 只删该属主）；
//  ⑤ 巡检（声明里缺宿主端补建、入对桥、无声明对应的产品宿主端清掉、非法相似名绝不碰、
//     未声明未装配空操作、失败落告警/成功自动消解）；
//  ⑥ 读视图（产品宿主端不进交换机端口视图，名字相近但非法的用户口照常显示）；
//  ⑦ 桥段成员处理（memberLinkName 认 Container 端口 → 宿主端名；桥段 enslave + up）、
//     Provider 接线（vhost-user 如实空操作、memif 走 veth、Attach/Delete 钩子与长名同源）
//     与恢复重放的依赖序（宿主端按名复用、由交换机段入桥）。
// 源码扫描守护（apply 路径不读配置发动机）的覆盖文件清单见 dhcpserver_test.go。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
	"github.com/xzjt/nfvis/internal/orchestrator/network"
)

// ctVethTestSwitch 测试用交换机名：**故意超过 15 字符**——LinkName 会派生成 `前缀-8hex`，
// 于是「用了 LinkName(交换机)」与「用了裸交换机名」在断言里可区分（红-绿变异用）。
const ctVethTestSwitch = "vs-container-switch"

// ---------- 内存 veth 操作面（ctVethIO 假实现） ----------

// ctVethFakeAttach 一次 Attach 调用（断言容器端名/pid/容器内名/MAC）。
type ctVethFakeAttach struct {
	peer     string
	pid      int
	niceName string
	mac      string
}

// ctVethFakeIO 内存版 veth 操作面：模拟「接口名 → 所在命名空间」与成对关系（删一端即整对消失），
// 并记录调用序列。语义与容器 veth_linux.go 的真实现对齐（按名复用、幂等删除、attach 幂等路径）。
type ctVethFakeIO struct {
	mu sync.Mutex
	// kernel 非 nil 时把 veth 的建/删/up/master 镜像进假内核（两者表示同一台内核：
	// 桥段按内核实况 enslave 成员、读视图按内核实况枚举端口，故两份状态必须一致）。
	kernel *dhcpFakeHost
	// links 接口名 → 所在命名空间（"host" 或 "ct:<pid>"）。成对的两个端都在这里登记。
	links map[string]string
	// pairs 宿主端名 → 容器端名（成对关系；容器端被移入容器后按容器内名登记，关系仍保留）。
	pairs map[string]string
	// bridges 内核里现存的 bridge 名（SetMaster 按真内核语义核对：bridge 不在 ⇒ 如实失败，
	// 这正是「先建接口、后入域」依赖序能被钉住的判据）。
	bridges map[string]bool
	// masters / ups 运行态（SetMaster / SetUp）。
	masters map[string]string
	ups     map[string]bool

	created        []string // 实际创建的 veth 对（宿主端名）
	ensureCalls    []string // 全部 EnsurePair 调用（宿主端名）
	masterCalls    []string // 全部 SetMaster 调用（"口@bridge"）
	upCalls        []string // 全部 SetUp 调用
	attachCalls    []ctVethFakeAttach
	netnsMoves     []string // 实际发生的「移入容器」（重复 attach 时不会再有）
	deleted        []string // 实际删除的宿主端
	deleteAttempts []string // 全部删除调用（含本就不存在的——幂等容错的证据）

	// 注入
	ensureErr error
	existsErr error
	attachErr map[string]error // 按容器端名注入 attach 失败
}

func newCtVethFakeIO() *ctVethFakeIO {
	return &ctVethFakeIO{
		links:     map[string]string{},
		pairs:     map[string]string{},
		bridges:   map[string]bool{},
		masters:   map[string]string{},
		ups:       map[string]bool{},
		attachErr: map[string]error{},
	}
}

// addBridge 在假内核里预置一台 bridge（SetMaster 的核对对象）。
func (f *ctVethFakeIO) addBridge(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bridges[name] = true
}

// seedHostEnd 在假内核里预置一个已在宿主端的接口（残留/带外对象的现场）。
func (f *ctVethFakeIO) seedHostEnd(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.links[name] = "host"
	f.mirrorAddLocked(name)
}

// ---------- 与假内核的镜像（同一台内核的两个视图） ----------

func (f *ctVethFakeIO) mirrorAddLocked(name string) {
	if f.kernel == nil || f.kernel.has(name) {
		return // 已存在不重建（重置 master/up 等运行态的代价会污染断言）
	}
	f.kernel.addLink(name, "veth")
}

func (f *ctVethFakeIO) mirrorMasterLocked(name, bridge string) {
	if f.kernel != nil {
		f.kernel.ctSetMaster(name, bridge)
	}
}

func (f *ctVethFakeIO) mirrorUpLocked(name string) {
	if f.kernel != nil {
		f.kernel.ctSetUp(name)
	}
}

func (f *ctVethFakeIO) mirrorDeleteLocked(name string) {
	if f.kernel != nil {
		f.kernel.removeLink(name)
	}
}

func ctNS(pid int) string { return fmt.Sprintf("ct:%d", pid) }

func (f *ctVethFakeIO) EnsurePair(host, peer string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls = append(f.ensureCalls, host)
	if f.ensureErr != nil {
		return f.ensureErr
	}
	if ns, ok := f.links[host]; ok && ns == "host" {
		f.mirrorAddLocked(host)
		f.mirrorAddLocked(peer)
		return nil // 宿主端在 ⇒ 整对在：**按名复用、绝不重建**
	}
	if _, ok := f.links[peer]; ok {
		delete(f.links, peer) // 只有对端在的异常态：清孤儿对端（与真实现同法）
		f.mirrorDeleteLocked(peer)
	}
	f.links[host], f.links[peer] = "host", "host"
	f.pairs[host] = peer
	f.created = append(f.created, host)
	f.mirrorAddLocked(host)
	f.mirrorAddLocked(peer)
	return nil
}

func (f *ctVethFakeIO) SetMaster(name, bridge string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.links[name]; !ok {
		return fmt.Errorf("Cannot find device %q", name)
	}
	if !f.bridges[bridge] {
		// 真内核语义：bridge 不存在 ⇒ `ip link set … master` 报 Cannot find device。
		return fmt.Errorf("Cannot find device %q", bridge)
	}
	f.masters[name] = bridge
	f.masterCalls = append(f.masterCalls, name+"@"+bridge)
	f.mirrorMasterLocked(name, bridge)
	return nil
}

func (f *ctVethFakeIO) SetUp(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.links[name]; !ok {
		return fmt.Errorf("Cannot find device %q", name)
	}
	f.ups[name] = true
	f.upCalls = append(f.upCalls, name)
	f.mirrorUpLocked(name)
	return nil
}

func (f *ctVethFakeIO) Attach(peer string, pid int, niceName, mac string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attachCalls = append(f.attachCalls, ctVethFakeAttach{peer: peer, pid: pid, niceName: niceName, mac: mac})
	if err := f.attachErr[peer]; err != nil {
		return err
	}
	ns, ok := f.links[peer]
	if !ok {
		// 容器端已不在宿主命名空间：按容器内名字核对（已接入 ⇒ 幂等成功）。
		if f.links[niceName] == ctNS(pid) {
			return nil
		}
		return fmt.Errorf("Cannot find device %q", peer)
	}
	if ns != "host" {
		return fmt.Errorf("device %q is not in the host namespace", peer)
	}
	delete(f.links, peer)
	f.mirrorDeleteLocked(peer) // 容器端已不在宿主命名空间（内核里也看不到）
	f.links[niceName] = ctNS(pid)
	f.netnsMoves = append(f.netnsMoves, peer+"->"+niceName)
	return nil
}

func (f *ctVethFakeIO) Delete(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteAttempts = append(f.deleteAttempts, name)
	ns, ok := f.links[name]
	if !ok {
		return nil // 设备不存在 ⇒ 已达成（幂等）
	}
	if ns != "host" {
		return nil // 不在宿主命名空间（容器端）：不可删
	}
	delete(f.links, name)
	f.mirrorDeleteLocked(name)
	// veth 成对：删一端即整对消失（对端无论在哪都一并消失）。
	for host, peer := range f.pairs {
		if host == name {
			delete(f.links, peer)
			delete(f.pairs, host)
			f.mirrorDeleteLocked(peer)
			break
		}
	}
	f.deleted = append(f.deleted, name)
	return nil
}

func (f *ctVethFakeIO) Exists(name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.existsErr != nil {
		return false, f.existsErr
	}
	_, ok := f.links[name]
	return ok, nil
}

// ctVethFakeLayer 底座假实现（每次 Open 返回同一个内存操作面）。
type ctVethFakeLayer struct {
	io      *ctVethFakeIO
	opens   int
	openErr error
}

func (l *ctVethFakeLayer) Open() (ctVethIO, error) {
	l.opens++
	if l.openErr != nil {
		return nil, l.openErr
	}
	return l.io, nil
}

// ---------- 夹具 ----------

type ctVethFixture struct {
	t      *testing.T
	p      *Provider
	host   *dhcpFakeHost
	io     *ctVethFakeIO
	layer  *ctVethFakeLayer
	alarms *network.AlarmStore
	cfg    model.Config
}

func newCtVethFixture(t *testing.T, cfg model.Config) *ctVethFixture {
	t.Helper()
	f := &ctVethFixture{
		t:      t,
		host:   newDHCPFakeHost(),
		io:     newCtVethFakeIO(),
		alarms: network.NewAlarmStore(),
		cfg:    cfg,
	}
	f.io.kernel = f.host // veth 操作面的建/删/up/master 镜像进假内核（同一台内核的两个视图）
	f.layer = &ctVethFakeLayer{io: f.io}
	// Runner 用「会真的建 bridge」的包装：ApplyBridgeDomain 建桥后要按名 enslave 成员，
	// 而 dhcpFakeHost 对未覆盖的命令一律返回成功且不落设备（按它会「建桥后找不到桥」）。
	f.p = New(ctVethFakeKernel{f.host})
	f.p.SetConfig(cfg)
	f.p.SetAlarms(f.alarms)
	f.p.SetContainerVethLayer(f.layer)
	return f
}

// ctVethFakeKernel 在 dhcpFakeHost 之上补一条内核命令语义：`ip link add name <br> type bridge`
// 真的把该 bridge 登记进设备表（其余命令逐字转发）。
type ctVethFakeKernel struct{ *dhcpFakeHost }

func (k ctVethFakeKernel) Run(ctx context.Context, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	fields := strings.Fields(line)
	if len(fields) >= 6 && fields[0] == "ip" && fields[1] == "link" && fields[2] == "add" &&
		fields[3] == "name" && strings.Contains(line, "type bridge") {
		k.mu.Lock()
		k.cmds = append(k.cmds, line)
		k.mu.Unlock()
		k.addLink(fields[4], "bridge")
		return "", nil
	}
	return k.dhcpFakeHost.Run(ctx, name, args...)
}

// ctSetMasterFake 在假内核里把一个接口挂到某 bridge 名下（读视图的成员来源）。
func (h *dhcpFakeHost) ctSetMaster(name, master string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l := h.links[name]; l != nil {
		l.master = master
	}
}

// ctSetUp 在假内核里置一个接口 up/down（veth 操作面镜像用）。
func (h *dhcpFakeHost) ctSetUp(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l := h.links[name]; l != nil {
		l.up = true
	}
}

// removeLink 在假内核里删掉一个接口（带外删除的现场）。
func (h *dhcpFakeHost) removeLink(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.links, name)
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ctCfgOneContainer 一个容器 + 一个 memif vNIC 的配置（接入测试交换机）。
func ctCfgOneContainer(owner string) model.Config {
	return model.Config{ContainerFunctions: []model.ContainerFunction{{
		Name:  owner,
		Image: "alpine:3.20",
		Interfaces: []model.VnfInterface{
			{Name: "eth0", Type: "memif", VirtualSwitch: ctVethTestSwitch, MAC: "02:aa:bb:cc:dd:01"},
		},
	}}}
}

// ---------- ① 命名（单一真源） ----------

func TestContainerVethNamesSingleSource(t *testing.T) {
	host, peer := orchestrator.ContainerVethNames("ct-a", "eth0")
	againHost, againPeer := orchestrator.ContainerVethNames("ct-a", "eth0")
	if host != againHost || peer != againPeer {
		t.Fatalf("同样的（容器, vNIC）必须得到同一对名字（确定性）：(%s,%s) vs (%s,%s)",
			host, peer, againHost, againPeer)
	}
	if len(host) != 15 || len(peer) != 15 {
		t.Fatalf("两端各 15 字符（IFNAMSIZ-1），得到 %d/%d：%s/%s", len(host), len(peer), host, peer)
	}
	if !strings.HasPrefix(host, ctVethHostPrefix) || !strings.HasPrefix(peer, ctVethPeerPrefix) {
		t.Fatalf("前缀应为 %s/%s，得到 %s/%s", ctVethHostPrefix, ctVethPeerPrefix, host, peer)
	}
	if host[len(ctVethHostPrefix):] != peer[len(ctVethPeerPrefix):] {
		t.Fatalf("两端应只差前缀（同一哈希）：%s vs %s", host, peer)
	}
	if !isProductContainerVethName(host) || !isProductContainerVethName(peer) || !isProductContainerVethHostEnd(host) {
		t.Fatalf("产品名识别应成立：%s / %s", host, peer)
	}
	if isProductContainerVethHostEnd(peer) {
		t.Fatalf("容器端不是宿主端：%s", peer)
	}
	// 任一输入不同 ⇒ 名字不同（测试取值域内不撞 8 位哈希）。
	seen := map[string]string{}
	for _, in := range [][2]string{{"ct-a", "eth0"}, {"ct-a", "eth1"}, {"ct-b", "eth0"}, {"ct-b", "eth1"}, {"ct-a", "eth10"}} {
		h, _ := orchestrator.ContainerVethNames(in[0], in[1])
		if prev, ok := seen[h]; ok {
			t.Fatalf("%v 与 %s 派生出同一宿主端名 %s", in, prev, h)
		}
		seen[h] = in[0] + "/" + in[1]
	}
	// 严格字符集（读视图过滤/删除身份核对的兜底判据）：相似但非法的名字一律不认。
	for _, bad := range []string{
		"", "nfvisct", "nfvisct1234567", "nfvisct123456789", "nfvisct1234567G",
		"nfvisct12g45678", "nfvisdh12345678", "ens192",
	} {
		if isProductContainerVethName(bad) {
			t.Fatalf("%q 不是产品容器 veth 名（严格前缀 + 8 位小写十六进制、15 字符）", bad)
		}
	}
}

// ---------- ② 宿主端收敛（只建接口 + up；入桥与 VLAN 归 bridge-domain 段） ----------

func TestContainerVethApplyBuildsInterfaceOnly(t *testing.T) {
	f := newCtVethFixture(t, model.Config{})
	port := orchestrator.VnfPort{
		VM: "ct-web", Interface: "eth0", Type: "memif",
		VirtualSwitch: ctVethTestSwitch, MAC: "02:aa:bb:cc:dd:01", VLAN: 100,
	}
	if err := f.p.ApplyVnfInterface(context.Background(), port); err != nil {
		t.Fatalf("收敛容器 vNIC 宿主端失败: %v", err)
	}
	host, peer := orchestrator.ContainerVethNames("ct-web", "eth0")
	if len(f.io.created) != 1 || f.io.created[0] != host {
		t.Fatalf("应建立且仅建立一对 veth（宿主端 %s）：%v", host, f.io.created)
	}
	if _, ok := f.io.links[peer]; !ok {
		t.Fatalf("容器端 %s 应同时在（未 attach 前留在宿主命名空间）", peer)
	}
	if !f.io.ups[host] {
		t.Fatal("宿主端应置 up（down 的口不转发也不收发帧）")
	}
	// 入桥**不在这里**：提交期本段先于交换机段，此刻内核 bridge 可能还不存在；
	// 由 bridge-domain 段的成员处理（l2.go 的 memberLinkName）完成——故这里不得有任何 master 调用。
	if len(f.io.masterCalls) != 0 {
		t.Fatalf("入桥归 bridge-domain 段的成员处理，本段不得 enslave：%v", f.io.masterCalls)
	}
	// VLAN 与 VPP 侧同口径：vNIC 级 vlan 不单独处理（VLAN 由交换机/端口声明经桥段落），
	// 故本段不得产生任何 `bridge vlan` 命令。
	for _, c := range f.host.lastCmds() {
		if strings.HasPrefix(c, "bridge vlan") {
			t.Fatalf("vNIC 级 vlan 不在接入路径处理（与 VPP 侧 memif 路径同口径），实际：%s", c)
		}
	}
}

func TestContainerVethApplyWithoutSwitch(t *testing.T) {
	// 未接入交换机：只建对 + up，不入桥、不产生 VLAN 命令（与 VPP 侧「建接口、不进 BD」同义）。
	f := newCtVethFixture(t, model.Config{})
	port := orchestrator.VnfPort{VM: "ct-db", Interface: "eth0", Type: "memif", VLAN: 30}
	if err := f.p.ApplyVnfInterface(context.Background(), port); err != nil {
		t.Fatalf("未接入交换机的 vNIC 也应能建对: %v", err)
	}
	host, _ := orchestrator.ContainerVethNames("ct-db", "eth0")
	if len(f.io.created) != 1 || f.io.created[0] != host {
		t.Fatalf("应建立 veth 对：%v", f.io.created)
	}
	if !f.io.ups[host] {
		t.Fatal("宿主端应置 up")
	}
	if len(f.io.masterCalls) != 0 {
		t.Fatalf("未接入交换机不应 enslave 到任何 bridge：%v", f.io.masterCalls)
	}
	for _, c := range f.host.lastCmds() {
		if strings.HasPrefix(c, "bridge vlan") {
			t.Fatalf("不应产生 VLAN 条目命令：%s", c)
		}
	}
}

// 先建接口、后入域：vnf-if 段（ApplyContainerVeth）建对，bridge-domain 段按 SwitchMembersOf
// 合流后的声明把宿主端当成员口 enslave——与 VPP 侧「先建 memif 接口、BD 段按名挂端口」同构。
// 用例同时钉住 memberLinkName 对 Container 端口的映射（宿主端名 ↔ LinkName(交换机)）。
func TestContainerVethBridgeSegmentEnslavesHostEnd(t *testing.T) {
	// memberLinkName 的映射（纯函数，含三类端口各自的归属）。
	host, _ := orchestrator.ContainerVethNames("ct-web", "eth0")
	if got, ok := memberLinkName(model.VSwitchPort{Container: "ct-web", ContainerInterface: "eth0"}); !ok || got != host {
		t.Fatalf("container 端口应映射到宿主端 veth 名 %s，得到 (%q,%v)", host, got, ok)
	}
	if _, ok := memberLinkName(model.VSwitchPort{Vnf: "vm-a", VnfInterface: "eth0"}); ok {
		t.Fatal("vnf 端口仍应由 libvirt 自建 tap 挂桥（本方法不碰）")
	}
	if got, ok := memberLinkName(model.VSwitchPort{Interface: "ens224"}); !ok || got != LinkName("ens224") {
		t.Fatalf("物理口端口应映射到 LinkName：(%q,%v)", got, ok)
	}

	cfg := model.Config{
		VirtualSwitches:    []model.VirtualSwitch{{Name: ctVethTestSwitch, Type: "l2"}},
		ContainerFunctions: ctCfgOneContainer("ct-web").ContainerFunctions,
	}
	f := newCtVethFixture(t, cfg)
	ctx := context.Background()
	switches, refErrs := orchestrator.SwitchMembersOf(cfg, orchestrator.DefaultVhostDir, orchestrator.DefaultMemifDir)
	if len(refErrs) != 0 {
		t.Fatalf("声明的交换机存在，不应有无法归位的 vNIC：%v", refErrs)
	}
	if len(switches) != 1 || len(switches[0].Ports) != 1 || switches[0].Ports[0].Container != "ct-web" {
		t.Fatalf("容器侧 vNIC 声明应合流进交换机端口集合：%+v", switches)
	}

	// ① vnf-if 段先跑：此刻内核里**没有**该交换机的 bridge（同一次提交里两者一起新建的现场）。
	if err := f.p.ApplyVnfInterface(ctx, orchestrator.VnfPort{
		VM: "ct-web", Interface: "eth0", Type: "memif", VirtualSwitch: ctVethTestSwitch,
	}); err != nil {
		t.Fatalf("建宿主端不应依赖 bridge 是否已在（先建接口、后入域）：%v", err)
	}
	// 宿主端是内核对象：把它登记进假内核（模拟产品刚建的 veth；桥段的 enslave 按内核实况做）。
	f.host.addLink(host, "veth")
	if f.host.has(LinkName(ctVethTestSwitch)) {
		t.Fatal("用例前提：此刻交换机 bridge 还不存在（模拟同一次提交里一起新建）")
	}
	// ② bridge-domain 段后跑：按合流后的声明建 bridge 并把宿主端 enslave 进去。
	if err := f.p.ApplyBridgeDomain(ctx, switches[0]); err != nil {
		t.Fatalf("bridge-domain 段应把容器宿主端当成员口处理：%v", err)
	}
	if row := f.host.link(host); row == nil || row.master != LinkName(ctVethTestSwitch) {
		t.Fatalf("宿主端应被桥段 enslave 到 %s：%+v", LinkName(ctVethTestSwitch), row)
	}
	if row := f.host.link(host); row == nil || !row.up {
		t.Fatalf("宿主端应被桥段置 up：%+v", row)
	}
}

func TestContainerVethApplyRejectsNonMemif(t *testing.T) {
	f := newCtVethFixture(t, model.Config{})
	err := f.p.ApplyContainerVeth(context.Background(),
		orchestrator.VnfPort{VM: "vm-a", Interface: "eth0", Type: "vhost-user"})
	if err == nil || !strings.Contains(err.Error(), "vhost-user") {
		t.Fatalf("非 memif 类型应如实拒绝并点名类型，得到 %v", err)
	}
	if len(f.io.ensureCalls) != 0 {
		t.Fatalf("拒绝路径不得下发任何 veth 操作：%v", f.io.ensureCalls)
	}
}

// ---------- ③ attach（幂等 + 一个失败不遮其它） ----------

func TestContainerVethAttachConvergesAndIsIdempotent(t *testing.T) {
	f := newCtVethFixture(t, model.Config{})
	ifaces := []model.VnfInterface{
		{Name: "eth0", Type: "memif", VirtualSwitch: ctVethTestSwitch, MAC: "02:aa:bb:cc:dd:01"},
		{Name: "eth1", Type: "memif", VirtualSwitch: ctVethTestSwitch},
	}
	if err := f.p.Attach(context.Background(), "ct-web", ifaces, 4242); err != nil {
		t.Fatalf("attach 失败: %v", err)
	}
	for _, nic := range ifaces {
		host, peer := orchestrator.ContainerVethNames("ct-web", nic.Name)
		if !f.io.ups[host] {
			t.Fatalf("vNIC %s 的宿主端应 up（attach 前先确保接口在）", nic.Name)
		}
		found := false
		for _, call := range f.io.attachCalls {
			if call.peer == peer && call.pid == 4242 && call.niceName == nic.Name && call.mac == nic.MAC {
				found = true
			}
		}
		if !found {
			t.Fatalf("应把容器端 %s 接入 pid 4242、容器内名 %s、MAC %q：%+v",
				peer, nic.Name, nic.MAC, f.io.attachCalls)
		}
	}
	if len(f.io.created) != 2 || len(f.io.netnsMoves) != 2 {
		t.Fatalf("两个 vNIC 应各建一对、各移入一次：created=%v moves=%v", f.io.created, f.io.netnsMoves)
	}
	// 幂等：再 attach 一次（容器编排可能重试）不得重建对、不得重复移入。
	if err := f.p.Attach(context.Background(), "ct-web", ifaces, 4242); err != nil {
		t.Fatalf("重复 attach 应幂等成功: %v", err)
	}
	if len(f.io.created) != 2 {
		t.Fatalf("重复 attach 绝不能重建 veth 对（重建会打断运行中容器的网络）：%v", f.io.created)
	}
	if len(f.io.netnsMoves) != 2 {
		t.Fatalf("已接入的容器端不应被重复移入：%v", f.io.netnsMoves)
	}
}

func TestContainerVethAttachPartialFailureNamesIfaceAndOthersConverge(t *testing.T) {
	f := newCtVethFixture(t, model.Config{})
	ifaces := []model.VnfInterface{
		{Name: "eth0", Type: "memif", VirtualSwitch: ctVethTestSwitch},
		{Name: "eth1", Type: "memif", VirtualSwitch: ctVethTestSwitch},
	}
	_, badPeer := orchestrator.ContainerVethNames("ct-web", "eth1")
	f.io.attachErr[badPeer] = errors.New("注入：移入命名空间失败")
	err := f.p.Attach(context.Background(), "ct-web", ifaces, 4242)
	if err == nil {
		t.Fatal("一个 vNIC 接不进去应如实报错（不吞）")
	}
	if !strings.Contains(err.Error(), "eth1") || !strings.Contains(err.Error(), "ct-web") {
		t.Fatalf("错误应点名容器与 vNIC：%v", err)
	}
	goodHost, _ := orchestrator.ContainerVethNames("ct-web", "eth0")
	if !f.io.ups[goodHost] {
		t.Fatalf("另一个 vNIC 应照常收敛（不因邻居失败被跳过）：%v", f.io.upCalls)
	}
	// pid 非法（容器没在跑）：如实拒绝，不猜。
	if err := f.p.Attach(context.Background(), "ct-web", ifaces, 0); err == nil || !strings.Contains(err.Error(), "pid") {
		t.Fatalf("pid 非法应如实报错：%v", err)
	}
}

// ---------- ④ 删除 ----------

func TestContainerVethDeleteIsIdempotentAndOwnerScoped(t *testing.T) {
	cfg := model.Config{ContainerFunctions: []model.ContainerFunction{
		{Name: "ct-web", Image: "alpine", Interfaces: []model.VnfInterface{
			{Name: "eth0", Type: "memif", VirtualSwitch: ctVethTestSwitch},
		}},
		{Name: "ct-db", Image: "alpine", Interfaces: []model.VnfInterface{
			{Name: "eth0", Type: "memif", VirtualSwitch: ctVethTestSwitch},
			{Name: "eth1", Type: "memif"}, // 未接入交换机：只建对
		}},
	}}
	f := newCtVethFixture(t, cfg)
	ctx := context.Background()
	for _, spec := range containerVethSpecsOf(cfg) {
		if err := f.p.ApplyContainerVeth(ctx, orchestrator.VnfPort{
			VM: spec.owner, Interface: spec.iface, Type: "memif", VirtualSwitch: spec.vs,
		}); err != nil {
			t.Fatalf("收敛 %s/%s 失败: %v", spec.owner, spec.iface, err)
		}
	}
	webHost, _ := orchestrator.ContainerVethNames("ct-web", "eth0")
	if err := f.p.DeleteVnfInterface(ctx, "ct-web", "eth0"); err != nil {
		t.Fatalf("删除容器 vNIC 宿主端失败: %v", err)
	}
	if !containsStr(f.io.deleted, webHost) {
		t.Fatalf("应删掉 %s：%v", webHost, f.io.deleted)
	}
	if _, ok := f.io.links[webHost]; ok {
		t.Fatalf("宿主端删掉后不应仍在（成对消失）：%v", f.io.links)
	}
	// 幂等：再删一次不报错（设备不存在按已达成）。
	attempts := len(f.io.deleteAttempts)
	if err := f.p.DeleteVnfInterface(ctx, "ct-web", "eth0"); err != nil {
		t.Fatalf("重复删除应幂等: %v", err)
	}
	if len(f.io.deleteAttempts) != attempts+1 {
		t.Fatalf("重复删除应仍走一次容错删除：%v", f.io.deleteAttempts)
	}
	// 属主范围：DeleteContainerVeths(ct-db) 删它自己的两端，不碰别人。
	db0, _ := orchestrator.ContainerVethNames("ct-db", "eth0")
	db1, _ := orchestrator.ContainerVethNames("ct-db", "eth1")
	if err := f.p.Delete(ctx, "ct-db"); err != nil {
		t.Fatalf("按属主回收失败: %v", err)
	}
	for _, name := range []string{db0, db1} {
		if !containsStr(f.io.deleted, name) {
			t.Fatalf("%s 应被回收：%v", name, f.io.deleted)
		}
	}
	// 未声明也未簿记的属主：空操作、不下发命令。
	before := len(f.io.deleteAttempts)
	if err := f.p.Delete(ctx, "ct-none"); err != nil || len(f.io.deleteAttempts) != before {
		t.Fatalf("无对象属主应空操作（err=%v，attempts %d→%d）", err, before, len(f.io.deleteAttempts))
	}
}

// ---------- ⑤ 巡检 ----------

func TestContainerVethReconcileEnsuresDeclaredAndClearsResidue(t *testing.T) {
	cfg := ctCfgOneContainer("ct-web")
	f := newCtVethFixture(t, cfg)
	f.io.addBridge(LinkName(ctVethTestSwitch)) // 交换机已收敛（巡检入对桥的前提）
	// 现场：一个「无声明对应」的产品宿主端（容器已删/切换残留）与一个名字相近但**非法**的用户口。
	residue := ctVethHostPrefix + "deadbeef"
	userLike := ctVethHostPrefix + "1234567g" // 非十六进制字符 ⇒ 不是产品名，绝不能碰
	f.host.addLink(residue, "veth")
	f.host.addLink(userLike, "veth")
	f.io.seedHostEnd(residue)
	f.io.seedHostEnd(userLike)

	if errs := f.p.ReconcileContainerVeth(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("巡检应无错误：%v", errs)
	}
	host, _ := orchestrator.ContainerVethNames("ct-web", "eth0")
	if len(f.io.created) != 1 || f.io.created[0] != host {
		t.Fatalf("声明里缺的宿主端应补建（%s）：%v", host, f.io.created)
	}
	if !f.io.ups[host] {
		t.Fatal("补建的宿主端应置 up")
	}
	if got := f.io.masters[host]; got != LinkName(ctVethTestSwitch) {
		t.Fatalf("巡检应把宿主端入对桥（%s）：%q", LinkName(ctVethTestSwitch), got)
	}
	if !containsStr(f.io.deleted, residue) {
		t.Fatalf("无声明对应的产品宿主端应清掉（%s）：%v", residue, f.io.deleted)
	}
	if _, ok := f.io.links[residue]; ok {
		t.Fatalf("残留宿主端清掉后不应仍在：%v", f.io.links)
	}
	if containsStr(f.io.deleteAttempts, userLike) {
		t.Fatalf("名字相近但非法的用户口绝不能被删（%q）：%v", userLike, f.io.deleteAttempts)
	}
	if _, ok := f.io.links[userLike]; !ok {
		t.Fatalf("名字相近但非法的用户口应保持不动：%v", f.io.links)
	}
}

func TestContainerVethReconcileAlarmRaisedAndResolved(t *testing.T) {
	cfg := ctCfgOneContainer("ct-web")
	f := newCtVethFixture(t, cfg)
	f.io.addBridge(LinkName(ctVethTestSwitch)) // 交换机已收敛（恢复后入对桥的前提）
	f.io.ensureErr = errors.New("注入：veth 建不出")
	if errs := f.p.ReconcileContainerVeth(context.Background(), cfg); len(errs) == 0 {
		t.Fatal("宿主端建不出应如实进未收敛项")
	}
	active := f.alarms.List(network.AlarmActive)
	found := false
	for _, a := range active {
		if a.Source == "container-veth" && a.Code == network.AlarmUnconverged {
			found = true
		}
	}
	if !found {
		t.Fatalf("巡检失败应落未收敛告警（source=container-veth）：%+v", active)
	}
	// 底座恢复 ⇒ 同一声明下一轮成功即自动消解。
	f.io.ensureErr = nil
	if errs := f.p.ReconcileContainerVeth(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("恢复后巡检应通过：%v", errs)
	}
	for _, a := range f.alarms.List(network.AlarmActive) {
		if a.Source == "container-veth" {
			t.Fatalf("成功一轮后该告警应自动消解：%+v", a)
		}
	}
}

func TestContainerVethReconcileNoDeclaredNoManagerIsNoop(t *testing.T) {
	// 未声明且从未装配：不构造管理器、不下发任何内核命令（不给没被用过的机器添巡检噪音）。
	host := newDHCPFakeHost()
	p := New(host)
	p.SetConfig(model.Config{})
	p.SetContainerVethLayer(&ctVethFakeLayer{io: newCtVethFakeIO()})
	if errs := p.ReconcileContainerVeth(context.Background(), model.Config{}); len(errs) != 0 {
		t.Fatalf("空声明巡检应空操作：%v", errs)
	}
	if n := len(host.lastCmds()); n != 0 {
		t.Fatalf("空声明且从未装配时不得下发内核命令：%v", host.lastCmds())
	}
	p.ctVethMu.Lock()
	mgr := p.ctVeth
	p.ctVethMu.Unlock()
	if mgr != nil {
		t.Fatal("空声明且从未装配时不应构造管理器")
	}
	// 声明了容器但没有 memif vNIC：同样空操作（无对象可对账）。
	cfg := model.Config{ContainerFunctions: []model.ContainerFunction{
		{Name: "ct-x", Image: "alpine", Interfaces: []model.VnfInterface{{Name: "eth0", Type: "sriov-vf"}}},
	}}
	if errs := p.ReconcileContainerVeth(context.Background(), cfg); len(errs) != 0 {
		t.Fatalf("无 memif vNIC 时巡检应空操作：%v", errs)
	}
	if len(host.lastCmds()) != 0 {
		t.Fatalf("无 memif vNIC 时不得下发内核命令：%v", host.lastCmds())
	}
	// 装配过（管理器在）后空声明也要走一轮：把带外残留收干净（与 DNS 代理巡检同口径）。
	residue := ctVethHostPrefix + "ffffffff"
	host.addLink(residue, "veth")
	io := newCtVethFakeIO()
	io.seedHostEnd(residue)
	io.addBridge(LinkName(ctVethTestSwitch)) // 交换机已收敛（巡检入对桥的前提）
	q := New(host)
	q.SetConfig(model.Config{})
	q.SetContainerVethLayer(&ctVethFakeLayer{io: io})
	if errs := q.ReconcileContainerVeth(context.Background(), ctCfgOneContainer("ct-tmp")); len(errs) != 0 {
		t.Fatalf("首次收敛应成功：%v", errs)
	}
	if errs := q.ReconcileContainerVeth(context.Background(), model.Config{}); len(errs) != 0 {
		t.Fatalf("装配过之后的空声明巡检应成功：%v", errs)
	}
	if !containsStr(io.deleted, residue) {
		t.Fatalf("装配过之后，无声明对应的残留也应被清掉：%v", io.deleted)
	}
}

// ---------- ⑥ 读视图（BridgeDomains 过滤） ----------

func TestContainerVethBridgeDomainsHidesProductHostEnds(t *testing.T) {
	cfg := model.Config{
		VirtualSwitches:    []model.VirtualSwitch{{Name: "vs-l2", Type: "l2"}},
		ContainerFunctions: ctCfgOneContainer("ct-web").ContainerFunctions,
	}
	f := newCtVethFixture(t, cfg)
	host, _ := orchestrator.ContainerVethNames("ct-web", "eth0")
	userLike := ctVethHostPrefix + "1234567g" // 名字相近但非法：必须照常显示（不误伤用户口）
	br := LinkName("vs-l2")
	f.host.addLink(br, "bridge")
	f.host.addLink(host, "veth")
	f.host.addLink(userLike, "veth")
	f.host.addLink("ens224", "")
	f.host.ctSetMaster(host, br)
	f.host.ctSetMaster(userLike, br)
	f.host.ctSetMaster("ens224", br)

	// ① 未装配管理器（簿记为空）：按严格前缀兜底也要隐藏产品宿主端。
	bd := ctFindBridgeDomain(t, f)
	if !containsStr(ctPortNames(bd), userLike) || !containsStr(ctPortNames(bd), "ens224") {
		t.Fatalf("非法相似名与普通口应照常显示：%v", ctPortNames(bd))
	}
	if containsStr(ctPortNames(bd), host) {
		t.Fatalf("产品容器 vNIC 宿主端不应进用户端口视图：%v", ctPortNames(bd))
	}
	// ② 收敛声明（装入簿记）后仍隐藏，且未被误删（读视图过滤不是删除）。
	if err := f.p.ApplyContainerVeth(context.Background(), orchestrator.VnfPort{
		VM: "ct-web", Interface: "eth0", Type: "memif", VirtualSwitch: ctVethTestSwitch,
	}); err != nil {
		t.Fatalf("收敛失败: %v", err)
	}
	bd = ctFindBridgeDomain(t, f)
	if containsStr(ctPortNames(bd), host) {
		t.Fatalf("簿记在位的产品宿主端也不应进用户端口视图：%v", ctPortNames(bd))
	}
	if !containsStr(ctPortNames(bd), userLike) {
		t.Fatalf("非法相似名不该被簿记过滤误伤：%v", ctPortNames(bd))
	}
	if _, ok := f.io.links[host]; !ok {
		t.Fatalf("读视图过滤不得删除内核对象：%v", f.io.links)
	}
}

// ctPortNames 交换机读视图里的端口名（断言辅助）。
func ctPortNames(bd network.BDRuntime) []string {
	out := make([]string, 0, len(bd.Ports))
	for _, p := range bd.Ports {
		out = append(out, p.Name)
	}
	return out
}

func ctFindBridgeDomain(t *testing.T, f *ctVethFixture) network.BDRuntime {
	t.Helper()
	bds, err := f.p.BridgeDomains()
	if err != nil {
		t.Fatalf("读交换机运行态失败: %v", err)
	}
	for _, bd := range bds {
		if bd.Name == LinkName("vs-l2") {
			return bd
		}
	}
	t.Fatalf("读视图里应有 %s：%+v", LinkName("vs-l2"), bds)
	return network.BDRuntime{}
}

// ---------- ⑦ Provider 接线 ----------

func TestContainerVethProviderDispatchAndHooks(t *testing.T) {
	f := newCtVethFixture(t, ctCfgOneContainer("ct-web"))
	ctx := context.Background()
	// VM vNIC（vhost-user）：内核侧如实空操作（宿主 tap 由 libvirt 建，产品不碰）。
	if err := f.p.ApplyVnfInterface(ctx, orchestrator.VnfPort{VM: "vm-a", Interface: "eth0", Type: "vhost-user"}); err != nil {
		t.Fatalf("VM vNIC 应如实空操作：%v", err)
	}
	if err := f.p.DeleteVnfInterface(ctx, "vm-a", "eth0"); err != nil {
		t.Fatalf("VM vNIC 删除应如实空操作：%v", err)
	}
	if len(f.io.ensureCalls) != 0 || len(f.io.deleteAttempts) != 0 {
		t.Fatalf("VM vNIC 不得碰容器 veth：ensure=%v delete=%v", f.io.ensureCalls, f.io.deleteAttempts)
	}
	// memif：走 veth；Attach 钩子与 AttachContainerVeth 同源。
	port := orchestrator.VnfPort{VM: "ct-web", Interface: "eth0", Type: "memif", VirtualSwitch: ctVethTestSwitch}
	if err := f.p.ApplyVnfInterface(ctx, port); err != nil {
		t.Fatalf("容器 vNIC 应走 veth 落地：%v", err)
	}
	host, _ := orchestrator.ContainerVethNames("ct-web", "eth0")
	if !containsStr(f.io.created, host) {
		t.Fatalf("容器 vNIC 应建宿主端：%v", f.io.created)
	}
	ifaces := []model.VnfInterface{{Name: "eth0", Type: "memif", VirtualSwitch: ctVethTestSwitch}}
	if err := f.p.Attach(ctx, "ct-web", ifaces, 777); err != nil {
		t.Fatalf("Attach 钩子应可用：%v", err)
	}
	if len(f.io.netnsMoves) != 1 {
		t.Fatalf("Attach 钩子应把容器端移入容器：%v", f.io.netnsMoves)
	}
	// Delete 钩子与 DeleteContainerVeths 同源。
	if err := f.p.Delete(ctx, "ct-web"); err != nil {
		t.Fatalf("Delete 钩子应可用：%v", err)
	}
	if !containsStr(f.io.deleted, host) {
		t.Fatalf("Delete 钩子应回收宿主端：%v", f.io.deleted)
	}
}

// 恢复重放：**先建接口、后入域**——宿主端按名核对（缺则建）且**不重建已存在的**（重建会打断
// 运行中容器），随后交换机段按 SwitchMembersOf 合流后的声明把宿主端 enslave 进 bridge。
// 本用例同时钉住依赖序：若重放把交换机段排在前面（或声明集不含 vNIC 侧声明），宿主端要么
// 无从挂桥、要么被「释放不再声明的成员」摘掉——断言会当场失败。
func TestContainerVethRecoveryReplayKeepsExistingPairs(t *testing.T) {
	cfg := model.Config{
		VirtualSwitches:    []model.VirtualSwitch{{Name: ctVethTestSwitch, Type: "l2"}},
		ContainerFunctions: ctCfgOneContainer("ct-web").ContainerFunctions,
	}
	f := newCtVethFixture(t, cfg)
	ctx := context.Background()
	host, peer := orchestrator.ContainerVethNames("ct-web", "eth0")
	// 存量现场：宿主端与容器端都在（上次生命的 veth；容器端此刻还在容器 netns 里）。
	f.io.seedHostEnd(host)
	f.io.seedHostEnd(peer)
	f.host.addLink(host, "veth")
	f.host.addLink(peer, "veth")

	if errs := f.p.EnsureConsistent(ctx, cfg); len(errs) != 0 {
		t.Fatalf("恢复重放应通过：%v", errs)
	}
	if len(f.io.created) != 0 {
		t.Fatalf("宿主端已存在时恢复重放绝不重建：%v", f.io.created)
	}
	if row := f.host.link(host); row == nil || row.master != LinkName(ctVethTestSwitch) {
		t.Fatalf("交换机段应把已存在的宿主端 enslave 到 %s（先建接口、后入域）：%+v",
			LinkName(ctVethTestSwitch), row)
	}
	// 带外删掉宿主端（连对端一起）：下一次重放按名补建（veth 成对，两端一起回来）并可入桥。
	delete(f.io.links, host)
	delete(f.io.links, peer)
	f.host.removeLink(host)
	f.host.removeLink(peer)
	if errs := f.p.EnsureConsistent(ctx, cfg); len(errs) != 0 {
		t.Fatalf("重放应补建缺失的宿主端：%v", errs)
	}
	if len(f.io.created) != 1 || f.io.created[0] != host {
		t.Fatalf("缺失的宿主端应被补建：%v", f.io.created)
	}
}
