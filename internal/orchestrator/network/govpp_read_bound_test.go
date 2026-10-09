package network

// 读数路径有界化的单测（决策 #422）。
//
// 真机现场：VPP 崩溃窗口内的一条 `show interfaces <if> detail`，其 sw_interface_dump 在
// **无超时**等待下永久阻塞，并持着 CLI 全局互斥——所有 CLI 命令排队挂起、管理面整体不可用。
// 本文件的假通道**按 govpp 的应答时限语义**工作（core/channel.go v0.13.0）：
//   - SetReplyTimeout(d>0) ⇒ 每次 ReceiveReply 最多等 d，到期回 ErrReplyTimeout；
//   - 时限 0（govpp 缺省）⇒ 不限时（假实现里就是永不返回）——这正是修复前的病态。
//
// 红-绿：把任一读数调用点改回 `reqCtx.ReceiveReply(...)`（不套包装）后，本文件的用例会在
// 看门狗窗口内失败（读数永不返回），修复后再跑全绿。

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.fd.io/govpp/api"
	"go.fd.io/govpp/core"
)

// fakeReply 假通道对一次接收的应答。ok=false 表示「这一刻没有应答」——按通道当前应答时限等待
// （时限<=0 = 永不返回，模拟 govpp 缺省的不限时语义）。
type fakeReply struct {
	stop bool
	err  error
	ok   bool
}

// recvKind 接收种类：单请求（SendRequest+ReceiveReply）与多请求（dump）。
type recvKind int

const (
	kindRequest recvKind = iota
	kindMulti
)

// fakeBoundChannel 只实现读数路径用到的最小子集（api.Channel）。
type fakeBoundChannel struct {
	// serve 决定第 n 次接收的应答（n 从 1 计数）；nil = 一律「没有应答」。
	serve func(kind recvKind, n int, msg api.Message) fakeReply

	mu       sync.Mutex
	timeout  time.Duration
	calls    int
	settled  int // SetReplyTimeout 被调用次数
	never    chan struct{}
	observed []time.Duration // 每次 SetReplyTimeout 的取值（顺序）
}

func newFakeBoundChannel(serve func(kind recvKind, n int, msg api.Message) fakeReply) *fakeBoundChannel {
	return &fakeBoundChannel{serve: serve, never: make(chan struct{})}
}

func (c *fakeBoundChannel) SendRequest(api.Message) api.RequestCtx { return fakeBoundReq{c} }

func (c *fakeBoundChannel) SendMultiRequest(api.Message) api.MultiRequestCtx {
	return fakeBoundMulti{c}
}

func (c *fakeBoundChannel) SubscribeNotification(chan api.Message, api.Message) (api.SubscriptionCtx, error) {
	return nil, nil
}

func (c *fakeBoundChannel) SetReplyTimeout(d time.Duration) {
	c.mu.Lock()
	c.timeout, c.settled = d, c.settled+1
	c.observed = append(c.observed, d)
	c.mu.Unlock()
}

func (c *fakeBoundChannel) CheckCompatiblity(...api.Message) error { return nil }

func (c *fakeBoundChannel) Close() {}

// receive 按 govpp 的语义完成一次接收：先看有没有应答；没有就按**当前**时限等待
// （govpp 在 receiveReplyInternal 开头读 replyTimeout，故这里同样每次接收时读取）。
func (c *fakeBoundChannel) receive(kind recvKind, msg api.Message) (bool, error) {
	c.mu.Lock()
	c.calls++
	n, budget := c.calls, c.timeout
	serve := c.serve
	c.mu.Unlock()

	if serve != nil {
		if r := serve(kind, n, msg); r.ok {
			return r.stop, r.err
		}
	}
	if budget <= 0 {
		<-c.never // govpp 缺省（0 = 不限时）：永不返回
	}
	<-time.After(budget)
	return false, fmt.Errorf("%w %s", core.ErrReplyTimeout, budget)
}

// timeoutNow 当前应答时限（断言「读数结束后已撤销」用）。
func (c *fakeBoundChannel) timeoutNow() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.timeout
}

func (c *fakeBoundChannel) setCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settled
}

type fakeBoundReq struct{ ch *fakeBoundChannel }

func (r fakeBoundReq) ReceiveReply(msg api.Message) error {
	_, err := r.ch.receive(kindRequest, msg)
	return err
}

type fakeBoundMulti struct{ ch *fakeBoundChannel }

func (r fakeBoundMulti) ReceiveReply(msg api.Message) (bool, error) {
	return r.ch.receive(kindMulti, msg)
}

// withReadBudget 把读数预算压小（可注入）供测试使用，并复原。
func withReadBudget(t *testing.T, d time.Duration) {
	t.Helper()
	old := vppReadBudget
	vppReadBudget = d
	t.Cleanup(func() { vppReadBudget = old })
}

// mustReturnWithin 在 d 内等 call 返回；超过 d 即判失败（这就是「无界等待」的判据）。
func mustReturnWithin(t *testing.T, d time.Duration, call func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("读数未在 %s 内返回：一次挂死的 dump 会持着 CLI 互斥把管理面整体拖死", d)
		return nil
	}
}

// stallMulti 模拟「VPP 崩溃窗口」：dump 前 half 条明细正常、其后不再返回；单请求读数
// （读视图里的单请求查询同样是这条病）一律不再返回。
func stallMulti(half int) func(kind recvKind, n int, msg api.Message) fakeReply {
	return func(kind recvKind, n int, msg api.Message) fakeReply {
		if kind == kindMulti && n <= half {
			return fakeReply{ok: true}
		}
		return fakeReply{}
	}
}

// alwaysReply 一律立即应答（写路径语义不变、预算关闭时走旧语义等用例用）。
func alwaysReply() func(kind recvKind, n int, msg api.Message) fakeReply {
	return func(kind recvKind, n int, msg api.Message) fakeReply { return fakeReply{ok: true} }
}

// TestVPPReadPathsBoundedOnStalledDump：读数路径上的 dump/单请求在「半途不再返回」时
// **在预算内**返回明确错误（而不是无限等待），且返回后通道应答时限已撤销回 govpp 缺省
// （写路径语义不变）。
func TestVPPReadPathsBoundedOnStalledDump(t *testing.T) {
	const budget = 40 * time.Millisecond
	withReadBudget(t, budget)

	cases := []struct {
		name string
		call func(ch api.Channel) error
	}{
		{"SwInterfaceIndex(l2)", func(ch api.Channel) error {
			_, _, err := (&govppL2Client{ch: ch}).SwInterfaceIndex("ens192")
			return err
		}},
		{"SwInterfaceNames(l2)", func(ch api.Channel) error { _, err := (&govppL2Client{ch: ch}).SwInterfaceNames(); return err }},
		{"BridgeDomains(l2，先嵌套读接口清单)", func(ch api.Channel) error { _, err := (&govppL2Client{ch: ch}).BridgeDomains(); return err }},
		{"BridgeDomainExists(l2)", func(ch api.Channel) error { _, err := (&govppL2Client{ch: ch}).BridgeDomainExists(100); return err }},
		{"MACTable(l2)", func(ch api.Channel) error { _, err := (&govppL2Client{ch: ch}).MACTable(100); return err }},
		{"SwInterfaceIndex(l3)", func(ch api.Channel) error {
			_, _, err := (&govppL3Client{ch: ch}).SwInterfaceIndex("ens192")
			return err
		}},
		{"IPTableExists(l3)", func(ch api.Channel) error { _, err := (&govppL3Client{ch: ch}).IPTableExists(100, false); return err }},
		{"Routes(l3)", func(ch api.Channel) error { _, err := (&govppL3Client{ch: ch}).Routes(100, false); return err }},
		{"BviOfBD(l3)", func(ch api.Channel) error { _, _, err := (&govppL3Client{ch: ch}).BviOfBD(100); return err }},
		{"IPTables(l3)", func(ch api.Channel) error { _, err := (&govppL3Client{ch: ch}).IPTables(); return err }},
		{"SwInterfaceTable(l3，单请求)", func(ch api.Channel) error {
			_, _, err := (&govppL3Client{ch: ch}).SwInterfaceTable(5, false)
			return err
		}},
		{"PolicerDump(storm)", func(ch api.Channel) error { _, err := (&govppStormClient{ch: ch}).PolicerDump(); return err }},
		{"AllInterfaceIndexes(storm)", func(ch api.Channel) error { _, err := (&govppStormClient{ch: ch}).AllInterfaceIndexes(); return err }},
		{"ClassifyTableIDs(storm，单请求)", func(ch api.Channel) error { _, err := (&govppStormClient{ch: ch}).ClassifyTableIDs(); return err }},
		{"ClassifyTableInfo(storm，单请求)", func(ch api.Channel) error {
			_, _, err := (&govppStormClient{ch: ch}).ClassifyTableInfo(7)
			return err
		}},
		{"AttachedL2Table(storm，单请求)", func(ch api.Channel) error {
			_, _, err := (&govppStormClient{ch: ch}).AttachedL2Table(5)
			return err
		}},
		{"ACLIndexByTag(acl)", func(ch api.Channel) error {
			_, _, err := (&govppAclClient{ch: ch}).ACLIndexByTag("nfvis-x")
			return err
		}},
		{"MacipBoundACL(acl)", func(ch api.Channel) error { _, _, err := (&govppAclClient{ch: ch}).MacipBoundACL(5); return err }},
		{"Bonds(bond)", func(ch api.Channel) error { _, err := (&govppBondClient{ch: ch}).Bonds(); return err }},
		{"BondMembers(bond)", func(ch api.Channel) error { _, err := (&govppBondClient{ch: ch}).BondMembers(5); return err }},
		{"LldpNeighbors(lldp，先嵌套读接口清单)", func(ch api.Channel) error { _, err := (&govppLldpClient{ch: ch}).LldpNeighbors(); return err }},
		{"NATSessions(nat，用户 dump + 会话 dump)", func(ch api.Channel) error { _, err := (&govppNatClient{ch: ch}).NATSessions(); return err }},
		{"NATAddressVRFs(nat)", func(ch api.Channel) error { _, err := (&govppNatClient{ch: ch}).NATAddressVRFs(); return err }},
		{"ProxyDump(dhcp)", func(ch api.Channel) error { _, err := (&govppDhcpClient{ch: ch}).ProxyDump(); return err }},
		{"TapDump(dhcpserver)", func(ch api.Channel) error { _, err := (&govppDHCPServerClient{ch: ch}).TapDump(); return err }},
		{"InterfaceAddresses(diag，先嵌套读接口清单)", func(ch api.Channel) error {
			_, err := (&govppDiagClient{ch: ch}).InterfaceAddresses(false)
			return err
		}},
		{"VhostUserSocket(vhost-user)", func(ch api.Channel) error {
			_, _, err := (&govppVhostUserClient{ch: ch}).VhostUserSocket(5)
			return err
		}},
		{"InterfaceStatus(vhost-user)", func(ch api.Channel) error {
			_, _, _, err := (&govppVhostUserClient{ch: ch}).InterfaceStatus(5)
			return err
		}},
		{"FindTagged(vxlan)", func(ch api.Channel) error { _, err := (&govppVxlanClient{ch: ch}).FindTagged("nfvis-"); return err }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := newFakeBoundChannel(stallMulti(2)) // 半途：两条明细之后不再返回
			err := mustReturnWithin(t, 20*budget, func() error { return tc.call(ch) })
			if err == nil {
				t.Fatal("dump 半途不再返回时必须回错误，而不是把空表当结果")
			}
			if !errors.Is(err, errVPPReadTimeout) {
				t.Fatalf("应为读数超时错误，实际 %v", err)
			}
			if !strings.Contains(err.Error(), "数据面读数超时") {
				t.Fatalf("超时错误要如实点名（读视图据此显示）: %v", err)
			}
			if got := ch.timeoutNow(); got != 0 {
				t.Fatalf("读数返回后应答时限应撤销回 govpp 缺省（0），实际 %s", got)
			}
			if ch.setCalls() == 0 {
				t.Fatal("读数路径必须真正装上应答时限（SetReplyTimeout 未被调用）")
			}
		})
	}
}

// TestVPPReadConnectionDropReturnsPromptly：连接中断要走「错误立即上抛」，**不能**等满预算
// （govpp 对不可用/写失败会投递带错误的应答；本用例把它与超时区分开）。
func TestVPPReadConnectionDropReturnsPromptly(t *testing.T) {
	withReadBudget(t, 5*time.Second) // 预算故意放大：立即返回只能来自错误本身

	broken := errors.New("write: broken pipe")
	ch := newFakeBoundChannel(func(kind recvKind, n int, msg api.Message) fakeReply {
		if n == 1 {
			return fakeReply{ok: true} // 第一条明细正常
		}
		return fakeReply{ok: true, err: broken} // 连接中断
	})
	start := time.Now()
	err := mustReturnWithin(t, 2*time.Second, func() error {
		_, err := (&govppL2Client{ch: ch}).SwInterfaceNames()
		return err
	})
	if err == nil {
		t.Fatal("连接中断必须上抛错误")
	}
	if !errors.Is(err, broken) {
		t.Fatalf("连接中断要原样透传（不得被改写/吞掉），实际 %v", err)
	}
	if errors.Is(err, errVPPReadTimeout) {
		t.Fatalf("连接中断不是读数超时，不得混淆: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("连接中断应立即返回，实际 %s（预算 5s 内等于在等超时）", d)
	}
}

// TestVPPReadSingleRequestBounded：单请求读数（非 dump）同样有界——覆盖公共包装，并钉住
// 「到期回明确错误」的文案口径（操作者据自查路径先看数据面状态）。
func TestVPPReadSingleRequestBounded(t *testing.T) {
	withReadBudget(t, 30*time.Millisecond)
	ch := newFakeBoundChannel(nil) // 一律没有应答

	err := mustReturnWithin(t, time.Second, func() error {
		_, _, err := (&govppStormClient{ch: ch}).ClassifyTableInfo(1)
		return err
	})
	if !errors.Is(err, errVPPReadTimeout) {
		t.Fatalf("单请求读数超时应回读数超时错误，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "show vpp") {
		t.Fatalf("超时文案要给自查路径（先看数据面状态）: %v", err)
	}
}

// TestVPPWritePathUnaffected：写路径不装时限、语义不变——本包装只在 ReceiveReply 期间生效，
// 写方法（add/del 这类下发）走的仍是 govpp 缺省（0 = 不限时）。
func TestVPPWritePathUnaffected(t *testing.T) {
	withReadBudget(t, 30*time.Millisecond)

	// 写：假通道给出成功应答（零值 reply 即 retval=0）
	ch := newFakeBoundChannel(alwaysReply())
	if err := (&govppL2Client{ch: ch}).BridgeDomainAddDel(100, true, true, "vs-x"); err != nil {
		t.Fatalf("写路径应照常成功: %v", err)
	}
	if n := ch.setCalls(); n != 0 {
		t.Fatalf("写路径不得装应答时限（SetReplyTimeout 调用 %d 次）", n)
	}
	// 读：装时限并撤销
	ch2 := newFakeBoundChannel(stallMulti(1))
	_ = mustReturnWithin(t, time.Second, func() error { _, err := (&govppL2Client{ch: ch2}).SwInterfaceNames(); return err })
	if ch2.setCalls() == 0 || ch2.timeoutNow() != 0 {
		t.Fatalf("读数应临时装时限并撤销，settled=%d timeout=%s", ch2.setCalls(), ch2.timeoutNow())
	}
}

// TestVPPReadBudgetDisabled：预算注入为 <=0 时退回原有语义（不算超时、不碰通道时限），
// 保证这个开关只用于「关掉有界」，不会把读数搞坏。
func TestVPPReadBudgetDisabled(t *testing.T) {
	withReadBudget(t, 0)
	ch := newFakeBoundChannel(func(kind recvKind, n int, msg api.Message) fakeReply {
		return fakeReply{ok: true, stop: true}
	})
	if _, err := (&govppL2Client{ch: ch}).SwInterfaceNames(); err != nil {
		t.Fatalf("预算关闭时读数应照常成功: %v", err)
	}
	if n := ch.setCalls(); n != 0 {
		t.Fatalf("预算关闭时不得设置应答时限，实际调用 %d 次", n)
	}
}
